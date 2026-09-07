# Troubleshooting

Every refusal in this system is typed and names the member, path or condition
that produced it, because a fault an operator cannot locate is an outage. Start
here:

```bash
zenchron-engineering autonomy doctor --text     # per-capability preflight, spends nothing
zenchron-engineering autonomy agents --text     # per-worker readiness, spends nothing
zenchron-engineering autonomy status --text     # every run and what it is blocked on
zenchron-engineering autonomy status RUN --text # one run, with a `next action` line
zenchron-engineering autonomy events RUN        # the durable journal
```

Doctor creates nothing, repairs nothing, and takes no ownership lock: a
preflight that repaired what it measured could not report on it. It also runs
when the operator configuration does not load, so a broken installation still
gets an explanation.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | completed |
| 10 | waiting — including a run handed to a running supervisor |
| 11 | failed |
| 12 | cancelled |
| 13 | authority refused or stale, including a refused agent transition |
| 14 | unknown run |
| 64 | invalid usage or configuration |

## Agents

**An agent shows `READY no`.** The `DETAIL` column already says which of the
four causes it is.

| Detail | Cause | Fix |
| --- | --- | --- |
| `executable <name> was not found on PATH` | the CLI is not installed, or not on the `PATH` this process inherited | install it, or pin `agents.<id>.command` to an absolute path |
| `the installed <name> does not advertise the sandbox, permission and working-directory capabilities this runtime requires, so it is refused rather than run with weaker constraints` | the installed version renamed or dropped a flag the runtime depends on | upgrade or downgrade the CLI. The runtime will not fall back to a weaker invocation: an unconstrained coding agent whose provenance describes it as constrained is the failure `operator_trusted` exists to prevent |
| `agent <id> home <path> is not an existing directory` | `agents.<id>.home` points nowhere, or the operator has no home directory | create it, or remove `home` to use the operator's own |
| `the configured provider credential is readable by other users; run chmod 600 on it` | a brokered agent's `credential_path` is group- or world-readable | `chmod 600` it |

`READY yes` means the worker can be INVOKED. It does not mean the account has
budget: account state is only observable by making a paid request, which no
readiness command makes. `AUTH unknown` is likewise truthful — Zenchron can
prove which adapter and executable it invoked, not how a third-party CLI
authenticated internally — and is never guessed into `subscription`.

**`unknown agent "codx"; configured agents are claude, codex, gemini`.** A typo,
or an agent you removed. The message lists what actually exists. The same
refusal answers `--agent`, `--assign N=AGENT`, `agent set --agent` and a control
submission, because a request selects among configured agents and can never
introduce one.

**`default_agent is required when more than one agent is configured`.** Picking
one for you would be choosing which worker, and which account, does your work.
Name one. With exactly one agent configured it is the default and may be
omitted.

**`agents.x.trust_mode must be "operator_trusted" for kind "codex_cli", got
"protected"`.** A provider's trust mode is a property of its adapter and cannot
be raised or lowered by configuration. Every CLI kind is `operator_trusted`;
`openai_responses` is `protected`.

**`agents.x.credential_path is not valid for a native CLI agent`** or
**`agents.x.command is not valid for a brokered provider agent`.** The members
are per-kind. A native CLI authenticates itself and Zenchron holds no credential
for it; a brokered provider has no local executable and no bypassable boundary.

**`agents.x.command must be a bare program name or an absolute path, not a
relative path`.** A relative path with a separator resolves against whatever
directory the process happens to be in — including, for a candidate-adjacent
working directory, one the work being done could write to.

**`refused permission bypass <mode> for agent <id>`.** `--dangerous-permission-bypass`
is one of the two statements an unsafe provider mode requires. Set
`allow_permission_bypass` on that agent as well, or drop the flag. The refusal
is raised before the process starts, so nothing ran.

## Configuration

**`provider and agents are mutually exclusive: move the provider entry into the
agents registry`.** Two statements of which worker does the work are two sources
of truth. Delete the `provider` block and write the equivalent `agents` entry;
the mapping is in [configuration.md](configuration.md). Expect live runs to wait
at `controller_changed` afterwards, because run identity is derived from the
configuration digest.

**`repository configuration may not set "agents": it is operator authority`.**
The in-repo `.zenchron.json` scope is a stated allowlist checked before
decoding. It may name only `budgets.*` and `watch.poll_interval_seconds` /
`watch.max_concurrent_runs`. Move the member into the operator layer, which
lives outside every repository the runtime works on.

