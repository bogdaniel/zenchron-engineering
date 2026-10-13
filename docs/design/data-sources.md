# Observed state and measurement contract

Industrial visual direction is accepted. Production design and live integration are pending.
All `/design` values are explicitly synthetic; `/app` is now a separate observed-data mode of the
same application, described in [the first runtime slice](runtime-integration.md); this audit is code inspection, not a runtime observation.
No API or governed action is added by this checkpoint.

## Activity dimensions, not an exclusive funnel

An objective can own several agent stages and issue-backed WorkUnits, with execution, review,
authority and publication active concurrently. Plans and WorkGraphs remain separate artifacts.
The six Overview dimensions must name their **entity, scope and observation time**. They must
never be summed into a total, converted into completion, or treated as exclusive objective states.

| Display | Existing source / defensible interpretation | Production boundary or gap |
| --- | --- | --- |
| Intent | No objective portfolio list in the current `/v1/` API | A bounded, sanitized objective index is needed; do not invent titles from private plan reasoning |
| Unapproved plans | `/v1/plans/{id}/detail`: `shown`, `revision`, `governing_revision` | Individual revision available; fleet plan index needed. Proposed and governing revisions can coexist |
| Executing runs | `/v1/runs`: `runs[].executing`, `agent` | Count distinct run IDs in a complete snapshot, not objectives or completed stages. Partial pagination must be labeled partial |
| Awaiting assurance | `/v1/runs/{id}`: phase/disposition, optional assurance and causal state | Require an explicit eligible phase/disposition interpretation; missing assurance is unknown, not pending or passing |
| Human authority | Run `authority_request` missing/stale claims; WorkGraph `units[].open_decisions` / `hold`; plan approval boundary | Separate request kinds; no complete human inbox projection today. Technical failure alone never creates a human request |
| PRs observed | Run `pull_request.number/state/merged/stale` and run-list PR facts | Distinct `(repository, PR number)` identities in the current snapshot; not an accepted-change or 7-day throughput series |
| Admitted handoffs | `/v1/workgraphs/{id}`: `revision`, `units[].output.handoff_id`, candidate revision/tree | Count current admitted outputs over current units for one pinned graph revision; invalidated outputs cannot be silently counted as current |
| Reviews completed | WorkGraph `requires_review`, `review_approved`, optional `review.candidate/verdict` | Use authoritative `review_approved` for applicable units; latest verdict is supplementary. No review requirement means “not applicable,” not 100% |
| Run elapsed time | Run `elapsed`, `active_elapsed`, `external_wait_elapsed` | Separate run wall/active/wait durations, not an objective lifecycle duration |
| Budget consumed | Plan detail `budget.child_runs/max_child_runs`, `provider_invocations/max_provider_invocations`, `wall_seconds/max_wall_seconds` | A consumed-budget meter is defensible; it is never engineering completion. Missing/unverifiable ceiling means no ratio |
| Cost | Plan detail `budget.cost_known`, optional `cost_micros` | Unknown stays unknown; never insert zero or sum a partially known fleet into an apparently complete cost |
| Workers / capacity | Run worker identity and executing counts; plan bound agent/provider/trust fields | Installed readiness and per-agent ceilings are not exposed. Internal `Fleet.capacity == 0` can mean unknown and has no registered fleet route |
| Controller | `/v1/controller` independent dimensions | Preserve reachability, serving, role/admission, consistency and generation match; one green “healthy” badge cannot replace them |
| History | `/v1/runs/{id}/events`: sequence, type, occurred_at, next_after, has_more; stream | Fetch bounded pages per exact run. Do not manufacture rich event prose from sanitized metadata |

The fixture replaces objective percentage bars with admitted handoff counts, completed review
counts, or awaiting plan approval. Counts demonstrate delivered artifacts and applicable review
coverage; they do **not** estimate remaining effort. Changing graph revision changes the denominator.

## Measurements deliberately unavailable

- **Accepted changes per human supervision hour:** requires an admitted acceptance record bound
  to exact revision and a measured supervision-time denominator. Neither a provider exit nor PR
  publication is acceptance. The current HTTP surface does not supply that aggregate.
