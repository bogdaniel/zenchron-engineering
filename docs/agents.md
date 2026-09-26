# Execution agents

An **agent** is a stable operator-chosen name for one execution worker. It is
three separate facts, and keeping them separate is the point:

```text
agent id        the name you type, and the name a run is bound to
provider kind   which adapter is built, and what it contacts
trust mode      what the runtime may honestly claim about the boundary
```

Collapsing any two is how a configuration change silently becomes a trust
change. Renaming a worker must not move it into another trust mode; a run that
recorded `codex` must keep meaning the worker you called `codex` even after the
executable, the model or the default moves on.

A future engineering **role** — planner, implementer, reviewer — is a fourth
concept and is deliberately absent. See [`../ROADMAP.md`](../ROADMAP.md).

## Supported kinds

| kind | worker | trust mode |
| --- | --- | --- |
| `codex_cli` | installed Codex CLI | `operator_trusted` |
| `claude_code` | installed Claude Code CLI | `operator_trusted` |
| `gemini_cli` | installed Gemini CLI | `operator_trusted` |
| `qwen_cli` | installed agentic Qwen coding CLI | `operator_trusted` |
| `openai_responses` | brokered OpenAI Responses provider | `protected` |

The trust mode is a property of the adapter, not of configuration. Stating the
wrong one is refused rather than silently corrected: a native CLI cannot be
moved into `protected` by writing the word, and the brokered provider cannot be
lowered out of it.

Local Qwen support drives an already-agentic CLI, not a raw completion endpoint.
A completion endpoint cannot edit a file, so supporting one would mean building
a second coding harness inside this repository — a different piece of work, and
not one this milestone needs.

## Configuring them

```json
{
  "agents": {
    "codex":  {"kind": "codex_cli",   "trust_mode": "operator_trusted"},
    "claude": {"kind": "claude_code", "trust_mode": "operator_trusted"},
    "gemini": {"kind": "gemini_cli",  "trust_mode": "operator_trusted"},
    "qwen":   {"kind": "qwen_cli",    "trust_mode": "operator_trusted"},
    "openai": {
      "kind": "openai_responses",
      "trust_mode": "protected",
      "model": "gpt-5",
      "credential_path": "/absolute/path/to/key"
    }
  },
  "default_agent": "codex"
}
```

Optional per-agent members:

- `command` — the executable. A bare program name is resolved on your `PATH`; an
  absolute path pins one exact program. A relative path with a separator is
  refused, because it would resolve against whatever directory the process
  happens to be in.
- `model` — the model to ask the provider for. Absent means the CLI's own
  configured default, which is a truthful answer for a tool you already set up.
- `home` — the directory holding that CLI's own authentication state. Absent
  means your own home, which is what "use the CLI you already authenticated"
  means.
- `unattended` — whether automatic discovery may start work on this agent.
  Default true; set false to keep an agent to explicit work only.
- `allow_permission_bypass` — standing permission for the provider's unsafe
  mode. See below.

`credential_path` and `endpoint` are valid only for `openai_responses`. Naming
one on a native CLI agent is refused rather than ignored: it would state a
Zenchron-held API credential for a worker Zenchron is deliberately not
authenticating.

`agents` and the pre-existing `provider` block are mutually exclusive. A
configuration written before the registry existed keeps working: its provider is
migrated into a one-agent registry under that provider's own historical id —
`native-codex` or `openai-responses` — in the trust mode it already had. Stating
both is refused rather than resolved by a precedence rule nobody wrote down.

`default_agent` is required once more than one agent is configured. Picking one
for you would be choosing which worker, and which account, does your work.

## Trust modes

### `operator_trusted`

Your installed, authenticated CLI, running under your account. What the runtime
does and does not claim:

**Proven.** The runtime injects no GitHub, SSH, signing or cloud publication
credential. The child environment is an allowlist built from scratch —
`os.Environ()` is never consulted — so nothing ambient reaches the worker or the
commands it spawns. The provider's least-privilege automation mode is selected
explicitly. The executable, its version, the effective permission and sandbox
mode and the security-relevant arguments are recorded in durable attempt
provenance.

**Not proven.** Filesystem *read* confinement. A CLI running as you can read
what you can read, whatever mode bounds its writes. That residual risk is what
this trust mode names, and it is never relabelled as proven.

What that refusal is, precisely, in this milestone: a `protected` agent must
prove its boundary before it may execute anything, an `operator_trusted` one
states that it cannot and is used anyway on the operator's authority, and a run
created under `protected` is never continued by an `operator_trusted` worker.

