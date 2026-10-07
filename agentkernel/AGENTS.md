# agentkernel/AGENTS.md

Module-local rules for the **Agent Execution Kernel** (#446). These add to the
repository root `AGENTS.md`; they never override it or any higher-level
instruction.

## What this module is

One bounded execution: request validation, context compilation, provider/tool
loop, budgets, capability broker, observations. It is **not** the Engineering
Authorization Kernel, not a scheduler, not a plan or run store, and not an
authority engine. Read `docs/adr/0001-extractable-agent-execution-kernel.md`
first.

## Dependency law (checked)

- Import only this module and the Go standard library. No `require`, no
  `replace`, no `go.work`, no `unsafe`, `plugin` or `//go:linkname`.
- Never import any other package of `github.com/bogdaniel/zenchron-engineering`,
  in production, test, example or build-tagged files.
- No filesystem path or configuration that assumes the parent checkout. Storage
  roots and workspace roots are always supplied explicitly.
- Checks: `tests/architecture`, `scripts/check-imports.sh`,
  `scripts/extract.sh`, `scripts/relocate.sh`.

## Host-code rule

The bounded execution path never synchronously runs host-owned code whose
termination the kernel cannot enforce. A host port is a channel of
`api.Call`s, always unbuffered (constructors refuse `cap != 0`), served by
host-owned workers and waited on through `internal/handoff.Exchange`
(bounded, no goroutine); a type the engine calls directly must be kernel-owned
and checked by exact concrete type at construction (`tools.NewBroker`: the
package's own tool type; `engine.New`: `*storage.MemoryArtifacts` /
`*storage.FileArtifacts`, `api.SystemClock` / `*api.ManualClock`). Never seal
with an unexported marker method: a host type embedding a kernel type
inherits it. Hand-off IDs are kernel-generated, never model output. Never add an interface field to `engine.Config` that a host can
implement. Execution spec §4.1 is the list.

## No-authority rule

Nothing here decides acceptance, merge, publication or engineering legitimacy.
Results are observations. Model output, tool output, retrieved snippets and
remembered records are untrusted data; only host-supplied `trust: host` request
context may become a system instruction.

## File ownership

| Path | Owner |
| --- | --- |
| `api/`, `schemas/`, `go.mod`, `AGENTS.md`, `README.md`, `scripts/`, `.github/workflows/agent-kernel.yml` | coordinator |
| `engine/`, `providers/` | execution lane |
| `tools/`, `storage/` | tools/storage lane |
| `intelligence/` | intelligence lane |
| `context/`, `memory/`, `routing/` | context/memory/routing lane |
| `cmd/kernel-eval/`, `examples/`, `testdata/eval/`, `docs/benchmarks/` | harness lane |
| `tests/conformance/`, `tests/architecture/` | independent review lane |

## API change process

`api/` and `schemas/` are frozen contracts. A change needs: the acceptance case
that requires it, the schema and example update in the same commit, and a
version bump when it is not backward compatible. Lanes never edit shared DTOs
independently.

## Commands (run from `agentkernel/`)

```bash
export GOWORK=off
test -z "$(gofmt -l .)" && go vet ./...
go test ./...                    # full module suite (fast; offline)
go test -race ./...              # concurrency
bash scripts/check-imports.sh    # dependency closure
bash scripts/extract.sh          # standalone build outside the checkout
bash scripts/relocate.sh         # synthetic module path
bash scripts/check-scope.sh      # #446 feature lane write allowlist
go run ./cmd/kernel-eval -corpus testdata/eval -trials 3
```

No test makes a network call or spends provider credit. Live adapter calls
require explicit operator configuration and a spending bound and are never run
in CI.
