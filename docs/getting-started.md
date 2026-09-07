# Getting started

This page takes a machine with nothing installed to one finished run. It
assumes you are the operator: the local account that owns the state directory,
holds the GitHub credential, and has already installed and logged into at least
one coding CLI. Every boundary in this system is drawn around that account, so
who you are on this machine is a real part of the configuration.

## Prerequisites

| Requirement | Why |
| --- | --- |
| Go toolchain matching `go.mod` (currently 1.25) | builds the binary |
| `git` 2.32 or newer | 2.32 is the release where `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` take effect. Below it the host user's global Git configuration — including credential helpers and hook paths — leaks into every runtime Git call, so `autonomy doctor` fails `git.features` |
| `gh`, authenticated | the forge credential for reading issues, pushing candidates, opening pull requests and resolving actor permissions. `github.credential_mode: "none"` is the alternative and it publishes nothing |
| Docker, with the pinned assurance image present locally | verification runs in a container with no network, a read-only root, all capabilities dropped and no Docker socket. Without it no candidate can be verified, so no run can complete |
| A pre-warmed Go module cache directory | verification is offline and downloads nothing. An absent or empty cache is a failure, not a warning |
| At least one installed and authenticated coding CLI: `codex`, `claude`, `gemini` or `qwen` | the execution worker. The runtime drives the CLI you already log into; it does not ship one |

## Build

```bash
go build -o bin/zenchron-engineering ./cmd/zenchron-engineering
bin/zenchron-engineering version
```

For a build that records its own provenance, inject the source identity so runs
can name the exact binary that created them:

```bash
go build -trimpath -ldflags "\
  -X main.buildKind=pre_adoption_build \
  -X main.version=$(git rev-parse --short HEAD) \
  -X main.sourceRevision=$(git rev-parse HEAD) \
  -X main.sourceTree=$(git rev-parse HEAD^{tree})" \
  -o bin/zenchron-engineering ./cmd/zenchron-engineering
```

A build that injects nothing still runs; it simply records no provenance claim
and can never be mistaken for an adopted controller.

## Where things live

```text
<user config dir>/zenchron/config.json     operator configuration (override: --config, ZENCHRON_CONFIG)
<repository>/.zenchron.json                optional in-repo layer; may only tighten bounds

<state_dir>/                               mode 0700, owner-only
  runtime.db                               append-only journal, runs, operations
  serve.sock                               control endpoint, mode 0600, only while `serve` runs
  locks/runtime/                           OS advisory ownership locks
  runs/<run-id>/candidate/                 the runtime-owned candidate clone
  artifacts/provider/<agent>/<run>/...     per-attempt transcripts, raw and sanitized
  artifacts/feedback/<run>/                admitted third-party text, local-only
```

Nothing runtime-owned is written into the repository you are changing. Keep
`state_dir` short: the control endpoint is `<state_dir>/serve.sock`, and a
socket path over 100 bytes is refused by the operating system's address limit.

```bash
mkdir -p ~/.zenchron/state ~/.zenchron/modcache
chmod 700 ~/.zenchron/state
```

## A minimal working configuration

Write this to `<user config dir>/zenchron/config.json`. It configures one
worker, the CLI you already authenticated.

```json
{
  "state_dir": "/Users/you/.zenchron/state",
  "project_model_path": "/Users/you/.zenchron/project-model.json",
  "policy_path": "/Users/you/.zenchron/policy.json",
  "assurance": {
    "image": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "dependency_cache_dir": "/Users/you/.zenchron/modcache"
  },
  "agents": {
    "codex": {"kind": "codex_cli", "trust_mode": "operator_trusted"}
  },
  "github": {"credential_mode": "github-cli"},
  "budgets": {
    "wall_limit_seconds": 1800,
    "max_execution_attempts": 2,
    "max_remediation_attempts": 2,
    "max_assurance_attempts": 2
  }
}
```

With exactly one agent configured, `default_agent` is that agent and may be
omitted. `assurance.image` must be a `sha256:` digest, because the program that
decides whether a candidate passed must not change underneath a run. The
`ProjectModel` and `EngineeringPolicy` are schema-validated documents; the
shapes are in `schemas/project-model.schema.json` and
`schemas/engineering-policy.schema.json`, and `fixtures/v0.1/valid/` holds
worked examples of both. Every member is explained in
[configuration.md](configuration.md).

## Check the installation before spending anything

```bash
bin/zenchron-engineering autonomy doctor --text
```

