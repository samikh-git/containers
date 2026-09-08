//go:build linux

package dataplane

// CheckpointPool is the warm-start inventory (PERFORMANCE.md): pre-booted
// sandboxes captured as runsc checkpoints, restored onto a fresh workspace
// in ~200ms instead of a ~1.4-2.2s cold boot.
//
// One slot = one future sandbox, prepared entirely off the critical path:
//
//	fill:  mint token → fresh slot workspace → boot sandbox (gVisor) →
//	       wait agent listening → runsc checkpoint → zfs snapshot @golden
//	claim: rename slot dir (atomic reservation) → clone golden snapshot
//	       into the new workspace → register the SLOT's token at the
//	       gateway → runsc restore with rewritten bundle (new /workspace
//	       source, fresh netns)
//
// Two hard facts from the de-risking experiments shape this design:
//
//  1. restore requires /workspace CONTENT to match the checkpoint moment —
//     hence the golden snapshot + clone, and hence warm starts apply only
//     to workspaces that don't exist yet (never to resumes);
//  2. environment variables are frozen into the checkpoint — so the session
//     token cannot be injected at claim time. Instead every slot freezes its
//     OWN unique token at boot, inert until claim registers it with the
//     gateway bound to the real workspace id. Attribution and revocation
//     stay per-workspace; an unclaimed slot's token is worthless (§9).
//
// Slots are strictly single-use (the token uniqueness argument above), and
// consumed slots leave their origin dataset behind while clones of it live
// — ZFS forbids destroying a clone's origin. Drain removes what it can and
// reports the rest.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// slotMeta is the on-disk record for one slot (slot.json).
type slotMeta struct {
	ID         string    `json:"id"`
	Token      string    `json:"token"`
	Image      string    `json:"image"`
	ProfileDir string    `json:"profile_dir"`
	GatewayURL string    `json:"gateway_url"`
	Network    string    `json:"network"`
	Workspace  string    `json:"workspace"` // slot's golden workspace id ("warm-<id>")
	CreatedAt  time.Time `json:"created_at"`
}

type CheckpointPool struct {
	// Dir is the pool's state directory: <Dir>/slots/<id>/{slot.json,ckpt/}.
	Dir     string
	Storage Storage
	Runtime *GVisorRuntime
	// Template carries the sandbox parameters slots are built with (image,
	// profile, gateway, network, cpus, memory). Claim only matches specs
	// whose identity-relevant fields agree.
	Template WorkspaceSpec
	// BootTimeout bounds the wait for the golden sandbox's agent to come up
	// during fill. Default 90s.
	BootTimeout time.Duration

	// Auto-refill (serve mode): target ready slots; RequestRefill is a no-op
	// until EnableAutoRefill sets target > 0. One fill runs at a time; a
	// claim that lands mid-fill sets pending so another pass follows.
	mu      sync.Mutex
	target  int
	filling bool
	pending bool
	fillCtx context.Context
	cancel  context.CancelFunc
}

func (p *CheckpointPool) slotsDir() string { return filepath.Join(p.Dir, "slots") }

func slotWorkspaceID(slotID string) string { return "warm-" + slotID }

// EnableAutoRefill turns on background top-ups to target ready slots.
// serve mode calls this so each Consume/Release is followed by a refill;
// CLI `pool fill` stays the explicit path when auto-refill is off (target 0).
// ctx cancellation (serve shutdown) aborts an in-flight fill.
func (p *CheckpointPool) EnableAutoRefill(ctx context.Context, target int) {
	if target <= 0 {
		return
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.target = target
	p.fillCtx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()
	p.RequestRefill()
}

// StopAutoRefill cancels an in-flight background fill and disables further
// RequestRefill calls. Safe when auto-refill was never enabled.
func (p *CheckpointPool) StopAutoRefill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.target = 0
	p.pending = false
}

// RequestRefill schedules a background Fill to the configured target.
// No-op when auto-refill is disabled. Concurrent calls coalesce onto one
// fill; a claim during that fill queues a follow-up pass.
func (p *CheckpointPool) RequestRefill() {
	p.mu.Lock()
	if p.target <= 0 {
		p.mu.Unlock()
		return
	}
	if p.filling {
		p.pending = true
		p.mu.Unlock()
		return
	}
	p.filling = true
	p.pending = false
	target := p.target
	ctx := p.fillCtx
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Unlock()
	go p.runFill(ctx, target)
}

func (p *CheckpointPool) runFill(ctx context.Context, target int) {
	defer func() {
		p.mu.Lock()
		pending := p.pending
		p.filling = false
		p.pending = false
		stillOn := p.target > 0
		p.mu.Unlock()
		if !stillOn {
			return
		}
		if pending {
			p.RequestRefill()
			return
		}
		// Consume may have raced after Fill's readySlots snapshot — top up
		// again if we are still short.
		if have, err := p.readySlots(); err == nil {
			p.mu.Lock()
			t := p.target
			p.mu.Unlock()
			if t > 0 && len(have) < t {
				p.RequestRefill()
			}
		}
	}()

	fillCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	built, err := p.Fill(fillCtx, target)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Printf("warning: warm pool refill: %v\n", err)
		}
		return
	}
	if built > 0 {
		fmt.Printf("warm pool: refilled %d slot(s) (target %d)\n", built, target)
	}
}

