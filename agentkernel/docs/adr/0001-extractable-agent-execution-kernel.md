# Module ADR-0001: Extractable Agent Execution Kernel (Gate A)

- Status: Accepted for Gate A only (module-local; not a root ADR)
- Date: 2026-10-07
- Issue: #446 (Gate A). Related: #445, #67, #70, #103, #471.
- Scope of this record: `agentkernel/**` and `.github/workflows/agent-kernel.yml`.

This is a **module-local** decision record. It does not take a root ADR number,
does not amend `docs/architecture.md`, `ROADMAP.md`, `docs/construction-principles.md`
or any root ADR, and must not be read as having done so. Those edits belong to
Gate B's explicitly scoped documentation change.

## 1. Vocabulary: two different kernels

The word "kernel" alone is ambiguous in this repository and is not used alone
anywhere in this module.

| Term | Owner | Meaning |
| --- | --- | --- |
| **Engineering Authorization Kernel** | root repository (ADR-0001, `domain/`, `policy/`, `authority/`, ...) | facts -> policy -> obligations -> evidence -> authority. Decides what engineering work may do and whether an outcome is accepted. |
| **Agent Execution Kernel** | this module (`agentkernel/`) | Runs **one bounded execution**: context compilation, provider/tool loop, budgets, capability brokering, observations. Decides nothing about engineering legitimacy. |

The Agent Execution Kernel can execute a bounded request without knowing that
Zenchron Engineering exists. The host alone decides what engineering work is
legitimate and what evidence permits its outcome.

## 2. Decision

1. **Gate A is an explicitly scoped experiment** authorized by #446. It ships a
   working, independently testable module with no production wiring: nothing in
   the root module imports it, no default, configuration, schema, journal or
   worker selection changes, and no provider credit is spent by CI.
2. **One repository, one separate nested Go module**
   (`github.com/bogdaniel/zenchron-engineering/agentkernel`, `go 1.25.0` to
   match the root module). No `go.work`, no root `go.mod` edit, no `replace`,
   no new service boundary. Extraction into its own repository must remain
   continuously demonstrable (`scripts/extract.sh`), not assumed necessary.
3. **Dependency law.** Production code, tests, examples and tools import only
   this module and the Go standard library. Gate A adds **no external
   dependency** (no `go.sum`). Any parent-module import, test-only or
   build-tagged included, fails `tests/architecture` and `scripts/check-imports.sh`.
4. **The kernel owns one bounded execution; the host owns engineering
   semantics around it.**

   ```text
   host -> ExecutionRequest -> Agent Execution Kernel -> provider/tool loop
        <- ExecutionResult (observation only) <-
   ```

   The result carries outcome, observations, artifact references, usage,
   provenance and one immutable termination observation. It has no acceptance,
   merge, authorization or candidate field. `completed` means the loop ended
   because the provider stopped proposing tools; it does not mean the objective
   was satisfied.

## 3. Responsibility matrix

| Concern | Owner |
| --- | --- |
| Engineering contracts, policy, plans, candidate lifecycle, assurance, acceptance | Zenchron (unchanged) |
| Controller, run leases, scheduling, durable retries/waits, process termination owners | Zenchron (unchanged) |
| Provider eligibility, frozen binding, total run budget | Host, translated into `ExecutionRequest` constraints |
| One bounded execution, context selection, tool proposals, local resource accounting | Agent Execution Kernel |
| Provider wire behaviour, usage normalization | Kernel provider adapters (`providers/*`) |
| Tool permission | Host-granted capability + broker validation; host revalidates at side-effect time |
| Repository bytes/symbols/imports with exact provenance | Kernel `intelligence/` (no engineering meaning) |
| ProjectModel, impact facts, policy-relevant interpretation | #67 |
| Learned production routing, accepted/rejected labels | #70 / host assurance and authority |
| External executable discovery/trust/wire protocol | #103 |

There is one engineering scheduler and it is not here. The kernel's per-execution
budget ledger is a resource limiter, not a scheduler, run store or queue.

## 4. Durable continuity and provider sessions

Durable engineering state lives in the host. Provider sessions are ephemeral
execution resources. Every provider call carries the full bounded transcript the
kernel compiled; no adapter requires an opaque provider conversation/session id
for correctness. Session or prompt-cache reuse is reported only as an
observation (`SessionObservation`) and treated as an optimization, never as
correctness or authority. Provider argv, Responses/Messages wire internals and
cache internals do not appear in `api/`.

## 5. Requirements carried (summary; specs are normative)

- Versioned request/result/event contract (`api/`, `schemas/`,
  `docs/spec/execution-v0.1.md`); unknown versions, invalid identifiers,
  duplicates, conflicts and unsupported mandatory features are refused before
  any side effect.
