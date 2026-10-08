# Deliberate breaks (#446 §14, acceptance row A17)

Independent review lane record. Every break below was applied to a
**disposable clone**, never to the real worktree, and nothing broken was
committed anywhere that is shared.

## Status (updated after `1406256`)

| Finding | Status | Regression check |
| --- | --- | --- |
| F1 negative reported usage renewed budget | fixed in `1406256` (`engine.plausibleUsage`) | `tests/conformance` `TestFindingNegativeReportedUsageRenewsBudget` (skip removed) |
| F2 a command that ran was hidden behind the stop | fixed in `1406256` (`run.observeUncertainMutation`) | `TestFindingCommandRanButResultHidesIt` (skip removed) |
| F3 unknown/incomplete stop settled `completed` | fixed in `1406256` (stop other than `end` → `failed/provider_failed`) | `TestFindingUnknownStopSettlesCompleted` (skip removed), `engine` `TestRefusalAndOutputBound` |
| F4 `check-imports.sh` passed on unresolvable imports | fixed in `1406256` (`go list` errors fail the script) | breaks 1a–1c re-run below |

Re-verification after the fix: breaks 1a, 1b and 1c are now caught by
`scripts/check-imports.sh`, which prints
`go: can't load test package: ... no required module provides package ...`
and then exits 1. The "not caught (F4)" cells in the table below record the
state before `1406256`.

## Bindings

| Item | Value |
| --- | --- |
| Branch / commit | `claude/446-agent-kernel-gate-a` at `4696e31d3dbd5aefe864d982e3a7f4e2af6eb9a2`, plus the uncommitted module tree of 2026-10-07 (wave-1 lanes, harness `cmd/`, `examples/`, `testdata/`, and `tests/`) synced over it |
| Disposable copy | `git clone --local --no-hardlinks` of the worktree into the session scratchpad; the module tree was snapshotted as a local commit in the clone only |
| Scope base | `refs/remotes/origin/main` of the source repository, fetched into the clone as `upstream/main` (merge-base `27b7c30e0088ec28130b34d9604cb12a945f918c`); every scope command ran with `BASE_REF=upstream/main` |
| Toolchain | `go1.27.1 darwin/arm64`, `GOWORK=off`, `GOFLAGS=-count=1`; module `go 1.25.0` |
| Revert | `git reset && git checkout -- . && git clean -fd -- agentkernel` after every break |

Commands, run from `agentkernel/` of the clone:

| Name | Command |
| --- | --- |
| build | `go build ./...` |
| vet | `go vet ./...` |
| imports | `bash scripts/check-imports.sh` |
| arch | `go test ./tests/architecture` |
| extract | `bash scripts/extract.sh` |
| scope | `BASE_REF=upstream/main bash scripts/check-scope.sh` |
| lane / conformance | the focused `go test` named in the row |

## Results

"Caught" means a non-zero exit with the decisive line shown. Every row was
followed by the repaired pass recorded at the end.

| # | Break | build | vet | imports | arch | extract | scope | behavioural tests |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1a | parent `runtime` import, production file | caught | caught | **not caught (F4)** | caught | caught | n/a | n/a |
| 1b | same, `_test.go` | passes (expected) | caught | **not caught (F4)** | caught | caught | n/a | n/a |
| 1c | same, `//go:build linux` (on darwin) | passes (expected, other GOOS) | passes (expected) | **not caught (F4)** | caught | caught (via arch test) | n/a | n/a |
| 1d | same, `//go:build ignore` | passes (expected) | passes (expected) | passes (expected) | caught | caught (via arch test) | n/a | n/a |
| 2a | parent-relative storage read (`../../runtime/store.go`, `../../runtime.db`) | passes (expected) | — | — | caught | caught (via arch test) | passes (expected: in-allowlist path) | n/a |
| 2b | symlink fixture `testdata/parent-runtime -> ../../runtime` | passes (expected) | — | — | caught | caught (via arch test) | caught | n/a |
| 3 | mutate root `README.md` | — | — | — | — | — | caught | n/a |
| 4 | remove the grant-kind check in the broker | — | — | — | — | — | — | caught (lane + conformance) |
| 5 | demote large mandatory context to optional | — | — | — | — | — | — | caught (lane + conformance) |
| 6a | index identity ignores build tags / cgo | — | — | — | — | — | — | caught (lane) |
| 6b | overlay deletion keeps the deleted file | — | — | — | — | — | — | caught (lane) |
| 7a | unknown cost reported as known | — | — | — | — | — | — | caught (lane + conformance) |
| 7b | routing ignores host eligibility | — | — | — | — | — | — | caught (lane + conformance) |
| 8 | unlocked (racy) ledger reservation | — | — | — | — | — | — | caught under `-race` only (lane) |
| 9 | late cancel overwrites observed termination | — | — | — | — | — | — | caught (lane + conformance) |
| 10 | memory items promoted to system text | — | — | — | — | — | — | caught by conformance only; the engine lane test does not catch it |

