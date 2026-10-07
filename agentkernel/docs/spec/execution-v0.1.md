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
must be non-nil. `(*Engine).Execute` always returns a settled result and a nil
error, including for refused requests.

## 2. Request (`api.ExecutionRequest`)

| Field | Meaning |
| --- | --- |
| `version` | MUST equal `api.ExecutionVersion` (`agentkernel.execution/v0.1`). |
| `execution_id`, `attempt_id` | Opaque host identities, echoed in every event and the result. Durable attempts are host-owned. |
| `objective` | Host text, non-blank, at most `api.MaxObjectiveBytes` (64 KiB). Rendered as the user turn's framing; not system text. |
| `mode` | `read_only` or `read_write` (see capabilities spec). |
| `workspace` | `WorkspaceRef{id, manifest_digest, git_revision?, dirty}`. The manifest digest identifies content; a Git revision alone never does. |
| `constraints` | `required_features`, `require_host_proven_isolation`, `instruction_digest` (recorded in provenance, never interpreted). |
| `context` | `[]ContextItem`; `required: true` items are never dropped (§6). |
| `grants` | `[]Capability`; the only source of tool authority. |
| `budget` | Finite envelope (§7). |
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

When several identity fields are invalid, which one is reported is unspecified
(the three are checked by map iteration).

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
| `tool.executed` / `tool.refused` | after dispatch (`refused` for broker refusals) |
| `artifact.recorded` | each artifact a tool stores |
| `execution.settled` | exactly once, last, unless recording already failed |

`budget.exhausted` is declared in `api` and the schema but **the engine never
emits it**; exhaustion appears in `execution.settled` and the termination.

`EventSink.Record` receives a context detached from cancellation
(`context.WithoutCancel`), so a cancelled execution still reports how it ended.

## 5. Outcomes and causes

Derived from `engine/engine.go`, `engine/compile.go`, `engine/loop.go`,
`engine/tools.go` and `engine/run.go`.

| Outcome | Cause | Dimension | Raised when |
| --- | --- | --- | --- |
| `completed` | `loop_completed` | — | a provider response has no tool calls and its stop is not `refused` or `max_tokens` (so `end`, `tool_use` without calls, and `other` all complete) |
| `exhausted` | `budget_exhausted` | `iterations` | no model turn can be reserved |
| `exhausted` | `budget_exhausted` | `output_tokens` | stop `max_tokens`; no output allowance remains; or the reservation fails on output |
| `exhausted` | `budget_exhausted` | `input_tokens` | the transcript estimate exceeds `context_window − max_output`; or the reservation fails on input |
| `exhausted` | `budget_exhausted` | `money` | the worst-case reservation would exceed the ceiling |
| `exhausted` | `budget_exhausted` | `tool_calls` | a proposal cannot be reserved |
| `exhausted` | `budget_exhausted` | `artifact_bytes` | a tool's artifact write was refused by the ledger |
| `exhausted` | `budget_exhausted` | `deadline` | the budget deadline passed (clock or run context) |
| `failed` | `provider_refused` | — | stop `refused` (checked before tool calls) |
| `failed` | `provider_failed` | — | a non-retryable provider error, or a retryable one with no retry allowance, other than the two classes below; untyped adapter errors become non-retryable `transport` |
| `failed` | `tool_failed` | — | `Broker.Dispatch` returned an error: a side effect's outcome is unknown |
| `blocked` | `provider_unavailable` | — | class `rate_limited` or `unavailable` and no retry is permitted |
| `blocked` | `invalid_request` | — | validation failed; or `context.Compile` failed for a reason other than capacity |
| `blocked` | `insufficient_capacity` | — | prompt overhead leaves no input capacity, or required context does not fit |
| `blocked` | `no_eligible_provider` | — | routing blocked, or the chosen binding has no configured adapter (no fallback) |
| `cancelled` | `host_cancelled` | — | the caller's context ended; `cancellation` is set (§8) |
| `incomplete` | `recording_failed` | — | an event could not be recorded |

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
provider call; they are never dropped, truncated or summarized.

## 7. Budgets

### 7.1 Ledger

`engine.ledger` holds one limit per dimension and reserves atomically across
all dimensions of one operation: every amount or none. Nothing adds to a
limit; retries and re-entry spend from the same envelope. Settlement replaces a
reservation with an actual charge, which may exceed it; the overrun is kept so
the next reservation fails. Money is always tracked; it refuses only when the
host set a ceiling.

### 7.2 Per operation

| Operation | Reserved before | Settled after |
| --- | --- | --- |
| model turn | 1 iteration (retries do not consume iterations) | — |
| provider attempt | input = local estimate of the full prompt; output = `min(binding.max_output_tokens, remaining output)`; money = worst case (§7.3) | success: input → reported `input` if present, else the estimate stays; output → reported `output` if present, else the full reservation stays; money → actual cost if computable, else the worst case stays. Failure: output released; input estimate and worst-case money stay |
| retry | 1 `provider_retries` | — (the next attempt reserves afresh) |
| tool proposal | 1 tool call, refused proposals included | — |
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

### 7.4 Estimation and usage lag

Input estimates come from the provider's `api.TokenEstimator` if it implements
one, otherwise `context.ApproximateTokens` (≈4 bytes/token, `exact: false`).
Provider-reported counts arrive only with the response. Consequence, stated
plainly: when a provider reports no input count, `max_input_tokens` is enforced
against the local estimate, which is not a guaranteed bound. Output and money
remain hard: unreported output keeps its full reservation and unpriced money
keeps the worst case.

### 7.5 Usage (`api.Usage`)

`reported` and `estimated` are never summed. A reported count is non-null only
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
  if it proposes no tools the execution completes; otherwise the next
  `interrupted` check cancels before any tool runs.

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

`EventSink.Record` returning an error marks the run failed-to-record: no later
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
- The kernel is not crash-resumable: a restarted host starts a new execution
  (new `attempt_id`); no uncertain mutation is replayed as though it did not
  happen.

## 12. Divergences from issue #446 (stated, not hidden)

1. `budget.exhausted` exists as an event kind but is never emitted (§4).
2. Retry exhaustion has no dedicated dimension in the termination (§5).
3. A provider stop of `other` (OpenAI `incomplete` for a reason other than
   `max_output_tokens`/`content_filter`; Anthropic `stop_sequence`,
   `pause_turn` and unknown values) without tool calls settles
   `completed/loop_completed`. An `incomplete` or `pause_turn` response is
   therefore reported as a completed loop; hosts must not read `completed` as
   more than "the provider stopped proposing tools".
4. `max_input_tokens` is soft when the provider reports no input count (§7.4).
