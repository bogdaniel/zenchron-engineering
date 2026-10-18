# #344 paired dogfood protocol (#481)

Status: **DRAFT until frozen.** A protocol is frozen when the values marked
`FREEZE:` are filled in, the file is committed, and its git blob digest is
recorded in the first line of each arm's operator log. After either arm starts,
this file changes only by a new dated amendment that states why; the original
text is never edited, and an amendment made to improve a result invalidates the
pair.

Parent epic #471. Milestone #481. Measurement definitions #66. Owner decisions:
issue #481 comment 6251685466. This protocol is not an acceptance of #481 and
does not authorize the measured run; the owner does that separately.

## 1. Question

Given one human, the same coding-agent families, one frozen base and the same
acceptance criteria, how do active human supervision, manual relays, authority
interventions and workarounds compare between a `zenchron_planned` run and a
`direct_agent` run of #344, and did each arm produce an independently reviewed,
review-ready change?

One pair is one observation. It reports no leverage ratio and is never read
against #66's representative ≥2× gate. `zenchron_planned` is not the
`zenchron_explicit` cohort.

## 2. Frozen task

Issue #344, "Governed operator release for held-material workspaces", exactly as
it reads at the freeze. Issue body sha256 `d045a5e90188212846ac2705c4fccab0b4e36c59de45c4b68419e22c61d62054`,
issue `updatedAt` `2026-10-03T18:28:36Z`, observed 2026-10-18. Re-check both at the
freeze; a difference stops the freeze.

Both arms receive the identical task text below as their only task statement, and
the same acceptance criteria. Nothing else about the solution is supplied.

Acceptance criteria (all are required for an `accepted` outcome):

1. An explicit operator command releases a run's held material and journals a
   typed release event. The event is bound to the exact held record: kind,
   revision, tree, and the operation and content digest where present.
2. Release refuses a stale or mismatched record reference and a run with no held
   material, with a distinct, typed error.
3. Release is idempotent (a second identical release is a no-op, not a second
   event) and restart-safe (replay reproduces it).
4. After release, `gc` may collect the workspace under its normal retention and
   liveness rules; unreleased held material is still never collected.
5. Status identifies that the material was released, by whom and why.
6. Release grants no execution, publication or budget authority and does not
   change the run's terminal disposition or reason.
7. Tests exercise disposable isolated held-material workspaces only. No real held
   workspace is read, released or collected at any point.
8. The change passes the repository's required checks on its exact head, is
   independently reviewed (a producer is never the sole source of acceptance
   evidence), and is a review-ready PR. No merge happens in either arm.

