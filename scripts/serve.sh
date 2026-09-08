#!/usr/bin/env bash
# Start the REST API + embedded web UI for local Docker Desktop development.
#
# Sources $REPO/.env (API keys) then .demo/.env (GATEWAY_ADMIN_TOKEN), brings
# the gateway up if needed, and serves on http://127.0.0.1:8400.
#
# On macOS, sandboxes use the default bridge network with published loopback
# ports — Docker Desktop cannot dial container IPs on --internal networks
# (wsnet), which freezes the chat UI. Gateway traffic goes via
# host.docker.internal:18443 (compose publishes container :8443 there so it
# does not collide with a Lima dataplane forward on :8443).
#
#   scripts/serve.sh
#   LOG_LEVEL=debug scripts/serve.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="${WORKDIR:-$REPO/.demo}"
ROUTER="${ROUTER:-$WORKDIR/bin/router}"
ROOT="${WORKSPACE_ROOT:-$HOME/.workspace-data}"

cd "$REPO"

# .demo/.env holds GATEWAY_ADMIN_TOKEN only. Root .env holds API keys and
# must be sourced last so a stale ANTHROPIC_API_KEY in .demo/.env cannot
# override the real one (that produced Anthropic 401 invalid x-api-key).
if [ -f "$WORKDIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  source "$WORKDIR/.env"
  set +a
fi
if [ -f "$REPO/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  source "$REPO/.env"
  set +a
fi

: "${GATEWAY_ADMIN_TOKEN:?Missing GATEWAY_ADMIN_TOKEN — run scripts/demo.sh build once, or add it to .demo/.env}"
: "${ANTHROPIC_API_KEY:?Missing ANTHROPIC_API_KEY — add it to $REPO/.env}"

if [ ! -x "$ROUTER" ]; then
  echo "building router → $ROUTER"
  mkdir -p "$(dirname "$ROUTER")"
  go build -o "$ROUTER" ./cmd/router
fi

# Ensure gateway is up with host-published policy port (for host.docker.internal).
docker compose -f "$REPO/docker-compose.yml" up -d

runtime=runc
if docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q runsc; then
  runtime=runsc
fi

GATEWAY_URL="${GATEWAY_URL:-http://host.docker.internal:18443/v1}"
ROUTE="${ROUTE:-tier-a,tier-b-openrouter}"

exec env GATEWAY_ADMIN_TOKEN="$GATEWAY_ADMIN_TOKEN" \
  ROUTER_API_TOKEN="${ROUTER_API_TOKEN:-}" \
  "$ROUTER" serve \
  --storage dir --root "$ROOT" \
  --runtime "$runtime" \
  --network bridge \
  --gateway "$GATEWAY_URL" \
  --gateway-admin http://127.0.0.1:8444 \
  --route "$ROUTE" \
  --profile "$REPO/examples/profile" \
  --image opencode-sandbox:v1 \
  --pool-n 0 \
  --log-level "${LOG_LEVEL:-info}"
