# Planning: profiles, templates and approved plans

`docs/agents.md` describes the workers an operator installs. This document
describes the layer above them: how an operator defines reusable engineering
agents, how Zenchron compiles an engineering outcome into an explicit plan, and
where the operator's approval sits in that path.

For the normative artifact shapes, read
[`spec/planning-v0.1.md`](spec/planning-v0.1.md). This document is the operator
surface.

## The shape of it

```text
an issue, or a stated objective
        |
        v
   plan issue 123 --template zenchron-feature
        |
        v
   proposed plan            roles, stages, dependencies, assignments,
        |                   independence obligations, gates, budget
        v
   plan show PLAN           read it
        |
        v
   plan approve PLAN --revision N --digest SHA   the boundary: nothing ran before this
        |
        v
   serve executes           ordinary EngineeringRuns, one per agent stage
        |
        v
   review and merge in GitHub, by a person
```

Three properties are worth stating before anything else.

**A plan is not authority to merge.** Approving one authorizes execution within
the policy and permission ceilings that already applied. Publication is still an
authority decision, and merge is still a person in GitHub.

**Nothing executes before approval.** There is no automatic approval in this
milestone, and adding one for convenience would remove the only boundary an
operator has before several workers start spending their subscription.

**Only `agent` stages become runs.** An `assurance_gate` is satisfied by
existing evidence, and a `human_decision_gate` by a person answering through the
existing authority boundary. Neither creates a worker run, because a run that
existed only to represent a gate would be asserting evidence nothing produced.

## Where customization lives

Operator-owned artifacts live in a directory the operator controls, named by
`planning_dir` in the operator configuration:

```json
{
  "planning_dir": "/home/operator/.config/zenchron/planning"
}
```

```text
planning/
  instructions/<id>.json   InstructionPack           model-visible instructions
  context/<id>.json        ContextPolicy             which context a role gets
  profiles/<id>.json       AgentProfile              a specialized worker
  templates/<id>.json      EngineeringPlanTemplate   a reusable process
```

It is a directory rather than inline configuration for a specific reason: a
run's identity is derived from the operator configuration digest, so instruction
prose inside that file would re-identify every run in flight whenever an operator
edited a sentence.

It is never a path inside a repository being worked on. Instruction content is
operator authority: a candidate may supply engineering context and may never
supply instruction. The in-repo `.zenchron.json` layer cannot name
`planning_dir`, define a profile, or install an instruction pack.

`planning_dir` is optional. An operator who has defined no custom agents has a
valid configuration, and plans still compile.

### The file name is the identity

`profiles/zenchron-reviewer.json` defines the profile a plan refers to as
`zenchron-reviewer`. A document that names a different `id` is refused rather
than silently renamed.

### The loader seals what it reads

Every artifact is content-addressed. The loader fills in the provenance it can
prove — the operator file it came from — and computes the digest, so an operator
never hand-writes a hash:

```text
written by the operator        filled in on load
----------------------        -----------------
instructions, capabilities,   schema_version
execution_agent, stages, ...  digest      (SHA-256 of the canonical document)
                              source      (operator_file + the exact path)
                              revision    (defaults to "1")
                              version     (defaults to 1)
```

An artifact that *does* state a digest must match, so an operator who wants to
pin content can.

## InstructionPack

Model-visible instructions that specialize a profile. They reach the worker as
trusted text, labelled as operator-owned, alongside the runtime's own
instructions.

`instructions/security-review-core.json`:

```json
{
  "instructions": [
    "Review the candidate diff for authentication, authorization and secret-handling defects.",
    "State every finding with the exact file and line it applies to.",
    "Do not modify the candidate: this profile reviews, it does not implement."
  ]
}
```

A pack is operator-owned configuration. A file inside a candidate that resembles
`AGENTS.md`, `CLAUDE.md` or a prompt is ordinary untrusted content, and the
adapters suppress it where the provider supports doing so.

## ContextPolicy

Which classes of context an assignment receives. It narrows; it never grants.

`context/independent-review.json`:

```json
{
  "exclude": ["producer_reasoning"]
}
```

An inclusion list is also possible, and must still carry the required classes:

```json
{
  "include": [
    "objective",
    "acceptance_criteria",
    "obligations",
    "permissions",
    "prohibitions",
    "candidate_diff",
    "policy_excerpts"
  ]
}
```

These five classes are always delivered and cannot be excluded:

```text
objective  acceptance_criteria  obligations  permissions  prohibitions
```

They carry the governance envelope, and a worker that cannot see its obligations
is a worker nothing can hold to them. A policy that excludes one is refused when
it is loaded, not quietly ignored when work starts.

`producer_reasoning` — the implementation worker's own reasoning transcript — is
a named class precisely so it can be refused. A policy that asks for it is
rejected: an independent reviewer never inherits the producer's hidden
reasoning, because that is what makes the review independent.

