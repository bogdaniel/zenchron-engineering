# Roadmap

This file exists because the repository could explain the Authorization Kernel
well and could not explain where the product was going. A coding agent reading
`docs/architecture.md` alone would reasonably conclude that a governed
single-task runtime is the finished category. It is not.

The trajectory is three layers, and each one is only buildable on the one below.

```text
        M0 - M1        Engineering Authorization Kernel
                       facts -> policy -> obligations -> evidence -> authority
                       one task, governed end to end
                              |
                              v
        M1-R (#63)     Persistent multi-agent Engineering Runtime
                       serve + named execution agents + concurrent runs
                       + GitHub feedback loop + operator control room
                              |
                              v
        M2 (#64)       Engineering Planner
                       roles + capabilities + decomposition
                       + dynamic agent selection
```

## Where the project is now

**M1-R (#63) is what this repository currently implements.** The runtime is a
persistent local control plane. An operator names the coding agents they have
already installed and authenticated, gives several of them work at once, watches
all of it from one place, reviews the results in GitHub, and leaves review
comments that reach the right worker without anything being copied between
tools.

What that milestone is careful NOT to be is an agent framework. Everything the
kernel established still holds: a coding agent authors changes and authorizes
none of them, every candidate mutation goes through a runtime-owned commit,
reassessment and exact-tree assurance, and publication is a #7 authority
decision rather than a provider's opinion of its own work.

See [`docs/product-architecture.md`](docs/product-architecture.md) for the shape
of the running system, and [`docs/supervisor.md`](docs/supervisor.md) for what
`serve` owns.

## The boundary between #63 and #64

This is the distinction most likely to be collapsed by someone reading either
issue alone, so it is stated here in the durable documentation rather than only
in an issue comment.

```text
Execution Agent                      Engineering Role
(#63, implemented)                   (#64, not implemented)

codex, claude, gemini, qwen,         product_architect, system_architect,
openai-protected, future adapters    planner, implementer, tester,
                                     security_reviewer, reviewer,
                                     integrator, release_reviewer

WHO does the work                    WHAT responsibility must be fulfilled
a named, configured worker           a semantic role in an engineering plan
```

An agent is a worker an operator installed. A role is a responsibility a plan
requires. #63 builds the workforce and the supervisor that runs it; #64 builds
the planner that decides which workforce a piece of work needs.

**#63 deliberately does not implement**: engineering roles, `EngineeringPlan`,
task decomposition, capability-based agent selection, cross-agent review
policies, or any planner. Naming those as runtime personas now would freeze a
decomposition before anything has been learned about which decompositions
actually work — which is exactly what principle P1 exists to prevent.

**What #63 leaves for #64**: a stable agent id separated from provider kind and
trust mode; capability and readiness metadata a planner can reason over; narrow
optional interfaces rather than one universal agent interface; a supervisor that
schedules durable runs rather than conversations; and a kernel that has never
learned a provider's name.

## What #63 implemented

- A named agent registry: stable agent ids, provider kinds and explicit trust
  modes, with backward-compatible migration from the single-provider
  configuration.
- Native adapters for Codex CLI, Claude Code, Gemini CLI and an agentic Qwen
  CLI, all driven through one shared operator-trusted lifecycle, plus the
  existing protected OpenAI Responses provider.
- `serve`: a persistent supervisor owning the scheduler, with an owner-only
  local control endpoint, drain / shutdown / stop-all lifecycle semantics, and
  one shared forge observation stream per repository.
- Concurrent EngineeringRuns under an operator ceiling, each with its own
  candidate workspace, branch, agent binding, budgets and PR lifecycle.
- A GitHub feedback loop gated by actor admission, delivered exactly once, with
  self-loop prevention by identity.
- An operator control room: `agents`, fleet `status`, `logs`, and trust-aware
  `doctor`.
- Typed provider capacity waits and an operator state-storage ceiling.

## What #63 deliberately refused

- **In-run provider handoff.** Changing a live run's agent is a typed, journalled
  refusal that records everything a successor would have needed, and points at a
  new generation instead. See [`docs/agents.md`](docs/agents.md).
- **Automatic issue discovery by default.** An issue exists because a project
  records work in issues. Its existence is not consent to spend a subscription
  on it.
- **A raw local-model coding harness.** Local Qwen support drives an
  already-agentic CLI. A completion endpoint cannot edit a file, so supporting
  one would mean building a second coding agent inside this repository.
- **AI-based agent routing.** Which worker suits which ticket is an operator's
  decision; a system that made it silently would be spending their subscriptions
  on its own opinion.

## Measuring it

The point of the milestone is leverage, not adapter count. The metric remains
the one from #11: **accepted engineering changes per human supervision hour**.
The immediate target for #63 is roughly 2x on ordinary work without worsening
defects, rework or authority safety. Higher-order leverage through role planning
and decomposition belongs to #64.

That is a measured target. It is deliberately not encoded as an assertion
anywhere in the test suite: a number the system can check is a number the system
will optimize, and this one has to be earned against real work.
