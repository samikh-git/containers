# Competitive Analysis v2 — EU Sovereignty & Local-First Angle

**Date:** July 2026 · Companion to [DESIGN.md](DESIGN.md) · Supersedes v1
**Positioning under analysis:** governed, model-agnostic execution layer for AI coding agents, for European/regulated buyers who cannot depend on foreign-jurisdiction clouds; local-first adoption starting on developer-owned hardware.

---

## 1. The Coder open-source question — resolved

This was the open research item, and the answer is favorable. Verified against coder.com/pricing and the coder/coder repository (July 2026):

**Licensing structure:** `coder/coder` is **AGPL-3.0 at the core, with a second proprietary license (`LICENSE.enterprise`) covering the premium code paths in the same repository.** It is an open-core company, and the line they drew matters enormously for us.

**What's free (Community):** unlimited workspaces, templates, and members in a *single organization*; OIDC SSO; web UI/CLI/API; basic ability to assign tasks to AI agents; **one** platform integration and **one** external auth integration; community support.

**What's paywalled (Premium, proprietary):**
- **Audit logging** — the entire compliance trail
- **RBAC** (custom roles, group/role access, OIDC group sync)
- Multi-organization access controls
- High availability (multiple server replicas) and workspace proxies
- Resource quotas per org/user
- **Agent Firewall** (add-on) — their egress-control answer
- **AI Gateway** (add-on) — their model-routing/governance answer

### Three strategic consequences

1. **Coder's open source cannot be assembled into a sovereignty product.** The exact features a regulated European buyer needs — audit trail, RBAC, egress firewall, model gateway — are proprietary, paid, and closed to inspection. A sovereignty-driven buyer using Coder must trust *closed-source US-company code* as the thing that watches their agents. That is a contradiction we can attack directly.
2. **Their premium add-ons validate our pillars and signal convergence.** "Agent Firewall" and "AI Gateway" are Coder building our §8 egress proxy and §9 policy gateway. They see the same future. The race is real; our edge is *where the open line sits* and *jurisdiction*, not the feature list.
3. **We cannot build on their core, and shouldn't.** AGPL would bind our distribution, and the open part (human workspace management via Terraform templates) is the part we don't need — the governance part we do need isn't open.

### Our counter-positioning (recorded in DESIGN §14)
Draw the open-core line **inverse to Coder's**: our governance/audit/egress code is **open source** — auditors and security reviewers can read the flight recorder they're being asked to trust — while the EU SaaS control plane, fleet/multi-org management, and support are commercial. "The code that watches your agents is open; theirs is not" is a one-sentence competitive weapon in every European procurement conversation.

---

## 2. The field, re-scored through the sovereignty lens

Scoring criteria this buyer actually applies: (J) jurisdiction of vendor and data path · (G) governance depth for *agents specifically* · (M) model-agnosticism · (L) local/self-host footprint.

### Coder (US) — primary competitor
- **J:** US company, CLOUD Act reach — a procurement flag for the strictest EU buyers even when self-hosted. **G:** paywalled + closed (see §1). **M:** genuinely good — Coder Agents is model-agnostic incl. self-hosted models; credit where due. **L:** heavy — coderd + Postgres + Terraform; no meaningful solo-developer path.
- **Verdict:** strong in US/global enterprise; beatable in EU-regulated on jurisdiction, open governance, and adoption weight. Their AGPL core also can't be quietly relicensed away — but the premium tier can grow, so watch it.

### Mistral Code / Mistral coding stack (FR) — the important new entrant in this frame
- Codestral/Devstral models + Mistral Code assistant; on-prem/air-gapped deployment GA'd; SSO, audit logging, no mandatory telemetry. EU jurisdiction, real sovereignty story.
- **But:** it's a *vertical* play — Mistral models, Mistral assistant. It is not an agent-swarm orchestration runtime, not agent-agnostic, and not model-agnostic (that's the point of it).
- **Verdict: partner more than competitor.** Mistral is our routing tier B and a co-sell story ("run Devstral under our governance layer, fully in-country"). The risk to watch is Mistral moving up-stack into orchestration/execution infrastructure — they have the brand and the capital. A first-class, benchmarked "Mistral profile" (DESIGN §9) makes us complementary before they decide to compete.

### Ona → OpenAI (US post-acquisition)
- The June 2026 acquisition removes the German-born neutral vendor and converts it into exactly what the EU sovereignty buyer fears. Every quarter of integration makes our Switzerland position more valuable. No longer a competitor for this segment; now a cautionary tale we can cite.

