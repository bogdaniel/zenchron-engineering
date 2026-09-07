# Zenchron Engineering OS

A local engineering control plane that coordinates the AI coding agents you have
already installed — Codex CLI, Claude Code, Gemini CLI, an agentic Qwen CLI —
across several project tasks at once, using GitHub issues as context and pull
requests as the review surface, while keeping verification, provenance, policy
and authority in the runtime rather than in the agent.

The core rule has not changed: **agents may reason and execute, but they may not
self-authorize material outcomes.**

## The operator workflow

```bash
# 1. Build it.
go build -o bin/zenchron-engineering ./cmd/zenchron-engineering

# 2. See which workers you can actually give work to. This spends nothing.
zenchron-engineering autonomy agents --text

# 3. Start the persistent runtime. It owns the work from here.
zenchron-engineering serve

# 4. In another terminal: give it several tickets, one agent each.
zenchron-engineering autonomy run issue 123 --agent codex
zenchron-engineering autonomy run issue 124 --agent claude

# 5. Watch all of it from one place.
zenchron-engineering autonomy status --text
zenchron-engineering autonomy logs RUN --follow

# 6. Review the pull requests in GitHub, and leave comments there.
#    Admitted feedback reaches the right worker; you copy nothing.

# 7. Merge when satisfied. That decision stays yours.
```

Start at [`docs/getting-started.md`](docs/getting-started.md).

```text
    you                          zenchron-engineering serve                GitHub
     |                                      |                                |
     |-- run issue 123 --agent codex ------>|                                |
     |-- run issue 124 --agent claude ----->|                                |
     |                                      |-- clone, invoke agent          |
     |                                      |   commit, reassess, assure     |
     |                                      |   authorize, push, open PR --->|
     |<-- status / logs --------------------|                                |
     |                                                                       |
     |-- review comment ---------------------------------------------------->|
     |                                      |<-- admitted feedback ----------|
     |                                      |-- worker corrects, PR updates ->|
     |-- merge ------------------------------------------------------------->|
```

## What it does for you, and what it refuses to do for you

**It removes the message bus.** You do not paste a prompt into a coding CLI,
paste its output back, copy failing test output somewhere else, create the
commit, push the branch, or shuttle review comments into a terminal. The runtime
owns all of that.

**It does not remove you from the decision.** Merge is external. No
configuration in this repository authorizes the runtime to adopt its own
candidate, and a coding agent's own report is never the evidence that accepts
its own change.

**It uses the subscriptions you already have.** Selecting `claude` runs the
Claude Code CLI you authenticated, not an Anthropic API adapter substituted for
it. The child environment is built from scratch, so an API key in your shell
cannot silently turn a subscription session into metered API billing. Zenchron
requires no OpenAI credential for normal local development.

**It tells the truth about what it can prove.** A CLI running under your account
can read what you can read. That is what `operator_trusted` names, it is never
relabelled as proven isolation, and work whose policy requires protected
execution is refused to it.

## Documentation

| | |
| --- | --- |
| [`docs/getting-started.md`](docs/getting-started.md) | install, configure, first run |
| [`docs/running-work.md`](docs/running-work.md) | one issue end to end |
| [`docs/running-multiple-tasks.md`](docs/running-multiple-tasks.md) | several at once, and what happens when one merges first |
| [`docs/github-feedback.md`](docs/github-feedback.md) | the review loop, and who is allowed to direct a worker |
| [`docs/agents.md`](docs/agents.md) | the workers, trust modes, provenance |
| [`docs/supervisor.md`](docs/supervisor.md) | `serve`, its control endpoint, drain/shutdown/stop-all |
| [`docs/configuration.md`](docs/configuration.md) | every configuration member and which layer owns it |
| [`docs/troubleshooting.md`](docs/troubleshooting.md) | symptom, cause, fix |
| [`ROADMAP.md`](ROADMAP.md) | where this sits, and what comes next |
| [`docs/product-architecture.md`](docs/product-architecture.md) | the running system |
| [`docs/architecture.md`](docs/architecture.md) | the Authorization Kernel |
| [`docs/principles.md`](docs/principles.md) | architectural principles P1-P12 |
| [`docs/construction-principles.md`](docs/construction-principles.md) | how the code implementing them is built |

