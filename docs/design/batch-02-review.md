# Batch 02 reconciliation — independent re-review

PR #594, review #5514572230 required current-main workflow parity, pending-plan pagination,
routing-safe skip focus and conflict resolution. This single reconciliation integrates main
`e6d90b9454ebb125cc1252eb363e7a147fec9a75` into the original `a150cc2` branch.
The published exact head and CI are reported on the PR to avoid a self-referential commit hash.
**Independent re-review requested. Production cutover and merge remain unapproved.**

## Review findings addressed

| Finding | Change and evidence |
| --- | --- |
| P0 current-main browser parity | Existing governed objective intake/revision/approval, typed decisions, publication human evidence and run controls now compose into React. Actual controller test submits these workflows; the complete matrix is in the inventory |
| P1 pending plans beyond 50 | Bounded plan pagination is visible on mobile, not nested inside the hidden desktop list. Selected exact routes retain offset; browser reaches pending request at offset 50; persisted plan index test reaches pending request after 101 attempts |
| P2 skip link/hash routing | React intercepts only skip activation, preserves destination and focuses/scrolls main. Native fixture anchor remains intact. Actual login/route keyboard test verifies URL unchanged and main focus |
| Conflicts with main | Both conflicts reconciled retaining main's PlanSummary/PlanIdentity reads and all schema contracts, plus React identity/event reads. Both UIs and main's controller workflows remain |
| Qualification | New real-controller browser proof, existing stop/rejection OS-process restart proof, legacy suite, responsive captures, contrast/keyboard/accessibility-tree evidence and bounded-load fixtures. Manual speech/operator acceptance remains an explicit release gate |

## Implementation and invariant review

ADR-0004, Planning v0.1 and WorkGraph govern this change. Existing server-owned read projections,
controller role, same-request controller binding, exact assignment/request/candidate/contract checks,
SQLite journal and scheduler remain authoritative. No new runtime, scheduler, policy, workflow
state machine, persisted format, dependency or production frontend listener was introduced.

The shared `GovernedAction` owns one frozen local confirmation/submission, exact receipt reads and
focus. Publication approve/reject share one owner so an unknown write closes both alternatives.
API reads remain separate from local selection and governed action results. Prospective plan
assignments and live reachability are re-read before review; the server checks authority afresh
at submission. Preview's intentionally empty controller binding is paired with a fresh observed
binding, rather than changing the existing read-only controller contract.

Focused source/render inspection corrected an optional zero wall-budget wire field, GET/SSE pause
projection mismatch, mobile pager placement and an action button slipping below bottom navigation.
The existing mobile threshold was retained; concise context/copy restored first-screen controls.
The canonical lifecycle remains separate from proposal/governing revision state. No task-count
completion percentage or fixture measurement is presented as runtime assurance.

Presentation interfaces satisfy §6(c), the requested reusable component foundation. HTTP decoding
uses the existing §6(b) transport boundary. `main.tsx` (501), `pages.tsx` (530) and `workgraphs.tsx`
(535) crossed the 500-line warning zone; they were reviewed for cohesive shell/page/graph ownership.
They remain below the review/hard-cap zones; no artificial splitting or size exception was used.
The bounded graph-request reduction and journal scans document their ceilings and indexing path.

## Validation evidence

- Strict TypeScript/esbuild, Go build/vet/gofmt and Windows build/vet pass.
- Affected Control Plane race tests pass, including real HTTP/schema/privacy proofs, pagination
  across request and graph windows, receipt retrieval beyond 500 events and shared GET/SSE pause state.
- Schema compilation includes all 38 schemas (24 Control Plane contracts); no new contract was
  omitted to satisfy the inherited count assertion.
- `TestReactEngineeringOSGovernedWorkspace`: 10 actual-controller checks, native owner-only socket,
  attested test binary/role and SQLite journals. Objective creation remains unapproved; stale
  assignments refuse, fresh approval has a durable receipt; revision remains unapproved; typed hold
  resolves durably; publication response lost after one real human-evidence event reconciles without
  resend. Unowned resume/refresh refuses; controller-owned resume and successor refresh succeed.
  No PR-observed event exists: the test never publishes or merges. External forge/producers are
  explicit offline test inputs, not production measurements or operator acceptance.
- Existing `TestEngineeringOSPlanRejectEndToEnd` normal/lost-response browser variants pass through
  controller disconnect/reconnect, reconstructed reader/socket/role and actual process crash/restart.
  Fresh browsers recover rejection/cancellation without another write.
- Preserved `TestBrowserWorkspace`: 26 legacy browser checks pass against the integrated main.
- Maintained presentation suite: 76 captures, 1440/1920/390, both themes, negative states, keyboard
  drawer/dialog/Escape/return and historical receipt focus, contrast/names and reflow. It also checks
  360/720 layouts and expanded text spacing. Actual app captures remain distinct from HTTP fixtures.
- Bounded HTTP load fixture: 2000 logical objective identities, a decoded 1200-WorkUnit graph,
  10000 logical event records; 50 visible nodes/events. Graph initial render measured 30 ms against
  a 5000 ms bound on this host (browser overhead included). This is not production latency.
- Frozen `/design`: 39/50 PNGs are byte-identical. Four receipt clocks differ; five images have
  at most 35 changed raster pixels; two old full-page images repeat sticky chrome at the bottom
  (inspected capture artifact). Two runtime images have intentional data/layout changes and are
  excluded from fixture equivalence. The pixel report is committed; no runtime baseline is self-approved.
- 21 Python checker regressions pass. File-size and frozen-baseline verification pass.

The former `a150cc2` file-size failure was real: `runtime/event_payload.go` had 1580 lines against
1573. Integrated main already extracted cohesive runtime payload responsibilities and retired the
stale baseline entry. This batch inherits that upstream repair; it does not increase ceilings,
add an exception, change the checker or waive a gate. Required GitHub `go` remains mandatory.
Full-suite testing is left to exact-head GitHub CI, with status reported separately on the PR.

## Screenshots and remaining acceptance

[Latest actual application captures](screenshots/reconciliation/README.md) contain desktop/mobile,
light/dark, mobile collapsed/expanded confirmation, durable decision/publication receipts and
Chromium accessibility-tree evidence. Labels explicitly identify isolated controller test data.
[Full supported-operation and deletion matrix](frontend-inventory.md) names every legacy file and
compatibility destination. **No legacy template/CSS/JS/route/test has been removed.**

Manual VoiceOver/NVDA speech, native high zoom and policy-specific selected_option/text decision
walkthroughs still need an independent operator. The full accessibility tree/keyboard/contrast
checks are qualification evidence, not WCAG certification. Production-scale projection latency,
a human-approved runtime visual baseline and operator acceptance are outstanding. Planning and
resume/refresh responses have no invented client retry/idempotency authority: ambiguity stays
closed for canonical inspection. The existing HTTP write deadline can precede a long planner reply.

Ready for independent source re-review of this reconciliation; **not ready for production cutover**.
No merge, self-approval, production restart, real publication or legacy retirement is authorized.
