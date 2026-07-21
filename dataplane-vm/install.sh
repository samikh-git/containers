#!/usr/bin/env bash
# Interactive installer for the Mac Mini production data plane
# (Lima Linux VM + Docker + gVisor + ZFS). Run on the Mac host:
#
#   ./dataplane-vm/install.sh
#
# Always leaves the policy gateway and router serve running; fails if the
# API never answers. Re-runnable: skips work that is already done, prompts
# before anything destructive or optional. Inside-VM package setup stays
# in provision.sh.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VM_NAME="${VM_NAME:-dataplane}"
DISK_NAME="${DISK_NAME:-zfs-pool}"
BIN_DIR="$REPO/dataplane-vm/bin"
RUN_DIR="$REPO/dataplane-vm/run"
ENV_FILE="$REPO/.env"
GATEWAY_CFG="$REPO/gateway.local.json"
IMAGE_SANDBOX="opencode-sandbox:v1"
IMAGE_GATEWAY="policy-gateway:v1"
PROFILE="$REPO/examples/profile"

# Defaults (overridable via prompts / env)
DISK_SIZE="${DISK_SIZE:-200GiB}"
RUNTIME="${RUNTIME:-}"          # gvisor | containerd | runsc
START_GATEWAY=yes   # always — install leaves the stack running
START_SERVE=yes
FILL_POOL="${FILL_POOL:-}"
POOL_N="${POOL_N:-2}"
SMOKE_UP="${SMOKE_UP:-}"
EXPOSE_UI="${EXPOSE_UI:-}"
DEPLOY_TUNNEL="${DEPLOY_TUNNEL:-}"   # Cloudflare Tunnel (DESIGN §6 tunnel mode)
CLOUDFLARE_TUNNEL_TOKEN="${CLOUDFLARE_TUNNEL_TOKEN:-}"
TUNNEL_HOSTNAME="${TUNNEL_HOSTNAME:-}"  # public hostname (dashboard route); docs only
BUILD_FRONTEND="${BUILD_FRONTEND:-}"
UI_PORT="${UI_PORT:-8400}"
ROUTE="tier-a"

# ---------------------------------------------------------------------------
# UI helpers
# ---------------------------------------------------------------------------

is_tty() { [[ -t 0 && -t 1 ]]; }

say()  { printf '\n== %s ==\n' "$*"; }
info() { printf '  %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ask "Prompt" "default"  -> sets REPLY
ask() {
  local prompt="$1" default="${2:-}" display
  if [[ -n "$default" ]]; then
    display="$prompt [$default]: "
  else
    display="$prompt: "
  fi
  if ! is_tty; then
    [[ -n "$default" ]] || die "non-interactive and no default for: $prompt"
    REPLY="$default"
    return
  fi
  read -r -p "$display" REPLY
  if [[ -z "$REPLY" ]]; then
    REPLY="$default"
  fi
}

# confirm "Prompt" "Y"|"N"  -> returns 0 for yes
confirm() {
  local prompt="$1" default="${2:-Y}" hint yn
  case "$default" in
    Y|y) hint="Y/n" ;;
    N|n) hint="y/N" ;;
    *)   hint="y/n" ;;
  esac
  if ! is_tty; then
    [[ "$default" == [Yy] ]]
    return
  fi
  read -r -p "$prompt [$hint]: " yn
  if [[ -z "$yn" ]]; then
    yn="$default"
  fi
  [[ "$yn" == [Yy]* ]]
}

