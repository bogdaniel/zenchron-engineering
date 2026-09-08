# Engineering Planning v0.1

This specification defines the durable artifacts of the M2 Engineering Planner
(#64): the layer that turns engineering intent into an explicit, operator-
approved `EngineeringPlan` executed through the existing #63 runtime.

It is normative for the JSON shapes under `schemas/*.schema.json` and for the Go
types in `domain/plan.go`. Behaviour lives in `planning/` (compilation,
resolution, validation) and `runtime/` (persistence, reconciliation); this
document defines what the artifacts mean.

## What this layer is not

The planner adds no second runtime. There is exactly one scheduler, one task
database, one event journal, one policy system and one authority system in this
product, and they are the ones #63 and the Authorization Kernel already own.

Explicitly out of scope here, with their owning issues:

- external executable ExecutionAgent adapter protocol — **#103**;
- Repository Intelligence / ProjectModel v2 — **#67**; ContextPack v0 consumes
  ProjectModel v1 facts as they are and grows no repository-analysis graph of
  its own;
- pack/adapter distribution and marketplaces — **#74**;
- learned model-quality routing — **#70**;
- continuous roadmap autonomy — **#69**.

## Frozen separations

```text
EngineeringRole  !=  AgentProfile  !=  ExecutionAgent
EngineeringPlanTemplate  !=  EngineeringPlan
EngineeringPolicy  !=  ContextPolicy
```

- **EngineeringRole** is a responsibility a plan requires: `implementer`,
  `security_reviewer`, `integrator`. It names no worker.
- **AgentProfile** is an operator-defined specialization of a registered
  worker: instructions, context policy, effective capabilities, tighter
  constraints.
- **ExecutionAgent** is the installed worker itself — the #63 agent id, its
  provider kind, trust mode and readiness.
- **EngineeringPlanTemplate** is reusable planning input.
- **EngineeringPlan** is a validated, approved, immutable revision.
- **ContextPolicy** selects context. It grants no authority and discharges no
  obligation; `EngineeringPolicy` remains the only obligation system.

## Artifact catalogue

| Artifact | Schema | Purpose |
| --- | --- | --- |
| `InstructionPack` | `instruction-pack.schema.json` | operator-owned model-visible instructions |
| `ContextPolicy` | `context-policy.schema.json` | operator-owned context selection |
| `AgentProfile` | `agent-profile.schema.json` | reusable custom agent over a registered worker |
| `EngineeringPlanTemplate` | `engineering-plan-template.schema.json` | reusable process, planning input only |
| `EngineeringPlan` | `engineering-plan.schema.json` | approved immutable plan revision |
| `AgentAssignment` | `agent-assignment.schema.json` | one resolved executable stage |
| `PlanRevisionProposal` | `plan-revision-proposal.schema.json` | proposed replacement revision |

Role, capability, independence and gate obligations are **not** a new artifact.
They are new effect members on the existing `EngineeringPolicy` schema
(`engineering_requirements`) compiled by the existing policy compiler into the
existing `EngineeringWorkContract` (`plan_requirements`).

JSON is canonical. There is no YAML source of truth.

## Identity and digests

Every operator-owned artifact and every plan revision carries a `digest`: the
SHA-256 of its own canonical (RFC 8785) document with the `digest` member
cleared. `domain.ContentDigest` is the only implementation.

An `AgentAssignment` freezes the identities that produced the work: profile
id/version/digest, each InstructionPack id/revision/digest, the ContextPolicy
identity, the underlying agent binding and the effective capability, trust and
invocation information. Editing profile v3 into v4 therefore cannot rewrite an
approved plan or a live assignment; it affects new proposals only.

## Roles and capabilities

The v0 role catalogue is closed:

```text
product_architect  system_architect  planner      implementer  tester
security_reviewer  reviewer          integrator   release_reviewer
```

The v0 capability ontology is deliberately small:

```text
requirements_analysis  architecture_reasoning  repository_analysis
code_change            verification            security_review
```

A capability id is added when it creates a real eligibility distinction or
carries a policy obligation. At M2, resolver discrimination comes mostly from
trust eligibility, invocation-mode support, availability and independence rather
than from a quality score, and that is stated rather than disguised.

The built-in `planner` role is a bounded responsibility a registered worker may
perform. It is not the Engineering Planner component, and its durable output is
a `PlanRevisionProposal`.

## Stage kinds

```text
agent                only this kind may become an EngineeringRun
assurance_gate       satisfied from existing evidence/authority state
human_decision_gate  satisfied from the existing human authority boundary
```

Gates create no worker runs and no fake evidence. They order the graph and
reference durable state that other machinery already owns.

## Invocation modes

```text
mutating                 ordinary bounded producer execution
non_mutating_planning    reasoning with no write authority over the workspace
```

A stage states the mode it requires. A provider that cannot enter the required
mode is **ineligible** for that stage; it is never degraded into the more
permissive mode. Planner-role stages require `non_mutating_planning`, and the
runtime independently verifies that the planning workspace was unchanged after
the invocation rather than trusting the provider's claim.

## Independence

Independence is typed, and the dimensions are ordered by strength:

```text
agent_profile  <  execution_agent  <  provider_kind  <  vendor_family  <  human
```

Two Claude Code profiles satisfy `agent_profile` independence and do **not**
satisfy `vendor_family`. Claude reached through two adapters is not vendor-family
independent because the adapter ids differ.

`human_substitution_permitted` is a **policy** statement. Where policy sets it,
an independent human review may substitute for an unavailable independent
worker; where policy does not, no operator approval can introduce it. With one
eligible vendor and a vendor-family requirement, the plan surfaces an explicit
blocked state naming the shortage rather than silently assigning the same vendor
or stalling without explanation.

## Customization law

> **Customization may specialize or narrow. It may not escalate.**

An `AgentProfile`, `InstructionPack`, `ContextPolicy` or
`EngineeringPlanTemplate` cannot raise provider trust, widen filesystem, network
or secret access, raise permission or budget ceilings, grant publication or
acceptance authority, suppress an `EngineeringPolicy` obligation, reset consumed
budget, or claim isolation the provider cannot prove.

Instruction content is operator-owned authority configuration. Repository or
candidate content may be ordinary untrusted engineering context and may never
become privileged agent instruction: `ArtifactSource.type` has only operator
classes, and a document claiming a repository source is refused by the schema.

A `ContextPolicy` may narrow optional context and may never remove the required
classes:

```text
objective  acceptance_criteria  obligations  permissions  prohibitions
```

`producer_reasoning` is a named class precisely so it can be refused: an
independent reviewer never inherits the implementation worker's reasoning
transcript.

## Plan budgets

An approved plan carries an aggregate envelope on top of the per-run budgets
that remain authoritative:

```text
max_child_runs            max_concurrency          max_provider_invocations
max_wall_seconds          max_cost_micros (optional)
```

`max_cost_micros` is optional because most configurations cannot report cost.
Unknown cost stays unknown; it is never rendered as zero, and no currency figure
is invented.

Consumption is a projection of the durable journal, never a stored counter:

```text
child_runs  provider_invocations  wall_seconds  cost_micros (+ cost_known)
```

`child_runs` and `provider_invocations` are attributed by the reconciler and
ENFORCED against the envelope before a stage starts. `wall_seconds` is carried
and validated against the operator ceiling at approval, but nothing yet
attributes elapsed provider time to a plan, so consumed `wall_seconds` reads 0
and that ceiling is not enforced during execution. It reads 0 rather than
pretending: an unenforced ceiling that looked enforced would be worse than one
that reads as unattributed.

> **A plan revision, reassignment, restart or decomposition step may not reset
> already-consumed plan or run budget.**

A revision may tighten the remaining envelope. Widening it requires the same
explicit operator authority that could have granted the ceiling originally, and
never a planner, profile or template statement.

## Revision and decomposition

Plans are revisioned immutable artifacts:

```text
current approved revision
        -> planner-role decomposition stage (non-mutating)
        -> PlanRevisionProposal
        -> deterministic validation
        -> operator approval
        -> new immutable approved revision
```

A proposal is made against an exact source revision and digest; a proposal
against a superseded revision is refused rather than rebased. Material changes -
stages, dependencies, assignments, budgets, trust or independence - require a
fresh approval before affected execution continues. Unaffected stages continue,
and completed stage outputs are reused only where their exact identity and
obligations remain valid.

There is no plan nesting: a stage cannot contain stages, and a template cannot
declare a `plan` stage kind. Both refusals are in the schemas.

## Approval

A proposed plan never executes because it parsed. The lifecycle is:

```text
intent -> proposed -> validated -> operator approval -> approved revision
       -> plan reconciler in serve -> dependency-ready agent stages
       -> ordinary #63 EngineeringRuns -> assurance/authority -> external human
```

Approval is durable operator authority to execute **within existing policy and
permission ceilings**. It is not merge, release or acceptance authority, and
there is no automatic approval in this milestone.
