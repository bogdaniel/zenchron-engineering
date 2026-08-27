# Vision

## Product thesis

Zenchron Engineering OS turns engineering intent into **authorized software changes**.

The system exists because autonomous coding is not primarily an execution problem. Models can increasingly plan, code, test, and use tools. The harder institutional problem is deciding:

- what a requested change can affect;
- which engineering obligations follow;
- what the executing actor is permitted to do;
- what evidence is sufficient for material claims;
- whether a privileged next action may occur.

Zenchron therefore governs **engineering change**, not merely agents.

## Durable abstraction

Coding agents are interchangeable execution engines beneath the platform:

```text
Intent
  -> Project state
  -> Engineering facts
  -> Policy resolution
  -> Work contract
  -> Execution
  -> Observed change
  -> Assurance/evidence
  -> Authority decision
  -> PR / merge / release / deploy
```

The implementation may use Codex, Claude, Gemini, local models, deterministic tools, humans, or future systems. No provider is part of the kernel's identity.

## Why this is not an agent orchestrator

Agent spawning, MCP access, sandboxes, budgets, provider routing, and workflow scheduling are useful infrastructure but are expected to commoditize.

Zenchron's differentiated domain is the mapping:

```text
engineering situation
  -> impact facts
  -> applicable policy
  -> required obligations and invariants
  -> sufficient evidence
  -> bounded authority
```

## Long-term direction

The same authorization kernel should eventually govern more than feature implementation:

- dependency and vulnerability remediation;
- production incidents;
- infrastructure and architecture drift;
- release and deployment decisions;
- policy violations;
- maintenance proposed by autonomous systems.

The long-term category is policy-governed autonomous software operations, with engineering execution as the first proving ground.

## Product layers

### Engineering Authorization Kernel

Facts, policy, contracts, invariants, evidence, and authority.

### Engineering Runtime

Context compilation, execution planning, provider adapters, workspaces, observation, and remediation loops.

### Engineering Control Plane

A later organization-level product spanning repositories, policies, providers, budgets, identities, assurance systems, and engineering intelligence.

## Native Zenchron ecosystem

- **Sentinel Shield**: preferred high-fidelity evidence and assurance backend.
- **Zenchron Foundry**: preferred trusted execution/material/provenance backend.

Both should integrate through contracts so the platform can interoperate with existing enterprise systems.

## MVP thesis

The first product proof is not "an agent opened a PR."

It is:

> Given the same project model and policy set, Zenchron can derive appropriately different obligations and authority decisions for different engineering impacts, execute through interchangeable providers, detect material scope expansion, and prevent any execution producer from granting itself material authority.