# choose "Prompt" "default" option1 option2 ...  -> sets REPLY to chosen option
choose() {
  local prompt="$1" default="$2"
  shift 2
  local opts=("$@") i
  if ! is_tty; then
    REPLY="$default"
    return
  fi
  printf '%s\n' "$prompt"
  for i in "${!opts[@]}"; do
    local mark=" "
    [[ "${opts[$i]}" == "$default" ]] && mark="*"
    printf '  %d)%s %s\n' "$((i + 1))" "$mark" "${opts[$i]}"
  done
  while true; do
    read -r -p "Choice [${#opts[@]} options, default=$default]: " REPLY
    if [[ -z "$REPLY" ]]; then
      REPLY="$default"
      return
    fi
    if [[ "$REPLY" =~ ^[0-9]+$ ]] && (( REPLY >= 1 && REPLY <= ${#opts[@]} )); then
      REPLY="${opts[$((REPLY - 1))]}"
      return
    fi
    for o in "${opts[@]}"; do
      if [[ "$REPLY" == "$o" ]]; then
        return
      fi
    done
    echo "  pick a number or one of: ${opts[*]}"
  done
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"
}

# ---------------------------------------------------------------------------
# Host / path checks
# ---------------------------------------------------------------------------

check_host() {
  say "host checks"
  [[ "$(uname -s)" == Darwin ]] || die "run this on the Mac Mini host (macOS), not inside the VM"
  case "$(uname -m)" in
    arm64|aarch64) ;;
    *)
      warn "this path is verified on Apple Silicon (arm64); continuing anyway"
      ;;
  esac
  case "$REPO" in
    "$HOME"/*) info "repo under ~ (Lima shared mount): $REPO" ;;
    *)
      die "repo must live under your home directory so Lima can mount it (currently: $REPO)"
      ;;
  esac
  need_cmd go
  if ! command -v brew >/dev/null 2>&1; then
    die "Homebrew is required (https://brew.sh) — needed to install Lima"
  fi
  if ! command -v limactl >/dev/null 2>&1; then
    if confirm "Lima not found. Install with Homebrew?" "Y"; then
      brew install lima
    else
      die "Lima is required"
    fi
  fi
  info "limactl $(limactl --version 2>/dev/null | head -1)"
  info "go $(go env GOVERSION 2>/dev/null || go version)"
}

# ---------------------------------------------------------------------------
# Interactive plan
# ---------------------------------------------------------------------------

gather_plan() {
  say "install plan"

  if [[ -z "$RUNTIME" ]]; then
    choose "Sandbox runtime:" "gvisor" \
      "gvisor" "containerd" "runsc"
    RUNTIME="$REPLY"
  fi
  case "$RUNTIME" in
    gvisor)
      info "gVisor direct (warm pool / checkpoint-restore). Gateway on VM host @ 10.88.0.1:8443"
      ;;
    containerd)
      info "containerd + runsc BinaryName. Gateway on VM host @ 10.88.0.1:8443"
      ;;
    runsc)
      info "Docker + runsc runtime. Gateway container on wsnet (http://gateway:8443)"
      ;;
    *) die "unknown runtime: $RUNTIME (want gvisor|containerd|runsc)" ;;
  esac

  if limactl disk list 2>/dev/null | awk '{print $1}' | grep -qx "$DISK_NAME"; then
    info "Lima disk '$DISK_NAME' already exists"
  else
    ask "ZFS disk size (created once, before first VM start)" "$DISK_SIZE"
    DISK_SIZE="$REPLY"
  fi

  ensure_secrets_plan

  if [[ -z "$BUILD_FRONTEND" ]]; then
    if [[ ! -f "$REPO/restapi/dist/index.html" ]]; then
      BUILD_FRONTEND=yes
      info "frontend not built yet — will run npm run build (embeds into router)"
    elif confirm "Rebuild frontend before compiling router?" "N"; then
      BUILD_FRONTEND=yes
    else
      BUILD_FRONTEND=no
    fi
  fi

  info "install will start the gateway and router serve (required)"

  if [[ "$RUNTIME" == "gvisor" && -z "$FILL_POOL" ]]; then
    if confirm "Pre-fill warm pool ($POOL_N slots)? ~4s each, first time only" "Y"; then
      FILL_POOL=yes
      ask "Warm pool size" "$POOL_N"
      POOL_N="$REPLY"
    else
      FILL_POOL=no
    fi
  fi

  if [[ -z "$EXPOSE_UI" ]]; then
    if confirm "Forward VM :$UI_PORT → Mac localhost (open UI in a browser)?" "Y"; then
      EXPOSE_UI=yes
    else
      EXPOSE_UI=no
    fi
  fi

  if [[ -z "$DEPLOY_TUNNEL" ]]; then
    warn "Cloudflare Tunnel = DESIGN §6 tunnel mode (TLS terminates at Cloudflare; sovereignty opt-out)"
    if confirm "Deploy Cloudflare Tunnel (expose UI via cloudflared in the VM)?" "N"; then
      DEPLOY_TUNNEL=yes
    else
      DEPLOY_TUNNEL=no
    fi
  fi
  if [[ "$DEPLOY_TUNNEL" == "yes" ]]; then
    if [[ -z "$TUNNEL_HOSTNAME" ]] && is_tty; then
      ask "Public hostname for the UI (optional; configure the same in the dashboard)" ""
      TUNNEL_HOSTNAME="$REPLY"
    fi
    ensure_tunnel_token_plan
  fi

  if [[ -z "$SMOKE_UP" ]]; then
    if confirm "Create a smoke-test workspace (id: smoke)?" "N"; then
      SMOKE_UP=yes
    else
      SMOKE_UP=no
    fi
  fi

  echo
  info "summary:"
  info "  runtime=$RUNTIME  disk=$DISK_NAME ($DISK_SIZE)  vm=$VM_NAME"
  info "  frontend=$BUILD_FRONTEND  warm_pool=$FILL_POOL($POOL_N)"
  info "  serve=yes (always)  expose_ui=$EXPOSE_UI  smoke=$SMOKE_UP"
  info "  cloudflare_tunnel=$DEPLOY_TUNNEL${TUNNEL_HOSTNAME:+ ($TUNNEL_HOSTNAME)}"
  if ! confirm "Proceed?" "Y"; then
    die "aborted"
  fi
}

ensure_secrets_plan() {
  mkdir -p "$RUN_DIR"
  # Load existing .env without clobbering already-exported vars.
  if [[ -f "$ENV_FILE" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
  fi

  if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    if is_tty; then
      ask "Anthropic API key (sk-ant-…; stored in $ENV_FILE)" ""
      ANTHROPIC_API_KEY="$REPLY"
      [[ -n "$ANTHROPIC_API_KEY" ]] || die "ANTHROPIC_API_KEY is required to reach the model"
    else
      die "set ANTHROPIC_API_KEY in the environment or $ENV_FILE"
    fi
  else
    info "ANTHROPIC_API_KEY already set"
  fi

  if [[ -z "${GATEWAY_ADMIN_TOKEN:-}" ]]; then
    GATEWAY_ADMIN_TOKEN="$(openssl rand -hex 16)"
    info "generated GATEWAY_ADMIN_TOKEN"
  else
    info "GATEWAY_ADMIN_TOKEN already set"
  fi

  write_env_file
}

ensure_tunnel_token_plan() {
  # Load existing .env without clobbering already-exported vars.
  if [[ -f "$ENV_FILE" ]]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
  fi
  CLOUDFLARE_TUNNEL_TOKEN="${CLOUDFLARE_TUNNEL_TOKEN:-}"

  if [[ -z "$CLOUDFLARE_TUNNEL_TOKEN" ]]; then
    if is_tty; then
      cat <<EOF
  Create a remotely-managed tunnel in the Cloudflare dashboard, then paste
  its install token here:
    https://dash.cloudflare.com/?to=/:account/tunnels
  After install, add a Published application route with Service URL:
    http://127.0.0.1:${UI_PORT}
EOF
      ask "Cloudflare Tunnel token (stored in $ENV_FILE)" ""
      CLOUDFLARE_TUNNEL_TOKEN="$REPLY"
      [[ -n "$CLOUDFLARE_TUNNEL_TOKEN" ]] || die "CLOUDFLARE_TUNNEL_TOKEN is required when DEPLOY_TUNNEL=yes"
    else
      die "set CLOUDFLARE_TUNNEL_TOKEN in the environment or $ENV_FILE (or DEPLOY_TUNNEL=no)"
    fi
  else
    info "CLOUDFLARE_TUNNEL_TOKEN already set"
  fi
  write_env_file
}

write_env_file() {
  mkdir -p "$RUN_DIR"
  local tmp
  tmp="$(mktemp)"
  if [[ -f "$ENV_FILE" ]]; then
    grep -vE '^(ANTHROPIC_API_KEY|GATEWAY_ADMIN_TOKEN|CLOUDFLARE_TUNNEL_TOKEN|TUNNEL_HOSTNAME)=' \
      "$ENV_FILE" >"$tmp" || true
  fi
  {
    [[ -n "${ANTHROPIC_API_KEY:-}" ]] && printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY"
    [[ -n "${GATEWAY_ADMIN_TOKEN:-}" ]] && printf 'GATEWAY_ADMIN_TOKEN=%s\n' "$GATEWAY_ADMIN_TOKEN"
    [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]] && printf 'CLOUDFLARE_TUNNEL_TOKEN=%s\n' "$CLOUDFLARE_TUNNEL_TOKEN"
    [[ -n "${TUNNEL_HOSTNAME:-}" ]] && printf 'TUNNEL_HOSTNAME=%s\n' "$TUNNEL_HOSTNAME"
  } >>"$tmp"
  mv "$tmp" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  info "wrote $ENV_FILE"
}

# ---------------------------------------------------------------------------
# Lima disk + VM
# ---------------------------------------------------------------------------

ensure_disk() {
  say "Lima disk ($DISK_NAME)"
  if limactl disk list 2>/dev/null | awk '{print $1}' | grep -qx "$DISK_NAME"; then
    info "already exists — skipping create"
    return
  fi
  info "creating $DISK_NAME ($DISK_SIZE) — must exist before first VM start"
  limactl disk create "$DISK_NAME" --size "$DISK_SIZE"
}

vm_status() {
  limactl list --format '{{.Name}}\t{{.Status}}' 2>/dev/null \
    | awk -v n="$VM_NAME" '$1 == n { print $2; found=1 } END { if (!found) print "Absent" }'
}

ensure_vm() {
  say "Lima VM ($VM_NAME)"
  local st
  st="$(vm_status)"
  case "$st" in
    Running)
      info "already running"
      ;;
    Stopped|Stopped/Broken)
      if confirm "VM exists but is stopped. Start it?" "Y"; then
        limactl start "$VM_NAME"
      else
        die "VM must be running"
      fi
      ;;
    Absent)
      info "creating from dataplane-vm/lima.yaml (first boot downloads Ubuntu — can take a few minutes)"
      limactl start --name="$VM_NAME" "$REPO/dataplane-vm/lima.yaml"
      ;;
    *)
      if confirm "VM status is '$st'. Try starting?" "Y"; then
        limactl start "$VM_NAME" || limactl start --name="$VM_NAME" "$REPO/dataplane-vm/lima.yaml"
      else
        die "unexpected VM status: $st"
      fi
      ;;
  esac

  # Wait until shell works.
  local i
  for i in $(seq 1 60); do
    if limactl shell "$VM_NAME" -- true 2>/dev/null; then
      info "VM shell ready"
      return
    fi
    sleep 2
  done
  die "VM did not become reachable via limactl shell"
}

# Run a command inside the VM (as the lima user). Extra args after --.
vm() {
  limactl shell "$VM_NAME" -- "$@"
}

# Run a command inside the VM as root.
vm_root() {
  limactl shell "$VM_NAME" -- sudo "$@"
}

# ---------------------------------------------------------------------------
# Provision (Docker, gVisor, ZFS)
# ---------------------------------------------------------------------------

ensure_provisioned() {
  say "provision (Docker + gVisor + ZFS)"
  local already=no
  if vm_root zpool list tank >/dev/null 2>&1 \
     && vm_root docker info >/dev/null 2>&1 \
     && vm_root test -x /usr/local/bin/runsc; then
    already=yes
    info "looks already provisioned (tank + docker + runsc present)"
    if ! confirm "Re-run provision.sh anyway?" "N"; then
      return
    fi
  fi
  info "running provision.sh inside the VM (apt + docker + runsc + zpool create)"
  vm_root bash "$REPO/dataplane-vm/provision.sh"
}

# ---------------------------------------------------------------------------
# Build binaries + images
# ---------------------------------------------------------------------------

build_frontend() {
  say "frontend (embedded into router)"
  if [[ "$BUILD_FRONTEND" != "yes" ]]; then
    info "skipped"
    return
  fi
  need_cmd npm
  (cd "$REPO/frontend" && npm install && npm run build)
  [[ -f "$REPO/restapi/dist/index.html" ]] || die "frontend build did not produce restapi/dist/"
}

build_binaries() {
  say "cross-compile router + gateway (linux/arm64)"
  mkdir -p "$BIN_DIR"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -C "$REPO" -o "$BIN_DIR/router" ./cmd/router
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -C "$REPO" -o "$BIN_DIR/gateway" ./cmd/gateway
  info "wrote $BIN_DIR/router"
  info "wrote $BIN_DIR/gateway"
}

build_images() {
  say "Docker images (inside VM, native arm64)"
  if vm_root docker image inspect "$IMAGE_SANDBOX" >/dev/null 2>&1; then
    if ! confirm "Image $IMAGE_SANDBOX exists. Rebuild?" "N"; then
      info "keeping existing $IMAGE_SANDBOX"
    else
      vm_root docker build -t "$IMAGE_SANDBOX" "$REPO/sandbox-image"
    fi
  else
    vm_root docker build -t "$IMAGE_SANDBOX" "$REPO/sandbox-image"
  fi

  if [[ "$RUNTIME" == "runsc" ]]; then
    if vm_root docker image inspect "$IMAGE_GATEWAY" >/dev/null 2>&1; then
      if confirm "Image $IMAGE_GATEWAY exists. Rebuild?" "N"; then
        vm_root docker build -f "$REPO/Dockerfile.gateway" -t "$IMAGE_GATEWAY" "$REPO"
      else
        info "keeping existing $IMAGE_GATEWAY"
      fi
    else
      vm_root docker build -f "$REPO/Dockerfile.gateway" -t "$IMAGE_GATEWAY" "$REPO"
    fi
  fi

  if [[ "$RUNTIME" == "containerd" || "$RUNTIME" == "gvisor" ]]; then
    # gVisor direct + containerd runtimes pull the OCI image from containerd's
    # dataplane namespace (dockerd's store is separate).
    info "importing $IMAGE_SANDBOX into containerd namespace 'dataplane'"
    vm_root bash -c \
      "ctr namespaces create dataplane 2>/dev/null || true; docker save '$IMAGE_SANDBOX' | ctr -n dataplane images import -"
  fi
}

# ---------------------------------------------------------------------------
# Gateway config + start
# ---------------------------------------------------------------------------

ensure_gateway_config() {
  if [[ ! -f "$GATEWAY_CFG" ]]; then
    cp "$REPO/gateway.local.json.example" "$GATEWAY_CFG"
    info "created $GATEWAY_CFG from example"
  fi
  # Host-binary path needs an explicit all-interfaces listen so CNI sandboxes
  # can reach the gateway at 10.88.0.1:8443.
  if [[ "$RUNTIME" != "runsc" ]]; then
    local host_cfg="$RUN_DIR/gateway.json"
    if command -v jq >/dev/null 2>&1; then
      jq '.listen = "0.0.0.0:8443"' "$GATEWAY_CFG" >"$host_cfg"
    else
      # Minimal fallback when jq isn't installed on the Mac.
      cat >"$host_cfg" <<'EOF'
{
  "listen": "0.0.0.0:8443",
  "routes": {
    "tier-a": {
      "kind": "anthropic",
      "base_url": "https://api.anthropic.com",
      "key_env": "ANTHROPIC_API_KEY",
      "allowed_models": ["claude-*", "anthropic/*"],
      "max_request_mb": 20
    }
  },
  "sessions": {}
}
EOF
    fi
    info "gateway listen config: $host_cfg"
  fi
}

gateway_url() {
  case "$RUNTIME" in
    runsc) echo "http://gateway:8443" ;;
    *)     echo "http://10.88.0.1:8443" ;;
  esac
}

write_run_helpers() {
  mkdir -p "$RUN_DIR/router-root"
  # Env file the VM processes source (shared mount — keys stay on disk under ~).
  cat >"$RUN_DIR/env" <<EOF
ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
GATEWAY_ADMIN_TOKEN=$GATEWAY_ADMIN_TOKEN
CLOUDFLARE_TUNNEL_TOKEN=${CLOUDFLARE_TUNNEL_TOKEN:-}
TUNNEL_HOSTNAME=${TUNNEL_HOSTNAME:-}
EOF
  chmod 600 "$RUN_DIR/env"

  cat >"$RUN_DIR/start-gateway.sh" <<EOF
#!/usr/bin/env bash
# Started by install.sh / re-run manually inside the VM.
set -euo pipefail
REPO="$REPO"
RUN_DIR="$RUN_DIR"
BIN_DIR="$BIN_DIR"
RUNTIME="$RUNTIME"
# shellcheck disable=SC1091
source "\$RUN_DIR/env"
export ANTHROPIC_API_KEY GATEWAY_ADMIN_TOKEN

if curl -sf -o /dev/null -H "Authorization: Bearer \$GATEWAY_ADMIN_TOKEN" \\
     http://127.0.0.1:8444/routes 2>/dev/null; then
  echo "gateway admin already up on :8444"
  exit 0
fi

if [[ "\$RUNTIME" == "runsc" ]]; then
  docker network inspect wsnet >/dev/null 2>&1 || docker network create --internal wsnet
  docker network inspect bridge-gw >/dev/null 2>&1 || docker network create bridge-gw
  if docker ps -a --format '{{.Names}}' | grep -qx gateway; then
    docker start gateway >/dev/null
  else
    docker run -d --name gateway \\
      --network bridge-gw -p 127.0.0.1:8444:8444 \\
      -v "\$REPO/gateway.local.json:/etc/gateway/gateway.json:ro" \\
      -v gateway-audit:/var/lib/gateway \\
      -e ANTHROPIC_API_KEY -e GATEWAY_ADMIN_TOKEN \\
      $IMAGE_GATEWAY
    docker network connect wsnet gateway
  fi
else
  # Host binary: reachable from CNI sandboxes at 10.88.0.1:8443.
  mkdir -p "\$RUN_DIR"
  : >"\$RUN_DIR/gateway-ledger.jsonl"
  nohup "\$BIN_DIR/gateway" \\
    --config "\$RUN_DIR/gateway.json" \\
    --ledger "\$RUN_DIR/gateway-ledger.jsonl" \\
    --admin 127.0.0.1:8444 \\
    >"\$RUN_DIR/gateway.log" 2>&1 &
  echo \$! >"\$RUN_DIR/gateway.pid"
fi

for _ in \$(seq 1 40); do
  curl -sf -o /dev/null -H "Authorization: Bearer \$GATEWAY_ADMIN_TOKEN" \\
    http://127.0.0.1:8444/routes && exit 0
  sleep 0.25
done
echo "gateway failed to become ready; see \$RUN_DIR/gateway.log" >&2
exit 1
EOF
  chmod +x "$RUN_DIR/start-gateway.sh"

  cat >"$RUN_DIR/start-serve.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
REPO="$REPO"
RUN_DIR="$RUN_DIR"
BIN_DIR="$BIN_DIR"
RUNTIME="$RUNTIME"
POOL_N="$POOL_N"
UI_PORT="$UI_PORT"
PROFILE="$PROFILE"
IMAGE_SANDBOX="$IMAGE_SANDBOX"
ROUTE="$ROUTE"
# shellcheck disable=SC1091
source "\$RUN_DIR/env"
export GATEWAY_ADMIN_TOKEN

COMMON=(
  --storage zfs --pool tank --runtime "\$RUNTIME" --root "\$RUN_DIR/router-root"
  --network wsnet --gateway "$(gateway_url)" --route "\$ROUTE"
  --gateway-admin http://127.0.0.1:8444
  --profile "\$PROFILE" --image "\$IMAGE_SANDBOX"
)

if [[ -f "\$RUN_DIR/serve.pid" ]] && kill -0 "\$(cat "\$RUN_DIR/serve.pid")" 2>/dev/null; then
  echo "router serve already running (pid=\$(cat "\$RUN_DIR/serve.pid"))"
  exit 0
fi

POOL_FLAG=()
if [[ "\$RUNTIME" == "gvisor" ]]; then
  POOL_FLAG=(--pool-n "\$POOL_N")
fi

# Bind loopback inside the VM so ROUTER_API_TOKEN is not required; reach it
# from the Mac via the SSH local-forward installed by install.sh.
nohup "\$BIN_DIR/router" serve "\${COMMON[@]}" \\
  --listen "127.0.0.1:\${UI_PORT}" "\${POOL_FLAG[@]}" \\
  >"\$RUN_DIR/serve.log" 2>&1 &
echo \$! >"\$RUN_DIR/serve.pid"
  echo "router serve started pid=\$(cat "\$RUN_DIR/serve.pid") log=\$RUN_DIR/serve.log"
EOF
  chmod +x "$RUN_DIR/start-serve.sh"

  cat >"$RUN_DIR/start-tunnel.sh" <<EOF
#!/usr/bin/env bash
# Remotely-managed Cloudflare Tunnel (DESIGN §6 tunnel mode).
# Installs cloudflared from pkg.cloudflare.com and runs:
#   cloudflared service install <token>
# Public hostname / Service URL are configured in the Cloudflare dashboard
# (Service URL must be http://127.0.0.1:${UI_PORT}).
set -euo pipefail
RUN_DIR="$RUN_DIR"
UI_PORT="$UI_PORT"
# shellcheck disable=SC1091
source "\$RUN_DIR/env"
: "\${CLOUDFLARE_TUNNEL_TOKEN:?CLOUDFLARE_TUNNEL_TOKEN missing in \$RUN_DIR/env}"

if ! command -v cloudflared >/dev/null 2>&1; then
  echo "installing cloudflared (Ubuntu noble package repo)"
  mkdir -p /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \\
    | tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
  echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared noble main' \\
    > /etc/apt/sources.list.d/cloudflared.list
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y cloudflared
fi
cloudflared --version

if systemctl is-active --quiet cloudflared 2>/dev/null; then
  echo "cloudflared service already active"
  systemctl status cloudflared --no-pager -l | head -n 12 || true
  exit 0
fi

# Remotely-managed tunnel: token embeds tunnel id + credentials.
# Only one cloudflared systemd unit may exist per host.
if systemctl list-unit-files cloudflared.service >/dev/null 2>&1 \\
   || [[ -f /etc/systemd/system/cloudflared.service ]]; then
  cloudflared service uninstall 2>/dev/null || true
fi

cloudflared service install "\$CLOUDFLARE_TUNNEL_TOKEN"
systemctl enable --now cloudflared 2>/dev/null || systemctl start cloudflared

for _ in \$(seq 1 30); do
  if systemctl is-active --quiet cloudflared; then
    echo "cloudflared is active — set dashboard Published application Service URL to http://127.0.0.1:\${UI_PORT}"
    exit 0
  fi
  sleep 1
done
echo "cloudflared failed to become active; journal:" >&2
journalctl -u cloudflared -n 40 --no-pager >&2 || true
exit 1
EOF
  chmod +x "$RUN_DIR/start-tunnel.sh"
}

start_gateway() {
  say "gateway"
  # Docker path needs root for docker; host-binary path needs root only if
  # binding privileged ports — 8443 is fine as root for consistency with
  # the rest of the data plane.
  vm_root bash "$RUN_DIR/start-gateway.sh"
  info "gateway admin responding on VM :8444"
}

# ---------------------------------------------------------------------------
# Warm pool / serve / smoke
# ---------------------------------------------------------------------------

fill_warm_pool() {
  say "warm pool"
  if [[ "$RUNTIME" != "gvisor" || "$FILL_POOL" != "yes" ]]; then
    info "skipped"
    return
  fi
  vm_root env GATEWAY_ADMIN_TOKEN="$GATEWAY_ADMIN_TOKEN" \
    "$BIN_DIR/router" pool fill --pool-n "$POOL_N" \
    --storage zfs --pool tank --runtime gvisor --root "$RUN_DIR/router-root" \
    --network wsnet --gateway "$(gateway_url)" --route "$ROUTE" \
    --gateway-admin http://127.0.0.1:8444 \
    --profile "$PROFILE" --image "$IMAGE_SANDBOX"
  vm_root env GATEWAY_ADMIN_TOKEN="$GATEWAY_ADMIN_TOKEN" \
    "$BIN_DIR/router" pool status \
    --storage zfs --pool tank --runtime gvisor --root "$RUN_DIR/router-root" \
    --network wsnet --gateway "$(gateway_url)" --route "$ROUTE" \
    --gateway-admin http://127.0.0.1:8444 \
    --profile "$PROFILE" --image "$IMAGE_SANDBOX" || true
}

ensure_port_forward() {
  say "UI port forward"
  if [[ "$EXPOSE_UI" != "yes" ]]; then
    info "skipped"
    return
  fi
  if [[ -f "$RUN_DIR/ui-forward.pid" ]] && kill -0 "$(cat "$RUN_DIR/ui-forward.pid")" 2>/dev/null; then
    info "already forwarding localhost:$UI_PORT"
    return
  fi
  local cfg="$RUN_DIR/lima-ssh.config"
  limactl show-ssh --format=config "$VM_NAME" >"$cfg"
  # Host alias in the generated config is lima-<name>.
  local host="lima-$VM_NAME"
  ssh -F "$cfg" -f -N -o ExitOnForwardFailure=yes \
    -L "127.0.0.1:${UI_PORT}:127.0.0.1:${UI_PORT}" "$host"
  pgrep -f "ssh -F $cfg .*${UI_PORT}:127.0.0.1:${UI_PORT}" \
    | head -1 >"$RUN_DIR/ui-forward.pid" || true
  info "Mac http://127.0.0.1:$UI_PORT → VM 127.0.0.1:$UI_PORT"
}

start_serve() {
  say "router serve"
  vm_root bash "$RUN_DIR/start-serve.sh"
  # Wait for listen — install fails if the server never comes up.
  local i
  for i in $(seq 1 60); do
    if vm curl -sf --max-time 1 "http://127.0.0.1:${UI_PORT}/api/workspaces" >/dev/null 2>&1; then
      info "serve is up on VM :$UI_PORT"
      return
    fi
    # Surface a dead process early.
    if vm_root bash -c "[[ -f '$RUN_DIR/serve.pid' ]] && ! kill -0 \$(cat '$RUN_DIR/serve.pid') 2>/dev/null"; then
      warn "serve process exited; log:"
      vm_root tail -n 40 "$RUN_DIR/serve.log" || true
      die "router serve failed to stay up"
    fi
    sleep 0.5
  done
  warn "serve log (last 40 lines):"
  vm_root tail -n 40 "$RUN_DIR/serve.log" || true
  die "serve did not answer /api/workspaces on :$UI_PORT"
}

smoke_workspace() {
  say "smoke workspace"
  if [[ "$SMOKE_UP" != "yes" ]]; then
    info "skipped"
    return
  fi
  vm_root env GATEWAY_ADMIN_TOKEN="$GATEWAY_ADMIN_TOKEN" \
    "$BIN_DIR/router" up --id smoke \
    --storage zfs --pool tank --runtime "$RUNTIME" --root "$RUN_DIR/router-root" \
    --network wsnet --gateway "$(gateway_url)" --route "$ROUTE" \
    --gateway-admin http://127.0.0.1:8444 \
    --profile "$PROFILE" --image "$IMAGE_SANDBOX"
  info "workspace 'smoke' is up"
}

# ---------------------------------------------------------------------------
# Cloudflare Tunnel (DESIGN §6 tunnel mode — convenience opt-out)
# ---------------------------------------------------------------------------

ensure_cloudflare_tunnel() {
  say "Cloudflare Tunnel"
  if [[ "$DEPLOY_TUNNEL" != "yes" ]]; then
    info "skipped"
    return
  fi
  [[ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]] \
    || die "DEPLOY_TUNNEL=yes but CLOUDFLARE_TUNNEL_TOKEN is empty"

  if vm_root systemctl is-active --quiet cloudflared 2>/dev/null; then
    info "cloudflared already active in the VM"
    if confirm "Reinstall Cloudflare Tunnel service (new token / force)?" "N"; then
      vm_root cloudflared service uninstall 2>/dev/null || true
    else
      info "keeping existing tunnel connector"
      if [[ -n "${TUNNEL_HOSTNAME:-}" ]]; then
        info "expected public URL: https://$TUNNEL_HOSTNAME/"
      fi
      return
    fi
  fi

  info "installing/running cloudflared inside the VM (token-based / remotely managed)"
  vm_root bash "$RUN_DIR/start-tunnel.sh"
  info "tunnel connector is up"
  if [[ -n "${TUNNEL_HOSTNAME:-}" ]]; then
    info "configure dashboard route: $TUNNEL_HOSTNAME → http://127.0.0.1:$UI_PORT"
  else
    info "configure a Published application in the dashboard → http://127.0.0.1:$UI_PORT"
  fi
  info "docs: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/"
}

print_done() {
  say "done — server is running"
  cat <<EOF
  VM:       limactl shell $VM_NAME
  Binaries: $BIN_DIR/{router,gateway}
  Secrets:  $ENV_FILE
  Helpers:  $RUN_DIR/start-gateway.sh
            $RUN_DIR/start-serve.sh
            $RUN_DIR/start-tunnel.sh

  Gateway URL for sandboxes: $(gateway_url)
  Runtime:                   $RUNTIME

EOF
  if [[ "$EXPOSE_UI" == "yes" ]]; then
    echo "  UI:  http://127.0.0.1:$UI_PORT/        (end-user)"
    echo "       http://127.0.0.1:$UI_PORT/admin  (operator)"
  else
    echo "  UI inside VM: http://127.0.0.1:$UI_PORT/"
    echo "  Forward later:"
    echo "    limactl show-ssh --format=config $VM_NAME > $RUN_DIR/lima-ssh.config"
    echo "    ssh -F $RUN_DIR/lima-ssh.config -N -L ${UI_PORT}:127.0.0.1:${UI_PORT} lima-$VM_NAME"
  fi
  if [[ "$DEPLOY_TUNNEL" == "yes" ]]; then
    echo
    echo "  Cloudflare Tunnel (DESIGN §6 tunnel mode):"
    echo "    connector: cloudflared systemd unit inside the VM"
    if [[ -n "${TUNNEL_HOSTNAME:-}" ]]; then
      echo "    hostname:  https://$TUNNEL_HOSTNAME/"
    fi
    echo "    dashboard: https://dash.cloudflare.com/?to=/:account/tunnels"
    echo "    Service URL must be: http://127.0.0.1:$UI_PORT"
    echo "    re-run: limactl shell $VM_NAME -- sudo bash $RUN_DIR/start-tunnel.sh"
  fi
  echo
  cat <<EOF
  Useful:
    limactl shell $VM_NAME -- sudo bash $RUN_DIR/start-gateway.sh
    limactl shell $VM_NAME -- sudo bash $RUN_DIR/start-serve.sh
    limactl stop $VM_NAME          # ZFS pool survives stop/start (verified)
EOF
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

main() {
  check_host
  gather_plan
  ensure_disk
  ensure_vm
  ensure_provisioned
  build_frontend
  build_binaries
  ensure_gateway_config
  write_run_helpers
  build_images
  start_gateway
  fill_warm_pool
  start_serve
  ensure_port_forward
  ensure_cloudflare_tunnel
  smoke_workspace
  print_done
}

main "$@"
