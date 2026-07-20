# Design: Sovereign AI Developer Workspace Platform

**Status:** Draft v3 — repositioned for EU/regulated sovereignty, local-first adoption, model-agnostic execution
**Scope of v1:** Single-host data plane (Mac Mini or any Linux-capable machine), local or EU-hosted control plane, OpenCode as the bundled agent harness. Multi-host scheduling and Kubernetes driver are future work (§14).

---

## 0. Positioning (drives every technical choice below)

A **governed execution layer for AI coding agents** for organizations — primarily in Europe and regulated industries — that cannot allow source code, prompts, or development telemetry to leave infrastructure they control or fall under foreign jurisdiction.

Three commitments, each traceable to a mechanism in this document:

1. **Sovereignty all the way up** — not just code (data plane, §2) but metadata (control plane modes, §3) and model inference (sovereign model routing, §9).
2. **Local-first** — the full platform runs on one machine the developer owns (a Mac Mini is the reference target), offline except for model inference, and *promotes* to team/enterprise deployment without migration (§4).
3. **Model-agnostic by construction** — the bundled agent harness is OpenCode (MIT-licensed, provider-neutral); the platform never assumes a specific model vendor, and routing to EU or self-hosted models is a first-class feature, not a fallback (§9).
4. **Centrally curated agent capabilities** — the MCP servers, skills, and context every agent instance starts with are org-curated, versioned, and enforced below the agent's reach, portable across all instances (§10).

### Non-goals (v1)
- Sub-second resume (gVisor checkpoint/restore is the future path; §14).
- Kubernetes driver, multi-host scheduling, cross-host migration.
- Competing with SaaS sandbox APIs on cold-start benchmarks or $/hour.
- Task-level UI for non-technical users (the "Governed User" surface, GTM.md §3) — second ring; v1 surfaces are terminal, diff viewer, and browser session.

### Non-functional targets
| Property | Target |
|---|---|
| Workspace resume latency (warm host) | p50 ≤ 3s, p95 ≤ 10s |
| Workspace data durability | Snapshot RPO ≤ 15 min; restore RTO ≤ 30 min (with off-host replication configured) |
| Session survival during control-plane outage | Active sessions unaffected; no new provisions |
| Control↔data protocol compatibility | Data plane N-2 versions supported |
| Sandbox egress | Default-deny; allowlist only |
| Offline operation (local mode) | Everything except model inference API |

---

## 1. The Sovereignty Contract

The core sales and compliance artifact: for each data class, the transit path, the operator, **and the governing jurisdiction**. This table ships as a signed, versioned document with every release.

| Data class | Transits | Operated by | Jurisdiction |
|---|---|---|---|
| Source code, diffs, terminal I/O | Browser ↔ Data Plane Router, directly | **Customer** | Customer's |
| Prompts + code context to models | Sandbox → Policy Gateway (data plane) → model endpoint | **Customer** (gateway); model provider (endpoint) | **Customer-chosen per model tier (§9)** — from customer-jurisdiction (self-hosted weights) to EU (Mistral et al.) to US-provider-EU-region |
| Audit ledger (prompts, tool calls, diffs, egress) | Data plane → customer log vault | **Customer** | Customer's |
| Workspace metadata (names, sizes, lifecycle) | Data plane ↔ Control plane | Depends on control-plane mode (§3) | **Local/self-hosted: customer's. SaaS: EU entity, EU region, EU subprocessors only** |
| Identity (SSO assertions, RBAC) | Browser ↔ Control plane | Same as above | Same as above |

**Stated honestly:** when a hosted model is used, code context reaches that provider — governed by the customer's agreement with the provider and the gateway's policy. Customers for whom no external provider is acceptable route to self-hosted weights (§9, tier C); the platform is built so that this is a configuration change, not an architecture change.

**Control-plane outage behavior:** active sessions continue; new provisions and logins fail. In local mode this is moot (control plane is on the same machine).

---

## 2. System Topology

