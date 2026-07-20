# Go-To-Market: Sovereign AI Developer Workspace Platform

**Date:** July 2026 · Companion to [DESIGN.md](DESIGN.md) and [COMPETITIVE.md](COMPETITIVE.md)

---

## 1. What This System Is (three altitudes)

### For an executive (30 seconds)
AI coding agents are the fastest-growing consumer of a company's most sensitive asset: its source code. Today, every agent product ships that code to someone else's cloud — almost always under US jurisdiction — with no unified record of what the agents did. This platform runs any AI coding agent on hardware the customer owns, under a policy and audit layer the customer controls, with the model itself chosen per-repository — from US frontier models down to fully self-hosted European ones. It installs on a single machine today and scales to an enterprise fleet without re-architecture.

### For a technical buyer (2 minutes)
The platform is three layers:
1. **A sandboxed execution engine** — each agent runs in a hardened, disposable sandbox (gVisor, default-deny networking) with its work persisted on snapshotted volumes. Filesystem-level cloning lets one repo fan out to twenty parallel agent branches on one box in seconds, with merge-back as ordinary git.
2. **A governance layer** — every agent starts from an org-curated *capability profile* (which MCP servers it can reach, hash-pinned; which skills it has, read-only; what context it's given), and every action lands in a tamper-evident *flight recorder* (prompt → tool call → diff → egress) shipped to the customer's SIEM. Enforcement sits below the agent: no agent can grant itself a tool, and a compromised MCP server can be revoked fleet-wide in one action.
3. **A sovereignty layer** — code, prompts, and logs never leave the customer boundary. The model endpoint is a routed policy choice per repo: US frontier via EU regions, EU providers (Mistral), or self-hosted open weights. The control plane that coordinates it all runs locally, in our EU-jurisdiction SaaS, or self-hosted — and sees metadata only.

### For a developer (30 seconds)
Install one binary on the machine on your desk. Point it at a repo. Fan out agents on isolated branches, watch them in a live terminal/diff view, merge the branch that wins. Everything runs locally, works offline except the model call, and you choose the model — including one running on your own GPU. When your company adopts it, your setup joins the org with one command; nothing migrates.

---

## 2. The Structured Pitch

### Problem (the wedge is fear + obligation, not productivity)
- European regulated organizations (banks, insurers, healthcare, public sector, defense-adjacent industrials) want the productivity of AI coding agents and **cannot accept the standard deployment**: code and prompts transiting US-jurisdiction clouds (CLOUD Act exposure), no unified audit of autonomous actions, and per-vendor lock-in of the model.
- The EU AI Act's logging/oversight obligations for deployers turn "what did the agents do?" from a nice-to-have into a filing requirement.
- Result today: these organizations either ban agents (losing the productivity), or adopt them with checkbox compliance (accumulating unmanaged risk). Both outcomes are unstable — which is the sales opening.

### Why now (three converging clocks)
1. **Agent adoption crossed from assistant to autonomous** — fleets of agents acting without a human per keystroke make governance a board-level topic.
2. **Jurisdiction anxiety is peaking** — the OpenAI/Ona acquisition (June 2026) showed model vendors capturing the runtime layer; European sovereignty programs and EU AI Act enforcement timelines create budgeted urgency.
3. **Open-weight coding models became good enough** (Devstral, Codestral, Apertus) that "fully in-country AI development" stopped being a fantasy — someone must supply the runtime that makes it usable.

### Solution (one sentence, from COMPETITIVE.md)
*The open, European-jurisdiction execution layer that governs what any AI coding agent — running any model, including yours — does with your code, on hardware you own, starting with the machine on your desk.*

### The lead enterprise narrative: "Centralize agentic AI — no agents on endpoints"
The strongest strategic pull for the enterprise sale is the **shadow-AI consolidation story**. Every security team is currently discovering Claude Code, Cursor, and Codex CLI installed ad hoc on laptops: corporate repos cloned to endpoints, personal API keys in dotfiles, zero audit, egress to whichever provider each developer picked. Their only tools today are banning (fails, drives it underground) or ignoring it (unmanaged risk). We are the third option: **a sanctioned place that is better than the laptop.**

- **The claim:** agents execute server-side; access is a browser session; repositories live on platform volumes and are **never cloned to endpoints at all**. Laptop stolen, compromised, or contractor offboarded — the code was never there. One egress point for all agentic AI collapses AI-Act scoping from "audit 500 laptops" to "audit one ledger."
- **The budget:** this framing pulls from endpoint security and shadow-IT remediation, not only platform engineering — and it recruits the CISO as champion rather than gatekeeper.
- **The enforcement story (we integrate, we don't build):** centralization sticks because the customer's existing MDM/egress controls block local agent binaries and provider APIs on laptops, and we supply the blocklist guidance and the destination to redirect people to. Their security stack becomes our enforcement arm.
- **Non-technical users:** analysts, ops, and PMs running agents for scripts, data wrangling, and automation multiply governed seats beyond engineering headcount (see Governed User tier, §3) — sequenced as a second ring, starting with semi-technical analysts, because they need task-level UI rather than a terminal and diff viewer.

**Two traps, held as explicit guardrails on this narrative:**
1. **Scope trap — "centralize your AI" is a bigger sentence than the product.** Taken literally it puts us against Microsoft 365 Copilot, Glean, and every enterprise AI portal — a bundling fight we lose. The defensible claim is exactly: **centralize *agentic execution*** — anything where an AI runs code, uses tools, and touches repositories. Chat-in-a-browser is not our fight; agents-with-hands is. All copy, decks, and sales training hold this line.
2. **VDI trap — developers will read centralization as VDI, and they have scar tissue.** Mitigations: the pitch is "no *agents* on endpoints," never "no coding on endpoints" — local IDEs stay, humans keep editing locally; it's autonomous tool-running that moves server-side. And the central platform must be *better* on day one, not merely compliant — which is why the fan-out demo and the sub-10s resume SLO are adoption survival conditions, not sales theater.

Layering: **sovereignty answers "why us, why Europe"; centralization answers "why now, and whose budget."** The narratives stack; neither replaces the other.

### Differentiation (defensible, verified against the field)
| Claim | Who can't match it, and why |
|---|---|
| **Open-source governance code** — the audit/egress/profile enforcement is inspectable | Coder paywalls exactly this (audit logging, Agent Firewall, AI Gateway are Premium + closed). "The code that watches your agents is open; theirs is not." |
| **EU-clean jurisdiction end to end** — including the metadata plane | Every US vendor (Coder, GitHub, Devin, Anthropic, OpenAI/Ona) fails this test even when self-hosted |
| **Agent- and model-agnostic** — one flight recorder and one capability policy across OpenCode, Claude Code, Codex CLI…; model per repo, tiered US-EU-self-hosted | Vertical vendors structurally can't (their agent, their model); Mistral won't route to OpenAI |
| **Local-first with promotion, not migration** — full platform on one Mac Mini, joins the org later unchanged | Coder has no solo path (coderd+Postgres+Terraform); vertical runtimes have no local path at all |
| **Fleet economics** — 20 parallel agent branches on one box via filesystem cloning | Workspace-per-human platforms and one-task-one-VM runtimes both price and perform worse at fan-out |

### Demo arc (the pitch is a demo, in this order)
1. **The hook (2 min):** one Mac-Mini-class box on the table. Fan out 20 agents on one real repo against one bug. Live terminals, live diffs. Merge the winner.
2. **The governance reveal (3 min):** open the flight recorder — every prompt, tool call, and egress attempt from those 20 agents, one ledger. Show a blocked exfiltration attempt (curl to an unlisted host → denied + logged). Pull an MCP server from the org profile → all agents lose it on restart.
3. **The sovereignty close (2 min):** flip one repo's model route from a frontier API to a local vLLM endpoint mid-session. Unplug the network cable; everything except model calls keeps working. "This is what 'your code never leaves the building' looks like when it's true."

### Objection handling (pre-armed)
- *"We already have Copilot/Claude/Cursor Enterprise."* — Those govern one vendor's agent with that vendor's telemetry. You will run more than one agent; who gives you one audit trail and one policy across all of them, and who holds it? (Also: their runtime, their jurisdiction.)
- *"Coder does self-hosted agents."* — Yes, and it's good — check whether the audit log, agent firewall, and AI gateway are in the open tier, whose jurisdiction the vendor is under, and what the minimum footprint is. All three answers favor us.
- *"Self-hosted models aren't good enough."* — Often true for architecture work; that's why routing is per-repo and tiered, with published task-success benchmarks per model profile. Sensitive repos on tier C, general work on tier A/B. Honesty here builds more trust than any claim.
- *"Another platform to operate?"* — One Linux image per host + a control plane we host in the EU (or one container you host). Compare with the coderd+Postgres+Terraform estate, or with operating three vendor VPC deployments.

### ICP and beachhead sequencing
1. **Beachhead:** EU mid-to-large regulated enterprises (500–10,000 engineers is the sweet spot: real compliance pressure, real platform teams, faster procurement than the very largest banks) in DACH/France/Benelux/Nordics. Entry buyer: platform engineering lead + CISO; economic buyer: CTO.
2. **Second ring:** EU public sector and defense-adjacent (longer cycles, bigger contracts, references compound), and AI-native EU startups that want the fan-out economics and inherit the governance for free.
3. **Channels:** (a) the open-source local mode is the top of funnel — every Mac Mini install is a pre-wired enterprise seat; (b) co-sell with EU model/cloud providers (Mistral tier-B routing, Hetzner/OVH/Scaleway reference architectures) — their regulated customers are our qualified pipeline; (c) compliance consultancies implementing EU AI Act programs, who need a technical control to recommend.

---

## 3. Monetization & Pricing

### The open-core line (drawn deliberately opposite to Coder — see COMPETITIVE.md §1)
**Open (Apache-2.0 or similar permissive for adoption; the governance code must be inspectable):** data plane image, sandbox runtime, egress proxy, flight recorder, capability-profile enforcement, local all-in-one mode, OpenCode integration and the sandbox contract.
**Commercial:** the EU SaaS control plane; multi-org/fleet management; SSO/SCIM at org scale; the self-hosted control plane license; signed sovereignty-contract releases; model-profile benchmark subscriptions; support/SLAs.

Rationale: giving away the governance code is what makes the sovereignty claim credible *and* what makes the free local tier genuinely complete for one person — while everything a *company* needs (coordinating many hosts, many people, many policies) is the paid surface. We monetize coordination and assurance, not surveillance features.

### Pricing metric: per developer, not per agent
The tempting metric — per agent-hour — is wrong for this market: it punishes the fan-out behavior we're selling ("run 20 agents" must never feel like a taxi meter), and regulated enterprises budget on predictable per-person costs. The governed *developer* is the stable unit of value: one human, unlimited agents and hosts within fair-use. Hosts are free and unlimited on paid tiers (we *want* hardware sprawl inside their boundary — it deepens the moat).

### Tiers
| Tier | Price (anchor) | What it is |
|---|---|---|
| **Solo / Local** | **Free, forever** | Full platform on machines you own; local control plane; local flight recorder; community support. No account required. The funnel and the credibility. |
| **Team** | **€39 / developer / month** | EU SaaS control plane; org capability profiles; SSO; SIEM shipping; up to 3 orgs' worth of policy layering; standard support. Self-serve with a card. |
| **Governed User** | **€15 / user / month (add-on to Team/Enterprise)** | Non-technical/semi-technical seats under the centralization narrative: task-level access to sandboxed agents under the same profiles and flight recorder, without terminal/diff surfaces. Second-ring product surface — priced now, shipped after the developer core is solid. |
| **Enterprise** | **€75–95 / developer / month, annual, 100-seat minimum** | Self-hosted *or* EU SaaS control plane; full RBAC/multi-org; fail-closed audit mode; signed sovereignty contract per release; air-gap support; model-profile benchmark subscription; named support with SLA. |
| **Sovereign add-on** | **+€20–30 / developer / month or fixed program fee** | For air-gapped/defense/public-sector: hardened releases, delivery via customer-controlled channels, extended N-version support, assistance with regulator submissions. |

Anchoring logic: Copilot Business/Enterprise sits at $19–39, Cursor at $20–40 — but those are *productivity* line items. This is a *governance + infrastructure* line item that replaces (or gates) several of them, priced against what it displaces: a Coder Premium estate, a DLP/CASB extension, and the internal platform team that would otherwise build it. €75–95 for enterprise is defensible the moment the AI Act filing uses our ledger as evidence. Notably, the customer's model spend flows through *their* provider agreements, not through us — we should **not** resell tokens in v1 (margin is thin, and being in the model money-flow undermines the neutrality story). Revisit only if customers beg for one bill.

### Expansion mechanics (how an account grows)
land (10–50 seats via a platform team pilot, often converted from organic local-mode use) → expand seats (agent adoption spreads team by team) → Enterprise upgrade (triggered by the first audit/AI-Act filing or the first air-gap requirement) → **centralization mandate** ("no agents on endpoints" policy, converting shadow-AI users into seats and adding Governed User seats beyond engineering) → Sovereign add-on (triggered by the most regulated business unit). Net-revenue-retention comes from seat growth, not usage growth — aligned with the flat-metric promise.

### What we deliberately don't charge for
- **Hosts/machines** — free at every tier (see above).
- **Agents or agent-hours** — the fan-out demo is the pitch; metering it would kill it.
- **The audit ledger itself at Solo/Team** — charging extra for compliance basics is the Coder mistake in mirror image; we charge for *organizational* compliance machinery (fail-closed mode, signed contracts, RBAC), not for the recorder.

### Unit economics sanity check
COGS at Team tier is nearly all control-plane hosting (metadata only — no code, no compute for customer workloads, no token resale), so gross margin on SaaS seats should exceed 85%; the customer supplies the expensive part (compute) by design. Enterprise self-hosted is software + support economics. The structural risk is not margin, it's **sales cost**: regulated-enterprise cycles are 6–18 months, which is exactly why the free local tier and the consultancy/model-provider channels must generate warm, pre-qualified pipeline rather than cold outbound.

### Open pricing questions (decide before first enterprise term sheet)
1. Does "developer" mean licensed human, or monthly-active human? (Recommend MAU with a floor — friendlier in agent-heavy orgs where some humans go weeks without direct commits.)
2. Is the self-hosted control plane license per-seat only, or per-seat + platform fee? (Recommend adding a modest platform fee — it prices the option value of air-gap and funds the N-2 support burden.)
3. Where does the Team→Enterprise RBAC line sit exactly? Too much RBAC in Team kills the upgrade; too little makes Team useless for 200-person orgs. Provisional: Team gets roles, Enterprise gets custom roles + OIDC group sync + multi-org.

---

## 4. TAM / SAM / SOM (bottom-up from governed seats)

All figures are ARR at the pricing anchors in §3; assumptions stated so they can be attacked individually. The centralization narrative is what moves TAM beyond developer headcount.

### Assumptions
| Assumption | Value | Basis |
|---|---|---|
| Professional developers in Europe (EU-27 + UK/CH/NO) | ~6.5M | Industry analyst estimates cluster at 6–7M |
| Share in enterprises (>250 employees) | ~40% → ~2.6M | Enterprise employment share of the developer population |
| Share in sovereignty-sensitive verticals (finance, insurance, health, public, energy, telco, defense/industrial) | ~45% of enterprise → **~1.2M dev seats** | These verticals over-index on in-house software in Europe |
| Blended paid dev seat | ~€900/yr | Mix of Team (€470/yr) and Enterprise (€900–1,140/yr) |
| Governed (non-technical) users, regulated orgs, steady state | 1–2× dev count | Analysts/ops/PM agent adoption under a centralization mandate; €180/yr each |

### The stack
- **Core wedge TAM (EU regulated dev seats):** 1.2M × ~€900 ≈ **€1.0–1.1B ARR**.
- **+ Governed Users (the centralization delta):** 1.2–2.4M non-technical seats × €180 ≈ **+€220–430M**. This is the concrete revenue answer to "why the centralization narrative matters": it adds ~20–40% to TAM *and* pulls from a second budget (endpoint security).
- **Full European enterprise expansion** (beyond regulated, once the beachhead holds): 2.6M dev seats + governed users → **~€2.5–3B ARR ceiling in Europe alone**.
- **SAM** (DACH/France/Benelux/Nordics regulated — where our GTM actually reaches in years 1–3): ~55–60% of the core wedge → **€600–750M ARR**.
- **SOM (5-year, credible):** 1.5–3% of SAM → **€10–20M ARR** ≈ 12,000–22,000 blended seats ≈ **15–30 enterprise accounts** of 300–1,000 devs plus their governed users. Sanity check: that's 3–6 new enterprise logos per year after year one, at regulated-industry sales cycles — aggressive but not fantasy.
- **Optionality excluded from the base case:** sovereignty demand outside Europe (UK already counted; Switzerland, Japan, Korea, Gulf states) plausibly doubles the ceiling; treat as expansion, not plan.

### Sensitivities (the honest ones)
- **Seat definition is the biggest lever:** MAU-based "developer" definitions in agent-heavy orgs could cut billable dev seats 20–30% — but the Governed User tier recovers much of it by capturing the humans the agents displace toward supervision. Decide the definition (§3 open questions) with this trade-off explicit.
- If **Copilot-bundle pricing pressure** forces the blended dev seat toward €600/yr, core wedge TAM drops to ~€700M — the governed-user and endpoint-security budget expansion then carries relatively more of the story.
- TAM here is *licensing* only; it excludes the sovereign add-on and any services, which are margin on top at the accounts that matter most.

## 5. GTM Risks (honest list)
- **The demo out-runs the product.** The pitch depends on the 20-agent fan-out and the live flight recorder actually working on one box. Do not begin enterprise sales motions before the demo arc is boringly reliable.
- **Coder responds** by open-sourcing governance or standing up an EU entity → pillar 0 halves (watch quarterly; COMPETITIVE.md kill conditions).
- **Sovereignty talks, Copilot walks:** procurement rhetoric doesn't always survive a 60% discount from Microsoft's bundle. Counter: never compete as a Copilot replacement — position as the layer their Copilot/Claude/Codex agents run *under* (agent-agnosticism is also commercial judo).
- **The free tier cannibalizes Team** for small companies (one control plane on a spare box). Accept it — those companies were never the buyer, and every such install is a reference and a résumé of the ecosystem.
- **AI Act enforcement slips** → compliance urgency deflates; pillar 3 (fan-out economics) must carry the pitch to AI-native buyers while the regulatory clock catches up.
- **Scope drift on "centralize your AI"** (trap 1, §2): if the narrative expands past agentic execution, we end up in the enterprise-AI-portal market against Microsoft's bundle. Guardrail: every asset says *agentic execution*; review quarterly for drift.
- **VDI backlash** (trap 2, §2): a centralization mandate that lands before the platform is demonstrably better than laptops creates organized developer resistance and poisons the account. Guardrail: no "no agents on endpoints" rollout recommendation until the account's pilot devs are voluntarily fanning out on the platform.
