# Agent Execution Kernel (`agentkernel/`)

A self-contained Go module that runs **one bounded model/tool execution**:
strict request validation, context compilation, eligible-provider routing, a
provider/tool loop under a finite budget, a capability broker over
host-supplied tools, and a single settled, observation-only result.

It is **not** the Engineering Authorization Kernel of the root repository. It
decides nothing about engineering legitimacy, acceptance, merge or publication.
`completed` means the provider stopped proposing tools, never that the
objective was met. See
[`docs/adr/0001-extractable-agent-execution-kernel.md`](docs/adr/0001-extractable-agent-execution-kernel.md).

## Maturity

**Gate A: isolated kernel complete; production adoption (Gate B) pending.**

Nothing in the root module imports this module. No default, configuration,
schema, journal, worker selection or billing path of Zenchron Engineering
changes because it exists. It is not production-enabled. Adoption requires a
separately scoped integration issue; the plan is
[`docs/integration-plan.md`](docs/integration-plan.md).

| Area | State |
| --- | --- |
| Execution API, schemas, engine | implemented, offline-tested |
| Tools: file read/search/write/patch, `read_files` macro, host-run commands | implemented; path guards are development-grade (`unproven` isolation) |
| Providers: scripted, OpenAI Responses, Anthropic Messages, local-protocol seam | offline conformance only; **no live call has been made** |
| Repository intelligence (Go), memory, routing, storage | implemented, offline-tested |
| Production wiring, CLI parity, crash resume | not in Gate A |

## Layout

| Package | Responsibility |
| --- | --- |
| `api` | versioned contract: request, result, event, ports, validation |
| `engine` | the bounded loop (`engine.New`, `(*Engine).Execute`) |
| `context` | deterministic context manifest (`context.Compile`) |
| `routing` | deterministic ranking inside the host's eligible set (`routing.Select`) |
| `tools` | capability broker (`tools.NewBroker`) and built-in tools |
| `storage` | module-owned artifact and record stores (memory and file) |
| `memory` | provenance-bearing derived records over `storage.Records` |
| `intelligence` | revision-bound Go structural index, overlays, views |
| `providers/*` | `scripted`, `openai`, `anthropic`, `local`, shared `conformance` suite |
| `schemas/` | canonical JSON Schemas (`agentkernel.execution/v0.1`) and examples |
| `examples/` | `standalone` (A01) and `hostport` (A02) runnable consumers |
| `cmd/kernel-eval` | offline benchmark harness over `testdata/eval` (`docs/benchmarks/`) |
| `tests/` | `architecture` (dependency law) and `conformance` (independent acceptance) |

Architecture and the real import graph: [`docs/architecture.md`](docs/architecture.md).
Normative specs: [`docs/spec/`](docs/spec/).

## Commands

Run from `agentkernel/` with the workspace disabled. The module has no
dependencies outside the standard library (no `go.sum`).

```bash
export GOWORK=off
test -z "$(gofmt -l .)" && go vet ./...   # format and vet
go build ./...
go test ./...                             # full module suite; offline
go test -race ./...                       # budgets, storage, events, overlays
bash scripts/check-imports.sh             # dependency closure on 3 platforms
bash scripts/extract.sh                   # copy module outside the repo; build/vet/test
bash scripts/relocate.sh                  # rewrite to example.invalid/relocated/agentkernel; build/test
bash scripts/check-scope.sh               # #446 write allowlist vs merge-base (BASE_REF, default origin/main)
go run ./examples/standalone              # scripted read+write task; prints the result JSON
go run ./examples/hostport                # host-supplied ports, document task
go run ./cmd/kernel-eval -corpus testdata/eval -trials 3   # offline benchmark; JSON report
```

The root repository's `go test ./...` never enters this module; CI runs it
through `.github/workflows/agent-kernel.yml`. No test makes a network call or
spends provider credit.

## Example

`examples/standalone/main.go` is the maintained, tested example
(`TestStandaloneSettlesAndWrites`). Its shape:

