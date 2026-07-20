package dataplane

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileLeaseAuthorityMonotonic(t *testing.T) {
	auth := &FileLeaseAuthority{Path: filepath.Join(t.TempDir(), "leases.json")}

	g1, err := auth.Acquire("ws1", "holder-a")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := auth.Acquire("ws1", "holder-b")
	if err != nil {
		t.Fatal(err)
	}
	if g2 <= g1 {
		t.Fatalf("generations not monotonic: %d then %d", g1, g2)
	}

	// Independent counters per workspace.
	gOther, err := auth.Acquire("ws2", "holder-a")
	if err != nil {
		t.Fatal(err)
	}
	if gOther != 1 {
		t.Fatalf("ws2 first generation = %d, want 1", gOther)
	}
}

func TestFileLeaseAuthorityConcurrent(t *testing.T) {
	auth := &FileLeaseAuthority{Path: filepath.Join(t.TempDir(), "leases.json")}

	const n = 50
	gens := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, err := auth.Acquire("ws1", "h")
			if err != nil {
				t.Error(err)
				return
			}
			gens[i] = g
		}(i)
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for _, g := range gens {
		if seen[g] {
			t.Fatalf("duplicate generation granted: %d", g)
		}
		seen[g] = true
	}
}

func TestFenceRejectsStaleGeneration(t *testing.T) {
	mount := t.TempDir()

	if err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1", Holder: "new", Generation: 5, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatalf("first fence write: %v", err)
	}

	// Same generation: fenced (a retry must have acquired a NEW generation).
	err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1", Holder: "dup", Generation: 5, GrantedAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("same-generation mount not fenced: %v", err)
	}

	// Older generation: fenced.
	err = FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1", Holder: "old", Generation: 3, GrantedAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("stale-generation mount not fenced: %v", err)
	}

	// Newer generation: allowed.
	if err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1", Holder: "newer", Generation: 6, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatalf("newer generation rejected: %v", err)
	}
}

// A clone inherits its parent's lease file with the snapshot; that token
// names the parent workspace and must not fence the branch's first lease.
func TestFenceIgnoresForeignWorkspaceToken(t *testing.T) {
	mount := t.TempDir()

	if err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1", Holder: "parent", Generation: 7, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Branch fences at generation 1 despite the parent's gen-7 token.
	if err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1-b1", Holder: "branch", Generation: 1, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatalf("parent token fenced the branch: %v", err)
	}

	// But the branch's own token now fences same-or-older branch attempts.
	err := FenceAndWrite(mount, Lease{
		WorkspaceID: "ws1-b1", Holder: "dup", Generation: 1, GrantedAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("branch not fenced against its own stale generation: %v", err)
	}
}