What it is **not** yet: policy cannot say "this work requires protected
execution". There is no such rule vocabulary, so nothing matches work to a trust
mode — choosing an `operator_trusted` worker is the operator's decision, made by
which agents they configure and which one they name. See the known limitations
in [`../ROADMAP.md`](../ROADMAP.md).

This is approximately the trust you already extend by running `codex` or
`claude` yourself, plus the runtime's guarantees, minus any claim about reads.

### `protected`

The brokered provider, whose filesystem, network and credential isolation
properties are proven before use and which fails closed when they are not.

### Trust is not authority

Neither mode authorizes anything. A coding agent authors a change and cannot
accept it — that is P2 and P12, and no trust mode touches them.

## Billing

Each of these CLIs treats its own API-key environment variable as an override
that displaces the interactive subscription session. Because the child
environment is built from scratch, `CODEX_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY` and `OPENAI_API_KEY` are all absent from the
invocation whether or not your shell has them. Selecting `claude` therefore runs
Claude Code, not an Anthropic API adapter substituted for it.

What the runtime does **not** claim is which plan ultimately paid. It can prove
which adapter and executable it invoked and that it substituted no credential of
its own; it cannot see inside a third-party CLI's authentication. So `auth_mode`
is an observation with stated provenance, and `unknown` is a legitimate and
common answer. It is never inferred to be `subscription` merely because Zenchron
supplied no key.

The same restraint applies to what counts as evidence. Only a provider's
credential artifact reports `local_cli_session`. A settings file, or the CLI's
state directory, proves the tool has been **configured** — a different fact,
which would be misleading under the same name, and misleading on exactly the
machines where nobody would think to check. Where an installed CLI exposes no
trustworthy credential artifact, the answer stays `unknown`.

On macOS, Claude Code stores its credentials in the login Keychain rather than
`~/.claude/.credentials.json`. The adapter does not observe the Keychain or infer
a session from a Keychain entry, so it reports `auth_mode: unknown` even when the
CLI is authenticated and works. Here, `unknown` does **not** mean unauthenticated
and does not block the agent: `ready` is decided by the executable and its
advertised capabilities, independently of the authentication observation.

## Readiness

```bash
zenchron-engineering autonomy agents --text
```

```text
AGENT        KIND           TRUST             READY  VERSION                  AUTH                 DETAIL
claude       claude_code    operator_trusted  yes    2.1.224 (Claude Code)    local_cli_session    executable found and every required capability is advertised
codex *      codex_cli      operator_trusted  yes    codex-cli 0.150.1        local_cli_session    executable found and every required capability is advertised
gemini       gemini_cli     operator_trusted  no     -                        unknown              executable gemini was not found on PATH
qwen         qwen_cli       operator_trusted  no     -                        unknown              executable qwen was not found on PATH
```

Nothing here spends money. Readiness requires an executable to be found and the
required capabilities to be advertised. Version and authentication observations
are reported alongside readiness; a credential file is not required to be ready. A
paid call is never made to prove a worker exists, and `ready` therefore means
"can be invoked", not "has budget" — account state is only observable by making
a paid request.

Readiness requires the installed CLI to **advertise** the exact flags the
runtime passes. Each adapter states the flags it depends on twice: once as a
capability the CLI must advertise, once as an argument that is passed. A CLI
that renamed or dropped one is reported unavailable rather than run in whatever
mode it defaults to — an unconstrained coding agent under your account whose
provenance describes it as constrained is the one failure this trust mode cannot
absorb.

`autonomy doctor` reports each agent independently. One uninstalled CLI is a
warning about that CLI, not a verdict on the system; having no usable worker at
all is the FAIL.

## The permission bypass

Every one of these CLIs offers a mode that approves everything. The runtime
never selects one on its own. Reaching it requires **two independent
statements**:

1. operator configuration, per agent: `"allow_permission_bypass": true`
2. the invocation, explicitly: `--dangerous-permission-bypass`

Either alone is refused *before* any process starts, so nothing executes under a
mode that was not authorized. When both are present the attempt's durable
provenance records `permission_bypass` and the effective mode, so a constrained
run and a bypass run stay distinguishable forever. `autonomy agents` and
`doctor` surface standing permission even when no run has used it, because a
standing permission is itself a posture.

## Invocation provenance

Every attempt records, without secrets:

```text
agent id, provider kind, trust mode, model
executable and version
effective sandbox mode and permission mode
whether a bypass was authorized and requested
observed auth mode and how it was observed
whether the workspace was bound by flag or by working directory
whether workspace instruction files were suppressed
the security-relevant argv, with the prompt replaced by its digest
```

The prompt is excluded from the durable record and referenced by digest: it
carries untrusted third-party text and is unbounded, while every
security-relevant flag is short and is kept verbatim.

