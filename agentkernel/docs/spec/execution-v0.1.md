# Execution contract v0.1 (`agentkernel.execution/v0.1`)

Normative for Gate A. The words MUST/MUST NOT describe behaviour the code in
`api/` and `engine/` implements; where the code and issue #446 differ, §12 says
so. Canonical JSON shapes: `schemas/execution-request.v0.1.schema.json`,
`schemas/execution-result.v0.1.schema.json`, `schemas/event.v0.1.schema.json`,
`schemas/common.v0.1.schema.json`, with examples under `schemas/examples/`.

## 1. Entry point

```go
type Executor interface {
    Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error)
}
```

`engine.New(engine.Config)` returns the implementation. `New` defaults nothing:
`Events`, `Clock` and at least one non-nil entry in `Providers` are required; a
non-nil `Broker` requires `Artifacts` and `OutputLimit > 0`; `Sources` entries
must be non-nil. Every hand-off channel (`Providers`, `Events`, `Sources`)
must be unbuffered (`cap == 0`), or `New` refuses it (§4.1). Two fields have stated defaults: `Admissions` (nil: an
in-memory `storage.MemoryRecords` owned by the Engine, §11.1) and
`SettleTimeout` (zero: `engine.DefaultSettleTimeout`, 5 s; negative is
refused; §4.1). Every port a host can implement is a hand-off channel
(§4.1); every value the kernel calls directly is of a kernel-owned concrete
type, checked by exact type in `New` so a host type embedding one is refused:
`Artifacts` a `*storage.MemoryArtifacts` or `*storage.FileArtifacts`, `Clock`
an `api.SystemClock` or non-nil `*api.ManualClock`, `Admissions` a
`*storage.FileRecords`, `Broker` a `*tools.Broker` (whose tools `NewBroker`
checks the same way).
`(*Engine).Execute` always returns a settled result and a nil error, including
for refused requests.

## 2. Request (`api.ExecutionRequest`)

| Field | Meaning |
| --- | --- |
| `version` | MUST equal `api.ExecutionVersion` (`agentkernel.execution/v0.1`). |
| `execution_id`, `attempt_id` | Host identities, echoed in every event and the result. `execution_id` keys the execution-wide budget envelope shared by all its attempts (§11); an `attempt_id` is admitted at most once per execution. |
| `objective` | Host text, non-blank, at most `api.MaxObjectiveBytes` (64 KiB). Rendered as the user turn's framing; not system text. |
| `mode` | `read_only` or `read_write` (see capabilities spec). |
| `workspace` | `WorkspaceRef{id, manifest_digest, git_revision?, dirty}`. The manifest digest identifies content; a Git revision alone never does. |
| `constraints` | `required_features`, `require_host_proven_isolation`, `instruction_digest` (recorded in provenance, never interpreted). |
| `context` | `[]ContextItem`; `required: true` items are never dropped (§6). |
| `grants` | `[]Capability`; the only source of tool authority. |
| `budget` | Finite envelope for the whole execution, all attempts together (§7, §11). |
| `providers` | `[]ProviderBinding`; the host's eligibility decisions. No credential value; `credential_handle` names one the host resolves. |

### 2.1 Validation (`ExecutionRequest.Validate(now)`)

Runs before any provider, tool or source call. It performs no I/O. A failure
settles `blocked/invalid_request` (§5). It refuses:

- an unknown `version`;
- `execution_id`, `attempt_id`, `workspace.id` not matching
  `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`;
- a blank or oversized `objective`; an unknown `mode`;
- `workspace.manifest_digest` or `constraints.instruction_digest` not
  `sha256:<64 hex>`; a `git_revision` that is not a full 40- or 64-hex name;
- a duplicate or unsupported `required_features` entry (supported:
  `api.SupportedFeatures` = `tools`, `artifacts`, `cancellation_provenance`);
- a context item with an invalid id, unknown kind or trust, a
  `content_digest` that is not the digest of `content`, an invalid `ref`, a
  duplicate id, or kind `instruction`/`constraint` without `trust: host`;
- grants with an invalid or duplicate handle, unknown kind, a mutating kind in
  `read_only` mode, or the wrong shape (§ capabilities spec);
- a deadline not after `now`; any of `max_iterations`, `max_tool_calls`,
  `max_input_tokens`, `max_output_tokens`, `max_artifact_bytes` not positive;
  `max_provider_retries` outside `0..api.MaxProviderRetries` (10); a money
  ceiling without an ISO-4217-shaped currency or a positive `max_micros`;
