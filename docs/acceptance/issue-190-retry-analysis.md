# #190: durable deterministic retry decisions

Base: current main `e209a07`. Scope: #190 and comment
[#6000726804](https://github.com/bogdaniel/zenchron-engineering/issues/190#issuecomment-6000726804).
The first ceiling-10 dogfood supplied a second production shape: the worker
addressing #337 produced a checkpoint, then observed policy compilation rejected
conflicting definitions of one requirement. The original workspace defect was
not needed to reach the retry storm.

Governing contracts: `docs/spec/runtime-v0.1.md` (operation lifecycle, failure
routing and retry dispositions), `docs/spec/v0.1.md` (pure policy compilation
and observed reassessment), P7/P10, and ADR-0003 (frozen budget authority).

## Trace before implementation

`EngineeringRuntime.Reconcile` folds the journal, repairs scheduler lag, checks
conditions and plans. `runOperation` plans a durable operation, then
`Scheduler.Next` scans all eligible operations, not just the requested one.
Its lease precedes the route check, `StartWithin` spends an attempt and physical
identity, and `handle` dispatches every kind. The effect and `operation.after`
are journalled before scheduler completion. Therefore the shared boundary for
withholding another attempt is after the actual lease and before `StartWithin`.

`lastFailure` previously rejected non-retry routes, but absence of a typed class
left retries bounded only by attempts. `failed` classified typed transport loss;
`commitFailed` recorded stage and a commit already produced, but no retry
classification. Policy/reassessment rejection therefore went straight back
through the budget-only path. This contradicted the repository's stated retry
law; no kernel invariant needs to change to repair it.

Every `RouteRetry` arm was traced:

| Class | Why another unchanged invocation can be legitimate |
| --- | --- |
| `transient_execution_provider`, `transient_infrastructure` | Explicit external/transient failure; existing attempt bounds apply. |
| `connectivity_unavailable` | Recognized transport loss; durable transport backoff and finite attempt authority remain. |
| `execution_incomplete`, `provider_no_progress` | A physical invocation reached a runtime bound; a new process can make progress. Productive bounded stops use checkpoints/continuations instead. |
| `feedback_unresolved`, `checkpoint_continuation_unresolved` | The provider did not resolve typed work; another response may resolve it. The continuation still ends its pass and retains its existing bounds. |
| `reviewer_protocol_incomplete` | One corrective invocation may repair a malformed/missing result; the existing correction ceiling remains. |
| `provider_background_work_unresolved` | A new invocation may actually finish its foreground work; existing finite retries remain. |

Other repeat paths were traced as well:

- `RouteWait` probes external prerequisites: account, quota/rate limit,
  availability, shutdown, toolchain, assurance prerequisites, storage, candidate
  guard and live writer. Existing wait/refund rules are unchanged. Authority
  waits never send a producer to solve authority.
- Format and candidate verdicts route to **different** producing operations;
  reassessment/integrity routes are not authority to repeat the same operation.
  Assurance's one identical confirmation is an explicit flake check, not an
  operation retry: a failed test verdict is not guessed deterministic.
- Explicit `unknown` stops. Unclassified filesystem, Git subprocess, forge and
  analyzer errors retain their existing budget-only behavior; repeating their
  wording does not prove determinism. Missing branches and ambiguous remote PR
  state can change externally and are not tagged local contradictions.
- Pure compiler rejection and locally established candidate/path/content/size,
  index, cleanliness or metadata contradictions can change only with relevant
  inputs. They now supply typed runtime-owned identities. Arbitrary errors
  inside an analyzer or tool are deliberately not wrapped as pure compilation.

Existing durable machinery supplies the operation identity/idempotency binding,
exact source/contract/candidate references, policy/model inputs and canonical
`Digest`. `runtimeCommit` already preserves a commit across failed observation.
Assurance failure signatures summarize verifier evidence, not model prose, but
cannot establish that arbitrary operation errors are deterministic. They remain
in the assurance/remediation path rather than becoming a universal classifier.

## Implementation and acceptance

The optional failure identity is journalled with the failed outcome. On the next
lease, equal deterministic identity and equal relevant input digest cause a
truthful wait before budget is spent. Changed input only makes a retry eligible;
the ordinary ceilings, integrity, permission and authority gates still govern.
No model reasoning, error-string match, sleep or retry timer decides equality.

`runtime/deterministic_retry_test.go` drives real runtime/SQLite/Git fixtures:

- Original three-attempt shape: real post-commit residue spends one failure;
  commit, dirty work and physical attempt identity remain intact across repeated
  reconciliations and clock changes.
- Dogfood shape: contradictory observed requirement definitions, with different
  rule/requirement ids, preserve a real checkpoint and spend one commit failure.
- The same law on `contract.compile`, before a workspace or provider exists.
- A repaired candidate path permits another attempt. A repaired policy permits
  recovery of the same checkpoint without a second commit. Uncommitted scratch
  and index refreshes cannot repair a committed-subject policy contradiction.
- Restart after `operation.after` and before the next reconciliation retains
  the failure identity and refuses another attempt.
- An identical unclassified analyzer error remains unclassified.
- Unavailable post-commit recovery storage remains an I/O error, rather than
  inheriting the original integrity refusal's deterministic classification.
- Candidate-only failures survive policy/model edits without another attempt.
  Name, flag and residue checks also ignore byte edits that cannot fix their
  predicate. Actual path/flag/residue repairs renew eligibility. Staged credential
  identity captures its file subject and ignores other files/index mode bits.
- A changed boundary model that actually changes compiled facts recovers the
  existing checkpoint, just as a repaired policy does.

`runtime/deterministic_wait_accounting_test.go` adds explicit timestamp proof:
the durable failure opens a wait, an hour of idle time stays excluded across
restart, the same operation's retry closes it at `operation.before`, two minutes
of recovery and three later minutes are charged, and another restart retains
that closed interval. Advancing past the frozen wall limit then stops the run.
Separate timestamp cases preserve other operations' probes and a new failure's
new wait. The scope inventory is normative in the runtime spec above; no common
source/policy/configuration basis remains for candidate-only checks.
Journal round trips preserve captured filename bytes. Restart with an older
draft's unversioned binding reports unresolved identity without granting a new
attempt merely because the binding definition changed.

Existing retry-routing, connectivity, provider-wait, deadline, checkpoint,
reviewer-protocol and post-commit recovery tests cover the preserved paths.
The #402 assertions still require exact commit/unproven-head preservation and
refuse assurance/publication. Known deterministic refusals now wait after one
failure instead of exhausting attempts; unclassified injected faults still
exercise bounded recovery attempts.

Validation results and the deliberate guard-removal mutation are recorded in
the PR. Full-suite evidence comes from GitHub CI for the current PR head.
The baseline file-size checker already fails on current main; no checker change
or unapproved exception is part of this issue.
