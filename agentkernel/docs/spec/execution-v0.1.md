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
must be non-nil. Two fields have stated defaults: `Admissions` (nil: an
in-memory `storage.Records` owned by the Engine, §11) and `SettleTimeout`
(zero: `engine.DefaultSettleTimeout`, 5 s; negative is refused; §4.1).
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

### 4.1 Recording bound

Every `EventSink.Record` call, every `ContextSource.ContextItems` call and
every admission-store (`Config.Admissions`) read and write goes through one
helper (`run.bounded`). The callee's context ignores the
caller's cancellation, so a cancelled execution still reports how it ended,
but it carries a deadline, and the kernel waits at most until it:

| Call | Bound |
| --- | --- |
| in-loop event, context source, admission read/write before any side effect (§11.1) | `budget.deadline`; if the deadline has passed or the host has cancelled, the settlement grace from now; a host cancellation arriving while the call runs shortens the wait to the grace from that moment |
| `execution.refused`, `execution.settled` (and the admission settlement write, §11) | one settlement grace (`Config.SettleTimeout`, default 5 s), fixed when settlement begins |

A sink call that does not return within its bound is a recording failure
("event sink did not return within …", §10). A source that does not return is
noted as unavailable, like any failing source. An admission read or write
that does not return refuses the attempt ("admission not recorded, nothing
ran"); a settlement write that does not return settles
`incomplete/recording_failed` and leaves the attempt unsettled. The kernel stops waiting and
cancels the callee's context; a callee that ignores its context keeps its own
goroutine until it returns (a deliberate, documented leak) but never holds
`Execute` open. `Execute` therefore returns within `budget.deadline` (or the
host's cancellation) plus one grace, **provided** providers, tools and the
`CommandRunner` honour their context: those are never abandoned, because abandoning a mutating call would hide its effect.

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
| `failed` | `provider_failed` | — | the provider's `TokenEstimator` returned a negative count, at compilation or before an attempt (detail names the estimator; no provider call follows); the ledger refused a negative reservation; a non-retryable provider error, or a retryable one with no retry allowance, other than the two classes below (untyped adapter errors become non-retryable `transport`); or a response without tool calls whose stop is `other` or `tool_use` ("provider stopped without completing") |
| `failed` | `tool_failed` | — | `Broker.Dispatch` returned an error: a side effect's outcome is unknown |
| `blocked` | `provider_unavailable` | — | class `rate_limited` or `unavailable` and no retry is permitted |
| `blocked` | `invalid_request` | — | validation failed; admission refused the attempt (already admitted, a widened budget, or a prior attempt unsettled, §11); or `context.Compile` failed for a reason other than capacity or a negative estimate |
| `blocked` | `insufficient_capacity` | — | prompt overhead leaves no input capacity, or required context does not fit |
| `blocked` | `no_eligible_provider` | — | routing blocked, or the chosen binding has no configured adapter (no fallback) |
| `cancelled` | `host_cancelled` | — | the caller's context ended; `cancellation` is set (§8) |
| `incomplete` | `recording_failed` | — | an event could not be recorded, or did not return within its bound (§4.1); the attempt's admission could not be read or written (detail "admission not recorded, nothing ran", no `execution.settled`); or its consumption could not be recorded at settlement (§11) |

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
never lowers what was used. Settlement replaces a
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

- the provider's own count, when its `api.TokenEstimator` reports `exact`;
- otherwise an upper bound, not an estimate: the UTF-8 byte length of
  everything sent (every message's role, content, tool-call id, replay bytes
  and tool calls; every tool spec) plus `messageOverheadTokens` (16) per
  message and per tool spec for the provider's framing. A byte-level BPE
  tokenizer never emits more tokens than input bytes. A provider whose framing
  exceeds the allowance must implement an exact estimator.

The reservation settles to the provider-reported `input` when reported and
plausible (§7.5); otherwise the upper bound stays charged.

Context fitting is a separate concept and stays estimate-based: context
compilation (§6) and the per-attempt "transcript no longer fits the context
window" check use the estimate from the provider's `api.TokenEstimator`, or
`context.ApproximateTokens` (≈4 bytes/token, `exact: false`). Consequently,
with an inexact estimator a tight `max_input_tokens` can admit context that
the first reservation then refuses (`exhausted/input_tokens`, before any
provider call); the bound is never exceeded.

An estimator is untrusted: a negative count fails the execution closed
(`failed/provider_failed`) before any provider call, whether it appears in
the prompt-overhead estimate, in `context.Compile` (which returns
`context.ErrNegativeEstimate`), or before an attempt. An estimated output
count is observation-only (`usage.estimated.output`); a negative one
contributes nothing and never reaches the ledger.

### 7.5 Usage (`api.Usage`)

`reported` and `estimated` are never summed. Implausible wire data is treated
as unknown before settlement (`engine.plausibleUsage`): a negative count is
unknown; and when `input` is reported and `cached_input` or
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

`EventSink.Record` returning an error, or not returning within its bound
(§4.1), marks the run failed-to-record: no later
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
`execution_id` together. The engine keeps one admission record per
`execution_id` in `Config.Admissions` (`storage.Records`, partition
`agentkernel.admissions`). After validation and before any side effect,
`run.admit` refuses, in this order:

1. an `(execution_id, attempt_id)` already admitted (re-entry), in any state:
   `blocked/invalid_request` "attempt … already admitted";
2. a budget that widens any bound of the first admitted attempt's budget: a
   later deadline, any larger numeric bound, or any money change other than a
   lower ceiling in the same currency (adding, removing or re-denominating a
   ceiling included): `blocked/invalid_request` "budget widens …";
3. any prior attempt of the execution still in flight, running concurrently or
   crashed without settling: `blocked/invalid_request` "prior attempt …
   unsettled; consumption unknown". The kernel fails closed; only the host can
   resolve an unsettled attempt, by starting a new execution.

Otherwise the attempt is recorded in flight before anything runs (a failed
or timed-out read or write refuses: `incomplete/recording_failed`, "nothing
ran"; an abandoned write that lands later leaves the attempt admitted and
unsettled, which fails closed), and its ledger
starts from the summed consumption of every settled prior attempt, in every
dimension (money and retries included; the deadline is absolute already). At
settlement the attempt's consumption is recorded within the settlement grace;
if that write fails or does not return within the grace, the result is `incomplete/recording_failed` with the
observed outcome in `detail`, and the attempt stays unsettled, so later
attempts are refused. A request refused by validation is never admitted.

Scope of enforcement: admission is serialized by the Engine, so concurrent
`Execute` calls for one `execution_id` admit exactly one. With the default
in-memory store the envelope holds for that Engine instance; across restarts
it needs a durable store (`storage.OpenFileRecords`). Several Engines or
processes sharing one store can race between read and write (Gate A has no
atomic compare-and-put); such hosts must serialize admission themselves.

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
cannot renew the execution's budget (§11.1, F7); negative estimates and
impossible cache partitions fail closed (§7.4, §7.5); recording is bounded
(§4.1).