Every break is caught by at least one required check. The finding is F4:
`scripts/check-imports.sh` printed `imports: ok` and exited 0 for 1a, 1b and
1c although `go list` failed on the parent import (see Findings).

## Breaks, patches and decisive output

Digests are `sha256` of the exact patch text (`git diff` in the clone).

### 1. Parent `runtime` import (production, test, build-tagged)

```diff
# 1a  sha256:c18d6753ebcb486619c44d39c03716c2629976e6f673dd609a07c4bfeaba91de
+++ b/agentkernel/engine/zz_parent.go
+package engine
+
+import _ "github.com/bogdaniel/zenchron-engineering/runtime"
# 1b  sha256:d15175b93911177e693b06ebb2f3d69ed23a8daddebdbe5636ede23d1a72cb3d
+++ b/agentkernel/engine/zz_parent_test.go   (same content)
# 1c  sha256:cc0c87498cd63b6d2086e349e43c34d7eb89da41a62f9bd7124a5a3d7ec3701a
+++ b/agentkernel/engine/zz_parent_tagged.go
+//go:build linux
+
+package engine
+
+import _ "github.com/bogdaniel/zenchron-engineering/runtime"
# 1d  sha256:3449436279d811f6865de2a096ab96e35448870b6e83c62ddd76428c81b1a4e2
+++ b/agentkernel/engine/zz_parent_ignored.go   (as 1c with //go:build ignore)
```

| Variant | Check | Decisive line |
| --- | --- | --- |
| 1a | build, vet | `engine/zz_parent.go:3:8: no required module provides package github.com/bogdaniel/zenchron-engineering/runtime` |
| 1a | extract | `engine/zz_parent.go:3:8: cannot find module providing package github.com/bogdaniel/zenchron-engineering/runtime: import lookup disabled by -mod=readonly` |
| 1a–1d | arch | `--- FAIL: TestImportsStayInsideModuleAndStdlib` with `engine/zz_parent*.go:N: import "github.com/bogdaniel/zenchron-engineering/runtime": parent repository package outside github.com/bogdaniel/zenchron-engineering/agentkernel` |
| 1b | vet, extract | `engine/zz_parent_test.go:3:8: no required module provides package ...` |
| 1c, 1d | extract | `--- FAIL: TestImportsStayInsideModuleAndStdlib` (extract runs the module tests, including tests/architecture) |
| 1a–1c | imports | `go: can't load test package: ... no required module provides package ...` then **`imports: ok`**, exit 0 (F4) |

### 2. Parent-relative storage read and symlink fixture

```diff
# 2a  sha256:e81774196ae9d0801bc02535c942f406802de28865a0435284c619ad778e8c75
+++ b/agentkernel/storage/zz_parent_read.go
+func readParent() ([]byte, error) {
+	if _, err := os.ReadFile(filepath.Join("..", "..", "runtime", "store.go")); err != nil {
+		return nil, err
+	}
+	return os.ReadFile("../../runtime.db")
+}
# 2b  sha256:2641bbc99587075cf8350961389697bf22c95318dd2a84a9ed7140dd8461e3ad
+++ b/agentkernel/testdata/parent-runtime   (new file mode 120000)
+../../runtime
```

| Variant | Check | Decisive line |
| --- | --- | --- |
| 2a | arch, extract | `--- FAIL: TestNoParentCheckoutPathLiterals` (`string "../../runtime/store.go" climbs out of the module directory`; `string "../../runtime.db" names the host runtime database`) |
| 2b | arch, extract | `--- FAIL: TestNoSymlinkEscapesModule` (`testdata/parent-runtime: symlink escapes the module` in the checkout; `unresolvable symlink` in the extracted copy) |
| 2b | scope | `scope: symlink escapes the module: agentkernel/testdata/parent-runtime -> ../../runtime` |

### 3. Out-of-allowlist write

```diff
# sha256:2fc31b3f2679b86e0e45f17cae76e8be9e9ef26da1f0837af22fb47aca129f20
--- a/README.md
+++ b/README.md
+
+scope probe
```

scope: `scope: out-of-allowlist path: README.md` (exit 1).

### 4. Capability check removed

```diff
# sha256:0277cfa16ce541e42babe7f5c5a4f63c8055f5692f6cf891b46435cef2415617
--- a/agentkernel/tools/broker.go
-		if g.Kind == t.Kind() && covers(g, scope) {
+		if covers(g, scope) {
```

