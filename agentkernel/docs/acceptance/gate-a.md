# Gate A acceptance record (#446 §15, §17)

This records the evidence that the isolated Agent Execution Kernel meets the
Gate A acceptance rows. It does not record production adoption: Gate B has not
started (`docs/integration-plan.md`).

## 1. Bindings

| Item | Value |
| --- | --- |
| Base | `27b7c30e0088ec28130b34d9604cb12a945f918c` (main, merge of #503) |
| Branch | `claude/446-agent-kernel-gate-a` |
| HEAD | `140625655e06ab3d711d5b1e57e1092f75f66962` at time of writing; the working tree also held this documentation update, not yet committed |
| Toolchain | `go version go1.27.1 darwin/arm64` |
| Module | `github.com/bogdaniel/zenchron-engineering/agentkernel`, `go 1.25.0`, no `require`, no `go.sum` |
| Environment | `GOWORK=off`, run from `agentkernel/`; no network, no credentials |
| CI | `.github/workflows/agent-kernel.yml` (format, vet on linux/amd64, linux/arm64, darwin/arm64, windows/amd64, tests, race, imports, extract, relocate, scope) |

## 2. Commands and results

Run on 2026-10-07 against the HEAD above plus the uncommitted docs. Every
command exited 0.

| Command | Decisive output |
| --- | --- |
| `test -z "$(gofmt -l .)"` | no files listed |
| `go vet ./...` | no output |
| `go test -count=1 ./...` | `ok` for all 17 packages with tests; no `FAIL` |
| `go test -race -count=1 ./...` | `ok` for all 17 packages; no race reported |
| `bash scripts/check-imports.sh` | `imports: ok (github.com/bogdaniel/zenchron-engineering/agentkernel depends only on itself and the standard library)` |
| `bash scripts/extract.sh` | `extract: ok (github.com/bogdaniel/zenchron-engineering/agentkernel built, vetted and tested in /var/folders/…/agentkernel-extract.h1dnTK)` |
| `bash scripts/relocate.sh` | `relocate: ok (built and tested as example.invalid/relocated/agentkernel)` |
| `bash scripts/check-scope.sh` | `scope: ok (base 27b7c30e0088ec28130b34d9604cb12a945f918c)` |
| `go run ./cmd/kernel-eval -corpus testdata/eval -trials 3 -out <scratch>/kernel-eval.json` | `kernel-eval: planned 108 attempted 108 executed 108 verified 108 verification_failed 0 harness_errors 0 (947 ms)`; outcomes 96 `completed`, 12 `exhausted` (expected by the corpus); modes `baseline`, `cold`, `warm`, `drift`; config digest `sha256:9627784c7e865f74d5c3e804b723f9ad99d8fe80ca046785c6f6f08cd60586c4`; baseline 5 (native CLI vs kernel) `not run: Gate B` |

`git diff --name-only 27b7c30 HEAD` outside `agentkernel/` lists only
`.github/workflows/agent-kernel.yml`.

## 3. Acceptance rows

Status values: **met**; **met with limitation** (the limitation is stated);
**pending Gate B**. Test names are Go test functions; `tests/conformance` is
the independent review lane's suite.

| ID | Requirement (short) | Evidence | Status |
| --- | --- | --- | --- |
| A01 | standalone consumer, no Zenchron object/config/DB | `examples/standalone` `TestStandaloneSettlesAndWrites`; `tests/conformance` `TestA01StandaloneScriptedExecution`; `scripts/extract.sh` (outside the checkout) | met |
| A02 | non-repository document task through supplied tools | `examples/hostport` `TestHostPortDocumentTask` (host `CommandRunner`, `EventSink`, `CredentialSource`, custom tool, `providers/local`) | met with limitation: no non-file capability kind, so the document tool borrows `file.search` (capabilities spec §2) |
| A03 | multi-step execution, resolvable artifacts, truthful usage, one settlement | `engine` `TestMultiStepToolExecution`, `TestSettlementRecordingFailureKeepsObservedOutcome`; `tools` `TestLargeFileDisclosesExcerptWithExactArtifact` | met |
| A04 | strict version/capability validation before side effects | `engine` `TestInvalidRequestRefusedBeforeSideEffects`; `api` `TestInvalidRequestExamplesAreRefusedOnTheExpectedField`, `TestDecodeRequestRefusesDuplicateKeys`, `TestDecodeRefusesNonCanonicalKeys`; `tests/conformance` `TestA04RefusedBeforeAnySideEffect`, `TestA04StrictDecodeRefusesAmbiguousJSON` | met |
| A05 | shared reservations obey limits; retry/re-entry cannot renew | `engine` `TestConcurrentReservationsCannotOverspend` (race), `TestSettleKeepsOverrun`, `TestBudgetDimensions`, `TestRetriesAreBoundedAndNeverRenew`, `TestMacroStepsAreChargedToTheToolBudget`; `tests/conformance` `TestA05ConcurrentExecutionsCannotOverspend`, `TestA05RetriesNeverRenewBudget`, `TestFindingNegativeReportedUsageRenewsBudget` | met with limitation: budgets are per `Execute` call; re-entering the same attempt id starts a fresh ledger, attempts are host-owned (F7); `max_input_tokens` is soft when the provider reports no input count (F6) |
| A06 | observed completion/failure survives later cancel; no leaks | `engine` `TestObservedOutcomeSurvivesLateCancel`, `TestCancellationProvenance`, `TestDeadlineExhaustion`; `api` `TestCancellationOf`; `tests/conformance` `TestA06ObservedTerminationSurvivesLateCancel`, `TestA06CancelInFlightIsTypedAndReleased`; provider `Cancellation`/`Deadline` conformance cases | met |
| A07 | missing capability, traversal/symlink escape, stale precondition, output-based privilege cannot mutate | `tools` `TestDispatchRefusesBeforeReachingTheTool`, `TestPathGuardsRefuseEscapesWithoutMutation`, `TestSameDirRefusesSwappedRoot`, `TestWritePreconditionNeverOverwritesNewerContent`, `TestGitMetadataIsNeverReachable`; `tests/conformance` `TestA07ProhibitedInvocationsCannotMutate`, `TestA07StalePreconditionUnderGrantCannotOverwrite`, `TestA07OutputCannotGrantPrivilege` | met with limitation: development-grade guards, isolation `unproven`; credential-shaped names/contents not refused |
| A08 | filtered output keeps errors and exact artifact; corruption/recording failure explicit | `tools` `TestOutputBoundKeepsErrorExitCodeAndExactArtifact`, `TestRecordingFailureIsExplicit`, `TestCommandFailurePreservesExitStderrAndArtifact`; `storage` `TestFileArtifactsDetectCorruptionAndReopen`; `engine` `TestRecordingFailureStopsSideEffects`, `TestUnknownSideEffectStopsExecution`; `tests/conformance` `TestFindingCommandRanButResultHidesIt` | met |
| A09 | required context survives; impossible required context is a typed block; retrieval untrusted | `context` `TestRequiredSurvivesTightBudget`, `TestImpossibleRequiredIsTypedBlock`, `TestRequiredSmuggledAsOptionalIsRefused`, `TestRetrievedContextStaysUntrusted`; `engine` `TestRequiredContextOverCapacityBlocks`, `TestTrustBoundary`; `tests/conformance` `TestA09*`, `TestTrustBoundaryRememberedInstructionsNeverBecomeSystem` | met |
| A10 | snapshots/overlays handle edit/add/delete/rename/config; base and other tasks untouched | `intelligence` `TestOverlayEditRefreshesDependents`, `TestOverlayAddFile`, `TestOverlayDeleteRemovesRelations`, `TestOverlayRenameMovesProvenance`, `TestOverlayGoModChangeInvalidatesModule`, `TestOverlayBuildTagChangeInvalidatesEverything`, `TestOverlayToolchainChangeInvalidates`, `TestOverlayRefusesUndeclaredWorkspaceDrift`, `TestConcurrentOverlaysAreIsolated` | met |
| A11 | unresolved edges and suggested-test incompleteness visible | `intelligence` `TestCallEdgesSeparateStaticFromPossibleDynamicUnresolved`, `TestIncompleteAnalysisIsRecorded`, `TestQueriesNeverClaimCompletenessTheyLack`, `TestSuggestedTestsAreInferredAndIncomplete`, `TestViewDependencyItemShowsIncompleteness` | met |
| A12 | memory provenance, stale/conflict, corruption, retention, partitioning | `memory` `TestPartitionIsolation`, `TestInvalidationIsTransitiveByIdentity`, `TestContradictionsCoexistUntilResolved`, `TestBoundsNeverEvictReferencedRecords`, `TestByteBound`, `TestCorruptionDegradesExplicitlyAndRebuilds`, `TestRecordValidation`, `TestConcurrentReadersAndWriters` | met with limitation: pins are process-local |
| A13 | pinned binding survives routing; ineligible/protected-incompatible never chosen | `routing` `TestPinnedPreservedAndNeverFallsBack`, `TestCheaperIneligibleNeverChosen`, `TestProtectedIncompatibleExcluded`, `TestUnknownPriceIsNeverZero`, `TestStaleObservationVisibleAndUnused`, `TestDeterministicTieBreak`; `engine` `TestChosenBindingWithoutAdapterBlocks`; `tests/conformance` `TestA13BindingSurvivesRoutingThroughEngine` | met |
| A14 | two real adapters pass shared offline conformance; live status truthful | `providers/conformance` run by `openai`, `anthropic`, `local` (`TestConformance`) and `scripted`; adapter tests in providers spec §4 | met with limitation: offline only, **no live call**; wire fixtures are written from documentation retrieved 2026-10-07 (providers spec §6) |
| A15 | disabled/corrupt/stale cache still correct, degradation explicit | `intelligence` `TestCacheFailuresDegradeToCorrectRebuild`, `TestCacheMissesWhenWorkspaceChanges`; `memory` `TestCorruptionDegradesExplicitlyAndRebuilds`; `tests/conformance` `TestA15CorruptCachesYieldCorrectBoundedExecution`, `TestA15FailingSourceDegradesExplicitly` | met |
| A16 | restart reopens derived state; no automatic replay of uncertain mutations | `intelligence` `TestCacheReopenAfterRestart`, `TestOverlayOfLoadedIndex`; `memory` `TestReopenAfterRestart`; `storage` `TestFileRecordsReopenAndDetectCorruption`, `TestFileArtifactsDetectCorruptionAndReopen`; `tests/conformance` `TestA16ReopenDerivedStateAfterRestart` | met with limitation: executions are not resumable by design; nothing is replayed |
| A17 | parent dependency, out-of-scope write and extraction failures demonstrated, incl. test/tagged bypass | `docs/acceptance/deliberate-breaks.md`; `tests/architecture` (`TestImportsStayInsideModuleAndStdlib` (walks tagged and ignored files), `TestNoParentCheckoutPathLiterals`, `TestNoSymlinkEscapesModule`, `TestPackageDependencyMap`, `TestNoLinknameDirectives`, `TestModuleMetadataIsSelfContained`); `scripts/check-imports.sh`, `extract.sh`, `check-scope.sh` | met |
| A18 | no root runtime/default/config/schema/journal/authority change | `scripts/check-scope.sh` pass; diff outside `agentkernel/` is only the new workflow (§2) | met |
| A19 | benchmark report with denominators, unknowns, failures, maintenance cost, limits | `docs/benchmarks/methodology.md`, `docs/benchmarks/results-gate-a.json`; this run (§2) | met with limitation: offline scripted fixtures prove mechanics only, not paid-model quality or accepted-change throughput; baseline 5 is Gate B |
| A20 | docs let a fresh agent build/test/extend/extract | `README.md`, `AGENTS.md`, `docs/adr/0001-extractable-agent-execution-kernel.md`, `docs/architecture.md`, `docs/spec/*`, `docs/concurrency.md`, `docs/integration-plan.md` | met |

Production adoption, comparative dogfood and native-CLI parity are not Gate A
rows; they are **pending Gate B**.

## 4. Deliberate failures

Ten break classes (#446 §14), sixteen variants, each applied in a disposable
clone, caught by at least one required check, and reverted to a passing tree.
Patches, digests and decisive output are in
[`deliberate-breaks.md`](deliberate-breaks.md). The exercise found F4
(`check-imports.sh` passed on unresolvable imports); with F1–F3 from the
independent review, it was fixed in `1406256`. After the fix, breaks 1a–1c are
also caught by `check-imports.sh` (`go: can't load test package: … no required
module provides package …`, exit 1).

## 5. Limitations and unresolved work

Open by design or carried to Gate B:

- Not production-enabled; no root code imports the module.
- Isolation is `unproven`; credential-shaped names/contents are not refused by
  kernel file tools (`.git` is).
- No live provider verification; quota and rate limit are both `rate_limited`.
- `max_input_tokens` is soft when input usage is unreported (F6).
- Budgets are per `Execute` call; attempt identity and cumulative budgets are
  host-owned (F7).
- Retry exhaustion has no dedicated termination dimension.
- `intelligence.index` cache has no retention; memory pins are process-local.
- Review finding F5 (low) remains open: intelligence hashes and parses whole
  files with no size cap (`intelligence/manifest.go`, `intelligence/session.go`),
  and file selection precedes the read, so a file swapped for a symlink in
  between could be followed (development-grade boundary, as for the tools).
- Harness/host seams: no exported manifest-digest helper outside
  `intelligence.Build`/`Open`; no structured per-call tool results in
  `ExecutionResult` (`cmd/kernel-eval` decodes the transcript); no non-file
  read capability kind; `run_command` only in `read_write`.
- Not crash-resumable; no inactivity/no-progress bound; no forensic
  transcript artifact (`docs/integration-plan.md` §5).
- Root documents (`docs/construction-principles.md` YAGNI, architecture,
  roadmap) are unchanged; amending them is Gate B work.

Gate A: isolated kernel complete. Gate B (production adoption): pending.
