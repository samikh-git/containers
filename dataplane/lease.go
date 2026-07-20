package dataplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Lease fencing (DESIGN §7): every mount of a workspace volume is guarded by
// a generation-numbered lease recorded in two places — the lease authority
// (control plane; in local mode a host-local file) and on the volume itself.
// A mount may only proceed when its generation is strictly newer than the
// one on disk, which makes two concurrent writers impossible by construction.

// Lease is the on-disk fencing token, written at the volume root.
type Lease struct {
	WorkspaceID string    `json:"workspace_id"`
	Holder      string    `json:"holder"`
	Generation  uint64    `json:"generation"`
	GrantedAt   time.Time `json:"granted_at"`
}

const leaseFileName = ".lease"

// LeaseAuthority grants monotonically increasing lease generations per
// workspace. In local mode this is a file under the storage root; in
// team/enterprise mode the same interface is served by the control plane
// over gRPC.
type LeaseAuthority interface {
	// Acquire grants the next generation for the workspace to holder.
	// Granting implies the previous holder must already be stopped — the
	// caller (router) is responsible for stopping it first.
	Acquire(wsID, holder string) (uint64, error)
}

// FileLeaseAuthority is the local-mode authority: one JSON file of counters.
type FileLeaseAuthority struct {
	Path string
	mu   sync.Mutex
}

func (f *FileLeaseAuthority) Acquire(wsID, holder string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	counters := map[string]uint64{}
	if b, err := os.ReadFile(f.Path); err == nil {
		if err := json.Unmarshal(b, &counters); err != nil {
			return 0, fmt.Errorf("lease state corrupt at %s: %w", f.Path, err)
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}

	counters[wsID]++
	gen := counters[wsID]

	b, err := json.MarshalIndent(counters, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return 0, err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		return 0, err
	}
	return gen, nil
}

// FenceAndWrite validates the granted generation against the volume's
// on-disk lease and, if strictly newer, records the new lease. A failure
// here means something newer holds (or held) this volume: the caller must
// abort before any container is created.
//
// The fence is scoped to the lease's WorkspaceID: a clone inherits its
// parent's lease file along with the rest of the snapshot, and that token
// names the parent, not the branch. A foreign token is replaced, not
// honored — fencing exists to serialize writers of ONE volume identity.
func FenceAndWrite(mountPath string, l Lease) error {
	p := filepath.Join(mountPath, leaseFileName)
	if b, err := os.ReadFile(p); err == nil {
		var prev Lease
		if err := json.Unmarshal(b, &prev); err == nil &&
			prev.WorkspaceID == l.WorkspaceID && prev.Generation >= l.Generation {
			return fmt.Errorf("fenced: volume %s holds generation %d (holder %q), requested %d",
				mountPath, prev.Generation, prev.Holder, l.Generation)
		}
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
