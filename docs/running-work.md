# Running one piece of work

One issue, end to end: choosing it, choosing a worker, watching the run, finding
what it produced, answering the authority boundary, and dealing with each way it
can stop. Everything here is a read or a write over durable state, so nothing
depends on your terminal staying open.

## Choose the issue

The unit of work is a GitHub issue. The runtime pins a SNAPSHOT of the issue at
run creation — title, body, state, digest — and compiles the work contract from
that snapshot. It does not re-read the issue as you edit it, because a contract
whose subject moved underneath it is a contract nobody agreed to. If the issue
genuinely changed, `autonomy refresh RUN` is the explicit path; see below.

An issue existing causes no execution. Automatic discovery is off unless you
enrolled repositories in `watch.repositories`.

## Choose the agent

```bash
zenchron-engineering autonomy agents --text
```

`--agent ID` binds the run to one configured worker; without it the run uses
`default_agent`. The binding is journalled at creation and is what every later
attempt, transcript and provenance record names. Two things follow from that:

- a live generation keeps the agent it was created with. `autonomy run issue N
  --agent other` against a live run refuses with a typed transition record
  rather than silently changing workers;
- execution trust and acceptance authority are separate. An `operator_trusted`
  agent may author a change and still cannot authorize it, and no coding agent
  authorizes its own change.

## Start it

```bash
zenchron-engineering autonomy run issue 42 --agent codex
```

The run id is deterministic from the repository, the issue and the configuration
digest, so the same command in the same configuration addresses the same run
rather than creating a second one. The command prints either
`created generation run-...` or `adopted existing generation run-... (this
controller created it)`, then drives the run until it settles.

- `--new-generation` always creates a fresh run and leaves any live generation
  untouched. Use it after a `stop`, after a terminal failure, or when you want
  the same issue worked by a different agent.
- adoption is refused when a DIFFERENT controller build or configuration created
  the live generation: reconciling it here would drive another controller's work
  under this one.
- `--dangerous-permission-bypass` requests the provider's unsafe permission
  mode for this invocation. It is refused before any process starts unless that
  agent's configuration also sets `allow_permission_bypass`. Two independent
  statements are required precisely so a bypass can never be reached by a
  default, and the attempt provenance records it forever.
- if a supervisor is running on this state directory, the run is submitted to it
  and the command returns with exit 10 instead of driving the work here. See
  [running-multiple-tasks.md](running-multiple-tasks.md).

## The lifecycle

```text
  source.observe        pin the issue snapshot; observe closure and opt-in
        |
  contract.compile      facts + policy -> obligations, invariants, permissions
        |
  candidate.create      runtime-owned clone at <state_dir>/runs/<run>/candidate
        |               on branch zenchron/<run-id>
        v
  execution.invoke  ->  candidate.commit
        |  ^                  |
        |  |                  v
        |  +--- remediation.gofmt / execution.remediation
        |                     |
        v                     v
  assurance.go          assurance.semantic        (offline, exact tree, in the pinned image)
        |
  authority.evaluate    action-scoped decision over evidence
        |
        +---- not authorized ----> waiting: awaiting_authority / authority_blocked
        |
  base.integrate        rebase before publication, merge from base after it
        |
  candidate.push -> pull_request.create / pull_request.update
        |
  github.observe        CI, reviews, merge state
        |
        v
  completed: merged
```

`phase` in `status` is the operator projection of that path — `contract`,
`execute`, `observe`, `assure`, `authorize`, `remediate`, `publish` — and
`disposition` is independently `active`, `waiting`, `completed`, `failed` or
`cancelled`. A waiting run may still perform the two observation-only
operations, `source.observe` and `github.observe`, so a run that is waiting can
still notice that its pull request was merged; it executes, verifies, authorizes
and publishes nothing while it waits.

## Watch it

```bash
zenchron-engineering autonomy status --text            # every run
zenchron-engineering autonomy status run-... --text    # one run
zenchron-engineering autonomy logs run-... --follow    # what the worker is saying
zenchron-engineering autonomy events run-... --follow  # the durable journal
```

`status RUN --text` renders the run's own state: source issue and digest,
controller identity, build provenance, both configuration digests, disposition
and reason, phase, base and candidate revisions, contract, current operation and
lease, evidence, every authority decision by action, the pending authority
request, superseded requests, pull request state, metadata integrity, forge
waits, attempt tallies, any sanitized execution diagnostic, and one `next
action` line. That last line is the only interpretive field and it is a total
function of the rest: the same durable state always yields the same sentence.