**`repository configuration may only tighten budgets.max_execution_attempts: 4
exceeds the operator bound 2`.** A proposed bound may only move DOWN. Raising is
refused rather than clamped, so a repository cannot probe the ceiling and never
learn it was overruled. `watch.poll_interval_seconds` is inverted and stated
inverted: a repository may only ask to be polled LESS often.

**`assurance.image must be a sha256: digest`.** A tag can move; the program that
decides whether a candidate passed must not change underneath a run.

**`watch.poll_interval_seconds must be at least 30`.** The floor is a floor, not
a clamp: a faster interval is refused so you learn the bound instead of silently
getting a different one than the file states.

**`input must contain exactly one JSON value`, a duplicate member error, or an
unknown field.** Both layers decode strictly. There is no lenient mode.

**Doctor reports `config.global` FAIL and everything else looks fine.** The
operator file did not load at all; nothing downstream of it was configured. Fix
that check first and re-run.

## State directory and ownership

**`state directory /path is mode 0755 and is reachable by other users; run
chmod 700 /path before serving a control endpoint`.** `serve` checks the
containing directory BEFORE creating anything, so a control path is never
published into a directory other users can reach.

```bash
chmod 700 /path
```

Doctor reports the same condition as `state.dir` with `run chmod 700`.

**`control endpoint /very/long/path/serve.sock is N bytes long, above the
100-byte limit an operating system allows for a socket address; choose a
shorter state_dir`.** A Unix socket address is a fixed-size kernel field.
Exceeding it otherwise fails with `invalid argument`, which explains nothing.
Move `state_dir` somewhere short — `~/.zenchron/state` — and restart. The state
directory contents are portable; the socket is recreated.

**`control endpoint <path>: exists and is not a socket; refusing to remove
it`.** Something else occupies the endpoint path. Removing it would be the
runtime deleting your file on a guess. Inspect it and move it yourself.

**`a supervisor is already listening on this state directory's control
endpoint`.** Exactly what it says, and it is deliberately distinct from a
permission fault: your action is to talk to the running supervisor, not to
repair anything. A socket left by a crash is reclaimed automatically, but only
after a connection attempt proves nobody is listening — unlinking first would
let a second supervisor steal a live endpoint from the first.

```bash
zenchron-engineering autonomy status --text     # is it doing what you wanted?
zenchron-engineering autonomy shutdown          # stop it, cancelling nothing
```

**`cannot take exclusive ownership of state dir <path>; another
zenchron-engineering process may already be running against it`.** The OS
advisory ownership lock is per process identity (host, pid, start token), so an
ordinary read alongside a running `serve` does not collide. Seeing this means a
process with the same identity holds it. Doctor's `state.lock` and
`state.liveness` checks probe the same mechanism; `state.liveness` FAILs on a
platform with no advisory locks, where a crashed owner could never be proven
dead and takeover would be blocked forever.

**`state_storage_exhausted`.** The state ceiling was reached before a candidate
clone was allocated, so nothing is half-written. Free space, or raise
`storage.max_state_bytes`, then `resume`. Nothing is ever reclaimed
automatically to make room: trading one active run's evidence for another's
progress is not a decision a scheduler gets to make. `autonomy gc --dry-run`
shows what is eligible under `gc.retention_hours`.

## Supervisor and submission

**`starting several issues at once needs a supervisor to own them; run
zenchron-engineering serve in another terminal, or start them one at a time with
autonomy run issue N --agent X`.** `run issues` is a submission, and there is
nothing to submit to. Exit 64.

**`no supervisor is running on <state_dir>; start one with
zenchron-engineering serve`.** `drain` and `shutdown` are instructions TO a
supervisor; with none running there is nothing to instruct, and saying so is
better than silently succeeding. `stop-all` works with or without one.

**`the supervisor is draining and is not accepting new work`.** A drain is
reversible only by restarting the supervisor. That is deliberate.

**`unknown agent "gemni"; configured agents are codex, claude`** from a
submission. One supervisor drives every configured agent, so this is a name that
is not in your registry rather than a limitation of the supervisor. Fix the
spelling, or add the agent to `agents` and restart `serve` so the new registry
is loaded.