// Fill tops the pool up to n ready slots. Builds up to maxParallelFill
// slots at a time so burst drain recovers faster without the resource
// spike of booting the whole target concurrently.
func (p *CheckpointPool) Fill(ctx context.Context, n int) (built int, err error) {
	have, err := p.readySlots()
	if err != nil {
		return 0, err
	}
	need := n - len(have)
	if need <= 0 {
		return 0, nil
	}

	const maxParallelFill = 2
	sem := make(chan struct{}, maxParallelFill)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for i := 0; i < need; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			mu.Lock()
			abort := firstErr != nil
			mu.Unlock()
			if abort {
				return
			}
			if err := p.buildSlot(ctx); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			built++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return built, firstErr
}

func (p *CheckpointPool) buildSlot(ctx context.Context) error {
	id, err := MintToken() // reuse the token generator for a random slot id
	if err != nil {
		return err
	}
	slotID := id[3:11] // 8 hex chars of the random token
	token, err := MintToken()
	if err != nil {
		return err
	}
	wsID := slotWorkspaceID(slotID)

	spec := p.Template
	spec.ID = wsID
	spec.Generation = 1
	spec.SessionToken = token // frozen into the checkpoint; inert until claim

	mount, err := p.Storage.EnsureWorkspace(ctx, wsID, spec.QuotaGB)
	if err != nil {
		return fmt.Errorf("slot storage: %w", err)
	}
	fail := func(err error) error {
		_ = p.Runtime.StopWorkspace(context.WithoutCancel(ctx), wsID)
		_ = p.Storage.DestroyWorkspace(context.WithoutCancel(ctx), wsID)
		return err
	}

	if err := p.Runtime.EnsureWorkspace(ctx, spec, mount); err != nil {
		return fail(fmt.Errorf("slot boot: %w", err))
	}

	// Wait for OpenCode to actually SERVE (HTTP 200, not just a bound
	// port) — the whole point of the warm pool is that all of init happens
	// now, not after the claim's restore.
	timeout := p.BootTimeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for !p.Runtime.AgentReady(ctx, wsID) {
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("slot boot: agent not serving after %s", timeout))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}

	slotDir := filepath.Join(p.slotsDir(), slotID)
	ckptDir := filepath.Join(slotDir, "ckpt")
	if err := os.MkdirAll(ckptDir, 0o755); err != nil {
		return fail(err)
	}
	if err := p.Runtime.CheckpointWorkspace(ctx, wsID, ckptDir); err != nil {
		_ = os.RemoveAll(slotDir)
		return fail(fmt.Errorf("slot checkpoint: %w", err))
	}
	// Sandbox is stopped now; freeze the exact content the checkpoint saw.
	if err := p.Storage.Snapshot(ctx, wsID, goldenSnapshot); err != nil {
		_ = os.RemoveAll(slotDir)
		return fail(fmt.Errorf("slot snapshot: %w", err))
	}

	meta := slotMeta{
		ID: slotID, Token: token,
		Image: spec.Image, ProfileDir: spec.ProfileDir,
		GatewayURL: spec.GatewayURL, Network: spec.Network,
		Workspace: wsID, CreatedAt: time.Now().UTC(),
	}
	b, _ := json.MarshalIndent(meta, "", " ")
	// slot.json is written LAST: a slot without it is invisible to Claim,
	// so a crash mid-build never yields a half-usable slot.
	if err := os.WriteFile(filepath.Join(slotDir, "slot.json"), b, 0o644); err != nil {
		_ = os.RemoveAll(slotDir)
		return fail(err)
	}
	return nil
}

func (p *CheckpointPool) readySlots() ([]slotMeta, error) {
	entries, err := os.ReadDir(p.slotsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []slotMeta
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		meta, err := readSlotMeta(filepath.Join(p.slotsDir(), e.Name()))
		if err != nil {
			continue // half-built or corrupt — ignored, cleaned by drain
		}
		out = append(out, meta)
	}
	return out, nil
}