`go test ./tools -run Dispatch`: `--- FAIL: TestDispatchRefusesBeforeReachingTheTool`;
`go test ./tests/conformance -run A07`: `--- FAIL: TestA07ProhibitedInvocationsCannotMutate`
(a write under the `file.read` grant mutated the workspace).

### 5. Mandatory context dropped

```diff
# sha256:13beac32adc66da217f96240f1c62b89cca10acc36c2f6a828fd448510f34296
--- a/agentkernel/engine/compile.go
-		if it.Required {
+		if it.Required && len(it.Content) < 1024 {
 			required = append(required, it)
 		} else {
+			it.Required = false
 			optional = append(optional, it)
```

`go test ./engine ./context -run Required`: `--- FAIL: TestRequiredContextOverCapacityBlocks`;
`go test ./tests/conformance -run A09`: `--- FAIL: TestA09ImpossibleRequiredContextIsTypedBlock`
and `--- FAIL: TestA09RequiredContextSurvivesTightBudget`. The survive test caught
this only after the review added its manifest `required` assertion; before
that, a demoted tier-1 task item still fitted and the test passed.

### 6. Index reused after build-config change / source deletion

```diff
# 6a  sha256:da3fdc03fcfdc2f07a3a671ca557a43793f405452c16f98ce0a15af300fbebf8
--- a/agentkernel/intelligence/identity.go
-		Settings:         settings,
+		Settings:         Settings{GoVersion: settings.GoVersion, GOOS: settings.GOOS, GOARCH: settings.GOARCH},
# 6b  sha256:9199eac0aacb898a0787535ad29bcf0a01a0e159b9f668dfde40d4c4bffaacc0
--- a/agentkernel/intelligence/overlay.go
 		if c.Delete {
-			delete(entries, c.Path)
```

`go test ./intelligence`: 6a `--- FAIL: TestOverlayBuildTagChangeInvalidatesEverything`;
6b `--- FAIL: TestOverlayDeleteRemovesRelations`.

### 7. Unknown price as zero / ineligible provider chosen

```diff
# 7a  sha256:873ed58d2629c02fa70ddffb757c8a6a84758094d7589bce49c1047b11963ae9
--- a/agentkernel/engine/usage.go
-		Cost:      api.Cost{Known: a.costKnown && a.calls > 0, Micros: a.costMicros},
+		Cost:      api.Cost{Known: a.calls > 0, Micros: a.costMicros},
# 7b  sha256:2620dfc895459c31e442e3d001d24847d1ba4bfc7699db4088dc208bb85f2b7a
--- a/agentkernel/routing/select.go
-	if !b.Eligible {
+	if false && !b.Eligible {
```

7a: `go test ./engine -run 'Money|Retries|Budget'`: `--- FAIL: TestMoneyCeiling`;
`go test ./tests/conformance -run A05`: `--- FAIL: TestA05RetriesNeverRenewBudget`.
7b: `go test ./routing`: `--- FAIL: TestCheaperIneligibleNeverChosen`;
`go test ./tests/conformance -run A13`: `--- FAIL: TestA13BindingSurvivesRoutingThroughEngine`.

### 8. Racy reservations

```diff
# sha256:974e0dc43fbf734f00dd8361e05e83fe427c705717a34708bfe09030453e3594
--- a/agentkernel/engine/budget.go
 func (l *ledger) reserve(amounts ...amount) (api.BudgetDimension, bool) {
-	l.mu.Lock()
-	defer l.mu.Unlock()
```

`go test -race ./engine -run Concurrent`: `WARNING: DATA RACE`, then
`--- FAIL: TestConcurrentReservationsCannotOverspend`. Without `-race` the
count assertion is probabilistic; the race detector is the decisive check, so
the CI race job is required, not optional.

### 9. Late cancel overwrites the observed termination

```diff
# sha256:e60b87bed0e42279ed587bea53fd3ae58525281bfdadc3de83eecd88c2174a93
--- a/agentkernel/engine/run.go
 func (r *run) finish(ctx context.Context, t api.Termination) api.ExecutionResult {
+	if r.parent.Err() != nil && t.Outcome != api.OutcomeCancelled {
+		t = r.termination(api.OutcomeCancelled, api.CauseHostCancelled, "late cancel")
+		t.Cancellation = api.CancellationOf(r.parent)
+	}
```

`go test ./engine -run LateCancel`: `--- FAIL: TestObservedOutcomeSurvivesLateCancel`;
`go test ./tests/conformance -run A06`: `--- FAIL: TestA06ObservedTerminationSurvivesLateCancel`.

### 10. Remembered instructions promoted to host instructions