```
   CONTROL PLANE — one of three modes (§3):
   [Local: same binary]  [EU SaaS]  [Self-hosted]
                  ▲
                  │ outbound-only gRPC from data plane:
                  │ WatchDesiredState / ReportActualState (metadata only)
   ┌──────────────┼──────────────────────────────────────────────┐
   │ CUSTOMER     │              THE DATA PLANE IMAGE (§7)       │
   │ BOUNDARY     │      one Linux system image, everywhere      │
   │        ┌─────┴──────────────────────┐                       │
   │ Browser│  Router (Go): reconciler · │                       │
   │  ──────►  proxy · lease mgr · audit │                       │
   │ direct │  shipper · egress proxy    │                       │
   │        └───┬────────────────┬───────┘                       │
   │            │                │                               │
   │   ┌────────▼──────┐  ┌──────▼───────────────┐               │
   │   │ Sandboxes     │  │ Workspace volumes    │               │
   │   │ (gVisor)      │  │ (ZFS datasets)       │               │
   │   └───────┬───────┘  └──────────────────────┘               │
   │           │ egress via allowlist proxy only                 │
   │   ┌───────▼─────────┐     ┌───────────────────────────────┐ │
   │   │ Policy Gateway  │────►│ Model endpoint per tier (§9): │ │
   │   │ (sovereign model│     │ A: US frontier via EU region  │ │
   │   │  routing)       │     │ B: EU provider (Mistral, …)   │ │
   │   └─────────────────┘     │ C: self-hosted weights (vLLM) │ │
   │                           └───────────────────────────────┘ │
   └──────────────────────────────────────────────────────────────┘
```

---

## 3. Control Plane: Three Modes, One Protocol

The reconciliation protocol (§5) is identical in all modes; the modes differ only in where the control plane runs and who operates it. This is what makes local → team promotion a re-point, not a migration.

- **Local mode (the developer path, free):** control plane and data plane run in one process tree on one machine. SQLite instead of Postgres. No account required; works offline except model calls. This is the Mac Mini experience: install, point at a repo, run agents tonight.
- **EU SaaS mode (the team path):** control plane operated by us — EU legal entity, EU region, EU subprocessors only, no US-jurisdiction dependencies in the metadata path. For sovereignty buyers, "it's only metadata" is not an excuse: workspace names, timestamps, and org structure are sensitive, so the metadata plane must itself be jurisdictionally clean.
- **Self-hosted mode (the regulated/air-gap path):** the control plane is one container + Postgres. Deliberately small — the reconciliation design (§5) keeps the control plane stateless-ish and thin precisely so that self-hosting it is an evening's work, not a platform adoption. Air-gapped deployments combine this with model tier C (§9) for zero external connectivity.

**Promotion path:** a local data plane joins a team control plane by re-registering (`router join <control-plane-url>`); volumes, leases, audit ledger, and policy files are unchanged. Governance is *silently on* in local mode (ledger written locally, egress default-deny with a developer-friendly allowlist), so promotion flips where logs ship and who holds policy — it does not bolt on a different system.

---

## 4. Local-First Product Mechanics

- **All-in-one binary** for the single-machine mode; the only external dependency is the model endpoint chosen in §9.
- **Governance always on, never in the way:** the audit ledger and egress proxy run identically in local mode. The solo developer never fills in a compliance form; the enterprise gets a fleet of pre-wired seats when those developers' machines join the org control plane.
- **Hardware floor:** one Mac-Mini-class machine (Apple Silicon, 16 GB) runs the control plane, router, and a working set of ~5–10 concurrent agent sandboxes (§7 notes the VM memory floor on macOS). The demo that sells pillar 2 of the strategy — twenty agents attacking one bug on one desk-side box — is a first-class test case, not a stunt.

