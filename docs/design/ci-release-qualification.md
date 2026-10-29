# PR #594 — CI recovery and release qualification

Reviewed implementation: `06b16d5af852bed874d0cb74a7c9e4c58ee771ef`.
The follow-up changes only evidence ownership/tests, one browser synchronization
assertion, CI-only race partitioning and review/runbook documents.
The approved frontend, action-recovery implementation, runtime behavior, API,
schemas and all legacy implementations remain unchanged from that reviewed head.
This work is governed by ADR-0004's existing controller boundary and ADR-0007's
exact-revision evidence/trust rules. No CI guard, timeout, workflow or gate is
relaxed, and no new dependency, abstraction or file-size exception is introduced.

The exact-head isolated browser run exposed an assertion racing the workspace's
post-approval refresh: it observed the durable approval receipt while the main
read still explicitly said refreshing/unapproved. The driver now waits for the
same exact `Plan approved · r1` rendered receipt before its original assertion.
No arbitrary sleep, timeout increase or dropped assertion is used; failure to
refresh still fails the existing bounded wait. Application closure while reads
refresh and the reviewed action-recovery implementation are unchanged.

## Classification defect and correction

The focused registry tests reproduce the two reported failures: the new receipt
production file had no owner and its test file was unreachable. Both now belong
to the existing race-escalated `store` domain. Tests prove changes to either file
select the receipt test and all durable-store evidence, including journal payload
validation. The full conformance/selector suite passes.

Active React sources/build inputs, embedded preview/runtime assets and API wire
schemas now select `operator_surface`, a race-escalated runtime-boundary domain.
It covers bounded read projections, original receipts, controller proof/role and
lost-control-response semantics. Every other Go package still runs whole. It is
not a claim that a Go-only job executes browser, bundling or screen-reader tests;
those obligations remain explicit in the operator runbook. No active path is
newly declared inert. Existing legacy inert rules are unchanged.

The shared `schemas/validate.go` remains cross-domain and is now an explicit
hotspot. This PR adds API registrations there, but the same file owns the compiler
and every kernel/planning contract. Narrowing its coverage merely because the
current diff is small would misrepresent that owner. The branch still selects
whole-runtime T1 and every high-risk race domain: 1471 selected names and 1298
race names at this checkout, versus 1470/1285 before correction. The difference
adds the receipt test and controller-boundary coverage; no prior race name drops.
New unknown paths still fail safe to whole runtime.

## Race investigation and honest acceptance boundary

Reviewed-head [CI 38567437260](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38567437260)
binds to `06b16d5`. `go` and `evidence` fail only on the two registry guards;
their runtime suites pass at 1204/1201 seconds. `evidence-race` hits the 60-minute
package alarm while `TestReviewTriggerErrorIsReportedSeparatelyFromDriveFailure`
has run only three seconds. The stack is in a runnable SQLite journal read, not
a race-detector report or a failed test assertion. The unchanged named test passes
under bounded race probes on exact trusted main in 9.05 seconds and the PR in
8.67 seconds. No deterministic test regression is established by this comparison;
it does not qualify the whole selected suite. Logs and new-head results stay on
the PR, with the old failure retained.

Trusted comparison base is `e6d90b9454ebb125cc1252eb363e7a147fec9a75`, unchanged on
remote main at investigation time. Its exact push-event T2
[CI 38371992248](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38371992248)
passed. The same revision's full T3 race sweep
[38537985758](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38537985758)
passed with runtime at 4837.143 seconds (80.6 minutes); its earlier
[38385588183](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38385588183)
hit the existing 100-minute package alarm while the current test had run only
15 seconds. These are whole-sweep observations, not the PR's selected T1 race
acceptance. They establish substantial existing aggregate cost/runner variation,
not a proof that any particular new-head test is free of defects.

The PR's T1 race package bound remains 60 minutes. If it expires, compare the
named active test on the reviewed branch and trusted base with a bounded focused
race run, and retain the package failure separately. A short passing current test
does not explain all suite cost or qualify all selected tests. Deterministic
failure, race-detector report and package alarm are different findings. No
runtime optimization or test weakening is justified without such evidence.

