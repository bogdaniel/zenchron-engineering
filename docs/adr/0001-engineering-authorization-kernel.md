# ADR-0001: Engineering Authorization Kernel

- Status: Accepted
- Date: 2026-08-27

## Context

Modern coding agents increasingly provide planning, implementation, tool use, subagents, tests, and autonomous execution. Building Zenchron primarily as another rigid multi-agent orchestrator would tie the product to an implementation layer likely to commoditize and would constrain increasingly capable execution engines.

The durable problem is institutional: determine what engineering work may affect, which obligations follow from policy, what evidence is sufficient, and whether a protected next action may occur.

## Decision

Zenchron Engineering OS will be centered on an **Engineering Authorization Kernel**.

The kernel's core transformation is:

```text
engineering facts
  + engineering policy
  -> obligations, invariants, and permissions
  -> evidence satisfaction
  -> action-scoped authority decision
```

Execution agents are replaceable providers beneath this architecture.

The repository will use **Go** for the core implementation and **JSON** as the canonical persisted/interchanged representation. External domain contracts will be described using JSON Schema. YAML is not canonical and may only be added later as an optional authoring format that normalizes to JSON.

## Consequences

### Positive

- provider independence;
- deterministic policy resolution can be separated from probabilistic reasoning;
- engineering semantics remain useful as models improve;
- assurance and authority become auditable;
- scope expansion can revise governance without forcing rigid workflows;
- Sentinel Shield and Zenchron Foundry can provide stronger native integrations without becoming mandatory dependencies.

### Costs

- impact/fact classification becomes a critical trust surface;
- uncertainty and fact provenance must be modeled explicitly;
- evidence lifecycle and staleness must be correct;
- policies and contracts require careful versioning;
- the system must resist becoming an overly complex schema platform before validating real engineering cases.

## Invariants

- Execution does not imply authority.
- A material change producer cannot be its sole acceptance authority.
- Unknown material facts never silently become false.
- Obligations may automatically increase; sensitive privilege may not automatically increase.
- Governance policy cannot be silently rewritten by execution-optimization logic.

## Rejected direction

A permanent pipeline of named AI roles such as Product Architect -> System Architect -> Implementation Agent -> Test Agent -> Security Agent -> Review Agent -> Release Agent is rejected as the kernel abstraction.

Such roles may be selected dynamically by an execution planner when useful, but the contract describes outcomes and evidence rather than a fixed simulated organization.
