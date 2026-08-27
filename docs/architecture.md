# Architecture

## System boundary

Zenchron Engineering OS is organized around an Engineering Authorization Kernel. Agent orchestration is an execution concern beneath that kernel.

```text
                    Engineering Intent
                           |
                           v
                     Project Model
                           |
                  Impact / Fact Analysis
                           |
                           v
                   Engineering Facts
                           +
                    Engineering Policy
                           |
                           v
                    Contract Compiler
                           |
                           v
               Engineering Work Contract
                   /        |        \
                  /         |         \
            Context     Execution    Obligations
            Compiler     Planner     / Invariants
                  \         |         /
                   \        |        /
                    Execution Runtime
                           |
              Codex / Claude / tools / humans
                           |
                           v
                    Candidate Change
                           |
                           v
                    Observed ChangeSet
                           |
              contract still valid?
                 /                 \
               yes                 no
                |                   |
                |              recompute facts
                |              revise contract
                |                   |
                +---------+---------+
                          |
                          v
                  Assurance Providers
                          |
                          v
                     Evidence Bundle
                          |
                          v
                    Authority Evaluator
                          |
                   action-scoped decision
```

## Kernel transformation

The durable transformation is:

```text
facts + policy
    -> obligations + invariants + permissions
    -> evidence satisfaction
    -> action-scoped authority
```

The kernel should remain free from provider-specific orchestration assumptions.

## Governance envelope

An execution receives a governance envelope containing at least:

- objective and acceptance intent;
- predicted/known impact;
- allowed and prohibited scope;
- invariants;
- obligations;
- actor permissions;
- prohibited privileges;
- evidence requirements;
- budget constraints when applicable;
- authority conditions.

Agents should have freedom to reason within this envelope. The platform governs boundaries and acceptance rather than micromanaging every reasoning step.

## Fact lifecycle

Facts may exist at different stages:

- `predicted`: inferred before execution;
- `observed`: derived while examining the actual change;
- `verified`: independently established where verification is possible.

Fact provenance must record how the system came to believe the fact.

## Scope reassessment

A predicted scope is not a permanent prison. Agents can discover legitimate additional work.

When observed changes materially exceed the existing contract:

1. suspend affected privileged actions;
2. recompute relevant facts;
3. resolve policies again;
4. revise the work contract;
5. invalidate or mark stale evidence that no longer applies;
6. continue under the revised governance envelope.

Obligation escalation can be automatic. Privilege escalation requires an appropriate external authority source.

## Evidence model

A claim and its evidence are conceptually distinct.

Evidence must identify at least:

- claim/requirement addressed;
- subject and exact revision;
- producer identity/type;
- environment/toolchain where relevant;
- policy/contract revision;
- result;
- integrity/provenance information;
- lifecycle state such as valid or stale.

Changing the evidence subject defaults existing evidence to stale unless explicit dependency analysis proves continued applicability.

## Authority

Authority is action-scoped, not a global pass/fail bit.

Examples:

- PR creation may be authorized;
- merge may be awaiting human authority;
- production deployment may be outside the contract's permissions.

Initial decision states should distinguish at least:

- `incomplete` — required evidence has not arrived;
- `blocked` — valid evidence establishes a violation;
- `stale` — required evidence exists but no longer applies;
- `awaiting_authority` — technical obligations are satisfied but an external approval is missing;
- `authorized` — the requested protected action may proceed.

## Provider boundaries

### AgentProvider

Codex, Claude, Gemini, local agents, and future systems belong behind provider adapters.

### AssuranceProvider

Tests, CI, static analyzers, Sentinel Shield, enterprise systems, and human review can produce or verify evidence. Sentinel Shield is the preferred Zenchron-native implementation, not a mandatory dependency.

### ExecutionEnvironmentProvider

Local worktrees, Docker, CI runners, customer infrastructure, and Zenchron Foundry can provide execution environments. Foundry is the preferred native provenance-aware implementation.

## Early implementation rule

Keep conceptual separation without prematurely splitting every concept into its own service or package. The MVP is a Go CLI/runtime proving engineering semantics, not a distributed control plane.
