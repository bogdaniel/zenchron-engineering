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
                            EngineeringPlan, AgentAssignment
                       #67  Repository Intelligence / ProjectModel v2
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

**M1-R (#63) is what this repository currently implements.** The runtime is a
persistent local control plane. An operator names the coding agents they have
already installed and authenticated, gives several of them work at once, watches
all of it from one place, reviews the results in GitHub, and leaves review
comments that reach the right worker without anything being copied between
tools.

What that milestone is careful NOT to be is an agent framework. Everything the
kernel established still holds: a coding agent authors changes and authorizes
none of them, every candidate mutation goes through a runtime-owned commit,
reassessment and exact-tree assurance, and publication is a #7 authority
decision rather than a provider's opinion of its own work.

See [`docs/product-architecture.md`](docs/product-architecture.md) for the shape
of the running system, and [`docs/supervisor.md`](docs/supervisor.md) for what
`serve` owns.

## The boundary between #63 and #64

M2 is the next layer, not the last one; see the graph above and #65 for what
follows it. The distinction below is singled out because it is the one most
likely to be collapsed by someone reading either issue alone, and it belongs in
durable documentation rather than only in an issue comment.

```text
Execution Agent                      Engineering Role
(#63, implemented)                   (#64, not implemented)

codex, claude, gemini, qwen,         product_architect, system_architect,
openai-protected, future adapters    planner, implementer, tester,
                                     security_reviewer, reviewer,
                                     integrator, release_reviewer

WHO does the work                    WHAT responsibility must be fulfilled
a named, configured worker           a semantic role in an engineering plan
```

An agent is a worker an operator installed. A role is a responsibility a plan
requires. #63 builds the workforce and the supervisor that runs it; #64 builds
the planner that decides which workforce a piece of work needs.

**#63 deliberately does not implement**: engineering roles, `EngineeringPlan`,
task decomposition, capability-based agent selection, cross-agent review
policies, or any planner. Naming those as runtime personas now would freeze a
decomposition before anything has been learned about which decompositions
actually work — which is exactly what principle P1 exists to prevent.

**What #63 leaves for #64**: a stable agent id separated from provider kind and
trust mode; capability and readiness metadata a planner can reason over; narrow
optional interfaces rather than one universal agent interface; a supervisor that
schedules durable runs rather than conversations; and a kernel that has never
learned a provider's name.

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

## Measuring it

The point of the milestone is leverage, not adapter count. #66 rebases the
benchmark on the actual `serve` product and measures whether #63 reaches the
>=2x hypothesis on ordinary work without worsening defects, rework or authority
safety. Higher-order leverage through role planning and decomposition belongs to
#64 and is measured there.
