# M2 dogfood: Zenchron planning and executing a Zenchron change

This is the record of #64's bounded self-building acceptance: a real Zenchron
issue, planned by a registered reasoning agent, executed through ordinary #63
runs under an operator-approved plan, with an independent review by a different
vendor.

It is a RECORD, not a demonstration. What went wrong is here too, because what
went wrong is most of what the exercise was for.

## Setup

The operator layer, in an operator-owned directory, with no core code change:

```text
~/.zenchron-m2/config.json          state_dir, planning_dir, plan budget ceiling
~/.zenchron-m2/planning/
  instructions/zenchron-implementation.json   how this repository is changed
  instructions/zenchron-review.json           how a change to it is reviewed
  context/independent-review.json             excludes producer_reasoning
  profiles/zenchron-builder.json              -> codex   (openai)
  profiles/zenchron-reviewer.json             -> claude  (anthropic)
  templates/zenchron-docs-change.json         implement -> review -> assurance
```

The registered workers are the two the operator already had: `codex` (Codex CLI)
and `claude` (Claude Code), both `operator_trusted`. Nothing in `runtime/`,
`domain/` or `policy/` knows any of the four artifacts above exists.

## What the planner produced

`autonomy plan issue N --template zenchron-docs-change --agent claude` ran Claude
Code in its own `plan` permission mode against a runtime-owned workspace
materialized at the exact trusted base. The runtime measured that workspace
before and after:

```json
{"agent_id":"claude","provider_kind":"claude_code","vendor_family":"anthropic",
 "invocation_mode":"non_mutating_planning","provider_mode":"plan",
 "workspace_unchanged":true}
```

The plan it produced, after policy obligations and deterministic validation:

```text
implementation   agent            role=implementer  profile=zenchron-builder  agent=codex  (openai)
review           agent            role=reviewer     profile=zenchron-reviewer agent=claude (anthropic)
                                  independent-of=implementation in vendor_family
assurance        assurance_gate   references claims claim-validation
```

The resolver's own record of why the review went to Claude and not to Codex:

```json
{"considered":[
  {"profile":"zenchron-builder","agent":"codex","eligible":false,
   "reasons":["profile does not advertise security_review",
              "vendor family \"openai\" is not independent of stage \"implementation\" in dimension \"vendor_family\""]},
  {"profile":"zenchron-reviewer","agent":"claude","eligible":true}],
 "selected":"zenchron-reviewer"}
```

Nothing executed until `autonomy plan approve`.

## What the deterministic layer refused, live

Four refusals happened before any work started, each from a different rule:

1. **Budget.** The first proposal needed five child runs against an operator
   ceiling of three. Refused; raising it required editing the operator
   configuration, which is the only path that can widen a ceiling.
2. **A gate that proved nothing.** A proposed `assurance_gate` named no claims.
   Refused. The compiler now completes such a gate from the contract's own
   required claims - an addition, never a removal - and refuses still where the
   contract requires nothing.
3. **An unrecognized member.** An answer carried a field outside the stated
   contract. Refused rather than partially read.
4. **An unbindable independence obligation.** A proposed independence
   requirement named no stages. Refused. The compiler now binds it to the
   material producers, exactly as it binds a policy obligation.

## What the live run found in the implementation

Three defects in this branch were found by running it, not by testing it:

- **The plan reconciler's engines had no customization registry.** The first
  stage run failed terminally with "instruction pack zenchron-implementation is
  no longer installed" about a pack that was installed the whole time. Fixed by
  loading the registry once in the composition root and handing it to every
  engine; a test now asserts that for every configured agent.
- **`serve` reconciled plans after reading the run list**, so a stage that became
  dependency-ready had its run created and then waited a full poll interval to be
  driven. Fixed; a test asserts the plan's runs are driven in the tick that
  created them.
- **A review stage was based on the trusted branch.** The independent reviewer
  opened its workspace, found the base tree, and reported *"the candidate tree
  contains no change"* as a blocking finding about the tree it had been given. It
  was right. A stage whose dependencies published a candidate is now based on
  that candidate.

The third one is the reason to run a dogfood at all: every deterministic test
passed while the reviewer was reviewing the wrong tree, because no test asserted
what a reviewer's workspace should contain.

## Outcome

The last exercise ran end to end on issue #77, on the branch under review,
with no intervention between approval and the satisfied gate.