## Thesis

Zenchron Engineering is **not** a generic multi-agent orchestrator. Coding agents
are replaceable execution providers. The durable system governs engineering
change:

```text
engineering intent
  -> engineering facts
  -> policy
  -> obligations and invariants
  -> bounded execution
  -> evidence
  -> authority decision
```

## Status

The Authorization Kernel and the local Engineering Runtime are implemented. The
current milestone makes that runtime a persistent, multi-agent, operator-usable
control plane; the Engineering Planner above it is a later layer and is not
built. See [`ROADMAP.md`](ROADMAP.md).

## Technology decisions

- Core implementation language: **Go**
- Canonical machine-readable representation: **JSON**
- Contract/schema format: **JSON Schema**
- Agent providers: replaceable adapters. Codex CLI, Claude Code, Gemini CLI and an agentic Qwen CLI run as operator-trusted local workers; the brokered OpenAI Responses provider remains the protected one
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

## Local runtime

The local runtime is a durable reconciler above the kernel. It persists
append-only run events and bounded operations in the operator state directory,
folds them deterministically into a run snapshot, and drives candidate work in
runtime-owned clones outside the target repository. See
[`docs/spec/runtime-v0.1.md`](docs/spec/runtime-v0.1.md).

Two connectivity questions are separate and are never collapsed:

```text
provider inference connectivity/auth  !=  candidate command connectivity/credentials
```

A provider control plane may authenticate explicitly to its configured AI
provider and hold the connectivity remote inference inherently requires. It
receives no GitHub, SSH, signing, cloud, or application credentials, and its
output is an observation carrying no acceptance authority. Candidate and tool
execution is confined to a bounded candidate workspace with no ambient host
environment, none of those credentials, network denied by default, and no
reach into the runtime database, the controller checkout, or another run.
Assurance is stricter still: an exact detached candidate tree, no AI-provider
credentials, separately constrained dependency preparation, and offline
`gofmt`, `go vet`, and `go test`.

The Codex adapter drives the installed Codex CLI natively; a Codex control
process with no network and no authentication cannot perform remote inference.
It does not, however, satisfy the candidate and tool boundary above. Local
Codex `--sandbox workspace-write` with
`sandbox_workspace_write.network_access=false` primarily bounds writes and
does not establish bounded reads, so provider tool execution under that mode
is not proven unable to read the runtime database and runtime state,
runtime locks, the controller checkout, another run's state, provider
credentials, or unrelated home data. Filesystem read isolation for the native
Codex adapter is unproven, and that confidentiality requirement is not
weakened to make the adapter appear compliant. The adapter is retained
explicitly as a bootstrap, operator-trusted adapter, and it is ineligible for
protected autonomous execution. Eligibility fails closed: a provider that does
not prove the required isolation properties, or does not report them at all,
is refused rather than assumed compliant.

What the adapter does prove is stated without overstatement: fail-closed
capability probing that never silently uses full-access or unsandboxed Codex,
runtime-owned policy rather than user Codex configuration defining the
effective sandbox constraints, a candidate command environment allowlisted
from scratch, provider credentials held only in the control plane, and a
candidate `AGENTS.md` that is left unmodified and cannot become trusted
instructions.

The protected provider separates reasoning from tool execution:

```text
reasoning provider
    -> tool request
    -> Zenchron-controlled ToolBroker
    -> candidate operations
```

Zenchron validates and executes each requested capability; the reasoning
provider never touches the filesystem directly. The initial capabilities are
repository read, repository search, candidate diff, apply patch, and run
bounded command. Candidate command execution continues through the existing
sandbox and receives the candidate workspace only: no runtime state, no
controller checkout, no other run's state, no provider credentials, and
network disabled by default. Provider credentials remain solely in the
control-plane process, and `ExecutionProvider` stays provider-independent.

That brokered adapter exists. The OpenAI Responses provider runs its
reasoning loop in the trusted controller process; the model emits only tool
names and JSON arguments, and there is no hosted shell, no code interpreter,
no file-upload channel, and no path handle crossing the boundary. Its API key
is an operator-supplied file reference, read inside the call and used only as
a control-plane request header, and a key file resolving inside the candidate
workspace is refused, so repository content can neither supply nor redirect
the credential. It is the provider that reports all four required isolation
properties as proven and is therefore the protected-eligible one. Eligibility
still follows from what a provider reports rather than from which adapter it
is, and it still fails closed. Docker remains a valid isolation primitive for
assurance and for providers whose architecture supports it; providers are not
required to be Docker-shaped.

