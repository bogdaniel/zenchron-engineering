# Agent Execution Kernel Gate B acceptance record (#518)

**Status: Gate B: in progress — offline adapter evidence.** Gate B is not
accepted. This record is marked accepted only after comparative dogfood
(#518 acceptance 10), live adapter conformance under an approved spending
bound (12), and the proof that the duplicate OpenAI loop was retired (#518
work item 4). Gate A's record is `agentkernel/docs/acceptance/gate-a.md`.

Governing decisions: ADR-0005 (the execution port), ADR-0006 (adopt), the
approved write allowlist and stop rules on #518, and
`agentkernel/docs/integration-plan.md` §3–§8.

## 1. Stage 1: host adapter and offline conformance

### Scope

| File | Change |
| --- | --- |
| `execution/agentkernel/**` (new) | the host adapter: an `execution.Port` over the kernel, its translation, its owned workers, and offline conformance tests |
| `execution/architecture_test.go` | R3 relaxed to exactly `execution/agentkernel` importing the kernel's public packages; everything else still refused |
| `go.mod` | `require github.com/bogdaniel/zenchron-engineering/agentkernel v0.0.0` and `replace … => ./agentkernel`; no `go.sum` entry (the kernel is stdlib-only) |
| this file | the Gate B record |

Nothing composes or registers the adapter. No `runtime/**`, `cmd/**` or
`agentkernel/**` file changed. The adapter is **not production-eligible**
(§3, obligation 11).

### Evidence binding

A commit cannot contain its own hash, so the binding lands in a later commit
that changes only this file; verify with
`git diff --name-only EVIDENCE_HEAD <binding commit>`.

EVIDENCE_HEAD: `bc976cf0d11e3c6b0eee09e3ffa34630dc5226a4` (branch `claude/518-gateb-adapter`). Superseded first binding: `ac4b61f`, before the corrections the owner asked for on #518 (comment 6047604378).

| Item | Value |
| --- | --- |
| Branch | `claude/518-gateb-adapter` |
| Base | `60ae3c968592bb13ca4d4d197caf5aa087abd81b` (main, merge of #527) |
| Toolchain | `go version go1.27.1 darwin/arm64` |
| Environment | offline: scripted provider only, no network, no credentials, no spend |

### Corrections after the owner's review of #529

1. The inactivity watchdog recomputes silence from a monotonic last-observed
   progress time when its timer fires, and re-arms for the remainder. It no
   longer cancels on a bare timer edge.
2. Progress is observed when an event is received, before the transcript
   write. Write latency never counts as silence, and a write failure stays
   `recording_failed` on its own.
3. Wording: the length-prefixed encoding is injective; SHA-256 is
   collision-resistant, not injective.

### Commands and results (on EVIDENCE_HEAD, 2026-10-08)

| Command | Result |
| --- | --- |
| `test -z "$(gofmt -l .)"` | pass |
| `go vet ./...` and `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `go build ./...` | pass |
| `go test ./execution/... -count=1 -race` | `ok execution`, `ok execution/agentkernel` |
| `cd agentkernel && GOWORK=off go test ./... -count=1` | every package `ok`, module untouched |
| `go list -deps ./... \| grep agentkernel` | `api`, `engine`, `storage`, `tools` directly, and their own dependencies (`context`, `routing`, `internal/handoff`, `internal/strictjson`); only `execution/agentkernel` imports them |
| `python3 scripts/check_file_sizes.py` | no file of this change warns or fails (largest: `adapter.go`, 354 lines); the checker's FAIL rows are pre-existing files outside this change |

### Acceptance rows covered offline

| #518 acceptance | Test (`execution/agentkernel`) |
| --- | --- |
| 1 translation round-trip, no widening default | `TestEveryRequestFieldIsClassified`, `TestTranslationMapsEveryHostField`, `TestReadOnlyModeGrantsNoWriteOrCommand`, `TestBudgetNeverWidensAHostBound`, `TestUnmappableHostFieldsAreRefused`, `TestConfigRefusesImplicitBounds` |
| 2 termination mapping incl. late cancellation | `TestTerminationMappingTable`, `TestLateCancellationDoesNotRewriteAnObservedOutcome` |
| 3 operator stop / shutdown keep the host owner | `TestHostCancellationKeepsTheHostOwner`, `TestKernelCauseCarriesMatchingProvenance` |
| 4 recording failure (transcript half only, §3 seam 1) | `TestRecordingFailureStopsFurtherSideEffects` |
| 5 budgets: observation-only usage, refusal, admission | `TestUsageIsObservationOnly`, `TestUnmappableHostFieldsAreRefused`, `TestReenteredAttemptIsRefused`, `TestConcurrentAdmissionAdmitsOne`, identity tests |
| inactivity (plan §5.1) | `TestInactivityIsEnforcedByTheAdapter`, `TestProgressObservedAtTheEdgeWins`, `TestSlowTranscriptWriteIsNotInactivity`, `TestGenuineSilenceIsInactivity`, `TestCompletionConcurrentWithExpiryIsPreserved` |
| uncertain side effect (plan §5.7) | `TestUncertainMutationIsSurfaced`, `TestUncertainSideEffectIsNeverNothingChanged` |
| hand-off workers bounded to Execute | `TestNoWorkerOutlivesExecute` |
| `MissingTools` (#522) | `TestMissingToolsProbesTheHostBoundary` |
| import boundary | `execution/architecture_test.go` R2, R3 |

Rows 6–12 are not covered by this stage.

### Deliberate breaks

Each guard was broken in place, the named tests run, and the source restored.
All 19 adapter mutations and 5 of 6 watchdog mutations were caught (the sixth is explained below the table); the decisive line is quoted.

| Break | Caught by |
| --- | --- |
| execution_id includes the attempt | `identity_test.go:16: attempts of one operation have execution ids … a retry would escape the envelope` |
| raw separator concatenation | `identity_test.go:36: separator in run vs operation: execution ids collide` |
| each token dimension gets the whole total | `request_test.go:180: total 2 split into input 2 + output 2` |
| tiny token total not refused | `request_test.go:261: token total too small: … want a refusal of budgets.max_tokens` |
| cost ceiling ignored without a rate card | `request_test.go:261: cost without pricing: error <nil>, want a refusal` |
| result slots not refused | `request_test.go:261: reviewer slot: error <nil>, want a refusal of reviewer_result_path` |
| whole command catalogue granted | `request_test.go:140: command grants [go-test npm-test], want only go-test` |
| findings elevated to host trust | `request_test.go:117: untrusted text reached host item finding-0` |
| kernel allowed to retry (poll) | `request_test.go:147: kernel retries 3: retries and waits are the host's` |
| kernel latch decides the cancellation class | `TestHostCancellationKeepsTheHostOwner`, `TestInactivityIsEnforcedByTheAdapter` fail |
| a late cancellation claims the termination | `adapter_test.go:80: late stop rewrote the outcome` |
| uncertain side effect hidden | `adapter_test.go:135: uncertain mutation hidden` |
| unknown output usage read as zero | `result_test.go:100: unknown usage reported as tokens` |
| watchdog cancels as a shutdown | `adapter_test.go:92: kernel termination {… Cancellation:controller_shutdown …}` |
| host cause not translated to kernel provenance | `adapter_test.go:63: operator stop: … Cancellation:unknown …, want cancellation "operator_stop"` |
| transcript failure swallowed | `adapter_test.go:111: error <nil> is not a *TerminationError` |
| workers never stopped | `adapter_test.go:229: 6 goroutines after Execute, 2 before` |
| in-memory admissions per engine | `adapter_test.go:153: error <nil> is not a *TerminationError` |
| catalogue not consulted by MissingTools | `adapter_test.go:251: missing [], want npm (not catalogued)` |
| watchdog: plain select (expiry cancels without re-reading progress) | `watchdog_test.go:109: the watchdog armed no timer` (edge test) and `:141` (slow write) |
| watchdog: write time counted as silence (no in-flight suppression) | `watchdog_test.go:141: the watchdog armed no timer` |
| watchdog: no refresh when the write ends | `watchdog_test.go:148: the watchdog armed no timer` |
| watchdog never cancels | `watchdog_test.go:116` and `:158: genuine silence was not cancelled` |
| expiry rewrites an observed completion | `watchdog_test.go:190: observed completion rewritten by the expiry: {… Outcome:failed …}` |
| R3 not relaxed | `architecture_test.go:126: R3: …/adapter.go imports …/agentkernel/api: no package of this module may import the agent kernel` |
| `cmd/` imports the kernel | `architecture_test.go:128: R3: …/zz_mut.go imports …/agentkernel/api: only execution/agentkernel may import …` |
| a subpackage of the adapter imports the kernel | `architecture_test.go:128: R3: …/sub/x.go …` |
| the adapter imports `runtime` | `architecture_test.go:114: R2: … an execution adapter may not import runtime` |

The sixth watchdog break, removing the refresh on receipt, survives. While
the host writes an event the watchdog counts no silence, and the refresh
when the write ends restarts the clock, so the refresh on receipt has no
separate observable effect. It stays as the defined observation point. A
slow write is covered by the write-end refresh and its break; a failing
write by the recording-failure test.

## 1b. Stage 2: fallible progress seam

Authority: the owner's decision on #518 (comment 6047604378, item 2) and the
later review conditions: no double write of one event, and a final progress
failure never rewrites the termination owner.

### Scope

| File | Change |
| --- | --- |
| `execution/progress.go` | `ProgressWriter func(Progress) error`, `WithProgressWriter`, `ProgressWriterFrom`; `ProgressRecorder` unchanged |
| `runtime/operations.go` | installs the writer, returning `RecordProviderProgress`'s error (3778 → 3780 lines; PR-specific #495 override needed) |
| `execution/agentkernel/{adapter,workers,result}.go` | the adapter writes progress through the fallible writer |
| `execution/progress_test.go`, `runtime/progress_writer_test.go`, `execution/agentkernel/progress_test.go` (new) | focused tests |

### Semantics

- `WithProgressWriter` installs the writer AND the best-effort recorder,
  derived from the same write with the error dropped. A native provider
  (the CLI inactivity recorder) sees the recorder it always saw: same
  synchronous write, same asynchronous coalescer above it, errors dropped.
- The adapter uses only the writer when one is installed, never also the
  recorder, so each kernel event is written to the progress row once. With
  only a recorder it falls back to it, wrapped never to fail (best effort, as
  before); with neither it writes nothing.
- An in-flight progress write failure is returned from the event sink after
  the transcript write: the kernel settles `recording_failed`
  (→ `execution_incomplete`) and takes no further side effect.
- A failed final write comes after settlement. A success becomes
  `failed`/`execution_incomplete` with the write error; a result that already
  failed or was cancelled keeps its class (it routes the host) and joins the
  error. The invocation's termination owner is never rewritten.
- A progress write in flight is host time, never provider silence, exactly
  like a transcript write.

### Evidence binding

EVIDENCE_HEAD: `a179c2ae225f793b7e88006163d81d10838f73b3` (branch
`claude/518-progress-writer`, base `c0bc2e9`, main after #531). This record's
binding lands in the next commit, which changes only this file.

### Commands and results (on EVIDENCE_HEAD, 2026-10-08, go1.27.1 darwin/arm64)

| Command | Result |
| --- | --- |
| `test -z "$(gofmt -l .)"` | pass |
| `go vet ./...` and `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `go build ./...` | pass |
| `go test ./execution/... -race -count=1` | `ok execution`, `ok execution/agentkernel` |
| `go test ./runtime/ -count=1 -run 'Progress\|ProviderInactivity\|InactivityCause\|Coalescing\|ProgressWriter'` | `ok runtime` (pins the existing coalescing and inactivity recorder behaviour) |
| `python3 scripts/check_file_sizes.py` | `runtime/operations.go` FAILs as before (3778 → 3780); no other touched file warns |

### Tests

| Behaviour | Test |
| --- | --- |
| writer installs the derived recorder; absent writer is nil | `TestProgressWriterInstallsTheBestEffortRecorder`, `TestProgressWriterAbsentOrNil` |
| runtime returns the record error; the recorder still writes when healthy | `TestRuntimeProgressWriterReturnsTheRecordError` |
| in-flight failure → `recording_failed`, no further call | `TestInFlightProgressWriteFailureIsARecordingFailure` |
| final failure → incomplete, owner kept | `TestFinalProgressWriteFailureMakesTheResultIncomplete`, `TestFinalProgressWriteFailureKeepsAFailedClass` |
| one write per event with both surfaces installed | `TestBothSurfacesWriteEachEventOnce` |
| no writer → best effort as before | `TestNoProgressWriterIsBestEffort` |
| slow progress write is not inactivity | `TestSlowProgressWriteIsNotInactivity` |

### Deliberate breaks

All 11 caught; each was applied in place, the named tests run, and the source
restored.

| Break | Caught by |
| --- | --- |
| writer installs no recorder | `progress_test.go:24: installing a writer left no best-effort recorder` |
| runtime writer swallows the error | `progress_writer_test.go:64: RecordProviderProgress failed and the writer reported success` |
| runtime installs only a recorder | `progress_writer_test.go:61: the runtime installed no fallible progress writer` |
| derived recorder writes nothing | `progress_writer_test.go:67: progress key "": the native recorder path must still write the row` |
| in-flight progress error ignored | `agentkernel/progress_test.go:50: error <nil> is not a *TerminationError` |
| final write error dropped | `agentkernel/progress_test.go:66: error <nil>, want the final write's failure` |
| final failure rewrites the owner | `agentkernel/progress_test.go:73: invocation … TerminationCause: …: a recording failure after settlement must not rewrite why the provider ended` |
| final failure overwrites a failed class | `agentkernel/progress_test.go:106: class "execution_incomplete", want the routing class kept` |
| adapter also calls the recorder | `agentkernel/progress_test.go:94: 7 recorder calls and 7 writes for 6 events: want 0 and one per event plus the final` |
| no recorder fallback | `agentkernel/progress_test.go:129: recorder got [], want the projection ending in a final write` |
| progress write counted as silence | `agentkernel/progress_test.go:152: the watchdog armed no timer` |

## 2. Design of stage 1

### Ownership

The host keeps run/attempt identity, budgets and their carry-forward,
cancellation provenance, the termination owner, recording, candidate
authority, waits and retries. The adapter translates, owns the hand-off
workers of exactly one `Execute`, and runs one bounded kernel execution. It
adds no scheduler, store of host records, journal, policy, authority or
lifecycle owner: the kernel's own file stores (admissions, tool-output
artifacts) live under an injected `StateDir`.

### Identity

`execution_id = "zx-" + hex(SHA-256(lp(RunID) lp(OperationID)))`,
`attempt_id = "za-" + hex(SHA-256(lp(RunID) lp(OperationID) lp(Attempt)))`,
`lp(s) = len(s) ":" s`. The length-prefixed input encoding is injective,
so it is unambiguous. SHA-256 is collision-resistant, not injective: two
distinct identities sharing an id would need a SHA-256 collision. One host operation is one kernel execution envelope;
one physical attempt is one attempt_id. Admissions are durable
`storage.FileRecords`, so a re-entered attempt is refused and two handles
admit one.

### Request translation

`TestEveryRequestFieldIsClassified` holds the authoritative per-field table.
In short:

- Host trust only from `TrustedInstructions`, `Instructions`, `Purpose`
  (instructions) and `Contract`, `AcceptanceObligations`, `Constraints`,
  `Prohibitions`, `Permissions` (constraints).
- `Findings` (tool_output), `Feedback` (workspace) and `Upstream`
  (model_derived) are required, untrusted observations.
- Grants: file read/search at the candidate root, file write only in a
  mutating mode, commands only as operator catalogue entries whose
  executable the contract's `RequiredTools` names.
- Refused: the five result slots/inbox, a model preference other than the
  bound model, an unknown mode, a non-absolute candidate dir, a missing tree.
  Host-only: `PriorAttemptFailure`, `SourceSnapshot`, `ControllerID`, `Base`.

### Budget mapping and refusal rules

| Host bound | Kernel | Rule |
| --- | --- | --- |
| `Deadline`, `WallLimit`, context deadline | `Budget.Deadline` | earliest of them and the adapter's `MaxWall` |
| `MaxTokens` (one total) | `MaxOutputTokens = min(limit, T/2)`, `MaxInputTokens = min(limit, T − out)` | `in + out ≤ T` by construction; `T < 2` refuses |
| `MaxCostMicros` | `Money{CostCurrency, MaxCostMicros}` | refused unless the binding has a complete trusted rate card in the configured cost currency |
| `InactivityLimit` | none in the kernel | enforced by the adapter. Progress is the monotonic instant a kernel event is received, taken before its transcript write. While a write is in flight no silence is counted, and the clock restarts when it ends, so storage latency never reads as inactivity. A write failure is `recording_failed` on its own. The timer only wakes the watchdog, which re-reads the last progress and cancels with `execution.ErrProviderInactive` (→ `provider_no_progress`, owner `inactivity`) only if the silence reached the limit; otherwise it re-arms for the remainder |
| retries | `MaxProviderRetries = 0` | the host owns retries and waits; a kernel retry is immediate |
| iterations, tool calls, artifact bytes | the adapter's explicit `Limits` | no host bound exists |

Kernel `Tokens`/`CostMicros` are observations only: reported (never estimated)
input + output when both are known, cost only when `known`, otherwise nil.

### Result translation

`TestTerminationMappingTable` is the total mapping. A cancellation's class and
owner come from the host cause (`execution.CancellationClass`,
`execution.OwnerOfCause`); the kernel's provenance only has to agree. No
sixth owner exists. Pre-execution refusals return no invocation provenance and
`Executed=false`. `tool_failed`, and any `tool.executed` "side effect outcome
uncertain", set `TerminationError.UncertainSideEffect`: the host reconciles
the candidate from the workspace, never "nothing changed".

### Host-worker interfaces (AGENTS.md §6)

| Interface | Gate | Purpose |
| --- | --- | --- |
| `TranscriptWriter` | (b) durable I/O | per-event forensic transcript; its error is the recording failure |
| `CommandRunner` | (b) process I/O | granted commands and toolchain probes through the host's sandbox |

Credentials need no adapter interface: a live provider adapter resolves its
own through the `api.CredentialSource` the composition root gives it.

## 3. Missing seams and obligations carried

1. **Progress recording has no error channel.** *Closed by stage 2 (§1b) on
   EVIDENCE_HEAD `a179c2a`; the text below is the original finding.* `execution/progress.go:37`
   (`WithProgressRecorder(ctx, record func(Progress))`) and `:46` return no
   error, and the host callback discards the write's result
   (`runtime/operations.go:921`,
   `_, _ = r.scheduler.RecordProviderProgress(...)`). The adapter cannot observe
   a durable progress failure, so it projects kernel events onto the recorder
   best-effort and claims `recording_failed` only for the transcript writer.
   Decision needed: change the progress contract (scope expansion: execution
   and runtime) or narrow #518 acceptance 4 to the transcript.
2. **No streaming transcript in the host artifact store.**
   `ArtifactStore.StoreExecutionAttemptTranscript` (`runtime/sandbox.go:765`)
   writes once, at the end. A `TranscriptWriter` must append per event, so the
   composition needs a new host writer (or an amended store) to make
   per-event durability real.
3. **Credential-shaped path/content refusal (plan §5.3).** Kernel built-in file
   tools have no host guard hook, and `runtime.GuardCandidate*`
   (`runtime/adapters.go:215,229,255`) cannot be applied to them. Closing it
   needs host file tools (`tools.NewHostTool`, served by adapter workers) over
   an injected guarded-file interface the composition implements with the
   surviving `ToolBroker` resolve gate. Blocks composition.
4. **Command runner.** `DockerSandbox` exposes no exported single-command run;
   the exported path is `ToolBroker.RunCommand` (`runtime/tool_broker.go:341`),
   a retirement-step file. ADR-0006 keeps the resolve gate and `DockerSandbox`
   as runner implementations, so that method must survive retirement or move.
   `MissingTools` probes through the runner (`sh -c 'command -v'`), so the
   runner must accept a probe with an empty `Dir`.
5. **Retry deadline vs the kernel envelope (contradiction).** The host
   re-anchors each retry's deadline at its lease (`runtime/scheduler.go:668`),
   so a retry after a durable wait carries a later deadline, which the kernel
   refuses within one execution as "budget widens deadline"
   (`TestRetryWithALaterDeadlineIsRefusedByTheKernelEnvelope`). With
   execution_id per operation (the binding identity rule), retries after waits
   are refused. Resolving it needs a decision: a kernel change (agentkernel is
   frozen), host deadline semantics, or an adapter-side clamp that reads the
   first envelope.
   **Resolved: kernel v0.2 adopted** (#530, owner decision on #518): the
   kernel contract is `agentkernel.execution/v0.2`, whose deadline is
   attempt-scoped. A retry with a later deadline is admitted and starts from
   recorded consumption; re-running a physical attempt and widening a
   numeric bound are still refused
   (`TestRetryWithALaterDeadlineIsAdmittedFromRecordedConsumption`). Legacy
   v0.1 admission records fail closed until explicitly recovered.
6. **Artifact bridge.** Tool output stays in kernel `FileArtifacts` under
   `StateDir`; `Result.Artifacts` is empty. The host artifact-store bridge is a
   Gate B obligation.
7. **Result slots** (reviewer, feedback resolution, handoff, message) are
   refused: the kernel's tools cannot write outside the candidate (plan §5.9).
8. **Quota vs rate limit (plan §5.6)**: every 429 is `provider_rate_limited`.
9. **Patch semantics (plan §5.4)**: kernel `apply_patch` is one exact
   replacement, not a unified diff; no `Diff` tool.
10. **Isolation (plan §5.5)**: the binding is always `unproven`.
11. **Abandoned admission claim (plan §5.11): not production-eligible.** A crash
    after `PutIfAbsent` leaves the claim held for ever and refuses every later
    attempt of that operation. An explicit recovery protocol is required
    before registration. No TTL may ever be added.
12. **Live adapter verification (plan §5.13)** needs approved credentials and
    spend.

## 4. What remains

1. Close or decide seams 2, 3, 5 and obligation 11 (seam 1 closed, §1b).
2. Composition and registration: a new, default-off agent kind in
   `cmd/zenchron-engineering/agentkernel_*.go` and the agent registry.
3. Root docs (§9 of the plan) for the adopted scope and the new kind.
4. Live conformance and comparative dogfood under approved spend.
5. The named retirement step for `runtime/openai_provider.go` and the
   model-facing `ToolBroker` surface, after parity.
