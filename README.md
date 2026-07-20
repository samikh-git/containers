# Sovereign AI Developer Workspace Platform

Governed execution layer for AI coding agents: sandboxed, disposable compute
over snapshotted storage, on hardware you own. See [DESIGN.md](DESIGN.md)
(architecture), [COMPETITIVE.md](COMPETITIVE.md) (market),
[GTM.md](GTM.md) (pitch & pricing).

## Layout

```
dataplane/        Router core (Go): storage, lease fencing, sandbox runtime
gateway/          Policy gateway (Go): key custody, routing, policy, ledger (§9/§11)
cmd/router/       Local-mode CLI (DESIGN §3/§4: no control plane)
cmd/gateway/      Policy gateway daemon + ledger verifier
restapi/          REST API + embedded web UIs over the router (DESIGN §4)
frontend/         End-user app (React + @opencode-ai/sdk): chat, models, diffs, permissions
sandbox-image/    The opencode-sandbox container image (DESIGN §7/§8)
```

## Web interface (REST API)

```sh
(cd frontend && npm install && npm run build)   # once, and after frontend changes
go build -o router ./cmd/router
./router serve                                  # http://127.0.0.1:8400
ROUTER_API_TOKEN=… ./router serve --listen 0.0.0.0:8400   # off-host access
```

The frontend build lands in `restapi/dist/` and is embedded into the router
binary, so deployment stays a single binary. `frontend/` is the end-user app
(served at `/`): create a workspace, chat with its agent, pick a model from
the profile's allowed providers, approve or deny permission requests, and
review the agent's changes as diffs. Built on React + the official
`@opencode-ai/sdk` (typed client + SSE event stream) through the router's
proxy. For UI development, `npm run dev` in `frontend/` proxies `/api` to a
running `router serve`.

The operator console (workspaces, fanout, provider keys, cost/usage,
hibernate) lives at `/admin` (React + [Kumo](https://kumo-ui.com); rebuild
with `npm run build` in `frontend/`). The API is plain JSON for scripting:

```sh
curl -X POST localhost:8400/api/workspaces -d '{"id":"ws1"}'
curl -X POST localhost:8400/api/workspaces/ws1/fanout -d '{"n":5}'
curl        localhost:8400/api/workspaces
curl        localhost:8400/api/usage?window=24h   # requires --ledger
curl -X POST localhost:8400/api/workspaces/ws1/down
curl -X DELETE localhost:8400/api/workspaces/ws1
```

Spec fields omitted from a request (image, cpus, memory_mb, quota_gb,
profile_dir, gateway_url, route, network) default to the `serve` command's
flags. Non-loopback listeners require `ROUTER_API_TOKEN` (sent as
`Authorization: Bearer …`); DESIGN §4's signed session tokens replace this
in team mode.

### Talking to the agent & managing provider keys

With a gateway configured, `serve` also exposes:

```sh
./router serve --gateway http://host.docker.internal:8443/v1 \
  --gateway-admin http://127.0.0.1:8444 --route tier-a-anthropic \
  --network bridge --profile $PWD/examples/profile
```

- **Provider keys** — the UI's key panel (or `PUT /api/keys/{route}`
  `{"key":"sk-ant-…"}`) sets/rotates a route's provider key via the gateway
  admin API at runtime; runtime keys override `key_env`, so the gateway can
  start with no key in its environment. Keys still live only in the gateway
  process (DESIGN §9); the router forwards and never stores them.
  `GET /api/keys` reports which routes hold a key (never values);
  `GET /api/routes` adds each route's kind, upstream, model allowlist and
  key source (env vs runtime) for the admin page's provider panel. The
  shipped `gateway.json` defines routes for Anthropic, OpenAI, Google
  Gemini, xAI, Mistral, DeepSeek, Groq, OpenRouter and a local
  OpenAI-compatible endpoint (Ollama); add or edit routes there to cover
  other providers.
- **Agent chat** — the end-user app (and the admin page's *Agent* button)
  talk to the sandbox's OpenCode server through the router
  (`/api/workspaces/{id}/opencode/…` reverse-proxies to port 4321 inside the
  sandbox, SSE included). On Docker Desktop the agent port is published on
  loopback only; on a native Linux daemon the router dials the container IP,
  which also covers `--internal` networks. A sandbox started without a
  network stays unreachable by design.
- **Permissions** — the example profile sets `"edit"/"bash": "ask"`, so the
  agent pauses on gated actions and the UI shows an approval card (allow
  once / always / deny) driven by OpenCode's `permission.asked` events.
- **Diff review** — the sandbox entrypoint initializes `/workspace` as a git
  repo with a baseline commit; the UI's *Changes* tab renders
  `GET …/opencode/vcs/diff?mode=git`, i.e. everything the agent changed
  since the workspace was created (rebuild the sandbox image to pick this
  up: `docker build -t opencode-sandbox:v1 sandbox-image/`).

## Policy gateway (DESIGN §9)

```sh
go build -o gw ./cmd/gateway
cp gateway/gateway.example.json gateway.json   # edit routes/sessions
ANTHROPIC_API_KEY=sk-ant-… ./gw --config gateway.json --ledger audit.jsonl
./gw --verify audit.jsonl                      # tamper-check the hash chain
```

Provider keys live in the gateway process only. Sandboxes hold per-workspace
session tokens (sent as `x-api-key` or `Bearer`); the gateway swaps them for
the real key, enforces model allowlists, size caps and secret tripwires, and
appends every request — allowed or blocked — to the hash-chained ledger.

Sessions are dynamic: the router mints a token per workspace at `up`/`fanout`,
registers it via the gateway's admin API (`--admin`, default 127.0.0.1:8444;
`GATEWAY_ADMIN_TOKEN` required if not loopback-bound), and revokes it on
`down`/`destroy`/hibernate.

