# AGENTS.md

This file is the persistent engineering handoff for Codex, Claude Code, Cursor, Gemini, local agents, and future coding agents working in this repository.

## Read first

Before making architectural or implementation changes, read:

1. `README.md`
2. `docs/vision.md`
3. `docs/principles.md`
4. `docs/architecture.md`
5. all accepted ADRs under `docs/adr/`
6. `docs/spec/v0.1.md`

Committed repository documents are the project source of truth. Chat history is not.

## Project identity

**Product/category:** Zenchron Engineering OS

**Repository:** `zenchron-engineering`

**Core:** Engineering Authorization Kernel

Zenchron transforms engineering intent into bounded, policy-governed work, independently verifiable evidence, and explicit authority decisions.

It is not primarily an agent orchestrator.

## Core invariant

```text
engineering facts
  -> policy
  -> obligations/invariants
  -> evidence
  -> authority
```

Agents may reason and execute. Agents do not self-authorize material outcomes.

## Technical constraints

- Core implementation language is Go.
- Canonical persisted/interchanged representation is JSON.
- JSON Schema defines external contract shapes.
- Do not introduce YAML as an equal canonical representation.
- Provider-specific behavior must remain behind adapters.
- The authorization kernel must not depend on Claude, Codex, or any single model vendor.
- Sentinel Shield is the preferred native assurance integration, not an unavoidable platform dependency.
- Zenchron Foundry is the preferred native trusted-environment/provenance integration, not an unavoidable platform dependency.

## Architectural constraints

Do not:

- model permanent `Architect Agent`, `Test Agent`, `Security Agent`, etc. as kernel primitives;
- build rigid multi-agent workflows when obligations can describe required outcomes;
- use an opaque aggregate risk score as an authorization mechanism;
- collapse unknown engineering facts into `false`;
- allow an implementing producer to be the sole source of acceptance evidence for a material claim;
- automatically expand privilege during contract recompilation;
- allow execution-learning logic to activate governance-policy changes;
- couple core domain types to a particular CI, Git host, LLM provider, or sandbox provider.

Prefer:

- typed facts with provenance and uncertainty;
- deterministic policy resolution after facts are established;
- obligations and invariants rather than prescribed agent sequences;
- exact revision/subject binding for evidence;
- explicit capability, permission, and authority boundaries;
- contract reassessment when observed scope materially differs from predicted scope;
- boring infrastructure until the engineering semantics are proven.

## Working style

For non-trivial changes:

1. inspect current repository state;
2. identify which specification or ADR governs the change;
3. state any architectural contradiction you discover rather than silently changing the thesis;
4. keep changes scoped to one coherent issue;
5. add or update tests for behavioral changes;
6. update documentation when a public contract or architectural invariant changes;
7. prefer small, reviewable commits;
8. report assumptions, unresolved uncertainty, and evidence produced.

## Initial milestone

Do not build a broad autonomous engineering platform yet.

The first milestone is to prove the authorization kernel using representative scenarios:

- trivial change;
- normal behavioral change;
- security-sensitive change;
- hidden scope expansion;
- material uncertainty/misclassification;
- stale evidence;
- privilege escalation request;
- failing evidence and remediation.

Primary metric: **accepted engineering changes per human supervision hour**.

Secondary metrics include authority precision/recall, first-pass acceptance, rework, escaped defects, false policy blocks, cost per accepted change, evidence completeness, and contract recompilation frequency.
