#!/usr/bin/env bash
# Cold-start microbench for router up (and optional agent-ready).
#
# Intended to run inside the Lima dataplane VM (ZFS + gVisor + containerd):
#   limactl shell dataplane -- sudo bash /Users/sami/Developer/containerization/scripts/bench-cold-start.sh
#
# Claims under test (PERFORMANCE.md / dataplane-vm/README.md):
#   containerd + gateway session ≈ 220ms
#   docker/runsc path ≈ 1–3s
set -euo pipefail

REPO="${REPO:-/Users/sami/Developer/containerization}"
ROUTER="${ROUTER:-$REPO/dataplane-vm/bin/router}"
GATEWAY_BIN="${GATEWAY_BIN:-$REPO/dataplane-vm/bin/gateway}"
PROFILE="$REPO/examples/profile"
IMAGE="${IMAGE:-opencode-sandbox:v1}"
WS_ID="${WS_ID:-coldbench}"
ITERS="${ITERS:-7}"
ADMIN_TOKEN="${GATEWAY_ADMIN_TOKEN:-bench-admin-token}"
ROOT="/tmp/coldbench-root"
LEDGER="$ROOT/bench-ledger.jsonl"
GW_CFG="$ROOT/gateway.json"
GW_PID=""

mkdir -p "$ROOT"
export GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN"

cleanup() {
  "$ROUTER" destroy --id "$WS_ID" --storage zfs --pool tank --runtime none --root "$ROOT" >/dev/null 2>&1 || true
  if [ -n "$GW_PID" ] && kill -0 "$GW_PID" 2>/dev/null; then
    kill "$GW_PID" 2>/dev/null || true
    wait "$GW_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

ensure_gateway() {
  if curl -sf -o /dev/null -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8444/routes; then
    echo "gateway admin already up on :8444"
    return
  fi
  cat >"$GW_CFG" <<'EOF'
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
  : >"$LEDGER"
  # No real provider key needed for session register / cold-start timing.
  ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-sk-ant-bench-placeholder}" \
    "$GATEWAY_BIN" --config "$GW_CFG" --ledger "$LEDGER" --admin 127.0.0.1:8444 \
    >"$ROOT/gateway.log" 2>&1 &
  GW_PID=$!
  for _ in $(seq 1 40); do
    curl -sf -o /dev/null -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8444/routes && break
    sleep 0.1
  done
  curl -sf -o /dev/null -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8444/routes \
    || { echo "gateway failed to start; log:"; cat "$ROOT/gateway.log"; exit 1; }
  echo "started gateway pid=$GW_PID"
}

ms_now() {
  # nanoseconds → ms integer (GNU date on the Linux VM)
  echo $(($(date +%s%N) / 1000000))
}

agent_endpoint() {
  local runtime="$1"
  if [ "$runtime" = "containerd" ] || [ "$runtime" = "containerd-runc" ]; then
    local ip
    ip=$(ctr -n dataplane containers info "ws-$WS_ID" 2>/dev/null \
      | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("Labels",{}).get("platform.agent-ip",""))' 2>/dev/null || true)
    if [ -n "$ip" ]; then
      echo "$ip:4321"
      return
    fi
  fi
  # Docker path: published port or container IP
  if out=$(docker port "ws-$WS_ID" 4321/tcp 2>/dev/null | head -1); then
    echo "$out"
    return
  fi
  local ip
  ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "ws-$WS_ID" 2>/dev/null || true)
  if [ -n "$ip" ]; then
    echo "$ip:4321"
  fi
}