What a downstream stage DOES receive is the upstream change itself. A stage that
depends on completed producer stages is given their exact commit and tree and
the diff they produced, delimited as untrusted data and framed by the
runtime-owned instructions like every other piece of candidate-derived text.

Where those producers PUBLISHED a candidate, the stage's own workspace is cloned
at that exact commit, so a reviewer opens the tree that contains the change.
Where they did not - the work exists only in another run's local workspace,
which the governed remote cannot serve - the stage runs at the trusted base and
the diff is what carries the change to it. Either way the review is about the
change and carries none of the producer's reasoning. A diff too large for the runtime's bound is
truncated and says so, and a diff the runtime could not read says that rather
than appearing as an empty change.

## AgentProfile

A reusable custom agent, built over a worker the operator already registered in
`agents`.

`profiles/zenchron-security-reviewer.json`:

```json
{
  "version": 3,
  "execution_agent": "claude",
  "model": "sonnet",
  "capabilities": ["repository_analysis", "security_review", "verification"],
  "instructions": ["security-review-core"],
  "context_policy": "independent-review",
  "trust_requirement": "operator_trusted",
  "constraints": {
    "deny_permission_bypass": true
  }
}
```

One execution agent backs as many profiles as an operator wants:

```text
claude                          codex
├── zenchron-architect          ├── zenchron-builder
├── zenchron-security-reviewer  └── zenchron-tester
└── zenchron-release-reviewer
```

Adding a profile changes no Zenchron code. It changes no scheduler, no kernel
and no domain type.

### What a profile may do, and may not

```text
may                                    may not
-----------------------------------    -----------------------------------
narrow the capability set              advertise a capability the worker
                                       does not hold
add operator-owned instructions        install an executable
select a supported model of the        introduce a provider or a credential
  same worker
select context within the required     remove required context
  classes
tighten wall time and attempts         raise any ceiling
refuse a permission bypass             grant one
                                       raise trust to `protected`
                                       grant publication or acceptance
                                       authority
                                       suppress an EngineeringPolicy
                                       obligation
                                       reset consumed budget
```

The law is one sentence: **customization may specialize or narrow; it may not
escalate.** Several of those refusals are unrepresentable rather than checked —
there is no constraint member that raises a ceiling, and no artifact source class
for a repository — and the rest are refused when the registry is loaded or when a
profile is bound to its worker.

A profile that requires `protected` trust while its worker's adapter is
`operator_trusted` is refused. It is not a promotion of the worker; it is a
profile that could never be eligible, and saying so at configuration time is
better than discovering it when work is already waiting.

## EngineeringPlanTemplate

A reusable process. It is planning **input**: policy may add obligations a
template omitted, and a template may not remove one, widen trust or permission,
declare evidence sufficient, reset a budget, or authorize adoption.

`templates/zenchron-feature.json`:

```json
{
  "description": "Architecture, implementation, verification, independent review.",
  "stages": [
    {
      "id": "architecture",
      "kind": "agent",
      "role": "system_architect",
      "requires_capabilities": ["architecture_reasoning", "repository_analysis"]
    },
    {
      "id": "implementation",
      "kind": "agent",
      "role": "implementer",
      "depends_on": ["architecture"],
      "requires_capabilities": ["code_change", "repository_analysis"]
    },
    {
      "id": "review",
      "kind": "agent",
      "role": "reviewer",
      "depends_on": ["implementation"],
      "independence": {
        "dimension": "vendor_family",
        "different_from": ["implementation"]
      },
      "profile": "zenchron-security-reviewer"
    },
    {
      "id": "assurance",
      "kind": "assurance_gate",
      "depends_on": ["review"],
      "required_claims": ["claim-tests"]
    }
  ]
}
```

A template is deliberately not an execution DSL: no loops, no expressions, no
scripts, no nested templates. The single conditionality it has is `when`, which
reuses the same fact predicate the policy compiler already evaluates:

```json
{
  "id": "security-review",
  "kind": "agent",
  "role": "security_reviewer",
  "depends_on": ["implementation"],
  "when": { "fact": "authentication.boundary_modified", "equals": true }
}
```

A stage whose condition does not match is not planned, and a dependency on an
excluded stage is dropped rather than left waiting for something that will never
happen.

`profile` on a stage is a **preference**. Policy obligations and eligibility
still decide, and a preference that is ineligible is explained rather than
obeyed.

## Independence

Independence is typed, and the dimensions are ordered by strength:

```text
agent_profile  <  execution_agent  <  provider_kind  <  vendor_family  <  human
```

Two Claude Code profiles satisfy `agent_profile` independence and do **not**
satisfy `vendor_family`. Claude reached through two adapters is not vendor-family
independent, because the adapter ids differ and the vendor does not.