- no provider binding; duplicate binding ids; a binding with an invalid id or
  kind, empty `model`/`model_version`/`config_fingerprint` (an unpinned version
  is the literal `unknown`), unknown isolation, non-positive `context_window`
  or `max_output_tokens`, `pinned` without `eligible`, an invalid
  `credential_handle`, or malformed pricing; more than one pinned binding;
- a money ceiling when any **eligible** binding lacks a complete trusted rate
  card in the ceiling's currency (`Pricing.prices`: currency matches, input,
  output, cached-input and cache-write rates all known and non-negative,
  `source` and `version` set). Outside a ceiling, a rate card may leave the
  cached-input or cache-write rate `null` (unknown, never zero); a negative
  rate is refused (`Pricing.valid`).

Checks run in a fixed order, so the first invalid field reported is
deterministic (`execution_id`, `attempt_id`, `workspace.id`, then the order
above).

### 2.2 Canonical decoding (`api.DecodeRequest`)

A host that receives JSON MUST decode with `DecodeRequest`, which refuses
unknown fields, duplicate keys at any depth, non-lower-case keys (Go's decoder
matches field names case-insensitively, so `Mode` would otherwise override
`mode`) and trailing data. `Execute` receives a Go value and cannot detect
these itself.

## 3. Result (`api.ExecutionResult`)

Observation only. There is no acceptance, merge, authorization or candidate
field, by construction.

