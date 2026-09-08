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

DOGFOOD_OUTCOME

## Supervision

DOGFOOD_SUPERVISION
