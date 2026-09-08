# M2 acceptance: the Engineering Planner (#64)

This is the evidence record for #64's twenty-four acceptance scenarios. It
exists because "prepares" and "proves" are different claims: a schema that can
represent a refusal prepares a scenario, and a test or a live run that produces
the refusal proves it.

Each row names the implementation, the deterministic test that drives it, and -
where the scenario requires real behaviour - the live evidence. A row with no
proving test is stated as such rather than counted.

Test names are exact. `go test ./... -run <name>` reproduces any of them.

## 1. Trivial compile

A trivial change compiles to the work and the assurance its contract already
requires - no architect, no security reviewer.

- `planning/compiler.go` — `baseStages`, the minimum decomposition.
- **Proved:** `planning.TestTrivialChangeCompilesWithoutCeremony`.

## 2. Policy-driven security

A security-sensitive change gains the independent review that
`EngineeringPolicy` states, through the existing policy compiler.

- `policy/compiler.go` — `addEngineeringRequirements`; `planning/compiler.go` —
  `applyPolicyObligations`.
- **Proved:** `policy.TestPolicyCompilesRoleCapabilityAndGateObligations`,
  `planning.TestSecuritySensitiveChangeGainsPolicyObligations`.

## 3. Parallel plan

A large change decomposes into a dependency graph with parallel implementation
stages, and those stages become distinct EngineeringRuns that the existing
supervisor drives concurrently.

- `planning/compiler.go` — `canonicalOrder`; `runtime/plan_reconciler.go`.
- **Proved:** `planning.TestParallelImplementationStagesCompileIntoOneGraph`,
  `runtime.TestApprovedPlanCreatesOneRunPerAgentStageAndNoneForGates`,
  `runtime.TestServeReconcilesAPlanAndDrivesItsRunsInOneTick` — the last one
  asserts the two runs the plan created are driven in the same supervisor tick,
  under the operator's own ceiling.

## 4. Graph and schema refusal

Cycles, dangling dependencies, invalid references and malformed plan JSON fail
deterministically. Nested plans are not representable at all.

- `planning/graph.go`; `schemas/engineering-plan.schema.json`.
- **Proved:** `planning.TestGraphRefusalsAreDeterministic`,
  `planning.TestRegistryRefusals`, and the invalid fixtures
  `nested-plan.engineering-plan.json`,
  `nested-plan.engineering-plan-template.json` in `schemas.TestInvalidFixtures`.

## 5. Custom agent profile

An operator defines a reusable profile over an already-registered worker and
uses it without changing scheduler, kernel or domain code.

- `planning/registry.go`.
- **Proved:** `planning.TestRegistryLoadsAndSealsOperatorArtifacts`.
- **Live:** the dogfood ran `zenchron-builder` (Codex) and `zenchron-reviewer`
  (Claude Code), both defined only as files under `planning_dir`.

## 6. Role, profile and agent flexibility

One worker backs several profiles; one role resolves onto different profiles;
the resolver explains the choice and every rejection.

- `planning/resolver.go`.
- **Proved:** `planning.TestOneAgentBacksSeveralProfilesAndTheChoiceIsExplained`,
  `planning.TestAnEscalatingProfileIsIneligibleWithItsReason`.
- **Live:** the dogfood's recorded selection explanation rejects
  `zenchron-builder` for the review stage with two reasons - it does not
  advertise `security_review`, and its vendor family is not independent of the
  implementation stage.

## 7. Custom workflow template

An operator-defined template compiles into an ordinary plan, and policy adds
obligations the template omitted.

- `planning/template.go`, `planning/compiler.go`.
- **Proved:** `planning.TestPolicyAddsObligationsATemplateOmitted`.
- **Live:** the dogfood plan followed `zenchron-docs-change` and pinned its
  digest in plan provenance.

## 8. Immutable composition identity

A profile or instruction pack edited after approval does not rewrite work in
flight, and the exact digests stay reconstructable.

- `runtime/operations.go` — `frozenInstructions`; `runtime/plan_store.go` —
  immutable revisions.
- **Proved:** `runtime.TestAnEditedInstructionPackRefusesRatherThanRewritingApprovedWork`,
  `runtime.TestPlanRevisionsAreImmutable`.

## 9. Planner uses the registered workforce

Semantic planning runs through a registered #63 execution agent. No direct
provider API, endpoint or credential path exists behind the planner.

