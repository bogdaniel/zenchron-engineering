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

### What an independence requirement with no peers means

An independence requirement names the stages a stage must differ from. Naming
none is a **shorthand**, and the compiler resolves it by one rule in every case:

```text
independence absent                     no obligation, and no dependency edge
independence with explicit peers        exactly those peers; each becomes a
                                        dependency, because an obligation that
                                        could resolve before the work it judges
                                        proves nothing
independence, empty different_from,     every material producer in the plan:
  on a stage that produces nothing        the reviewer's ordinary meaning
independence, empty different_from,     REFUSED, typed, before any edge is added
  on a material producer
policy-required independence            the same rule, with policy's own peers
                                        merged in and never weakened
```

The refusal is the part worth stating. The shorthand means "differ from whoever
produced the change in this plan". On a reviewer that is unambiguous: the
reviewer gains a dependency on each producer, which is the order it already had.
On a stage that IS a producer, the same expansion names its **peers** — stages
with no ordering relation to it — and each peer then becomes a dependency. Two
producers written that way depend on each other, cycle detection correctly
refuses the plan, and the operator is shown a cycle nobody proposed. That is
exactly what happened to the first real #63 + #106 dogfood.

So the compiler refuses the ambiguity while it is still legible, and says which
stage, which member and what to do instead. It does not guess which sibling
producer the stage meant, and it does not drop the requirement: an independence
obligation is never silently discarded, whoever asked for it. Policy-required
independence is only ever strengthened, never completed into peer dependencies
on a producer.

The planner's output contract states this rule to the model, which is the other
half of the fix: `independence` is an **optional** member, to be omitted unless a
stage genuinely requires independence, and the roles on which an empty
`different_from` is refused are named.

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

An approval also binds the ASSIGNMENTS it was shown. The plan digest binds the
document that was read; on its own it says nothing about who will perform it.
Resolution is otherwise recomputed from the live registry and workforce on
every look, so a stage that had not started yet resolved again when it became
dependency-ready - and an instruction pack edited in place, a profile edited,
or a changed default worker between the approval and the first run meant the
first execution froze a configuration nobody had approved. Those assignments
are now recorded when the approval is, immutably per stage, and the approval
event carries their canonical digest so the binding sits in the journal rather
than only in the table beside it. Resolution reads them back for stages that
have not started, which is the same law that already applied to stages that had.

The binding is taken against the state approving WOULD leave, not the state it
replaces - the same computation `plan show --revision N` renders. That matters
for the most ordinary flow there is: revising a plan while it executes. The
supersession an approval records resets a stage the revision materially changes
to a zero projection, lifecycle cleared and generation back to zero, so reading
the pre-approval state saw it as "already started" and left it unbound - and
nothing then governed it, because the privilege comparison engages only above
generation zero and the supersession had just taken it back to zero.

An approval NAMES the assignment set it approves. The revision digest binds the
document an operator read and does not move when a profile, an instruction pack
or the workforce is edited, so an approval that named only the document
authorized whichever assignments the registry happened to resolve at decide
time. `plan show` prints the set's digest in the command it offers, and
`plan approve --assignments SHA256` is REQUIRED and refuses when the set has
moved since - the same shape that stops a revision number alone from deciding an
unread document. A rejection binds no assignments and refuses the argument.

Requiring it applies to new approvals. An approval already durable in a journal
was written before this existed, stays readable, and keeps executing under the
compatibility rule below; what cannot happen is a NEW one being written without
naming what it authorizes.

And the approval AUTHORIZES the rows. The bound assignments live in a table
beside the journal, and a table is editable while a hash-chained event is not -
so the digest the approval recorded is replayed with the rest of the approval,
and those rows are read, digested with the same definition the approval used,
and refused unless they are the set the operator approved. Two durable sources
that can disagree are one durable source and one opportunity, and without this
the side table was the one that won. A disagreement fails closed: nothing
resolves and no run is created.

