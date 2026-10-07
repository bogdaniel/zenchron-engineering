# Gate B integration plan (not implemented)

Gate A delivered an isolated module. **Nothing in this plan is done, and Gate A
grants no permission to edit any file it names.** Gate B needs its own issue,
with its own file allowlist, opened after the relevant #445 boundaries
stabilize (#446 §18). Until then the parent issue reports "isolated kernel
complete; production adoption pending".

## 1. Current root seams (read-only inventory)

Snapshot of base `27b7c30`. These are **today's** paths in a monolithic
`runtime` package that #445 is decomposing; Gate B must re-inventory after #445
lands and must not treat them as permanent architecture.

| Seam | File (at `27b7c30`) | Shape relevant to the kernel |
| --- | --- | --- |
| Provider port | `runtime/adapters.go` | `ExecutionProvider{Execute(ctx, ExecutionRequest) (ExecutionResult, error)}` |
| Request | `runtime/adapters.go` | `ExecutionRequest`: `RunID`, `OperationID`, `Attempt`, `PriorAttemptFailure`, `SourceSnapshot`, `ControllerID`, `Base`, `Candidate`, `CandidateDir`, `Contract`, `Objective`, `AcceptanceObligations`, `Constraints`, `Prohibitions`, `Permissions`, `TrustedInstructions`, `Instructions`, `Purpose`, `Mode domain.InvocationMode`, `ModelPreference`, `DenyPermissionBypass`, `Findings`, `Feedback`, `Upstream`, `ReviewerResultPath`, `FeedbackResolutionPath`, `HandoffPath`, `RequiredTools`, `ScratchDir`, `Deadline *time.Time`, `Budgets ProviderBudget` |
| Invocation budget | `runtime/adapters.go` | `ProviderBudget{MaxTokens, MaxCostMicros *int64; WallLimit, InactivityLimit, InactivityWindow time.Duration}` |
| Result | `runtime/adapters.go` | `ExecutionResult`: `ProviderID`, `Model`, `AuthMode`, `Attempt`, `Outcome OperationState`, `Tokens`, `CostMicros *int64`, `Artifacts`, `ChangeSummary`, `ChangedPaths`, `Failure *ProviderFailure`, `PriorContext`, `Invocation *InvocationProvenance`, `Executed`, `Review`, `ReviewRefusal`, `Resolution`, `Answer` |
| Failure taxonomy | `runtime/adapters.go` | `FailureClass` (e.g. `transient_execution_provider`, `provider_rate_limited`, `provider_quota`, `provider_unavailable`, `provider_no_progress`, `controller_shutdown`, `run_cancelled`, `execution_incomplete`, `execution_deadline_exceeded`, `connectivity_unavailable`, `candidate_credential_material`, `unknown`); `RouteFailure` |
| Operation state | `runtime/runtime.go` | `OperationState`: `pending`, `leased`, `running`, `succeeded`, `failed`, `cancelled`, `unknown` |
| Duplicate model/tool loop | `runtime/openai_provider.go` | `OpenAIProvider{ArtifactStore, Model, AuthMode, APIKeyFile, Endpoint, HTTP Doer, Broker ToolBroker, MaxIterations, MaxToolCalls, MaxResultBytes, NoProgressLimit, Timeout}`; zero bounds default to 16 iterations, 64 tool calls, 2 no-progress turns, 10 min; key read from an operator file per `Execute`; redacted transcript artifact; `Isolation()` claims all four properties `proven` |
| Duplicate tool boundary | `runtime/tool_broker.go` | `ToolBroker{CandidateDir, Sandbox DockerSandbox, MaxBytes, DependencyCacheDir}`: `ReadFile`, `Search`, `Diff`, `ApplyPatch` (unified diff via `git apply`, paths pre-enumerated with `--numstat`), `RunCommand` (network-denied `DockerSandbox`); resolve gate refuses traversal, `.git`, credential-shaped names/contents (`GuardCandidate`), symlinked ancestors |
| Native CLI catalogue | `runtime/agent_specs.go` | one `cliAgentSpec` per coding CLI; child environment is an allowlist with no API-key variables (subscription billing is never displaced) |
| Native CLI lifecycle | `runtime/cli_agent.go` | `CLIAgentProvider` (operator_trusted), `InvocationProvenance`, `ExecutionScratchDir` |
| Termination owners | `runtime/termination_owner.go`, `runtime/process_unix.go` | `TerminationOwner`: `provider_exited`, `deadline`, `inactivity`, `operator_stop`, `controller_shutdown` (plus `""` undecided and `not_started`), committed once by `runBoundedProcess` |
| Isolation statement | `runtime/provider_isolation.go` | `ProviderIsolation`, `IsolationProven` |
| Sandbox, artifacts | `runtime/sandbox.go` | `DockerSandbox`; `ArtifactStore struct{Root string}` |
| Scheduler | `runtime/scheduler.go` | `Scheduler{Store, Clock, Owner, Liveness, LeaseDuration, MaxConcurrentRuns, MaxConcurrentObservations, MaxConcurrentVerifications}` |
| Run budgets | `runtime/controller.go`, `runtime/agent_binding.go`, `domain/plan.go` | `RunBudgets{WallLimit, LifecycleDeadline, MaxExecutionAttempts, MaxExecutionContinuations, MaxProviderInvocations, MaxRemediationAttempts, MaxAssuranceAttempts, ProviderInactivityLimit, …}`; `BudgetDimension{Known, Remaining}`, `RemainingBudgets`; `PlanBudgetEnvelope`, `StageBudget` |
| Composition root | `cmd/zenchron-engineering/autonomy.go` | `executionProvider(...)` builds `runtime.OpenAIProvider` (with a `ToolBroker` over `DockerSandbox`) wrapped in `candidateBoundProvider` |

## 2. Ownership mapping

| Owner | What Gate B connects | What it must not do |
| --- | --- | --- |
| #445 | selects the one execution seam after decomposition; owns moving/retiring existing runtime/provider files | Gate B must not refactor the runtime itself |
| #67 | consumes `intelligence.Snapshot` (facts with `Evidence`/`Confidence`, `Incomplete`, `CallResult.Limits`) through a translation seam into ProjectModel v2 | no second ProjectModel; no fact promoted to policy-relevant without #67's pipeline; inferred stays inferred |
| #70 | consumes `api.Usage`, `api.RoutingDecision`, `routing.Report` as observations | no learned routing in the kernel; no outcome labels from kernel results |
| #103 | unchanged: external executable discovery, trust and wire protocol | kernel in-process providers are not a replacement protocol; a #103 adapter could later back an `api.Provider` only through #103's own boundary |
| #84/#85/#86/#87, ADR-0003 | frozen provider binding, account recovery, capacity, pause, config lifecycle | the kernel never waits for capacity; it returns typed observations |

## 3. Host adapter design (outside `agentkernel/`)

One host-owned adapter, in whatever package #445 designates for execution
providers, is the **only** code importing both vocabularies. It implements the
host `ExecutionProvider` and composes `engine.Engine` with host ports:

| Kernel port | Host implementation |
| --- | --- |
| `api.EventSink` (served by a host worker, `api.ServeEvents`) | appends to the existing journal/attempt record; an append failure returns an error so the kernel settles `recording_failed` |
| `api.Clock` | `api.SystemClock` (`engine.New` accepts only kernel clocks, by exact type) |
| `Config.Artifacts` | a kernel `storage.FileArtifacts` root; a bridge to the existing artifact store is host code and therefore must sit behind a hand-off (not available in Gate A) |
| `api.CommandRunner` (served, `api.ServeCommands`) | the existing `DockerSandbox` path used by `ToolBroker.RunCommand` (network none, read-only, cap-drop) |
| `api.CredentialSource` | reads the operator `APIKeyFile` per call, refusing a key inside the candidate workspace (today's rule); called by the adapter on the host's provider worker |
| tools | `tools.Workspace` on `CandidateDir`, **plus** host guards the kernel lacks (§5); or host tools (`tools.NewHostTool`, served by `tools.ServeTool`) over the existing resolve gate |
| `api.Provider` (served, `api.ServeProvider`) | `providers/openai` / `providers/anthropic` with the host's endpoint and `http.Client` |

Request translation:

| Host input | Kernel field |
| --- | --- |
| `RunID`, `OperationID`, `Attempt` | `ExecutionID`, `AttemptID` (valid identifiers ≤128 chars, derived by the host; never invented by the kernel) |
| `TrustedInstructions`, `Instructions` | `ContextItem{kind: instruction, trust: host, required: true}` |
| `Constraints`, `Prohibitions`, `AcceptanceObligations` | `ContextItem{kind: constraint, trust: host, required: true}` |
| `Findings`, `Feedback`, `Upstream`, eligible prior-attempt observations | `ContextItem{kind: observation}` with non-host trust; required when the contract requires them |
| `Objective` | `Objective` |
| `Candidate`, `CandidateDir`, `SourceSnapshot`, `Base` | `WorkspaceRef{id, manifest_digest (host-computed), git_revision, dirty}` |
| `Mode` (non-mutating) / `Permissions` | `ModeReadOnly`/`ModeReadWrite` and explicit grants; command grants name host-fixed argv |
| `Deadline`, `Budgets.WallLimit` | `Budget.Deadline` = the earlier of the two |
| `Budgets.MaxTokens` | an explicit host split into `MaxInputTokens`/`MaxOutputTokens` (§5) |
| `Budgets.MaxCostMicros` | `Budget.Money` only when every eligible binding has a trusted complete rate card; otherwise no ceiling and no claim of one |
| agent model, auth mode, frozen binding | one pinned `ProviderBinding` with `ConfigFingerprint`, `Isolation` as the host can prove it |
| operator stop / controller shutdown | `context.WithCancelCause` with `api.Cancellation(api.CancelOperatorStop / api.CancelControllerShutdown)` |

Result translation (proposal; Gate B decides and tests each row):

| Kernel termination | Host |
| --- | --- |
| `completed/loop_completed` | provider observation "loop ended"; never acceptance; assurance runs as today |
| `exhausted/*` | `OperationFailed`; `deadline` → `execution_deadline_exceeded`; others → `execution_incomplete` |
| `blocked/provider_unavailable` | `provider_rate_limited` or `provider_unavailable` by `ProviderError.Class` (detail), routed to the host's durable wait |
| `failed/provider_failed` | by class: `auth` → account unavailable; `transport` → `connectivity_unavailable` / transient; else `transient_execution_provider` or `unknown` |
| `failed/provider_refused` | `OperationFailed` with a refusal diagnostic |
| `failed/tool_failed` | unknown side-effect outcome: the host reconciles the candidate before any retry |
| `blocked/invalid_request`, `insufficient_capacity`, `no_eligible_provider` | pre-execution refusal; no attempt charged as executed |
| `cancelled/host_cancelled` | the host's own termination owner already committed (`operator_stop`, `controller_shutdown`); the kernel's `cancellation` must agree, never override |
| `incomplete/recording_failed` | `execution_incomplete` with the observed outcome in the diagnostic |

`Usage.Reported` (never estimates) → `Tokens`; `Usage.Cost` only when
`known` → `CostMicros`; `Provenance` → `InvocationProvenance`; `Executed` =
at least one `provider.responded`; `ChangedPaths`/`ChangeSummary` from the
host's own diff after `Execute`.

## 4. Host authorities preserved

Gate B must leave each of these with its current owner: candidate writer guard
(#241 git guard, `RequireGitGuard`), process containment and the five
termination owners, progress/inactivity watchdogs, attempt identity and
journal recording, run/plan budget consumption and carry-forward
(`RemainingBudgets`), durable waits and retry eligibility (`RouteFailure`,
`PriorAttemptContextEligible`), candidate credential/commit guards
(`GuardCandidate*`), independent assurance, authority and publication. The
kernel's settlement latch maps onto the existing owners; it is not a sixth
owner.

## 5. Missing kernel obligations

Gate B must close or explicitly carry each; none may be bypassed because Gate A
lacks it.

1. **Inactivity / no-progress bound.** Root has `InactivityLimit`,
   `ProviderInactivityLimit` and `NoProgressLimit`; the kernel has none.
2. **Single token budget.** Root `MaxTokens` is one total; the kernel needs
   an input/output split.
3. **Path policy.** Kernel file tools refuse any `.git` component
   (case-insensitive) but not credential-shaped names or contents; root
   `ToolBroker` refuses both (`GuardCandidate`).
4. **Patch semantics.** Root applies unified diffs via `git apply`; kernel
   `apply_patch` is a single exact replacement. No kernel `Diff` tool.
5. **Isolation.** Root `OpenAIProvider` claims `proven` isolation via its
   resolve gate and `DockerSandbox`; kernel tools are `unproven`. A kernel path
   may claim `host_proven` only with host tools and the sandboxed runner.
6. **Quota vs rate limit.** The kernel classifies every 429 `rate_limited`;
   root distinguishes `provider_quota`.
7. **Uncertain side effects.** An execution that stops after a mutating tool
   records `tool.executed` "side effect outcome uncertain"; the adapter must
   map it to candidate reconciliation, never to "nothing changed". (Incomplete
   or paused provider stops now settle `failed/provider_failed`, not
   `completed`.)
8. **Forensic transcript.** Root writes a redacted transcript artifact per
   `(RunID, OperationID, Attempt)`; the kernel records events only.
9. **Reviewer/feedback/handoff files**, `RequiredTools`/`MissingTools`
   probing, `PriorAttemptFailure` gating: host-side, no kernel equivalent.
10. **Defaults.** Root defaults zero bounds to finite values; the kernel refuses
    zero, so the adapter supplies every bound explicitly.
11. **Attempt identity and budgets.** The kernel enforces one budget
    envelope per `execution_id` through its admission store (execution spec
    §11.1): a re-entered `attempt_id` is refused, a later attempt cannot widen
    the first budget and starts from recorded consumption, and an unsettled
    prior attempt blocks the next. Gate B must:
    - map the host's attempt identity onto `execution_id`/`attempt_id`
      consistently: one host execution is always one `execution_id`, and one
      host attempt (`RunID`, `OperationID`, `Attempt`) is always one
      `attempt_id`, so a retried or resumed host attempt can never present a
      fresh `execution_id` that escapes the envelope;
    - supply a durable `engine.Config.Admissions` (`storage.FileRecords`; the
      default is in-memory, scoped to one `Engine` instance). Its
      `PutIfAbsent` claims are atomic across Engines and processes sharing
      the root on one local filesystem; a network filesystem needs its own
      proof;
    - define the explicit recovery protocol for an abandoned claim (crash,
      failed settlement write): the kernel never expires one by time and
      refuses every later attempt of that execution until the host recovers
      it;
    - keep envelopes that span executions (run, plan, operator) host-owned.
12. **Harness/host seams.** No exported manifest-digest helper outside
    `intelligence.Build`/`Open` (the host computes `WorkspaceRef.ManifestDigest`
    itself); `ExecutionResult` has no structured per-call tool results
    (`cmd/kernel-eval` decodes the transcript); `CapabilityKind` has no
    non-file read kind; `command.run` is `read_write`-only.
13. **Live adapter verification** under an approved spending bound (providers
    spec §6).

## 6. Retiring the duplicate loop

`runtime/openai_provider.go` and the model-facing surface of
`runtime/tool_broker.go` duplicate the kernel's loop and broker. Gate B picks
one owner:

- **Adopt:** the kernel engine becomes the loop for brokered API execution;
  `ToolBroker`'s resolve gate and `DockerSandbox` survive as host tool/runner
  implementations; `OpenAIProvider`'s loop is removed in the same controlled
  migration once the Gate B tests below pass. Coexistence is limited to the
  opt-in dogfood window and ends at a named removal step.
- **Defer:** the module stays experimental and the root loop remains the only
  production loop.

An indefinite two-owner loop is not an option.

## 7. Enablement and rollback

- **Opt-in.** A new execution-agent kind (not a reinterpretation of an
  existing agent or `openai` kind), configured through the established agent
  registry and frozen-binding points. Default off. No change to the selected
  or default worker, no automatic provider switch, no subscription-to-API
  fallback, no silent billing change.
- **Rollback.** Disable or remove the agent; new runs use the existing path.
  Recorded attempts keep their recorded provider kind and provenance; turning
  the feature off never reinterprets them. Kernel DTOs persist only as
  versioned artifacts (`agentkernel.execution/v0.1`), never as canonical host
  records, so a version change cannot rewrite history.

## 8. Gate B test list

1. Translation round-trip: every host request field above maps or is refused;
   no default widens a host bound.
2. Termination mapping table (§3) row by row, including late cancellation not
   rewriting an observed outcome.
3. Operator stop and controller shutdown produce the host termination owner
   and a matching kernel `cancellation`; no sixth owner.
4. Journal append failure → `recording_failed`, no further side effect.
5. Budgets: run and plan budgets decrease by kernel-reported usage only; unknown
   stays unknown; retries never renew. Host attempt identity maps onto
   `execution_id`/`attempt_id` consistently, and two host processes admitting
   attempts of one `execution_id` against a shared admission store admit at
   most one (atomic compare-and-put or host serialization).
6. Durable waits: rate-limited/unavailable/quota route to existing waits; the
   kernel never polls.
7. Candidate integrity: writer guard, credential guard, commit content guard
   and `.git` protection hold for kernel-driven writes.
8. Commands run only through `DockerSandbox` with network denied.
9. Existing crash, termination, quota, cancellation and candidate-integrity
   regressions pass unchanged for affected behaviour.
10. Comparative dogfood on identical task snapshots against the existing
    provider, with exact-tree independent assurance and approved spend (#66
    outcome definitions).
11. Enable/disable/rollback with in-flight and historical runs.
12. Live adapter conformance under a spending bound, recorded with date and
    API version.

## 9. Root documents Gate B must amend

Not touched in Gate A; each needs the Gate B issue's approved file scope:

- `docs/construction-principles.md` — the YAGNI list ("an AI agent router",
  "a raw-LLM coding harness") must be amended for the adopted scope, or the
  module stays experimental (module ADR-0001 §8.1).
- `docs/architecture.md` and `docs/product-architecture.md` — place the Agent
  Execution Kernel and the host adapter; keep "one store, one scheduler".
- `ROADMAP.md` — record adoption or deferral.
- A root ADR (next free number) for the adoption decision.
- Operator docs touched by a new agent kind: `docs/agents.md`,
  `docs/configuration.md`, `docs/troubleshooting.md`.
