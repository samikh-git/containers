# Performance Improvements

Current cold start path and proposed optimizations, ordered by impact.

## Implemented

### Fewer Docker round trips on the cold-start path

The no-prior-container cold start previously spent four docker CLI calls
before the container ran: an unconditional `docker stop` + `docker rm -f`
in `Up()`, then a `docker ps -a` idempotency check inside `EnsureWorkspace`,
then `docker run`. Now:

- `Up()` checks `Status()` once and only calls `StopWorkspace` when a
  container for the workspace actually exists (`stopPriorHolder`,
  `dataplane/router.go`).
- `DockerRuntime.EnsureWorkspace` runs `docker run` optimistically; the
  idempotency check (same generation running → no-op, otherwise replace)
  moved into the name-conflict fallback path (`dataplane/docker.go`).

Common cold start: 2 daemon round trips instead of 4 (~100-250ms saved).

### Session minting overlapped with stop + fence (item 3, corrected)

The original proposal — parallelize `prepareSession` with
`Runtime.EnsureWorkspace` — was wrong: the container *does* depend on the
session (`prepareSession` sets `spec.SessionToken`, which `EnsureWorkspace`
injects as `OPENCODE_SESSION_TOKEN`). Racing them could start a sandbox with
no credential.

Instead, `Up()` overlaps session minting with the stop-prior-holder + fence
sequence, and joins both before compute starts. Same savings, no race — and
when a prior container exists, the session POST hides entirely behind the
much longer stop. If stop/fence fails after the session registered, the
token is revoked rather than leaked.

### Concurrent idle probes

`IdleMonitor.Tick` probed workspaces sequentially, and `docker stats
--no-stream` blocks ~1.5-2s per call (it samples two intervals) — tick
latency scaled linearly with workspace count (10 workspaces ≈ 20s+ per
tick). Probes now run concurrently across workspaces; `lastActive`
bookkeeping and hibernation stay in the main goroutine.

## Current Cold Start Timeline

```
Storage (zfs create)              ~50-200ms
Lease (read/write JSON)           ~5ms
Stop prior (docker stop -t 30)    ~0-30s  (only if prior container exists)
Fence (read/write .lease)         ~1ms
Session (HTTP POST to gateway)    ~5-20ms
docker ps -a (idempotency check)  ~50-150ms
docker run -d                     ~200-800ms  (image pre-pulled)
  └─ dockerd -> containerd -> runsc -> entrypoint -> opencode serve
opencode serve (Node.js startup)  ~500ms-2s
                                  ─────────────────────────────────
Total (no prior container)        ~1-3s
```

Bottleneck breakdown:
- `opencode serve` (Node.js cold start): 500ms-2s — largest single contributor
- Docker daemon chain (CLI -> dockerd -> containerd -> runsc): 200-800ms
- ZFS create (first time): 50-200ms (subsequent calls are fast)
- Everything else: <30ms combined

## Proposed Optimizations

### 1. Pre-Warmed Pool — DONE (checkpoint/restore, VM-verified)

Implemented as gVisor checkpoint/restore, not pause/unpause — the original
sketch was unimplementable (see the caveat kept below for history). Three
de-risking experiments in the VM settled the design:

- **Mount-propagation slots don't work**: gVisor's gofer snapshots the
  mount tree at boot; host mounts added under an existing bind afterwards
  are invisible inside the sandbox. (Kills the "slots" variant.)
- **Restore can rewrite the /workspace bind source**, provided the new
  source's *content* matches the checkpoint moment — which a ZFS clone of
  a snapshot taken at checkpoint time satisfies by construction.
- **Restore re-scrapes the netns**: restored into a fresh netns with a new
  CNI-assigned veth/IP, the netstack comes up on the new address (~200ms to
  the agent listening). Namespaces are single-use: runsc consumes the
  veth's address and never puts it back.

Shipped pieces:

- `dataplane/gvisor.go` — `GVisorRuntime` (`--runtime gvisor`): raw runsc
  against OCI bundles. No dockerd, no containerd task, no per-container
  snapshot (one shared read-only rootfs dir per image — the image contract
  sends all writes to /workspace and /tmp). The only runtime that can
  checkpoint/restore; also the fastest cold boot. Full surface: endpoints,
  ActivityProbes (runsc events --stats), graceful drain.
- `dataplane/warmpool.go` — `CheckpointPool` (`router pool fill|status|
  drain`): a slot = fresh workspace dataset + booted sandbox + `runsc
  checkpoint` (~235MB) + `zfs snapshot @golden`, built off the critical
  path. slot.json written last (crash-safe); claims are atomic dir renames.
  `router serve --pool-n N` enables auto-refill: after each Consume/Release,
  a background Fill tops the pool back up to N (coalesced; one fill at a
  time). CLI `up` leaves auto-refill off.
- `dataplane/warm.go` — `Router.Up` fast path: fresh workspace + matching
  slot → clone golden snapshot → lease + fence → register the slot's token
  → restore. Any failure discards the slot and falls back to cold boot.