What approval cannot bind is bound when the stage starts. A stage approved
before its producer ran was approved with no upstream at all, so the upstream
outputs it consumes are rebound from the settled producer at that point, and
only where the approved assignment's own context selection says the worker is
entitled to them. Approval freezes what the operator authorized, not facts that
did not exist yet.

One identity the assignment cannot carry is the provider behind an agent id:
the engine is built from live agent configuration. An id re-pointed at another
provider kind, another vendor family or a weaker trust mode is refused before
the run is created, because otherwise an independence obligation stated in
terms of vendor family would be quietly false.

A stage that resolved to a BLOCKER at approval is not bound. The operator was
shown a blocker rather than an assignment, so there is no identity to hold
execution to, and readiness at planning time was never a promise - an agent that
authenticates an hour later performs the stage under whatever resolves then.
That is a deliberate cut rather than an oversight, so the view NAMES those
stages: `unbound` lists them, because rendering them beside bound stages
identically is what would let an operator believe an approval had covered them.
The marker is today derived from the live resolution, so it stops naming a
stage once its blocker clears - the window where it matters most - which #118
closes by deriving it from the approval record instead.

A revision approved before this boundary existed has no binding and resolves
live, exactly as it did then. That is the ABSENCE of a recorded digest, and it
stays distinct from an approval that deliberately bound nothing - which records
the canonical digest of the empty set, and can therefore be named and checked
like any other. Rows appearing under an approval that named none are refused
rather than trusted.

### Work whose input moved

The assignment a stage will actually execute is the DURABLE one. It is
persisted before the engine is built and before the run exists, and the
`plan.stage_assigned` association is appended only after the run exists - so a
start that failed in between leaves a frozen row that nothing points at, and
the row is kept on the next attempt because assignments are immutable per
revision, stage and generation. Every guard therefore applies to that row and
not to whatever the resolver produced this pass: checking the fresh resolution
meant the safety check protected an assignment that was about to be discarded,
while the one that ran was never checked at all.

A frozen assignment nothing ever ran is intent, not history. If its input has
been replaced since it was frozen, the performance it was frozen for will never
happen, and refusing it would refuse it forever - the row cannot change. So the
stage advances an execution generation instead, exactly as it would if that
performance had run and been invalidated: the abandoned row stays where it is,
the next performance is frozen against what the producer actually settled, and
the privilege boundary compares the two. A retry the producer did not outrun
re-uses the assignment it froze and burns no generation.

The one exception is a freeze whose RUN survived. The association is appended
after the run is created, so a crash between the two leaves a live run the
projection does not name, which is indistinguishable from a failed start by the
plan journal alone. Discarding that freeze would leave the run executing,
unstopped and attributed to no stage - consumed plan budget made invisible - so
the durable runs are asked, the stage is associated with the run that exists,
and the sweep below handles it from there. That run then finishes its work
against the candidate it was created for and is invalidated when it completes,
so it spends its invocations on a performance already known to be superseded.
It is the same policy any run gets when its input moves underneath it, and
stopping one here rather than at completion would be new machinery; it is
recorded because the window makes it knowable earlier than usual.

A completed stage is invalidated when the upstream work its frozen assignment
names has been replaced - a producer at `goal_state_reached` is not finished,
and reviewer feedback can move it to a different candidate. Everything
downstream of that stage is invalidated with it. This is what stops a gate from
being re-proved by an independent review that was performed on work nobody is
proposing any more.

Only a head the producer is FINISHED WITH counts as movement. A run's recorded
candidate advances on every commit and checkpoint, so a producer working through
feedback moves it long before it has produced anything a reviewer should read.
Re-performing against an interim head would pay for a review of work still being
changed, invalidate it again the moment the producer settles, and - if that
commit never became reachable - pin the new performance to a head nothing can be
based on. The same rule guards the freeze itself: a stage does not start - and
does not bind itself to an upstream head - while the run that produced that head
is back at work. That covers a first performance too, because a plan stage stays
completed while its run is re-activated.

