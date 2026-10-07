# Deliberate breaks (#446 §14, acceptance row A17)

Independent review lane record. Every break below was applied to a
**disposable clone**, never to the real worktree, and nothing broken was
committed anywhere that is shared.

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

- **F4 (medium): `scripts/check-imports.sh` cannot fail on an unresolvable
  import.** `go list -deps -test ./...` runs inside a process substitution
  (`done < <(go list ...)`), whose exit status `set -euo pipefail` does not
  observe. A parent-module import is exactly an import `go list` cannot
  resolve, so the script prints the error and then `imports: ok` with exit 0
  (breaks 1a, 1b, 1c). For 1c on darwin this script was the only toolchain
  check meant to see the linux-tagged file. Defence in depth held
  (tests/architecture and extract caught every variant), but the script
  itself is vacuous for the case it exists for. Coordinator-owned; repair by
  capturing `go list` output into a variable (or a temp file) and failing on
  its exit status before scanning.