**The session-token problem** (env is frozen into the checkpoint, so a
token can't be injected at claim): every slot freezes its *own* unique
token at boot, inert until claim registers it at the gateway under the
real workspace id. Attribution and revocation stay per-workspace; no
harness changes needed. Restores only apply to workspaces that don't exist
yet — a resume's content can't match the golden checkpoint.

**Measured in the VM (2026-07-19, ZFS + wsnet CNI + gateway session)**:

```
                         router up    agent HTTP 200 (end-to-end)
warm (restore)           ~600-760ms   ~630-780ms
cold (same runtime)      ~240-300ms   ~2.6-2.8s
```

Warm start beats cold end-to-end by ~4x; the checkpoint is taken only
after the agent *serves* HTTP 200 (not merely listens), so a restored
sandbox answers immediately. Slot build cost: ~4s each, off the critical
path. Consumed slots leave their golden dataset behind as the clone's ZFS
origin; `pool drain` garbage-collects origins once their clones are gone.

**Historical design caveat (why pause/unpause was unworkable)**: Docker,
containerd, and runsc cannot attach a bind mount — or set env vars like
the session token — on an already-created container; both are fixed at
create time, and pause/unpause does not allow reconfiguration. The network
is also per-spec.

### 2. Containerd Client Library — DONE (verified in the VM)

Implemented as an *additive* runtime: `dataplane/containerd.go`
(`//go:build linux`) talks to containerd directly (`--runtime containerd`
for gVisor, `containerd-runc` without) in its own `dataplane` namespace on
the containerd instance Docker CE already runs. `DockerRuntime` stays as the
dev-path runtime — Docker Desktop for Mac cannot reach the containerd
socket, and it carries the bind-mount warm-up workaround.

Covers the full surface, not just the 3-method `Runtime` interface:
`AgentEndpoint` resolves the sandbox's CNI-assigned IP (recorded as a
container label), and the runtime implements `ActivityProbes` itself — CPU
via the containerd metrics API with delta sampling between ticks (item 4,
also done on this path: no process spawn, no ~2s `docker stats`
double-sample block), `/busy` probed over HTTP from the host instead of
`docker exec curl`.

Networking is CNI (containerd has none built in): a named `--network` maps
to `/etc/cni/net.d/<name>.conflist`; provision.sh writes an isolated bridge
for `wsnet`. Images must be imported
once from dockerd's store (`docker save … | ctr -n dataplane images import -`).

Verified end-to-end in the Lima VM (2026-07-18): cold start ~220ms with a
gateway session (vs ~1-3s on the Docker path), replace-running-holder
~450ms, clean teardown. One environment finding: the dedicated gVisor shim
(io.containerd.runsc.v1) hangs task creation against containerd 2.x, so
gVisor is driven through the runc v2 shim with BinaryName=runsc — dockerd's
own mechanism (see dataplane-vm/README.md).

### 3. Parallelize Session Minting with Container Creation — DONE (corrected)

Implemented in corrected form; see "Implemented" above. The proposal as
originally written here was a bug: the container depends on the session
token (`OPENCODE_SESSION_TOKEN` env var), so session minting and container
creation cannot race. Session minting now overlaps the stop-prior-holder +
fence sequence instead.

### 4. Replace `docker stats` with Containerd Metrics — DONE (part of item 2)

`ContainerdRuntime` implements `ActivityProbes` via containerd's metrics
API: CPU usage is delta-sampled between watch ticks — no process spawn, and
no ~2s blocking double-sample per workspace like `docker stats --no-stream`.
The docker runtimes keep shelling out via `DockerProbes`; `router watch`
picks whichever the active runtime provides.

### 5. Replace Node.js Agent Harness with Go Binary

**Impact**: `opencode serve` startup drops from ~500ms-2s to ~10-50ms
**Effort**: Weeks-months (external dependency)
**Priority**: Low (pre-warmed pool solves this without code changes)

`opencode-ai` is an npm package. Replacing it with a Go binary eliminates the
Node.js runtime from the startup path. However, this requires either forking
the agent harness or contributing a Go implementation upstream.

The pre-warmed container pool (item 1) achieves the same result (zero effective
cold start) without touching the agent harness. This item is only worth pursuing
long-term if Node.js startup becomes a bottleneck even with pre-warming
(e.g. pool exhaustion under burst load).

Note: Node.js must remain in the sandbox image as a toolchain for agents that
need it (npm, JS/TS development). Only the agent harness server itself would
be replaced.

## Summary

| # | Optimization | Impact | Effort | Status |
|---|---|---|---|---|
| — | Fewer Docker round trips (Up + EnsureWorkspace) | ~100-250ms per cold start | — | Done |
| — | Concurrent idle probes | Tick no longer scales with workspace count | — | Done |
| 3 | Overlap session minting with stop + fence | ~5-20ms (more with a prior container) | — | Done (corrected) |
| 1 | Pre-warmed pool (checkpoint/restore) | Agent-ready ~700ms vs ~2.6-2.8s (measured) | — | Done (VM-verified) |
| 2 | Containerd runtime (additive) | Cold start ~220ms vs ~1-3s (measured) | — | Done (VM-verified) |
| 4 | Containerd metrics for idle | No process spawn per tick | — | Done (part of 2) |
| 5 | Go agent harness | Node.js startup eliminated | Weeks-months | Deferred |

Item 4 shipped as part of item 2 (`ContainerdRuntime` implements
`ActivityProbes` via the metrics API; the docker runtimes keep
`DockerProbes`). Item 1 shipped as checkpoint/restore on the new gvisor
runtime — it hides the Node.js startup cost entirely for fresh workspaces,
which makes item 5 relevant only if pool exhaustion under burst load ever
matters.