```diff
# sha256:f798a39691ed8ffb189599ae84f0194e9720b2dfc678fea51d5b9d802269f341
--- a/agentkernel/engine/transcript.go
-	return it.Trust == api.TrustHost && (it.Kind == api.ContextInstruction || it.Kind == api.ContextConstraint)
+	return (it.Trust == api.TrustHost || it.Trust == api.TrustMemory) && (it.Kind == api.ContextInstruction || it.Kind == api.ContextConstraint || it.Kind == api.ContextMemory)
```

`go test ./tests/conformance -run 'Trust|Untrusted'`:
`--- FAIL: TestTrustBoundaryRememberedInstructionsNeverBecomeSystem`.
`go test ./engine ./memory -run Trust` **passes**: the engine's own
`TestTrustBoundary` has no `kind=memory` item, so it does not see this
promotion. The conformance test is the check that catches it.

## Repaired pass

After the last revert, in the same clone: build, vet, imports, arch, extract
(`extract: ok (... built, vetted and tested in /var/folders/.../agentkernel-extract.*)`),
scope (`scope: ok (base 27b7c30e...)`) and every focused lane and conformance
command above exited 0.

## Findings from this exercise

- **F4 (medium; fixed in `1406256`): `scripts/check-imports.sh` cannot fail
  on an unresolvable import.** `go list -deps -test ./...` runs inside a process substitution
  (`done < <(go list ...)`), whose exit status `set -euo pipefail` does not
  observe. A parent-module import is exactly an import `go list` cannot
  resolve, so the script prints the error and then `imports: ok` with exit 0
  (breaks 1a, 1b, 1c). For 1c on darwin this script was the only toolchain
  check meant to see the linux-tagged file. Defence in depth held
  (tests/architecture and extract caught every variant), but the script
  itself is vacuous for the case it exists for. Coordinator-owned; repair by
  capturing `go list` output into a variable (or a temp file) and failing on
  its exit status before scanning.

## Review 5443104514 guards

Each guard added for human review 5443104514 was removed (or weakened as
described), the named focused test was run and failed with the line below,
and the guard was restored and the same test passed again. Breaks were applied
in the implementing lane's working tree and never committed. Commits:
`22247ed` (storage, item 3), `ece635d` (tools, item 2), `c8c54f1` (engine,
items 4–6); the root break belongs to PR #511 (item 1), outside the #446
allowlist.

File:line values are as recorded when the break ran. `engine/integrity_test.go`
was revised afterwards (comments and subtests), so its recorded lines no longer
match the current file; the test and subtest names below locate each check.

### Storage (item 3, `22247ed`)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| `occupiedSize` counts file length minus the header line, never the header's `size` claim | header-claim accounting restored (the old `storedSize`) | `artifacts_test.go:256: reopen: used = 16, want 41` (`TestFileArtifactsAccountOccupiedBytesNotHeaderClaims`) | passes |
| `Put` charges a repair for its growth over the counted bytes (`setCount`) | old repair arithmetic (an existing key is never re-charged or re-counted) | `artifacts_test.go:268: repair: used = 25, want 40` | passes |

### Tools (item 2, `ece635d`)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| `refuseSymlinks` on every component of the requested path | replaced by an `Lstat` of the final name only | `link_alias_test.go:73: read_file "meta/config" leaked .git content: ... [remote] url=gitsecret` (`TestSymlinkAliasToGitMetadataIsRefused`) | passes |
| `checkOpened` post-open `os.SameFile` against an `Lstat` of the name | `SameFile` check removed | `link_alias_test.go:130: a file whose name was swapped after the open was accepted` (`TestCheckOpenedRefusesSwappedFile`) | passes |
| `hardLinked` (unix `Nlink > 1`) | always returns false | `link_alias_unix_test.go:18: read_file "src/h" leaked .git content: ... url=gitsecret`; `write_file src/h` also succeeds (`TestHardLinkAliasToGitMetadataIsRefused`) | passes |

