# syntax=docker/dockerfile:1.7
#
# opencode-sandbox — the sandbox image every agent workspace runs (DESIGN.md §7/§8).
#
# Contract with the data plane (enforced by the router, not by this image):
#   - Runs read-only (--read-only); the ONLY writable mounts are:
#       /workspace          rw   ZFS dataset/clone (bind mount)
#       /tmp                rw   tmpfs, size-capped
#   - /etc/agent-profile   ro   capability-profile bundle (§10): MCP allowlist,
#                               skills, context packs, opencode config
#   - No network except the router's veth; all egress via allowlist proxy.
#   - No secrets, no API keys, no tokens baked in — model access goes through
#     the policy gateway (OPENCODE_BASE_URL is injected by the router).
#   - Runs as uid 1000 in-container; host sees a remapped unprivileged UID.
#
# Build multi-arch (Mac Mini VM is arm64, most EU hosting is amd64):
#   docker buildx build --platform linux/arm64,linux/amd64 -t opencode-sandbox:v1 .
#
# Supply chain: pin the base by digest before first release, generate an SBOM,
# and sign with cosign — the image is part of the audited surface a sovereignty
# customer reviews. (Digest omitted here only because this is the v1 sketch.)

# --- termbridge: the WebSocket↔PTY bridge behind the web terminal (§7 —
# terminal input is an idle-check activity signal; see termbridge/main.go).
# Built in its own stage so the sandbox image never carries a Go module cache.
FROM golang:1.23-bookworm AS termbridge-builder
WORKDIR /src
COPY termbridge/go.mod termbridge/go.sum ./
RUN go mod download
COPY termbridge/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/termbridge .

FROM debian:bookworm-slim AS base

ARG NODE_VERSION=22.12.0
ARG GO_VERSION=1.23.4
ARG OPENCODE_VERSION=1.17.18
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

# --- System toolchain: what agents actually shell out to. Keep this list
# deliberate — every binary here is attack surface inside the sandbox.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
      openssh-client \
      curl \
      build-essential \
      pkg-config \
      python3 \
      python3-venv \
      python3-pip \
      ripgrep \
      jq \
      unzip \
      xz-utils \
    && rm -rf /var/lib/apt/lists/*

# --- Node.js (official tarball; TARGETARCH mapped to Node's naming)
RUN arch="$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64)" \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
    | tar -xJ -C /usr/local --strip-components=1

# --- Go toolchain
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    | tar -xz -C /usr/local
ENV PATH="/usr/local/go/bin:${PATH}"

# --- OpenCode: the agent harness (MIT, model-agnostic — DESIGN §9)
RUN npm install -g "opencode-ai@${OPENCODE_VERSION}" \
 && npm cache clean --force

# --- Unprivileged runtime user. uid 1000 in-container; the host additionally
# remaps via userns so even in-container root would be nobody on the host (§8).
RUN useradd --create-home --uid 1000 --shell /bin/bash agent

# --- All mutable state lives on /workspace so it (a) survives scale-to-zero,
# and (b) is FORKED WITH THE CLONE on agent branching — a branch inherits warm
# npm/pip/go caches from its parent, which is half of why fan-out is fast (§7).
ENV HOME=/workspace/.home \
    XDG_CACHE_HOME=/workspace/.cache \
    XDG_CONFIG_HOME=/workspace/.config \
    XDG_DATA_HOME=/workspace/.local/share \
    npm_config_cache=/workspace/.cache/npm \
    PIP_CACHE_DIR=/workspace/.cache/pip \
    GOPATH=/workspace/.cache/go \
    GOMODCACHE=/workspace/.cache/go/pkg/mod \
    TMPDIR=/tmp

# Injected/overridden by the router at ContainerCreate — placeholders only:
#   OPENCODE_BASE_URL   → policy gateway (never a provider URL, never a key)
#   OPENCODE_CONFIG     → /etc/agent-profile/opencode.json (from the profile bundle)
ENV OPENCODE_CONFIG=/etc/agent-profile/opencode.json

COPY --from=termbridge-builder /out/termbridge /usr/local/bin/termbridge
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0555 /usr/local/bin/entrypoint.sh /usr/local/bin/termbridge

LABEL org.opencontainers.image.title="opencode-sandbox" \
      org.opencontainers.image.description="Read-only agent sandbox: OpenCode + Node/Python/Go toolchains. All state on /workspace." \
      org.opencontainers.image.source="https://example.eu/platform/sandbox-image" \
      org.opencontainers.image.licenses="Apache-2.0"

USER agent
WORKDIR /workspace
EXPOSE 4321 4322

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