Operations are persisted in SQLite, in `runtime.db` under the operator state
directory, with an explicit schema version, ordered migrations, refusal to
open a newer schema than the binary supports, durable run-scoped idempotency
keys, transactional lease acquisition and heartbeat, compare-and-swap row
revisions, and deterministic queue ordering that survives restart. Lease
takeover requires owner liveness; wall-clock expiry alone never authorizes it.

The same database holds the persistent run identity and the append-only event
journal under that same schema-version and ordered-migration mechanism, with
monotonic per-run sequence allocation, transactional append, refusal of
duplicate or invalid sequences, a persisted and verified hash chain, and
deterministic replay that rebuilds the run snapshot after reopen. Canonical
event rows hold references only; raw transcripts never live in them. Event
identity is durable and caller-owned while sequence, chain links, and state
digests are allocated inside the append transaction, so a watch controller
holding a run's lease and an operator command holding none can write the same
journal concurrently without colliding and without a gap. Owner liveness is
the OS advisory ownership lock, held for a runtime instance's whole lifetime
and released by the kernel on death; a lock file is never liveness evidence,
and a platform that cannot decide reports the owner alive so takeover is
blocked rather than guessed.

The trusted candidate Git metadata baseline is persisted the same way. Every
successful runtime-owned Git operation journals the digest it established,
and integrity is asserted against that journalled baseline rather than
against the live repository, so a restart cannot launder metadata that was
tampered with while no runtime was running.

Configuration is two layers with different authority. The operator layer,
outside every repository the runtime works on, is the only place that may
authorize a credential, name the provider, name the assurance image, choose
the state directory, enrol repositories for watch, choose the opt-in label,
set the reclamation window, or name the operator identity a run is recorded
against. The in-repo layer may only tighten: it can lower a bound and never
raise one, and it can raise no capability, concurrency ceiling, budget,
credential authority, or publication authority. Its permitted members are an
allowlist enforced before decoding, and a proposal that would raise a bound
is refused rather than clamped.

## Operator surface

`autonomy authorize` records evidence, not authority. It appends one typed
human-authority event bound to exact state, and the kernel's action-scoped
authority evaluator decides what that evidence means. Recording an approval
and still reading `blocked`, `incomplete`, or `stale` is an ordinary outcome.
Human approval cannot expand contract permissions, grant a privilege
expansion, bypass policy, make stale evidence current, or replace independent
assurance: it satisfies only outstanding claims whose evidence class is human
approval. It cannot be given past a controller mismatch, a moved source, an
externally changed candidate head, or a terminal run, because those gate
recording as well as reading. A rejection is recorded as failed evidence
rather than as an absence, so it blocks rather than merely remaining
unsatisfied.

The request an operator answers is a projection of current runtime state plus
the exact missing human-authority requirement, regenerated from the journal
on every read and never persisted or consulted as a permission. Its
identifier digests the exact bindings - run, action, repository, controller,
pinned source, base, candidate revision and tree, contract revision, and the
current decision and evidence context - so an operator names exact state
rather than typing SHAs. Any material change produces a different identifier;
the old approval is refused as stale rather than retargeted, and recorded
authority is admitted as evidence only for the contract and candidate
revisions it was given against, which are never rebound.

A candidate Zenchron cannot authorize its own adoption. The
human-authorizable actions are an allowlist - open a pull request, update
one, push the run-owned candidate branch - and adoption is not on it, so a
merge or adoption action is refused as unsupported rather than given a shape
that looks governed. There is no `autonomy merge` and no automatic
pull-request merge; a merged pull request is only an observation. Final
adoption of the runtime's own candidate remains external to the candidate
runtime.

Operator identity is recorded provenance, not cryptographically verified
person identity. A record names the configured identity, the local account,
and the host, and carries an explicit provenance value stating that none of
it was authenticated. Resolution fails closed, and the identity is an
operator-layer member the repository layer may not name, so a repository
cannot choose who is recorded as authorizing a change to it.