### Engine (items 4–6, `c8c54f1`)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| ledger refuses a negative amount (`errNegativeAmount`, `engine/budget.go`) | check removed | `budget_test.go:59: negative reservation: dim "" err <nil>, want input_tokens errNegativeAmount` (`TestNegativeReservationCannotRenewBudget`) | passes |
| `context.Compile` refuses a negative estimate (`ErrNegativeEstimate`) | check removed | `compiler_test.go:261: req-body: err <nil>, want ErrNegativeEstimate` (`TestNegativeEstimateIsRefused`) | passes |
| prompt-overhead estimate check (`engine/compile.go`) | check removed | `integrity_test.go:52: termination completed/loop_completed ..., want failed/provider_failed` (`TestNegativeEstimateFailsClosed/overhead`; removed in `0c2e7ef`) | passes |
| per-attempt estimate check (`engine/loop.go`) | check removed | `0 provider calls, detail "negative input_tokens reservation refused"` (`TestNegativeEstimateFailsClosed/reservation`, recorded at `integrity_test.go:47`; removed in `0c2e7ef`): the ledger root still failed closed; the test requires the estimator-named detail | passes |
| impossible cache partition makes input and cache parts unknown (`plausibleUsage`), sum leg | check removed | `termination completed/loop_completed ..., want exhausted/budget_exhausted` (`TestImpossibleCachePartitionGrantsNoRefund/parts_sum_exceeds_input`, recorded at `:76`) | passes |
| same, single-part leg | check removed | `termination completed/loop_completed (provider stopped proposing tools), want exhausted/budget_exhausted` (`.../one_part_exceeds_input`, recorded at `:82`) | passes |
| byte upper-bound input reservation (`inputReservation`) | estimate forced `Exact: true`, so the ~bytes/4 estimate is reserved as is | `termination completed/loop_completed ..., want exhausted/budget_exhausted` (`TestInputBudgetIsAHardBound/estimate_fits_bound_does_not`, recorded at `:102`); `2 provider calls, want 1` (`.../unreported_input_keeps_the_bound`, recorded at `:131`) | passes |
| re-entry refusal (`admissionRefusal`), engine | check removed | `admission_test.go:49: termination completed/loop_completed ..., want blocked/invalid_request` (`TestReentryCannotRenewBudget/same_attempt_refused`) | passes |
| re-entry refusal, conformance across restart | check removed | `budget_integrity_test.go:43: termination exhausted/budget_exhausted (no model turns remain), want blocked/invalid_request` (`TestA05ReentryAcrossRestartCannotRenewBudget`) | passes |
| widening refusal (`widenedBound`) | check removed | `admission_test.go:86: termination completed/loop_completed ..., want blocked/invalid_request` (`TestReentryCannotRenewBudget/widened_*`) | passes |
| unsettled-prior-attempt refusal | check removed | `admission_test.go:142: termination incomplete/recording_failed (...), want blocked/invalid_request` (`TestUnsettledAttemptBlocksTheNext`) | passes |
| ledger starts from recorded consumption (`ledger.restore`) | restore removed | `admission_test.go:62: termination completed/loop_completed ..., want exhausted/budget_exhausted` (`TestReentryCannotRenewBudget/later_attempt_starts_from_consumed`) | passes |
| in-flight marker write error refuses the attempt (`run.admit`) | error ignored | `admission_test.go:156: 1 provider calls, detail "consumption of attempt \"att-1\" not recorded: ..."` (`TestAdmissionWriteFailureRefusesBeforeSideEffects`) | passes |
| admission serialized by `Engine.admitMu` | mutex removed | `admission_test.go:199: only 0 of 7 concurrent attempts were refused; more than one was admitted` (`TestConcurrentAttemptsAdmitExactlyOne`) | passes |
| bounded `EventSink.Record` (`run.bounded`) | unbounded call | `record_test.go:91: Execute still running 2.4s after the bound; a stuck sink held it open` (mid-loop, host-cancel and terminal subtests of `TestStuckEventSinkCannotHoldExecutionOpen`) | passes |
| bounded `ContextSource.ContextItems` | unbounded call | `record_test.go:138: Execute still running after deadline + grace; a stuck source held it open` (`TestStuckContextSourceCannotHoldExecutionOpen`) | passes |
| host cancellation shortens a bounded wait to the grace | shortening removed | `record_test.go:91: Execute still running 2.1s after the bound` (`TestStuckEventSinkCannotHoldExecutionOpen/mid_loop_host_cancel`) | passes |
| bounded admission read, write and settlement | unbounded calls | `record_test.go:209: Execute still running 2.4s/2.1s after the bound; a stuck admission store held it open` (`TestStuckAdmissionStoreCannotHoldExecutionOpen`; removed in `0c2e7ef`) | passes |
| wall-clock deadline check in `run.interrupted` (`engine/loop.go`) | wall-time comparison removed | `interrupt_test.go:32: interrupted = {...}, false; want exhausted on the deadline` (`TestDeadlineHoldsBeforeItsTimerFires`) | passes |

Superseded by `0c2e7ef` (kept as the record of what `c8c54f1` was checked
against): the prompt-overhead and per-attempt estimate guards no longer exist,
because the `api.TokenEstimator` port was removed (`TestNegativeEstimateFailsClosed`
and conformance `TestA05NegativeEstimateCannotMintBudget` were deleted with
it); `run.bounded` and `engine/record_test.go` were replaced by
`internal/handoff.Exchange` and `engine/handoff_test.go`, where
`TestStuckEventSinkCannotHoldExecutionOpen` and
`TestStuckContextSourceCannotHoldExecutionOpen` now live and
`TestStuckAdmissionStoreCannotHoldExecutionOpen` was deleted (the admission
store is kernel-owned `*storage.FileRecords`, no longer a host port); the
`Engine.admitMu` admission mutex was replaced by the `PutIfAbsent` claim, and
`TestUnsettledAttemptBlocksTheNext` and
`TestAdmissionWriteFailureRefusesBeforeSideEffects` moved to
`engine/claim_test.go`. The follow-up section below re-breaks the
replacements.

