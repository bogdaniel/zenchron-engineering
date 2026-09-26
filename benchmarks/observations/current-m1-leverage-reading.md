# Current M1 leverage reading

As of 2026-09-25, **the accessible record does not support a measured
`zenchron_explicit` cohort or a leverage claim**.

```text
direct_agent: unmeasured
leverage_ratio: unmeasured
operator_attention: unavailable
```

This is an access-limited reading, not a completed harvest of the operator's
serve history. No primary run/event records for the nominated candidates were
available in this candidate workspace. The existence or absence of comparable
measurements outside this workspace remains unknown. Zero eligible observations
here does not mean zero product runs, zero successes, or zero supervision.

## Evidence and provenance

[Machine-readable evidence](current-m1-evidence.json) contains the candidate
dispositions, source paths and SHA-256 digests, access results, and explicitly
empty observation set. It is an evidence inventory, not a new benchmark schema
or synthetic measurement. The base revision is supplied by the task context;
it was not independently recovered from Git history.

The workspace inventory found source, tests, fixtures and acceptance narratives,
but no runtime database, JSONL event journal or candidate-specific run export.
`git log` failed under the runtime Git guard; no history was obtained and the
guard was not bypassed. GitHub was not accessed because network access was
prohibited. The operator state directory is outside the permitted workspace.
These restrictions prevent the required primary-record harvest; this report
must not be described as completing that part of #317.

The inspected durable repository material supports only these narrower facts:

- `benchmarks/README.md` states that the direct and explicit baselines are not
  implemented there. This statement does not establish the contents of external
  runtime state.
- `runtime/journal.go` and `runtime/sqlite_store.go` define persisted run identity,
  operations and hash-chained events in `runtime.db`. They identify where a
  future authorized export should come from, but are not observations of runs.
- `docs/acceptance/m2-dogfood.md` is context only: it describes a separate planning
  exercise and operator interventions. Its narrative is not repurposed as an
  explicit-mode measurement. Tests and fixtures are likewise not product runs.

No timestamps, provider/model identities, controller identities, attempts,
feedback counts, terminal outcomes or PR acceptance results for the nominated
candidates have been verified. The PR/issue numbers below are selection leads
from #317, not independently harvested GitHub facts.

## Candidate classification

No candidate is included. “Excluded” means missing evidence for this reading,
not evidence that the run was unsuccessful or unsuitable in principle.

| Candidate | Classification | Missing fact / reason |
| --- | --- | --- |
| Issue #217 | excluded | Run identity, lifecycle timestamps and controller binding; overlap with #221 unverified. |
| Issue #221 | excluded | Run identity, lifecycle timestamps and controller binding; overlap with #217 unverified. |
| PR #299 | excluded | Review history, feedback events, corrective attempts and accepted revision unavailable. |
| Issue #307 | excluded | Journal and provider outcome unavailable; provider/lifecycle failure attribution unverified. |
| Failed first #66 attempt behind PR #313 | excluded | Attempt journal and PR history unavailable; failure and terminal outcome unverified. |
| PR #309 | excluded | Run-to-PR binding, agent/provider/model, acceptance evidence and terminal state unavailable. |
| PR #311 | excluded | Run-to-PR binding, agent/provider/model, acceptance evidence and terminal state unavailable. |
| PR #294 | excluded | Run-to-PR binding, agent/provider/model, acceptance evidence and terminal state unavailable. |
| PR #263 | excluded | Run-to-PR binding, agent/provider/model, acceptance evidence and terminal state unavailable. |
| Automatic controller successions without deployment relay | excluded | No transition IDs, controller records or operator activity record available; no-relay claim unverified. |

PR #316 is mentioned in the issue's motivation, not supplied as a measurement
candidate. Neither benchmark implementation is a dependency of this reading.

## What remains unmeasured

Elapsed run time is not human-attention time. Even with event timestamps,
counts of approvals, retries, feedback or transitions cannot reveal minutes
spent reading, deciding, prompting, correcting, monitoring or recovering.
Silence in a journal cannot prove that no human intervention occurred.

For this explicit cohort, where supervision occurred is also unknown. The
candidate descriptions suggest review correction and failure recovery as
places to inspect, but they are not verified interventions. No operator timing
log is available. Provider usage and cost are unavailable, not zero. Accepted
work, success rate, throughput, concurrency and latency cannot be calculated
without the missing primary records and acceptance boundaries.

## Smallest prospective comparison

With no eligible historical observation and no historical attention timer,
one new direct-agent run alone cannot produce a valid leverage ratio. The
minimum is **one real task executed twice**, once directly and once through
explicit serve, with prospectively timed operator attention in both arms.
No existing observation is currently eligible to pair with the direct run.

1. Select one bounded, real behavioral or build/tooling task before either run.
   A nominated PR's original task may be used only after its original objective,
   base revision and acceptance criteria are recovered and frozen. Do not use
   this evidence-report task as a substitute for a product-work observation.
2. Start isolated copies at the same base with the same task, dependencies,
   tools, permissions and acceptance criteria. Use the same coding agent,
   provider, exact model/version and relevant settings; record these identities
   at invocation. Keep solutions hidden between arms, use independent operators
   where feasible, and record operator experience and order to disclose learning
   effects. No planner is involved.
3. In the direct arm, the operator drives that coding agent manually. In the
   explicit arm, the operator assigns the same objective through serve. Include
   all attempts, failures and recovery through independent acceptance or a
   predeclared stopping limit; retain failed outcomes rather than replacing them.
4. For each operator, record timestamped start/stop attention intervals from
   task intake through acceptance: reading/setup, task submission, active
   monitoring, clarification, review, correction prompts, approvals, recovery,
   deployment relay if needed, and final verification. Pause for unattended
   waiting; record waiting separately as elapsed time. Record shared setup
   separately with a predeclared allocation. Resolve overlapping intervals so
   a person's time is not counted twice; sum across people. Mark missed timing
   as missing, never reconstruct it from event counts.
5. Preserve run/attempt/controller identities, provider/model, base and final
   revisions, PR references, timestamped events, acceptance results and attention
   logs. Record provider usage/cost only when actually reported. Evaluate both
   arms against the frozen acceptance criteria on their exact resulting trees.
6. If both arms produce the same accepted task and both attention totals are
   complete and positive, report direct attention divided by explicit attention
   as a **single-task attention ratio**, alongside outcomes and elapsed times.
   If acceptance differs, timing is missing, or the denominator is zero, leave
   leverage unmeasured. One pair does not establish general product leverage.

An existing explicit observation can replace the prospective explicit arm only
if its primary identity/acceptance evidence and contemporaneous attention log
are recovered, and the same starting task and agent/provider/model are available.
The named candidates have not met those conditions in this reading.

## Completing the historical harvest

An authorized follow-up needs an in-workspace, sanitized export of the nominated
runs from the existing durable store, including their ordered events and
operations, controller transition records, agent bindings, and the corresponding
GitHub issue/PR/review/acceptance history. Preserve source identifiers and export
provenance, include unsuccessful attempts, and distinguish terminal run state
from accepted work. Derive overlap only from recorded intervals, distinguishing
run-lifetime overlap from active provider execution. A no-relay claim additionally
needs operator evidence; automated transitions alone cannot prove it.

No production code, benchmark apparatus, permission policy or sensitive-data
handling changed. The authorization and sensitive-data boundaries are unchanged;
this change contains only this report and its evidence inventory.