`logs` prints the SANITIZED transcript of each provider attempt, with a header
naming the operation, the attempt number and the agent that produced it. The raw
transcript beside it is local-only forensic material and is deliberately never
rendered. `events` renders the journal in persisted order, opening the store
directly: it takes no ownership of the state directory and no run-driving lease,
so it is safe to tail a run a supervisor is driving. A payload above 1 KiB is
replaced by its size and digest rather than truncated into JSON that no longer
parses.

Both reads exit with the run's own disposition mapping, so `status` and `run`
can never disagree about what a disposition means.

## Find what it produced

| Thing | Where |
| --- | --- |
| Candidate workspace | `<state_dir>/runs/<run-id>/candidate`, also reported as `workspace` in `autonomy status` JSON |
| Branch | `zenchron/<run-id>` |
| Pull request | the `pull request` line of `status RUN --text`, and `BRANCH / PR` in the fleet view |
| Attempt transcripts | `<state_dir>/artifacts/provider/<agent>/<run>/<operation>/attempt-N.sanitized-candidate.log`, read for you by `autonomy logs` |
| Admitted feedback text | `<state_dir>/artifacts/feedback/<run>/`, local-only |

The candidate is a runtime-owned clone outside the repository you are changing.
Read it freely; do not commit in it. The runtime records a metadata baseline
after every Git operation it owns and checks the workspace against the journal
before handing it to anything, so an outside commit is detected as a
`workspace_integrity_violation` and there is no silent resume over it.

## Answer the authority boundary

A protected action — publishing, merging — proceeds only on a current authorized
decision. When every remaining requirement is a human approval, the run waits at
`awaiting_authority` and `status` names an exact request id:

```bash
zenchron-engineering autonomy authorize run-... req-... --approve --note "reviewed the diff"
zenchron-engineering autonomy authorize run-... req-... --reject
```

You type a run id and a request id and nothing else. The request id is a digest
of the exact state — candidate revision, tree, contract revision, controller
digest — so naming the id IS naming the state you approved. Recording an answer
is EVIDENCE, not a permission: the evaluator decides afterwards what that
evidence means, and "recorded, still blocked" and "recorded, still incomplete"
are ordinary results. If the candidate moved between reading and answering, the
request is stale and the command exits 13; nothing is broken and retrying the
same command verbatim will refuse again. Superseded requests you already
answered stay visible in `status` as `stale request`, because an approval is
contextual evidence and never a standing permission.

## Resume, refresh, stop

```bash
zenchron-engineering autonomy resume run-...      # reconcile again
zenchron-engineering autonomy refresh run-...     # re-read changed source intent
zenchron-engineering autonomy stop run-...        # cancel, durably, with a journalled reason
```

`resume` means exactly one thing: ask the runtime to reconcile this run again.
It clears no wait. A restored credential simply proceeds; evidence that now
satisfies a claim simply proceeds; a wait the runtime re-derives from durable
state stays. Two conditions are refused rather than walked over, because
reconciling would be a silent override: `opt_in_removed`, where consent was
withdrawn by removing the label, and a `workspace_integrity_violation` against
the current candidate. A cancelled run is not resumed either — explicit operator
intent is not withdrawn by asking again.

`refresh` is the only thing that re-reads changed source intent, and it is
deliberately not a flag on `resume`. It draws a GENERATION BOUNDARY: the current
generation is settled through the same cancellation path `stop` uses, under the
distinct reason `operator_source_refresh`, which stales that run's contract,
evidence and authority together; the next generation for the same issue is then
started and reconciled. The old journal is preserved exactly, and because the
boundary is a generation, the previous candidate branch and any pull request
opened from it are abandoned.

Two things are worth separating there, because the words overlap. Both `stop`
and `refresh` settle a generation as `cancelled` — that is the DISPOSITION, and
`autonomy status` reports it identically for either. What differs is the recorded
REASON and what happens next: `stop` is an operator withdrawing the work and
starts nothing, while `refresh` is an operator replacing the subject and starts
the successor generation in the same command.

`stop` is the only thing that cancels a run WITHOUT replacing it. It is durable, idempotent, and
journalled with the reason `operator_stop`; a second stop appends nothing.
Killing the process, closing the terminal, or shutting down a supervisor cancels
nothing.

## Changing a run's agent

