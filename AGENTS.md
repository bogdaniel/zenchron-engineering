# AGENTS.md

This file is the persistent engineering handoff for Codex, Claude Code, Cursor, Gemini, local agents, and future coding agents working in this repository.

## Read first

Before making architectural or implementation changes, read:

1. `README.md`
2. `ROADMAP.md` — where the project is, and what the current milestone is a prerequisite for
3. `docs/product-architecture.md` — the system an operator actually runs
4. `docs/vision.md`
5. `docs/principles.md` and `docs/construction-principles.md`
6. `docs/architecture.md`
7. all accepted ADRs under `docs/adr/`
8. `docs/spec/v0.1.md`

Committed repository documents are the project source of truth. Chat history is not.

Read `ROADMAP.md` before concluding what this product is. The Authorization
Kernel is the core, and a governed single-task runtime is not the finished
category: #63 built the persistent multi-agent execution runtime, and #64 builds
the Engineering Planner above it. Both are implemented here. Repository
Intelligence (#67), the external adapter protocol (#103) and everything after
them are separate, later layers; do not implement any of them while working on
something else.

## Project identity

**Product/category:** Zenchron Engineering OS

**Repository:** `zenchron-engineering`

**Core:** Engineering Authorization Kernel

Zenchron transforms engineering intent into bounded, policy-governed work, independently verifiable evidence, and explicit authority decisions.

It is not primarily an agent orchestrator.

## Core invariant

```text
engineering facts
  -> policy
  -> obligations/invariants
  -> evidence
  -> authority
```

Agents may reason and execute. Agents do not self-authorize material outcomes.

## Technical constraints

- Core implementation language is Go.
- Canonical persisted/interchanged representation is JSON.
- JSON Schema defines external contract shapes.
- Do not introduce YAML as an equal canonical representation.
- Provider-specific behavior must remain behind adapters.
- The authorization kernel must not depend on Claude, Codex, or any single model vendor.
- `zenchron-engineering`, `sentinel-shield` and `zenchron-foundry` are SEPARATE
  products. This repository must be complete and useful without either sibling.
  No milestone here depends on one, neither is a privileged or native
  implementation inside it, and branded origin never grants stronger trust or
  authority. Any external product integrates later through the same generic
  ExecutionProvider, AssuranceProvider, evidence-producer and
  environment/provenance interfaces every other implementation uses.

## Architectural constraints

Do not:

- model permanent `Architect Agent`, `Test Agent`, `Security Agent`, etc. as kernel primitives;
- build rigid multi-agent workflows when obligations can describe required outcomes;
- use an opaque aggregate risk score as an authorization mechanism;
- collapse unknown engineering facts into `false`;
- allow an implementing producer to be the sole source of acceptance evidence for a material claim;
- automatically expand privilege during contract recompilation;
- allow execution-learning logic to activate governance-policy changes;
- couple core domain types to a particular CI, Git host, LLM provider, or sandbox provider.

Prefer:

- typed facts with provenance and uncertainty;
- deterministic policy resolution after facts are established;
- obligations and invariants rather than prescribed agent sequences;
- exact revision/subject binding for evidence;
- explicit capability, permission, and authority boundaries;
- contract reassessment when observed scope materially differs from predicted scope;
- boring infrastructure until the engineering semantics are proven.

## Current product surface

The operator-facing system is documented, not inferred:

- `docs/supervisor.md` — `serve`, the persistent supervisor, and its
  local control endpoint
- `docs/agents.md` — named execution agents, trust modes, provenance and the
  provider-handoff refusal
- `docs/planning.md` — operator-defined agent profiles, instruction packs,
  context policies, plan templates, and the plan approval boundary
- `docs/spec/planning-v0.1.md` — the normative M2 planning artifacts
- `docs/getting-started.md`, `docs/running-work.md`,
  `docs/running-multiple-tasks.md`, `docs/github-feedback.md`,
  `docs/configuration.md`, `docs/troubleshooting.md`

Seven distinctions in that surface are frequently collapsed by a reader in a
hurry, and collapsing any of them is a defect:

- **agent id != provider kind != trust mode.** They are three separate facts.
- **execution trust != acceptance authority.** An `operator_trusted` worker may
  author a change and may never accept it.
- **EngineeringRole != AgentProfile != ExecutionAgent.** A role is a
  responsibility a plan requires; a profile is an operator-defined
  specialization of a worker; an execution agent is the installed worker. One
  agent backs several profiles, and one role resolves to whichever profile is
  eligible.
- **EngineeringPlanTemplate != EngineeringPlan.** A template is reusable
  planning input with no governance or execution authority. A plan is a
  validated, approved, immutable revision.
- **EngineeringPolicy != ContextPolicy.** `EngineeringPolicy` is the only
  obligation system in this product. A `ContextPolicy` selects which context
  classes an assignment receives; it grants no authority and can never remove
  context the work contract requires.
- **only `agent` stages become EngineeringRuns.** `assurance_gate` and
  `human_decision_gate` reference existing evidence, authority and
  human-decision state. A gate that created a worker run would be a fake run
  asserting evidence nothing produced.
- **customization may specialize or narrow; it may not escalate.** A profile,
  instruction pack, context policy or template cannot raise trust, widen
  access, raise a ceiling, grant publication or acceptance authority, suppress
  a policy obligation, or reset consumed budget.

## Design principles for implementation

`docs/construction-principles.md` is binding, not advisory: DRY for knowledge
rather than for text, Go-adapted SOLID, composition over inheritance, and YAGNI.
A change can be functionally green and still fail review for materially
violating them. Provider-specific knowledge belongs in adapters and their specs;
adding a provider must not require semantic changes in kernel or domain
packages.

## Working style

For non-trivial changes:

1. inspect current repository state;
2. identify which specification or ADR governs the change;
3. state any architectural contradiction you discover rather than silently changing the thesis;
4. keep changes scoped to one coherent issue;
5. add or update tests for behavioral changes;
6. update documentation when a public contract or architectural invariant changes;
7. prefer small, reviewable commits;
8. report assumptions, unresolved uncertainty, and evidence produced.

## Milestones

**M0-M1, complete.** Prove the authorization kernel using representative
scenarios (below), and build the durable single-task local runtime above it.

**M1-R (#63), adopted.** Make that runtime a persistent, usable, multi-agent
engineering execution runtime: `serve`, named execution agents, concurrent runs,
the GitHub feedback loop, and an operator control room.

**M2 (#64), the current surface.** The Engineering Planner above that runtime:
engineering roles and capabilities, operator-defined `AgentProfile`s built from
`InstructionPack`s and `ContextPolicy`s, reusable `EngineeringPlanTemplate`s, a
proposed `EngineeringPlan` that an operator approves before anything executes,
typed assurance and human-decision gates, an aggregate plan budget envelope, and
a plan reconciler inside `serve` that turns dependency-ready `agent` stages into
ordinary #63 `EngineeringRun`s. Read `docs/spec/planning-v0.1.md` for the
normative artifacts and `docs/planning.md` for the operator surface.

What #64 is careful NOT to be is a second runtime. It adds no scheduler, no task
database, no event journal, no policy system and no authority system: role,
capability, independence and gate obligations compile through the existing
`EngineeringPolicy` compiler, plans persist in the existing SQLite store and
journal, and the existing scheduler and leases remain the only thing that
decides when a run executes.

**M3 and beyond, not implemented.** #67 is Repository Intelligence beside this
layer, and #103 owns the external execution-agent adapter protocol that #64
deliberately split out. #65 is the accepted roadmap tracker and carries the full
graph through M7; `ROADMAP.md` is its in-repository form. Do not build any of
that while working on something else, and treat later milestones as hypotheses
to be reviewed against evidence rather than a queue.

Still true, and still the reason the kernel came first: do not build a broad
autonomous engineering platform. Breadth is earned one governed capability at a
time, and every capability added since M0 has had to keep the kernel's
invariants intact.

The kernel scenarios that first milestone proved, retained here because they
remain the acceptance shape for governance work:

- trivial change;
- normal behavioral change;
- security-sensitive change;
- hidden scope expansion;
- material uncertainty/misclassification;
- stale evidence;
- privilege escalation request;
- failing evidence and remediation.

Primary metric: **accepted engineering changes per human supervision hour**.

Secondary metrics include authority precision/recall, first-pass acceptance, rework, escaped defects, false policy blocks, cost per accepted change, evidence completeness, and contract recompilation frequency.
