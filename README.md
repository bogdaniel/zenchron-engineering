# Zenchron Engineering OS

Policy-governed autonomous software engineering infrastructure that turns engineering intent into bounded work, verified evidence, and authorized software changes.

## Thesis

Zenchron Engineering is **not** a generic multi-agent orchestrator. Coding agents are replaceable execution providers. The durable system governs engineering change:

```text
engineering intent
  -> engineering facts
  -> policy
  -> obligations and invariants
  -> bounded execution
  -> evidence
  -> authority decision
```

The core rule is simple: **agents may reason and execute, but they may not self-authorize material outcomes.**

## Status

This repository is in architecture/bootstrap stage. The first objective is to specify and validate the Engineering Authorization Kernel before building a broad orchestration or control-plane product.

## Technology decisions

- Core implementation language: **Go**
- Canonical machine-readable representation: **JSON**
- Contract/schema format: **JSON Schema**
- Agent providers: replaceable adapters; initial targets are Codex and Claude
- Assurance providers: pluggable; Sentinel Shield is the Zenchron-native high-fidelity integration
- Execution environments: pluggable; Zenchron Foundry is the native provenance-oriented integration

YAML is not a canonical representation. It may be supported later only as an optional authoring format that normalizes to JSON.

## Core domain

The v0.1 domain is centered on:

- `ProjectModel`
- `EngineeringFact`
- `EngineeringPolicy`
- `EngineeringWorkContract`
- `EvidenceBundle`
- `AuthorityDecision`

See [`docs/spec/v0.1.md`](docs/spec/v0.1.md).

## Repository map

```text
AGENTS.md                    Persistent instructions for coding agents
docs/vision.md               Product thesis and boundaries
docs/principles.md           Frozen architectural principles
docs/architecture.md         System architecture
docs/adr/                    Architecture decisions
docs/spec/v0.1.md            Initial domain specification
schemas/                     Canonical JSON Schemas and Go validation tests
domain/                      Go v0.1 representations and canonical JSON codecs
reassessment/                Observed-scope validation and contract reassessment
fixtures/v0.1/               Positive and targeted invalid schema fixtures
cmd/zenchron-engineering/    CLI entry point
```

## Non-goals for the first milestone

Do not prematurely build a dashboard, Kubernetes orchestration, a visual workflow designer, a generic multi-agent framework, a vector database, a custom CI system, a marketplace, or enterprise RBAC.

The first milestone must prove that the same facts + policies can produce appropriately different engineering obligations and authority decisions across low-, medium-, and high-impact work.

## Development

```bash
go test ./...
go run ./cmd/zenchron-engineering version
go run ./cmd/zenchron-engineering selfhost issue 4
go run ./cmd/zenchron-engineering selfhost issue 4 --model gpt-5.6-terra --fallback-model gpt-5.6-luna
go run ./cmd/zenchron-engineering selfhost resume issue 4
```

`selfhost issue` requires authenticated local `gh` and `codex` CLIs. It only
starts from a clean, synchronized `main`, creates a dedicated issue branch,
requires an open PR and durable review handoff, and never merges it.

Codex execution always uses an explicit model. ChatGPT-authenticated sessions
default to the issue-scoped migration targets `gpt-5.6-terra` then
`gpt-5.6-luna`; other authentication modes require `--model`. Up to two
`--fallback-model` values may follow. Only recognizable transient capacity
failures advance to the next model, and only while the issue branch, history,
and working tree remain unchanged. The durable handoff records the successful
model, authentication-mode class, and attempt count, never credentials.

Go-backed bootstrap operations resolve one runtime before repository mutation:
a compatible local Go installation is preferred, with automatic toolchain
downloads disabled. Otherwise Docker is used only when its daemon is reachable
and the repository-derived `golang:<go.mod version>` image already exists
locally. Bootstrap never pulls that image implicitly and executes the resolved
immutable image ID. Docker Go commands use an unprivileged container with only
the repository mounted. The container has outbound network access so Go can
resolve the non-vendored modules pinned by `go.mod` and `go.sum`; no host
credentials or environment are forwarded. Go's home, module, and build caches
live in an isolated writable tmpfs that is discarded after each command.

Before either `selfhost issue` executes Codex or `selfhost resume` publishes an
interrupted candidate, selfhost writes a preflight diagnostic identifying the
resolved repository, exact trusted `origin/main` base revision, and selected Go
runtime.

Before publishing a candidate PR, the bootstrap independently runs the
deterministic `format`, `vet`, and `test` checks through that resolved runtime.
Executor-reported commands are retained as observations and need not use the
same shell spelling. If execution is interrupted after creation of an
`issue-N` branch but before publication, `selfhost resume issue N` can recover
only an uncommitted candidate exactly based on `origin/main`; it refuses
unrelated history, ignored local state, a remote branch, or a PR. Successful
Codex execution provenance is retained only in Git-local state for that
interrupted candidate and is removed after the handoff is published. Both paths
stop before merge.

## License

No license has been selected yet. Do not assume permission to redistribute or reuse this code until an explicit license is committed.
