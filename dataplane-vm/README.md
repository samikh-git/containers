# Production data plane on the Mac Mini (DESIGN §7)

Gets you from "Docker Desktop + runc" (the dev path in the main README) to
the real target: Docker + gVisor (`runsc`) + ZFS, inside one Linux VM.

**Status: verified end-to-end** on Apple Silicon via Lima — real gVisor
sandboxes, a real ZFS pool, a real 20-branch fan-out with clone economics
confirmed on disk. Findings from that run are folded into the steps below
and into the code itself (`dataplane/zfs.go`).

## Quick path (interactive installer)

From the Mac host (repo under `~/…` so Lima can mount it):

```sh
./dataplane-vm/install.sh
```

Prompts for runtime (`gvisor` recommended), disk size, API key, warm pool,
UI port-forward, optional Cloudflare Tunnel (DESIGN §6 tunnel mode), and an
optional smoke workspace. Always starts the policy gateway and `router serve`,
and fails the install if the API never comes up. Re-runnable — skips work that
is already done. Inside-VM package setup remains in `provision.sh`; the
installer orchestrates the host-side half (Lima disk/VM, cross-compile,
images, gateway wiring).

Non-interactive example:

```sh
RUNTIME=gvisor FILL_POOL=yes EXPOSE_UI=yes \
  ANTHROPIC_API_KEY=sk-ant-… ./dataplane-vm/install.sh
```

With a Cloudflare Tunnel (token from **Networking → Tunnels → Create**;
dashboard Published application Service URL must be `http://127.0.0.1:8400`):

```sh
DEPLOY_TUNNEL=yes TUNNEL_HOSTNAME=app.example.com \
  CLOUDFLARE_TUNNEL_TOKEN=eyJ… \
  RUNTIME=gvisor EXPOSE_UI=yes ANTHROPIC_API_KEY=sk-ant-… \
  ./dataplane-vm/install.sh
```

Manual steps below are the same flow, expanded.

## 1. Create the ZFS disk, then boot the VM

```sh
brew install lima
limactl disk create zfs-pool --size 200GiB   # must exist BEFORE start —
                                              # additionalDisks references a
                                              # pre-created Lima disk, it does
                                              # not create one inline
limactl start --name=dataplane dataplane-vm/lima.yaml
```

## 2. Provision it (Docker, gVisor, ZFS pool)

```sh
limactl shell dataplane sudo bash ~/Developer/containerization/dataplane-vm/provision.sh
```

This installs Docker CE, downloads `runsc` and registers it as a Docker
runtime, then creates a ZFS pool named `tank` on the VM's dedicated
`additionalDisk`.

**One thing it has to work around:** Lima auto-formats and mounts
`additionalDisks` as ext4 by default (observed: `/dev/vdb1` mounted at
`/mnt/lima-zfs-pool`). ZFS wants the raw whole-disk device, so the script
unmounts and wipes that filesystem before creating the pool — this is
already handled, not something you need to do manually.

**Confirmed on real hardware:** with no `/dev/kvm` available inside the
nested VM, gVisor selects its `systrap` platform (the modern default, not
the old, much slower `ptrace` fallback) — verified via the registered
runtime's reported `dev.gvisor.flag.platform` and, more concretely, by the
sandbox's own `uname -a` reporting `4.19.0-gvisor`, gVisor's synthetic
kernel identity, not the VM's real `6.8.0` kernel. ZFS built and loaded on
arm64 with no DKMS issues.

## 3. Get the sandbox image and binaries into the VM

The VM's Docker daemon is native `linux/arm64`, so builds run directly —
no cross-arch emulation needed on Apple Silicon:

```sh
limactl shell dataplane -- sudo docker build \
  -t opencode-sandbox:v1 ~/Developer/containerization/sandbox-image
limactl shell dataplane -- sudo docker build \
  -f ~/Developer/containerization/Dockerfile.gateway \
  -t policy-gateway:v1 ~/Developer/containerization
```

Router and gateway binaries: cross-compile from macOS (fast, no VM Go
toolchain needed) — they land in the shared `~` mount automatically:

```sh
GOOS=linux GOARCH=arm64 go build -o dataplane-vm/bin/router ./cmd/router
GOOS=linux GOARCH=arm64 go build -o dataplane-vm/bin/gateway ./cmd/gateway
```

## 4. Wire up the gateway

**A container attached only to an `--internal` network cannot publish
ports** — Docker silently drops the mapping (`docker ps` shows `8444/tcp`
with no host-side binding). The fix, matching what `docker-compose.yml`
already does on macOS: attach the gateway to a normal bridge network first
for the port publish, then also attach `wsnet` so sandboxes can reach it.