```go
ws, _ := tools.NewWorkspace(dir, nil)                  // absolute, clean dir
broker, _ := tools.NewBroker(ws.ReadFile(), ws.WriteFile())
artifacts, _ := storage.NewMemoryArtifacts(1 << 20)
eng, _ := engine.New(engine.Config{
	Providers: map[string]api.Provider{"scripted": scripted.New(
		scripted.Step{Response: api.ProviderResponse{Text: "done", Stop: api.StopEnd}},
	)},
	Broker: broker, Artifacts: artifacts, Events: sink, Clock: clock, OutputLimit: 4096,
})
res, _ := eng.Execute(ctx, api.ExecutionRequest{
	Version: api.ExecutionVersion, ExecutionID: "ex-1", AttemptID: "a-1",
	Objective: "Summarize notes.txt.", Mode: api.ModeReadOnly,
	Workspace:   api.WorkspaceRef{ID: "ws", ManifestDigest: manifestDigest},
	Constraints: api.Constraints{InstructionDigest: api.Digest([]byte("host instructions"))},
	Grants:      []api.Capability{{Handle: "read", Kind: api.CapabilityFileRead, Roots: []string{"."}}},
	Budget: api.Budget{Deadline: time.Now().Add(time.Minute), MaxIterations: 5, MaxToolCalls: 5,
		MaxInputTokens: 50000, MaxOutputTokens: 4000, MaxArtifactBytes: 1 << 20},
	Providers: []api.ProviderBinding{{ID: "scripted", Kind: "scripted", Model: "fixture",
		ModelVersion: api.UnknownVersion, ConfigFingerprint: "fp", Eligible: true,
		Isolation: api.IsolationUnproven, ContextWindow: 32000, MaxOutputTokens: 1000}},
})
// res.Termination.Outcome == api.OutcomeCompleted; the error is always nil.
```

`sink` implements `api.EventSink` and `clock` implements `api.Clock`; both are
required, nothing is defaulted.

## Limitations

- **Not production-enabled.** Gate B is pending.
- **Not crash-resumable.** Stores reopen after a restart; an in-flight
  execution does not resume and an uncertain mutation is never replayed.
- **No protected-isolation claim.** File-tool guards (`os.Root`, symlinked-root
  refusal, re-check before rename) are development-grade; provenance reports
  `isolation: unproven` unless the host proves otherwise. `.git` paths are
  refused; credential-shaped names and contents are not. Process spawning,
  containment and reaping belong to the host's `api.CommandRunner`.
- **No CLI parity.** The OpenAI and Anthropic adapters are raw API adapters,
  not native coding-CLI adapters.
- **Budget envelope is per execution, enforced per Engine store.** Every
  attempt of one `execution_id` shares one envelope: re-entry, widening and
  starting beside an unsettled attempt are refused (execution spec §11.1).
  The default admission store is in-memory; enforcement across restarts
  needs `engine.Config.Admissions` backed by `storage.FileRecords`, and
  several processes sharing one store must serialize admission. Envelopes
  spanning executions remain the host's.
- **Bounded recording, not bounded adapters.** A stuck `EventSink` or
  `ContextSource` is abandoned after the deadline (or cancellation) plus
  `SettleTimeout`; providers, tools and `CommandRunner`s are never abandoned
  and must honour their context.
- **Live verification: none.** Adapters are verified against fake transports
  shaped from primary documentation retrieved 2026-10-07.
- **File-backed stores refuse Windows** (`storage.prepareRoot`): directory
  fsync is unavailable there.
- **CI is engineering discipline**, not isolation from a malicious same-user
  process.

Acceptance evidence for Gate A: [`docs/acceptance/gate-a.md`](docs/acceptance/gate-a.md).

## Contributing

Read [`AGENTS.md`](AGENTS.md) (dependency law, file ownership, API change
process) and [`docs/concurrency.md`](docs/concurrency.md) before editing.
