# Providers v0.1

Normative for Gate A. Code: `api/provider.go`, `providers/*`. Provider
adapters own wire behaviour and usage normalization only; retries, routing and
budgets belong to the engine, and eligibility, credentials, endpoints and
prices belong to the host.

## 1. Adapter contract (`api.Provider`)

```go
type Provider interface {
    Complete(ctx context.Context, request ProviderRequest) (ProviderResponse, error)
}
```

An adapter MUST:

1. perform exactly one bounded call per `Complete`, with no retry, routing,
   budget or waiting decision of its own;
2. send the full transcript in `ProviderRequest.Messages` and depend on no
   provider-held session state for correctness;
3. honour `ProviderRequest.MaxOutputTokens` and use `Binding.Model` — no model
   name or price is assumed by any adapter;
4. return only `*api.ProviderError` on failure, with a typed `Class`,
   `Retryable` set only for transient classes, and a `Detail` from which the
   credential value has been removed;
5. map `ctx` cancellation to class `cancelled` and deadline expiry to
   `deadline`, returning promptly;
6. report usage counts it does not receive as `nil` (unknown), never `0`;
7. reject a 2xx body that does not match its contract as `malformed`, and any
   tool call without an id, a name and a JSON-object argument as `malformed`;
8. resolve the credential through the host's `api.CredentialSource` on every
   call, from `Binding.CredentialHandle`.

An adapter MAY implement `api.TokenEstimator`; none in Gate A does, so the
engine uses `context.ApproximateTokens`. An untyped error from an adapter is
treated by the engine as a non-retryable `transport` failure.

`StopReason`: `end`, `tool_use`, `max_tokens`, `refused`, `other`. See the
execution spec §5 and §12 for how the engine settles each, including the
`other` → `completed` divergence.

## 2. Shared wire mechanics (`providers/internal/wire`)

- `wire.Doer` (`Do(*http.Request)`) is the network boundary; `*http.Client`
  satisfies it and tests inject fakes. Endpoints are adapter `Config` fields
  supplied by the host, never read from repository content.
- `wire.Post`: a finished context before sending returns
  `cancelled`/`deadline`; a transport error is `transport` (retryable) unless
  the context ended; the body is read up to `MaxResponseBytes + 1` and a larger
  body is `oversized` (not retryable).
- `wire.Classify` (non-2xx): 401/403 → `auth`; 429 → `rate_limited`
  (retryable); 503 and 529 → `unavailable` (retryable); other ≥500 → `server`
  (retryable); everything else → `rejected`. `Detail` is
  `error.type: error.message` when present, else a raw excerpt, bounded to 512
  bytes, redacted.
- `wire.Credential`: an empty handle or nil source is `auth`; a source error is
  `auth` without the source's own text (it may echo what it resolved); an
  empty secret is `auth`.
- `wire.Redact` replaces the secret with `[redacted]` before any detail is
  stored.

The engine retries `transport`, `rate_limited`, `unavailable` and `server`
only while `max_provider_retries` allows; then `rate_limited`/`unavailable`
settle `blocked/provider_unavailable` and the rest `failed/provider_failed`.

## 3. Adapters

### 3.1 `providers/openai` — OpenAI Responses API

| Item | Value (from the package comment) |
| --- | --- |
| Endpoint | `POST /v1/responses`, full URL in `Config.Endpoint` |
| Primary docs, retrieved 2026-10-07 | https://developers.openai.com/api/reference/resources/responses/methods/create · …/responses/methods/retrieve · https://developers.openai.com/api/docs/guides/function-calling · …/guides/error-codes · …/guides/prompt-caching (platform.openai.com/docs returned HTTP 403 to the fetcher that day) |
| API version | none: the Responses API carries no version header |
| Auth | `Authorization: Bearer <secret>` |