```sh
limactl shell dataplane -- sudo docker network create --internal wsnet
limactl shell dataplane -- sudo docker network create bridge-gw

limactl shell dataplane -- sudo docker run -d --name gateway \
  --network bridge-gw -p 127.0.0.1:8444:8444 \
  -v /path/to/gateway.json:/etc/gateway/gateway.json:ro \
  -e ANTHROPIC_API_KEY -e GATEWAY_ADMIN_TOKEN \
  policy-gateway:v1
limactl shell dataplane -- sudo docker network connect wsnet gateway
```

## 5. Run it — same flags, different backend

```sh
limactl shell dataplane -- sudo GATEWAY_ADMIN_TOKEN=... ~/Developer/containerization/dataplane-vm/bin/router up \
  --id ws1 --storage zfs --pool tank --runtime runsc \
  --network wsnet --gateway http://gateway:8443 --route tier-a \
  --profile ~/Developer/containerization/examples/profile \
  --image opencode-sandbox:v1
```

This is the exact same `Router`/`DockerRuntime`/`ZFSStorage` code already
tested on macOS with the `dir` backend — only the flags change. What's real
here versus the Docker Desktop dev path, all confirmed by actually running
it:

- **Real gVisor isolation.** `docker inspect ws1 | grep Runtime` shows
  `runsc`; the sandbox's own kernel identity is gVisor's Sentry, not the
  host.
- **Real ZFS clone economics — the actual product claim, measured.** A
  200MB seed file cloned into 20 branches: each branch `REFER`s the full
  200MB (logically complete, independently writable) but `USE`s only
  ~128KB of real disk — roughly 2.5MB of physical storage for 20 branches
  of a 200MB workspace, not 4GB. Verified with `zfs list -o name,used,refer`.
  Write isolation confirmed: a file written in one branch is genuinely
  absent from every other branch and the base.
- **No Docker Desktop bind-mount quirk** — confirmed absent. A native Linux
  daemon talks to the ZFS-mounted filesystem directly, so
  `dockerRunWithMountFallback` (`dataplane/docker.go`) never triggers here.

### A bug this run found and fixed (real infrastructure, not dev-path noise)

Fresh ZFS datasets mount `root:root`. The sandbox runs as uid 1000 (the
`agent` user baked into `sandbox-image/Dockerfile`). On a real Linux bind
mount with strict POSIX permissions, that's a hard failure — the sandbox's
own entrypoint couldn't `mkdir` its `$HOME` and exited immediately. This
never surfaced in `dir`-backend testing on Docker Desktop, where the
host↔VM permission translation is more permissive than a real Linux bind
mount. Fixed in `dataplane/zfs.go`: `EnsureWorkspace` now chowns a freshly
created dataset to uid/gid 1000 before returning; clones inherit correct
ownership automatically from their snapshot.

A second, smaller fix in the same file: `zfs clone` has no `-p` — unlike
`zfs create`, it will not auto-create a missing parent dataset. The first
branch of any workspace failed with "parent does not exist" until
`CloneBranch` was made to `zfs create -p` the `branches/` namespace dataset
before cloning into it.

## Direct containerd runtime (no dockerd hop)

`--runtime containerd` (gVisor) / `--runtime containerd-runc` (plain runc)
talks to containerd directly over its socket — the containerd that Docker CE
already installs — in its own `dataplane` namespace. This drops the
`docker CLI → dockerd → containerd` chain per operation, and the idle
monitor reads cgroup metrics over the same client instead of spawning
`docker stats` (PERFORMANCE.md items 2 and 4). Same security posture as the
Docker path: read-only rootfs, all caps dropped, no-new-privileges, tmpfs
`/tmp`, pids limit, no network unless named.