**`repository "other/name" is not governed by this supervisor`.** Enrolment is
operator configuration, not a request. Add it to `watch.repositories` and
restart `serve`.

**`watch.repositories is empty: watch observes only repositories an operator
enrolled, so there is nothing to watch`.** `autonomy watch` needs enrolment.
`serve` does not: with no enrolment it governs the repository the invocation
targets and takes explicit submissions only.

## Assurance and Docker

**`assurance.verifier_sandbox` FAIL — `the verifier sandbox is <state>: Docker
is missing, the daemon is unreachable, or the pinned assurance image is not
present locally`.** Without it no candidate can be verified, so no run can
complete. Start the daemon and pull the image by its pinned digest.

**`assurance.toolchain` FAIL — `the pinned assurance image did not resolve the
Go toolchain on the runtime sandbox path`.** A reachable daemon holding the
image proves a container can start, not that it can build. The probe runs with
no network and nothing but an empty directory mounted, so the fix is an image
carrying a toolchain on that path; the runtime never falls back to a host Go.

**`assurance.docker_endpoint` FAIL.** `assurance.docker_host` must be a
`unix://` socket path or a `tcp://` endpoint with no userinfo, query or
fragment.

**`assurance.dependency_cache` FAIL.** Three spellings, one cause:

| Message | Fix |
| --- | --- |
| `no assurance.dependency_cache_dir is configured; offline verification has no trusted module material to read and never downloads any` | configure it |
| `the dependency cache <path> cannot be inspected` | create it |
| `the dependency cache <path> is EMPTY` | provision it from the trusted base module graph using the pinned image |

Verification is offline by contract. An empty cache is a false readiness claim,
not a warning: containers will start and then fail on missing modules.

**A run waits at `assurance_dependency_unavailable`.** The same environment
condition, met at run time. Re-running the identical command against the
identical environment produces the identical result, which is why it waits
instead of consuming assurance attempts. Provision what is missing, then
`resume`; the same run re-derives assurance against the same exact commit, tree
and contract.

**`assurance.semantic` WARN — `no independent semantic acceptance producer is
configured`.** Expected when no brokered provider is configured. The producer is
built from an `openai_responses` agent that has a `credential_path` — or from a
pre-registry `provider` block of kind `openai` — and it does not have to be the
agent doing the work. Configure one alongside your CLI agents if your policy
requires the semantic evidence class; without one, a contract requiring that
class is refused before any execution rather than failing after the work.

## GitHub

**`github_auth_required`.** The forge credential is gone. `gh auth login`, then
`resume`. With `github.credential_mode: "none"` this is the permanent state by
configuration: `none` is an explicit refusal to authorize forge writes, not
anonymous access.

**A run waits and `status` shows a `github` line with
`error_class=rate_limited`.** The forge budget is exhausted until the printed
reset time; no action is needed before then. Runs in one repository already
share one observation stream, so the budget is not multiplied by concurrency.

**My comment never reached the worker.** In order of likelihood:

| Cause | How to confirm | Fix |
| --- | --- | --- |
| Nothing has polled since the comment | `autonomy events RUN` shows no `feedback.observed` for it | polling belongs to whoever owns the clock. Either run `serve`, which polls every active run on a schedule, or drive the run yourself with `autonomy resume RUN`, which observes once before reconciling |
| `github.credential_mode` is `none`, or the adapter cannot resolve permissions | the observation says `the configured forge adapter cannot resolve actor permissions` | set `github-cli` and authenticate `gh` |
| The actor is below the threshold | `autonomy events RUN` shows `feedback.observed` with `actor is below the configured repository permission threshold` | grant repository permission, or lower `feedback.min_permission` deliberately — `read` on a public repository admits the entire internet |
| The actor is a bot or App | reason `authored by an automation account that the operator has not allowlisted` | add its exact login to `feedback.allowed_bots` |
| The comment is the runtime's own | reason `authored by this runtime` | nothing to fix; this is the self-loop guard |
| The review names a superseded commit | reason `describes a candidate head this run has already moved past` | re-review the current head named in `status` |
| The generation is terminal | the observation says `this generation is terminal; new feedback routes to the active generation` | comment on the live generation's pull request |
| It was delivered already | `feedback.consumed` names the keys, agent, operation and attempt | nothing; delivery is once, by design |

Full rules in [github-feedback.md](github-feedback.md).

## Runs

