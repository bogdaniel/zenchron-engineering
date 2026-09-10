# Product architecture

`docs/architecture.md` describes the Authorization Kernel: how engineering facts
become obligations, evidence and an authority decision. This document describes
the system an operator actually runs, and where the kernel sits inside it.

The two are not alternatives. Every path below ends at the same kernel.

## The running system

```text
                        operator                      GitHub
                            |                            |
              states work,  |                            | issues, reviews,
              approves      |                            | comments, CI
              plans,        |                            |
              reviews PRs   |                            |
                            v                            v
                    +---------------------------------------------+
                    |   zenchron-engineering serve                |
                    |   persistent supervisor, owns the scheduler |
                    +---------------------------------------------+
                       |          |          |          |         |
             agent     |  plan    |   run    | shared   | control |
             registry  |  recon-  | scheduler| forge    | endpoint|
             + profiles|  ciler   |          | observer | (local) |
                       v          v          v          v         v
                    +---------------------------------------------+
                    |        durable EngineeringRuns              |
                    |        one per task, in one SQLite store    |
                    +---------------------------------------------+
                                     |
              +----------+-----------+-----------+-----------+
              v          v           v           v           v
           Codex      Claude      Gemini       Qwen       protected
           CLI        Code        CLI          CLI        OpenAI
              \          \          |          /           /
               \          \         |         /           /
                +----------+--------+--------+-----------+
                                     |
                          candidate workspace (per run)
                                     |
                          runtime-owned commit + tree
                                     |
                                 reassessment
                                     |
                          exact-tree assurance + evidence
                                     |
                             authority evaluator
                                     |
                             push / pull request
                                     |
                              human review in GitHub
```

Four things about this diagram are load-bearing.

**The agents are at the bottom, not the middle.** They author changes. Every
arrow from the candidate workspace THROUGH publication is runtime-owned, and
none of it is influenced by what a provider said about its own work. Review and
merge are the arrows after that, and both remain external: a human reads the
pull request in GitHub and a human merges it.

**There is one store and one scheduler.** The supervisor is an owner, not a
second runtime. Adding `serve` added ownership, concurrency, a shared
observation path and a lifecycle; it added no second task model, no second
journal and no second authority system.

**The control endpoint is inside the box.** It is local-only and owner-only,
because anything that can submit work to it can start coding agents under the
operator's account. See [`supervisor.md`](supervisor.md).

**The plan reconciler is beside the scheduler, not above it.** It answers which
dependency-ready stage should become a run and which typed gate has been
satisfied. It never decides when a run executes, never holds a lease, and never
duplicates a concurrency ceiling: the scheduler that was already there does all
of that. See [`planning.md`](planning.md).

## Where a plan sits

```text
engineering intent (an issue, an objective)
        |
        +---- ProjectModel facts ----+
        +---- EngineeringPolicy -----+     compiled obligations:
        +---- PlanTemplate ----------+     roles, capabilities,
                                     |     independence, gates
                                     v
                          Engineering Planner
              deterministic compilation, plus optional reasoning
              through a registered agent in a NON-MUTATING mode
                                     |
                                     v
                          proposed EngineeringPlan
                                     |
                          deterministic validation
                          (cycles, references, trust,
                           capability, budget, independence)
                                     |
                                     v
                        OPERATOR PLAN APPROVAL
              decomposition, assignments, blockers, budget envelope
                                     |
                                     v
                       approved immutable plan revision
                                     |
                          plan reconciler inside serve
             run state + assurance/evidence + human decisions
                                     |
                        dependency-ready stages and gates
                                     |
                    +----------------+----------------+
                    v                                 v
             AgentAssignments                   typed gates
             (agent stages only)         assurance_gate, human_decision_gate
                    |                                 |
                    v                                 |
          ordinary #63 EngineeringRuns                |
                    |                                 |
                    +----------------+----------------+
                                     v
                        assurance + AuthorityDecision
                                     |
                                     v
                          external human authority
```

The two halves of that picture answer different questions. The planner decides
**what work is required**; the reconciler decides **which dependency-ready stage
becomes a run and which gate is satisfied**; the existing scheduler decides
**when an eligible run executes**; the Authorization Kernel decides **what may be
committed to the world**. Nothing was moved between them.

An approved plan is operator authority to execute within already-existing policy
and permission ceilings. It is not merge authority, not release authority, and
not acceptance authority. Only `agent` stages become runs: a gate references
evidence, authority or human-decision state that other machinery already owns,
because a gate that created a worker run would be asserting evidence nothing
produced.

## Composition: profiles over agents

```text
   operator planning_dir            registry                    a plan stage

   instructions/<id>.json  -->  InstructionPack  --+
   context/<id>.json       -->  ContextPolicy   ---+--> AgentProfile
   profiles/<id>.json      -->  AgentProfile  -----+         |
   templates/<id>.json     -->  PlanTemplate                 | resolved for
                                                             | role +
                                                             | capabilities +
   agents:                                                   | independence +
     codex  -> codex_cli   -->  ExecutionAgent  <------------+ invocation mode
     claude -> claude_code
```

One `ExecutionAgent` backs several profiles; one role resolves to whichever
profile is eligible. A profile is a specialization of a worker the operator
already registered: it can narrow capabilities, add operator-owned instructions,
select context and tighten constraints, and it can never raise trust, widen
access or grant authority. See [`planning.md`](planning.md).

## Two intake paths