The stage is then performed AGAIN under the same approved plan, as a new
EXECUTION GENERATION. A producer moving from candidate A to candidate B is an
execution fact, not a change to the plan: the role, profile, worker, trust
ceiling, permissions, independence requirement and topology an operator
approved are all unchanged, and the obligation - review this work - is simply
renewed. So nothing is re-planned and nobody is asked to approve anything.

A generation is its own assignment and its own #63 run: the run identity carries
it, so the new performance cannot adopt the run whose work was discarded, and
the new assignment names the candidate it will actually consume. The old run is
retired - still attributed to the plan's budget, and stopped rather than left
executing what nobody will read - and the record of what it consumed is left
exactly as it was, because that performance happened.

Where re-performing WOULD change something governance-relevant - the role, the
profile version or digest, the worker binding (its id, provider kind, vendor
family, model and trust mode), the invocation mode, the independence obligation
- the stage is blocked instead, naming what would change. That is a different
obligation from the one approved, and it goes through the approval boundary as a
revision. Obligation renewal may be automatic; authority change may not.

The comparison is STRUCTURAL. What a generation may move is stated - which
performance it is, the run it became, the resolver's explanation, the upstream
candidate whose replacement caused it, and a budget that narrows - and
everything else in the frozen assignment has to be identical, compared as one
canonical record rather than as a list of fields somebody remembered. That
matters because a profile document names its instruction packs and context
policy by id while their CONTENT digests are frozen separately: editing a pack
in place leaves the profile id, version and digest untouched, and a field-by-
field comparison would have let a new generation execute different operator
instructions with no approval.

A stage being performed again whose previous performance cannot be read is
BLOCKED. A later generation exists because an earlier one happened, so absence
is not evidence that the obligation is unchanged.

The comparison is against the PREVIOUS PERFORMANCE, wherever it was recorded. An
assignment is stored under the revision that governed when the stage started, so
a stage completed under one revision and carried unchanged into the next keeps
its record under the older one - the ordinary result of propose, approve,
propose, approve. Looking only under the revision governing now would find
nothing to compare in exactly the histories this boundary exists for.

That comparison therefore crosses revisions, and two of the things it compares
are POINTERS rather than obligations. The assignment carries the plan revision
and plan digest it was resolved under, and a per-revision pointer to the
compiled contract; a stage carried forward unchanged into a later approved
revision necessarily gets new values for all three, because the plan document
and the stored contract are written per revision. Reading those as a changed
obligation refused the renewal this boundary exists to permit - a stage whose
upstream had moved could not be re-performed at all once any later revision had
been approved.

So they are erased from the structural comparison and replaced by what they
point AT. The plan's durable identity has to be the same one, the renewal has
to be resolved under the revision actually executing - which is the approved
one - and to name that revision's own content, and the previous performance has
to be recorded no later than it. The
contract has to be the same contract by identity, and the compiled documents
the two revisions were planned against have to say the same thing: the whole
contract less its own revision string and the predecessor it names, compared as
one canonical record. A revision whose recompiled contract obliges anything
different - an added invariant, a changed required claim, a narrowed scope - is
a different obligation and is blocked like any other authority change. A
contract that cannot be read at all is blocked too, for the same reason an
unreadable previous performance is.

Upgrading an installation that already holds an old-style same-revision
invalidation converges after one bounded extra cycle: the stored assignments
become generation 0, the pending invalidation is re-derived once, and the stage
is then performed as generation 1 like any other.

Any movement of the upstream candidate counts, including a base integration.
A candidate that has absorbed its base is not the candidate that was reviewed,
so the reviewer did not see what is now there. This errs toward asking for the
work again rather than reusing a verdict about something else.

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

### Referenced issue context