An edit to the source issue stops the run rather than silently changing what
it is working toward. `autonomy refresh RUN` is the explicit path that
re-reads it, and it grants nothing: no permission, no privilege expansion, no
publication. It is a separate command rather than a flag on `resume`, which
is what makes "a plain resume never absorbs changed source intent"
structural. It records refresh intent under a reason distinct from an
operator stop and draws a generation boundary, which is what stales the prior
contract, evidence, and authority; the prior journal is preserved exactly.

Signalling `autonomy watch` stops the watch controller and leaves every run
resumable: discovery and scheduling stop, but no cancellation is journalled
and the journal is left exactly as the last completed step wrote it.
`autonomy stop RUN` is the only thing that cancels a run. It is explicit
operator intent, it is durable across a restart, and it reuses the
scheduler's existing cancellation request rather than introducing a second
mechanism. `autonomy resume RUN` clears no wait; it asks the runtime to
reconcile again, and it refuses a withdrawn opt-in, a recorded
workspace-integrity violation, and a cancelled run rather than walking over
them.

`autonomy status` and `autonomy events` are reads over persisted state and
make no network call; `events` takes neither ownership of the state directory
nor a run-driving lease, so it can tail a run another controller is driving,
and it prints artifact references rather than artifact content. `autonomy
doctor` answers each capability question independently: PASS states what was
proven, and a check that cannot be answered is WARN or FAIL, never a silent
PASS. It makes no provider inference call and no forge write, resolves
credentials without placing one in the report, and neither repairs nor holds
what it measures. `autonomy gc` reclaims only heavyweight local material that
a canonical row already explains by reference; a non-terminal run's material,
a leased run's material, anything inside the retention window, the runtime
database and its canonical rows, and anything whose ownership cannot be
proven are never eligible.

`zenchron-engineering serve` is the persistent supervisor. It owns the
scheduler, drives every active run, observes each repository once on behalf of
all of them, and answers an operator while the work continues. Its control
endpoint is an operator-authority boundary rather than a convenience socket:
anything able to submit an accepted request to it can start operator-trusted
coding agents under the local account, so it is a Unix-domain socket inside the
owner-only state directory, owner-only itself, checked before creation and
re-checked by every client, with no TCP listener and no stored credential beside
it. Drain, shutdown and stop-all are three different operations, and only the
last one cancels runs.

`autonomy agents` reports each configured worker: found, capabilities
advertised, version, and the authentication state actually observed. It spends
nothing, and an unobserved authentication mode is reported as `unknown` rather
than inferred to be a subscription. Readiness requires the installed CLI to
advertise the exact flags the runtime passes, so a CLI that renamed one is
reported unavailable instead of being run in whatever mode it defaults to.

`autonomy status` with no run named is the all-runs view; naming a run keeps the
detailed single-run projection unchanged. `autonomy logs RUN` shows the
sanitized provider transcripts with attempt boundaries and the agent that
produced each, so an operator never has to locate an artifact path. The raw
transcripts beside them stay local-only forensic material and are never
rendered.

GitHub review feedback reaches the worker through one admission gate. Reviews,
inline comments, pull-request conversation and post-creation issue comments are
all admitted on the ACTOR's current repository permission — defaulting to
collaborator-equivalent write — and never on what the comment says. The
runtime's own comments are refused by identity, automation accounts are refused
unless allowlisted, an unresolvable actor is never admitted, and an admitted
item is delivered exactly once, bound to the invocation that received it, framed
as untrusted data. A review of a superseded commit stays history, and a frozen
generation never wakes up because someone commented on its old pull request.

Changing a live run's execution agent is a governed transition that this
milestone always refuses, recording a complete typed transition record —
candidate, both agents, both trust modes, reason, contract, cumulative budgets
with unknown dimensions left unknown, consumed feedback — and pointing at a new
generation instead. Budgets never reset across it, and a run created under
protected execution is never continued by an operator-trusted worker.

## Repository map