- `runtime/planner.go` — it contains no HTTP client, no endpoint and no
  credential field.
- **Proved:** `runtime.TestPlanningInvocationUsesTheProvidersOwnReadOnlyMode`,
  `runtime.TestPlanningIsRefusedByAProviderWithoutANonMutatingMode`.
- **Live:** the dogfood plan records `agent_id: claude`,
  `provider_kind: claude_code`, `provider_mode: plan`.

## 10. Operator approval

A proposed plan is visible with its assignments, blockers and budget, and does
not execute until approved. An edit produces a new revision.

- `runtime/plan_service.go`, `cmd/zenchron-engineering/plan.go`.
- **Proved:** `cmd.TestPlanProposalAwaitsApproval`,
  `cmd.TestPlanApprovalIsRecordedAgainstTheExactRevision`,
  `runtime.TestAnUnapprovedPlanCreatesNothing`.
- **Live:** the dogfood plan sat unapproved until `plan approve`, and `serve`
  reported `awaiting_operator_approval` for it.

## 11. Independence shortage

With one eligible vendor and a vendor-family obligation, the plan surfaces an
explicit block. A human substitution is offered only where policy permits it.

- `planning/resolver.go` — `independenceViolation`, `blocked`.
- **Proved:** `planning.TestSingleVendorIndependenceShortageBlocksExplicitly`,
  `planning.TestHumanSubstitutionIsAvailableOnlyWherePolicyPermitsIt`,
  `planning.TestARevisionCannotEscapeIndependenceByBecomingAGate`.
- The substitution is an operator DECISION - `plan revise --substitute-human
  STAGE` - and it produces a revision that goes through the ordinary approval
  boundary. The resulting gate records the role it stands in for, so the
  obligation stays checkable; a gate that names no role fulfils none, which is
  what stops "turn the reviewer into a gate" from being an escape.
- **Live:** the dogfood satisfied the obligation rather than blocking on it, with
  two eligible vendors: `codex` (openai) implemented and `claude` (anthropic)
  reviewed, and the resolver recorded why the builder profile was ineligible for
  the review stage - missing capability AND vendor family not independent.

## 12. Role-specific context

Implementer, reviewer and security reviewer receive different ContextPacks, and
no reviewer inherits the producer's reasoning transcript.

- `planning/context.go`.
- **Proved:** `planning.TestImplementerAndIndependentReviewerReceiveDifferentPacks`,
  `planning.TestProducerReasoningIsNeverDelivered`.
- **Live:** the dogfood reviewer worked from the published upstream candidate
  delivered as delimited untrusted data, and its review cites the diff it was
  given - not the producer's transcript, which it never received.

## 13. Deterministic safety validation

A proposal that violates policy, trust, capability, budget or independence is
refused whatever recommended it.

- `planning/validate.go`.
- **Proved:** `planning.TestProposalsThatWeakenPolicyAreRefused`,
  `runtime.TestPlannerAnswersOutsideTheVocabularyAreRefused`.
- **Live:** three separate live refusals during the dogfood - a proposal needing
  five child runs against an envelope of three, an assurance gate with no
  claims, and an answer carrying an unrecognized member. All three were refused
  before anything executed.

## 14. Dependency gating ownership

The reconciler creates runs only when dependencies are satisfied; the #63
scheduler and leases remain the sole worker scheduling mechanism.

- `runtime/plan_reconciler.go`.
- **Proved:** `runtime.TestDependenciesAndGatesGateTheGraph`,
  `runtime.TestServeReconcilesAPlanAndDrivesItsRunsInOneTick`.

## 15. Revision safety

An upstream change invalidates only affected downstream stages; consumed budget
is not reset; privilege is not widened; active runs are not silently rewritten.

- `runtime/plan_service.go` — `InvalidatedStages`; `planning/validate.go` —
  `revisionViolations`.
- **Proved:** `planning.TestRevisionsCannotWidenPrivilege`,
  `planning.TestRevisionsCannotClaimBackConsumedBudget`,
  `runtime.TestDecompositionEmitsAProposalAndPausesAffectedWork`,
  `runtime.TestConsumedBudgetSurvivesRevisionAndRestart`.

## 16. Crash recovery

Restart reconstructs plan revisions, approval, assignments, dependency and gate
state and child runs from the durable store alone.

