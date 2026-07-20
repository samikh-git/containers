package dataplane

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// splitRoutes parses a comma-separated route list into trimmed, non-empty
// names, preserving order (precedence for the gateway's per-model selection).
func splitRoutes(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// Router ties the three layers together in the order the design requires:
// storage before lease, lease before compute (§7). It is the local-mode
// core; the team/enterprise reconciler drives these same methods from a
// desired-state watch stream instead of a CLI.
type Router struct {
	Storage Storage
	Leases  LeaseAuthority
	Runtime Runtime
	// Sessions registers sandbox tokens at the policy gateway (§9). Nil in
	// storage-only flows; required when a spec carries a GatewayURL.
	Sessions SessionRegistrar
	// Pool, when set, enables the warm-start fast path for workspaces that
	// don't exist yet: restore a pre-booted checkpoint instead of a cold
	// boot (see warm.go). Requires a Runtime implementing WarmRestorer and
	// a Storage implementing SnapshotCloner.
	Pool WarmPool
}

// prepareSession mints and registers the sandbox's session token when a
// gateway is in play. Called with the FINAL workspace id (branch ids get
// their own tokens — attribution in the ledger is per-branch).
func (r *Router) prepareSession(ctx context.Context, spec *WorkspaceSpec) error {
	if spec.GatewayURL == "" {
		return nil
	}
	if r.Sessions == nil {
		return fmt.Errorf("workspace %s: GatewayURL set but no session registrar configured", spec.ID)
	}
	routes := splitRoutes(spec.Route)
	if len(routes) == 0 {
		return fmt.Errorf("workspace %s: GatewayURL set but no route (model tier) named", spec.ID)
	}
	token, err := MintToken()
	if err != nil {
		return err
	}
	if err := r.Sessions.Register(ctx, token, spec.ID, routes); err != nil {
		return err
	}
	spec.SessionToken = token
	return nil
}

func (r *Router) revokeSessions(ctx context.Context, wsID string) {
	if r.Sessions == nil {
		return
	}
	// Best-effort: a failed revoke must not block a stop, but it must be
	// loud — an unrevoked token is a policy gap until the gateway restarts.
	if err := r.Sessions.RevokeWorkspace(ctx, wsID); err != nil {
		fmt.Printf("warning: revoking sessions for %s: %v\n", wsID, err)
	}
}

// Up ensures a workspace exists and its sandbox is running at the spec's
// generation. Safe to call any number of times.
func (r *Router) Up(ctx context.Context, spec WorkspaceSpec) (mountPath string, err error) {
	// Warm-start fast path: a fresh workspace with a matching pool slot is
	// restored from checkpoint. A failed claim falls through to the cold
	// path (loudly) — warm start is an optimization, never a gate.
	if mount, handled, werr := r.warmUp(ctx, spec); handled {
		if werr == nil {
			return mount, nil
		}
		fmt.Printf("warning: warm start of %s failed, cold-booting instead: %v\n", spec.ID, werr)
	}

	mountPath, err = r.Storage.EnsureWorkspace(ctx, spec.ID, spec.QuotaGB)
	if err != nil {
		return "", fmt.Errorf("storage: %w", err)
	}

	gen, err := r.Leases.Acquire(spec.ID, containerName(spec.ID))
	if err != nil {
		return "", fmt.Errorf("lease: %w", err)
	}
	spec.Generation = gen // must reach the container label — it's what Status()
	// and any future reconciler use to tell actual state apart from stale (§5).

	// Session minting is independent of the stop+fence sequence, so the two
	// overlap. Both must finish before compute: the token travels into the
	// container's environment, and the fence write stays the last gate
	// before compute (§7).
	sessCh := make(chan error, 1)
	go func() { sessCh <- r.prepareSession(ctx, &spec) }()

	// The authority's grant implies the prior holder is to be stopped; do it
	// before fencing so the fence write is the last gate before compute.
	stopFenceErr := func() error {
		if err := r.stopPriorHolder(ctx, spec.ID); err != nil {
			return fmt.Errorf("stop prior holder: %w", err)
		}
		return FenceAndWrite(mountPath, Lease{
			WorkspaceID: spec.ID,
			Holder:      containerName(spec.ID),
			Generation:  gen,
			GrantedAt:   time.Now().UTC(),
		})
	}()
	sessErr := <-sessCh
	if stopFenceErr != nil {
		if sessErr == nil {
			// The session registered but its sandbox will never start —
			// don't leave the credential live.
			r.revokeSessions(ctx, spec.ID)
		}
		return "", stopFenceErr
	}
	if sessErr != nil {
		return "", sessErr
	}

	if err := r.Runtime.EnsureWorkspace(ctx, spec, mountPath); err != nil {
		return "", fmt.Errorf("runtime: %w", err)
	}
	return mountPath, nil
}

// stopPriorHolder stops the workspace's existing container, if any. The
// existence check matters for cold-start latency: StopWorkspace costs two
// daemon round trips (stop + rm) whether or not a container exists, and the
// common cold-start case has none.
func (r *Router) stopPriorHolder(ctx context.Context, wsID string) error {
	statuses, err := r.Runtime.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range statuses {
		if s.ID == wsID {
			return r.Runtime.StopWorkspace(ctx, wsID)
		}
	}
	return nil
}

// FanOut snapshots the workspace and creates n agent branches from that
// snapshot, each with its own volume clone, lease, and sandbox (§7). Branch
// workspace IDs are "<wsID>-b<i>"; snapshot name records the moment.
func (r *Router) FanOut(ctx context.Context, base WorkspaceSpec, n int) ([]string, error) {
	snap := "fanout-" + time.Now().UTC().Format("20060102-150405")
	if err := r.Storage.Snapshot(ctx, base.ID, snap); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	var branches []string
	for i := 1; i <= n; i++ {
		branchID := fmt.Sprintf("b%d", i)
		branchWS := base.ID + "-" + branchID

		mount, err := r.Storage.CloneBranch(ctx, base.ID, snap, branchID)
		if err != nil {
			return branches, fmt.Errorf("clone %s: %w", branchID, err)
		}

		gen, err := r.Leases.Acquire(branchWS, containerName(branchWS))
		if err != nil {
			return branches, fmt.Errorf("lease %s: %w", branchWS, err)
		}
		if err := FenceAndWrite(mount, Lease{
			WorkspaceID: branchWS,
			Holder:      containerName(branchWS),
			Generation:  gen,
			GrantedAt:   time.Now().UTC(),
		}); err != nil {
			return branches, err
		}

		spec := base
		spec.ID = branchWS
		spec.Generation = gen
		spec.SessionToken = "" // each branch mints its own (per-branch ledger attribution)
		if err := r.prepareSession(ctx, &spec); err != nil {
			return branches, err
		}
		if err := r.Runtime.EnsureWorkspace(ctx, spec, mount); err != nil {
			return branches, fmt.Errorf("runtime %s: %w", branchWS, err)
		}
		branches = append(branches, branchWS)
	}
	return branches, nil
}

// Down stops the sandbox; data stays on the volume (scale-to-zero, §7).
// The workspace's gateway sessions are revoked: a stopped sandbox must not
// leave a live credential behind.
func (r *Router) Down(ctx context.Context, wsID string) error {
	if err := r.Runtime.StopWorkspace(ctx, wsID); err != nil {
		return err
	}
	r.revokeSessions(ctx, wsID)
	return nil
}

// Hibernate is the scale-to-zero stop (§7): graceful stop (SIGTERM →
// checkpoint hook → grace → kill, implemented by the runtime), then a
// snapshot of the volume, then session revocation. For branch workspaces
// the snapshot may fail (clones snapshot under their parent); that is
// tolerated — the clone's data persists regardless.
func (r *Router) Hibernate(ctx context.Context, wsID string) error {
	if err := r.Runtime.StopWorkspace(ctx, wsID); err != nil {
		return err
	}
	snap := "hibernate-" + time.Now().UTC().Format("20060102-150405")
	if err := r.Storage.Snapshot(ctx, wsID, snap); err != nil {
		fmt.Printf("note: no hibernate snapshot for %s: %v\n", wsID, err)
	}
	r.revokeSessions(ctx, wsID)
	return nil
}

// Destroy stops the sandbox and removes the workspace's data, snapshots and
// branches. Branch sandboxes are stopped first.
func (r *Router) Destroy(ctx context.Context, wsID string) error {
	branches, err := r.Storage.ListBranches(ctx, wsID)
	if err != nil {
		return err
	}
	for _, b := range branches {
		if err := r.Runtime.StopWorkspace(ctx, wsID+"-"+b); err != nil {
			return err
		}
		r.revokeSessions(ctx, wsID+"-"+b)
	}
	if err := r.Runtime.StopWorkspace(ctx, wsID); err != nil {
		return err
	}
	r.revokeSessions(ctx, wsID)
	return r.Storage.DestroyWorkspace(ctx, wsID)
}