Request: `model` = binding model; system messages joined into `instructions`;
user/assistant messages as `message` items; assistant tool calls as
`function_call` items; tool results as `function_call_output` (the Responses
API has no error flag, so the engine's JSON tool message carries the status);
tools as `{"type":"function", …, "strict": true}`; `max_output_tokens`;
`"store": false`; `previous_response_id` is never sent.

Response: `status: failed` → `server` (status 200, not retryable); any status
other than `completed`/`incomplete` → `malformed`. `output_text` parts form the
text; a `refusal` part sets stop `refused`; `function_call` items become tool
calls; reasoning and other item types are skipped (not needed for
correctness, so no `Replay` is produced). Stop: refusal → `refused`;
`incomplete` + `max_output_tokens` → `max_tokens`; `incomplete` +
`content_filter` → `refused`; other `incomplete` → `other`; tool calls →
`tool_use`; else `end`. `model` → `ModelVersionObserved`.

Usage: `input` = `usage.input_tokens` (all input); `output` =
`usage.output_tokens`; `cached_input` = `input_tokens_details.cached_tokens`;
`cache_write_input` = `input_tokens_details.cache_write_tokens`, unknown when
absent. `cached_input > 0` adds a `SessionObservation` with the response id.

### 3.2 `providers/anthropic` — Anthropic Messages API

| Item | Value (from the package comment) |
| --- | --- |
| Endpoint | `POST /v1/messages`, full URL in `Config.Endpoint` |
| Primary docs, retrieved 2026-10-07 (docs.anthropic.com redirects to platform.claude.com) | https://platform.claude.com/docs/en/api/messages · …/api/errors · …/build-with-claude/handling-stop-reasons · …/build-with-claude/thinking · …/build-with-claude/prompt-caching |
| API version | `anthropic-version: 2023-06-01` (`anthropic.APIVersion`) |
| Auth | `x-api-key: <secret>` |

Request: system messages joined into `system`; consecutive tool messages
become one user turn of `tool_result` blocks with `is_error`; assistant turns
are text and `tool_use` blocks, or, when the message carries this adapter's
`Replay` (tag `anthropic.messages/2023-06-01`), the exact original content
array; tools carry `"strict": true`; `max_tokens` = `MaxOutputTokens`.

Response: a non-array `content` or missing `stop_reason` is `malformed`.
`text` blocks form the text; `tool_use` blocks become tool calls. When a turn
has `thinking`/`redacted_thinking` blocks **and** tool calls, the exact content
array is returned as `Replay`, because the Messages API requires those blocks
back unchanged within a tool-use turn; the engine carries it only in the
execution transcript. Stop: `end_turn` → `end`; `tool_use` → `tool_use`;
`max_tokens` and `model_context_window_exceeded` → `max_tokens`; `refusal` →
`refused`; anything else (`stop_sequence`, `pause_turn`, future values) →
`other`.

Usage: the API's `input_tokens`, `cache_read_input_tokens` and
`cache_creation_input_tokens` are additive, so `input` is their sum (absent
cache fields add nothing); `cached_input` = cache reads; `cache_write_input` =
cache creation, but **unknown** when `cache_creation.ephemeral_1h_input_tokens
> 0`, because 1-hour writes bill at a rate one rate card cannot express.
`cached_input > 0` adds a `SessionObservation`.

### 3.3 `providers/local` — local-provider seam (`agentkernel.local/v0.1`)

`POST <Endpoint>` with `api.ProviderRequest` as JSON; `Authorization: Bearer`
only when the binding names a credential handle (`Config.Credentials` may then
be nil otherwise). A 200 body is an `api.ProviderResponse`, decoded strictly
(unknown fields and trailing data refused, unknown stop refused, tool calls
validated); `null` counts are unknown. No real local backend ships; the seam is
exercised through the conformance suite and `examples/hostport`.

### 3.4 `providers/scripted`

Deterministic in-process provider for offline tests and failure injection:
`scripted.New(steps...)` replays `Step{Response | Err, Block, Entered,
Before}` in order (safe for concurrent callers, each consumes one step);
`Block` waits for the context and returns `cancelled`/`deadline`; a call past
the script fails as `rejected`. `Requests()` returns what was sent.

## 4. Conformance suite (`providers/conformance`)

`conformance.Run(t, Target)` runs offline against a fake `Transport` (records
requests, replays scripted replies, never touches the network). Targets run:
`openai`, `anthropic`, `local` (wire) and `scripted` (in-process; wire-only
cases skipped).

| Case | Proves |
| --- | --- |
| `ToolRoundTrip` | tool call id/name/arguments round-trip; usage incl. cache counts normalized; observed model; second request replays the full transcript (call and result ids), host system text, strict tools, output bound |
| `UsageUnknown` | absent usage → all nil; partial usage keeps missing counts nil |
| `OutputLimit` | `max_tokens` stop and the sent output bound |
| `Refusal` | refusal → stop `refused` |
| `Cancellation` | in-flight cancel returns `cancelled` promptly |
| `Deadline` | expired deadline returns `deadline` |
| `CredentialAtCallTime` | credential resolved once per call; sent in a header, never the body |
| `CredentialFailureRedacted` | a failing source (whose error echoes the key) yields `auth` without the key, and nothing is sent |
| `Malformed` | partial JSON, HTML, empty body, wrong JSON type → `malformed` |
| `Oversized` | body over `MaxResponseBytes` → `oversized` |
| `TransportFailure` | transport error (whose text contains the key) → retryable `transport`, key redacted |
| `StatusClassification` | 400/413 → `rejected`, 401/403 → `auth`, 429 → retryable `rate_limited`, 500 → retryable `server`, 503/529 → retryable `unavailable`; status kept, key redacted |

Every successful response and every error is also checked to not contain the
key. Adapter-specific tests add: OpenAI `TestNoServerSideState`,
`TestFailedStatusIsServerError`, `TestInvalidFunctionArgumentsAreMalformed`;
Anthropic `TestHeaders`, `TestThinkingReplayedVerbatim`,
`TestForeignReplayIgnored`, `TestUsageNormalization`; local
`TestUnknownFieldIsMalformed`.

## 5. Credentials and billing

- No `api` type holds a credential value. `ProviderBinding.CredentialHandle`
  names one; `api.CredentialSource` (host-injected) resolves it at call time.
  `api.Secret` prints and serializes as `[redacted]`.
- Secrets never enter canonical requests, results, events, fingerprints,
  transcripts, memory or artifacts; adapter error details are redacted.
- The OpenAI and Anthropic adapters are **API-billed** calls against whatever
  account the host's credential belongs to. The kernel never selects an
  account, never falls back from a subscription CLI to an API, and never
  switches provider silently: routing preserves a pinned binding and blocks
  rather than substituting.
- Prices come only from the host's `ProviderBinding.Pricing`; absent pricing is
  unknown cost, never zero.
- Quota exhaustion and rate limiting are not distinguished: both arrive as
  HTTP 429 and classify `rate_limited` (retryable), so a quota error consumes
  retry allowance before settling `blocked/provider_unavailable`.

## 6. Live verification status

**None.** No adapter has been called against a live API in Gate A, and no test
or CI job makes a network call or spends credit. The fake wire bodies in the
adapter tests are encoded by test code written from the primary documentation
listed above (retrieved 2026-10-07); they are not recordings of live traffic.
Field names such as OpenAI `input_tokens_details.cache_write_tokens` and
Anthropic `strict` on tools are therefore documented-but-unexercised. Live
calls require explicit operator configuration and a spending bound and are a
Gate B activity.

`Provenance.ModelVersionObserved` records what a provider reported; Gate A has
no observed value from any real provider.
