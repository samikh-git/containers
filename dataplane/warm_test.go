package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeWarmRuntime is fakeRuntime plus the WarmRestorer capability.
type fakeWarmRuntime struct {
	*fakeRuntime
	restored   []string
	restoreErr error
	// tokenSeen records the SessionToken the restore was asked to run with.
	tokenSeen string
}

func (f *fakeWarmRuntime) RestoreWorkspace(_ context.Context, spec WorkspaceSpec, mountPath, imageDir string) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	// Same fencing contract as EnsureWorkspace: lease on the volume first.
	if _, err := os.Stat(filepath.Join(mountPath, leaseFileName)); err != nil {
		panic("RestoreWorkspace called before lease was fenced onto the volume")
	}
	if _, err := os.Stat(imageDir); err != nil {
		return err
	}
	f.restored = append(f.restored, spec.ID)
	f.tokenSeen = spec.SessionToken
	f.running[spec.ID] = spec.Generation
	return nil
}

// fakePool hands out one prepared slot.
type fakePool struct {
	slot     *WarmSlot
	claimed  bool
	consumed bool
	released bool
	refills  int
}

func (p *fakePool) Claim(context.Context, WorkspaceSpec) (*WarmSlot, bool) {
	if p.slot == nil || p.claimed {
		return nil, false
	}
	p.claimed = true
	return p.slot, true
}
func (p *fakePool) Consume(*WarmSlot)                  { p.consumed = true }
func (p *fakePool) Release(context.Context, *WarmSlot) { p.released = true }
func (p *fakePool) RequestRefill()                     { p.refills++ }

// fakeRegistrar records session registrations.
type fakeRegistrar struct {
	registered map[string]string // token -> workspace
	revoked    []string
}

func (r *fakeRegistrar) Register(_ context.Context, token, workspace string, _ []string) error {
	if r.registered == nil {
		r.registered = map[string]string{}
	}
	r.registered[token] = workspace
	return nil
}
func (r *fakeRegistrar) RevokeWorkspace(_ context.Context, workspace string) error {
	r.revoked = append(r.revoked, workspace)
	return nil
}

// newWarmTestRouter builds a router whose storage holds a golden slot
// workspace with a snapshot, plus a pool exposing it as one slot.
func newWarmTestRouter(t *testing.T) (*Router, *fakeWarmRuntime, *fakePool, *fakeRegistrar, string) {
	t.Helper()
	root := t.TempDir()
	storage := &DirStorage{Root: root}
	ctx := context.Background()

	// Golden slot workspace: some content, then a snapshot of it.
	mount, err := storage.EnsureWorkspace(ctx, "warm-slot1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "seeded.txt"), []byte("golden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storage.Snapshot(ctx, "warm-slot1", goldenSnapshot); err != nil {
		t.Fatal(err)
	}

	slotDir := filepath.Join(root, "slot1")
	ckpt := filepath.Join(slotDir, "ckpt")
	if err := os.MkdirAll(ckpt, 0o755); err != nil {
		t.Fatal(err)
	}

	rt := &fakeWarmRuntime{fakeRuntime: newFakeRuntime()}
	pool := &fakePool{slot: &WarmSlot{
		ID: "slot1", Token: "st-slot-token",
		SourceWorkspace: "warm-slot1", Snapshot: goldenSnapshot,
		ImageDir: ckpt, dir: slotDir,
	}}
	reg := &fakeRegistrar{}
	router := &Router{
		Storage:  storage,
		Leases:   &FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
		Runtime:  rt,
		Sessions: reg,
		Pool:     pool,
	}
	return router, rt, pool, reg, root
}

func TestWarmUpRestoresFreshWorkspaceFromSlot(t *testing.T) {
	router, rt, pool, reg, _ := newWarmTestRouter(t)
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img", GatewayURL: "http://gw:8443", Route: "tier-a"}

	mount, err := router.Up(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.restored) != 1 || rt.restored[0] != "ws1" {
		t.Fatalf("restored = %v, want [ws1] (warm path must restore, not boot)", rt.restored)
	}
	if len(rt.ensured) != 0 {
		t.Fatalf("cold boot ran (%v) despite a successful warm claim", rt.ensured)
	}
	// The workspace was born as a clone of the golden snapshot.
	if _, err := os.Stat(filepath.Join(mount, "seeded.txt")); err != nil {
		t.Fatalf("workspace missing golden content: %v", err)
	}
	// The SLOT's frozen token was registered under the final workspace id
	// (never a fresh mint — env is part of the checkpoint).
	if ws := reg.registered["st-slot-token"]; ws != "ws1" {
		t.Fatalf("slot token registered for %q, want ws1", ws)
	}
	if rt.tokenSeen != "st-slot-token" {
		t.Fatalf("restore ran with token %q, want the slot's", rt.tokenSeen)
	}
	if !pool.consumed || pool.released {
		t.Fatalf("slot end state consumed=%v released=%v, want consumed only", pool.consumed, pool.released)
	}
	if pool.refills != 1 {
		t.Fatalf("refills=%d, want 1 after successful consume", pool.refills)
	}
}

func TestWarmUpSkipsExistingWorkspace(t *testing.T) {
	router, rt, pool, _, _ := newWarmTestRouter(t)
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img"}

	// First Up: workspace doesn't exist → warm. Second Up: exists → cold
	// (a resume's content can't match the golden checkpoint).
	if _, err := router.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pool.slot = &WarmSlot{ID: "slot2"} // pool would have inventory again
	pool.claimed = false
	if _, err := router.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if pool.claimed {
		t.Fatal("warm slot claimed for an existing workspace — restore content would not match")
	}
	if len(rt.ensured) != 1 {
		t.Fatalf("second Up must cold-boot exactly once, ensured=%v", rt.ensured)
	}
}

func TestWarmUpFailureFallsBackToColdBoot(t *testing.T) {
	router, rt, pool, reg, _ := newWarmTestRouter(t)
	rt.restoreErr = errors.New("restore exploded")
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img", GatewayURL: "http://gw:8443", Route: "tier-a"}

	mount, err := router.Up(ctx, spec)
	if err != nil {
		t.Fatalf("Up must succeed via cold fallback: %v", err)
	}
	if mount == "" {
		t.Fatal("no mount path from cold fallback")
	}
	if len(rt.ensured) != 1 || rt.ensured[0] != "ws1" {
		t.Fatalf("cold boot ensured=%v, want [ws1]", rt.ensured)
	}
	if !pool.released || pool.consumed {
		t.Fatalf("failed slot end state consumed=%v released=%v, want released only", pool.consumed, pool.released)
	}
	if pool.refills != 1 {
		t.Fatalf("refills=%d, want 1 after release of a failed claim", pool.refills)
	}
	// The slot token's registration must not survive the failed restore
	// (the cold path mints and registers its own).
	if len(reg.revoked) == 0 {
		t.Fatal("failed warm start left the slot token registered")
	}
}

func TestUpWithoutPoolUnchanged(t *testing.T) {
	router, rt, _ := newTestRouter(t)
	ctx := context.Background()
	if _, err := router.Up(ctx, WorkspaceSpec{ID: "ws1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if len(rt.ensured) != 1 {
		t.Fatalf("plain Up regressed: ensured=%v", rt.ensured)
	}
}