To address the observed aggregate resource bound, CI partitions the same sorted
high-risk selection across four isolated runners. Tests prove the union contains
all 1298 selected names exactly once, including receipt evidence, for broad
fallback as well as narrow selection. Each part still uses race detection and
the unchanged 60-minute package timeout; no test or assertion is omitted.
Matrix fail-fast is disabled. The existing `evidence-race` check fails unless
all parts succeed; failure, cancellation and skip remain failures of the gate.
The strict required `go` check, push-event T2, trust policy and full unpartitioned
T3 are unchanged. This CI-only execution change is explicitly reviewable under
ADR-0007; it changes neither adoption eligibility nor runtime authority. Hosted
results for every part and the aggregate are required before acceptance.

## Parity, authority and compatibility

The [supported-operation and removal inventory](frontend-inventory.md) remains
current against the same main: login/observations, objectives/creation/revision,
plans/approval/rejection, WorkGraphs, runs/control/history, typed decisions,
publication evidence, products, agents and diagnostics have React destinations.
Source parity is not the completed operator objective-to-real-PR walkthrough.
Selected-option/text policy scenarios and manual assistive-technology checks
remain evidence debt. No supported legacy operation or frontend file is deleted.

Governed actions still prove the measured serving controller and execute under
its role through existing Control Plane/socket/controller/kernel paths. Receipt
metadata is observational, not authority. The exact original request survives
navigation/reload; stale authorization and automatic mutation retry are absent.
Command/receipt crash gaps remain unknown and closed, never declared unapplied.

Deployment must pair receipt-capable controller and read binaries. The reader is
read-only; a controller-only partial upgrade or old-controller/new-UI mix cannot
qualify all four recovery workflows. New events require no SQL migration but
older Go readers fail closed on unknown event kinds. After receipts are written,
roll back the frontend using a compatible Go binary; do not downgrade a reader
to pre-receipt code or delete history to make it open. Back up quiescent isolated
state/artifacts through the existing operator procedure before an authorized
deployment. Restoring an older database is not a way to undo an external push,
authorization or PR. Supersession, policy and controller adoption retain their
existing authority boundaries. An unmerged PR is not a trusted adopted controller.

## Next gate

### Runtime failure investigation (diagnostic head `29c690e`)

[CI 38593937196](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38593937196)
passed whole-runtime T1 (1013.307 seconds) and all four mandatory T1 race
partitions (1298 selected names). The required `go` job failed its runtime
package's unchanged 30-minute alarm. The named active test,
`TestWorkGraphIntegrationUnitInvalidatedWhenUpstreamOutputReplaced`, had run only
two seconds; its stack shows runnable reconciliation work. Its isolated trusted
main and PR probes passed at 7.45 and 7.51 seconds. This identifies a package
alarm, not a demonstrated deadlock or a proven cause of the package's cost.

The earlier objective conflict assertion remains unexplained. Failure-only
diagnostics now capture the approved plan, WorkGraph, reconcile/tick history,
runs, events, candidate contents and Git conflict state without changing the
60-iteration bound or required conflict/remediation/review assertions. Linux
isolation, shuffled repetitions, the 345-test related cohort and unpartitioned
runtime runs have not reproduced it. The original and diagnostic heads' complete
non-root Linux runs passed at 878.763 and 1704.241 seconds under different loads;
they do not establish a diagnostic-code regression or a fix. A forced failure
validated the logger only, not the original defect.

The `go` job now emits standard `go test -json` records so its downloadable log
retains per-test timings and failure output for the next diagnostic run. The
full unpartitioned suite, 30-minute bound, check identity and failure exit status
remain intact. No rerun is requested merely to obtain a green result. Two older
execution watcher goroutines in the panic stack warrant separate cleanup-path
inspection; neither their contribution to the alarm nor a connection to the
original conflict failure is established. Runtime behavior is unchanged.

Qualification remains **blocked** until the required exact-head checks pass and
the conflict failure has a supported explanation and regression proof. A focused
green test or passing T1/race job does not waive the failed full-suite check.

Execute the [operator acceptance runbook](operator-acceptance.md) against `/app`,
using a legitimately provisioned compatible controller. It distinguishes
read-only isolated checks, permitted test execution and explicit human plan,
decision and publication authority. It requires genuine PR outcome and manual
accessibility evidence and supplies no fabricated approval.

The PR reports exact-head CI conclusions and a remaining-blocker table. Required
checks must pass before CI qualification; human acceptance must be recorded
before cutover. Production restart/cutover, merge, self-approval, publication and
legacy retirement are not performed by this batch.
