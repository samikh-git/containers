package dataplane

import (
	"context"
	"fmt"
	"time"
)

// The warm-start seam, portable across platforms (the one real
// implementation, CheckpointPool + GVisorRuntime, is linux-only).

// goldenSnapshot names the slot workspace's snapshot of the exact content
// the checkpoint saw — the only valid /workspace origin for a restore.
const goldenSnapshot = "golden"

// WarmSlot is one claimed unit of pool inventory: a checkpoint image, the
// golden workspace snapshot its /workspace content must be cloned from, and
// the session token frozen inside it at slot-boot time.
type WarmSlot struct {
	ID              string
	Token           string
	SourceWorkspace string // workspace holding the golden snapshot
	Snapshot        string
	ImageDir        string // runsc checkpoint image directory

	dir string // claimed slot directory (pool-internal)
}

// WarmPool hands out slots. Claim's reservation must be atomic across
// processes; a claimed slot ends in exactly one of Consume (restore
// succeeded) or Release (discard).
type WarmPool interface {
	Claim(ctx context.Context, spec WorkspaceSpec) (*WarmSlot, bool)
	Consume(slot *WarmSlot)
	Release(ctx context.Context, slot *WarmSlot)
}

// WarmRefiller is the optional WarmPool capability for keeping inventory
// topped up. serve mode enables it so a claim (Consume or Release) kicks a
// background Fill back to the configured target; CLI `up` leaves it off.
type WarmRefiller interface {
	RequestRefill()
}

// WarmRestorer is the optional Runtime capability warm starts need:
// start a sandbox from a checkpoint image instead of booting it.
type WarmRestorer interface {
	RestoreWorkspace(ctx context.Context, spec WorkspaceSpec, mountPath, imageDir string) error
}

func requestRefill(pool WarmPool) {
	if rf, ok := pool.(WarmRefiller); ok {
		rf.RequestRefill()
	}
}

// warmUp attempts the warm-start fast path. Returns handled=false when the
// path doesn't apply (no pool, wrong backend capabilities, workspace
// already exists, no matching slot) — the caller falls through to the cold
// path with nothing changed. Once a slot is claimed, handled=true: a
// failure discards the slot and reports the error, and the caller may still
// cold-boot (the cloned volume, if any, is a valid empty-ish workspace).
//
// Ordering matches the cold path's invariants (§7): storage first (clone),
// then lease, then fence, then compute (restore). The fence write lands on
// the cloned volume BEFORE restore; the .lease file did not exist at
// checkpoint time and new files in /workspace are fine — only content that
// the checkpoint saw must be unchanged.
func (r *Router) warmUp(ctx context.Context, spec WorkspaceSpec) (mountPath string, handled bool, err error) {
	if r.Pool == nil {
		return "", false, nil
	}
	restorer, ok := r.Runtime.(WarmRestorer)
	if !ok {
		return "", false, nil
	}
	cloner, ok := r.Storage.(SnapshotCloner)
	if !ok {
		return "", false, nil
	}
	exister, ok := r.Storage.(WorkspaceExister)
	if !ok || exister.WorkspaceExists(ctx, spec.ID) {
		// Existing workspace = resume; its content cannot match the golden
		// checkpoint. Cold path only.
		return "", false, nil
	}

	slot, ok := r.Pool.Claim(ctx, spec)
	if !ok {
		return "", false, nil
	}
	discard := func(err error) (string, bool, error) {
		r.Pool.Release(context.WithoutCancel(ctx), slot)
		requestRefill(r.Pool) // Release destroys inventory; refill if serve enabled it
		return "", true, err
	}

	mount, err := cloner.CloneWorkspaceFrom(ctx, slot.SourceWorkspace, slot.Snapshot, spec.ID)
	if err != nil {
		return discard(fmt.Errorf("warm clone: %w", err))
	}

	gen, err := r.Leases.Acquire(spec.ID, containerName(spec.ID))
	if err != nil {
		return discard(fmt.Errorf("lease: %w", err))
	}
	spec.Generation = gen
	if err := FenceAndWrite(mount, Lease{
		WorkspaceID: spec.ID,
		Holder:      containerName(spec.ID),
		Generation:  gen,
		GrantedAt:   time.Now().UTC(),
	}); err != nil {
		return discard(err)
	}

	// The slot's frozen token becomes this workspace's credential: register
	// it under the FINAL workspace id so ledger attribution and revocation
	// stay per-workspace (§9). No new mint — the sandbox's environment is
	// part of the checkpoint and cannot change.
	if spec.GatewayURL != "" {
		if r.Sessions == nil {
			return discard(fmt.Errorf("workspace %s: GatewayURL set but no session registrar configured", spec.ID))
		}
		routes := splitRoutes(spec.Route)
		if len(routes) == 0 {
			return discard(fmt.Errorf("workspace %s: GatewayURL set but no route (model tier) named", spec.ID))
		}
		if err := r.Sessions.Register(ctx, slot.Token, spec.ID, routes); err != nil {
			return discard(err)
		}
	}
	spec.SessionToken = slot.Token

	if err := restorer.RestoreWorkspace(ctx, spec, mount, slot.ImageDir); err != nil {
		r.revokeSessions(context.WithoutCancel(ctx), spec.ID)
		return discard(fmt.Errorf("warm restore: %w", err))
	}
	r.Pool.Consume(slot)
	requestRefill(r.Pool)
	return mount, true, nil
}
