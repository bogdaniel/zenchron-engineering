# PR #594 — CI recovery and release qualification

Reviewed implementation: `06b16d5af852bed874d0cb74a7c9e4c58ee771ef`.
The follow-up changes only evidence ownership/tests and review/runbook documents.
The approved frontend, action-recovery implementation, runtime behavior, API,
schemas and all legacy implementations remain unchanged from that reviewed head.
This work is governed by ADR-0004's existing controller boundary and ADR-0007's
exact-revision evidence/trust rules. No CI guard, timeout, workflow or gate is
relaxed, and no new dependency, abstraction or file-size exception is introduced.

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
binds to `06b16d5`. `go` and `evidence` fail on the registry guards. Final job
conclusions, timeout stacks and new-head results are recorded on the PR when
available; no focused probe is counted as full race acceptance.

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

Execute the [operator acceptance runbook](operator-acceptance.md) against `/app`,
using a legitimately provisioned compatible controller. It distinguishes
read-only isolated checks, permitted test execution and explicit human plan,
decision and publication authority. It requires genuine PR outcome and manual
accessibility evidence and supplies no fabricated approval.

The PR reports exact-head CI conclusions and a remaining-blocker table. Required
checks must pass before CI qualification; human acceptance must be recorded
before cutover. Production restart/cutover, merge, self-approval, publication and
legacy retirement are not performed by this batch.
