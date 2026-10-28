# Final-integration feature acceptance

An operator can require that a feature's own browser acceptance tests pass on the
**final integrated candidate** of an objective, without asking the same of the
early work units that cannot pass them yet. The requirement is the operator's, the
tests come from the trusted base, the evidence is specific to the feature, and a
failure holds the objective for a governed decision instead of retrying or
refunding anything. It adds no scheduler, no plan-level evidence store and no
second authority system: it is one declared fact, one claim and one rule on top of
the browser machinery in `docs/browser-acceptance.md`.

Nothing here activates by itself. The runtime recognizes the pieces; a policy
rule the operator adds (below) is what makes a contract require the claim.

## The contract

1. **The operator declares the feature** in the governed planning request:
   `--acceptance-feature <id>` on `plan`/`objective` commands, or
   `acceptance_feature` on the dashboard's plan-propose API. The id matches
   `^[a-z0-9][a-z0-9-]{0,47}$` and names `browser-acceptance/<id>.txt`.
2. **The compiler binds it.** `planning.Compile` stamps it on the one *final*
   `integration_compose` stage: the integrator no other integrator consumes. A plan
   with no such stage, or with several, is refused before approval. A planner
   proposal that carries `acceptance_feature` is refused, and a re-proposal that
   does not restate the feature inherits the previous revision's, so a planner can
   neither invent, remove nor replace the operator's requirement. The declaration
   is part of the plan digest; plans that declare none keep their exact documents
   and digests.
3. **Capacity.** The stage's *effective* input set - the units
   `compileObjectiveGraph` gives its WorkGraph unit, not the literal `depends_on` -
   may have at most **19** units, and none may be named `none`. Both are refused at
   proposal time. The attribution decision (below) is a closed menu of at most 20
   options: the inputs and the reserved `none`. The ordinary WorkGraph capacity is
   unchanged.
4. **The run's origin carries it.** The activated unit's `BatchOrigin` copies the
   feature from the digest-pinned plan stage, and it is part of that batch's
   identity only when non-empty. Whenever the origin is loaded it is verified
   against the plan the graph pins; a disagreement stops the run
   (`origin.acceptance_feature`).

## The fact, the claim and the rule

From a verified origin that declares a feature - never from candidate paths - the
runtime produces the fact `acceptance.final_integration = true` for that run only,
at both the predicted and the observed compilation (one analyzer serves both, so
reassessment cannot lose it). A run without a declaration produces nothing and
compiles exactly as before; intermediate units never see it.

The operator activates the requirement with one policy rule, like any other:

```json
"FEATURE-FINAL-INTEGRATION-001": {
  "when": { "fact": "acceptance.final_integration", "equals": true },
  "effect": {
    "required_claims": { "claim-feature-acceptance": { "evidence_class": "automated_test", "independent_from_change_producer": true } },
    "obligations": { "feature-acceptance": { "statement": "The declared feature must pass its acceptance tests on the exact integrated candidate.", "required_claims": ["claim-feature-acceptance"], "material": true } }
  }
}
```

The obligation is material, so every protected action - including publication - is
held while the claim is missing, failed or stale. The rule carries no authority
condition of its own.

## The evidence

`claim-feature-acceptance` is discharged only by a `feature_acceptance` record: the
browser run, with the feature's manifest `browser-acceptance/<id>.txt` and the
base's test surface both read from the **trusted base** (the plan's base, so the
feature's tests must be merged before the plan is created), against the exact
integrated tree, in the same network-less sandbox. The record binds the feature,
commit, tree, base, checker revision, selection, surface, environment and artifact
digest. It is a different kind from `browser_acceptance`, so neither record can be
read as the other, and generic green assurance proves nothing about it. The
assurance binding gains `|feature-acceptance@…` over the feature, base and checker,
so a changed base or checker plans the check again. A missing, empty or invalid
feature manifest, or a base the run cannot name, refuses verification.

## A judged failure is a hold

A feature test that ran and failed is `FailureFeatureAcceptance`, routed
`RouteHold`. It is a recorded verdict:

- The assurance operation succeeds as a verdict and its attempt is **not** given
  back (`RestoreAttempt` is never called), and the verification stays charged to
  the assurance meter. A wait route would refund both.
- It is not retried: the same deterministic tree is never verified again, and a run
  that recomposes to a tree a prior epoch of the same unit already failed records
  that prior verdict (`identical_failed_candidate:<run>`) without spending assurance.
- The run rests as `waiting / feature_acceptance_failed`, never as
  `goal_state_reached`. The reason is an external wait: it does not spend the run's
  wall budget while an operator decides.
- An inconclusive run, a cut-off verification, an exhausted budget and a missing
  prerequisite are not verdicts and keep their own classes.

## Attribution, authorization and reintegration

A failure of the integrated whole names no upstream input, so the runtime does not
choose one.

1. It admits a system `decision_request` (purpose `feature_attribution`) in the
   integrator's scope. Its closed option set is the integration's consumed input
   units plus `none`, frozen at admission; the body holds only the feature, commit
   and tree. If the scope has no room, nothing is asked and the problem is reported.
2. The operator resolves it. **Selecting an input authorizes nothing.** `none`
   writes nothing and leaves the hold.
3. A selected input receives one system-authored `Finding` bound to its exact
   admitted handoff, saying the operator selected it and that no defect was
   detected. The existing delivery copies it into that unit's own scope (and refuses
   a stale handoff), and the existing `epoch_authorization` decision is raised for
   the unit. That second decision is still mandatory; its approval is the only thing
   that creates the `RemediationAuthorization`.
4. The unit's next epoch runs. When its corrected output is admitted - and only that
   exact output, not an external replacement, an unrelated dependency or a
   twice-replaced one - the integrator is invalidated and the existing automatic
   reintegration grants its next epoch. The held run counts as settled for that new
   epoch only while it is authenticated: waiting with the hold reason, a verified
   integration origin that declares the feature, and a current feature failure.
5. The new epoch composes the corrected inputs and the feature runs on the new tree.

## Limits

- Feature tests live in the same Go package and base test surface as the ordinary
  browser tests (`cmd/zenchron-engineering`, `scripts/`); the manifest is the only
  new file.
- An open attribution decision keeps the run `decision_pending`; resolve it (with
  `none` if no input is to blame) before an externally caused epoch can start.
- The dashboard API accepts the declaration; the browser form does not expose it yet.
- The impact registry (`ci/evidence/registry.json`) classifies the new sources and
  tests; the activation of the policy rule, the pinned image and the toolchain are
  separate, operator-owned steps.