```bash
zenchron-engineering autonomy agent set run-... --agent claude --reason "codex is rate limited"
```

This always REFUSES in this milestone, and the refusal is the product. A
complete typed transition record is journalled and printed: the candidate
revision and tree, both agent identities with both trust modes, the stated
reason, the contract and configuration digests in force, the cumulative
remaining budgets with unknown dimensions left unknown, the count and digest of
already-consumed feedback, and the refusal code. The attempt is therefore
auditable, and the invariant holds: a run's provider never changes silently and
never widens privilege by changing. A run created under `protected` is never
continued by an `operator_trusted` worker.

Start the work you actually want instead:

```bash
zenchron-engineering autonomy run issue 42 --agent claude --new-generation
```

## What a stop means

Waiting reasons, in the vocabulary `status` prints:

| Reason | Meaning | What clears it |
| --- | --- | --- |
| `awaiting_authority` | every outstanding requirement is a human approval | `autonomy authorize` |
| `authority_blocked` | valid evidence establishes a violation | fix the violation in new work |
| `evidence_required`, `fresh_evidence_required` | a machine claim is unproduced or stale; the run may still produce it | ordinary reconciliation |
| `execution_provider_quota` | the worker's plan or account allowance is spent | it returns on the provider's own schedule, then `resume` |
| `execution_provider_rate_limited` | the provider asked to be called less often | wait; repeated rate limiting means the configured concurrency is above what that account tolerates |
| `execution_provider_account_unavailable` | the provider refused at its own account boundary before any reasoning happened | repair the account, then `resume` |
| `assurance_dependency_unavailable` | the verifier's environment is not there: no toolchain in the image, a missing or empty module cache, or a module the offline cache does not hold | provision it, then `resume`; the same run re-derives assurance against the same commit and tree |
| `state_storage_exhausted` | the local state ceiling was reached before a candidate clone was allocated | free space or raise `storage.max_state_bytes`; nothing is ever reclaimed automatically to make room |
| `github_auth_required` | the forge credential is gone | `gh auth login`, then `resume` |
| `opt_in_removed` | the opt-in label was removed, withdrawing consent | restore the label; resuming does not restore consent |
| `source_intent_changed` | the pinned issue moved | `autonomy refresh RUN`; a plain resume never absorbs it |
| `source_closed` | the source issue was closed | reopen it, or stop the run |
| `controller_changed` | a different controller build or configuration created this run | restore that configuration, or start new work |
| `candidate_external_changed` | the candidate head was changed outside the runtime | inspect it; no approval can be given past this |
| `requested_privilege_expansion` | the observed scope needs a permission policy does not grant | fix the policy, then start new work |
| `execution_checkpointed` | productive work was preserved when a runtime bound was reached | `resume` continues it |
| `merged` | terminal success | nothing |

Failures stop rather than wait, because retrying cannot change the answer:

| Class | Why it stops |
| --- | --- |
| `governed_remote_mismatch` | the workspace is bound to a remote that is not this run's governed remote. Changing the governed remote changes which repository the run is about, and that is a different run |
| `required_evidence_unsupported` | the claim gating a protected action names an evidence class no configured producer declares and no human records. Detected before any model budget is spent |
| `candidate_credential_material` | a high-confidence credential value is present in candidate-visible content, detected before any producer is admitted |
| `workspace_integrity_violation` | the runtime-owned workspace does not match the metadata baseline the journal recorded |
| `material_scope_change` | observed changes materially exceed the contract; facts and policy are recomputed and the contract revised |
| `base_integration_conflict` | the base moved and the candidate could not be integrated cleanly; routed to bounded remediation, never to a force-push |

Terminal failure reasons carry the operation that exhausted: for example
`execution.invoke_attempts_exhausted` or `assurance.go_failure_not_retryable`.

## See also

- [getting-started.md](getting-started.md) — prerequisites and the first run
- [running-multiple-tasks.md](running-multiple-tasks.md) — several issues under one supervisor
- [github-feedback.md](github-feedback.md) — steering a live run from a review
- [configuration.md](configuration.md) — budgets, agents, storage
- [troubleshooting.md](troubleshooting.md) — symptom to cause to fix
- [agents.md](agents.md), [supervisor.md](supervisor.md), [product-architecture.md](product-architecture.md)
- [architecture.md](architecture.md), [spec/runtime-v0.1.md](spec/runtime-v0.1.md), [../README.md](../README.md), [../ROADMAP.md](../ROADMAP.md)