- Finite explicit budgets (deadline, iterations, tool calls, input/output
  tokens, artifact bytes, provider retries); shared atomic reservations; no
  retry or re-entry renews budget, enforced by the kernel: `Budget` is the
  envelope of an `execution_id` across all its attempts, admission is an
  atomic claim (`storage.Records.PutIfAbsent`), a crashed attempt fails
  closed, and no clock passage releases a claim; a monetary ceiling that
  cannot be enforced from trusted prices and token bounds is refused.
- Capability broker: model tool calls are untrusted proposals; capability comes
  only from host grants; path guards that refuse every symlink and (on unix)
  hard-linked file; expected-content patch preconditions; no unrestricted
  shell (commands are host-named grants executed by the host's process
  boundary).
- Context compiler: mandatory context is never dropped or summarized;
  impossible mandatory context yields `insufficient_capacity`; retrieved,
  remembered and tool content stays untrusted data.
- Repository intelligence: Go + file manifests via stdlib; immutable base
  snapshots; execution-private overlays; explicit unresolved edges.
- Memory: provenance, derivation identity invalidation, conflicts kept,
  partitioned, bounded, atomic, corruption detected.
- Routing: deterministic ranking within the host-supplied eligible set; pinned
  binding preserved; unknown price is unknown, never zero.
- Cancellation: `context.Context` cancellation alone does not establish cause;
  host supplies typed provenance through `context.WithCancelCause`; unknown
  stays explicit. Exactly one settlement; a later cancellation cannot rewrite an
  observed provider completion or failure.
- Recording failure stops further side effects and returns `incomplete` /
  `recording_failed` with the evidence available.
- Host-code rule (added after review 5443104514): the bounded execution path
  never synchronously runs host-owned code whose termination it cannot
  enforce. Event sinks, context sources, providers, command runners and host
  tools are bounded request/reply hand-offs served by host-owned workers; the
  kernel starts no goroutine to call them and none outlives `Execute`. Types
  the engine calls directly are sealed and kernel-owned.

## 6. Limitations (Gate A, stated, not hidden)

- **Not production-enabled.** Gate B (adoption through a host adapter after #445
  boundaries stabilize) is pending and requires a separately scoped issue.
- **Not crash-resumable.** Derived artifacts, index and memory reopen after a
  restart; an execution in flight does not resume, and an uncertain mutation is
  never replayed as though it did not happen.
- **No protected-isolation claim.** File tools validate paths and re-check at
  open time, but this is a development-grade boundary; it is reported as
  `unproven`. Process containment, reaping and the five process-termination
  owners stay with the host.
- **No CLI parity.** Raw API adapters (OpenAI Responses, Anthropic Messages) are
  not native coding-CLI adapters; existing Claude Code/Codex adapters are
  untouched.
- **Live verification.** Adapters are verified offline against recorded/fake
  transports shaped from primary documentation; CI makes no paid calls.
- **CI is engineering discipline, not isolation** from a malicious same-user
  process.

## 7. Relationship to YAGNI and the roadmap

`docs/construction-principles.md` (YAGNI) lists "an AI agent router" and "a
raw-LLM coding harness" as out of scope until a failing acceptance case proves
otherwise. #446 supplies those acceptance cases (A01-A20) explicitly and scopes
this module as a separately tested execution experiment. This ADR does **not**
supersede that rule for the root module and grants no permission to turn
Zenchron into a generic agent platform. Within this module, ranking is
deterministic over a host-eligible set; it is not an "AI router" in the sense
the principle forbids (no learned selection, no eligibility creation).

## 8. Contradictions with committed contracts and the decision each needs

1. **YAGNI raw-LLM harness / router (construction-principles).** Resolved for
   Gate A by #446's explicit scoping (section 7). Gate B must amend
   `docs/construction-principles.md` in its approved file scope, or the module
   stays experimental.
2. **Two bounded model/tool loops.** `runtime/openai_provider.go` and
   `runtime/tool_broker.go` already implement a loop and a tool boundary. Gate A
   neither copies nor edits them. Gate B must choose one owner and retire the
   other in the same controlled migration (#445); an indefinite two-owner loop is
   not acceptable.
3. **"One cancellation path" (construction-principles DRY).** The kernel has its
   own settlement latch for one execution. Gate B must map it onto the host's
   existing termination owners rather than add a sixth owner.
4. **ExecutionRequest naming.** The root `runtime` package already has an
   `ExecutionRequest`. This module's type lives in its own namespace
   (`agentkernel/api`); only the Gate B host adapter imports both vocabularies.

## 9. Alternatives considered

- *Package inside the root module:* rejected; extraction would not be
  demonstrable and parent imports would be one edit away.
- *Separate repository now:* rejected by #446; extraction stays demonstrable.
- *Separate service:* rejected; no process boundary is justified.
- *Shared SQLite (`runtime.db`):* rejected; module-owned in-memory and file
  stores only. Gate B may choose a backend after ownership is proven; a raw
  shared DB handle is forbidden.

## 10. Costs

A second module to build and test (own CI workflow), a second vocabulary until
Gate B maps it, and maintenance of two adapters' offline conformance fixtures.
