package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeHibernateRuntime extends fakeWarmRuntime with ProcessHibernator.
type fakeHibernateRuntime struct {
	*fakeWarmRuntime
	stateDir      string
	images        map[string]struct{ dir, token string }
	checkpointErr error
	checkpointd   []string
}

func (f *fakeHibernateRuntime) HibernateCheckpoint(_ context.Context, wsID, token string) error {
	if f.checkpointErr != nil {
		return f.checkpointErr
	}
	if _, ok := f.running[wsID]; !ok {
		return errors.New("not running")
	}
	ckpt := filepath.Join(f.stateDir, "hibernate", wsID, "ckpt")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		return err
	}
	if f.images == nil {
		f.images = map[string]struct{ dir, token string }{}
	}
	f.images[wsID] = struct{ dir, token string }{ckpt, token}
	f.checkpointd = append(f.checkpointd, wsID)
	delete(f.running, wsID)
	f.stopped = append(f.stopped, wsID)
	return nil
}

func (f *fakeHibernateRuntime) HibernateImage(wsID string) (string, string, bool) {
	img, ok := f.images[wsID]
	if !ok {
		return "", "", false
	}
	return img.dir, img.token, true
}

func (f *fakeHibernateRuntime) ClearHibernate(wsID string) error {
	delete(f.images, wsID)
	return nil
}

func TestHibernateCheckpointsAndResumeRestores(t *testing.T) {
	root := t.TempDir()
	storage := &DirStorage{Root: root}
	rt := &fakeHibernateRuntime{
		fakeWarmRuntime: &fakeWarmRuntime{fakeRuntime: newFakeRuntime()},
		stateDir:        filepath.Join(root, "gv"),
	}
	reg := &fakeRegistrar{}
	router := &Router{
		Storage:  storage,
		Leases:   &FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
		Runtime:  rt,
		Sessions: reg,
	}
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img", GatewayURL: "http://gw:8443", Route: "tier-a"}

	mount, err := router.Up(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readSessionToken(mount)
	if err != nil || token == "" {
		t.Fatalf("session token not persisted: %q %v", token, err)
	}
	if len(rt.ensured) != 1 {
		t.Fatalf("cold Up ensured=%v", rt.ensured)
	}

	if err := router.Hibernate(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	if len(rt.checkpointd) != 1 {
		t.Fatalf("expected process checkpoint, got %v", rt.checkpointd)
	}
	if _, running := rt.running["ws1"]; running {
		t.Fatal("still running after hibernate")
	}
	if len(reg.revoked) == 0 {
		t.Fatal("hibernate must revoke gateway sessions")
	}

	rt.ensured = nil
	reg.revoked = nil
	reg.registered = nil
	mount2, err := router.Up(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if mount2 != mount {
		t.Fatalf("mount changed on resume: %s vs %s", mount2, mount)
	}
	if len(rt.restored) != 1 || rt.restored[0] != "ws1" {
		t.Fatalf("resume restored=%v, want [ws1]", rt.restored)
	}
	if len(rt.ensured) != 0 {
		t.Fatalf("resume must not cold-boot, ensured=%v", rt.ensured)
	}
	if ws := reg.registered[token]; ws != "ws1" {
		t.Fatalf("frozen token re-registered for %q, want ws1", ws)
	}
	if rt.tokenSeen != token {
		t.Fatalf("restore token %q, want frozen %q", rt.tokenSeen, token)
	}
	if _, ok := rt.images["ws1"]; ok {
		t.Fatal("hibernate image should be cleared after successful resume")
	}
}

func TestHibernateResumeFailureFallsBackToCold(t *testing.T) {
	root := t.TempDir()
	storage := &DirStorage{Root: root}
	rt := &fakeHibernateRuntime{
		fakeWarmRuntime: &fakeWarmRuntime{fakeRuntime: newFakeRuntime()},
		stateDir:        filepath.Join(root, "gv"),
	}
	reg := &fakeRegistrar{}
	router := &Router{
		Storage:  storage,
		Leases:   &FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
		Runtime:  rt,
		Sessions: reg,
	}
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img", GatewayURL: "http://gw:8443", Route: "tier-a"}

	if _, err := router.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := router.Hibernate(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}

	rt.restoreErr = errors.New("restore exploded")
	rt.ensured = nil
	rt.restored = nil
	if _, err := router.Up(ctx, spec); err != nil {
		t.Fatalf("Up must succeed via cold fallback: %v", err)
	}
	if len(rt.ensured) != 1 {
		t.Fatalf("cold fallback ensured=%v", rt.ensured)
	}
	if _, ok := rt.images["ws1"]; ok {
		t.Fatal("failed resume must clear the hibernate image")
	}
}

func TestHibernateWithoutTokenStopsNormally(t *testing.T) {
	root := t.TempDir()
	storage := &DirStorage{Root: root}
	rt := &fakeHibernateRuntime{
		fakeWarmRuntime: &fakeWarmRuntime{fakeRuntime: newFakeRuntime()},
		stateDir:        filepath.Join(root, "gv"),
	}
	router := &Router{
		Storage: storage,
		Leases:  &FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
		Runtime: rt,
	}
	ctx := context.Background()
	mount, err := storage.EnsureWorkspace(ctx, "ws1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := FenceAndWrite(mount, Lease{WorkspaceID: "ws1", Holder: containerName("ws1"), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	rt.running["ws1"] = 1

	if err := router.Hibernate(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	if len(rt.checkpointd) != 0 {
		t.Fatalf("checkpointed without token: %v", rt.checkpointd)
	}
	if len(rt.stopped) == 0 {
		t.Fatal("expected ordinary stop")
	}
}
