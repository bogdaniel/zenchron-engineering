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

EVIDENCE_HEAD: recorded by the binding commit (see the PR).

| Item | Value |
| --- | --- |
| Branch | `claude/518-gateb-adapter` |
| Base | `60ae3c968592bb13ca4d4d197caf5aa087abd81b` (main, merge of #527) |
| Toolchain | `go version go1.27.1 darwin/arm64` |
| Environment | offline: scripted provider only, no network, no credentials, no spend |

### Commands and results (on EVIDENCE_HEAD, 2026-10-08)

| Command | Result |
| --- | --- |
| `test -z "$(gofmt -l .)"` | pass |
| `go vet ./...` and `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `go build ./...` | pass |
| `go test ./execution/... -count=1 -race` | `ok execution`, `ok execution/agentkernel` |
| `cd agentkernel && GOWORK=off go test ./... -count=1` | every package `ok`, module untouched |
| `go list -deps ./... \| grep agentkernel` | `api`, `engine`, `storage`, `tools` directly, and their own dependencies (`context`, `routing`, `internal/handoff`, `internal/strictjson`); only `execution/agentkernel` imports them |
| `python3 scripts/check_file_sizes.py` | no file of this change warns or fails (largest: `adapter.go`, 353 lines); the checker's FAIL rows are pre-existing files outside this change |

### Acceptance rows covered offline

| #518 acceptance | Test (`execution/agentkernel`) |
| --- | --- |
| 1 translation round-trip, no widening default | `TestEveryRequestFieldIsClassified`, `TestTranslationMapsEveryHostField`, `TestReadOnlyModeGrantsNoWriteOrCommand`, `TestBudgetNeverWidensAHostBound`, `TestUnmappableHostFieldsAreRefused`, `TestConfigRefusesImplicitBounds` |
| 2 termination mapping incl. late cancellation | `TestTerminationMappingTable`, `TestLateCancellationDoesNotRewriteAnObservedOutcome` |
| 3 operator stop / shutdown keep the host owner | `TestHostCancellationKeepsTheHostOwner`, `TestKernelCauseCarriesMatchingProvenance` |
| 4 recording failure (transcript half only, §3 seam 1) | `TestRecordingFailureStopsFurtherSideEffects` |
| 5 budgets: observation-only usage, refusal, admission | `TestUsageIsObservationOnly`, `TestUnmappableHostFieldsAreRefused`, `TestReenteredAttemptIsRefused`, `TestConcurrentAdmissionAdmitsOne`, identity tests |
| inactivity (plan §5.1) | `TestInactivityIsEnforcedByTheAdapter`, `TestProgressRenewsTheInactivityBound` |
| uncertain side effect (plan §5.7) | `TestUncertainMutationIsSurfaced`, `TestUncertainSideEffectIsNeverNothingChanged` |
| hand-off workers bounded to Execute | `TestNoWorkerOutlivesExecute` |
| `MissingTools` (#522) | `TestMissingToolsProbesTheHostBoundary` |
| import boundary | `execution/architecture_test.go` R2, R3 |

Rows 6–12 are not covered by this stage.

### Deliberate breaks

Each guard was broken in place, the named tests run, and the source restored.
All 19 adapter mutations were caught; the decisive line is quoted.

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
| uncertain side effect hidden | `adapter_test.go:151: uncertain mutation hidden` |
| unknown output usage read as zero | `result_test.go:100: unknown usage reported as tokens` |
| watchdog cancels as a shutdown | `adapter_test.go:92: kernel termination {… Cancellation:controller_shutdown …}` |
| host cause not translated to kernel provenance | `adapter_test.go:63: operator stop: … Cancellation:unknown …, want cancellation "operator_stop"` |
| transcript failure swallowed | `adapter_test.go:127: error <nil> is not a *TerminationError` |
| workers never stopped | `adapter_test.go:245: 6 goroutines after Execute, 2 before` |
| in-memory admissions per engine | `adapter_test.go:169: error <nil> is not a *TerminationError` |
| catalogue not consulted by MissingTools | `adapter_test.go:267: missing [], want npm (not catalogued)` |
| R3 not relaxed | `architecture_test.go:126: R3: …/adapter.go imports …/agentkernel/api: no package of this module may import the agent kernel` |
| `cmd/` imports the kernel | `architecture_test.go:128: R3: …/zz_mut.go imports …/agentkernel/api: only execution/agentkernel may import …` |
| a subpackage of the adapter imports the kernel | `architecture_test.go:128: R3: …/sub/x.go …` |
| the adapter imports `runtime` | `architecture_test.go:114: R2: … an execution adapter may not import runtime` |

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
`lp(s) = len(s) ":" s`. One host operation is one kernel execution envelope;
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
| `InactivityLimit` | none in the kernel | enforced by the adapter: progress = each recorded kernel event; expiry cancels with `execution.ErrProviderInactive` → `provider_no_progress`, owner `inactivity` |
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

1. **Progress recording has no error channel.** `execution/progress.go:37`
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

1. Close or decide seams 1, 2, 3, 5 and obligation 11.
2. Composition and registration: a new, default-off agent kind in
   `cmd/zenchron-engineering/agentkernel_*.go` and the agent registry.
3. Root docs (§9 of the plan) for the adopted scope and the new kind.
4. Live conformance and comparative dogfood under approved spend.
5. The named retirement step for `runtime/openai_provider.go` and the
   model-facing `ToolBroker` surface, after parity.
