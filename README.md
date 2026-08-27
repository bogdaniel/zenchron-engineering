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
```

## License

No license has been selected yet. Do not assume permission to redistribute or reuse this code until an explicit license is committed.