When policy requires vendor-family independence and only one vendor is eligible,
Zenchron does three things and never a fourth:

```text
it surfaces an explicit blocked state naming the shortage
it offers an independent human review IF policy explicitly permits that
  substitution
it stays blocked until a genuinely eligible independent producer exists

it never silently assigns the same vendor
```

Whether a human leg may substitute is a **policy** statement, not an operator
one. Approving a plan cannot introduce a substitution the policy did not permit.

## The aggregate budget envelope

An approved plan carries a ceiling across all of its runs, on top of the
per-run budgets that remain authoritative:

```text
max_child_runs             how many EngineeringRuns this plan may create
max_concurrency            how many may be active at once, within the
                           operator's global ceiling
max_provider_invocations   total provider invocations attributable to the plan
max_wall_seconds           total ACTIVE execution wall time across the plan:
                           elapsed less what its runs spent waiting on something
                           external
max_cost_micros            optional, and usually absent
```

Cost is reported only where the provider or runtime actually reports it. A
subscription CLI reports none, and unknown stays **unknown** in the approval view
and everywhere else. It is never rendered as zero, and no currency figure is
invented.

The plan's remaining provider invocations reach a child run as a RUN TOTAL,
counted across every execution binding that run makes. It is a different
resource from `max_execution_attempts`, which bounds retries of ONE binding: a
continuation is a new binding with its own attempt allowance, so a plan
remainder carried as an attempt ceiling could be spent twice over. The total
refuses the next invocation; it never retroactively fails a run whose last
permitted invocation finished the work.

Consumption is derived from the durable journal rather than kept as a counter,
so it survives a restart:

```text
child_runs  provider_invocations  wall_seconds  cost_micros (+ cost_known)
```

Child runs, provider invocations and wall seconds are all attributed as they
happen - on every pass, not only when a stage settles, because a stage that
reached its goal state is not finished spending: reviewer feedback re-activates
its run - and enforced before a stage starts.

One case is knowingly imperfect and stays that way: a crash between a provider
invocation and the append that records it loses that one count. Recording an
intent before invoking would trade a lost count for a phantom one, and a
phantom is worse - it spends an operator's ceiling on work that never ran. A plan that has spent its envelope
starts nothing further and says which dimension stopped it; a stage already
running is left alone, because a plan ceiling is not a per-run timeout and the
run has one of its own.

> A plan revision, reassignment, restart or decomposition step may not reset
> already-consumed plan or run budget.

A revision may tighten what remains. Widening requires the same explicit operator
authority that could have granted the ceiling in the first place — never a
planner, profile or template statement.

## The operator commands

```text
zenchron-engineering autonomy plan issue 123 [--template zenchron-feature]
                                             [--agent claude] [--deterministic]
zenchron-engineering autonomy plan show PLAN [--revision N] [--text]
zenchron-engineering autonomy plan approve PLAN --revision N --digest SHA256 [--note "..."]
zenchron-engineering autonomy plan reject  PLAN --revision N --digest SHA256 [--note "..."]
zenchron-engineering autonomy plan revise PLAN [--template ...] [--deterministic]
                                               [--substitute-human <stage>]
zenchron-engineering autonomy plan status PLAN [--text]
zenchron-engineering autonomy plan list [--text]
```

`plan issue` compiles a proposal and stores it. It does not execute it.

By default the decomposition is REASONED by a registered execution agent in its
own non-mutating mode: `--agent` selects which one, and an agent whose adapter
cannot prove such a mode is refused with that reason rather than run
permissively. `--deterministic` compiles the same obligations with no model
invocation at all, and the approval view says which of the two produced the
plan.

`plan show` is the approval view: the decomposition and its rationale, the
resolved assignments with the profile and worker each stage would use, the
candidates that were considered and why they were rejected, any blockers, and the
budget envelope with known and unknown fields distinguished.

These commands work while `serve` is running, which is when they matter most:
`serve` is what emits a decomposition proposal and then waits for an answer.

- **Reads** - `show`, `status`, `list` - open the durable store and take no
  runtime ownership. Reading a plan is not an act that owns anything.
- **Decisions** - `approve`, `reject`, `revise` - go to the supervisor that owns
  the work, over the same owner-only control endpoint `drain` and `shutdown`
  use, and are applied under the same lock the plan reconciler holds. One writer
  applies them against the state it is reconciling. With no supervisor running,
  this terminal is the owner and decides directly; the output is identical
  either way, so an operator cannot tell which process applied their decision.
- **Proposals** - `plan issue N` and `plan revise` - go to the supervisor too,
  first one included. The local path would take the exclusive ownership lock
  `serve` already holds, so an operator could once decide plans while `serve`
  ran but not start one.
