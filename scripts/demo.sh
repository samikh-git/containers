#!/usr/bin/env bash
# Bring up the governed AI workspace stack and test it — identically on the
# Mac (dev) and the Mac Mini (data-plane reference hardware).
#
#   scripts/demo.sh build     build sandbox + gateway images, the router binary
#   scripts/demo.sh up        start gateway (compose) + wsnet, provision workspace "demo"
#   scripts/demo.sh test      send a real prompt through OpenCode -> gateway -> model
#   scripts/demo.sh check     verify the isolation properties (no egress, no keys, ledger intact)
#   scripts/demo.sh status    show what's running
#   scripts/demo.sh down      stop the workspace, stop the gateway
#
# First run:
#   export ANTHROPIC_API_KEY=sk-ant-...
#   scripts/demo.sh build && scripts/demo.sh up && scripts/demo.sh test && scripts/demo.sh check
#
# Requires: Docker Desktop running, go, jq, curl.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="${WORKDIR:-$REPO/.demo}"
ENV_FILE="$WORKDIR/.env"
WS_ID="${WS_ID:-demo}"
IMAGE="opencode-sandbox:v1"
ROUTER="$WORKDIR/bin/router"

mkdir -p "$WORKDIR/bin" "$WORKDIR/wsdata"

# --- Runtime choice: runsc when present (the real §8 posture — the Mac
# Mini's production Linux VM, or any bare Linux host), runc otherwise (macOS
# via Docker Desktop, where no gVisor is available). Same script, honest
# about which posture it's actually running.
runtime_flag() {
  if docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q runsc; then
    echo runsc
  else
    echo runc
  fi
}

need() { command -v "$1" >/dev/null || { echo "missing required tool: $1" >&2; exit 1; }; }
need docker; need go; need jq; need curl

ensure_env() {
  if [ ! -f "$ENV_FILE" ]; then
    : "${ANTHROPIC_API_KEY:?Set ANTHROPIC_API_KEY before first run (gateway needs it to reach the model).}"
    printf 'ANTHROPIC_API_KEY=%s\nGATEWAY_ADMIN_TOKEN=%s\n' \
      "$ANTHROPIC_API_KEY" "$(openssl rand -hex 16)" > "$ENV_FILE"
    echo "wrote $ENV_FILE (holds your API key + a generated admin token — not committed, see .gitignore)"
  fi
  set -a; source "$ENV_FILE"; set +a
}

cmd_build() {
  echo "== sandbox image =="
  docker build -t "$IMAGE" "$REPO/sandbox-image"
  echo "== gateway image + router binary =="
  ensure_env
  docker compose -f "$REPO/docker-compose.yml" build
  go -C "$REPO" build -o "$ROUTER" ./cmd/router
  echo "build complete."
}

cmd_up() {
  ensure_env
  [ -f "$REPO/gateway.local.json" ] || cp "$REPO/gateway.local.json.example" "$REPO/gateway.local.json"

  echo "== gateway + wsnet (docker compose) =="
  docker compose -f "$REPO/docker-compose.yml" up -d
  for i in $(seq 1 20); do
    curl -sf http://127.0.0.1:8444/sessions -X DELETE -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
      >/dev/null 2>&1 && break
    sleep 1
  done

  RT="$(runtime_flag)"
  echo "== workspace \"$WS_ID\" (runtime: $RT) =="
  GATEWAY_ADMIN_TOKEN="$GATEWAY_ADMIN_TOKEN" "$ROUTER" up --id "$WS_ID" \
    --storage dir --root "$WORKDIR/wsdata" --runtime "$RT" \
    --network wsnet --gateway http://gateway:8443 --gateway-admin http://127.0.0.1:8444 \
    --route tier-a --profile "$REPO/examples/profile" --image "$IMAGE"

  if [ "$RT" = "runc" ]; then
    echo
    echo "NOTE: running under runc, not gVisor (runsc) — Docker Desktop on macOS has"
    echo "no gVisor runtime. This proves the network/credential isolation model"
    echo "(DESIGN §9); the syscall-level sandbox boundary (§8) only exists on a real"
    echo "Linux host or the data-plane Linux VM. See README for that distinction."
  fi
}

cmd_test() {
  echo "== sending a prompt through OpenCode -> gateway -> model =="
  local sid
  sid=$(docker exec "ws-$WS_ID" curl -s -X POST http://127.0.0.1:4321/session \
        -H 'Content-Type: application/json' -d '{}' | jq -r .id)
  echo "session: $sid"
  docker exec "ws-$WS_ID" curl -s -X POST "http://127.0.0.1:4321/session/$sid/message" \
    -H 'Content-Type: application/json' \
    -d '{"parts":[{"type":"text","text":"Say hello in exactly one word."}]}' \
    | jq '{error: .info.error.data.message, reply: (.parts[]? | select(.type=="text") | .text)}'
}

cmd_check() {
  echo "== 1. sandbox has no direct internet (must fail) =="
  # curl -w always prints %{http_code} — "000" on connection failure — even
  # when curl itself exits nonzero, so don't append a fallback string on top
  # of it. curl's nonzero exit IS the expected outcome here, so guard the
  # capture with `|| true` — set -e would otherwise abort the whole script.
  code=$(docker exec "ws-$WS_ID" curl -s --max-time 5 -o /dev/null -w '%{http_code}' https://api.anthropic.com 2>/dev/null || true)
  echo "  http_code: ${code:-<none>}"
  [ "$code" = "000" ] || echo "  WARNING: sandbox reached the internet directly — network isolation is broken"

  echo "== 2. sandbox holds no provider key =="
  docker exec "ws-$WS_ID" sh -c '
    env | grep -E "^OPENCODE_(BASE_URL|SESSION_TOKEN)=" | sed "s/=st-.*/=st-…redacted/"
    if env | grep -q ANTHROPIC_API_KEY; then echo "  WARNING: provider key present in sandbox env"; else echo "  no ANTHROPIC_* vars in sandbox"; fi'

  echo "== 3. audit ledger hash chain =="
  docker run --rm -v "$(docker volume inspect containerization_gateway-audit --format '{{.Name}}' 2>/dev/null || echo containerization_gateway-audit):/var/lib/gateway" \
    --entrypoint /gateway "policy-gateway:v1" --verify /var/lib/gateway/audit.jsonl
}

cmd_status() {
  echo "== containers ==" && docker ps --filter "name=ws-$WS_ID" --filter name=gateway \
    --format 'table {{.Names}}\t{{.Status}}\t{{.Networks}}'
  echo "== router status ==" && "$ROUTER" status --storage dir --root "$WORKDIR/wsdata" \
    --runtime "$(runtime_flag)" 2>/dev/null || true
}

cmd_down() {
  ensure_env
  "$ROUTER" destroy --id "$WS_ID" --storage dir --root "$WORKDIR/wsdata" \
    --runtime "$(runtime_flag)" 2>/dev/null || true
  docker compose -f "$REPO/docker-compose.yml" down
}

case "${1:-}" in
  build)  cmd_build ;;
  up)     cmd_up ;;
  test)   cmd_test ;;
  check)  cmd_check ;;
  status) cmd_status ;;
  down)   cmd_down ;;
  *) echo "usage: $0 {build|up|test|check|status|down}" >&2; exit 2 ;;
esac
