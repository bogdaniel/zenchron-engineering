# Configuration

Configuration is two layers with strictly different authority. Which layer a
member lives in is a security decision, not a filing decision: the operator
layer is the only place that may name a credential, a worker, an assurance
image, a state directory, or the identity a run is recorded against, and the
in-repo layer may only lower bounds the operator already set. A repository the
runtime is changing therefore cannot rewrite the terms under which it is
changed.

```text
operator layer  <user config dir>/zenchron/config.json   authorizes
      |         (or --config PATH, or $ZENCHRON_CONFIG)
      v
in-repo layer   <repository root>/.zenchron.json         may only TIGHTEN
      |
      v
effective configuration for this invocation, digested per layer
```

Both layers are decoded strictly: exactly one JSON value, no duplicate object
members, no unknown members. Each layer is digested over its canonical form, so
a run records which configuration governed it rather than a path that may have
moved. Changing the operator layer changes the digest, and a run created under a
different digest is a different run identity; a live run whose controller
configuration moved waits with `controller_changed` rather than continuing under
terms nobody approved.

## Resolving the operator file

1. `--config PATH` if given;
2. `ZENCHRON_CONFIG` if set;
3. `<user config dir>/zenchron/config.json` — `~/Library/Application Support`
   on macOS, `$XDG_CONFIG_HOME` or `~/.config` on Linux.

## Complete operator layer

```json
{
  "state_dir": "/Users/you/.zenchron/state",
  "project_model_path": "/Users/you/.zenchron/project-model.json",
  "policy_path": "/Users/you/.zenchron/policy.json",
  "assurance": {
    "image": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "docker_host": "",
    "dependency_cache_dir": "/Users/you/.zenchron/modcache"
  },
  "agents": {
    "codex":  {"kind": "codex_cli",   "trust_mode": "operator_trusted"},
    "claude": {"kind": "claude_code", "trust_mode": "operator_trusted"},
    "gemini": {"kind": "gemini_cli",  "trust_mode": "operator_trusted"},
    "qwen":   {"kind": "qwen_cli",    "trust_mode": "operator_trusted"},
    "openai": {
      "kind": "openai_responses",
      "trust_mode": "protected",
      "model": "gpt-5",
      "credential_path": "/Users/you/.zenchron/openai.key"
    }
  },
  "default_agent": "codex",
  "github": {"credential_mode": "github-cli"},
  "budgets": {
    "wall_limit_seconds": 1800,
    "max_execution_attempts": 2,
    "max_execution_continuations": 8,
    "max_remediation_attempts": 2,
    "max_assurance_attempts": 2
  },
  "supervisor": {"max_concurrent_runs": 3, "poll_interval_seconds": 60},
  "storage": {"max_state_bytes": 21474836480},
  "feedback": {
    "min_permission": "write",
    "allowed_bots": ["some-review[bot]"],
    "self_logins": ["my-github-login"]
  },
  "watch": {
    "repositories": ["owner/name"],
    "label": "zenchron:auto",
    "poll_interval_seconds": 60,
    "max_concurrent_runs": 3
  },
  "gc": {"retention_hours": 168},
  "operator": {"id": "you@example.com", "require_configured_id": true}
}
```

### Paths and assurance

| Member | Meaning | Default |
| --- | --- | --- |
| `state_dir` | Absolute path to the runtime's own directory: `runtime.db`, `artifacts/`, `runs/<run>/candidate`, `locks/`, and the `serve.sock` control endpoint. Must be mode 0700. | required |
| `project_model_path` | Absolute path to a schema-valid `ProjectModel`. | required |
| `policy_path` | Absolute path to a schema-valid `EngineeringPolicy`. | required |
| `assurance.image` | The verification container image, pinned as a `sha256:` digest. A tag is refused: the program that decides whether a candidate passed must not change underneath a run. | required |
| `assurance.docker_host` | `unix://` socket path or `tcp://` endpoint, with no userinfo, query or fragment. | the daemon's default |
| `assurance.dependency_cache_dir` | Absolute path to the operator-provisioned Go module cache mounted read-only into every verification. Verification is offline and downloads nothing, so an absent or empty cache is a doctor `FAIL`. | unset, and unusable |

Keep `state_dir` short. The control endpoint path is `<state_dir>/serve.sock`
and a Unix socket address is a fixed-size kernel field; a path over 100 bytes is
refused with that reason.

### Agents

`agents` is a map from an operator-chosen id to one worker. An id is 1-64
characters of `a-z`, `0-9`, `-` or `_`, and may not start with a separator.