```text
plan plan-92eea8c0b1a7e75858e8093f9d25855c revision 1 (approved)
planned by: claude (claude_code, anthropic) in non_mutating_planning mode; workspace verified unchanged: true
template: zenchron-docs-change v1
stages:
  implementation   agent            completed   role=implementer profile=zenchron-builder  agent=codex  (openai)
  review           agent            completed   role=reviewer    profile=zenchron-reviewer agent=claude (anthropic)
                                                independent-of=implementation in vendor_family
  assurance        assurance_gate   satisfied   references claims claim-validation
budget: child runs 2/6, provider invocations 2/14, concurrency ceiling 2
cost: unknown - no configured provider reported one, and unknown is not zero
```

From the plan's own journal, in order:

| time (UTC) | event |
| --- | --- |
| 12:42:03 | `plan_proposed`, `plan_validated` - the planner's answer compiled and passed deterministic validation |
| 12:42:11 | `plan_approved` by the operator, 8 seconds later |
| 12:42:17 | `stage_assigned` implementation -> codex, `run_started` `run-7bce...`, `budget_consumed` |
| 12:45:22 | implementation `stage_completed` (`goal_state_reached`); review assigned to claude and started in the same tick |
| 12:50:18 | review `stage_completed`; `gate_satisfied` on `claim-validation` |

Three minutes five seconds of implementation, four minutes fifty-six of
independent review, eight minutes fifteen from proposal to satisfied gate. The
planning invocation itself is not separately instrumented - the plan's first
journal event is the proposal.

What it produced: [PR #105](https://github.com/bogdaniel/zenchron-engineering/pull/105),
`docs/troubleshooting.md` +32/-3, no Go change, CI `go` green on the exact head.
The assurance gate is satisfied by the evidence of the implementation run's own
verification, at revision `7aed24f9f809d98f9743ad1314b6149685769f40`.

The review is the part worth reading. The independent reviewer - a different
vendor, working from the PUBLISHED candidate rather than the base - checked each
acceptance criterion against the code the documentation describes, raised four
findings (an incompletely stated containment rule that `doctor` does not catch, a
bind mount the prose does not admit is writable, a diagnostic string presented as
a gate, and two hardening flags the rest of the system sets), and then wrote
this:

> I could not execute `gofmt -l .`, `go vet ./...`, or `go test ./...` - the
> sandbox refused each of those commands, including with the sandbox override, so
> I have **no executed evidence** that they pass and I am not claiming they do.

It went on to argue from the diff why those commands' outcomes must equal the
base's, and marked the argument as an argument. That is the behaviour the
independence obligation exists to buy: a second worker that will not certify
what it did not run. Nothing here judged the findings for it - they are on the
pull request for a person.

This is one plan, one trivial documentation case. It shows the path works end to
end; it shows nothing about leverage.


## Supervision

Honest accounting, because the number that flatters is the one that leaves out
the failed attempts.

**The successful #77 plan, proposal to satisfied gate:** one operator decision
(the approval), zero interventions, 8m15s wall clock. No prompt was carried by
hand: the objectives, the profiles, the diff the reviewer read and the base it
read it from were all produced by the system.

**The whole exercise that reached it:** heavy supervision, and it earned its
keep.

| supervision | what it cost | what it bought |
| --- | --- | --- |
| 4 deterministic refusals before any execution | 4 reviews of a refusal message, 1 operator config edit to raise a ceiling | budget ceiling honoured; a gate that proved nothing, an unrecognized field and an unbindable obligation all refused rather than executed |
| 3 defects found by running it | 3 fix commits with regression tests | a missing registry, a wasted poll interval, and a reviewer reading the wrong tree - none of which any test in this branch caught |
| 1 restart mid-plan | one `serve` kill and relaunch on a new binary | recovery proven live rather than only in tests |
| 1 self-inflicted disruption | the exercise restarted on a fresh issue | I closed a pull request while its run was in flight; the runtime refused to continue on a candidate whose external state had changed, which is the correct behaviour and also entirely my fault |

The supervision that mattered was engineering supervision of THIS branch, not
supervision of the workers. Once the branch was correct, the plan needed one
decision.

No cost figure is claimed anywhere: both providers are subscription CLIs that
report none, and `cost_known: false` is what the runtime recorded.

