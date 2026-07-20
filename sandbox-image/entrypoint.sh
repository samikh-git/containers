#!/bin/sh
# Sandbox entrypoint. The rootfs is read-only; the only writable places are
# /workspace (ZFS bind mount) and /tmp (tmpfs). Everything here must respect that.
set -eu

# 1. First boot of this workspace/clone: create the state dirs that HOME and
#    the cache env vars point at. On a cloned branch these already exist —
#    inherited warm from the parent snapshot — and this is a no-op.
mkdir -p \
  /workspace/.home \
  /workspace/.cache \
  /workspace/.config \
  /workspace/.local/share

# 2. Fail loudly if the router didn't wire the sandbox correctly. These are
#    contract checks, not configuration — a sandbox without a profile or
#    gateway must not run (DESIGN §9, §10).
[ -f "${OPENCODE_CONFIG}" ] || { echo "FATAL: capability profile not mounted at ${OPENCODE_CONFIG}" >&2; exit 64; }
[ -n "${OPENCODE_BASE_URL:-}" ] || { echo "FATAL: OPENCODE_BASE_URL not set (policy gateway)" >&2; exit 64; }

# 3. Git identity comes from the profile bundle if the org curates one;
#    otherwise a neutral default (commits are attributed properly at PR time).
if [ -f /etc/agent-profile/gitconfig ]; then
  export GIT_CONFIG_GLOBAL=/etc/agent-profile/gitconfig
fi

# 4. First boot: make /workspace a git repository. Diff review in the UI
#    (and OpenCode's own change tracking) works against VCS state; a baseline
#    commit gives "everything the agent did since the workspace was created".
#    Cloned branches inherit their parent's repo and history — a no-op here.
if [ ! -d /workspace/.git ]; then
  cat > /workspace/.gitignore <<'EOF'
.home/
.cache/
.config/
.local/
.agent-state/
EOF
  git -C /workspace init -q -b main
  git -C /workspace -c user.name=workspace -c user.email=workspace@local \
    add -A
  git -C /workspace -c user.name=workspace -c user.email=workspace@local \
    commit -qm "workspace created" --allow-empty
fi

# 5. Agent checkpoint restore (§7 stop sequence writes here on SIGTERM).
if [ -f /workspace/.agent-state/checkpoint.json ]; then
  echo "resuming from checkpoint $(date -u +%FT%TZ)" >&2
fi

# 6. Web terminal bridge on 4322 (same reachability as 4321: router only,
#    §8). Backgrounded — it dies with the sandbox; shells are not part of
#    the checkpoint contract (§7: /workspace survives, processes don't).
termbridge &

# 7. Hand PID 1 to the harness. OpenCode serves the router on 4321 (bound
#    inside the sandbox netns — only the router's veth can reach it, §8) and
#    must expose /busy for the three-condition idle check (§7).
exec opencode serve --hostname 0.0.0.0 --port 4321
