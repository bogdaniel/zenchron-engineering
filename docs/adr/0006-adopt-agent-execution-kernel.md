# ADR-0006: Adopt the Agent Execution Kernel for Brokered API Execution

- Status: Accepted
- Accepted: 2026-10-08
- Date: 2026-10-07
- Issue: #518 (Gate B of #446; absorbs #477). Prerequisite: the #445
  execution/provider-boundary slice #521; its execution-port decision is
  ADR-0005 (accepted, #526). Related: #67, #70, #103, #66.
- Naming: "ExecutionPort" below is the port #521 freezes as `execution.Port`.

## Context

#446 Gate A delivered `agentkernel/`: an extractable nested Go module that runs
**one bounded execution** (context compilation, provider/tool loop, budgets,
capability brokering, observations) and imports nothing from the root module.
Its acceptance record is `agentkernel/docs/acceptance/gate-a.md`, with evidence
bound to `762bcb6` (#510, binding in #519). Its module-local ADR
(`agentkernel/docs/adr/0001-extractable-agent-execution-kernel.md`) is
explicitly not a root ADR, and it leaves four contradictions for Gate B to
resolve (§8):

1. `docs/construction-principles.md` lists "an AI agent router" and "a raw-LLM
   coding harness" as YAGNI.
2. Production already has a bounded model/tool loop: `runtime/openai_provider.go`
   over the model-facing surface of `runtime/tool_broker.go`. Gate A runs a
   second one beside it.
3. Construction principles allow "one cancellation path". The kernel has its own
   settlement latch.
4. `ExecutionRequest` exists in both vocabularies.

`agentkernel/docs/integration-plan.md` §6 offers two choices, adopt or defer,
and rules out an indefinite two-owner loop. #518 makes the choice a
prerequisite and names its direction: adopt.

Two terms are easily confused, and confusing them is a defect:

| Term | Decides |
| --- | --- |
| **Engineering Authorization Kernel** (ADR-0001) | facts -> policy -> obligations -> evidence -> authority. What work may do and whether an outcome is accepted. |
| **Agent Execution Kernel** (`agentkernel/`) | How one bounded model/tool execution runs. Nothing about engineering legitimacy. |

This ADR concerns only the second. It does not touch the first.

## Decision

**Adopt.** For brokered API execution, the Agent Execution Kernel becomes the
**single owner of the model/tool loop**. The loop in `runtime/openai_provider.go`
and the model-facing surface of `runtime/tool_broker.go` are **retired during
Gate B**, in the same controlled migration, once the parity tests in
integration-plan §8 pass. `ToolBroker`'s resolve gate and `DockerSandbox`
survive as host-owned tool and command-runner implementations behind the
kernel's hand-off ports. Coexistence is limited to the opt-in dogfood window
and ends at a named removal step in #518. There is no indefinite two-owner loop.

### Sequencing

```text
Gate A (#510/#519, evidence at 762bcb6)          done
  -> #445 execution/provider-boundary slice (#521)  freezes ExecutionPort,
                                                 ExecutionRequest, ExecutionResult
  -> #518                                        kernel = one more ExecutionPort
                                                 implementation
```

The kernel enters production as one `ExecutionPort` implementation, behind a
host adapter that lives **outside `agentkernel/`** and is the only code that
imports both vocabularies. That is an adapter replacement, not a new
cross-cutting runtime feature. No #518 code lands before the slice freezes the
port, and #518 does not refactor `runtime/` itself.

```text
WorkUnit / EngineeringRun
  -> ExecutionEnvelope (host-owned translation)
  -> ExecutionPort
       |- native CLI workers           (own ExecutionPort implementations)
       '- Agent Execution Kernel       (via the host adapter)
  -> ExecutionResult + typed handoff/result channel
```

Native coding-CLI workers (`runtime/cli_agent.go`, `runtime/agent_specs.go`)
stay as their own `ExecutionPort` implementations. This ADR claims **no CLI
lifecycle parity** for the kernel and does not route CLI work through it.

### Ownership split

| Owner | Concerns |
| --- | --- |
| Host | run and attempt identity; run, plan and operator budgets; cancellation provenance; the process termination owner; journal and evidence recording; candidate and workspace authority (writer, credential, commit and `.git` guards); durable waits and retries |
| Execution implementation (kernel or CLI worker) | one bounded model/tool execution; provider-specific interaction; usage and progress observations |

The kernel never owns an `EngineeringPlan`, the WorkGraph, evidence
sufficiency, publication authority or scheduling. The orchestrator never sees
provider internals: no argv, wire payloads, tool-call syntax, cache flags or
provider session files. An API worker cannot acquire authority its run lacks.
The kernel's settlement latch maps onto the existing five termination owners and
is never a sixth one. This resolves contradiction 3.

### Enablement and rollback

- **Opt-in:** a new execution-agent kind, default off, configured through the
  existing agent registry and frozen-binding points. It does not reinterpret
  the existing `openai` kind. Nothing changes for the default worker: no
  automatic provider switch, no subscription-to-API fallback, no billing
  change.
- **Rollback during the dogfood window:** both loops exist, so disabling or
  removing the kernel agent kind routes new runs back to the existing
  `OpenAIProvider` path. This is a configuration change.
- **Rollback after retirement:** once the old loop is deleted at #518's named
  removal step, no flag can bring it back. Rollback then means a code and
  deployment rollback to a release that still contains it. The removal step
  therefore lands only after the parity tests pass, and in its own revertible
  change.
- **In both cases** recorded attempts keep their recorded provider kind and
  provenance. Kernel DTOs persist only as versioned artifacts
  (`agentkernel.execution/v0.1`), never as canonical host records, so neither
  kind of rollback reinterprets an already recorded attempt.

### Amendment of the YAGNI position

This ADR amends the `docs/construction-principles.md` YAGNI position for
**exactly this scope**: one bounded, deterministic-routing model/tool execution
engine behind one `ExecutionPort`, replacing the existing API loop. #446's
acceptance cases (A01–A20) and the Gate A evidence are the failing acceptance
cases that principle requires. Routing stays deterministic ranking within a
host-supplied eligible set. It does not authorize a generic agent platform, a
learned router, a multi-agent conversation framework, or a second scheduler,
store, journal, policy or authority system. ProjectModel stays with #67,
learning with #70 and the external executable protocol with #103.

### Root documents Gate B must amend

These are listed here but **not edited by this change**. Each must be in #518's
approved write allowlist (integration-plan §9):

- `docs/construction-principles.md`: the YAGNI list, bounded as stated above.
- `docs/architecture.md` and `docs/product-architecture.md`: place the Agent
  Execution Kernel and the host adapter, and keep one store and one scheduler.
- `ROADMAP.md`: record the adoption.
- `docs/agents.md`, `docs/configuration.md`, `docs/troubleshooting.md`: the new
  agent kind.

### Reversal condition

Defer only if the #445 execution/provider-boundary slice (#521) exposes a real
architectural contradiction. Examples: the frozen `ExecutionPort` cannot carry
a host authority without the kernel owning it; the adapter would need raw
runtime or DB access from inside `agentkernel/`; or a host control would have
to be bypassed. In that case a superseding ADR records the deferral. The module
stays experimental, and the existing loop remains the only production loop.
Inconvenience, migration cost or an unfinished obligation is not such a
contradiction.

## Consequences

### Positive

- One model/tool loop for API execution instead of two. Its budgets, refusals,
  settlement and recording-failure semantics are already tested adversarially.
- Provider-specific behaviour stays behind one port. CLI and API workers share
  the host's lifecycle, authority and evidence semantics.
- The execution engine stays extractable, and nothing in the authorization
  kernel depends on it.

### Costs

- **Migration risk.** Retiring `OpenAIProvider`'s loop touches a production path
  that has crash, termination, quota, cancellation and candidate-integrity
  regressions. All of them must pass unchanged.
- **A duplicate loop until parity.** Both loops exist during the dogfood window.
  The removal step must be named and must not slip.
- **Live verification costs money.** Live adapter conformance and comparative
  dogfood (#66 outcome definitions, `kernel-eval` baseline 5) need approved
  spend and approved credentials. Without both, #518 stops.
- **Gate A limits carried into Gate B,** each closed or explicitly carried,
  never bypassed (integration-plan §5):
  - kernel file tools do not refuse credential-shaped paths or contents, which
    `GuardCandidate` does;
  - every 429 is classified `rate_limited`, so quota and rate limit are not
    distinguished;
  - there is no bridge to the host artifact store (`Config.Artifacts` is
    exact-type), so the host needs a hand-off or `FileArtifacts`;
  - there is no durable recovery protocol for an abandoned admission claim; the
    kernel never expires one by time;
  - kernel-owned file I/O is not deadline-bounded;
  - there is no inactivity bound, no input/output token split and no forensic
    transcript.

## Invariants

- Exactly one owner of the brokered API model/tool loop after Gate B.
- The Agent Execution Kernel returns observations, never acceptance or
  authority. `completed` means the loop ended, not that the objective was met.
- Host authorities stay with their current owners, with no sixth termination
  owner.
- Enabling, disabling or rolling back (by configuration during dogfood, by
  code/deployment after retirement) never reinterprets recorded attempts.
- The YAGNI amendment covers this scope only.
