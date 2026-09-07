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

Nothing here spends money. Readiness is an executable being found, capabilities
being advertised, a version being printed and a credential file existing. A
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