## Wiring sandboxes to the gateway (no other egress)

```sh
docker network create --internal wsnet          # no route to the outside world
docker build -f Dockerfile.gateway -t policy-gateway:v1 .
docker run -d --name gateway --network wsnet -p 127.0.0.1:8444:8444 \
  -v $PWD/gateway.json:/etc/gateway/gateway.json:ro \
  -e ANTHROPIC_API_KEY -e GATEWAY_ADMIN_TOKEN policy-gateway:v1

GATEWAY_ADMIN_TOKEN=… ./router up --id ws1 --network wsnet \
  --gateway http://gateway:8443/v1 --route tier-a-anthropic
```

Sandboxes on the internal network can reach exactly one thing: the gateway.

## Idle detection & scale-to-zero (DESIGN §7)

```sh
./router watch --idle 10m --interval 30s --ledger audit.jsonl
```

A workspace hibernates (graceful stop → snapshot → session revocation) only
when ALL THREE hold for the whole idle window: no model traffic in the
ledger, sandbox CPU below the floor, and the agent's `/busy` endpoint says
false. Probe errors count as active — misjudging kills work; idling doesn't.

## Quickstart (dev, any machine — no ZFS/Docker needed)

```sh
go build -o router ./cmd/router

./router up     --id demo --storage dir --runtime none
./router fanout --id demo --n 5 --storage dir --runtime none
./router status
./router destroy --id demo --storage dir --runtime none
```

The `dir` backend uses APFS clonefile / Linux reflinks, so snapshots and
branch clones are copy-on-write and near-instant even in dev.

## Production shape (inside the Linux data plane image)

```sh
# ZFS pool "tank", gVisor runtime, sandbox image from sandbox-image/
./router up     --id ws1 --storage zfs --pool tank --runtime gvisor \
                --network wsnet --profile /etc/profiles/org-default \
                --gateway http://gateway:8443 --route tier-a-anthropic
./router fanout --id ws1 --n 20
```

`--runtime gvisor` drives runsc directly (checkpoint/restore capable). For
containerd-managed sandboxes without the warm pool, use `runsc` /
`containerd`. Defaults auto-detect: ZFS + runsc when present, dir + runc
otherwise.

### Warm pool (fast fresh starts)

Pre-boot and checkpoint sandboxes off the critical path, then restore onto
a new workspace (~4× faster end-to-end than cold boot; see
[PERFORMANCE.md](PERFORMANCE.md)):

```sh
./router pool fill --pool-n 3 --storage zfs --pool tank --runtime gvisor \
  --network wsnet --gateway http://gateway:8443 --route tier-a-anthropic \
  --profile /etc/profiles/org-default --image opencode-sandbox:v1

./router serve --storage zfs --pool tank --runtime gvisor --pool-n 2 …  # auto-refills after each claim
./router pool status
./router pool drain
```

Warm restore applies only to workspaces that do not exist yet (resume must
cold-boot). `serve --pool-n N` (default 2; `0` disables) keeps inventory
topped up in the background; CLI `up` does not auto-refill.

Setting this up on real hardware (e.g. a Mac Mini, via a Lima Linux VM):
run [`./dataplane-vm/install.sh`](dataplane-vm/install.sh) (interactive;
creates the Lima disk/VM, provisions Docker+gVisor+ZFS, cross-compiles
binaries, builds images, wires the gateway). Details and the manual
walkthrough live in [dataplane-vm/](dataplane-vm/) — **verified
end-to-end** on Apple Silicon: real gVisor sandboxes, a real ZFS pool,
warm-pool restore timings, and a real 20-branch fan-out with clone
economics confirmed on disk (~128KB actual disk per clone of a 200MB
workspace). Two real bugs it found are already fixed in `dataplane/zfs.go`.

## Invariants the code enforces (and tests lock in)

- **Storage → lease → compute, in that order.** A sandbox never starts on a
  volume that isn't fenced (`dataplane/router.go`, asserted in tests).
- **One writer per volume identity.** Generation-numbered leases, checked on
  disk at mount; clone-inherited parent tokens are foreign and replaced
  (`dataplane/lease.go`).
- **Idempotent convergence.** `up`/`EnsureWorkspace` at the same generation
  is a no-op; actual state is rebuilt from container labels, never from
  router memory.
- **Sandbox posture** (DESIGN §8): read-only rootfs, all caps dropped,
  no-new-privileges, tmpfs /tmp, `--network none` until the gateway lands,
  workspace + read-only profile as the only mounts.

## Not yet implemented (next milestones)

1. Flight-recorder events beyond model traffic (tool calls, egress; §11)
2. Control-plane reconciler protocol (gRPC watch stream, §5)
3. Capability-profile bundle resolution (§10)
4. Egress allowlist proxy for non-model traffic (git remotes, registries; §8)
5. Snapshot/replication scheduling for the 15-minute RPO (§7)