```text
    explicit operator submission                optional discovery
    (the default, and the product)              (off unless configured)

    autonomy run issue 123 --agent codex        watch.repositories + label
                    |                                     |
                    +-------------------+-----------------+
                                        v
                              EngineeringRun created
```

Issue existence causes no execution. A GitHub issue exists because the project
records work in issues; treating that as consent would mean an operator's
subscription is spent by other people filing tickets. Automatic discovery
remains available as one intake policy, and new configuration enables none.

## One task, end to end

```text
issue snapshot pinned (title/body frozen, digested)
        |
contract compiled from facts + policy
        |
candidate workspace cloned (runtime-owned, per run)
        |
   +--> execution agent invoked  <-------------------+
   |            |                                    |
   |    workspace observed (not the provider's word) |
   |            |                                    |
   |    runtime-owned commit + tree                  |
   |            |                                    |
   |    reassessment against the governance envelope |
   |            |                                    |
   |    exact-tree assurance, independent evidence   |
   |            |                                    |
   |    authority evaluation for publication         |
   |            |                                    |
   |    push + pull request (runtime-owned)          |
   |            |                                    |
   |    human review in GitHub                       |
   |            |                                    |
   +---- admitted feedback ---------------------------+
                |
        human merges, externally
```

The loop from review back to the agent is what removes the operator from the
message bus. What it does not do is remove them from the decision: merge stays
external, and no configuration in this repository authorizes self-adoption.

## The agent registry, and what the planner reads from it

```text
   operator configuration            runtime                     planner (#64)

   agents:                       AgentRegistry              EngineeringPlan
     codex   -> codex_cli    ->    resolve id        ->       role: implementer
     claude  -> claude_code        trust mode                 capability: code change
     gemini  -> gemini_cli         readiness                  vendor family
     qwen    -> qwen_cli           model/version              invocation modes
     openai  -> openai_responses   capabilities               -> eligible profile
```

An agent id, a provider kind and a trust mode are three separate facts. Keeping
them separate is what lets an operator rename a worker without changing its
trust, lets a run stay bound to the worker it started with while defaults move
underneath it, and lets the planner reason about capability without the kernel
learning any provider's name.

The planner reads a projection of that registry — id, vendor family, trust mode,
capabilities, invocation modes, readiness — and nothing else. Vendor family is a
separate fact from provider kind on purpose: two adapter ids backed by the same
vendor are not vendor-family independent, so a plan requiring vendor independence
cannot be satisfied by adding another adapter for the same models.

Adding a provider touches an adapter spec, a registration in the composition
root, and tests. It does not touch the scheduler, the reconciler, the kernel,
the authority evaluator, Git or the forge adapter. That is a design property
this milestone is reviewed against rather than a runtime test, because a test
that added an imaginary provider would prove only that the fake was easy to add.

## Trust, stated honestly

```text
operator_trusted                          protected
-----------------------------------       -----------------------------------
your installed, authenticated CLI         brokered provider
runs under your account                   runs behind a proven boundary
least-privilege provider mode selected    isolation properties proven
no publication credential injected        fails closed when unproven
environment built from scratch
host READ confinement UNPROVEN            eligible for protected-required work
```

`operator_trusted` is a truthful classification, not a degraded `protected`. It
says: this is approximately the trust you already extend by running `codex`
yourself, plus the runtime's guarantees about credentials, permission mode and
provenance, minus any claim about what the process can read. It will never
become `protected` by renaming it.

The isolation gate is applied per trust mode and fails closed: anything not
explicitly `operator_trusted` must prove the boundary first. What does not exist
yet is a policy vocabulary for "this work requires protected execution", so
matching work to a trust mode is the operator's decision rather than a rule the
runtime enforces.

Neither trust mode is authority. An `operator_trusted` agent may write the
change and cannot accept it; a `protected` one cannot either.

## Where the kernel is

Unchanged, and deliberately unaware of everything above:

```text
domain/        typed facts, contracts, evidence, authority decisions,
               and the M2 planning artifacts
policy/        deterministic policy resolution, including the role,
               capability, independence and gate obligations
authority/     action-scoped authority evaluation
evidence/      evidence binding and staleness
reassessment/  observed-scope reassessment
analysis/      project model and observed change

planning/      the Engineering Planner: plan compilation, graph
               validation, the operator customization registry and
               role-to-profile resolution. No scheduler, no policy
               engine, no authority, and no provider names.

runtime/       the operational layer: scheduler, journal, Git, forge,
               providers, supervisor, plan reconciler. Provider-specific
               knowledge lives here and nowhere below it.
cmd/           composition root: builds real components, translates
               outcomes into exit codes, owns no orchestration
```

No package under `domain/`, `policy/`, `authority/`, `evidence/`,
`reassessment/` or `planning/` mentions Codex, Claude, Gemini, Qwen or OpenAI.
That is the boundary the provider work was measured against, and the planner is
measured against it too: it reasons over vendor families and invocation modes
that `runtime/` projects for it, never over a provider's name.

## Related documents

- [`../ROADMAP.md`](../ROADMAP.md) — where this sits, and what each milestone adds
- [`supervisor.md`](supervisor.md) — what `serve` owns and refuses
- [`agents.md`](agents.md) — the workers, their trust modes and provenance
- [`planning.md`](planning.md) — profiles, templates, plans and approval
- [`spec/planning-v0.1.md`](spec/planning-v0.1.md) — the normative planning artifacts
- [`architecture.md`](architecture.md) — the Authorization Kernel
- [`principles.md`](principles.md) — architectural and construction principles