**How gVisor is driven**: NOT via the dedicated `containerd-shim-runsc-v1` —
that shim hangs task creation against containerd 2.x (verified here:
`runsc create` succeeds and the sandbox boots, but the shim's ttrpc
connection drops and Create never returns; same incompatibility family as
containerd#11708). Instead the runtime uses the standard runc v2 shim with
`BinaryName=/usr/local/bin/runsc` — the exact mechanism dockerd uses for
daemon.json `"runtimes"`, and it works with the stock gVisor `runsc` binary.

Setup differences from the Docker path (provision.sh handles the first):

1. **CNI instead of Docker networks**: a named `--network foo` is
   `/etc/cni/net.d/foo.conflist`. provision.sh writes an isolated bridge
   conf for `wsnet` (subnet `10.88.0.0/24`, no default route, no masquerade
   — no egress, like `docker network create --internal`).
2. **Image import**: dockerd's image store is separate from containerd's.
   After building the sandbox image, import it once:

   ```sh
   limactl shell dataplane -- sudo bash -c \
     'docker save opencode-sandbox:v1 | ctr -n dataplane images import -'
   ```

3. **Gateway placement**: sandboxes on the CNI bridge reach the *host* at
   `10.88.0.1`, so on this path run the gateway binary on the VM host (or
   publish the gateway container's port on that address) and pass
   `--gateway http://10.88.0.1:8443`.

```sh
limactl shell dataplane -- sudo GATEWAY_ADMIN_TOKEN=... ~/Developer/containerization/dataplane-vm/bin/router up \
  --id ws1 --storage zfs --pool tank --runtime containerd \
  --network wsnet --gateway http://10.88.0.1:8443 --route tier-a \
  --image opencode-sandbox:v1
```

The router resolves the agent endpoint from the sandbox's CNI-assigned IP
(recorded as a container label), and `router watch` probes `/busy` over HTTP
from the host — no `docker exec`. A sandbox with no network cannot be
probed and counts as busy (fails toward staying up), so pair `watch` with
networked sandboxes on this runtime.

**Status: verified end-to-end in this VM** (2026-07-18): `router up` with
`--runtime containerd --network wsnet` + gateway session in **~220ms**
(vs ~1-3s on the Docker path), sandbox kernel `4.19.0-gvisor`, OpenCode
reachable on its CNI IP from the host, replace-running-holder in ~450ms
(SIGTERM drain), `down`/`destroy` leave no containers, tasks, CNI leases,
or datasets behind. Also confirmed along the way: the ZFS pool survives a
`limactl stop`/`start` cycle (the open question above).

## Warm pool (gvisor runtime): checkpoint/restore fast starts

`--runtime gvisor` drives runsc directly against OCI bundles (no dockerd,
no containerd task — containerd is only the image store). It is the only
runtime with checkpoint/restore, which powers the pre-warmed pool
(PERFORMANCE.md item 1):

```sh
# build 3 slots off the critical path (each: boot → agent serves →
# runsc checkpoint → zfs snapshot @golden; ~4s per slot)
limactl shell dataplane -- sudo GATEWAY_ADMIN_TOKEN=... ~/Developer/containerization/dataplane-vm/bin/router pool fill --pool-n 3 \
  --storage zfs --pool tank --runtime gvisor \
  --network wsnet --gateway http://10.88.0.1:8443 --route tier-a \
  --profile ~/Developer/containerization/examples/profile \
  --image opencode-sandbox:v1

# a fresh workspace now restores from a slot instead of cold-booting
limactl shell dataplane -- sudo GATEWAY_ADMIN_TOKEN=... ~/Developer/containerization/dataplane-vm/bin/router up --id ws1 \
  --storage zfs --pool tank --runtime gvisor \
  --network wsnet --gateway http://10.88.0.1:8443 --route tier-a \
  --profile ~/Developer/containerization/examples/profile \
  --image opencode-sandbox:v1

# serve keeps the pool topped up after each claim (target = --pool-n, default 2;
# --pool-n 0 disables). Background fill starts immediately on serve.
… router serve --runtime gvisor --pool-n 2 …

# inventory / teardown
… router pool status …
… router pool drain  …
```

**Measured here (2026-07-19)**: warm `up` ≈ 600-760ms to an agent that
answers HTTP immediately (the checkpoint is taken after the agent serves
200, not merely listens); cold boot on the same runtime ≈ 2.6-2.8s to the
same bar. Warm starts apply only to workspaces that don't exist yet — a
resume's `/workspace` content can't match the golden checkpoint, which is a
hard gVisor restore requirement. Each slot carries its own unique session
token frozen into the checkpoint (env can't change at restore); the token
is inert until `up` registers it at the gateway under the real workspace
id. Consumed slots leave their golden dataset behind as the ZFS clone
origin; `pool drain` garbage-collects origins whose clones are gone.

## Fan-out timing, honestly

20 branches came up in ~18 seconds on this run — but that's *sandbox boot
time* (20 real `docker run` + gVisor Sentry starts), not clone time. The
ZFS clone step itself is what's near-instant (the `zfs list` output above
is the evidence); DESIGN §6's resume-latency SLO (p50 ≤3s, p95 ≤10s per
sandbox) governs the boot side and is a separate thing to benchmark under
load, not yet measured here.

## Not yet done

- `docker-compose.yml` and `scripts/demo.sh` still assume the macOS dev
  path (`dir` storage, `runc`, Docker Desktop directly). The host-side
  installer (`install.sh`) covers the production VM path; adapting
  `scripts/demo.sh` to call into it (or share helpers) is still open.
- Snapshot/replication scheduling (§7's 15-minute RPO) isn't wired up —
  `router watch` covers idle detection, not backup cadence.
- VM restart persistence for the ZFS pool is verified for
  `limactl stop`/`start` (see containerd section above); a full Mac reboot
  cycle is still worth a once-over on each new Lima version.