### The endpoint-free enterprise posture ("no agents on endpoints")
Local-first (above) is the *individual* adoption path on machines the developer owns. The *enterprise* deployment posture is its complement, and the lead sales narrative (GTM.md §2): agents execute only on platform hosts; user access is a browser session (§6); repositories live on platform volumes and are **never cloned to user endpoints**. Consequences the architecture already delivers without new mechanism:
- Endpoint loss/compromise/offboarding never exposes code — it was never there.
- All agentic AI egress passes one governed point (router + gateway), so compliance scoping is one ledger, not a laptop fleet.
- Shadow-AI consolidation: unsanctioned local agent installs are replaced by a sanctioned, *better* destination (fan-out, curated context, models individuals can't access) rather than a ban.

Enforcement of the endpoint side is deliberately **not built here** — the customer's existing MDM/egress controls block local agent binaries and provider APIs on laptops; we ship the blocklist guidance and are the destination. Two scope guardrails, held in the design as in the pitch (traps detailed in GTM.md §2): this posture centralizes **agentic execution** — AI that runs code, uses tools, touches repos — not chat or general AI portals; and it removes *agents* from endpoints, not coding — local IDEs and human editing workflows are untouched, with the platform holding the agent-side workloads. A centralization mandate is only recommendable once the platform is demonstrably better than the laptop (resume SLO, fan-out), not merely compliant.

---

## 5. Control ↔ Data Plane Protocol: Declarative Reconciliation

Control plane stores desired state; data plane converges and reports actual state (the Kubernetes pattern, minus Kubernetes).

- Data plane opens a persistent gRPC stream: `WatchDesiredState(host_id, protocol_version)`. Control plane streams the desired-state set, then deltas; each object carries a monotonic `generation`.
- Reconcile loop: diff desired vs. actual (rebuilt from containerd labels + on-disk lease records, so it survives router restarts), converge, `ReportActualState`.
- **Idempotent by construction:** "ensure workspace X at generation N" is safe to apply repeatedly; at-least-once delivery is harmless; ordering reduces to generation comparison.
- **Partition behavior:** control plane unreachable → existing sessions continue, idle policy enforced from cached desired state, no new provisions. Host rebooted → router rebuilds and reports actual state; the control plane reconciles to reality rather than trusting its cache. Host silent >15 min → marked `Unknown`; no rescheduling in v1 (volumes are host-local).
- **Versioning:** protocol carries `protocol_version`; control plane supports N-2. Router upgrades are customer-initiated (their hardware) with an advisory channel; a `security_mandatory` floor lets the control plane refuse *new* workspaces on outdated routers without touching running sessions.

### Control-plane data model (Postgres; SQLite in local mode)

```
hosts(id, org_id, protocol_version, last_heartbeat, status)
workspaces(id, org_id, host_id, name, desired_state{Running|Stopped|Deleted},
           generation, idle_policy, created_at)
workspace_status(workspace_id, actual_state, resume_latency_ms,
                 volume_bytes, reported_at)            -- written only by reconcile reports
volumes(id, workspace_id, host_id, dataset_name, quota_bytes,
        lease_holder, lease_generation, last_snapshot_at)
model_routes(org_id, tier{A|B|C}, endpoint_url, allowed_models[], policy_ref)
agent_profiles(id, org_id, layer{platform|org|team|workspace}, generation,
               bundle_hash, overridable_flags, created_at)   -- capability profiles, §10
audit_meta(workspace_id, event_count, last_shipped_at)  -- counts only, never content
```
`workspaces` additionally carries `profile_ref` — the resolved capability-profile bundle (§10) at its pinned generation.

Point lookups and per-org scans only; desired vs. actual in separate tables written by separate actors. Migrations: additive within a release, expand/migrate/contract across releases.

---

## 6. Interactive Traffic Path

Code, terminal output, and diffs flow **browser ↔ router, directly** — never through the control plane, never through a third-party TLS-terminating proxy in the sovereign modes.

1. Browser authenticates to the control plane, requests workspace X.
2. Control plane returns a short-lived signed session token (JWT, ≤15 min) + router address.
3. Browser connects directly to the router (WebSocket); router validates the token offline against cached control-plane public keys — sessions survive control-plane outages until refresh-token expiry (org-configurable).
4. Router proxies to the workspace's OpenCode server.

Ingress modes, chosen per deployment and **recorded as a compliance decision**:
- **Local mode:** localhost / LAN / the customer's VPN (Tailscale-style mesh on their own tailnet is compatible — the mesh sees ciphertext only with their keys, but even this is documented as a decision).
- **Direct mode (default for teams):** customer's own DNS + TLS + load balancer or VPN. No third party. The setup burden is the price of the guarantee; we document it, we don't hide it.
- **Tunnel mode (explicit opt-out of the guarantee):** a CDN tunnel (e.g., Cloudflare) terminates TLS and can observe traffic, and is typically US-jurisdiction. Available for convenience deployments; **disqualified by default in the sovereign profile** and requires an org-admin override that is itself an audited event.

---

## 7. The Data Plane Image: One Linux System, Any Hardware

Portability is a property of the design, not a promise: the data plane ships as **a single versioned Linux system image** — router + containerd + gVisor (runsc) + ZFS — that runs identically:

- **On a Mac (the beachhead):** inside one lightweight Linux VM via Virtualization.framework. The VM is an *additional outer boundary* (macOS host → VM → gVisor → sandbox), not an apology. Costs stated plainly: a fixed VM memory floor (~2–4 GB) and resume latency toward the top of the SLO band.
- **On bare-metal Linux or any KVM/VMware host:** the same image, no VM wrapper needed. This covers Hetzner/OVH/Scaleway boxes, on-prem racks, and EU cloud instances — the "any hardware" requirement reduces to "can run one Linux image."

Because the image is byte-identical everywhere, the isolation stack, branching mechanism, audit pipeline, and egress policy never fork per platform — which is also what makes the compliance story auditable once instead of per-environment.

### Workspace lifecycle (mechanics unchanged from v2, restated briefly)
- **Storage:** one ZFS dataset per workspace → quotas, instant snapshots, CoW clones, `zfs send` replication from one mechanism. Snapshot every 15 min while active; nightly send to customer-designated storage. Single-host without replication shows a persistent warning: host loss = data loss, knowingly accepted.
- **Volume fencing:** generation-numbered lease recorded in the control plane *and* on the dataset; mount requires matching generation; prior holder force-stopped before a new grant. Two writers on one volume is impossible by construction.
- **Idle & scale-to-zero:** idle = no WebSocket activity **and** no sandbox CPU above floor **and** agent reports not-busy (`/busy` endpoint), for the org-configured window. Stop = SIGTERM → agent checkpoint hook → 30s grace → SIGKILL → snapshot → unmount → lease release. Contract: everything under `/workspace` survives; processes don't.
- **Resume:** warm image cache; create container + mount dataset + start OpenCode. p50 ≤ 3s / p95 ≤ 10s.

### Agent branching (git-level, ZFS-assisted)
Each agent branch = ZFS clone of the latest workspace snapshot (instant, CoW) + its own sandbox + lease; inside the clone the agent works on a **git branch**, and merge-back is a normal PR. Independent `.git` per clone — no shared-base staleness, no overlay corruption class. Abandoned clones reaped after TTL with UI warning. This is the mechanical basis of the "fan out 20 agents on one box" motion.

---

## 8. Sandbox Isolation & Security

**Framing:** defense in depth with a bounded blast radius — never "escape is impossible."

**Layers (identical everywhere, per §7):** gVisor user-space kernel (systrap) → user-namespace remap (sandbox root = unprivileged high host UID) → all capabilities dropped, `no_new_privs` → seccomp on the Sentry itself → read-only rootfs, only `/workspace` writable → cgroup CPU/mem/pids limits → ZFS quota. On macOS hosts, the Virtualization.framework VM adds a hardware-virtualization outer boundary.

**Egress control (the likely real incident is exfiltration or prompt-injection steering, not kernel escape):**
- Per-sandbox network namespace; no route to internet, other sandboxes, host, or metadata endpoints.
- Sole egress: the router's allowlist proxy — org git remotes, approved package registries, the policy gateway. Every request logged with workspace attribution into the audit ledger.
- Sandboxes hold **no model API keys**; the model path exists only through the policy gateway.

**Router ↔ sandbox trust:** OpenCode's port binds inside the sandbox netns; only the router holds a veth in. Per-session tokens on router→OpenCode requests. No docker socket, host paths, or device nodes ever mounted.

**Blast radius:** one sandbox compromised → its own clone + its allowlist, nothing else. One host lost → that host's workspaces to last snapshot. Control plane lost → no new work, no lost work.

---

## 9. Sovereign Model Routing & the Model-Agnostic Harness

This section is why OpenCode is the bundled agent, and it is a headline feature, not a caveat.

### Why OpenCode
- **MIT-licensed and provider-neutral** — the harness imposes no model vendor, which is the precondition for every routing tier below. A platform bundling a vendor's agent (Codex, Claude Code, Copilot) inherits that vendor's jurisdiction and roadmap; bundling an open harness keeps the platform Switzerland.
- The platform treats OpenCode as the *default*, not the *only*, harness: the sandbox contract (workspace mount, `/busy` endpoint, gateway-only egress, session-token auth) is documented so other agent CLIs can run under the same flight recorder and egress policy. Uniform governance across heterogeneous agents is the product (see COMPETITIVE.md pillar 1).

### The three routing tiers (per-org policy in `model_routes`, enforced by the gateway)
| Tier | Endpoint | Jurisdiction posture | Trade-off |
|---|---|---|---|
| **A** | US frontier models via EU-region enterprise channels (Bedrock/Vertex EU, data-residency terms) | Pragmatic: EU residency, US provider | Best capability; provider agreement governs |
| **B** | EU-jurisdiction providers — **Mistral** (Codestral/Devstral line) and peers | EU entity, EU infra, outside CLOUD Act reach | Near-frontier coding capability, clean jurisdiction |
| **C** | Self-hosted open weights (vLLM/Ollama on customer GPUs — Devstral, Codestral open releases, Apertus, etc.) | Fully customer-controlled; air-gap compatible | Capability gap vs. frontier; zero external transit |

The gateway owns the upstream URL, so moving between tiers is configuration. Per-org policy can also *split* routing: sensitive repos pinned to tier C, general work on tier A/B — policy is per-workspace, not per-installation.

### Making weaker models usable (the engineering behind tier C)
Model-agnosticism is only real if agents remain productive on non-frontier models. Platform-level mitigations, **shipped as capability-profile content (§10) that configures the harness — never as gateway middleware that rewrites traffic in flight**: strict tool-schema validation with automatic repair-and-retry on malformed tool calls; trimmed tool surface for smaller models; prompt templates per model family shipped as tested profiles ("Devstral 2 profile", "Apertus profile"); and capability-tiered task routing (let orgs send review/test-writing to tier C and architecture work to tier A/B). Keeping this out of the gateway preserves audit integrity — the ledger records what was actually sent — and keeps in-flight behavior debuggable. We benchmark and publish per-profile task success rates; honesty about the capability trade-off is part of the sovereignty brand.

### Gateway scope: a thin policy enforcement point, not an LLM gateway (build-vs-buy, decided)
The gateway bundles two superficially similar jobs with opposite build/buy answers.

**We build the policy enforcement point, because we cannot buy it.** Four load-bearing design promises exist only if there is a chokepoint we control between agent and model: sandboxes hold no API keys (§8), per-repo tier routing (this section), audited-never-silent tier fallback (§12), and flight-recorder completeness (§11). The hosted gateway products (Portkey, Cloudflare AI Gateway, OpenRouter) would see every prompt from mostly US jurisdiction — recreating the exact problem this platform exists to remove — and Coder sells its closed-source equivalent as a paid add-on, which makes ours being open and inspectable a procurement weapon (COMPETITIVE.md §1). A differentiator cannot be rented.

**We do not build a general LLM gateway.** Protocol translation across providers, retry/failover UX, caching, cost dashboards, and prompt management are a commodity treadmill served by LiteLLM, Envoy AI Gateway, Kong, and a dozen funded startups — and none of it is why anyone buys this platform. Critically, **OpenCode already does provider abstraction natively**; building translation in the gateway would implement the same layer twice and double our exposure to provider API churn.

**What the thin version is:** a forward proxy the sandbox is already forced through (sole egress path), which terminates the sandbox-side connection (plaintext inside the boundary — OpenCode's base URL points at the gateway, so no TLS interception tricks), injects custody-held keys upstream over TLS, consults `model_routes` per workspace, captures request/response streams into the audit ledger, and applies the policy blocks below. The one unavoidable piece of protocol awareness is parsing streamed responses well enough to log token counts and tool calls — scoped to **two wire formats: OpenAI-compatible and Anthropic**, which covers effectively the entire market (vLLM, Ollama, Mistral, and most tier-C serving stacks are OpenAI-compatible). A bounded, slow-moving surface — not the treadmill.

**Explicit non-goals:** response caching, cost-analytics dashboards, prompt management, A/B routing UX, and any in-flight payload rewriting. **Chaining, not competing:** enterprises arriving with a mandated LiteLLM/Kong estate point our gateway's upstream at theirs — we keep policy, key custody, and capture; they keep their routing/caching investment. This converts "we already have a gateway" from objection to integration checkbox.

### Gateway policy functions (unchanged in kind from v2, restated)
Reliable: model/provider allowlists, org policy injection, full request/response capture to the audit ledger, size and file-pattern blocks (`*.pem`, `secrets/**`). Best-effort and labeled as such: pattern-based secret tripwires on outbound prompts — the real controls are tier choice and the provider agreement, not regex.

---

## 10. Agent Capability Profiles: MCP Servers, Skills & Curated Context

Governance has three legs: controlling where models run (§9), recording what agents did (§11), and — this section — controlling **what agents can do and know before they act**. A *capability profile* is a versioned, org-curated bundle defining the default toolset and context of every OpenCode instance (and any harness honoring the sandbox contract). Curate once, ported to all instances.

### What a profile contains
1. **MCP server set.** An explicit allowlist of MCP servers, each pinned by version *and content hash*, with per-server config. Two classes, both governed:
   - *Local MCP servers* run inside the sandbox — third-party code inside the boundary, so they are subject to org review before entering the profile, launched from the read-only bundle (not fetched at runtime), and constrained by the same sandbox isolation and egress rules as the agent itself.
   - *Remote MCP servers* are reachable only if their endpoint is derived onto the egress allowlist from the profile; traffic transits the router proxy and lands in the audit ledger like any other egress.
2. **Skills.** Curated skill/command definitions mounted **read-only** at a well-known path where the harness discovers them. Read-only matters: neither the agent nor a prompt-injected instruction can modify the skills it runs under.
3. **Context packs.** Org-curated AGENTS.md-style context — coding conventions, architecture overviews, security and compliance rules ("never write PII to logs", "GPL dependencies require approval") — mounted read-only and referenced from the harness config. This is the "general context, curated and ported everywhere" requirement made mechanical: one edit in the org profile reaches every instance at its next boot.

### Layering and override policy
Profiles compose in layers: **platform defaults → org → team → workspace/user**. Each layer declares what lower layers may do per item class: `locked` (org pins the MCP allowlist; nobody adds servers), `extend` (teams may add context packs), or `open` (users may add personal skills — typically only in local mode). The merge is deterministic, and the *resolved* profile — not the layers — is what gets pinned and recorded.

### Distribution and enforcement (below the agent's reach)
- A resolved profile is a **content-addressed bundle**; the workspace object in desired state (§5) carries `profile_ref` + generation. The router fetches and caches the bundle, verifies its hash, and mounts it read-only into the sandbox at boot.
- **Enforcement is structural, not configuration:** read-only mounts, an egress allowlist computed from the profile, and no write path from the sandbox to harness or router config. An agent cannot grant itself a tool, an MCP server, or a context change — the profile is decided outside the sandbox and enforced by the router. This is the difference between policy and a suggestion.
- Profile updates create a new generation; running sandboxes adopt it at next boot, or immediately (forced restart) when flagged `security_mandatory` — e.g., pulling a compromised MCP server fleet-wide in one action.
- **Local mode:** identical mechanism, profile authored locally. Joining a team control plane merges the local layer under the org layers per the override policy — promotion, not migration, as everywhere else (§3).

### Audit tie-in
Every flight-recorder event (§11) records the profile generation in force, so an audit answers not just "what did the agent do" but "under which toolset, which MCP servers, and which instructions was it operating" — the shape of question both incident forensics and EU AI Act oversight actually ask.

---

## 11. Audit Ledger ("Flight Recorder")

Captured at router + gateway: prompts, tool invocations, executed commands, file-write events, egress requests — one hash-chained (tamper-evident) ledger per workspace, **uniform across any harness run in the sandbox**, shipped to the customer's SIEM/S3. Control plane receives event counts only. Local mode writes the same ledger to local disk.

**Vault-unreachable failure mode is an org policy, chosen on purpose:** bounded local buffer (default 1 GiB / 24 h), then `fail-closed` (suspend new agent actions until logs drain — the regulated-profile default) or `fail-open` (continue, drop oldest, persistent alert).

This ledger maps directly onto EU AI Act logging/oversight obligations for deployers of AI systems — the compliance filing is a product output, not a consulting engagement.

---

## 12. Reliability & Failure Modes

| Failure | Behavior |
|---|---|
| Control plane down | Sessions continue (offline token validation); no new provisions/logins; idle policy from cache. Local mode: N/A. |
| Router crash | systemd/launchd restart; sandboxes untouched; state rebuilt from containerd labels + lease files; clients reconnect with jittered backoff. |
| Host reboot | Sandboxes gone, datasets intact; workspaces report `Stopped`; resume on open. Work since last checkpoint lost, within the stated contract. |
| Log vault down | Bounded buffer → org-chosen fail-open/closed (§11). |
| Model endpoint down/slow | Gateway: timeout, exponential backoff + jitter, per-upstream circuit breaker; optional org-configured fallback tier (an audited policy decision — silent fallback from tier C to tier A would be a sovereignty violation). |
| Thundering herd | Per-host provision concurrency cap (default 4), FIFO queue with UI feedback; reconciliation means control-plane restart replays no command storm. |
| Host disk loss | Restore from replicated snapshots: RPO ≤ 15 min active / ≤ 24 h idle; RTO ≤ 30 min. Without replication: acknowledged single-host risk (§7). |

---

## 13. Observability & Rollout

- **Traces** span control plane → gRPC → reconciler → containerd/ZFS per lifecycle operation; slow resume decomposes into lease-wait vs. image-pull vs. mount vs. agent-start.
- **Metrics:** resume-latency histogram, provision failure by class, reconcile lag (desired minus acked generation — the protocol health signal), audit buffer depth, snapshot age, egress denials, per-tier model latency/error rates.
- **Logs stay in the customer boundary;** control plane gets metrics and error-class counters only.
- **Rollout:** control plane blue-green behind flags — rollback just re-serves older desired state, and data planes converge to it. Data plane image is versioned and atomic (image swap + router restart; sandboxes untouched); customer-controlled timing per §5.

---

## 14. Alternatives Considered & Revisit Triggers

**Firecracker/Kata microVMs instead of gVisor** (E2B/Fly approach; Modal runs gVisor). Hardware-virtualization boundary and full syscall compatibility. Rejected for v1: no-KVM portability matters more in this positioning than before — the data plane image must run inside a macOS VM and on arbitrary EU hosting where nested virt is unavailable, which is gVisor-friendly and Firecracker-hostile. Fast resume does not force the switch: gVisor checkpoint/restore (Modal's approach) is the cheaper path. **Revisit when:** dev tooling hits gVisor syscall gaps (ptrace-heavy debuggers, io_uring) or a flagship customer requires a hardware boundary on Linux hosts.

**Build on Coder's open-source core instead of a bespoke data plane.** Coder's core is AGPL-3.0, but the governance surface this product *is* — audit logging, RBAC, Agent Firewall, AI Gateway — sits in Coder's proprietary premium tier, so the open core cannot be assembled into a sovereign governance product, and AGPL obligations would bind our distribution. Building on it buys the part we don't need (human workspace management) and withholds the part we do. **Revisit if** Coder open-sources its governance tier (see COMPETITIVE.md watch items).

**Plain long-lived VM per developer.** Right answer for a team running two agents; wrong for fleets of parallel short-lived agent branches, and it has no governance story. We say so honestly in the docs.

**Licensing implication for us (strategy, recorded here deliberately):** our open-core line is drawn *opposite* to Coder's — the governance/audit/egress code is **open source** (a sovereignty buyer must be able to inspect the thing that watches their agents; closed-source audit tooling is a contradiction in this market), while the EU SaaS control plane, fleet/multi-org management, and support are the commercial layer.

## 15. Driver Interface (scoped honestly)

The router provisions through a `Runtime` interface (`EnsureWorkspace`, `StopWorkspace`, `Status`). v1 ships exactly one implementation: containerd + runsc + ZFS inside the data plane image. Kubernetes is future work and is not "the same code with a different flag" — PVCs vs. datasets, RuntimeClass, ingress, and lease semantics all differ. The interface is the seam; the K8s driver is a project.

---

## Appendix A: v2 → v3 Change Map

| Change | Where |
|---|---|
| EU/regulated sovereignty as primary positioning; jurisdiction column in the contract | §0, §1 |
| Control plane: three modes (local / EU SaaS / self-hosted); "metadata is also sensitive" | §3 |
| Local-first: all-in-one binary, governance silently on, promotion-not-migration | §3, §4 |
| macOS flip: data plane = one Linux system image; Mac VM is an outer boundary, not a caveat | §7 |
| Sovereign model routing: three tiers, per-workspace policy, weak-model usability engineering | §9 |
| OpenCode rationale + open sandbox contract for other harnesses | §9 |
| Tunnel mode disqualified by default in sovereign profile; override is audited | §6 |
| **Agent capability profiles: curated MCP allowlist, read-only skills, portable context packs, layered override policy, enforced below the agent** | §10 |
| Audit ledger mapped to EU AI Act obligations; hash-chained; records profile generation | §11 |
| Gateway scoped as thin policy enforcement point: build the PEP, don't build an LLM gateway; two wire formats; chaining to existing gateways; weak-model mitigations moved to profiles | §9 |
| Endpoint-free enterprise posture ("no agents on endpoints"): code never on endpoints, single governed egress, MDM as enforcement arm, scope guardrails against portal/VDI traps | §4 |
| Open-core line: governance code open, control plane commercial (inverse of Coder) | §14 |
| Firecracker rejection strengthened by no-KVM portability requirement | §14 |

## Appendix B: Original Review Findings → Resolution Map (from v2, still current)

| Review finding | Resolution |
|---|---|
| gVisor named on macOS host | §7: single Linux image; VM outer boundary on macOS |
| Data path contradicted sovereignty | §1 contract; §6 direct-by-default, tunnel audited opt-out |
| Control↔data protocol unspecified | §5: declarative reconciliation, idempotent, N-2 versioned |
| No volume fencing | §7: generation-numbered leases, on-disk + control plane |
| "Durable" storage undesigned | §7: ZFS, RPO/RTO numbers, replication, single-host warning |
| Block-CoW branching unsound | §7: ZFS clone + git branch |
| No egress policy | §8: default-deny netns, allowlist proxy, logged |
| "Escape structurally impossible" overclaim | §8: defense-in-depth, blast-radius statement |
| Scale-to-zero ignored agent workloads | §7: three-condition idle, graceful drain, honest SLO |
| Data plane SPOF / fleet story | §12; §5 versioning + customer-controlled upgrades |
| Audit/DLP failure modes | §11: bounded buffer, org-chosen fail-open/closed |
| Sandbox↔router trust undefined | §8: netns isolation, per-session tokens |
| Docker↔K8s driver overclaim | §15: one driver in v1 |
| No observability design | §13 |
| No data model | §5 |
| No alternatives analysis | §14 with revisit triggers |