### Vertical agent runtimes — Copilot (US/MS), Devin (US), Claude Code (US), Codex (US)
- All US jurisdiction; all single-agent captive runtimes; VPC options exist (Copilot self-hosted runners, Devin Enterprise VPC, Claude self-hosted sandboxes) but the vendor's software still operates the loop and each brings its own disjoint audit surface.
- **Verdict:** for our buyer these are the *workload*, not the platform — things a customer might run under our flight recorder (open sandbox contract, DESIGN §9), not alternatives to it. The threat is "good enough" checkbox compliance: a buyer accepting Copilot-on-ARC instead of real governance. Counter: uniform cross-agent audit + jurisdiction, which none of them can offer even in principle.

### Sandbox APIs — E2B, Daytona, Modal, Morph, Fly, Vercel (US)
- SaaS compute for agent *builders*; US jurisdiction; no BYOC governance story; commoditizing on cold-start milliseconds. Structurally unable to say "code never leaves your boundary."
- **Verdict:** different buyer, different product. Irrelevant to the EU-regulated segment unless one pivots to BYOC enterprise (watch item — Morph's VM branching would be the concerning one, as it overlaps our fan-out mechanic).

### EU AI platform players — LightOn, Aleph Alpha (+ Apertus open weights)
- Sovereign *model/platform* providers for regulated EU orgs; none offers an agent execution/governance runtime.
- **Verdict:** partners and routing targets (tier B/C), plus a channel: their regulated customers are precisely our buyer, already qualified.

### The one that doesn't exist yet
There is **no European-jurisdiction, open-governance, agent-agnostic, model-agnostic execution layer with a local-first path.** That intersection is empty. Every axis of it is individually occupied (Coder: self-hosted agents; Mistral: EU models on-prem; E2B: sandboxes; OpenCode/Continue: open harnesses) — the combination is not.

---

## 3. The differentiation stack (revised)

**One sentence:** *The open, European-jurisdiction execution layer that governs what any AI coding agent — running any model, including yours — does with your code, on hardware you own, starting with the machine on your desk.*

Pillars, in priority order:

- **Pillar 0 — Jurisdiction + open governance (the EU wedge, new):** EU-clean metadata plane (or fully local/self-hosted), no US-jurisdiction dependency in any data path, and **open-source governance code** vs. Coder's paywalled closed equivalent. Maps to EU AI Act logging obligations out of the box (DESIGN §11). This is the pillar procurement can't argue with.
- **Pillar 1 — Record *and* constrain (flight recorder + capability profiles):** one tamper-evident ledger across every harness and model tier, shipped to the customer's SIEM — paired with org-curated capability profiles (DESIGN §10) that centrally pin what any agent can reach (hash-pinned MCP allowlist), do (read-only skills), and knows (portable context packs), enforced by the router below the agent's reach. The audit answer becomes "here's what agents did *and* here's proof of what they could and couldn't do" — a materially stronger compliance claim than logging alone, and one instantly-revocable fleet-wide (pull a compromised MCP server in one action). Coder's closest analogs (Agent Firewall, AI Gateway) are paid, closed, and not a cross-harness curation layer.
- **Pillar 2 — Sovereign model routing:** three tiers (US-frontier-EU-region / EU providers / self-hosted weights), per-repo policy, benchmarked model profiles so tier C is usable, OpenCode as the neutral bundled harness. This is the "bring your own model" promise made concrete — and it's why the platform bundles an MIT harness rather than any vendor's agent.
- **Pillar 3 — Local-first fleet economics:** all-in-one on a Mac Mini tonight, ZFS-clone agent fan-out on one box, promotion-not-migration to team/enterprise. Coder has no solo path; the vertical runtimes have no local path at all.

## 4. Watch items & kill conditions
- **Coder open-sources audit logging / Agent Firewall / AI Gateway,** or ships an EU-entity offering → pillar 0 halves; fall back to local-first + agent-agnostic + unit-of-work. Check their changelog/licensing quarterly — this is the single most important external variable.
- **Mistral moves up-stack** into agent orchestration/execution infra → tier-B partner becomes competitor with a better brand in-market; our counter is agent/model agnosticism (they will never route to OpenAI; we route to everyone). Cultivate the partnership early to raise the cost of competing with us.
- **EU AI Act enforcement softens** or agent-logging obligations get watered down → compliance pull weakens; pillar 3 (productivity economics) must carry more weight.
- **A sandbox API (esp. Morph) pivots to EU BYOC** → they arrive with runtime maturity; our moat is the governance layer plus incumbent EU relationships — build both deep early.
- **Sovereignty buyers won't pay a premium** (procurement talks sovereignty, buys Copilot) → reposition toward pillar 3 for AI-native mid-market, keep governance as the enterprise upsell.