- `runtime/plan_projection.go`, `runtime/plan_store.go`.
- **Proved:** `runtime.TestRestartReconstructsPlanStateFromTheStore`,
  `runtime.TestRestartReconstructsPlanStateFromTheStoreAlone`.

## 17. Self-building dogfood

Zenchron planned and executed a real Zenchron change: issue #77, planned by
`claude` in `plan` mode, implemented by `codex`, reviewed independently by
`claude` from the published candidate, assurance gate satisfied, resulting in
[PR #105](https://github.com/bogdaniel/zenchron-engineering/pull/105) with CI
green on the exact head. One operator decision, no intervention between approval
and the satisfied gate, 8m15s wall clock.

- **Live:** `docs/acceptance/m2-dogfood.md` - the full record, including the
  three defects the exercise found in this branch and the one disruption I
  caused myself.

## 18. Leverage measurement

- **Live:** `benchmarks/zenchron_planned/2026-09-08-issue-77.json`, in #66's
  shape. Two child runs, two provider invocations, one operator decision,
  495 seconds proposal to gate, cost `known: false`. No leverage ratio is
  claimed: #66's harness, corpus and both baselines do not exist, and one
  trivial documentation case proves nothing about leverage. Supervision minutes
  are recorded as unmeasured rather than estimated.

## 19. Operator-owned customization boundary

Repository or candidate configuration cannot create instruction packs, install
executables, raise ceilings or replace identities bound to active work.

- `runtime/config.go` — `repositoryScope`; `planning/escalation.go`;
  `schemas/instruction-pack.schema.json`.
- **Proved:** `runtime.TestRepositoryConfigCannotIntroducePlanningArtifacts`,
  `planning.TestProfilesMayNarrowButNeverEscalate`,
  `planning.TestEscalatingCustomizationIsUnrepresentable`, and the invalid
  fixture `candidate-authored.instruction-pack.json`.

## 20. One policy system

Role, capability and independence obligations compile through the existing
policy path. No second obligation engine exists.

- `policy/compiler.go`; `domain/types.go` — `PolicyEffect.EngineeringRequirements`.
- **Proved:** the whole of `policy/plan_requirements_test.go`, and
  `policy.TestContractsWithoutPlanObligationsStayByteIdentical` for the
  historical-identity guarantee.

## 21. Typed gates create no worker runs

- `planning/graph.go`, `runtime/plan_reconciler.go`.
- **Proved:** `runtime.TestApprovedPlanCreatesOneRunPerAgentStageAndNoneForGates`,
  `runtime.TestDependenciesAndGatesGateTheGraph`.

## 22. Decomposition revision boundary

A planner-role stage emits a schema-valid `PlanRevisionProposal`; a material
change requires approval before affected execution continues; nesting is
unrepresentable.

- `runtime/plan_decomposition.go`.
- **Proved:** `runtime.TestDecompositionEmitsAProposalAndPausesAffectedWork`.

## 23. Read-only planning eligibility

Planning uses a runtime-owned workspace bound to exact trusted state; the
runtime verifies it unchanged; a provider that cannot prove the mode is refused.

- `runtime/planner.go`, `runtime/agent_specs.go`.
- **Proved:** `runtime.TestPlanningInvocationVerifiesTheWorkspaceItself`,
  `runtime.TestAProviderThatWritesDuringPlanningIsRefused`,
  `runtime.TestAnUntrackedFileCountsAsAChangedWorkspace`,
  `runtime.TestPlanningIsRefusedWhenTheInstalledCLINoLongerAdvertisesTheMode`.
- **Live:** the dogfood records identical workspace digests before and after and
  `workspace_unchanged: true`.

## 24. Aggregate plan budget

Approval shows the envelope and known/unknown cost truthfully; execution
enforces it; consumption cannot reset.

- `domain/plan.go` — `PlanBudgetEnvelope`; `runtime/plan_reconciler.go`.
- **Proved:** `runtime.TestTheAggregateEnvelopeBoundsChildRuns`,
  `planning.TestTemplateMayTightenTheEnvelopeAndNeverWidenIt`,
  `runtime.TestUnknownCostStaysUnknownAndKnownCostAccumulates`.
- **Live:** a live proposal was refused for needing five child runs against an
  operator ceiling of three, and the approval view reports cost as unknown
  rather than zero.
