# ADR-0005: The Execution Port

- Status: Accepted
- Date: 2026-10-07
- Issue: #521 (child of #445; unblocks #518, Gate B of #446). This ADR lands
  with #521 PR B, which creates the package it describes.

## Context

Gate B (#518) adds the Agent Execution Kernel as one more execution
implementation. That is only an adapter replacement if the host has ONE stable
execution seam to plug it into. Before #521 it did not:

- the port types (`ExecutionProvider`, `ExecutionRequest`, `ExecutionResult`,
  `FailureClass`, ...) sat in the 1,160-line `runtime/adapters.go`, mixed with
  assurance, evidence-producer and candidate-guard concerns;
- the provider decoded host-owned result files, so `ExecutionResult` carried a
  reviewer verdict and a feedback resolution (removed by #521 PR A, #524);
- hidden `context` side-channels, anonymous capability assertions and host code
  calling helpers in provider files formed the real contract (PR C).

## Decision

### The frozen boundary

```text
EngineeringRun / WorkUnit -> host (runtime) -> execution.Port -> native providers
                                                              (later: the kernel adapter)
```

**The host owns** run and attempt identity, budgets, cancellation provenance,
the process termination owner, journal and evidence recording, candidate and
workspace authority, and durable waits and retries.

**An execution implementation owns** one bounded model/tool execution, the
provider-specific interaction, and usage and progress observations.

### Where it lives

A new top-level leaf package, `execution/`, following this repository's
top-level convention (`authority/`, `evidence/`, `orchestration/`) rather than
#445's `internal/execution` sketch. It contains only vocabulary:

- `Port` (the `Execute` signature unchanged), `Request` (moved as-is),
  `Budget`, `AttemptRef` and `Validate`, `Purpose`;
- the value types a request and result carry (`Ref`, `Candidate`, `Artifact`,
  `Finding`, `FeedbackContext`, `UpstreamContext`, `PriorAttemptObservations`,
  `InvocationProvenance` and the types it embeds);
- `Outcome` (`Succeeded`, `Failed`, `Cancelled`), `Result`, `Failure`;
- `FailureClass`, with only the provider-emitted classes declared here;
- `TerminationOwner` (the five owners plus Undecided and NotStarted, values
  unchanged), `ErrRunStopped`, `ErrProviderInactive`, `OwnerOfCause`,
  `CancellationClass`, `NotStartedError`, `NotStartedResult`;
- `Progress` with `WithProgressRecorder`/`ProgressRecorder`, and
  `ProviderIsolation`.

Import rules, enforced by `execution/architecture_test.go`:

- **R1** `execution` imports only the standard library and `domain`.
- **R2** packages under `execution/*/` import none of `runtime`,
  `controlplane`, `authority`, `evidence`, `planning`, `orchestration`.
- **R3** only `cmd/...` imports an `execution/*/` adapter, and no package of
  this module imports `agentkernel/...`. Gate B relaxes the second half to
  exactly `execution/agentkernel`.

### What stays in `runtime`, deliberately

`RouteFailure` and the host-only failure classes; the capability interfaces;
`terminalOwnership`/`runBoundedProcess`; `verificationExecution`; the
candidate-writer lock and the prompt renderers; `CLIAgentProvider`, the Claude
parser, `OpenAIProvider` and `ToolBroker`. Gate B retires the OpenAI loop and
the model-facing `ToolBroker` surface, so moving them now would be wasted work.
The constants of the moved host-vocabulary types (`TrustMode`, `AttemptBound`,
`FeedbackClass`, `GitActorOrigin`, `GitTargetClass`) stay with the runtime code
that assigns them; only the types moved.

### The alias seam

`runtime/execution_aliases.go` keeps type, constant and variable aliases
(`type ExecutionRequest = execution.Request`, ...) so the existing `Execute`
implementations and tests compile unchanged. The aliases are a temporary seam,
not vocabulary: their removal point is **#445 Slice 9**, which rewrites the
remaining references to the `execution` names.

The one non-alias churn is `Outcome`. The host's `OperationState` carries
pending, leased, running and unknown states an execution never reports, so it
cannot be an alias of `execution.Outcome`. Result sites were rewritten
mechanically, and the host converts once (`providerOutcome`). The strings are
the same, so nothing on the wire or in durable state changes.

### Fields whose ownership is ambiguous (recorded, not changed)

- `Result.Attempt` echoes the host value, which is authoritative.
- `Deadline` and `Budgets.WallLimit` represent one bound twice.
- `InactivityWindow` is Claude-specific.
- `PriorAttemptContextEligible` runs inside a provider.
- `InvocationProvenance.DeadlineBound` is set by the host, and `GitRefusals`
  are relayed through the provider.
- `Executed` and `Invocation != nil` are two signals for the same thing.
- Token and cost budgets are enforced only by the OpenAI loop.
- `Outcome` and `Failure` together form a bag of flags.

## Contradictions recorded for #445 and #518

1. #445 orders Slice 3 after Slices 0-2. This slice jumps ahead because #518
   sequences it (#445 is not stop-the-world, per its 2026-10-04 comment), so
   it carries its own ADR and import test.
2. It uses `execution/` instead of #445's `internal/execution`.
3. It is a partial Slice 3: the vocabulary moves, but the classifiers
   (`classifyAgentFailure`) and the process runner do not.
4. Two #518 acceptance items need restating: there is no host journal append
   during execution (only the progress row and the transcript artifact are
   written mid-flight), and nothing in the host consumes
   `Result.Tokens`/`CostMicros` today.
5. Cancellation is mapped twice: `execution.CancellationClass` and the OpenAI
   loop's `cancellationOwner`, which keeps `unknown` for every cause but an
   operator stop. Both are preserved and pinned by tests; Gate B picks one
   before the OpenAI loop is retired.
6. The `candidateBoundProvider` `MissingTools` forwarding defect is #522. It is
   fixed between PR B and PR C, so PR C's capability assertions codify the
   corrected boundary.

## Consequences

### Positive

- Gate B adds the kernel as one more `execution.Port` implementation under
  `execution/agentkernel`, composed in `cmd/`, without a cross-cutting runtime
  change.
- The compiler and the import guards, not convention, keep host and
  governance concepts out of the seam.

### Costs

- Two names for every moved type until #445 Slice 9 removes the aliases.
- The seam still carries the ambiguous fields above and the context
  side-channels PR C names; this ADR freezes where the boundary is, not that
  everything crossing it is already clean.

## Related

- ADR-0006 (#523, adopt the Agent Execution Kernel) builds on this port.
- ADR-0002 — the local engineering runtime and its execution trust boundary.