Timer race. The wall-clock check closes a race found under full-suite load: a
bounded port call abandoned at the budget deadline returned before the run
context's deadline timer fired, so the run took one more step and settled
`completed` after a stuck context source. It did not reproduce in isolation
(0 failures in 60 runs of the stuck-source test), so the invariant is pinned
deterministically by `TestDeadlineHoldsBeforeItsTimerFires`, which uses an
injected clock that has not reached the deadline and a context with no
deadline.

### Root prerequisite (item 1, PR #511)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| the root `ExecutionRequest{` producer scan (`executionRequestOffenders`) stops at any directory with its own `go.mod` | `go.mod` skip removed | `offenders = ["a.go: ..." "nested/b.go: ..."], want only the root module's a.go` (`TestExecutionRequestScanSkipsNestedModules`) | passes |

## Review 5443104514 follow-up: hand-off and atomic admission

Commit `0c2e7ef` (with the module ADR update `a2b551f`) replaced bounded
synchronous port calls with hand-offs and made admission an atomic claim
(review items 5 and 6, as decided). Same protocol: guard removed, focused test
failed with the line below, guard restored, test passed. File:line values are
as recorded; for `engine/handoff_test.go:157` and `:214` the recorded line is
the shared `execute` helper call inside the named test.

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| `handoff.Exchange` starts no goroutine (send and wait are `select`s against the bound) | the call is handed off by a kernel goroutine (`go func(){ out <- call }()`) | `engine/handoff_test.go:157: 1 goroutines outlived Execute beyond the 6 running before it: the kernel leaked one` (`TestStuckEventSinkCannotHoldExecutionOpen`; the provider case fails the same way at `:214`, `TestStuckProviderCannotHoldExecutionOpen`); `internal/handoff/handoff_test.go:67: err handoff: not answered ... want ErrNotTaken at the bound` (`TestExchangeNeverTaken`) | passes |
| the bound timer in `newWait` | timer removed | `engine/handoff_test.go:157: Execute still running 2.4s after the host got stuck; the host held it open` (`TestStuckEventSinkCannotHoldExecutionOpen`) | passes |
| `FileRecords.PutIfAbsent` links the synced temp file (`os.Link`, fails if the name exists) | `os.Link` replaced by `os.Rename` (replaces an existing name) | `storage/records_test.go:215: 16 writers created the key, want exactly 1` (`TestPutIfAbsentAdmitsExactlyOne`) | passes |
| `run.admit` refuses when the claim is held (`storage.ErrExists`) | `ErrExists` ignored | `engine/claim_test.go:116: more than one contender was admitted` (`TestTwoEnginesOneRootAdmitExactlyOne`); `engine/claim_test.go:142: termination completed/loop_completed ..., want blocked/invalid_request` (`TestCrashedAttemptBlocksTheNext`); `engine/admission_test.go:144: only 0 of 7 concurrent attempts were refused` (`TestConcurrentAttemptsAdmitExactlyOne`) | passes |

### Cross-process claim (OS processes)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| `os.Link` (fails if the name exists) in `FileRecords.PutIfAbsent` | replaced by `os.Rename` (overwrites) | `records_process_test.go:61: 8 processes created the claim, want exactly 1` | passes |

## Second review (adversarial): `a7f97a5`

Findings of the second review are labelled `R2-F<n>` to keep them apart from
the first review lane's F1–F4 above.

Commit `a7f97a5` repaired the second, adversarial review's findings R2-F1–R2-F9 and
the buffered-channel concern (`gate-a.md` §7). Same protocol: guard removed or
weakened as described, focused test failed with the line below, guard
restored, test passed. File:line values are as recorded; the
`budget_test.go:131` line is inside `TestUnansweredProviderCallKeepsReservations`,
and `host_test.go:83` / `:113` are the `dispatchWithin` calls in
`TestHostToolHandOff` / `TestCommandHandOff`.