`workspace_instructions_suppressed` is a real difference between providers.
Codex, Claude Code and Qwen expose a flag that keeps a candidate repository's own
`AGENTS.md` / `CLAUDE.md` / `QWEN.md` out of the session; the Gemini CLI does
not. Where it is false the runtime-owned trusted instruction text still frames
everything in the workspace as data, but the adapter cannot prove the file was
never read, so it does not claim to.

## Choosing an agent for a run

```bash
zenchron-engineering autonomy run issue 123 --agent codex
zenchron-engineering autonomy run issue 124 --agent claude
zenchron-engineering autonomy run issue 125            # the configured default
```

The chosen agent is frozen into the run's durable provenance at creation, in the
append-only journal. Changing your default later changes what *new* work uses;
runs already in flight keep the worker they started with, and a replay of the
journal alone says which one that was.

## Changing a live run's agent

```bash
zenchron-engineering autonomy agent set RUN --agent claude --reason "codex session lost"
```

**This milestone always refuses, and the refusal is the product.** It journals a
complete typed transition record — the predecessor attempt, the exact candidate
revision and tree, both agents, both trust modes, your stated reason, the
governing contract and configuration, the cumulative remaining budgets, the
evidence and authority context, and the feedback already consumed — then changes
nothing and tells you what to do instead:

```bash
zenchron-engineering autonomy run issue 123 --agent claude --new-generation
```

The existing run and its journal are left untouched.

Refusing is a decision rather than a gap. A safe in-run handoff has to
re-resolve the successor's effective grant against the new provider, carry
cumulative budgets forward, retire the predecessor's provider session without
letting the successor inherit it, and present already-admitted feedback as
history rather than as new input. Building that to serve a case an operator can
already express keeps the invariant that matters — a run's provider never
changes silently, and never widens privilege by changing — at a fraction of the
machinery.

Two properties hold regardless:

- **Budgets do not reset.** The record carries them, and dimensions no
  subscription CLI reports stay explicitly *unknown* rather than being flattened
  to zero. Zero would say the run has spent nothing and may spend nothing;
  neither is true.
- **Trust never lowers.** A run created under `protected` execution is never
  continued by an `operator_trusted` worker, and that refusal is classified
  distinctly so an operator can see which rule stopped them.

Running an issue again with a different `--agent` reaches the same refusal, so
the transition cannot be walked into by accident.

## Adding a provider

The change surface is an adapter spec, a kind registration, a branch in the
composition root's factory, and tests. Nothing in the scheduler, the reconciler,
the kernel, the authority evaluator, Git or the forge adapter learns the new
provider's name.

## Related documents

- [`product-architecture.md`](product-architecture.md) — where agents sit
- [`supervisor.md`](supervisor.md) — what drives them
- [`configuration.md`](configuration.md) — the full configuration surface
- [`../ROADMAP.md`](../ROADMAP.md) — agents versus future engineering roles

## Candidate Git is brokered

A provider may create and modify candidate work. It has **no authority to
destructively discard it**.

That law exists because of a measured cost. During live dogfood a coding CLI
twice reached for habitual recovery — `git checkout -- <path>` against a dirty
runtime-owned candidate workspace — and erased its own uncommitted
implementation edits. No governed candidate commit was lost, because the runtime
owns those; what was lost was the expensive part, the reasoning already
performed, and the worker then spent more subscription capacity reconstructing
it.

Every native CLI invocation therefore runs under a brokered Git boundary:

```text
provider runs git
      ↓
runtime-owned shim, first on the worker search path
      ↓
classify the argv
      ↓
discard family  → refused, recorded, diagnostic to the provider
everything else → executed as the real git
```

The refused family is the operations whose purpose or effect is to throw
uncommitted work away: `checkout`, `switch`, `restore`, `clean`,
`reset --hard`/`--merge`, `stash` (push/save/clear/drop), forced `git rm`,
`checkout-index`, `sparse-checkout`, `worktree`, `read-tree -u`, and
`submodule deinit`. Read-only and ordinary engineering Git — `status`, `diff`,
`log`, `show`, `add`, `apply`, `commit` — is passed through untouched. This is a
destruction guard, not a Git allowlist.

