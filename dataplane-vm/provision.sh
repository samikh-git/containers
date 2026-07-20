#!/usr/bin/env bash
# Provisions the Lima VM (dataplane-vm/lima.yaml) as the production data
# plane: Docker + gVisor (runsc) runtime + a ZFS pool on the dedicated
# additionalDisk (DESIGN §7). Run once, as root, inside the VM:
#
#   limactl shell dataplane sudo bash ~/Developer/containerization/dataplane-vm/provision.sh
#
# Idempotent-ish: safe to re-run, but zpool creation is skipped if "tank"
# already exists.
set -euo pipefail

echo "== packages =="
apt-get update
apt-get install -y ca-certificates curl gnupg zfsutils-linux

echo "== docker =="
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi

echo "== gVisor (runsc) =="
# NOTE: the dedicated containerd shim (containerd-shim-runsc-v1) is
# deliberately NOT installed. It hangs task creation against containerd 2.x
# (same incompatibility family as containerd#11708); the router's containerd
# runtime instead drives runsc through the standard runc v2 shim with a
# BinaryName override — the same mechanism dockerd uses for daemon.json
# "runtimes", verified end-to-end in this VM.
ARCH=$(uname -m)   # aarch64 on Apple Silicon
URL="https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}"
curl -fsSL "${URL}/runsc" -o /usr/local/bin/runsc
curl -fsSL "${URL}/runsc.sha512" -o /tmp/runsc.sha512
( cd /usr/local/bin && sha512sum -c /tmp/runsc.sha512 )
chmod +x /usr/local/bin/runsc
runsc --version

echo "== docker daemon: register runsc runtime =="
mkdir -p /etc/docker
cat > /etc/docker/daemon.json <<'EOF'
{
  "runtimes": { "runsc": { "path": "/usr/local/bin/runsc" } }
}
EOF
# NOTE: Docker's own --userns-remap is deliberately NOT enabled here. gVisor
# already remaps privilege inside the sandbox on its own terms (DESIGN §8);
# stacking Docker's userns-remap on top has known rough edges with some
# runsc platforms. Treat it as a hardening step to test in isolation, not a
# default — flagging honestly rather than asserting it "just works".
systemctl restart docker

echo "== CNI (for --runtime containerd) =="
# The direct-containerd runtime (PERFORMANCE.md item 2) does its own
# networking via CNI: a named --network is /etc/cni/net.d/<name>.conflist.
# "wsnet" below is an ISOLATED bridge — no default route, no masquerade, so
# sandboxes get no egress (DESIGN §8); they reach only each other and the
# host side of the bridge (10.88.0.1), which is where the gateway listens on
# this path. The Docker runtimes keep using Docker networks; this file is
# only read by --runtime containerd|containerd-runc.
apt-get install -y containernetworking-plugins   # -> /usr/lib/cni
mkdir -p /etc/cni/net.d
cat > /etc/cni/net.d/wsnet.conflist <<'EOF'
{
  "cniVersion": "1.0.0",
  "name": "wsnet",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni-wsnet",
      "isGateway": true,
      "ipMasq": false,
      "ipam": {
        "type": "host-local",
        "subnet": "10.88.0.0/24"
      }
    }
  ]
}
EOF

echo "== ZFS pool =="
POOL_DEV="${POOL_DEV:-}"
if [ -z "$POOL_DEV" ]; then
  for d in /dev/vdb /dev/sdb /dev/nvme1n1; do
    [ -b "$d" ] && POOL_DEV="$d" && break
  done
fi
if [ -z "$POOL_DEV" ]; then
  echo "Could not auto-detect the additionalDisk block device." >&2
  echo "Run 'lsblk' and re-run with: POOL_DEV=/dev/xxx bash provision.sh" >&2
  exit 1
fi

# Lima auto-formats+mounts additionalDisks as ext4 by default (observed:
# /dev/vdb1 mounted at /mnt/lima-zfs-pool). ZFS wants the RAW whole-disk
# device, unmounted and unformatted — unwind Lima's default before creating
# the pool. wipefs clears the ext4 signature so zpool create doesn't need -f
# to fight past it (still passed below as defense in depth, not the primary
# mechanism — this is a fresh Lima disk every time, never a real data disk).
MOUNTPOINT=$(findmnt -n -o TARGET "${POOL_DEV}1" 2>/dev/null || true)
if [ -n "$MOUNTPOINT" ]; then
  echo "unmounting Lima's default ext4 on ${POOL_DEV}1 ($MOUNTPOINT)"
  umount "${POOL_DEV}1"
fi
wipefs -a "$POOL_DEV" || true

if ! zpool list tank >/dev/null 2>&1; then
  zpool create -f -o ashift=12 \
    -O compression=lz4 -O dedup=off -O atime=off \
    tank "$POOL_DEV"
else
  echo "pool 'tank' already exists, skipping create"
fi
zfs create -p tank/workspaces 2>/dev/null || true

echo "== ARC cap (2GiB — leave headroom for sandboxes) =="
mkdir -p /etc/modprobe.d
echo "options zfs zfs_arc_max=2147483648" > /etc/modprobe.d/zfs.conf
echo 2147483648 > /sys/module/zfs/parameters/zfs_arc_max 2>/dev/null || true

echo
echo "== verification =="
zpool status tank
docker info --format '{{json .Runtimes}}'
echo "-- runsc smoke test (expect a gVisor-flavored uname, not the host kernel) --"
docker run --rm --runtime=runsc alpine uname -a

echo
echo "done. Next: build images and run the router with --storage zfs --pool tank --runtime runsc"
echo
echo "For the direct-containerd runtime (--runtime containerd), also import the"
echo "sandbox image into containerd's 'dataplane' namespace (dockerd's image"
echo "store is separate) after building it:"
echo "  docker save opencode-sandbox:v1 | ctr -n dataplane images import -"
