# Operator-usability rehearsal (#481)

Level 2 evidence: a real Chrome (Playwright, 1440 px wide) drove the real
composition root, SQLite, controller socket, supervisor, plan reconciler and
HTTP service. Only the providers, the forge and the agent CLIs' help output are
fake; no live provider credit was used, no real GitHub repository was touched
and no production controller was involved. The clicks are automated. They show
the actions exist and work, not that a person found them easy. Real operator
judgement is the #344 Arm A itself.

Run against a local integration of main `d4f3a37`, #612 and #598 with the
fixes below. Rerun: `ZENCHRON_OPERATOR_REHEARSAL=1
ZENCHRON_PLAYWRIGHT_MODULE=<playwright-core/index.mjs> go test
./cmd/zenchron-engineering -run '^TestOperatorRehearsal$' -count=1 -v` (about
4.5 minutes). Two full runs were green.

`rehearsal-results.json` holds the per-step results as the driver recorded them.
Where its automatic status differs from the reviewed verdict below, the verdict
below governs (step 7).

| # | Step | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | Open the product | PASS | `01-product-workspace.png` |
| 2 | Create an objective | PASS | `02-objective-created.png` |
| 3 | Inspect the proposed plan | PASS | `03-proposed-plan.png` |
| 4 | Approve through the governed UI action | PASS | `04-approved.png` |
| 5 | Inspect the WorkGraph | PASS | `05-workgraph-detail.png` |
| 6 | Assignments, dependencies, concurrency | PASS | `06-graph-concurrent.png` |
| 7 | WorkUnit budget and attempts | PARTIAL, fixed (#631) | `07-budget-attempts.png` |
| 8 | Admitted handoff and review outcome | PASS | `12-review-stage.png` |
| 9 | Find the open DecisionRequest | PASS | `09-action-required.png` |
| 10 | Resolve it | PASS | `10-decision-resolved.png` |
| 11 | Dependent unit proceeds | PASS | `11-graph-all-complete.png` |
| 12 | Assurance, candidate, PR, completion | PARTIAL | `15-objective-final.png`, `16-integrator-pr.png` |
| 13 | Agents page | PASS with gaps (#632) | `13-agents.png` |

Token and cost rendering were not exercised: the fake providers report neither.
Step 7 does show that an unrecorded lifecycle bound reads "Not recorded", not 0.

## Defects found

| Issue | Finding | Disposition |
| --- | --- | --- |
| #626 | Agents page had no level-1 heading; existing browser suite failed | fixed on #598 |
| #627 | Action required never listed a unit's run awaiting human publication authority (blocking) | fixed, PR #634 |
| #631 | Budget ceilings read as consumption | fixed on #612 |
| #628 | A plan-stage run (for example the reviewer) awaiting authority is undiscoverable | open, not blocking |
| #629 | Objective page shows no PR or evidence summary and reads "completed" before publication | open, not blocking |
| #630 | Run page says "Authority request: Not recorded" beside live authorize buttons | open, not blocking |
| #632 | Agents page: no ineligibility reason, no profile-to-agent relationship, no model | open, not blocking |
| #633 | Existing React browser suite is flaky at a focus assertion | open |

## Reading

The operator supervised plan approval, concurrent execution, a worker-asked
human decision, dependency release, the independent review verdict and the PR
from the Product Workspace with no terminal. #629 and #628 are discoverability
gaps: the operator must know to look at the run page. They are expected to cost
supervision minutes in Arm A and are measured there, not hidden.

## Final run on merged main

Re-run on main `b02b8a4` (#625, #598, #634, #612 merged) with the harness from
PR #636's successor branch `test/481-operator-rehearsal-harness-main`: result file
`rehearsal-results-main-b02b8a4.json`. Steps 1-11, 13, 15 and 16 pass, including
step 7 on the merged budget panel. Steps 12 and 14 stay PARTIAL: #629 (objective
page has no PR/evidence summary) and #628 (a plan-stage run awaiting authority is
undiscoverable), both open and non-blocking. Screenshots above are from the
earlier pre-merge integration run; the final run's artifacts stay in the
operator's scratch directory and are not committed.