- `plan show --revision N` renders one exact revision. The default is the
  governing one, which is not always the revision being ASKED about: a
  decomposition proposal is stored as a new unapproved revision while the
  approved one keeps executing.

  A revision that is not governing is shown as a PREVIEW: the state beside each
  stage is what approving it WOULD leave, not what is happening now. It is built
  by the same computation approval itself applies, so a stage the revision
  changes reads as pending and re-resolves, an unchanged completed stage keeps
  its work, and the view names the governing revision and lists the stages
  approving would redo. Nothing is written, and the governing view is unchanged
  by looking.

`plan approve` records the operator's decision and produces the approved
immutable revision. It NAMES the revision and digest being decided, and
`plan show` prints the exact command: a plan can gain a new unapproved revision
between reading it and deciding - `serve` stores a decomposition proposal as
one, on its own - and a decision that just took "whatever is newest" would
approve something nobody read. `plan reject` records the refusal. `plan revise` produces a
new proposal rather than editing an approved plan in place, because an approved
revision is immutable and the work bound to it stays bound to what was approved.

### Work whose input moved

A completed stage is invalidated when the upstream work its frozen assignment
names has been replaced - a producer at `goal_state_reached` is not finished,
and reviewer feedback can move it to a different candidate. Everything
downstream of that stage is invalidated with it. This is what stops a gate from
being re-proved by an independent review that was performed on work nobody is
proposing any more.

The stage is NOT performed again under the same revision, and the plan says so
rather than pretending: a stage's run identity is fixed within a revision, so
starting it again would adopt the very run whose work was marked unusable.
The stage reports as blocked, naming what moved, and a revision is what has the
work done again - because a revision is what produces a different run.

`plan revise --substitute-human STAGE` is the operator acting on an independence
shortage that policy permits a person to fill: the blocked worker stage becomes a
human decision gate answering the contract's own independent claims. It applies
nothing — it proposes a revision, which needs the same approval as any other, and
it is refused where policy did not permit the substitution. The gate records the
role it stands in for, so the obligation stays checkable rather than quietly
disappearing with the stage.

`plan status` reports stage and gate state for one plan, `plan list` lists every
plan, and global `autonomy status` shows plan state — awaiting_approval,
executing, blocked, completed, rejected — alongside the child runs. Where a
newer revision is proposed while an older one is executing, the fleet view shows
both, as `2<3`: revision 2 governs, revision 3 is waiting for you.

The shared flags are the ones every `autonomy` command takes: `--repo`,
`--config`, `--text`, and `--agent` where an agent is being selected.

## Revision and decomposition

Plans are revisioned immutable artifacts, not mutable conversational state.

```text
current approved revision
        |
        v
planner-role decomposition stage        runs in a NON-MUTATING provider mode
        |
        v
PlanRevisionProposal                    a proposal, never a child plan
        |
        v
deterministic validation
        |
        v
operator approval
        |
        v
new immutable approved revision
```

A proposal is made against an exact source revision and digest. One made against
a superseded revision is refused rather than rebased.

A material change — stages, dependencies, assignments, budgets, trust or
independence — requires a fresh approval before affected execution continues.
Unaffected stages carry on, and a completed stage's output is reused only where
its exact identity and obligations remain valid under the new revision.

There is no plan nesting. A stage cannot contain stages, and a template cannot
declare a `plan` stage kind; both are refused by the schemas rather than by
convention.

## How planner reasoning runs

Planner reasoning is not a hidden model client. It runs through a registered
execution agent, in that provider's own enforceable non-mutating mode, against a
runtime-owned planning workspace built from the exact trusted snapshot:

```text
Codex CLI      --sandbox read-only
Claude Code    --permission-mode plan
Qwen CLI       --approval-mode plan          (not live-qualified)
Gemini CLI     no provable non-mutating mode  -> ineligible for planning
```

Each mode is probed against the installed binary before it is relied on, exactly
as every other flag this runtime depends on is. A provider that cannot enter the
mode is **ineligible**: the resolver selects another eligible planner or produces
a typed blocked state, and the invocation is never downgraded into a mode that
could write.

After the invocation, the runtime verifies the planning workspace is unchanged
itself, rather than trusting the provider's claim. The planning invocation
receives no candidate-write authority, no publication credential and no
acceptance authority, and the controller checkout is never its workspace.

Reasoning output is a **proposal**. Deterministic code enforces permissions,
trust ceilings, capabilities, policy obligations, independence, budgets and graph
validity, and refuses a proposal that violates any of them however confidently it
was recommended.

## Related documents

- [`spec/planning-v0.1.md`](spec/planning-v0.1.md) — the normative artifacts
- [`agents.md`](agents.md) — the workers a profile specializes
- [`supervisor.md`](supervisor.md) — what `serve` owns, including the reconciler
- [`product-architecture.md`](product-architecture.md) — where this sits
- [`configuration.md`](configuration.md) — the operator configuration file