wait_agent() {
  local ep="$1" deadline=$(( $(ms_now) + 15000 ))
  [ -n "$ep" ] || { echo "no-endpoint"; return 1; }
  while [ "$(ms_now)" -lt "$deadline" ]; do
    if curl -sf --max-time 1 "http://$ep/global" >/dev/null 2>&1 \
      || curl -sf --max-time 1 "http://$ep/" >/dev/null 2>&1 \
      || curl -sf --max-time 1 "http://$ep/busy" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

stats() {
  # args: integer ms samples → count min median max mean
  # (must not read stdin — a heredoc would steal a piped sample stream)
  python3 -c '
import sys
xs=sorted(int(x) for x in sys.argv[1:])
if not xs:
  print("n=0"); raise SystemExit
n=len(xs); mean=sum(xs)/n
med=xs[n//2] if n%2 else (xs[n//2-1]+xs[n//2])/2
print(f"n={n} min={xs[0]} median={med:.0f} mean={mean:.0f} max={xs[-1]}  (ms)")
print("samples=" + ",".join(map(str, xs)))
' "$@"
}

bench_runtime() {
  local runtime="$1" gateway_url="$2" network="$3"
  local -a up_ms=() ready_ms=()
  echo
  echo "========== runtime=$runtime network=$network gateway=$gateway_url iters=$ITERS =========="

  # One throwaway up/destroy to warm image/code paths (not counted).
  echo "(warmup)"
  "$ROUTER" destroy --id "$WS_ID" --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" >/dev/null 2>&1 || true
  GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN" "$ROUTER" up --id "$WS_ID" \
    --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" \
    --network "$network" --gateway "$gateway_url" --gateway-admin http://127.0.0.1:8444 \
    --route tier-a --profile "$PROFILE" --image "$IMAGE" >/dev/null
  "$ROUTER" destroy --id "$WS_ID" --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" >/dev/null

  for i in $(seq 1 "$ITERS"); do
    "$ROUTER" destroy --id "$WS_ID" --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" >/dev/null 2>&1 || true
    # Ensure no leftover container/task
    docker rm -f "ws-$WS_ID" >/dev/null 2>&1 || true
    ctr -n dataplane tasks kill -s SIGKILL "ws-$WS_ID" >/dev/null 2>&1 || true
    ctr -n dataplane tasks delete "ws-$WS_ID" >/dev/null 2>&1 || true
    ctr -n dataplane containers delete "ws-$WS_ID" >/dev/null 2>&1 || true

    local t0 t1 elapsed ep t2 ready
    t0=$(ms_now)
    GATEWAY_ADMIN_TOKEN="$ADMIN_TOKEN" "$ROUTER" up --id "$WS_ID" \
      --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" \
      --network "$network" --gateway "$gateway_url" --gateway-admin http://127.0.0.1:8444 \
      --route tier-a --profile "$PROFILE" --image "$IMAGE" >/dev/null
    t1=$(ms_now)
    elapsed=$((t1 - t0))
    up_ms+=("$elapsed")

    ep=$(agent_endpoint "$runtime" || true)
    t2=$(ms_now)
    if wait_agent "$ep"; then
      ready=$(( $(ms_now) - t0 ))
      ready_ms+=("$ready")
      printf "  iter %d: up=%4dms  agent_ready=%4dms  ep=%s\n" "$i" "$elapsed" "$ready" "$ep"
    else
      printf "  iter %d: up=%4dms  agent_ready=TIMEOUT  ep=%s\n" "$i" "$elapsed" "${ep:-none}"
    fi

    "$ROUTER" destroy --id "$WS_ID" --storage zfs --pool tank --runtime "$runtime" --root "$ROOT" >/dev/null
  done

  echo -n "router up:        "
  stats "${up_ms[@]}"
  if [ "${#ready_ms[@]}" -gt 0 ]; then
    echo -n "agent ready:      "
    stats "${ready_ms[@]}"
  fi
}

echo "Cold-start bench starting $(date -Is)"
echo "router=$ROUTER image=$IMAGE ws=$WS_ID"
ensure_gateway

# Match dataplane-vm README paths.
bench_runtime containerd "http://10.88.0.1:8443" wsnet
bench_runtime runsc      "http://10.88.0.1:8443" wsnet

echo
echo "Done $(date -Is)"
echo "Note: 'router up' is the claimed metric (~220ms containerd / ~1-3s docker)."
echo "      'agent ready' includes OpenCode/Node listen and is a stricter UX metric."