| Field | Meaning |
| --- | --- |
| `version`, `execution_id`, `attempt_id` | Echoed. |
| `termination` | The single settled `Termination{outcome, cause, dimension?, cancellation?, detail?, observed_at}`. |
| `final_text` | Last provider text; set only when outcome is `completed`. |
| `observations` | Every recorded event, in order, as `{seq, kind, detail, tool_call, grant, ref}` (no usage). |
| `artifacts` | Every artifact stored through the engine's metered store during the execution. |
| `usage` | §7.5. |
| `context` | The `ContextManifest`, once compiled. |
| `routing` | The `RoutingDecision`, once routed. |
| `provenance` | `kernel_version` (`api.KernelVersion` = `agentkernel/0.1.0-gate-a`), chosen provider id/kind/model, `model_version_bound` and `model_version_observed`, `config_fingerprint`, workspace id and digest, `instruction_digest`, `context_manifest_digest`, `isolation` (the chosen binding's host statement; `unproven` before routing), `sessions` (§9). |
| `event_count` | Number of events recorded (equals `len(observations)`). |

## 4. Events (`api.Event`, `api.EventSink`)

Every event carries `version`, `execution_id`, `attempt_id`, `seq` (1, 2, 3 …
per execution, no gaps among recorded events), `kind`, `observed_at` (from
`api.Clock`), `source` (`agentkernel/engine`), and optionally `detail`,
`tool_call`, `grant`, `ref`, `usage`. Events are enough for host accounting;
they are not an engineering journal.

| Kind | Emitted when |
| --- | --- |
| `execution.refused` | validation failed (then `execution.settled`) |
| `execution.started` | validation passed |
| `routing.decided` | after `routing.Select`; detail names the choice or the block |
| `context.compiled` | after `context.Compile`; detail has the manifest digest and source notes |
| `provider.requested` | before each provider attempt, after reservation |
| `provider.responded` | after a successful call; carries reported `usage` |
| `provider.failed` | after a failed call; detail is the redacted `ProviderError` |
| `tool.proposed` | per tool call, after its reservation, before dispatch |
| `tool.executed` / `tool.refused` | after dispatch (`refused` for broker refusals); also, when a tool that reports `mutated` ran and the execution then stops, a `tool.executed` with detail `side effect outcome uncertain: …` (`run.observeUncertainMutation`), so `exhausted`/`failed` is never read as "no side effect happened" |
| `artifact.recorded` | each artifact a tool stores |
| `execution.settled` | exactly once, last, unless recording already failed |

There is no separate exhaustion event: exhaustion is visible in
`execution.settled` and in `Termination.dimension`.

### 4.1 Host ports: bounded hand-off

Rule: the bounded execution path never synchronously executes host-owned code
whose termination it cannot enforce. Every host port is a channel of
`api.Call[Q, R]{ID, Context, Request, Reply}` read by **host-owned workers**
(`api.ServeEvents`, `api.ServeContext`, `api.ServeProvider`,
`api.ServeCommands`, `tools.ServeTool`, or the host's own goroutines). The
kernel side is one function, `internal/handoff.Exchange`:

- it sends the call with a `select` against the bound (enqueueing is bounded),
  then waits for the reply with a `select` against the bound (waiting is
  bounded); it starts **no goroutine**, so when the bound passes nothing of
  the kernel's is left waiting and no kernel goroutine outlives `Execute`;
- it distinguishes a call **never taken** (`handoff.ErrNotTaken`: the host
  never saw it, so it had no effect) from one **taken but not answered**
  (`handoff.ErrNoAnswer`: its effect is unknown). "Never taken" means "never
  seen" only because every host hand-off channel is unbuffered: a send
  completes only when a worker receives. `engine.New`, `tools.NewHostTool`
  and `tools.NewCommand` refuse a channel with `cap != 0`;
- a bound already over when `Exchange` starts (its time passed, or `Shorten`
  already closed with no grace) is checked without blocking before the call
  is offered, so nothing is handed over (a `select` would choose among ready
  cases at random); once the send has succeeded, the bound changes nothing
  about it having been taken;
- `Reply` is created by the kernel per call with capacity 1, so a worker's
  single reply never blocks, even after the kernel stopped waiting; a late
  reply is discarded and changes nothing. Workers never close it; a reply
  channel closed without a value is `handoff.ErrNoAnswer` (outcome unknown),
  never the zero value read as an answer (for an event, "durable").
- `Context` carries the bound and is cancelled when the kernel stops waiting.
- `ID` is stable and unique within the attempt, and always kernel-generated:
  `<execution>/<attempt>/event-<seq>`, `…/source-<i>`, `…/provider-<n>`, and
  for tools and commands the call's producer `…/tool-<n>`, `n` numbering the
  attempt's dispatched tool calls. No part comes from model output: the
  model's tool-call ID could repeat, or mimic another port's ID. A proposal
  whose tool-call ID repeats an earlier one of the attempt is refused back to
  the model (`tool.refused`) and never dispatched. The kernel never sends one
  ID twice; a host seeing it again treats it as the same request.
- Queue capacities: the `Serve*` helpers return unbuffered channels and answer
  one call at a time; reply channels have capacity 1. A host wanting parallel
  answers runs its own workers on its own channel.

The host owns its workers and their lifetime: they must outlive `Execute` (so
a cancelled execution can still record how it ended) and stop when the host
cancels their context. A worker whose callee ignores its context strands the
host's goroutine, never the kernel's.

| Host-owned callback reachable from `Execute` | Reached through | Bound | Not taken | Taken, not answered |
| --- | --- | --- | --- | --- |
| `EventSink.Record` (in-loop event) | `Config.Events` | `budget.deadline`; cut to the settlement grace by a host cancellation, also mid-wait; the grace from now once the deadline passed or the host cancelled | recording failure (§10) | recording failure (§10) |
| `EventSink.Record` (`execution.refused`, `execution.settled`) | `Config.Events` | one settlement grace, fixed when settlement begins | recording failure | recording failure |
| `ContextSource.ContextItems` | `Config.Sources` | as an in-loop event | source noted unavailable | source noted unavailable |
| `Provider.Complete`, and the host code adapters call (`CredentialSource`, HTTP `Doer`) | `Config.Providers` (`api.ServeProvider`, for kernel adapters too) | `budget.deadline`; the grace after a host cancellation | no call counted, every reservation released; settles on the deadline or cancellation | call counted with unknown usage, every reservation kept; settles on the deadline or cancellation |
| `CommandRunner.Run` | `tools.NewCommand(chan<- api.CommandCall, dir)` | the run context (deadline or host cancellation) | tool error, command did not run | `mutated`, outcome unknown: `failed/tool_failed`, observed as `side effect outcome uncertain` |
| host tools | `tools.NewHostTool(HostTool{…, Calls})` | the run context | tool error, did not run | outcome unknown; for a mutating kind `failed/tool_failed` and observed as uncertain |

Kernel-owned code is called directly, because the kernel controls its
termination: the built-in file tools, `tools.Broker` (`tools.NewBroker`
accepts only package `tools`' own concrete tool type), the stores of package
`storage` (`Config.Artifacts` accepts only `*storage.MemoryArtifacts` and
`*storage.FileArtifacts`; admissions take `*storage.FileRecords`), and the
clocks (`Config.Clock` accepts only `api.SystemClock` and `*api.ManualClock`).
These are exact-type checks, not interface marker methods: a host type that
embeds a kernel type to inherit a marker, then overrides a method with host
code, is refused at construction.
Removed host callbacks: `api.TokenEstimator` (token counts are the kernel's
own, §7.4) and `tools.SnapshotGuard` (a host compares manifests itself).

`Execute` therefore returns within `budget.deadline` (or the host's
cancellation) plus one settlement grace, plus the kernel's own local
filesystem I/O for artifacts and admissions.

## 5. Outcomes and causes

Derived from `engine/engine.go`, `engine/compile.go`, `engine/loop.go`,
`engine/tools.go` and `engine/run.go`.

| Outcome | Cause | Dimension | Raised when |
| --- | --- | --- | --- |
| `completed` | `loop_completed` | — | a provider response has no tool calls and stop `end` |
| `exhausted` | `budget_exhausted` | `iterations` | no model turn can be reserved |
| `exhausted` | `budget_exhausted` | `output_tokens` | stop `max_tokens`; no output allowance remains; or the reservation fails on output |
| `exhausted` | `budget_exhausted` | `input_tokens` | the transcript estimate exceeds `context_window − max_output`; or the reservation fails on input |
| `exhausted` | `budget_exhausted` | `money` | the worst-case reservation would exceed the ceiling |
| `exhausted` | `budget_exhausted` | `tool_calls` | a proposal cannot be reserved |
| `exhausted` | `budget_exhausted` | `artifact_bytes` | a tool's artifact write was refused by the ledger |
| `exhausted` | `budget_exhausted` | `deadline` | the budget deadline passed (clock or run context) |
| `failed` | `provider_refused` | — | stop `refused` (checked before tool calls) |
| `failed` | `provider_failed` | — | the ledger refused a negative reservation; a non-retryable provider error, or a retryable one with no retry allowance, other than the two classes below (untyped adapter errors become non-retryable `transport`); or a response without tool calls whose stop is `other` or `tool_use` ("provider stopped without completing") |
| `failed` | `tool_failed` | — | `Broker.Dispatch` returned an error: a side effect's outcome is unknown (including a command or mutating host tool taken but not answered within its bound, §4.1) |
| `blocked` | `provider_unavailable` | — | class `rate_limited` or `unavailable` and no retry is permitted |
| `blocked` | `invalid_request` | — | validation failed; admission refused the attempt (already admitted, a widened budget, or a prior attempt in flight or unsettled, §11.1); or `context.Compile` failed for a reason other than capacity |
| `blocked` | `insufficient_capacity` | — | prompt overhead leaves no input capacity, or required context does not fit |
| `blocked` | `no_eligible_provider` | — | routing blocked, or the chosen binding has no configured adapter (no fallback) |
| `cancelled` | `host_cancelled` | — | the caller's context ended; `cancellation` is set (§8) |
| `incomplete` | `recording_failed` | — | an event was refused, or not taken or acknowledged within its bound (§4.1); the attempt's admission claim could not be written or its record read (detail "admission not recorded, nothing ran", no `execution.settled`); or its consumption could not be recorded, or its claim released, at settlement (§11.1) |

`provider_retries` is a ledger dimension but never a termination dimension:
running out of retries settles the provider error's own outcome, with
"after N attempt(s)" in `detail`.

Tool refusals and tool errors (status `refused`/`error` without a Dispatch
error) do not terminate: they return to the model as tool messages.

## 6. Context compilation (engine side)

Request items split into required and optional; `Sources` contribute optional
items only (§6 of the intelligence/memory spec covers sources). Input capacity
is `min(context_window − reserved, max_input_tokens) − overhead`, where
`reserved = min(binding.max_output_tokens, budget.max_output_tokens)` and
overhead is the estimate of the boundary text, objective framing and tool specs.
Required items that do not fit settle `insufficient_capacity` before any
provider call; they are never dropped, truncated or summarized. These are
estimates, used to fit the context window; the hard `max_input_tokens` charge
is the per-attempt reservation of §7.4.

## 7. Budgets

### 7.1 Ledger

`engine.ledger` holds one limit per dimension and reserves atomically across
all dimensions of one operation: every amount or none. Nothing adds to a
limit; retries spend from the same envelope, and a later attempt of the same
execution starts from what earlier attempts consumed (§11). A negative amount
is refused at the root (`errNegativeAmount`, distinct from exhaustion) and
never lowers what was used. Ledger arithmetic never wraps: the limit check is
by subtraction (`n > limit − used`), and a reservation, settlement or restore
whose sum passes `MaxInt64` saturates there and stays saturated, so the
dimension is exhausted (a provider reporting an input count near `MaxInt64`
stops further calls in this attempt and every later one). A negative prior
consumption restored from a record is corrupt, hence unknown, and charged as
saturated. Money prices are computed in `float64` and converted with a range
check: a price `int64` cannot hold saturates (`MaxInt64`); a reported usage
priced that way has an unknown cost and keeps its worst-case money charge.
Settlement replaces a
reservation with an actual charge, which may exceed it; the overrun is kept so
the next reservation fails. Money is always tracked; it refuses only when the
host set a ceiling.

### 7.2 Per operation

| Operation | Reserved before | Settled after |
| --- | --- | --- |
| model turn | 1 iteration (retries do not consume iterations) | — |
| provider attempt | input = the input reservation (§7.4); output = `min(binding.max_output_tokens, remaining output)`; money = worst case (§7.3) | success: input → reported `input` if present (after §7.5 plausibility), else the reservation stays; output → reported `output` if present, else the full reservation stays; money → actual cost if computable, else the worst case stays. Failure: output released; input reservation and worst-case money stay |
| retry | 1 `provider_retries` | — (the next attempt reserves afresh) |
| tool proposal | 1 tool call, refused proposals included | — |
| composite-tool step | 1 tool call for each step after the first (`run.chargeStep`, passed as `tools.Env.StepBudget`) | — |
| artifact write | `len(data)` artifact bytes | refunded if the store's `Put` fails |

### 7.3 Money

Worst case for one attempt is `context_window × (highest known input-side
rate among input, cached-input, cache-write) + max_output × output rate`,
rounded up to whole micros (`engine.worstCaseCost`). It uses the context
window, not the local estimate, because an estimate is not a bound. A ceiling
is admitted only when every eligible binding is fully priced in its currency
(§2.1), so under a ceiling every rate is known and the reservation is a true
worst case; without a ceiling the money dimension only feeds the ledger.

Actual cost (`engine.actualCost`) needs a rate card and reported `input` and
`output`. Reported input is split into plain, cached and cache-written parts,
each at its own rate. A part reported as `0` needs no rate; an unreported part
is harmless only when its rate is known and equals the input rate; a positive
part needs its rate. Otherwise the call's cost is unknown and contributes
nothing, so `cost.micros` stays a lower bound (`engine` `TestMoneyCeiling` subtests).

### 7.4 Input bound and estimation

`max_input_tokens` is a hard bound. Each attempt reserves
(`engine.inputReservation`):

an upper bound, not an estimate, computed by the kernel alone: the UTF-8 byte
length of everything sent (every message's role, content, tool-call id,
replay bytes and tool calls; every tool spec) plus `messageOverheadTokens`
(16) per message and per tool spec for the provider's framing. A byte-level
BPE tokenizer never emits more tokens than input bytes. No host code supplies
a count the ledger trusts (there is no provider estimator port).

The reservation settles to the provider-reported `input` when reported and
plausible (§7.5); otherwise the upper bound stays charged.

Context fitting is a separate concept and stays estimate-based: context
compilation (§6) and the per-attempt "transcript no longer fits the context
window" check use `context.ApproximateTokens` (≈4 bytes/token,
`exact: false`). Consequently a tight `max_input_tokens` can admit context
that the first reservation then refuses (`exhausted/input_tokens`, before any
provider call); the bound is never exceeded. An estimated output count is
observation-only (`usage.estimated.output`) and never reaches the ledger.

### 7.5 Usage (`api.Usage`)

`reported` and `estimated` are never summed. Implausible wire data is treated
as unknown before settlement (`engine.plausibleUsage`): a negative count is
unknown, and a negative `input`, `cached_input` or `cache_write_input` makes
all three unknown (an adapter may already have folded a negative part into
`input`; adapters that sum parts into `input`, such as `anthropic`, report
`input` and the parts unknown themselves when a summand is negative or the sum
overflows); and when `input` is reported and `cached_input` or
`cache_write_input` exceeds it, or the two known parts together do, the
partition is impossible and `input`, `cached_input` and `cache_write_input`
are all unknown, so neither the input reservation nor the worst-case money
charge is refunded from those numbers. A reported count is non-null only
if every provider call reported it; a missing one is listed in `unknowns`
(`reported.input`, `reported.output`, `reported.cached_input`,
`reported.cache_write_input`). `estimated.input`/`output` are always present;
`estimated.cached_input` and `estimated.cache_write_input` are always unknown.
`cost.known` is false when there was no call or any call lacked pricing or
needed counts; `cost.micros` is then a lower bound and `cost` is in
`unknowns`. A failed call has unknown usage and therefore makes cost unknown.
`cost.currency`, `rate_source`, `rate_version` come from the binding's rate
card when present. Counters: `provider_calls`, `tool_calls`, `iterations`,
`retries`, `artifact_bytes`, `latency_millis` (sum of provider call latency).

## 8. Cancellation and termination

- The engine runs the loop under `context.WithDeadline(ctx, budget.deadline)`
  and distinguishes the caller's context from its own deadline.
- Before every side effect (`run.interrupted`): a recording failure wins, then
  caller cancellation, then the budget deadline.
- Caller cancellation settles `cancelled/host_cancelled` with
  `cancellation = api.CancellationOf(ctx)`: the provenance in an
  `*api.CancelCause` passed to a `context.CancelCauseFunc`
  (`api.Cancellation(p)`), `deadline` if the caller's own context deadline
  expired, otherwise `unknown`. A bare `cancel()` is `unknown`; the kernel
  never infers operator stop or controller shutdown.
- The budget deadline settles `exhausted/deadline`, never `cancelled`.
- A provider that returns a response despite cancellation keeps that response:
  it settles by its own stop (e.g. `completed` for `end`) if it proposes no
  tools; otherwise the next `interrupted` check cancels before any tool runs.

**Settlement.** `run.finish` is called exactly once per `Execute`. The
termination the loop returned is final; nothing observed later rewrites it.
The one exception is a recording failure: if any event, including
`execution.settled`, cannot be recorded, the result is
`incomplete/recording_failed` and `detail` carries the observed outcome
("observed before recording stopped: …"). A late cancellation cannot turn an
observed completion or failure into `cancelled` (`TestObservedOutcomeSurvivesLateCancel`).

## 9. Sessions, transcript and replay

- Provider sessions are ephemeral. Each `api.ProviderRequest` carries the full
  transcript the kernel compiled; no adapter may require provider-held state.
  The OpenAI adapter sends `store: false` and never `previous_response_id`; the
  Anthropic adapter sends the whole message list.
- `Message.Replay` / `ProviderResponse.Replay` are opaque adapter-owned bytes.
  The engine copies a response's `Replay` into the assistant message it
  appends and never interprets it. It counts toward the input estimate, lives
  only in the execution's in-memory transcript, and never appears in the
  result or events. An adapter ignores replay it did not write (Anthropic tags
  its own with `anthropic.messages/2023-06-01`).
- `SessionObservation{provider_id, cache_reused, ref}` is appended to
  `provenance.sessions` when an adapter reports provider cache reuse. It is an
  efficiency observation; `ref` is a provider response id for diagnosis and is
  never sent back.

## 10. Recording failure

An event sink refusing an event, or not taking or acknowledging it within
its bound (§4.1: "event sink did not take/answer call … within …"), marks the run failed-to-record: no later
event is recorded, every subsequent side-effect check stops, and the result is
`incomplete/recording_failed` with whatever observations were recorded before.
An artifact store failure inside a tool after a mutation is an unknown outcome
(`tool_failed`, see the capabilities spec), not a recording failure.

## 11. Compatibility and versioning

- One version is accepted and emitted: `agentkernel.execution/v0.1`.
- Because `DecodeRequest` refuses unknown fields, any new request field is a
  contract change for existing decoders. Changes follow `AGENTS.md`: the
  acceptance case requiring it, schema and example updates in the same commit,
  and a version bump when not backward compatible.
- Schema/Go agreement is tested (`api/schema_coverage_test.go`,
  `api/schema_test.go`).
- The kernel is not crash-resumable: a restarted host starts a new attempt
  (new `attempt_id`); no uncertain mutation is replayed as though it did not
  happen.

### 11.1 Admission: one envelope per execution

`Budget` is the envelope of the whole execution, every attempt of one
`execution_id` together. The admission store is kernel-owned
(`Config.Admissions *storage.FileRecords`; nil: an in-memory
`storage.MemoryRecords` owned by the Engine), so no host code runs inside
admission. It holds two partitions per `execution_id`:

- `agentkernel.admission_claims`: the in-flight **claim**, whose value is the
  claiming `attempt_id`;
- `agentkernel.admissions`: the **record**: the first settled attempt's
  budget (the envelope) and each settled attempt's `attempt_id` and
  cumulative consumed totals: the execution's whole charge through that
  attempt, earlier attempts included (its ledger started from them).

After validation and before any side effect, `run.admit`:

1. claims the `execution_id` with `Records.PutIfAbsent`. If the claim is held
   it refuses `blocked/invalid_request`: "attempt … already admitted" when the
   holder is this `attempt_id`, else "prior attempt … in flight or unsettled;
   consumption unknown". A failed claim write refuses
   `incomplete/recording_failed`, "nothing ran". A refused contender reserves
   nothing, calls no provider and writes no record.
2. holding the claim, reads the record and refuses (releasing the claim) an
   `attempt_id` already settled ("already admitted") or a budget that widens
   any bound of the envelope: a later deadline, any larger numeric bound, or
   any money change other than a lower ceiling in the same currency
   ("budget widens …").
3. starts the ledger from what the execution has consumed: per dimension, the
   largest cumulative total any settled attempt recorded (money and retries
   included; the deadline is absolute). Records are never added together,
   since each already includes the ones before it.

At settlement the claim holder appends its ledger's cumulative charge to the
record and then
deletes the claim; the read-modify-write happens only while holding the
claim, so it needs no other lock. If either write fails the result is
`incomplete/recording_failed` with the observed outcome in `detail`, and the
claim stays, so later attempts are refused. A claim left by a crashed attempt
is never expired by time: its consumption is unknown, so the kernel fails
closed until the host recovers it through an explicit recovery protocol
(Gate B / host work, not implemented here). A request refused by validation
is never admitted.

Atomicity: `PutIfAbsent` is part of the `storage.Records` contract and is
never a check followed by a write. `MemoryRecords` does it under its mutex.
`FileRecords` writes the framed value to a synced temp file and `link(2)`s it
to the record name, which creates the name only if it is absent, atomically,
with its full content; the directory is then fsynced. Of any number of
contenders — goroutines, Engines, or processes opening the same root — exactly
one is admitted. This holds for processes sharing one **local** filesystem
on Linux and macOS (Windows is refused by the file stores). On network
filesystems `link` atomicity depends on the server; an NFS client retrying a
lost reply can report "exists" for its own successful link, which refuses
the attempt (fail closed), never admits two.

`usage` reports the attempt's own consumption; the ledger enforces the
execution's. Authority that spans executions (a cumulative spend across
`execution_id`s, a per-task or per-operator envelope) remains the host's: the
kernel enforces exactly the per-execution envelope above.

## 12. Divergences from issue #446 (stated, not hidden)

1. Retry exhaustion has no dedicated dimension in the termination (§5).

Resolved: negative reported counts are dropped to unknown before settlement,
so they cannot credit the ledger (`engine.plausibleUsage`, F1); a possibly
mutating tool that ran before a stop is observed (F2); `other` and
`tool_use`-without-calls stops settle `failed`, not `completed` (F3); the
never-emitted `budget.exhausted` kind was removed. After review 5443104514:
`max_input_tokens` is a hard bound (§7.4, F6); re-entry and later attempts
cannot renew the execution's budget, with admission atomic across processes
on one local filesystem (§11.1, F7); impossible cache partitions fail closed
(§7.5) and the provider estimator port was removed (§7.4); every host port is
a bounded hand-off that leaves no kernel goroutine behind (§4.1). After the
second adversarial review of PR #510: ledger arithmetic saturates instead of
wrapping and prices are range-checked (§7.1); kernel-owned ports are checked
by exact type, not by embeddable marker methods (§4.1); cumulative attempt
records are no longer summed (§11.1); hand-off IDs are kernel sequences and a
repeated model tool-call ID is refused (§4.1); a negative usage part makes
input unknown (§7.5); an expired bound hands nothing over, a closed reply is
no answer, and hand-off channels must be unbuffered (§4.1).