```text
AGENTS.md                       Persistent instructions for coding agents
ROADMAP.md                      Where the project is and what comes next
docs/getting-started.md         Install, configure, first run
docs/running-work.md            One issue end to end
docs/running-multiple-tasks.md  Several at once, and base drift between them
docs/github-feedback.md         The review loop and its admission gate
docs/agents.md                  Execution agents, trust modes, provenance
docs/supervisor.md              serve, its control endpoint and lifecycle
docs/configuration.md           Every configuration member and its layer
docs/troubleshooting.md         Symptom, cause, fix
docs/product-architecture.md    The running system
docs/vision.md                  Product thesis and boundaries
docs/principles.md              Frozen architectural principles P1-P12
docs/construction-principles.md How the implementing code is built
docs/architecture.md            Authorization Kernel architecture
docs/adr/                       Architecture decisions
docs/spec/v0.1.md               Initial domain specification
docs/spec/runtime-v0.1.md       Local runtime specification
schemas/                        Canonical JSON Schemas and Go validation tests
domain/                         Go v0.1 representations and canonical JSON codecs
reassessment/                   Observed-scope validation and contract reassessment
runtime/                        Local runtime, adapters, supervisor, scheduler, state
fixtures/v0.1/                  Positive and targeted invalid schema fixtures
cmd/zenchron-engineering/       CLI entry point and composition root
```

## Non-goals

Do not prematurely build a dashboard, Kubernetes orchestration, a visual workflow designer, a generic multi-agent framework, a vector database, a custom CI system, a marketplace, or enterprise RBAC.

Nor, at this milestone: an AI agent router, a raw-LLM coding harness, a hosted
control plane, remote worker federation, autonomous merge, or the engineering
roles and planning layer that [`ROADMAP.md`](ROADMAP.md) assigns to the next one.

The kernel milestone had to prove that the same facts and policies produce
appropriately different obligations and authority decisions across low-, medium-
and high-impact work. That property still governs everything added since.

## Development

```bash
go test ./...
go run ./cmd/zenchron-engineering version

# The persistent runtime and the operator surfaces over it.
go run ./cmd/zenchron-engineering serve
go run ./cmd/zenchron-engineering autonomy agents --text
go run ./cmd/zenchron-engineering autonomy run issue 123 --agent codex
go run ./cmd/zenchron-engineering autonomy run issues 123 124 --assign 123=codex --assign 124=claude
go run ./cmd/zenchron-engineering autonomy status --text
go run ./cmd/zenchron-engineering autonomy logs <run> --follow
go run ./cmd/zenchron-engineering autonomy agent set <run> --agent claude --reason "..."
go run ./cmd/zenchron-engineering autonomy drain
go run ./cmd/zenchron-engineering autonomy shutdown
go run ./cmd/zenchron-engineering autonomy stop-all --reason "end of day"

go run ./cmd/zenchron-engineering selfhost issue 4
go run ./cmd/zenchron-engineering selfhost issue 4 --model gpt-5.6-terra --fallback-model gpt-5.6-luna
go run ./cmd/zenchron-engineering selfhost resume issue 4
go run ./cmd/zenchron-engineering autonomy doctor --text
go run ./cmd/zenchron-engineering autonomy run issue 4
go run ./cmd/zenchron-engineering autonomy status <run> --text
go run ./cmd/zenchron-engineering autonomy authorize <run> <request-id> --approve
go run ./cmd/zenchron-engineering autonomy refresh <run>
go run ./cmd/zenchron-engineering autonomy stop <run>
go run ./cmd/zenchron-engineering autonomy watch
go run ./cmd/zenchron-engineering autonomy gc --dry-run
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
and the repository-derived `golang:<line>` image already exists locally, where
`<line>` is the compatibility line (major.minor) of the `go.mod` go directive
rather than its literal text. Rewriting `go 1.25` to `go 1.25.0` is therefore
a formatting change and does not change the operator precondition. An exact
toolchain requirement, should one ever be introduced, is represented
separately from the compatibility line. Bootstrap never pulls that image
implicitly; at execution the resolved image is pinned to its immutable image
identity, and that exact identity is what provenance records. Docker Go
commands use an unprivileged container with only the repository mounted. The
container has outbound network access so Go can resolve the non-vendored
modules pinned by `go.mod` and `go.sum`; no host credentials or environment
are forwarded. Go's home, module, and build caches live in an isolated
writable tmpfs that is discarded after each command.

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