| Member | Meaning | Default |
| --- | --- | --- |
| `kind` | `codex_cli`, `claude_code`, `gemini_cli`, `qwen_cli`, or `openai_responses`. The kind decides which adapter is built and which service or executable sees the work. | required |
| `trust_mode` | `operator_trusted` for every CLI kind, `protected` for `openai_responses`. Stated by the operator and checked against the kind; a mismatch is refused. Trust is a property of the adapter and configuration can neither raise nor lower it. | required |
| `command` | Native CLI only. A bare program name resolved on your `PATH`, or an absolute path pinning one exact program. A relative path containing a separator is refused, because it would resolve against whatever directory the process happens to be in. | the kind's own name: `codex`, `claude`, `gemini`, `qwen` |
| `model` | The model to ask that provider for. Required for `openai_responses`. | the CLI's own configured default |
| `home` | Native CLI only, absolute. The directory holding that CLI's own authentication and session state. When it is set the provider's home variable (`CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `QWEN_HOME`) is set too; when it is not, the agent is invoked exactly as you would invoke it yourself. | the operator's home directory |
| `credential_path` | `openai_responses` only, absolute. A path to an operator-held credential, never a token value. Naming it on a native CLI agent is refused: the CLI authenticates itself and Zenchron holds no credential for it. | required for `openai_responses` |
| `endpoint` | `openai_responses` only. Overrides the provider's API endpoint. Refused on a native CLI agent. | provider default |
| `allow_permission_bypass` | Native CLI only. Standing operator permission for that provider's unsafe mode. It grants nothing alone: an invocation must also pass `--dangerous-permission-bypass`, and the resulting attempt provenance records it forever. Refused on `openai_responses`, whose boundary is proven rather than bypassable. | `false` |
| `unattended` | Whether the supervisor's automatic discovery may start work on this agent. Explicit `false` keeps it to explicit operator commands. | `true` |

`default_agent` names the agent `autonomy run issue N` uses when no `--agent` is
given. It is required whenever more than one agent is configured; with exactly
one, that one is the default. Naming an agent that is not in the registry is
refused with the list of configured ids.

### Migrating from `provider`

`provider` and `agents` are mutually exclusive. Two statements of which worker
does the work are two sources of truth, so a file carrying both is refused with
`provider and agents are mutually exclusive: move the provider entry into the
agents registry` rather than resolved by a precedence rule nobody wrote down.

A configuration that still has only `provider` keeps working. It is migrated
into a one-agent registry under the id that provider already recorded, in the
trust mode it already had:

| `provider.kind` | agent id | agent kind | trust mode |
| --- | --- | --- | --- |
| `native-codex` | `native-codex` | `codex_cli` | `operator_trusted` |
| `openai` | `openai-responses` | `openai_responses` | `protected` |

The migrated Codex agent keeps the pre-registry environment exactly, where
`provider.credential_path` was exported as both `HOME` and `CODEX_HOME`; a newly
configured `codex_cli` agent does not get that environment. To move over, delete
the whole `provider` block and write the equivalent `agents` entry. Two
consequences of the move are worth knowing before you make it:

- run identities are derived from the configuration digest, so live runs created
  under the old file wait with `controller_changed`. Finish them first, or
  restart the work with `autonomy run issue N --new-generation`.
- the independent semantic acceptance producer is built from a BROKERED
  provider: an `openai_responses` agent that has a `credential_path`, or the
  pre-registry `provider` block of kind `openai`. If your old file supplied one,
  configure an equivalent brokered agent alongside your CLI agents; it does not
  have to be the agent doing the work, and the same operator credential serves
  both. Without one, `autonomy doctor` reports `assurance.semantic` as `WARN`
  and a contract requiring the semantic evidence class is refused before any
  execution rather than failing after the work.

### Budgets

Every budget is an operator ceiling for one run. `wall_limit_seconds`,
`max_execution_attempts`, `max_remediation_attempts` and
`max_assurance_attempts` are required and must be at least 1.
`max_execution_continuations` may be absent, because configurations written
before it existed cannot contain it; absent resolves to 8, and an explicit 0 is
refused like any other malformed bound. Attempts and continuations are different
resources: attempts retry one execution binding, continuations are successive
pieces of productive work.

### Concurrency and polling

`supervisor.max_concurrent_runs` and `watch.max_concurrent_runs` both state the
run ceiling; `supervisor.poll_interval_seconds` and
`watch.poll_interval_seconds` both state the interval. When both are stated the
STRICTER value wins — fewer concurrent runs, longer interval — so neither member
can be used to loosen the other. The default ceiling is 1: an operator who wants
parallel work must raise it. The polling floor is 30 seconds and it is a floor,
not a clamp: a faster interval is refused rather than silently replaced. The
default interval is 60 seconds.

### Discovery

`watch.repositories` is the complete set the runtime may observe. There is no
discovery crawler, so a repository that is not listed is not watched, whatever
it says about itself. Automatic issue intake is DISABLED while the list is
empty, which is the default for new configuration: an issue exists because a
project records work in issues, and its existence is not consent to spend a
subscription on it. `watch.label` is the opt-in label an issue must carry;
default `zenchron:auto`.

### Storage, feedback, GC, identity

| Member | Meaning | Default |
| --- | --- | --- |
| `storage.max_state_bytes` | Ceiling on the state directory, checked before a candidate clone is allocated so a full disk is a typed wait rather than a half-written workspace. | 0, meaning unbounded |
| `feedback.min_permission` | The repository permission an actor must currently hold before their review or comment may reach a worker. See [github-feedback.md](github-feedback.md). | `write` |
| `feedback.allowed_bots` | Automation logins explicitly admitted, in GitHub's own login spelling. | none |
| `github.credential_mode` | `github-cli` uses your own `gh` login; `token` gives the runtime a publication identity of its own so your reviews are admissible feedback; `none` refuses forge writes. | required |
| `github.token_path` | Absolute path to an owner-only file holding the publication token, required by and only used with `credential_mode: "token"`. | none |
| `budgets.lifecycle_deadline_seconds` | Optional bound on TOTAL elapsed time for a run, including waits on people and accounts. `wall_limit_seconds` bounds the work; this bounds the calendar. Absent means a run waits as long as a person takes. | none |
| `feedback.self_logins` | Identities the operator knows to be this system. The runtime also resolves its own credential identity on every feedback observation; this member exists for the identities it cannot discover. | none |
| `gc.retention_hours` | Retention window for `autonomy gc`. Nothing younger is ever eligible for reclamation. | 168 (7 days) |
| `operator.id` | The identity a run is recorded as having been authorized by. It is provenance, not authentication: nothing here is signed and no challenge was issued. | the local account name |
| `operator.require_configured_id` | Refuse the local account name as a substitute for a configured identity. | `false` |
| `github.endpoint` | Alternate API endpoint. | github.com |

## The in-repo layer

`.zenchron.json` at the repository root is read from the controller checkout. It
is optional; a missing file is not an error, a present and invalid one is. Its
scope is a stated allowlist checked BEFORE decoding, so adding a field to the
Go struct cannot by itself hand a repository a new authority.

May name:

| Member | Rule |
| --- | --- |
| `budgets.wall_limit_seconds` | at least 1, at or below the operator value |
| `budgets.max_execution_attempts` | at least 1, at or below the operator value |
| `budgets.max_execution_continuations` | at least 1, at or below the operator value |
| `budgets.max_remediation_attempts` | at least 1, at or below the operator value |
| `budgets.max_assurance_attempts` | at least 1, at or below the operator value |
| `watch.max_concurrent_runs` | at least 1, at or below the effective operator ceiling |
| `watch.poll_interval_seconds` | at or above the effective operator interval — a repository may only ask to be polled LESS often |

May not name, and is refused with `repository configuration may not set "X": it
is operator authority`: `state_dir`, `project_model_path`, `policy_path`,
`assurance`, `provider`, `agents`, `default_agent`, `github`, `feedback`,
`storage`, `supervisor`, `gc`, `operator`, and inside `watch` both
`repositories` and `label`. Every credential, every endpoint, every worker
identity, the enrolment set, the opt-in label and the approver identity are
therefore unreachable from inside a repository.

A proposal that would RAISE a bound is refused rather than clamped:
`repository configuration may only tighten budgets.max_execution_attempts: 4
exceeds the operator bound 2`. Silently handing back the operator's number would
let a repository probe the ceiling and never learn it had been overruled.

```json
{
  "budgets": {"wall_limit_seconds": 900, "max_execution_attempts": 1},
  "watch": {"poll_interval_seconds": 300}
}
```

`autonomy doctor --text` reports `config.global`, `config.repository`,
`config.tighten` and `config.watch` separately, so a refusal names which layer
and which member produced it.

## See also

- [getting-started.md](getting-started.md) — the minimal working configuration
- [running-work.md](running-work.md) — driving one issue
- [running-multiple-tasks.md](running-multiple-tasks.md) — the concurrency ceiling in practice
- [github-feedback.md](github-feedback.md) — the `feedback` block in full
- [troubleshooting.md](troubleshooting.md) — configuration refusals by message
- [agents.md](agents.md), [supervisor.md](supervisor.md), [product-architecture.md](product-architecture.md)
- [architecture.md](architecture.md), [spec/runtime-v0.1.md](spec/runtime-v0.1.md), [../README.md](../README.md), [../ROADMAP.md](../ROADMAP.md)