An issue that says "resolve #110, #111 and #112" is planning input pointing at
more planning input. The planner cannot follow those pointers: a planning
invocation is non-mutating AND network-isolated, and a provider that could fetch
its own context would be a second, unreviewed trust boundary into this system.

So the controller follows them, through the same governed forge boundary that
reads the primary issue, **before** the invocation starts:

```text
pinned        each referenced issue is read once, digested, and stored as a
              local-only snapshot; nothing re-reads the forge mid-attempt
untrusted     the text reaches the model inside UNTRUSTED-SOURCE markers, with
              the standing the primary issue's text has: data describing desired
              behaviour, never an instruction and never authority
provenance    repository, issue and digest are recorded on the plan, and a
              reference that could NOT be read is recorded as such
bounded       explicit "#N" citations from the PRIMARY issue only - depth one,
              never transitive - and at most twelve of them, with the bound
              stated rather than applied silently
```

The referenced text is deliberately **not** folded into the plan objective: the
objective is what the plan document carries and what its digest is over, and a
plan is not a copy of the forge.

A referenced issue the forge could not return does not stop planning and does not
disappear. It becomes visible product state — `plan show` says which issue was
not available to the planner and why — because a plan reasoned from four of five
referenced issues is a different plan from one reasoned from all five, and the
operator deciding whether to approve it should know which they have.

### Reading the model's answer

The contract asks for the answer in a fenced json block, and the runtime reads
the **last fenced block** that parses as a proposal. A coding CLI's transcript is
mostly not JSON — it echoes Go source, test names and prose — so scanning the
whole of it for balanced braces desynchronizes on the first unmatched brace or
odd quote. On a real #119 transcript that left 29 unclosed braces, the model's
correct answer never formed a candidate at all, and the last thing that still
parsed was the output contract's own example, echoed as part of the prompt. The
runtime refused a stage called `kebab-case-id`.

A fence has no such ambiguity: it says where the answer starts and stops, and the
echoed contract is not inside one. The brace scan remains the fallback for a
model that answers without a fence.

### When reasoning does not reach a plan

A proposal the deterministic compiler refuses used to leave nothing behind on a
first plan: there was no revision document, so there was no plan, so `plan list`
said "no plans" and `plan show` said "no such plan" about work an operator had
just spent a provider invocation on. An answer the runtime could not read at all
left even less, because it never reached compilation.

Both are now a durable **planning attempt**: from an operator's chair they are
the same event — an invocation was spent and there is no plan — so they get the
same record.

```text
plan identity        a plans row with no revision. Nothing that lists or reads
                     executable work returns it, so it can never be mistaken for
                     a plan; what it gives the evidence is somewhere to hang
one journal event    plan.attempt_refused, in the plan's own append-only stream,
                     under the same hash chain every other durable fact uses
```

Because it is a journal projection rather than a stored record, an attempt read
before a restart and an attempt read after one are the same replayed facts.

`plan list` names the plan and says it has no plan; `plan show PLAN` answers,
without a single grep through an artifact directory:

```text
which attempt, and which revision it would have been
which reasoning agent produced it, under which model and mode
the runtime's own proof that the planning workspace did not change
the proposed decomposition - stages, roles, dependencies, independence -
  with an independence over nothing shown as exactly that
the typed deterministic reasons it was refused
the referenced issues the planner was given, and the ones it was not
the transcript, by path
and that NO executable EngineeringPlan exists
```

An attempt grants nothing. It is not approvable, because approval names a
revision and there is no revision; it is not executable, because execution reads
the approved revision and there is none. Proposing again is the ordinary next
step, and it promotes the same plan identity rather than colliding with it.

## Related documents

- [`spec/planning-v0.1.md`](spec/planning-v0.1.md) — the normative artifacts
- [`agents.md`](agents.md) — the workers a profile specializes
- [`supervisor.md`](supervisor.md) — what `serve` owns, including the reconciler
- [`product-architecture.md`](product-architecture.md) — where this sits
- [`configuration.md`](configuration.md) — the operator configuration file