func readSlotMeta(dir string) (slotMeta, error) {
	var meta slotMeta
	b, err := os.ReadFile(filepath.Join(dir, "slot.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// matches reports whether a slot's frozen identity agrees with the spec.
// Image, profile, gateway and network are all baked into the checkpoint
// (env, mounts, netstack) and must agree exactly; cpus/memory are host-side
// cgroup facts applied fresh at restore, so they don't gate a match.
func (m slotMeta) matches(spec WorkspaceSpec) bool {
	return m.Image == spec.Image &&
		m.ProfileDir == spec.ProfileDir &&
		m.GatewayURL == spec.GatewayURL &&
		m.Network == spec.Network
}

// Claim reserves a matching slot. Reservation is an atomic directory rename
// (".claimed-<id>"), so concurrent routers never double-claim.
func (p *CheckpointPool) Claim(ctx context.Context, spec WorkspaceSpec) (*WarmSlot, bool) {
	slots, err := p.readySlots()
	if err != nil {
		return nil, false
	}
	for _, meta := range slots {
		if !meta.matches(spec) {
			continue
		}
		src := filepath.Join(p.slotsDir(), meta.ID)
		dst := filepath.Join(p.slotsDir(), ".claimed-"+meta.ID)
		if err := os.Rename(src, dst); err != nil {
			continue // raced with another claimer — try the next slot
		}
		return &WarmSlot{
			ID:              meta.ID,
			Token:           meta.Token,
			SourceWorkspace: meta.Workspace,
			Snapshot:        goldenSnapshot,
			ImageDir:        filepath.Join(dst, "ckpt"),
			dir:             dst,
		}, true
	}
	return nil, false
}

// Consume finalizes a successful restore: the checkpoint image is dead
// weight now (single-use token), but the golden dataset stays behind as
// the clone's ZFS origin — a small record of it is kept under consumed/
// so Drain can garbage-collect the origin once its clones are gone.
func (p *CheckpointPool) Consume(slot *WarmSlot) {
	if meta, err := readSlotMeta(slot.dir); err == nil {
		if err := os.MkdirAll(filepath.Join(p.Dir, "consumed"), 0o755); err == nil {
			b, _ := json.Marshal(meta)
			_ = os.WriteFile(filepath.Join(p.Dir, "consumed", meta.ID+".json"), b, 0o644)
		}
	}
	_ = os.RemoveAll(slot.dir)
}

// Release discards a slot whose claim failed. The slot is destroyed rather
// than returned to the pool: its token may already be registered, and a
// restore that failed once is not inventory worth keeping. If the origin
// dataset can't go yet (the claim's clone may live on as a cold-boot
// workspace), it is recorded for Drain's garbage collection.
func (p *CheckpointPool) Release(ctx context.Context, slot *WarmSlot) {
	meta, metaErr := readSlotMeta(slot.dir)
	_ = os.RemoveAll(slot.dir)
	if err := p.Storage.DestroyWorkspace(ctx, slot.SourceWorkspace); err != nil && metaErr == nil {
		if mkErr := os.MkdirAll(filepath.Join(p.Dir, "consumed"), 0o755); mkErr == nil {
			b, _ := json.Marshal(meta)
			_ = os.WriteFile(filepath.Join(p.Dir, "consumed", meta.ID+".json"), b, 0o644)
		}
	}
}

// SlotInfo is the pool status row (CLI `pool status`).
type SlotInfo struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Network   string    `json:"network"`
	CreatedAt time.Time `json:"created_at"`
}

func (p *CheckpointPool) Status() ([]SlotInfo, error) {
	slots, err := p.readySlots()
	if err != nil {
		return nil, err
	}
	var out []SlotInfo
	for _, m := range slots {
		out = append(out, SlotInfo{ID: m.ID, Image: m.Image, Network: m.Network, CreatedAt: m.CreatedAt})
	}
	return out, nil
}

// Drain destroys every ready slot, then garbage-collects consumed slots'
// origin datasets. Origins still referenced by live clones survive (ZFS
// refuses to destroy them) and their records are kept for a later drain.
func (p *CheckpointPool) Drain(ctx context.Context) error {
	entries, err := os.ReadDir(p.slotsDir())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(p.slotsDir(), e.Name())
		if meta, err := readSlotMeta(dir); err == nil {
			if derr := p.Storage.DestroyWorkspace(ctx, meta.Workspace); derr != nil {
				fmt.Printf("note: keeping golden dataset for slot %s (live clones?): %v\n", meta.ID, derr)
			}
		}
		_ = os.RemoveAll(dir)
	}
	// Consumed-slot origins: destroyable once their workspace clones are
	// promoted or gone.
	consumed, err := os.ReadDir(filepath.Join(p.Dir, "consumed"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range consumed {
		rec := filepath.Join(p.Dir, "consumed", e.Name())
		var meta slotMeta
		b, err := os.ReadFile(rec)
		if err != nil || json.Unmarshal(b, &meta) != nil {
			_ = os.Remove(rec)
			continue
		}
		if derr := p.Storage.DestroyWorkspace(ctx, meta.Workspace); derr != nil {
			fmt.Printf("note: keeping golden dataset for consumed slot %s (live clones): %v\n", meta.ID, derr)
			continue
		}
		_ = os.Remove(rec)
	}
	return nil
}