**Aliases are resolved before classification.** A worker could otherwise author
its own bypass out of two permitted commands — `git config alias.co checkout`,
then `git co -- file` — because real Git expands aliases *after* the broker has
authorized the verb it was spelled with, and a checked-out `.git/config` can
carry one too. The lookup runs under the same configuration the execution will
use, including leading `-c`/`-C` options, so `git -c alias.x=reset\ --hard x` is
resolved too. Chains are followed, cycles, shell aliases (`!…`), malformed
values and expansions that will not terminate all **fail closed** — "I could not
tell what this would do" is not "this is safe", and nothing executes alias
content to find out what it means. An alias whose name collides with a real Git
command is ignored, exactly as Git ignores it, so `git status` is never refused
because a config file once mentioned it. A resolved command is executed in its
resolved form, so the lookup and the execution cannot disagree.

**A destructive command is refused whether or not the workspace is dirty.** That
is the smaller law and the stronger one: permitting it on an observably clean
tree would require checking dirtiness and then executing, which is a race the
provider's own process can win by writing a file in between. The decision is
made from the argv, which cannot change. Dirtiness is still *observed*, so the
diagnostic and the durable record can say what was at stake, but it cannot
change the answer.

### Who asked

A refusal names the operation, the resource and, since #259, the actor that
directly originated it:

```text
model_tool           the model's own tool call
provider_runtime     the provider's machinery — repository probing, checkpointing, restore
zenchron_runtime     this controller
workload_subprocess  a program running inside the workload — a test binary, a build system
unknown              the runtime could not establish it
```

The origin is derived from execution topology — the distance between the broker
and the controller process that prepared the guard — and never from the shape of
the command. Reading intent out of argv is how a run's 63 refusals were first
reported as model stubbornness when the model had issued none of them: 51 came
from the candidate's own test binaries and the rest from the provider probing
its repository.

Two properties make the answer usable. **Causation is not provenance**: a model
that runs `go test ./...` causes the test binaries that follow, and the commits
those binaries attempt are recorded as `workload_subprocess`, never promoted to
`model_tool` because a model-originated process is somewhere in their ancestry.
And **the origin changes no decision** — it is recorded beside an answer that was
already given, an `unknown` origin is never more permissive than a known one, and
a provider kind whose topology has not been measured yields `workload_subprocess`
or `unknown` rather than a guess at the model.

### Why an absolute path does not get around it

Two mechanisms, and the second is the one that matters. The guard directory is
first on the worker's search path, so a bare `git` — what a model types —
resolves to the broker. And the worker's environment carries a brokered
`GIT_DIR` naming a path that is not a repository, so Git performs no discovery
at all: `/usr/bin/git reset --hard`, `sh -c '/usr/bin/git clean -fd'` and
`command /usr/bin/git restore .` fail, naming the brokered path in Git's own
error. The shim is the one caller that clears the sentinel.

**What is not claimed.** These workers are `operator_trusted`: they run under
the operator's own account. A worker that deliberately sets out to defeat the
runtime can both go around the name *and* clear the variable — `unset GIT_DIR;
/usr/bin/git reset --hard` reaches the repository. No in-process mechanism
closes that while one account owns both sides; it is the same residual this
trust mode already names, and it is why `RequireProtectedIsolation` refuses
these adapters for protected work. What is closed is the whole of the observed
failure: habitual destructive recovery, in every spelling that does not
dismantle the runtime's own environment.

**A controller that cannot install the boundary dispatches nothing.** The
production composition always intends the guard, so a broker executable it
cannot resolve is a controller unable to enforce its own law rather than a
composition that chose not to. That is refused before the capability probe and
before any process, as `candidate_guard_unavailable` — a typed wait an operator
clears by repairing the installation, never a provider fault, because nothing
about the worker, the work, the account or the network is wrong. A deliberately
unguarded composition — a unit test, a probe, an embedder driving one
invocation — remains possible and is recorded truthfully as `GitGuarded=false`.

The boundary grants the worker no new command surface. It adds no tool to any
provider's allowlist, so a stage that was obliged nothing is still obliged
nothing.

### What an operator sees

A refusal is an **observation, not a failure** — the invocation that carried it
may well have succeeded, which is the case worth reporting, because it is where
expensive reasoning was nearly lost and was not. `autonomy status` prints it
separately from any execution diagnostic:

```text
candidate discard refused   1 destructive Git operation(s) refused; dirty candidate work preserved: git checkout -- <1 operand(s)> (2 dirty candidate path(s) preserved)
```

It is never reported as provider quota, unavailability, a stall, an assurance
failure or an unknown: the runtime knew exactly what it refused.

The runtime's own candidate Git is unaffected. `RepositoryGitRunner` and
`GitRunner` build their environment from scratch and resolve Git from their own
trusted search path, so they never see the guard's path or its sentinel —
candidate creation, inspection, staging, runtime-owned commits, branches and
authorized pushes all behave exactly as before. The restriction is on the
execution provider's authority, not on Zenchron's candidate machinery.