**Exit 14, `unknown run "run-..."; autonomy run issue <number> starts one`.**
The command was well formed and the configuration loaded; only the subject is
unknown. Check the id with `autonomy status --text`. A run id is derived from
the repository, the issue and the configuration digest, so a changed operator
configuration produces different ids.

**Exit 13 from `authorize`.** The state you approved has moved, or the action is
not human-authorizable. Nothing is broken and retrying verbatim will refuse
again: re-read `autonomy status RUN --text`, which names the current request id.

**Exit 13 from `agent set`.** Always, in this milestone. The printed record is
the product: it names both agents, both trust modes, the candidate revision and
tree, the contract and configuration in force, the remaining budgets, and the
consumed-feedback digest. Start a new generation with the agent you want:
`autonomy run issue N --agent X --new-generation`.

**`adopting it would reconcile another controller's work under this one`.** A
live generation was created by a different controller build or configuration.
Restore that configuration, or use `--new-generation`.

**`run ... was cancelled (operator_stop); explicit operator intent is not
withdrawn by asking again`.** A run id is deterministic from the repository, the
issue and the configuration digest, so `autonomy run issue N` addresses the same
cancelled run rather than creating a fresh one. Start new work with `autonomy
run issue N --new-generation`, which leaves the cancelled generation and its
journal untouched.

**`run ... is waiting on opt_in_removed`.** The opt-in label was removed from the
source issue, which withdraws consent to work on it. Restore the label; the run
then resumes through the ordinary schedule. Resuming does not restore consent.

**`run ... stopped on workspace_integrity_violation against its current
candidate`.** The runtime-owned workspace does not match the metadata baseline
the journal recorded — an edit or commit made outside the runtime, or the
aftermath of a conflicting base integration. There is no silent resume over a
mismatch. Inspect with `autonomy events RUN`, then start new work.

**`source_intent_changed`.** The pinned issue moved. A plain resume never
absorbs it; `autonomy refresh RUN` is the explicit, journalled path, and it
draws a generation boundary that abandons the previous candidate branch and any
pull request opened from it.

**`controller_changed`.** The run was created by a different controller binary or
configuration digest. Restore it, or start new work. A merged pull request
observed on the same pass still completes the run passively.

**`requested_privilege_expansion`.** Policy does not grant the permission the
observed scope needs. Fix the policy and start new work; privilege escalation
requires an external authority source, not a retry.

**`base_integration_conflict`.** The base moved and the candidate could not be
integrated. It routes to bounded provider remediation, never to a force-push
after publication. If the remediation budget is exhausted, start a new
generation. See the worked example in
[running-multiple-tasks.md](running-multiple-tasks.md).

**`governed_remote_mismatch`, `required_evidence_unsupported`,
`candidate_credential_material`.** All three stop rather than wait, because
retrying reads the same answer and no operator action clears them in place: the
governed remote defines which repository the run is about, an unsatisfiable
evidence class needs a different policy or a different configured producer, and
a credential value in candidate-visible content must be removed from the subject
itself. Each of them is detected before model budget is spent.

**`execution_provider_quota` / `execution_provider_rate_limited` /
`execution_provider_account_unavailable`.** Capacity and account waits, not
engineering attempts: no reasoning happened, no candidate moved, no evidence or
authority changed, and no remediation budget was consumed. Quota returns on the
provider's own schedule. Repeated rate limiting means
`supervisor.max_concurrent_runs` is above what that account tolerates. An
unavailable account is repaired by you, then `resume`.

**A run fails with `..._attempts_exhausted` or `..._failure_not_retryable`.**
The named operation ran out of its budget, or produced a class that does not
route to a retry. `autonomy logs RUN` shows what the worker actually said;
`autonomy events RUN` holds the journal. Raise the relevant budget in
`budgets.*` only if the work genuinely needs more attempts, then start a new
generation.

## See also

- [getting-started.md](getting-started.md), [running-work.md](running-work.md), [running-multiple-tasks.md](running-multiple-tasks.md)
- [github-feedback.md](github-feedback.md), [configuration.md](configuration.md)
- [agents.md](agents.md), [supervisor.md](supervisor.md), [product-architecture.md](product-architecture.md)
- [architecture.md](architecture.md), [spec/runtime-v0.1.md](spec/runtime-v0.1.md), [../README.md](../README.md), [../ROADMAP.md](../ROADMAP.md)
