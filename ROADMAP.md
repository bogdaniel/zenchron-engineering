# Roadmap

This file exists because the repository could explain the Authorization Kernel
well and could not explain where the product was going. A coding agent reading
`docs/architecture.md` alone would reasonably conclude that a governed
single-task runtime is the finished category. It is not.

The accepted roadmap is tracked in **#65**, which is authoritative for
sequencing and for the product boundaries below. This file is its durable
in-repository form; where the two ever disagree, #65 is the tracker and this is
the copy that needs updating.

```text
        Foundation     Engineering Authorization Kernel + local runtime
                       facts -> policy -> obligations -> evidence -> authority
                       trackers: #12, #29, #33
                              |
        M1             Useful persistent multi-agent Engineering Runtime
                       #63  serve, named workers, concurrent runs,
                            control room, GitHub feedback loop
                       #66  rebase the benchmark on the actual product
                              |
        M2             Agentic Engineering System
                       #64  Engineering Planner, roles, capabilities,
                            AgentProfile, EngineeringPlan, AgentAssignment
                       #67  Repository Intelligence / ProjectModel v2
                       #103 external execution-agent adapter protocol
                              |
        M3             Continuous self-building
                       #69  roadmap -> planner -> serve -> governed runs
                            -> review -> adoption -> next eligible work
                              |
        M4             Engineering Learning Plane
                       #70  empirical routing and optimization, with
                            PolicyChangeProposal and no self-activation
                              |
        M5             Multi-repository engineering programs
                       #71  cross-repository impact, dependency-aware plans
                              |
        M6             Organization / ecosystem platform
                       #72  team control plane, shared policy, identities
                       #74  engineering pack ecosystem
                       #30  trusted repository-specific extensions
                       #31  portable EngineeringRun handoff
                              |
        M7             Policy-governed autonomous software operations
                       #73  CVEs, dependencies, incidents, drift,
                            deployment and rollback authority
```

Each milestone has a product gate in #65. Later milestones are **hypotheses**,
to be reviewed against evidence from earlier ones before implementation begins —
not a queue to work through.

## Product separation law

`zenchron-engineering`, `sentinel-shield` and `zenchron-foundry` are separate
products and separate repositories.

```text
zenchron-engineering != sentinel-shield != zenchron-foundry
```

Zenchron Engineering must be complete and useful without either sibling. No
milestone here depends on one; neither is a privileged or native implementation
inside this repository; the core defines generic execution, assurance, evidence
and environment/provenance capabilities; branded origin never grants stronger
trust or authority; and any external product may integrate later through those
stable generic interfaces. #68 was closed `not_planned` for violating this
boundary.

## Leverage, not feature count

The roadmap is organized around human leverage. The north star is **accepted
engineering changes per human supervision hour**, guarded by authority quality,
escaped defects, rework, false blocks and cost.

```text
~1x        a human operates coding agents by hand
M1  >=2x   several explicit tasks supervised concurrently, no message shuttling
M2  5-10x  roles planned and work decomposed across governed runs
M3+        supervision moves to programs rather than individual tasks
```

These are hypotheses to measure. They are deliberately not encoded as
assertions anywhere in the test suite: a number the system can check is a number
the system will optimize, and these have to be earned against real work.

## Where the project is now

