# Orchestration readiness: #390 gate and M2-O capability audit

Scope: #471. Audited revision `main@83b19ce29c7bcdff04666ca77fbd4c5c7b9f8162`
(merge of PR #547), the exact commit the task named. Verification ran in an
isolated worktree checked out at that exact SHA, never on a branch that mixes
in unrelated local work. Does not implement #476, #479 or #480.

**Verdict: the #390 gate is satisfied. No repair was needed.** The
quarantine fix merged by PR #410 (`80ba2e5`/`081a0cb`/`af97f25`, all ancestors
of `83b19ce`) survived the three PRs merged after it (#544, #546, #547) that
touched the same files (`runtime/operations.go`, `runtime/reconciler.go`)
unchanged in effect. This was proven live, not assumed from the diff — see
"Reproducing #390" below. Unrestricted real-provider autonomous dogfood is
**not blocked by #390** as of this revision. The one still-open item is the
batch-scope limitation on integration-conflict Finding routing, which is a
known, previously-scoped #475 follow-up, not a regression.

## Phase 1 — Capability matrix

Legend: **Implemented** = code on `main` at `83b19ce`. **Tested** = a
controlled test (real SQLite, real Git, fake/stub provider) exercises it and
passes at this revision. **Operational evidence** = an actual dogfood/live
run exercised it; absent unless stated. Do not read "Tested" as "Operational
evidence" — they are different claims.

| Capability | Implemented | Tested (this audit) | Operational evidence |
|---|---|---|---|
| WorkGraph persistence, frontier, handoff, restart | yes (#472, `orchestration/workgraph.go`, `runtime/workgraph*.go`) | yes — `TestRestartReconstructsPlanStateFromTheStore(Alone)`, `TestRestartReproducesTheFrontierWithoutDuplicateChildren`, `TestHandoffRunsEndToEnd`, `TestHandoffWritesAreConditionalOnThePhaseTheCallerRead` (all PASS) | none produced in this audit (read-only + controlled tests only) |
| Independent review, BLOCK remediation, fresh review, acceptance | yes (#474, PR #547: `runtime/review_remediation*.go`) | yes — `TestReviewRemediationEndToEndH1BlockH2Approve`, `TestAdmitReviewRemediation*` (9 variants), `TestReviewTriggerRunsOncePerTickPerRun`, `TestSupersededRetryBindingNeverInvokesTheProviderWithNoFindings` (all PASS) | docs/review-remediation.md records six independent re-review rounds (B1/B2, R1-R12) against this exact capability during its own construction |
| Parallel integration and exact subject binding | yes (#475, PR #546: `runtime/integration_dispatch.go`, `runtime/integration.go`) | yes — `TestWorkGraphIntegrationComposesTwoParallelOutputs`, `...ReleasesDownstreamOnlyAfterItsOwnHandoff`, `...RestartReproducesTheSameOutcome`, `...UnitInvalidatedWhenUpstreamOutputReplaced`, `TestAdmitIntegratedHandoffLosesToSupersededInputBeforeInsert` (all PASS) | none |
| Verification/work capacity and reviewer budgets | yes (`runtime/capacity.go`, `runtime/review_run.go`) | yes — `TestTheObservationCeilingBoundsWaitingRuns`, `TestARunHoldsAtMostOneActiveOperation`, `TestReviewBudgetIsAlwaysFinite`, `TestReviewBudgetUsesTheOperatorConfiguredEnvelope`, `TestCapacityStateSurvivesARestart` (all PASS) | none |
| Human decision wait, authorized resolution, continuation | yes (#508, PR #544: `orchestration/decision_resolution.go`, `runtime/decision_resumption.go`) | yes — `TestAWorkUnitHoldConsumesNoCapacityUntilAuthorizedResolution`, `TestAnEarlyResolutionDoesNotBypassAnUnsatisfiedDependency`, `TestSupervisorResolveDecisionEnforcesAuthorityAndIdempotency`, `TestADecisionResumptionSurvivesRestartWithoutDuplicating`, `TestExtractKindFlag` (the real `decision resolve` CLI) (all PASS) | none |
| Controller restart, provider-session independence | yes (`runtime/controller.go`, decision/review resumption) | yes — `TestAReadoptionRefusesWhatItCannotSanction` (10 subtests), `TestAReadoptionThroughTheStableEntrypointIsAccepted`, `TestReviewTriggerSurvivesSupervisorRestart`, `TestADecisionResumptionSurvivesRestartWithoutDuplicating` (all PASS) | none |
| Integration-conflict Finding routing | yes, **scoped** (PR #546, Review B1) | yes — `TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff` (PASS) | none |

**The integration-conflict Finding limitation**, named in `runtime/integration_dispatch.go:313-328`
(`refuseIntegrationConflict`): a textual conflict is admitted as a typed,
correctly-sourced, durable Finding — but into the **integration unit's own
single-item batch**, never the producer's. #473's per-batch isolation has no
cross-batch delivery primitive yet, so the Finding is visible to an operator
reading that run/batch, but is not automatically routed to the producer's own
inbox the way a review BLOCK is. This is a deliberately scoped, already-tracked
#475 follow-up (the comment explicitly forbids resurrecting reverted #543's
graph-wide mailbox to close it informally). Not a defect; a documented gap an
operator must know about before relying on automatic conflict-to-producer
routing.

## Phase 2 — #390

### What's on main

Three commits, all ancestors of `83b19ce`, merged as PR #410 on 2026-10-02:

- `80ba2e5` — failure classification becomes authoritative in
  `runtime/operations.go`: an invocation stays `Succeeded` only through the
  governed checkpoint path (#54/#328/#379); every other failed+mutated
  settlement goes through `settleRefusedMaterial`, which quarantines the
  refused paths byte-for-byte outside the candidate `.git` and restores the
  workspace to the exact pre-attempt subject.
- `081a0cb` — settles stop ownership before journalling the quarantine, and
  fails the attempt closed (never discards) on any copy/restore failure
  regardless of result shape.
- `af97f25` — adopts an orphaned quarantine left by a crash, bound to its own
  operation, before the next provider dispatch.

This is the fix PR #389/#393/#400/#408 (and the still-open PR #404, which got
`CHANGES_REQUESTED` on 2026-10-02 for exactly this gap) needed and never
landed. #404 remains open and superseded; it should be closed by the operator
as "fixed by #410," not merged — it is missing the byte-exact
`held_material.go` quarantine step this fix added.

### Reproducing the defect live, on `83b19ce`

Four PRs landed after PR #410 and before this audit (#544, #546, #547 plus
one `main` tip move), all touching `runtime/operations.go` and
`runtime/reconciler.go` — the exact two files the #390 fix lives in. The task
was to verify the fix did not regress under that churn, not take the diff on
faith.

1. Ran the existing restored-defect regression suite
   (`runtime/refused_material_quarantine_test.go`,
   `runtime/held_material_test.go`) at the exact `83b19ce` tree: all 5
   quarantine tests and all 11 held-material tests **PASS**, including
   `TestClaudeAutomaticDetachmentQuarantinesBeforeAdmission`, which replays
   the exact `provider_background_work_unresolved` + `mutated:true` shape
   from the live #388/#389 incident.

2. Proved the test isn't vacuous by breaking the guard adversarially:
   `runtime/operations.go:1553`'s
   `Checkpoint: record.Mutated && (class == FailureExecutionIncomplete || ...)`
   was changed to `Checkpoint: record.Mutated` — exactly PR #408's rejected
   defect ("#408 turns every dirty failure into `candidate.checkpointed`").
   Result: 4 of 5 quarantine tests **FAIL** with messages naming the exact
   admitted defect (`"a refused, mutating invocation settled \"succeeded\",
   want failed"`, `"automatic detachment admitted"`). The guard was reverted
   immediately after (`git diff` against the worktree is clean; nothing from
   this step is committed anywhere).

This is the live reproduction the task required: the historical defect
reproduces instantly when the guard is removed, and does not reproduce on
the actual `83b19ce` tree with the guard in place.

### The four required test scenarios

All exist and pass at `83b19ce`, none added by this audit:

1. failed mutation A → later no-change success: `TestRefusedMaterialIsNotAdmittedByALaterNoChangeSuccess`
2. failed mutation A → later success producing B: `TestRefusedMaterialIsNotAttributedToALaterSuccessThatCreatesB`
3. restart: `TestQuarantineSurvivesARestart`, `TestAnOrphanedQuarantineIsAdoptedAfterACrash`
4. retry exhaustion, preservation: `TestExhaustionWithNoMaterialHoldsNothing`,
   `TestHeldMaterialSurvivesRestartAndRenewsNoAuthority`

Budget/authority non-reset is structural, not merely untested: quarantine
reuses the existing operation/attempt identity (`settleRefusedMaterial` never
creates an operation or touches `MaxAttempts`), and `TestARestartRenewsNeitherTheAttemptNorTheRunBudget`
covers the adjacent renewal invariant.

#409 was reviewed and not resurrected, per instruction — it is an earlier,
independently-closed bootstrap attempt at the same repair; #410 is the one
that actually landed and holds.

## Phase 3 — Integrated acceptance

All runs below used the real scheduler, supervisor, controller, WorkGraph and
SQLite/Git store at `83b19ce`, in an isolated worktree, with the standard
fake/stub provider test harness (no code changed; no commits produced).

| Scenario | Test(s) | Result |
|---|---|---|
| Restart | `TestRestartResumesTheSameLogicalRun`, `TestRestartBetweenDeterministicFailureAndReconciliationDoesNotRepeat`, `TestRestartCannotOverwriteDurableAttemptEvidence` | PASS |
| Stale head / same-head review supersession | `TestReviewPortDetectsStalenessAfterTheHeadMoves`, `TestAdmitReviewRemediationRefusesAStaleSubject`, `TestReviewRemediationLiveHeadCheckCatchesAnExternalMoveWithoutAnObservationTick` | PASS |
| Capacity exhaustion | `TestTheObservationCeilingBoundsWaitingRuns`, `TestARunHoldsAtMostOneActiveOperation`, `TestAdmitReviewRemediationRefusesARunWhoseBudgetIsExhaustedButNotYetJournalled` | PASS |
| Unreadable store | `TestAnUnreadableDecisionStoreNeverAuthorizesDispatch`, `TestAnUnreadableRunIsUnavailableNotWaiting`, `TestAnUnreadablePublishedDirectoryRefusesRatherThanRebuilds` | PASS |
| Denied authority | `TestG_AuthorizingAWrongActionIsRefused`, `TestH_AFabricatedRequestIdentityIsRefused`, `TestAuthorityRefusalNeverReachesAdmission` | PASS |
| Decision resolution resumes a WorkUnit from an actual operator-authorized action | `TestAWorkUnitHoldConsumesNoCapacityUntilAuthorizedResolution`, `TestSupervisorResolveDecisionEnforcesAuthorityAndIdempotency`, `TestExtractKindFlag` (the real `decision resolve` CLI path) | PASS |
| Controller invokes independent review when correctly configured | `TestReviewTriggerRunsOncePerTickPerRun`, `TestReviewTriggerSurvivesSupervisorRestart` | PASS |

`go build ./...`, `gofmt -l .` (clean) and `go vet ./...` (clean) all ran
against the exact `83b19ce` tree. `go test ./...` (the full suite) was not
run locally — it is CI-only per repository policy and takes ~20 minutes; the
focused package-scoped runs above are the local evidence this audit produced.
GitHub's post-merge `go` check for `83b19ce` is the full-suite evidence of
record (required-check green per the `main` ruleset, per ADR-0007).

## Blockers

None for #390. The integration-conflict batch-scope gap (Phase 1) is a known,
already-filed limitation of #475, not a new blocker, and does not involve
refused/mutated material or candidate admission — it only limits *who is
automatically notified* of a textual conflict.

## Operational configuration notes

- Automatic review requires `serve --reviewer-agent <id>`; it is nil-disabled
  by default (#547 B3).
- Human decisions are resolved through the real CLI (`zenchron-engineering
  decision resolve ...`, `cmd/zenchron-engineering/decision.go`), never by
  pasting an answer into a provider conversation.
- Quarantined material from a refused attempt lives under
  `runs/<run>/quarantine/<op>-attempt-<n>/`, outside the candidate's `.git`;
  an operator recovering from a stuck run should look there before assuming
  work was lost.

## What this audit did not do

- No live run against a real LLM provider (Claude Code / Codex CLI). Every
  PASS above is a controlled test with a fake/stub provider — see the
  Implemented/Tested/Operational-evidence distinction in Phase 1.
- No code was changed. #390's fix already existed and held; there was nothing
  to repair.
- Did not touch #476, #479 or #480, per task scope.
