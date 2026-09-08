package dataplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionTokenFile is where the router persists the sandbox's gateway
// session token on the workspace volume. Hibernate needs it after the
// process is gone: the token is frozen into a runsc checkpoint and must be
// re-registered (same value) on resume — a fresh mint would not match the
// restored environment.
const SessionTokenFile = ".agent-state/session.token"

// ProcessHibernator is the optional Runtime capability for process-level
// scale-to-zero: checkpoint a running sandbox, then restore it later onto
// the *same* workspace volume (content already matches by construction).
// Distinct from the warm pool, which clones a golden snapshot into a
// brand-new workspace id.
type ProcessHibernator interface {
	// HibernateCheckpoint captures the running sandbox for wsID, tears it
	// down, and records token for resume. image lives under the runtime's
	// state dir (not under /workspace — writes there would diverge from
	// the checkpointed mount view).
	HibernateCheckpoint(ctx context.Context, wsID, token string) error
	// HibernateImage reports a usable checkpoint for wsID.
	HibernateImage(wsID string) (imageDir, token string, ok bool)
	// ClearHibernate drops a stored checkpoint (after successful resume or
	// when falling back to a cold boot).
	ClearHibernate(wsID string) error
}

func writeSessionToken(mountPath, token string) error {
	if token == "" {
		return nil
	}
	path := filepath.Join(mountPath, SessionTokenFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0o600)
}

func readSessionToken(mountPath string) (string, error) {
	b, err := os.ReadFile(filepath.Join(mountPath, SessionTokenFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// resumeHibernate attempts process-level warm resume for an existing
// workspace that was hibernated with a runsc checkpoint. handled=false
// means the caller should try the pool warm path or cold boot.
func (r *Router) resumeHibernate(ctx context.Context, spec WorkspaceSpec) (mountPath string, handled bool, err error) {
	hib, ok := r.Runtime.(ProcessHibernator)
	if !ok {
		return "", false, nil
	}
	restorer, ok := r.Runtime.(WarmRestorer)
	if !ok {
		return "", false, nil
	}
	exister, ok := r.Storage.(WorkspaceExister)
	if !ok || !exister.WorkspaceExists(ctx, spec.ID) {
		return "", false, nil
	}
	imageDir, token, ok := hib.HibernateImage(spec.ID)
	if !ok {
		return "", false, nil
	}

	discard := func(err error) (string, bool, error) {
		_ = hib.ClearHibernate(spec.ID)
		return "", true, err
	}

	mount, err := r.Storage.EnsureWorkspace(ctx, spec.ID, spec.QuotaGB)
	if err != nil {
		return discard(fmt.Errorf("storage: %w", err))
	}

	gen, err := r.Leases.Acquire(spec.ID, containerName(spec.ID))
	if err != nil {
		return discard(fmt.Errorf("lease: %w", err))
	}
	spec.Generation = gen
	spec.SessionToken = token

	// Token is known up front (frozen in the checkpoint). Overlap gateway
	// re-registration with fence + terminal install — same shape as warmUp.
	type regResult struct{ err error }
	regCh := make(chan regResult, 1)
	go func() {
		if spec.GatewayURL == "" {
			regCh <- regResult{}
			return
		}
		if r.Sessions == nil {
			regCh <- regResult{fmt.Errorf("workspace %s: GatewayURL set but no session registrar configured", spec.ID)}
			return
		}
		routes := splitRoutes(spec.Route)
		if len(routes) == 0 {
			regCh <- regResult{fmt.Errorf("workspace %s: GatewayURL set but no route (model tier) named", spec.ID)}
			return
		}
		regCh <- regResult{r.Sessions.Register(ctx, token, spec.ID, routes)}
	}()

	fenceErr := FenceAndWrite(mount, Lease{
		WorkspaceID: spec.ID,
		Holder:      containerName(spec.ID),
		Generation:  gen,
		GrantedAt:   time.Now().UTC(),
	})
	termErr := r.installTerminalToken(spec.ID, mount)
	reg := <-regCh

	if fenceErr != nil {
		if reg.err == nil && spec.GatewayURL != "" {
			r.revokeSessions(context.WithoutCancel(ctx), spec.ID)
		}
		return discard(fenceErr)
	}
	if termErr != nil {
		if reg.err == nil && spec.GatewayURL != "" {
			r.revokeSessions(context.WithoutCancel(ctx), spec.ID)
		}
		return discard(termErr)
	}
	if reg.err != nil {
		return discard(reg.err)
	}

	if err := restorer.RestoreWorkspace(ctx, spec, mount, imageDir); err != nil {
		r.revokeSessions(context.WithoutCancel(ctx), spec.ID)
		return discard(fmt.Errorf("hibernate restore: %w", err))
	}
	_ = hib.ClearHibernate(spec.ID)
	return mount, true, nil
}

// hibernateProcess checkpoints a running sandbox when the runtime supports
// it; otherwise stops it. Token comes from the volume (written at Up).
// Returns whether a process checkpoint was taken (volume snapshot still
// happens either way for RPO).
func (r *Router) hibernateProcess(ctx context.Context, wsID, mountPath string) (checkpointed bool, err error) {
	hib, ok := r.Runtime.(ProcessHibernator)
	if !ok {
		return false, r.Runtime.StopWorkspace(ctx, wsID)
	}
	token, err := readSessionToken(mountPath)
	if err != nil || token == "" {
		// Pre-hibernate workspaces or storage-only runs: degrade to stop.
		return false, r.Runtime.StopWorkspace(ctx, wsID)
	}
	if err := hib.HibernateCheckpoint(ctx, wsID, token); err != nil {
		// Checkpoint failed — still try a normal stop so scale-to-zero
		// does not leave a running sandbox behind.
		_ = r.Runtime.StopWorkspace(context.WithoutCancel(ctx), wsID)
		return false, err
	}
	return true, nil
}
