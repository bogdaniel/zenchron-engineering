# Design and implementation validation

**Visual gate accepted. Operational parity and legacy retirement pending.** Successful fixture
simulation is not backend acceptance. Runtime proof below uses isolated deterministic test data,
the real governed controller path and durable journal, never an operator's production state.

## Focused evidence

- React strict TypeScript check and esbuild bundle pass (`npm run build` in `frontend/`).
  React/React DOM, compiler/types and esbuild are pinned; no extra state/router/test framework.
- Focused race-enabled control-plane tests pass for the new plan index/context, existing private
  wire boundary, authenticated preview/runtime delivery and relevant governed action behavior.
- Focused race-enabled CLI/controller tests pass for real plan rejection, same-request controller
  proof, plan-lock racing protection and stop. `TestEngineeringOSPlanRejectEndToEnd` uses actual
  controller composition, socket, controller role and real journal. Its browser submits one POST,
  preserves the selected revision, focuses the historical receipt and rehydrates it after controller
  composition/read-store/role/socket reconstruction. A discarded reply after the actual journal write and a failed reconciliation
  read remain closed; the later receipt read never resends the action. Changed digest/controller, unattested build,
  released role and disconnected pending request cases refuse without a journal decision.
- `scripts/check_design_preview.mjs` passes the frozen fixture interactions and 52 captures:
  50 explicitly synthetic design images and two genuine isolated empty-runtime API images.
  Synthetic 500-objective/10,000-event checks render at most 50 records. Their timings describe
  browser fixture interaction, not production throughput or runtime reduction cost.
- Its React helper verifies bearer transport, 50-row paging across a 500-row HTTP contract,
  search, escaped text, gate/run separation, pending/actionable distinction, changed-subject
  review refusal, independent identity/reachability, stale observation handling under a controlled
  clock, permission/malformed/mismatched/disconnected reads and historical receipts. These HTTP
  mocks are presentation evidence only and are not captured as genuine runtime state.
- 37 further browser captures exercise the actual isolated controller flow at 1440, 1920 and
  390 px in both actual themes. Captures visibly say isolated test data. Fresh documents prevent
  stale Chrome compositor images; selected theme is asserted before capture. Metadata is in
  `screenshots/batch-01/{reject,disconnected,reconnected}-validation.json`.
- Keyboard proofs cover modal containment, Escape to the exact opener, receipt focus, closed
  mobile-sidebar inertness, drawer containment and return, and route-preserving recovery inspection.
  First-screen mobile decision buttons and expanded-binding confirmation footer remain visible.
- Rendered text contrast/name checks cover all four dark/light fixture views and all four
  dark/light React compositions. Reflow/text spacing are covered in the frozen preview. These
  focused checks do not constitute a complete WCAG or screen-reader qualification.
- Actual screenshots were inspected and revised for mobile action placement, focus return,
  disabled light-button contrast and closed drawer shadow. Visual regressions are checked with
  layout/reflow/visibility contracts; an operator-approved pixel baseline is not yet established.
- `go build ./...`, `go vet ./...`, `gofmt` and 21 Python checker regression tests pass.
  The sandbox denied an optional module-version stat-cache write; Go build returned success.
  Task-specific Go build cache lives under `/private/tmp`.

## Exact source evidence

The batch is recorded as a local commit. Its exact head is reported with the final rerun rather
than embedding a self-referential commit hash here. Final browser output can be directed to temporary
artifact directories without modifying reviewed captures:

```sh
# Existing Playwright/Chrome paths are environment-owned, not project dependencies.
ZENCHRON_LIVE_SCREENSHOTS=/private/tmp/zenchron-head-live \
ZENCHRON_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs \
ZENCHRON_CHROME_EXECUTABLE=/absolute/path/to/chrome \
go test -race ./cmd/zenchron-engineering \
  -run 'TestEngineeringOSPlanRejectEndToEnd|TestGovernedPlanRejectIsDecidedOnceServerSide|TestGovernedStopProvesTheControllerOnTheSameRequest|TestControlPlane' -count=1

ZENCHRON_PREVIEW_SCREENSHOTS=/private/tmp/zenchron-head-preview \
ZENCHRON_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs \
ZENCHRON_CHROME_EXECUTABLE=/absolute/path/to/chrome \
ZENCHRON_PREVIEW_TOKEN_FILE=/absolute/path/to/isolated-state/control-plane.token \
node scripts/check_design_preview.mjs
```

Use the existing Go Control Plane listener built from that head. No remote PR, publication,
production approval or merge was performed. Full-suite/current-head CI has not been run locally;
it remains the GitHub CI gate.

## Existing repository completion failure

`python3 scripts/check_file_sizes.py` fails on **unchanged** `runtime/event_payload.go`:
1,580 lines against the frozen 1,573 ceiling. The starting HEAD also has 1,580 lines and there is
no task diff for that file or its baseline. This was not hidden, waived or repaired by unrelated
runtime changes. All added source files remain below 500 lines; no changed source crossed 500 and
no size exception was introduced. `controlplane/api.go` was already in the warning zone and gained
two registrations. The repository completion gate remains red until the separate overage is resolved.

## Review and remaining gates

Ponytail review traced the real read/auth/action/controller/store flows and their callers. It
corrected confirmation opener capture, modal/drawer containment, hash navigation for in-page
recovery, stale-state action counts, unnecessary coupled reads and light disabled-button specificity.
The server action allowlist, same-request proof, role/plan lock, journal, private-text boundary and
uncertain-write reconciliation remain authoritative. No backend workflow interface or state machine
was added; presentation satisfies §6(c), HTTP integration §6(b).

Full objective/public-title and product scope projection, WorkGraph association, complete plan
activity, governed approval bindings, remaining operational screens/login, real process-level restart
qualification, production-load performance and operator acceptance remain open. Controller
reconstruction evidence above is not represented as an operating-system process-restart test.
The [complete legacy-removal inventory](frontend-inventory.md) is retained. No obsolete Go-template
page, static asset, old navigation route or template test has been removed.
