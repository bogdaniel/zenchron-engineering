# Gate A acceptance record (#446 §15, §17)

This records the evidence that the isolated Agent Execution Kernel meets the
Gate A acceptance rows. It does not record production adoption: Gate B has not
started; it is tracked by #518 (blocked by #445) under parent #446, and
planned in `docs/integration-plan.md`.

## 1. Bindings

PR #510 was merged into `main` without a separate rebase, after #511. The
evidence head is therefore the merge commit on `main` that contains both; every
result in §2 was produced on exactly that tree. A commit cannot contain its own
hash, so this binding lands in a later commit that changes only
`agentkernel/docs/acceptance/`; verify with
`git diff --name-only EVIDENCE_HEAD <binding commit>`.

EVIDENCE_HEAD: `762bcb60eaec216120e40f502fa55d74f4c07f9c` (main, merge of #510)

BASE: `f06471deee119dbdb1fa33a00990f744cb30225b` (main, merge of #511; first
parent of `EVIDENCE_HEAD`). `git diff BASE EVIDENCE_HEAD` is exactly the change
#510 brought to `main`.

| Item | Value |
| --- | --- |
| Branch | `claude/446-agent-kernel-gate-a` (PR #510) |
| Prerequisite | PR #511 (root test scope fix, review item 1), merged to `main` as `BASE` immediately before #510 |
| Provenance | original base `27b7c30e0088ec28130b34d9604cb12a945f918c` (main, merge of #503); first record at `140625655e06ab3d711d5b1e57e1092f75f66962`; human review 5443104514 on `f53fedf2652d37060683c177a1061c097dc44a8a`; review fixes `22247ed`, `ece635d`, `c8c54f1`, `0c2e7ef`, the ADR update `a2b551f` (§6), and the second-review repairs `a7f97a5` (§7). the refusal-ID fix `f2d4917`; last branch head `86ccf38`, merged with `main` as `8511687`. |
| Toolchain | `go version go1.27.1 darwin/arm64` |
| Module | `github.com/bogdaniel/zenchron-engineering/agentkernel`, `go 1.25.0`, no `require`, no `go.sum` |
| Environment | `GOWORK=off`, run from `agentkernel/`; no network, no credentials |
| CI | `.github/workflows/agent-kernel.yml` (format, vet on linux/amd64, linux/arm64, darwin/arm64, windows/amd64, tests, race, imports, extract, relocate, scope) and the root required check `go` (`.github/workflows/ci.yml`) |

## 2. Commands and results

Run on `EVIDENCE_HEAD` (`762bcb6`), 2026-10-07, from `agentkernel/` with
`GOWORK=off` unless noted. The earlier results recorded on `1406256` predate
review 5443104514 and are superseded.

| Command | Decisive output |
| --- | --- |
| `test -z "$(gofmt -l .)"` | no files listed |
| `go vet ./...` | clean (also `GOOS=windows`) |
| `go test -count=1 ./...` | 19 packages `ok`, no failures |
| `go test -race -count=1 ./...` | all packages `ok`, no race reported |
| `bash scripts/check-imports.sh` | `imports: ok (... depends only on itself and the standard library)` |
| `bash scripts/extract.sh` | `extract: ok (... built, vetted and tested in <tmp outside the checkout>)` |
| `bash scripts/relocate.sh` | `relocate: ok (built and tested as example.invalid/relocated/agentkernel)` |
| `bash scripts/check-scope.sh` | `BASE_REF=f06471d`: `scope: ok (base f06471deee119dbdb1fa33a00990f744cb30225b)` |
| `go run ./cmd/kernel-eval -corpus testdata/eval -trials 3 -out <scratch>/kernel-eval.json` | `planned 108 attempted 108 executed 108 verified 108 verification_failed 0 harness_errors 0` |
| Gate A workflow (`agent-kernel.yml`) on `EVIDENCE_HEAD` | run 37670662572 on `762bcb6`: success |
| root CI (`ci.yml`, job `go`) on `EVIDENCE_HEAD` | run 37670662745 on `762bcb6`: success |
| `git diff --name-only BASE EVIDENCE_HEAD` outside `agentkernel/` | `.github/workflows/agent-kernel.yml` only (199 files changed in total) |

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
| A05 | shared reservations obey limits; retry/re-entry cannot renew | `engine` `TestConcurrentReservationsCannotOverspend` (race), `TestSettleKeepsOverrun`, `TestBudgetDimensions`, `TestRetriesAreBoundedAndNeverRenew`, `TestMacroStepsAreChargedToTheToolBudget`, `TestReentryCannotRenewBudget` (re-entry, widening, later attempt starts from consumed), `TestConcurrentAttemptsAdmitExactlyOne` (race), `TestTwoEnginesOneRootAdmitExactlyOne` (two Engines, two separately opened `FileRecords` handles on one root; race), `TestCrashedAttemptBlocksTheNext`, `TestSettlementReleasesTheClaim`, `TestUnsettledAttemptBlocksTheNext`, `TestAdmissionWriteFailureRefusesBeforeSideEffects`, `TestNegativeReservationCannotRenewBudget`, `TestImpossibleCachePartitionGrantsNoRefund`, `TestInputBudgetIsAHardBound`, `TestLedgerSaturatesInsteadOfWrapping`, `TestOutOfRangePriceIsUnknown`, `TestUnansweredProviderCallKeepsReservations`, `TestHugeReportedInputStopsFurtherCalls`, `TestOverflowCannotRenewBudgetAcrossAttempts`, `TestPriorConsumptionIsCountedOnce`, `TestNegativeUsagePartMakesInputUnknown`; `storage` `TestPutIfAbsentAdmitsExactlyOne` (memory and file stores, 16 writers), `TestPutIfAbsentAcrossProcesses` (8 OS processes, exactly one creates); `providers/anthropic` `TestNegativeOrOverflowingUsagePartMakesInputUnknown`; `context` `TestNegativeEstimateIsRefused`; `tests/conformance` `TestA05ConcurrentExecutionsCannotOverspend`, `TestA05RetriesNeverRenewBudget`, `TestA05ReentryAcrossRestartCannotRenewBudget`, `TestFindingNegativeReportedUsageRenewsBudget` | met. Admission claims each `execution_id` with `storage.Records.PutIfAbsent` before any side effect (execution spec §11.1). The supported file-backed store, `FileRecords`, implements it as a synced temp file `link(2)`ed to the record name, which is atomic across processes sharing **one local filesystem on Linux or macOS**; Windows is refused by the file stores; on network filesystems `link` atomicity depends on the server (an NFS retry can report "exists" for its own link, which refuses the attempt and never admits two). Claims never expire by time: a claim left by a crashed attempt fails closed, and explicit recovery is Gate B / host work. The default in-memory store holds the envelope for one `Engine` instance; across restarts it needs `Config.Admissions` backed by `FileRecords`. A tight `max_input_tokens` can be exhausted by the byte upper-bound reservation before the first provider call (fails closed, never exceeds the bound). Envelopes spanning executions remain host-owned |
| A06 | observed completion/failure survives later cancel; no leaks | `engine` `TestObservedOutcomeSurvivesLateCancel`, `TestCancellationProvenance`, `TestDeadlineExhaustion`, `TestStuckEventSinkCannotHoldExecutionOpen`, `TestStuckContextSourceCannotHoldExecutionOpen`, `TestStuckProviderCannotHoldExecutionOpen` (each also asserts no kernel goroutine outlives `Execute`), `TestLateHostRepliesAreIgnored`, `TestStuckHostToolIsAnUncertainMutation`, `TestHandOffIDsAreStableAndUnique`, `TestModelToolCallIDsNeverBecomeHandOffIDs`, `TestClosedEventReplyIsRecordingFailure`, `TestNewRefusesHostCodeInKernelPorts` (exact kernel types; unbuffered hand-off channels), `TestDeadlineHoldsBeforeItsTimerFires`; `internal/handoff` `TestExchangeAnswered`, `TestExchangeNeverTaken`, `TestExchangeTakenNeverAnswered`, `TestExchangeLateAnswerIsDiscarded`, `TestExchangeShortenedByCancellation`, `TestExpiredBoundHandsNothingOver`, `TestClosedReplyIsNoAnswer`; `tools` `TestHostToolHandOff`, `TestCommandHandOff`, `TestConstructorsRefuseHostCode`; `api` `TestCancellationOf`; `tests/conformance` `TestA06ObservedTerminationSurvivesLateCancel`, `TestA06CancelInFlightIsTypedAndReleased`; provider `Cancellation`/`Deadline` conformance cases | met. Every host port (event sink, context source, provider, command runner, host tool) is a bounded request/reply hand-off (`internal/handoff.Exchange`: a timer and a `select`, no goroutine) served by host-owned workers; the kernel never runs host code on its goroutines and no kernel goroutine outlives `Execute` (execution spec §4.1). `Execute` returns within the deadline (or host cancellation) plus one settlement grace, plus the kernel's own local file I/O for artifacts and admissions, which is not deadline-bounded (see §5) |
| A07 | missing capability, traversal/symlink escape, stale precondition, output-based privilege cannot mutate | `tools` `TestDispatchRefusesBeforeReachingTheTool`, `TestPathGuardsRefuseEscapesWithoutMutation`, `TestSameDirRefusesSwappedRoot`, `TestWritePreconditionNeverOverwritesNewerContent`, `TestGitMetadataIsNeverReachable`, `TestSymlinkAliasToGitMetadataIsRefused` (read and write through `meta -> .git` and similar in-root aliases), `TestSymlinkInsideGrantIsRefused`, `TestCheckOpenedRefusesSwappedFile`, `TestHardLinkAliasToGitMetadataIsRefused` (unix), `TestSearchSkipsGitFiles` (a `.git` file, not only a directory, is skipped by `search`), `TestConstructorsRefuseHostCode` (`NewBroker` refuses a host type embedding a kernel tool); `tests/conformance` `TestA07ProhibitedInvocationsCannotMutate`, `TestA07StalePreconditionUnderGrantCannotOverwrite`, `TestA07OutputCannotGrantPrivilege` | met with limitation: development-grade guards, isolation `unproven` (capabilities spec §4.1, §9). Residual risk: a concurrent same-user process that swaps a parent directory for a link between the per-component check and the open can redirect that open inside the granted root (`.git` included when the grant covers it); hard links are not detected off unix; credential-shaped names/contents are not refused |
| A08 | filtered output keeps errors and exact artifact; corruption/recording failure explicit | `tools` `TestOutputBoundKeepsErrorExitCodeAndExactArtifact`, `TestRecordingFailureIsExplicit`, `TestCommandFailurePreservesExitStderrAndArtifact`; `engine` `TestModelToolCallIDsNeverBecomeHandOffIDs` (artifact producers are kernel sequences `<execution>/<attempt>/tool-<n>`; a repeated model tool-call ID is refused); `storage` `TestFileArtifactsDetectCorruptionAndReopen`, `TestFileArtifactsPutRepairsCorruptCopy`, `TestFileArtifactsAccountOccupiedBytesNotHeaderClaims`; `engine` `TestRecordingFailureStopsSideEffects`, `TestUnknownSideEffectStopsExecution`; `tests/conformance` `TestFindingCommandRanButResultHidesIt` | met |
| A09 | required context survives; impossible required context is a typed block; retrieval untrusted | `context` `TestRequiredSurvivesTightBudget`, `TestImpossibleRequiredIsTypedBlock`, `TestRequiredSmuggledAsOptionalIsRefused`, `TestRetrievedContextStaysUntrusted`; `engine` `TestRequiredContextOverCapacityBlocks`, `TestTrustBoundary`; `tests/conformance` `TestA09*`, `TestTrustBoundaryRememberedInstructionsNeverBecomeSystem` | met |
| A10 | snapshots/overlays handle edit/add/delete/rename/config; base and other tasks untouched | `intelligence` `TestOverlayEditRefreshesDependents`, `TestOverlayAddFile`, `TestOverlayDeleteRemovesRelations`, `TestOverlayRenameMovesProvenance`, `TestOverlayGoModChangeInvalidatesModule`, `TestOverlayBuildTagChangeInvalidatesEverything`, `TestOverlayToolchainChangeInvalidates`, `TestOverlayRefusesUndeclaredWorkspaceDrift`, `TestConcurrentOverlaysAreIsolated` | met |
| A11 | unresolved edges and suggested-test incompleteness visible | `intelligence` `TestCallEdgesSeparateStaticFromPossibleDynamicUnresolved`, `TestIncompleteAnalysisIsRecorded`, `TestQueriesNeverClaimCompletenessTheyLack`, `TestSuggestedTestsAreInferredAndIncomplete`, `TestViewDependencyItemShowsIncompleteness` | met |
| A12 | memory provenance, stale/conflict, corruption, retention, partitioning | `memory` `TestPartitionIsolation`, `TestInvalidationIsTransitiveByIdentity`, `TestContradictionsCoexistUntilResolved`, `TestBoundsNeverEvictReferencedRecords`, `TestByteBound`, `TestCorruptionDegradesExplicitlyAndRebuilds`, `TestRecordValidation`, `TestConcurrentReadersAndWriters` | met with limitation: pins are process-local |
| A13 | pinned binding survives routing; ineligible/protected-incompatible never chosen | `routing` `TestPinnedPreservedAndNeverFallsBack`, `TestCheaperIneligibleNeverChosen`, `TestProtectedIncompatibleExcluded`, `TestUnknownPriceIsNeverZero`, `TestStaleObservationVisibleAndUnused`, `TestDeterministicTieBreak`; `engine` `TestChosenBindingWithoutAdapterBlocks`; `tests/conformance` `TestA13BindingSurvivesRoutingThroughEngine` | met |
| A14 | two real adapters pass shared offline conformance; live status truthful | `providers/conformance` run by `openai`, `anthropic`, `local` (`TestConformance`) and `scripted`; adapter tests in providers spec §4 | met with limitation: offline only, **no live call**; wire fixtures are written from documentation retrieved 2026-10-07 (providers spec §6) |
| A15 | disabled/corrupt/stale cache still correct, degradation explicit | `intelligence` `TestCacheFailuresDegradeToCorrectRebuild`, `TestCacheMissesWhenWorkspaceChanges`; `memory` `TestCorruptionDegradesExplicitlyAndRebuilds`; `tests/conformance` `TestA15CorruptCachesYieldCorrectBoundedExecution`, `TestA15FailingSourceDegradesExplicitly` | met |
| A16 | restart reopens derived state; no automatic replay of uncertain mutations | `intelligence` `TestCacheReopenAfterRestart`, `TestOverlayOfLoadedIndex`; `memory` `TestReopenAfterRestart`; `storage` `TestFileRecordsReopenAndDetectCorruption`, `TestFileArtifactsDetectCorruptionAndReopen`, `TestFileArtifactsAccountOccupiedBytesNotHeaderClaims` (reopen counts occupied bytes, a forged header frees nothing, repair is charged); `tests/conformance` `TestA16ReopenDerivedStateAfterRestart`, `TestA05ReentryAcrossRestartCannotRenewBudget` | met with limitation: executions are not resumable by design; nothing is replayed |
| A17 | parent dependency, out-of-scope write and extraction failures demonstrated, incl. test/tagged bypass | `docs/acceptance/deliberate-breaks.md`; `tests/architecture` (`TestImportsStayInsideModuleAndStdlib` (walks tagged and ignored files), `TestNoParentCheckoutPathLiterals`, `TestNoSymlinkEscapesModule`, `TestPackageDependencyMap`, `TestNoLinknameDirectives`, `TestModuleMetadataIsSelfContained`); `scripts/check-imports.sh`, `extract.sh`, `check-scope.sh` | met |
| A18 | no root runtime/default/config/schema/journal/authority change | `scripts/check-scope.sh`; diff outside `agentkernel/` is only the new workflow (§2, `git diff BASE EVIDENCE_HEAD`). The root test fix for review item 1 is PR #511, a separate prerequisite merged to `main`, not part of this branch | met |
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

Every guard added for review 5443104514 (storage, tools, engine, the root
#511 scan, and the follow-up hand-off and atomic admission of `0c2e7ef`) was
also removed in turn, failed its focused test, and passed again
when restored; see "Review 5443104514 guards" and "Review 5443104514 follow-up: hand-off
and atomic admission" in `deliberate-breaks.md`.

## 5. Limitations and unresolved work

Open by design or carried to Gate B:

- Not production-enabled; no root code imports the module.
- Isolation is `unproven`; credential-shaped names/contents are not refused by
  kernel file tools (`.git` is).
- No live provider verification; quota and rate limit are both `rate_limited`.
- Admission atomicity (`FileRecords.PutIfAbsent`) holds for processes on one
  local filesystem on Linux and macOS; Windows file stores are refused; on
  network filesystems `link` atomicity is server-dependent (a false "exists"
  refuses, never double-admits). Cross-process behaviour is tested with
  eight separate OS processes racing one claim
  (`storage` `TestPutIfAbsentAcrossProcesses`, re-executing the test binary),
  and with two engines on separately opened handles of one root.
- A claim left by a crash (including a crash between the record write and the
  claim delete at settlement) is never expired: later attempts fail closed
  until an explicit host recovery protocol exists (Gate B).
- Envelopes spanning executions are host-owned; the default in-memory
  admission store holds the envelope for one `Engine` instance only.
- A tight `max_input_tokens` can be exhausted by the byte upper-bound
  reservation before the first provider call.
- Kernel-owned local file I/O (artifact and admission stores) is not
  deadline-bounded: a hung filesystem (for example a hard NFS mount) can hold
  `Execute` open.
- `api.Serve*` workers answer one call at a time; a host wanting parallel
  answers runs its own workers.
- Command and host-tool hand-offs wait until the run context ends with no
  settlement grace, so a reply arriving at that instant may be reported as an
  uncertain mutation.
- The kernel calls its artifact store directly, and `engine.New` accepts
  `Config.Artifacts` only by exact type (`*storage.MemoryArtifacts` or
  `*storage.FileArtifacts`), so a host-bridged artifact store is not possible
  in Gate A; Gate B uses a hand-off or `FileArtifacts`.
- Kernel file tools: a concurrent same-user parent-directory swap is not
  excluded; hard links are refused only on unix.
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

## 6. Review 5443104514 resolution

Human review 5443104514 (on `f53fedf`) listed seven blocking items.

| # | Item | Resolution | Evidence |
| --- | --- | --- | --- |
| 1 | root CI red: the root `ExecutionRequest{` producer scan read the nested module | PR #511, a separate root prerequisite outside the #446 allowlist: the scan stops at any directory with its own `go.mod`; the root identity guard is not weakened and the kernel API is not renamed | `runtime` `TestExecutionRequestScanSkipsNestedModules`; root `go` check green on the evidence head (§2) |
| 2 | A07: `.git` reachable through an in-workspace symlink alias | `ece635d`: file tools refuse a symlink in any path component (in-root links included; `TestSymlinkInsideGrantIsFollowed` removed), recheck the opened file with `os.SameFile`, and refuse hard-linked files on unix; residual risk documented (capabilities spec §4.1, §9) | `tools` `TestSymlinkAliasToGitMetadataIsRefused`, `TestSymlinkInsideGrantIsRefused`, `TestCheckOpenedRefusesSwappedFile`, `TestHardLinkAliasToGitMetadataIsRefused` |
| 3 | artifact repair renewed file-store capacity after restart | `22247ed`: capacity counts occupied bytes, never header claims; headerless files count in full; a repair is charged for its growth (capabilities spec §8) | `storage` `TestFileArtifactsAccountOccupiedBytesNotHeaderClaims` |
| 4 | a negative `TokenEstimator` count minted input capacity; impossible cache partitions refunded money | `c8c54f1`: the ledger refuses negative amounts at the root and an impossible partition makes input and cache parts unknown (execution spec §7.5). `0c2e7ef` removed the `api.TokenEstimator` port: input reservation is the kernel's own byte upper bound, so no host count reaches the ledger (§7.4); `context.Compile` still refuses a negative estimate from its estimator | `engine` `TestNegativeReservationCannotRenewBudget`, `TestImpossibleCachePartitionGrantsNoRefund`, `TestInputBudgetIsAHardBound`; `context` `TestNegativeEstimateIsRefused` |
| 5 | A05: re-entry renewed the budget; `max_input_tokens` soft | `c8c54f1`: one envelope per `execution_id`; re-entry, widening and starting beside an unsettled attempt are refused; later attempts start from recorded consumption; `max_input_tokens` is a hard ledger bound (§7.4). `0c2e7ef`: admission claims the `execution_id` with the new `storage.Records.PutIfAbsent` (`FileRecords`: synced temp file + `os.Link`), atomic across processes on one local filesystem; a losing contender reserves and calls nothing; claims never expire by time (§11.1) | `engine` `TestReentryCannotRenewBudget`, `TestConcurrentAttemptsAdmitExactlyOne`, `TestTwoEnginesOneRootAdmitExactlyOne`, `TestCrashedAttemptBlocksTheNext`, `TestSettlementReleasesTheClaim`, `TestUnsettledAttemptBlocksTheNext`, `TestAdmissionWriteFailureRefusesBeforeSideEffects`, `TestInputBudgetIsAHardBound`; `storage` `TestPutIfAbsentAdmitsExactlyOne`; `tests/conformance` `TestA05ReentryAcrossRestartCannotRenewBudget`; stated limits in A05 |
| 6 | event recording outside every cancellation/deadline bound | `0c2e7ef`: every host port is a bounded hand-off (`internal/handoff.Exchange`, no goroutine) to host-owned workers (`api.Serve*`, `tools.ServeTool`); enqueue and reply waits are bounded, reply channels hold one value, late replies are discarded, call IDs are stable per attempt (tool IDs are kernel sequences since `a7f97a5`, §7); the kernel never runs host code on its goroutines and no kernel goroutine outlives `Execute` (execution spec §4.1). `c8c54f1`: `run.interrupted` also compares wall time, closing a timer race seen under full-suite load | `engine` `TestStuckEventSinkCannotHoldExecutionOpen`, `TestStuckContextSourceCannotHoldExecutionOpen`, `TestStuckProviderCannotHoldExecutionOpen`, `TestLateHostRepliesAreIgnored`, `TestStuckHostToolIsAnUncertainMutation`, `TestHandOffIDsAreStableAndUnique`, `TestDeadlineHoldsBeforeItsTimerFires`; `internal/handoff` `TestExchange*` (five cases); `tools` `TestHostToolHandOff`, `TestCommandHandOff` |
| 7 | acceptance evidence stale relative to the reviewed head | this record: §1 binds `EVIDENCE_HEAD` (`762bcb6`, #510 merged after #511) and `BASE`, and §2 was run there | §1, §2 |

## 7. Second review (adversarial) resolution

A second, adversarial review of the Gate A branch (not recorded on GitHub)
reported nine findings and a concern about buffered hand-off channels. All are
repaired in `a7f97a5`; the contract changes are in the execution,
capabilities and providers specs.

| # | Finding | Resolution in `a7f97a5` | Evidence |
| --- | --- | --- | --- |
| R2-F1 | a huge reported count wrapped ledger arithmetic and renewed budget, within or across attempts | ledger arithmetic saturates at `MaxInt64`; an out-of-range price makes cost unknown | `engine` `TestHugeReportedInputStopsFurtherCalls`, `TestOverflowCannotRenewBudgetAcrossAttempts`, `TestLedgerSaturatesInsteadOfWrapping`, `TestOutOfRangePriceIsUnknown` |
| R2-F2 | the seal could be passed by a host type embedding a kernel type | `engine.New` and `tools.NewBroker` accept only exact kernel types (`Config.Artifacts` `*storage.MemoryArtifacts`/`*storage.FileArtifacts`, `Config.Clock` `api.SystemClock`/`*api.ManualClock`, package `tools`' own tool type); `storage.Artifacts` removed; `api.Clock` is a plain interface | `engine` `TestNewRefusesHostCodeInKernelPorts`; `tools` `TestConstructorsRefuseHostCode` |
| R2-F3 | admission summed cumulative records, counting prior attempts more than once | admission restores the largest recorded cumulative consumption | `engine` `TestPriorConsumptionIsCountedOnce` |
| R2-F4 | tool hand-off IDs reused the model's tool-call ID | tool hand-off IDs and artifact producers are kernel sequences `<execution>/<attempt>/tool-<n>`; a repeated model tool-call ID is refused per attempt | `engine` `TestModelToolCallIDsNeverBecomeHandOffIDs` |
| R2-F5 | a negative or overflowing usage part did not make input unknown | input and both cache parts become unknown, in the Anthropic adapter and in the engine | `providers/anthropic` `TestNegativeOrOverflowingUsagePartMakesInputUnknown`; `engine` `TestNegativeUsagePartMakesInputUnknown` |
| R2-F6 | `Exchange` could offer a call after its bound was over | an expired bound hands nothing over (`ErrNotTaken`) | `internal/handoff` `TestExpiredBoundHandsNothingOver` |
| R2-F7 | `search` echoed a `.git` file (a gitdir file, not a directory) | `search` skips any `.git` entry, file or directory | `tools` `TestSearchSkipsGitFiles` |
| R2-F8 | a closed reply channel read as an answer | a closed reply is no answer (`ErrNoAnswer`); a closed event reply is a recording failure | `internal/handoff` `TestClosedReplyIsNoAnswer`; `engine` `TestClosedEventReplyIsRecordingFailure` |
| R2-F9 | guards to pin: every widening dimension, reservations kept for an unanswered provider call, the tool hand-off bound | each re-broken and caught (`deliberate-breaks.md`) | `engine` `TestReentryCannotRenewBudget` (`widened_*`), `TestUnansweredProviderCallKeepsReservations`; `tools` `TestCommandHandOff`, `TestHostToolHandOff` |
| — | buffered hand-off channels let "not taken" be reported for a call the host had already received | hand-off channels must be unbuffered (`cap == 0`): `engine.New`, `tools.NewHostTool` and `tools.NewCommand` refuse a buffered one | `engine` `TestNewRefusesHostCodeInKernelPorts`; `tools` `TestConstructorsRefuseHostCode` |


### Second review re-verification (`a7f97a5`)

The second reviewer re-verified findings 1–9 and the buffered-channel concern
as closed and found one more defect: a refused request reused the admitted
run's event hand-off IDs (`<execution>/<attempt>/event-1`), which a host
deduplicating by ID would ack without recording. Refused runs now use a
per-refusal nonce namespace (`…/refused-<nonce>/event-<seq>`); regression
`engine` `TestRefusedRequestNeverReusesAnAdmittedEventID`.

Gate A: isolated kernel complete. Gate B (production adoption, #518): pending.
