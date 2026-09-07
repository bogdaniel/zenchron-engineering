# Software construction principles

[`principles.md`](principles.md) states P1–P12: what the system must be true
*about*. This document states how the code that implements them is expected to
be built. The two are different kinds of rule and are deliberately kept apart —
but they are not independent, and each principle below says which governance
principle it serves.

These are requirements. A change can be functionally green and still fail review
for materially violating them.

## DRY — for knowledge, not for text

DRY applies to **knowledge, invariants and behaviour**, never to superficial
similarity.

Required:

- Shared provider lifecycle behaviour — attempt identity, cancellation, artifact
  handling, provenance, trust-mode reporting, feedback framing — has one
  canonical implementation. `runtime/cli_agent.go` is that statement for native
  CLIs; `runtime/agent_specs.go` holds only what genuinely differs.
- An adapter never reinvents scheduler, Git, artifact, retry, redaction,
  feedback or authority logic.
- Provider-specific argv and protocol construction stays provider-specific.
  Forcing four CLIs with different flag grammars through one "universal"
  argument abstraction would be premature DRY.
- A duplicated **security or policy invariant** is a review finding wherever
  divergence could change semantics. There is one cancellation path, one
  admission gate, one publication authority.

Avoid premature DRY. Two adapters containing similar-looking code is not a
reason to invent an abstraction; extract only when the shared concept is stable
and genuinely the same responsibility.

*Serves P2 and P12: an invariant stated twice is an invariant that will
eventually be enforced once.*

## SOLID, adapted for Go

### S — Single responsibility

A package or component has one coherent reason to change:

```text
agent registry        resolves and validates configured workers
adapters              translate execution requests into provider invocations
scheduler             scheduling and leases
feedback admission    actor and routing decisions
Git subsystem         candidate, history and publication state
assurance             verification evidence
authority kernel      independent of every provider mechanic
supervisor            ownership, concurrency, lifecycle
```

Do not create a `Manager` or `Service` that owns registry + process execution +
GitHub + Git + authority + UI. The composition root wires; it does not decide.

*Serves P11: execution concerns and governance concerns must be able to change
independently.*

### O — Open/closed

Adding a provider means a new adapter plus registration and configuration. It
does not mean edits spread through scheduler, kernel or authority packages.
Provider differences are expressed as capability metadata and small interfaces,
never as `switch provider == ...` scattered through the runtime.

*Serves P1: execution planning is replaceable, so replacing it must be cheap.*

### L — Liskov substitution

Anything admitted as an execution provider honours the same semantic contract:
exact request binding, observation-only results, cancellation semantics,
artifact identity, no self-authorization, truthful provenance and trust
reporting.

A provider that cannot honour a required property **refuses or declines that
capability explicitly** rather than lying through the interface. A native CLI
reports host read confinement as unproven; it does not report `proven` because
the interface has a field for it.

*Serves P4 and P12: a claim carries its provenance, and an unproven claim stays
unproven.*

### I — Interface segregation

Prefer small capability interfaces over one universal agent interface. Session
resume, version discovery, auth introspection, permission resolution,
conversation reading and usage reporting are optional capabilities a provider
may implement, not methods every provider must fake.

Consumers depend only on what they use. Feedback admission needs an actor's
permission and nothing else, so it asks for exactly that interface — and a forge
that cannot answer is a legal configuration in which feedback does not reach
workers, reported as such rather than as a failure.

*Serves P5: a capability nobody has is `unknown`, not `false`.*

### D — Dependency inversion

High-level runtime, scheduler and kernel behaviour depends on Zenchron-owned
interfaces and domain contracts, never on a vendor's SDK or CLI types. Provider
construction belongs at the composition root under `cmd/`.

The boundary is checkable: no package under `domain/`, `policy/`, `authority/`,
`evidence/` or `reassessment/` mentions any provider by name.

*Serves the core constraint in `AGENTS.md`: the authorization kernel does not
depend on any single model vendor.*

## Composition over inheritance

Go has no inheritance to avoid, so the rule is about how composition is used:

- Providers compose a shared process runner, artifact recorder, invocation
  provenance, cancellation handling and environment builder.
- The runtime composes scheduler, Git, forge, provider, assurance and authority
  services through narrow interfaces.
- Avoid embedding that implicitly exposes a large method set to simulate
  inheritance. Prefer explicit delegation where it preserves a responsibility
  boundary — `NativeCodexProvider` delegates rather than embeds, because it is a
  migration surface and not a base class.
- No provider inherits a monolithic `BaseAgent` carrying unrelated behaviour.

*Serves P1: a composition can be rearranged; a hierarchy has to be rebuilt.*

## YAGNI

Implement what the current acceptance contract requires or what a demonstrated
invariant inherits. Nothing else.

Out of scope until a failing acceptance case proves otherwise: an AI agent
router; a generic multi-agent conversation framework; a raw-LLM coding harness;
a hosted control plane; a plugin marketplace; remote worker federation; a
message bus; a new policy language; a provider-handoff framework beyond the
explicit semantics required; and speculative abstractions for providers nobody
is implementing.

When uncertain, take the smallest implementation that satisfies the current
contract and leaves a clean seam. A refusal that records everything a future
implementation would need is often that smallest implementation — see the
provider-handoff record in [`agents.md`](agents.md).

*Serves P1 and P11: an unbuilt abstraction cannot freeze a decomposition, and
cannot quietly acquire governance meaning.*

## Review checklist

Review of a substantive change explicitly answers:

- What logic is duplicated, and why is that the right call?
- Does each proposed abstraction have at least one real current consumer?
- Did provider-specific code leak into a domain or kernel layer?
- Is any interface broader than its consumers need?
- Has one component accumulated unrelated responsibilities?
- Could composition replace branching or copying?
- Does any code exist only for a hypothetical future requirement?
- Is any security or policy invariant now stated in two places?
