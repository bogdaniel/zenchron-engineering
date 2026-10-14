# PR #594 — focused uncertainty recovery review

Review #5516015303 at `de644a3` found that creation, revision, resume and refresh lost their
unresolved operation when a component unmounted. Current run/plan state cannot prove which request
produced it; resume previously had no durable acknowledgement at all.
This batch addresses that one finding. Visual identity, primary composition, legacy UI and existing
controller authority paths remain intact. The next gate is independent re-review, not cutover.

## Original request retained; result observed separately

Before a POST, the shared component saves a delivery reference in origin-local storage scoped by a
hash of the existing credential. Each operation has its own UUID key, so concurrent references cannot
overwrite one another. It retains the original protected subject/parameters, controller binding and
SHA-256 of the exact escaped API path, newline and JSON wire body. It stores no credential, full body
or operator note. Reload/navigation restores every unresolved reference for that interaction scope;
new submissions remain disabled until exact reconciliation. Corrupt/unavailable storage fails closed,
and an unsuccessful pre-send save sends nothing. No automatic expiry, retry queue or resend exists.

The four interactions send the correlation UUID in `X-Zenchron-Operation-ID`. After auth/CSRF/shape
validation, Go computes the wire digest itself and forwards that metadata through the same one-shot
control request. The controller still proves its live identity and holds its existing role before
executing the original command. Only after success does it append an observational receipt to the
existing plan/run journal, before replying. Metadata cannot authorize execution or material outcomes.

`GET /v1/operator-action-receipts/{id}?digest=...` is bearer-authenticated and read-only. It uses the
existing event primary key for an exact bounded lookup, verifies the event hash and subject, and
returns command/controller/request correlation plus canonical plan or run-result references. Planning
recovery verifies original plan/revision or repository/issue; refresh verifies the source and exact
successor. An unrelated current state or mismatched digest cannot settle the request. Resume receipts
mean scheduler reconciliation was admitted; they do not mean a run completed or a worker executed.

Recovery focuses a clearly historical receipt with a link to the canonical result. It retains the
original subject even if the currently displayed plan advanced or the source run was cancelled by
refresh. Domain state, evidence and authority are still owned by their original reducers/kernel.
The controller refuses redispatch when the correlation is already journalled; this is not a new
idempotency entitlement. Existing domain guards remain responsible for concurrent command semantics.

## Failure and compatibility boundaries

A command and its receipt are separate existing journal transactions. A crash/write failure between
the effect and its receipt cannot be declared unapplied. Receipt persistence failure is explicitly
`unknown` (`action_outcome_unknown`), never a refusal. No receipt, a failed read or wrong digest leaves
the original browser reference and submission block intact. The operator can inspect the exact subject;
absence does not license a retry. Successful domain effects are not rolled back or silently repeated.

There is no SQL migration, new database, runtime, scheduler, policy or authoritative workflow state
machine. Two closed event types, `plan.operator_action_receipt` and `run.operator_action_receipt`, are
added to the existing journal. They change no phase, budget, candidate, gate or authority state; tests
check identical state digests before/after each receipt. They are delivery observations, not acceptance
evidence. API/body contracts for legacy callers are unchanged; the correlation header/socket fields
are optional and the old interface continues to work without them.

Paired controller/reader Go builds are required to support receipt recovery. An older controller may
ignore correlation and leave recovery unproven; the browser stays closed. Older Go readers do not
recognize these new event types and fail closed. Roll back the frontend using a compatible Go binary;
do not roll the runtime back to a reader unable to understand a journal already containing receipts.
Browser reference loss (manual storage clearing, credential rotation or a different origin/browser)
is not proof that an operation never happened. Kernel authority and domain idempotency remain binding.

## Proof and independent acceptance

- Updated actual-controller Chrome driver discards the real HTTP reply after each of the four
  successful commands, navigates away, returns and reloads. It verifies the original request stays
  blocked, wrong-digest and missing-receipt reads remain unknown, then recovers the exact result with
  one POST per operation. Creation/revision remain unapproved; refresh selects the actual successor.
- Storage-write refusal sends zero POSTs. Persisted references contain neither credential nor note.
  Historical receipt focus and dark/light 1440/1920/390 contrast/reflow checks remain in the driver.
- Go counts exactly four receipt events, confirms they leave workflow state digests unchanged, closes
  the actual writer and reconstructs identical receipts through a fresh read-only store.
- Focused runtime receipt tests cover stream shape, annotation refusal, read-only reopen and tampering.
  HTTP tests cover authenticated exact binding, invalid IDs, missing/different receipts, and persistence
  uncertainty being `unknown`. Existing same-request/role/refusal tests remain required.
- Strict TypeScript/embedded build, static/Windows checks, file-size checks and affected race suites
  are run. No dependency, size exception, timeout, assertion or required CI gate is weakened.

The final local driver passed 11 checks, including original revision 1 retained on the revision 2
page. Affected Control Plane/schema race suites, receipt durability/tamper tests, controller-role
tests, TypeScript/build/static checks and 21 Python regressions pass. The preserved presentation
driver passed 76 captures; the file-size check passes without a new exception or raised ceiling.
No new dependency, interface, size-cap crossing or `ponytail:` corner was introduced. The new hook
owns only shared browser delivery-reference I/O; the existing transport and journal are reused.

[Actual rendered recovery and responsive gallery](screenshots/action-recovery/README.md).
Local results and exact-head CI are reported on the PR. New recovery screenshots are isolated
controller test data, not operator acceptance. Manual screen-reader/high-zoom qualification and real
operator acceptance remain outstanding; production cutover, publication, merge and self-approval
remain unauthorized. No legacy template, asset, route or test is retired.

## Prior-head CI issue, not waived

At `de644a3`, required `go` and `evidence` passed. `evidence-race` hit its existing 60-minute runtime
package budget with 1285 selected tests; the last reported test had been running 8 seconds. Its
focused `TestReviewTriggerSurvivesSupervisorRestart` race probe passed in 11.6 seconds under a bounded
90-second run. This does not prove the full suite would pass, or identify a deterministic individual
failure. The timeout/fallback policy is unchanged. New-head CI must be inspected separately and the
race qualification remains open unless that job passes. No rerun-to-green or timeout waiver is used.