| Finding | Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- | --- |
| R2-F1 | ledger subtraction check | removed | `overflow_test.go:26: 2 provider calls: a second call was admitted after a reported input of 9223372036854775807 consumed max_input_tokens` (`TestHugeReportedInputStopsFurtherCalls`) | passes |
| R2-F1 | saturating ledger addition | removed | `budget_test.go:87: input_tokens consumed -2, want saturation at MaxInt64` (`TestLedgerSaturatesInsteadOfWrapping`) | passes |
| R2-F1 | negative-prior guard on restore | removed | `budget_test.go:87: tool_calls consumed -5, want saturation at MaxInt64` | passes |
| R2-F1 | saturated cost is unknown | saturated cost reported known | `budget_test.go:105: cost of 9223372036854775807 tokens reported known` (`TestOutOfRangePriceIsUnknown`) | passes |
| R2-F1 | price range guard | removed | `budget_test.go:114: price(NaN) = 0, want saturation at MaxInt64`; on amd64 the partial break fails at `budget_test.go:105 ... -9223372036854775808 micros` | passes |
| R2-F2 | exact-type check on `Clock` and `Artifacts` in `engine.New` | removed | `seal_test.go:59: engine.New accepted it (err <nil>)` (`TestNewRefusesHostCodeInKernelPorts`) | passes |
| R2-F2 | exact-type check on tools in `tools.NewBroker` | removed | `host_test.go:155: NewBroker accepted a host type embedding a kernel tool` (`TestConstructorsRefuseHostCode`) | passes |
| R2-F3 | admission restores the largest cumulative record | records summed again | `admission_test.go:189: attempt 4 exhausted/budget_exhausted/iterations no model turns remain after only 3 of 5 iterations were used` (`TestPriorConsumptionIsCountedOnce`) | passes |
| R2-F4 | tool hand-off IDs are kernel sequences | model tool-call ID reused as the hand-off ID | `handoff_ids_test.go:92: hand-off ID "exec-1/att-1/event-3" sent for 2 different requests` (`TestModelToolCallIDsNeverBecomeHandOffIDs`) | passes |
| R2-F4 | repeated model tool-call ID refused | refusal removed | `handoff_ids_test.go:97: the repeated model tool-call ID was dispatched as "exec-1/att-1/tool-2"` | passes |
| R2-F5 | Anthropic adapter: a negative or overflowing usage part makes input unknown | guard removed | `anthropic_test.go:211: ...want input and both cache parts unknown` (`TestNegativeOrOverflowingUsagePartMakesInputUnknown`) | passes |
| R2-F5 | engine: same rule in `plausibleUsage` | guard removed | `overflow_test.go:62: reported input 10 cached <nil> write <nil>; a negative part must make all three unknown` (`TestNegativeUsagePartMakesInputUnknown`) | passes |
| R2-F6 | `Exchange` never offers a call once its bound is over | expired-bound check removed | `handoff_test.go:140: call 1: err handoff: not answered: x did not answer call id within 0s, want ErrNotTaken` (`TestExpiredBoundHandsNothingOver`) | passes |
| R2-F7 | `search` skips any `.git` entry | directories only | `file_tools_test.go:267: search echoed .git file content: "other/.GIT:1: gitdir: gitsecret ..."` (`TestSearchSkipsGitFiles`) | passes |
| R2-F8 | a closed reply channel is no answer | closed reply read as a value | `handoff_test.go:160: closed reply read as answer <nil> (err <nil>), want ErrNoAnswer` (`TestClosedReplyIsNoAnswer`); engine: `handoff_ids_test.go:124: nothing was recorded, yet the result reports 6 recorded events and completed/loop_completed` (`TestClosedEventReplyIsRecordingFailure`) | passes |
| R2-F9 | every widening dimension in `widenedBound` | dimensions removed | `admission_test.go:86` / `:96`: want `blocked/invalid_request` (`TestReentryCannotRenewBudget`, `widened_*`) | passes |
| R2-F9 | an unanswered provider call (`ErrNoAnswer`) keeps its reservations | reservations released | `budget_test.go:131: input_tokens charged 0 after handoff: not answered, want 10` (`TestUnansweredProviderCallKeepsReservations`) | passes |
| R2-F9 | tool and command hand-offs are bounded by the run context | bound removed | `host_test.go:113` / `:83`: `Dispatch still waiting 2s after its context ended` (`TestCommandHandOff`, `TestHostToolHandOff`) | passes |
| buffered channels | `engine.New`, `tools.NewHostTool`, `tools.NewCommand` refuse a buffered hand-off channel | `cap == 0` check removed | `seal_test.go:59` (engine, `TestNewRefusesHostCodeInKernelPorts`); `host_test.go:161: NewHostTool accepted a buffered calls channel`; `host_test.go:164: NewCommand accepted a buffered runner channel` (`TestConstructorsRefuseHostCode`) | passes |

### Refusal ID namespace (second-review re-verification)

| Guard | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| per-refusal nonce namespace in `run.refuse` | `r.refusal = ""` | `refusal_ids_test.go:42: hand-off ID "exec-1/att-1/event-1" sent for 3 different events: [execution.started execution.refused execution.refused]` | passes |