Doctor answers, per capability, whether the thing a real run depends on is
actually there: the state directory and its mode, the runtime database and its
schema, the ownership lock and the crash evidence behind it, the trusted Git
binary and its isolation, the Docker endpoint, the pinned image and whether it
resolves a Go toolchain offline, the dependency cache, the forge credential and
its rate limit, both configuration layers, per-agent readiness, the control
endpoint, and the state ceiling. It creates nothing, repairs nothing, takes no
ownership lock, and makes no model call. `FAIL` exits 11; `WARN` and `PASS` exit
0. Resolve every `FAIL` before starting work — [troubleshooting.md](troubleshooting.md)
maps the common ones to a fix.

```bash
bin/zenchron-engineering autonomy agents --text
```

```text
AGENT        KIND           TRUST             READY  VERSION   AUTH                 DETAIL
codex *      codex_cli      operator_trusted  yes    0.45.0    local_cli_session    executable found and every required capability is advertised
```

This listing spends nothing: an executable is found or it is not, a version
string is printed or it is not, an authentication state file exists or it does
not. No model call is ever made to prove a worker exists. `ready` means the
worker can be invoked, not that its account has budget — account state is only
observable by making a paid request, which this command never does. `auth` is
what was OBSERVED of that CLI's own state; `unknown` is a truthful answer and is
never guessed into `subscription`.

Readiness requires the installed CLI to ADVERTISE the exact flags the runtime
passes. A CLI that has renamed or dropped one of them is reported unavailable
rather than run in a weaker mode, because an unconstrained coding agent whose
provenance describes it as constrained is the one failure `operator_trusted`
cannot tolerate.

## The first run

From inside a clone of the repository you want changed, with an open issue:

```bash
bin/zenchron-engineering autonomy run issue 42
bin/zenchron-engineering autonomy status --text
bin/zenchron-engineering autonomy logs run-... --follow
```

`run issue` prints the run id it created, then drives the run in this terminal
until it reaches a resting point, and exits with that disposition: 0 completed,
10 waiting, 11 failed, 12 cancelled, 13 an authority refusal, 14 an unknown run,
64 invalid usage or configuration. Expect the first run to stop at
`awaiting_authority`: publication is a protected action and no coding agent
authorizes its own change. `autonomy status RUN --text` names the exact request
to answer, and [running-work.md](running-work.md) walks the whole lifecycle.

Closing the terminal is not a cancellation. The journal is durable; the run
resumes with `autonomy resume RUN`. `autonomy stop RUN` is the only thing that
cancels a run.

## What is billed, plainly

An `operator_trusted` agent is the CLI you installed and logged into, run under
your own account. The runtime builds that child process's environment from
scratch rather than inheriting yours, so `CODEX_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, `GOOGLE_API_KEY` and `OPENAI_API_KEY` are absent from the
invocation whether or not your shell has them. Each of those CLIs treats such a
variable as an override that displaces the interactive subscription session, so
their absence is exactly what keeps "Zenchron supervised my Claude Code run"
from silently meaning "Zenchron billed my API account". Claude Code's `--bare`
flag, which forces strict API-key authentication, is deliberately never passed.
The runtime does not claim to know which plan ultimately paid; it claims only
that it substituted no credential of its own, which is why `auth` may read
`unknown`.

OpenAI API billing IS used when, and only when, you configure it:

- an agent of kind `openai_responses` with a `credential_path`, when a run is
  actually bound to that agent. That is the `protected` provider: its isolation
  is proven, it fails closed, and its requests are metered against the key at
  that path;
- a brokered provider configured as the independent semantic acceptance
  producer — an `openai_responses` agent with a `credential_path`, or the
  pre-registry `provider` block of kind `openai`. That verifier makes paid model
  calls during assurance when a contract requires the semantic evidence class,
  whether or not the agent doing the work is a local CLI.

OpenAI API billing is NOT used by `autonomy doctor`, `autonomy agents`,
`autonomy status`, `autonomy logs`, `autonomy events`, or by any run bound to a
native CLI agent. Readiness, diagnosis and every operator read are free by
construction.

## Next

- [running-work.md](running-work.md) — one issue from creation to merge
- [running-multiple-tasks.md](running-multiple-tasks.md) — `serve`, several issues at once
- [github-feedback.md](github-feedback.md) — directing a worker from a review
- [configuration.md](configuration.md) — every member and its default
- [troubleshooting.md](troubleshooting.md) — symptom to cause to fix
- [agents.md](agents.md), [supervisor.md](supervisor.md), [product-architecture.md](product-architecture.md)
- [architecture.md](architecture.md), [../README.md](../README.md), [../ROADMAP.md](../ROADMAP.md)