Non-goals (from #344): automatic release after a timeout; salvaging or
re-queueing released material.

## 3. Frozen controls

| Control | Value |
| --- | --- |
| Repository | `bogdaniel/zenchron-engineering` |
| Base SHA, both arms | FREEZE: `main` after the measurement slice, #598 and #612 land |
| #344 code on base | none (verified at freeze: no release operation exists; `git grep -n -i 'release' runtime/ | grep -i held` empty) |
| Instrumentation version | FREEZE: git SHA of the measurement slice and its `benchmark` result schema version |
| Controller | FREEZE: `zenchron-engineering` build version and source revision for Arm A |
| Provider families and models | Implementer `codex` (openai, `codex-cli 0.163.0`), reviewer `claude` (anthropic, Claude Code `2.1.302`). Neither config pins a model; the CLIs' defaults apply. FREEZE: the resolved provider, model id, CLI version and relevant settings of each CLI, captured before Arm A. If a resolved model identity cannot be established, the CLI default is not a frozen identity: the limitation is recorded and resolved before the measured comparison launches. Resolved by one CLI invocation each on 2026-10-18 (owner-authorized, one trivial prompt per CLI): `codex` `codex-cli 0.163.0` reported model `gpt-6.1-sol`, provider `openai`, reasoning effort `high`; `claude` Claude Code `2.1.302`, configured alias `sonnet`, reported model `claude-sonnet-5-5`. The alias is not the identity: the frozen reviewer identity is `claude-sonnet-5-5`, and an alias that later resolves differently is a mismatch. At the start of each arm the same facts are captured again and compared with the frozen values; a difference (a default silently changing between arms) stops the arm and is reported, because it compromises comparability. Arm B uses the same CLIs, versions and models |
| Agent capabilities and trust modes | PROPOSED from the operator config: `codex` (`codex_cli`, `operator_trusted`), `claude` (`claude_code`, `operator_trusted`), default agent `codex`. Arm B uses the same CLIs with the same permission modes |
| Execution budgets | PROPOSED from the operator config: per run `wall_limit_seconds` 1800, `max_execution_attempts` 2, `max_remediation_attempts` 2, `max_assurance_attempts` 2. FREEZE: the plan budget envelope. Arm B has the same wall-clock allowance (see §7) |
| Reviewer requirement | An independent reviewer of a different vendor family from the implementer. Arm B must also use a different-family reviewer, run by the operator |
| Workspaces | two separate clones at the frozen base, distinct paths, distinct branches `dogfood/344-planned` and `dogfood/344-direct` |
| Arm order | Arm A (`zenchron_planned`) first, Arm B (`direct_agent`) second |
| Operator | Bogdan Olteanu operates both arms and times both arms |

## 4. Operator rules

Permitted in both arms: ordinary coding-agent CLIs and their own tools, the
operator's editor and terminal, GitHub, and the Product Workspace in Arm A.

Not permitted in either measured arm: any additional AI operator assistant that
queues, monitors, interprets results, drafts interventions or reviews work.
This session's assistant is not used during either arm.

Arm A orchestration control (approve plan, resolve a decision, inspect handoffs,
review outcomes, find the PR) goes through the Product Workspace. CLI commands
that belong to #344's own engineering and test contract are not orchestration
relays. Carrying a prompt, finding, context or status between workers by hand is
a relay in either arm.

Arm B coordination is manual by definition. Every prompt, finding, status or
context carried from one agent session to another is logged as a relay, none
hidden.

Intended governance acts are logged as authority interventions, not relays:
plan approval, a genuine DecisionRequest resolution, publication or merge
intent. No DecisionRequest is manufactured. Whether a genuine one arose is
recorded either way.

## 5. Logging

The operator starts the activity log (the `benchmark` operator log) before the
first action of the arm and keeps it running. Entry kinds: `task_preparation`,
`active_supervision`, `reviewing_output`, `authority_intervention`,
`manual_relay`, `recovery_workaround`, `unattended_wait`,
`final_acceptance_merge`. Waiting on a provider or on CI is `unattended_wait`
and never counts as human attention. Time during which the operator does
something else unrelated is not logged as supervision either; the log records
what was done, not what was possible.

Each arm also records the screen and terminal session for independent review.
If no inspectable recording exists for an arm, its relay count is reported as
`self_reported`.

Runtime facts for Arm A come only from the existing durable state through the
bounded collector. Tokens and cost are recorded only where a provider reports
them; otherwise `unknown`. Subscription CLIs reporting no cost are `unknown`,
not zero. Arm B has no runtime state: its provider usage is whatever the CLI
reports, else `unknown`.

## 6. Required measurements

Per arm, at minimum: active human supervision minutes by kind; manual relays
(self-reported or audited); authority interventions; workarounds; workers and
WorkUnits (Arm A) or sessions (Arm B); parallelism and overlap; review and
remediation cycles; accepted or rejected final outcome and why; cycle time from
first task preparation to review-ready PR or to stop; tokens and cost where
reported; provider and model identities; governance and safety outcomes
(refusals, unsafe authority outcomes, escaped defects found by the auditor).

An arm that fails, is abandoned or exceeds its allowance is measured the same
way and reported unchanged. Failure is a result, not an invalidation.

## 7. Stopping and allowance

Each arm stops at the first of: a review-ready PR with all criteria met and an
independent review recorded; the operator declares the arm failed; or the
wall-clock allowance of 4 hours of elapsed time since the arm's first log
entry. The same allowance applies to both arms. Active human supervision is
measured separately and is not what the allowance counts. An arm that reaches the
limit without accepted work is recorded as an incomplete (censored) result with
its state at the limit: not an artificial success and not a zero-productivity
comparison. Stopping early is logged with a
reason and is not repeated.

## 8. Isolation

Arm B starts only after Arm A's provider sessions are closed. Arm B's workspace is
a fresh clone at the frozen base, with no Arm A branch, patch, stash or remote
ref present. No Arm A diff, plan, handoff, review or transcript is pasted into an
Arm B prompt. Provider session identifiers differ between arms and are recorded.
`git log`, a remote branch listing and the provider session list are captured at
Arm B's start as isolation evidence.

## 9. Known confounds, stated up front

- n = 1 pair. No generalization and no ratio.
- Arm B runs second: the operator has learned the task, the codebase's shape and
  likely pitfalls from Arm A. This favors Arm B on time and relays and cannot be
  removed.
- Arm A includes planning and orchestration overhead and the product's own
  early-version defects; Arm B has none.
- Self-reported relay and time counts until audited.
- Provider variance, quota or outage waits, and CI queue time differ by arm.
- Subscription-CLI cost is unknown for both arms.
- The operator knows which arm is the product under test.

## 10. Independent audit

After both arms, the raw logs, session recordings, the frozen protocol and the
collector output go to an auditor who operated neither arm. The auditor classifies
relays from the evidence and may reclassify log entries. The audit is recorded
beside the raw log as a separate document; the raw log is never edited. Until an
audit exists, relay counts carry `self_reported`. No auditor is named or implied
if none inspected the evidence.

## 11. Invalidation

The pair is invalidated, and reported as invalid with the reason, if any of these
occurs:

- either arm started from a base other than the frozen SHA;
- #344 code, a patch or a transcript from one arm entered the other;
- the activity log was not running from the arm's first action, or entries were
  edited after the fact;
- different operators ran the two arms;
- an AI operator assistant queued, monitored, interpreted or drafted during a
  measured arm;
- the protocol, task text, criteria, models, budgets or allowance were changed
  after an arm started to improve a result;
- a real held workspace was touched;
- Arm A's runtime state was reset, edited or replayed to hide a failure.

A failed or poor arm is not an invalidation.

## 12. Outputs

Raw operator logs and collector records, the derived result JSON and report for
each arm under `benchmarks/`, the audit document, and a short comparison that
states the confounds in §9 first. Nothing is edited to look better afterward.