### Contract v0.2: attempt-scoped deadline (#518)

Applied to a disposable copy of the module, one break at a time, each reverted
before the next; `go test -count=1 ./engine/ ./tests/conformance/ ./api/`.

| Rule | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| a new attempt may carry a later deadline | `widenedBound` compares the deadline again | `admission_test.go:75: termination blocked/invalid_request (budget widens deadline …), want exhausted/budget_exhausted` (`later_deadline_new_attempt_admitted_from_consumed`); `budget_integrity_test.go:81` (`TestA05DeadlineIsAttemptScoped`) | passes |
| the same `attempt_id` is refused, whatever its deadline | settled-attempt check disabled | `admission_test.go:86: termination completed/loop_completed …, want blocked/invalid_request` (`same_attempt_later_deadline_refused`); `budget_integrity_test.go:75: same attempt with a later deadline: {Outcome:exhausted …}` | passes |
| cumulative bounds still cannot widen | `max_tool_calls` case removed | `admission_test.go:111` (`widened_max_tool_calls`); `budget_integrity_test.go:81: later deadline with a widened tool-call bound: {Outcome:exhausted …}` | passes |
| consumption carries forward | `ledger.restore` dropped | `admission_test.go:75` (later-deadline attempt completed instead of exhausting iterations); `TestPriorConsumptionIsCountedOnce` | passes |
| v0.1 requests are refused | `Validate` also accepts `agentkernel.execution/v0.1` | `execution_test.go:85` (`TestA04RefusedBeforeAnySideEffect/v0.1_version`); `schema_test.go:164: want *api.ValidationError on "version", got <nil>` (`request-v0.1-version.json`) | passes |

Review corrections on #530 (versioned admission records checked before the
claim, kernel identity, additive historical v0.1 artifacts), same procedure:

| Rule | Patch | Decisive failure | Restored |
| --- | --- | --- | --- |
| an incompatible record is refused before any claim | version checked only after claiming (pre-claim check removed) | `admission_version_test.go:67: refusal left a claim held by "att-2"` (`TestUnreadableRecordIsRefusedBeforeClaiming`, every case) | passes |
| a legacy record seen after the claim is refused | post-claim version recheck removed | `admission_version_test.go:93: admitClaimed = {…}, true; want refused with "legacy unversioned (v0.1)"` (`TestLegacyRecordAfterClaimReleasesTheClaim`) | passes |
| that refusal releases the claim | `releaseClaim` skipped on post-claim refusal | `admission_version_test.go:96: claim held by "att-2", want ""`; `budget_integrity_test.go:50` | passes |
| a failed release fails closed and says so | release error dropped | `admission_version_test.go:93: … want refused with "admission claim not released: delete refused"` | passes |
| a legacy unversioned (v0.1) record fails closed | `versionRefusal` accepts any unversioned record | `admission_version_test.go:64`; `claim_test.go:260: termination completed/loop_completed …, want blocked/invalid_request` (`TestUnreadableAdmissionVersionFailsClosed`) | passes |
| a present unversioned record is legacy whatever it contains | the old `rec.Version == "" && len(rec.Attempts) == 0` shortcut restored | `admission_version_test.go:64: admit = {…}, true; want refused with "legacy unversioned (v0.1)"` (`legacy_zero_attempts`, `legacy_empty_object`); `claim_test.go:260: termination completed/loop_completed …, want blocked/invalid_request` (same cases, FileRecords); `admission_version_test.go:122: recordConsumption = <nil>; want the legacy refusal` (`TestSettlementNeverExtendsALegacyRecord`) | passes |
| an unknown record version fails closed | unknown version accepted | `admission_version_test.go:64`; `claim_test.go:260` | passes |
| every new record carries `agentkernel.admission/v0.2` | version not written on creation | `admission_test.go:47: detail "… legacy unversioned (v0.1) admission state …", want it to say "already admitted"` (and `:60`, `:75`, `:86`, `:111`) | passes |
| `api.KernelVersion` is `agentkernel/0.2.0` | reverted to `agentkernel/0.1.0-gate-a` | `schema_test.go:148: example is agentkernel.execution/v0.2 / agentkernel/0.2.0, want current … / agentkernel/0.1.0-gate-a` | passes |
| v0.1 schemas are retained | `*.v0.1.schema.json` deleted | `schema_test.go:248: glob ../schemas/*.v0.1.schema.json: <nil> (0 matches)` (`TestHistoricalArtifactsParse`) | passes |
| v0.1 examples are retained | `schemas/examples/valid/` deleted | `schema_test.go:249: glob ../schemas/examples/valid/*.json: <nil> (0 matches)` | passes |
