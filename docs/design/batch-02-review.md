# Implementation batch 02 review

Date: 2026-10-14. Base: `f898cfd469ec9f831f3fdc46fd5e9234bb79975e`.
The final commit and its exact-head rerun results are reported separately to avoid a self-referential hash.

The first batch left standalone operational workflows in the old console. Batch 02 implements them
inside the shared React shell: Runs/activity, WorkGraphs, Products, agent bindings, controller
findings and login. Plans gain revision selection, gate references and bounded journal history.
Overview gains canonical fleet aggregates with explicit independent scope and unknown capacity.
Existing stop and plan rejection are the only HTTP actions; no authority system or dependency changed.

## Reviewed changes

Ponytail review traced the routes, caller signatures, transport/authentication, read projections,
controller proofs and durable effects. It corrected:

- Policy reference revision is a string, matching the Go DTO; an initial browser failure reproduced
  the mismatch before correction.
- Long mobile histories and 50-node lists buried controls/inspection; history now scrolls within a
  bounded panel, run control appears early, and mobile graph selection uses one selected card.
- Narrow identity columns fragmented hashes and repository names; factual aside values now use the
  full column width. The expanded stop confirmation keeps its footer visible.
- Shared source matching treated issue zero as a non-issue match. The regression failed first, then
  passed after requiring a positive issue in the common API/legacy rule.
- Gate DTOs carrying an invented run and invalid cursor/dependency/subject responses now fail closed.
- Runtime CSS selectors are scoped so the frozen fixture composition remains unchanged.

New modules are cohesive, with no backend interface or speculative framework. Shared presentation
satisfies §6(c); HTTP transport/wire validation is the existing §6(b) boundary. One `ponytail:` corner is explicit: filtering the cached full fleet before paging is intended for
fewer than about 10,000 runs; measured latency would require a canonical read index. Existing bounded
literal-search and per-process-action-lock comments remain.
No source was pushed past 500 lines; the already-warning `controlplane/api.go` is now 653 lines.
No baseline/exception or CI gate was altered.

## Validation evidence

The final rerun uses the current commit's embedded bundle, not a stale running binary:

- Strict TypeScript/esbuild; Go build/vet/gofmt; focused race tests for affected read/auth/action paths.
- Actual controller/browser normal and discarded-response rejection/stop, exact subject/controller,
  one journal effect, failed reconciliation remaining closed, reader/socket/role reconstruction,
  actual controller OS-process crash/restart and fresh-browser durable recovery.
- Actual login through the Go bearer boundary: refused token never becomes a session, destination
  preserved, no credential in local storage or old asset imports.
- Maintained preview/React browser entry: 76 captures, including 50 original fixture views, two
  actual empty API views, six actual login views and 18 explicitly labeled HTTP projection fixtures.
- Real-controller driver: 71 captures on isolated test input across 1440/1920/390 and both themes,
  including historical/disconnected/reconnected/process-restarted and lost-response states.
- Focus containment/Escape/return, historical receipt focus, readable contrast/names, reflow and
  bounded record rendering. These are focused WCAG/visual regression evidence, not full certification.
- 21 Python checker regressions. The file-size checker remains red solely on unchanged
  `runtime/event_payload.go`: 1,580 against its frozen 1,573 ceiling. This is not waived or hidden.

Current-head full-suite CI is not available: no branch was published, PR approved or merge performed.
Production load, screen-reader/high-zoom qualification, an approved pixel baseline and operator
acceptance of the operational replacement remain open. The interactive app is `/app` on the existing
Go Control Plane; `/design` remains the frozen fixture checkpoint.

## Legacy removal gate and inventory

No legacy template, asset, route or test has been removed. [The complete inventory](frontend-inventory.md)
records the exact candidates, retained backend boundaries, compatibility mappings and parity status.
Retirement requires operator acceptance, followed by a separate concrete cutover that preserves
read APIs, governed controller actions and useful existing bookmarks. Tests passing does not supply
that human acceptance.