**M2 (#64) is what this repository currently implements, on top of the adopted
M1-R (#63) runtime.**

#63 made the runtime a persistent local control plane. An operator names the
coding agents they have already installed and authenticated, gives several of
them work at once, watches all of it from one place, reviews the results in
GitHub, and leaves review comments that reach the right worker without anything
being copied between tools.

#64 adds the layer that decides what work is required. An operator states an
engineering outcome; Zenchron compiles the roles, capabilities, context,
independence obligations and typed gates that outcome needs into an explicit
`EngineeringPlan`, presents it with its assignments, blockers and aggregate
budget, and executes nothing until the operator approves it. Approved
dependency-ready stages become ordinary #63 `EngineeringRun`s.

What neither milestone is is an agent framework. Everything the kernel
established still holds: a coding agent authors changes and authorizes none of
them, every candidate mutation goes through a runtime-owned commit,
reassessment and exact-tree assurance, and publication is a #7 authority
decision rather than a provider's opinion of its own work. A plan is coordination
above that, not a second authority beside it.

See [`docs/product-architecture.md`](docs/product-architecture.md) for the shape
of the running system, [`docs/supervisor.md`](docs/supervisor.md) for what
`serve` owns, and [`docs/planning.md`](docs/planning.md) for the operator surface
the planner adds.

## The boundary between #63 and #64

M3 is the next layer, not the last one; see the graph above and #65 for what
follows it. The distinction below is singled out because it is the one most
likely to be collapsed by someone reading either issue alone, and it belongs in
durable documentation rather than only in an issue comment.

```text
Execution Agent                      Engineering Role
(#63)                                (#64)

codex, claude, gemini, qwen,         product_architect, system_architect,
openai-protected, future adapters    planner, implementer, tester,
                                     security_reviewer, reviewer,
                                     integrator, release_reviewer

WHO does the work                    WHAT responsibility must be fulfilled
a named, configured worker           a semantic role in an engineering plan
```

An agent is a worker an operator installed. A role is a responsibility a plan
requires. #63 built the workforce and the supervisor that runs it; #64 built the
planner that decides which workforce a piece of work needs.

#64 added a third concept between them, and collapsing it into either is the
same defect:

```text
AgentProfile
(#64)

zenchron-security-reviewer -> claude   an operator-defined specialization:
zenchron-architect         -> claude   instructions, context policy, effective
zenchron-builder           -> codex    capabilities, tighter constraints

HOW a worker was specialized for a responsibility
```

One `ExecutionAgent` backs several profiles. One `EngineeringRole` resolves to
whichever profile is eligible for it. Neither collapses into the other, and a
profile can only ever narrow what its underlying agent already is.

**What #63 left for #64, and #64 consumed**: a stable agent id separated from
provider kind and trust mode; capability and readiness metadata a planner can
reason over; narrow optional interfaces rather than one universal agent
interface; a supervisor that schedules durable runs rather than conversations;
and a kernel that has never learned a provider's name.

## What #64 implemented

- **Roles and capabilities.** A closed v0 role catalogue and a deliberately
  small capability ontology, with the capability floor each role requires and
  which roles produce material change.
- **Plan-shaped obligations through the existing policy compiler.** Role,
  capability, independence, trust and gate requirements are new
  `EngineeringPolicy` effect members compiled into the work contract. There is
  no `PlannerPolicy`: obligations from two rules merge to the stronger
  requirement, and the one member that is a permission rather than an obligation
  — permitted human substitution — intersects instead.
- **Operator-defined composition.** `InstructionPack`, `ContextPolicy`,
  `AgentProfile` and `EngineeringPlanTemplate` are operator-owned files under
  `planning_dir`, content-addressed and sealed on load. An operator builds a
  reusable custom agent over an installed worker without changing core code.
- **`EngineeringPlan` as a revisioned immutable artifact**, with typed `agent`,
  `assurance_gate` and `human_decision_gate` stages, an aggregate budget
  envelope, and canonical JSON validated by schema exactly as the kernel
  contracts are.
- **A provable non-mutating planning invocation.** Planner reasoning runs
  through a registered #63 execution agent in the provider's own enforceable
  read-only mode — Codex's `read-only` sandbox, Claude Code's `plan` permission
  mode — against a runtime-owned planning workspace the runtime verifies was
  unchanged afterwards. No direct provider API path was introduced.
- **Deterministic validation and an explicit approval boundary.** Cycles,
  dangling dependencies, unknown vocabulary, gates carrying worker requirements
  and privilege-expanding proposals are refused before an operator ever sees
  them; nothing executes until the operator approves a revision.
- **Typed independence.** Profile, execution agent, provider kind, vendor family
  and human legs are distinct equivalence classes, ordered so a stronger
  requirement is never silently degraded. Where only one vendor is eligible and
  policy required vendor independence, the stage blocks explicitly and offers the
  human substitution policy permitted - as an operator decision that produces a
  revision, never as something the resolver applies.
- **A plan reconciler inside `serve`.** It marks gates satisfied from existing
  evidence, authority and human-decision state, identifies dependency-ready
  `agent` stages, and creates ordinary `EngineeringRun`s for them. The existing
  scheduler and leases remain the only worker scheduling mechanism.

## What #64 deliberately refused

- **A second scheduler, task database, event journal, policy system or
  authority system.** Plans persist in the existing SQLite store and journal,
  and dependency-ready stages become ordinary runs the existing scheduler
  executes.
- **Automatic plan approval.** A proposed plan does not execute because the
  planner returned valid JSON. Convenience is not a reason to remove the one
  boundary an operator has before several workers start spending their
  subscription.
- **Nested plans and sub-plan schedulers.** A planner-role decomposition stage
  emits a `PlanRevisionProposal` against the current revision. A plan containing
  a plan is not representable, in a template or in a plan.
- **A permissive planning fallback.** A provider that cannot enter a provable
  non-mutating mode is ineligible for planner-role stages. It is never run in
  its ordinary editing mode instead.
- **Repository-authored instruction.** Candidate content may be ordinary
  engineering context and may never become privileged agent instruction. An
  artifact source has only operator classes, and nothing reads instruction
  content out of a candidate.
- **An AI quality router.** Which profile suits which stage is decided by trust
  eligibility, invocation mode, availability, independence and operator
  preference. Learned routing waits for #70 and the evidence it needs.

## Known limitations of #64

Stated here rather than left to be rediscovered, and each with the upgrade that
would close it.

- **Gemini CLI and the brokered OpenAI provider have no provable non-mutating
  mode**, so neither is eligible for a planner-role stage. Gemini's approval
  modes bound what is auto-approved rather than proving the model cannot write;
  the brokered provider's read-only mode would be a restricted tool surface
  rather than a CLI flag, which is separate work. The upgrade is a read-only
  tool surface for the brokered provider.
- **Qwen's read-only mode is not live-qualified.** Qwen Code is not installed on
  the acceptance host, so its `plan` approval mode is taken from upstream
  documentation rather than verified against a binary — the same honest gap #63
  records for Gemini and Qwen generally. The probe is fail-closed, so a wrong
  flag makes the agent ineligible for planning rather than mis-invoked.
- **The capability ontology does not discriminate between today's coding CLIs.**
  Codex, Claude Code, Gemini and Qwen are general coding agents and advertise the
  same broad capability set, which is stated honestly rather than differentiated
  for appearance. Resolver discrimination therefore comes from trust eligibility,
  invocation-mode support, availability, independence and operator preference. A
  capability id is added when it creates a real eligibility distinction.
- **ContextPack v0 consumes ProjectModel v1 as it is.** It grows no
  repository-analysis or dependency graph of its own; #67 enriches the model and
  #64 consumes richer facts when they exist.
- **A downstream stage can only build on PUBLISHED upstream work.** Where its
  dependencies published a candidate, the stage's workspace is cloned at that
  exact commit and it builds on the change. Where they did not - the work exists
  only in another run's local workspace - the stage runs at the trusted base and
  is given the upstream commit, tree and diff as untrusted data instead. That is
  enough for a review and not enough for an integration stage that must build on
  unpublished work; the upgrade needs the governed-remote boundary to gain a
  local-source case.
- **A failed stage is terminal for its plan.** The plan reconciler does not
  retry a stage whose run failed: retries inside a run belong to the existing
  scheduler, and re-running a stage is a new revision through the ordinary
  approval boundary. The upgrade is an explicit `plan retry STAGE` that produces
  that revision for the operator.
- **A review stage's findings live in its transcript.** A reviewing role runs as
  an ordinary run and produces no candidate change, so its judgement reaches an
  operator through `autonomy logs RUN` rather than as structured findings the
  plan consumes. Turning a review into typed findings that a downstream
  remediation stage can bind to is follow-up work; today it is text a person
  reads.
- **External execution-agent adapters are out of scope.** #64 plans against
  already-registered #63 workers. The versioned external adapter protocol is
  #103, and pack/adapter distribution is #74.

## What #63 implemented

- A named agent registry: stable agent ids, provider kinds and explicit trust
  modes, with backward-compatible migration from the single-provider
  configuration.
- Native adapters for Codex CLI, Claude Code, Gemini CLI and an agentic Qwen
  CLI, all driven through one shared operator-trusted lifecycle, plus the
  existing protected OpenAI Responses provider.
- `serve`: a persistent supervisor owning the scheduler, with an owner-only
  local control endpoint, drain / shutdown / stop-all lifecycle semantics, and
  one shared forge observation stream per repository.
- Concurrent EngineeringRuns under an operator ceiling, each with its own
  candidate workspace, branch, agent binding, budgets and PR lifecycle.
- A GitHub feedback loop gated by actor admission, delivered exactly once, with
  self-loop prevention by identity.
- An operator control room: `agents`, fleet `status`, `logs`, and trust-aware
  `doctor`.
- Typed provider capacity waits and an operator state-storage ceiling.

## What #63 deliberately refused

- **In-run provider handoff.** Changing a live run's agent is a typed, journalled
  refusal that records everything a successor would have needed, and points at a
  new generation instead. See [`docs/agents.md`](docs/agents.md).
- **Automatic issue discovery by default.** An issue exists because a project
  records work in issues. Its existence is not consent to spend a subscription
  on it.
- **A raw local-model coding harness.** Local Qwen support drives an
  already-agentic CLI. A completion endpoint cannot edit a file, so supporting
  one would mean building a second coding agent inside this repository.
- **AI-based agent routing.** Which worker suits which ticket is an operator's
  decision; a system that made it silently would be spending their subscriptions
  on its own opinion.

## Known limitations of #63

Stated here rather than left to be rediscovered, and each with the upgrade that
would close it. None is load-bearing for the milestone's acceptance; all are
things a reviewer or operator should know before relying on them.

- **Provider handoff is refusal-only.** The typed record carries everything a
  successor would need; performing the transition does not.
- **No provider-native session identifier is persisted.** Continuity comes from
  the candidate, the contract, the admitted feedback and the journal, so the
  "session metadata does not migrate" law holds trivially rather than through
  machinery.
- **The concurrency ceiling bounds whole-run reconciliation**, not specifically
  expensive execution operations. A run holds a slot whether it is invoking a
  coding agent or making a cheap forge read. The upgrade is a
  per-operation-kind concurrency class in the scheduler; see
  [`docs/supervisor.md`](docs/supervisor.md).
- **Provider flag vocabulary must be verified against the operator's INSTALLED
  CLIs**, never against documentation alone. An upstream rename does not
  announce itself: it turns a working agent into a permanently unavailable one,
  described in terms of capabilities rather than of the flag that moved. That
  makes it a live-acceptance step, not only a debugging step; see
  [`docs/troubleshooting.md`](docs/troubleshooting.md).
- **Gemini exposes no flag suppressing workspace instruction files**, so its
  provenance records `workspace_instructions_suppressed: false` rather than
  claiming parity with the other three adapters.
- **Structured provider output is not parsed.** Four providers would mean four
  schemas and four parsers; the invocation provenance already answers what this
  milestone asks for.
- **The state-storage ceiling uses a fixed per-run reservation** and a full
  directory walk per allocation, both marked `ponytail:` in the source with
  their upgrade path.
- **Feedback delivery is bounded** to the most recent applicable items per
  invocation, newest first.
- **The review loop needs a publication identity of its own.** Feedback is
  admitted by WHO wrote it, which is what makes admission robust against what it
  says. With `github.credential_mode: "github-cli"` the runtime publishes as the
  operator, so the guard that refuses runtime-authored feedback refuses the
  operator's reviews too and the loop cannot run. `credential_mode: "token"`
  gives the runtime its own identity - a GitHub App installation token or a
  dedicated account - and `autonomy doctor` names the collision when it exists.
  The guard itself is unchanged: an operator's own login is never admitted as an
  exception.
- **Gemini CLI and Qwen CLI are not live-qualified.** Neither is installed on
  the acceptance host, so their adapters are covered by deterministic tests and
  their flag grammar is taken from upstream documentation rather than verified
  against a binary. The probe is fail-closed, so a wrong flag makes the agent
  unavailable rather than mis-invoked - a mitigation, not a substitute. #63 does
  not wait on them; they are qualified when there is an intent to use them.
- **Policy cannot require protected execution.** The isolation gate is applied
  per agent trust mode and fails closed on anything not explicitly
  `operator_trusted`, and a `protected` run is never continued by an
  operator-trusted worker - but there is no rule vocabulary for "this work
  requires protected execution", so nothing matches work to a trust mode. Until
  #63 this was masked: the gate was unconditional, which refused every native
  CLI at construction and made the operator-trusted path unreachable, so the
  guarantee held only because nothing could run. The upgrade is a policy effect
  naming a required execution trust, evaluated where permissions already are.

## Measuring it

The point of the milestone is leverage, not adapter count. #66 rebases the
benchmark on the actual `serve` product and defines three comparable modes:
`direct_agent`, `zenchron_explicit` (#63) and `zenchron_planned` (#64).

#64's bounded self-building result is recorded against `zenchron_planned`
semantics, with whatever it actually measured. The 5-10x figure in the leverage
table above remains a hypothesis to be earned against real work, and it is
deliberately not encoded as an assertion anywhere in the test suite: a number the
system can check is a number the system will optimize.
