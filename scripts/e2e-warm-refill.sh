#!/usr/bin/env bash
# E2E: serve-mode warm-pool auto-refill (Lima dataplane VM).
#
#   limactl shell dataplane -- sudo bash /Users/sami/Developer/containerization/scripts/e2e-warm-refill.sh
#
# Verifies:
#   1. serve --pool-n 2 builds initial inventory in the background
#   2. POST /api/workspaces claims a slot (warm path)
#   3. pool returns to 2 ready slots without a manual fill
set -euo pipefail

REPO="${REPO:-/Users/sami/Developer/containerization}"
ROUTER="${ROUTER:-$REPO/dataplane-vm/bin/router}"
ADMIN_TOKEN="${GATEWAY_ADMIN_TOKEN:-bench-admin-token}"
export GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN"

PROFILE="$REPO/examples/profile"
IMAGE="${IMAGE:-opencode-sandbox:v1}"
ROOT="${ROOT:-/tmp/warm-refill-e2e}"
LISTEN="127.0.0.1:8400"
WS_ID="refill-e2e"
TARGET=2

COMMON=(
  --storage zfs --pool tank --runtime gvisor --root "$ROOT"
  --network wsnet --gateway http://10.88.0.1:8443 --route tier-a
  --gateway-admin http://127.0.0.1:8444
  --profile "$PROFILE" --image "$IMAGE"
)

slot_count() {
  "$ROUTER" pool status "${COMMON[@]}" 2>/dev/null | grep -c . || true
}

cleanup() {
  echo "== cleanup =="
  if [[ -n "${SERVE_PID:-}" ]] && kill -0 "$SERVE_PID" 2>/dev/null; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
  fi
  "$ROUTER" destroy --id "$WS_ID" "${COMMON[@]}" >/dev/null 2>&1 || true
  "$ROUTER" pool drain "${COMMON[@]}" >/dev/null 2>&1 || true
  # Best-effort leftover golden datasets / sandbox state
  zfs list -H -o name 2>/dev/null | grep -E "tank/workspaces/(warm-|${WS_ID})" \
    | xargs -r -n1 zfs destroy -r 2>/dev/null || true
  umount "$ROOT"/gvisor/rootfs/* 2>/dev/null || true
  rm -rf "$ROOT"
}
trap cleanup EXIT

echo "== pre-clean =="
cleanup
trap cleanup EXIT
mkdir -p "$ROOT"
SERVE_LOG="$ROOT/serve.log"

echo "== start serve (pool-n=$TARGET) =="
"$ROUTER" serve "${COMMON[@]}" --listen "$LISTEN" --pool-n "$TARGET" \
  >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!

# Wait for listen + auto-refill banner
for i in $(seq 1 50); do
  if grep -q "warm pool auto-refill enabled" "$SERVE_LOG" 2>/dev/null \
     && curl -sf --max-time 1 "http://$LISTEN/api/workspaces" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$SERVE_PID" 2>/dev/null; then
    echo "serve died:"; cat "$SERVE_LOG"; exit 1
  fi
  sleep 0.1
done
grep -q "warm pool auto-refill enabled" "$SERVE_LOG" \
  || { echo "missing auto-refill banner"; cat "$SERVE_LOG"; exit 1; }
echo "serve up (pid=$SERVE_PID)"

echo "== wait for initial fill to $TARGET slots =="
deadline=$((SECONDS + 180))
while (( SECONDS < deadline )); do
  n=$(slot_count)
  echo "  ready slots: $n"
  if (( n >= TARGET )); then
    break
  fi
  if grep -q "warning: warm pool refill:" "$SERVE_LOG" 2>/dev/null; then
    echo "refill error:"; grep "warm pool" "$SERVE_LOG" || true
    exit 1
  fi
  sleep 1
done
n=$(slot_count)
if (( n < TARGET )); then
  echo "FAIL: initial fill stuck at $n (want $TARGET)"
  cat "$SERVE_LOG"
  exit 1
fi
grep -q "warm pool: refilled" "$SERVE_LOG" && echo "initial refill logged" || true

echo "== claim via API (expect warm restore) =="
t0=$(date +%s%N)
code=$(curl -s -o "$ROOT/up.json" -w "%{http_code}" -X POST "http://$LISTEN/api/workspaces" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$WS_ID\"}")
t1=$(date +%s%N)
up_ms=$(( (t1 - t0) / 1000000 ))
echo "  POST /api/workspaces → HTTP $code in ${up_ms}ms"
[[ "$code" == "200" || "$code" == "201" ]] || { cat "$ROOT/up.json"; echo "FAIL: up"; exit 1; }
cat "$ROOT/up.json"
echo

after_claim=$(slot_count)
echo "  ready slots immediately after claim: $after_claim"
if (( after_claim >= TARGET )); then
  echo "FAIL: pool did not drop after claim (still $after_claim)"
  exit 1
fi

echo "== wait for auto-refill back to $TARGET =="
deadline=$((SECONDS + 180))
while (( SECONDS < deadline )); do
  n=$(slot_count)
  echo "  ready slots: $n"
  if (( n >= TARGET )); then
    break
  fi
  sleep 1
done
n=$(slot_count)
if (( n < TARGET )); then
  echo "FAIL: auto-refill stuck at $n after claim"
  grep "warm pool" "$SERVE_LOG" || true
  cat "$SERVE_LOG"
  exit 1
fi

# Confirm agent answers (warm path should be near-instant once up returned)
ep=$("$ROUTER" status "${COMMON[@]}" 2>/dev/null | awk -v id="$WS_ID" '$1==id || $1~id {print; exit}')
echo "  status: $ep"
ready=0
for i in $(seq 1 60); do
  # AgentEndpoint isn't on CLI; probe via serve proxy
  if curl -sf --max-time 1 "http://$LISTEN/api/workspaces/$WS_ID/opencode/" >/dev/null 2>&1 \
     || curl -sf --max-time 1 "http://$LISTEN/api/workspaces/$WS_ID/opencode/app" >/dev/null 2>&1 \
     || curl -sf --max-time 1 "http://$LISTEN/api/workspaces/$WS_ID/opencode/busy" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
if (( ready != 1 )); then
  echo "WARN: agent HTTP probe via proxy did not return 200 (may be path/version); continuing"
fi

echo
echo "PASS: serve auto-refill kept pool at $TARGET after a warm claim (${up_ms}ms create)"
grep "warm pool" "$SERVE_LOG" || true