- **First-pass acceptance:** requires a defined cohort, attempt/rework lineage and independent
  acceptance result. A current status percentage would silently discard rework and missing outcomes.
- **Objective cycle time:** requires defined intent-start and material-outcome endpoints, including
  revision/subject changes. Existing run durations cannot stand in for this metric.
- **Throughput / trends:** requires timestamped, deduplicated exact accepted subjects over a specified
  window and historical completeness. Current PR identities are a snapshot, not a time series.
- **Agent utilization:** requires installed-worker concurrency ceilings, leases and a common
  observation boundary. Active run count alone cannot establish free slots or utilization.

The preview shows Unavailable/Unknown with a concise reason. Production must preserve those states
until the source and aggregation are reviewed; it may not substitute fixture series on a failed read.

## Read state, UI state, and authority

| Owner | Contents | Allowed behavior |
| --- | --- | --- |
| Observed projection | Fixture module in `/design`; validated typed API reads in `/app` | Render facts with identity, revision and freshness. Keep unknown, stale, failed, absent and refused distinct |
| Local UI state | `app.js`: route, theme, filters, pagination, selected node, disclosure and simulated outcomes | Search/select/inspect without changing engineering state. Changing theme preserves filters |
| Server authority | Existing governed controller services; future reviewed HTTP actions | Validate exact action/target, subject and request binding, current controller and eligibility; only server authorization permits mutation |

`fixtureActionBlocker` gates **only** browser simulation. It is checked both at opening and confirming
and refuses stale, disconnected, permission-refused, failed or unverified context. It cannot be reused
as a production authorization evaluator. All fixture actions are labeled simulations and CSP prohibits
runtime requests. Confirmation repeats request ID, objective, short candidate/plan, consequence and
restrictions; complete binding stays available in a native disclosure. Its buttons remain visible
while technical detail scrolls.

Existing run `authority_request` includes status/claims, but not the full request ID/digest or complete
controller/contract/policy binding shown by the fixture. Existing controller build metadata is not
that binding. Plan detail deliberately excludes objective/rationale/private reasoning and assignment
resolution. Production approval needs reviewed bounded projections and existing governed command
paths, not client reconstruction. HTTP currently permits only run stop and plan reject. Publication
approval, plan approval and decision resolution need explicit backend work before becoming actionable.

Runtime-owned handoff repair and evidence recheck appear separately from human authority. They may
proceed only within existing permission and consumed-budget limits. Failed plans are not automatically
restarted. Recovery requiring a new material decision must surface the controller's actual request;
the client must never manufacture one or grant a broader retry.

## Implementation and retirement order

The React plan-rejection batch is now implemented; the original read-only adapters were superseded. Continue with
reviewed source contracts and the complete four workflows: Overview → Objectives Portfolio → Objective
Workspace → Action Required through one typed API layer, one shell and the existing Go listener.
Build each projection/action with focused invariant tests. Until action eligibility is proven,
production buttons remain disabled or direct to the existing governed CLI.

Then consolidate Products, WorkGraphs, Runs/history, Agents, controller/diagnostics and all legitimate
operator actions using the [migration inventory](frontend-inventory.md). Replace browser tests and
map old bookmarks to new destinations. Remove templates, legacy JS/CSS, presentation handlers and
asset infrastructure only after behavioral parity and explicit production acceptance. The preview
has no legacy-console navigation link; the retained implementation remains available during migration.

The 500-objective / 10,000-event fixture verifies bounded 50-record rendering and browser interactions.
It does not establish runtime-store performance or justify loading an unbounded production history.
Production must use bounded server queries/pagination, stable IDs and cancellation for obsolete reads.

## React batch 01 source update

The source inventory above describes the pre-implementation audit. The new bounded `/v1/plans`
index and exact `/v1/plans/{id}/context` read reuse existing plan rows and the canonical reducer/journal.
They expose source identities, canonical pending state and historical receipt metadata, not private
objective prose. Read views remain independent snapshots. No throughput, historical trend, objective
percentage, agent ceiling or plan-to-WorkGraph association was introduced. Actual test controller
behavior and synthetic browser measurements are separately labeled in
[the implementation evidence](runtime-integration.md).
