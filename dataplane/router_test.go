package dataplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeRuntime records calls so tests can assert ordering guarantees without
// Docker. It also lets tests simulate a runtime failure.
type fakeRuntime struct {
	ensured []string
	stopped []string
	running map[string]uint64
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{running: map[string]uint64{}}
}

func (f *fakeRuntime) EnsureWorkspace(_ context.Context, spec WorkspaceSpec, mountPath string) error {
	// The contract: the lease file must already be on the volume when the
	// runtime is asked to start compute (storage → lease → compute, §7).
	if _, err := os.Stat(filepath.Join(mountPath, leaseFileName)); err != nil {
		panic("EnsureWorkspace called before lease was fenced onto the volume")
	}
	f.ensured = append(f.ensured, spec.ID)
	f.running[spec.ID] = spec.Generation
	return nil
}

func (f *fakeRuntime) StopWorkspace(_ context.Context, id string) error {
	f.stopped = append(f.stopped, id)
	delete(f.running, id)
	return nil
}

func (f *fakeRuntime) Status(context.Context) ([]WorkspaceStatus, error) {
	var out []WorkspaceStatus
	for id, gen := range f.running {
		out = append(out, WorkspaceStatus{ID: id, Generation: gen, State: "running"})
	}
	return out, nil
}

func newTestRouter(t *testing.T) (*Router, *fakeRuntime, string) {
	t.Helper()
	root := t.TempDir()
	rt := newFakeRuntime()
	return &Router{
		Storage: &DirStorage{Root: root},
		Leases:  &FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
		Runtime: rt,
	}, rt, root
}

func TestUpIsIdempotentAndFencesForward(t *testing.T) {
	router, rt, _ := newTestRouter(t)
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img"}

	mount1, err := router.Up(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	mount2, err := router.Up(ctx, spec) // retry / duplicate delivery
	if err != nil {
		t.Fatalf("second Up must succeed (idempotent path): %v", err)
	}
	if mount1 != mount2 {
		t.Fatalf("mount path changed across Up calls: %s vs %s", mount1, mount2)
	}
	if len(rt.ensured) != 2 {
		t.Fatalf("runtime ensured %d times, want 2 (each Up converges)", len(rt.ensured))
	}
}

func TestUpPropagatesLeaseGenerationToRuntime(t *testing.T) {
	router, rt, _ := newTestRouter(t)
	ctx := context.Background()
	spec := WorkspaceSpec{ID: "ws1", Image: "img"}

	if _, err := router.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Up(ctx, spec); err != nil { // second call: lease generation must advance
		t.Fatal(err)
	}

	statuses, err := rt.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Generation != 2 {
		t.Fatalf("container generation = %+v, want a single status at generation 2 "+
			"(the lease authority's second grant) — generation must reach the runtime label, "+
			"not stay pinned at the caller's zero value", statuses)
	}
}

func TestFanOutCreatesIndependentBranches(t *testing.T) {
	router, rt, _ := newTestRouter(t)
	ctx := context.Background()
	base := WorkspaceSpec{ID: "ws1", Image: "img"}

	mount, err := router.Up(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	// Seed workspace content that branches must inherit.
	if err := os.WriteFile(filepath.Join(mount, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	branches, err := router.FanOut(ctx, base, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 3 {
		t.Fatalf("got %d branches, want 3", len(branches))
	}

	// Every branch sandbox started, and each branch volume has the seeded
	// file plus its own (branch-scoped) lease.
	byID := map[string]bool{}
	for _, id := range rt.ensured {
		byID[id] = true
	}
	storage := router.Storage.(*DirStorage)
	for i, b := range branches {
		if !byID[b] {
			t.Fatalf("branch %s sandbox never started", b)
		}
		branchMount := filepath.Join(storage.wsDir("ws1"), "branches", "b"+string(rune('1'+i)))
		if _, err := os.Stat(filepath.Join(branchMount, "main.go")); err != nil {
			t.Fatalf("branch %s missing inherited file: %v", b, err)
		}
	}

	// Branch edits must not leak into the base workspace.
	b1 := filepath.Join(storage.wsDir("ws1"), "branches", "b1")
	if err := os.WriteFile(filepath.Join(b1, "hack.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, "hack.txt")); !os.IsNotExist(err) {
		t.Fatal("branch write leaked into base workspace")
	}
}

func TestDestroyStopsBranchesAndRemovesData(t *testing.T) {
	router, rt, root := newTestRouter(t)
	ctx := context.Background()
	base := WorkspaceSpec{ID: "ws1", Image: "img"}

	if _, err := router.Up(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := router.FanOut(ctx, base, 2); err != nil {
		t.Fatal(err)
	}
	if err := router.Destroy(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}

	stopped := map[string]bool{}
	for _, id := range rt.stopped {
		stopped[id] = true
	}
	for _, id := range []string{"ws1", "ws1-b1", "ws1-b2"} {
		if !stopped[id] {
			t.Fatalf("%s not stopped during destroy", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "ws1")); !os.IsNotExist(err) {
		t.Fatal("workspace data survived destroy")
	}
}

func TestSnapshotNameCollisionRejected(t *testing.T) {
	router, _, _ := newTestRouter(t)
	ctx := context.Background()

	if _, err := router.Up(ctx, WorkspaceSpec{ID: "ws1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := router.Storage.Snapshot(ctx, "ws1", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := router.Storage.Snapshot(ctx, "ws1", "s1"); err == nil {
		t.Fatal("duplicate snapshot name accepted; snapshots must be immutable history")
	}
}
