# Zenchron visual design checkpoint

Status: **Action Required design accepted across desktop/mobile, dark/light and negative states; identity frozen; React batches 01–02; operational acceptance and legacy retirement pending**.
`/design` retains the fixture checkpoint. `/app` implements the four destinations using React/TypeScript,
shared CSS and the existing Go listener. Its first live workflow is governed plan rejection.
Approval/publication remain disabled; this is not complete operational parity or legacy retirement.

Open the existing Go Control Plane at `/design` after its usual token sign-in:

```sh
go run ./cmd/zenchron-engineering control-plane --state-dir /absolute/path/to/existing/state
# http://127.0.0.1:8787/design
```

The usual legacy sign-in page is deliberately retained during this checkpoint.
Prototype routes share its listener/token/method/path protections. `/design` reads no runtime
measurements and sends no actions; CSP explicitly forbids connections. The [React `/app` batch](runtime-integration.md)
uses the existing bearer API and governed rejection; its state never falls back to fixtures.

| Experience | Browser destination | Interactions |
| --- | --- | --- |
| Engineering overview | `/design#/overview` | Overlapping activity dimensions, grounded coverage, human queue, workers, recent results |
| Objectives portfolio | `/design#/objectives` | Title/issue/blocker search, lifecycle/product filters, combined no-results state, reset, objective drill-down |
| Objective workspace | `/design#/objectives/OBJ-612` | Lifecycle, selectable WorkGraph nodes, separate Plan/Evidence/Activity routes, worker provenance, bound publication request |
| Action required | `/design#/actions/OBJ-612` | Type filters, decision selection, exact binding, confirmation/rejection, closed technical disclosure, pinned confirmation, refusal states, local outcome and reset |

Additional scope sketches cover Products, Execution agents, Controller health and
Diagnostics inside this same shell. They are not claims of production behavioral parity.
OBJ-612 is the complete lifecycle specimen; other fixture objectives have contextual
summaries and navigation. This deliberate boundary is visible in the interface.

## Design system: factory floor

The reference contributes near-black surfaces, compact telemetry and technical structure.
Zenchron's identity comes from **evidence before authority**, a six-part operational flow,
bounded execution envelopes and exact-subject human decisions. The activity dimensions overlap; they are not exclusive objective lifecycle states. The flow is a display of
existing engineering semantics, not a permanent set of kernel agents or a rigid scheduler.

- Flagship dark: charcoal `#0e1010`, panel `#151818`, hairline grid/borders;
  warm orange `#ff9b62` for attention, muted green `#afcba0` for established outcomes.
- Light: warm neutral canvas `#f4f4ef`, near-white panels; darker functional orange
  `#954010` and green `#3a612c` preserve contrast instead of reusing dark colors blindly.
- System sans for titles, narrative, navigation and actions. Monospace is reserved for
  identifiers, exact bindings, times, metrics and compact technical annotations.
- Spacing tokens: 4/8/12/16/24/32 px. Restrained 3–5 px corner radii, no marketing
  cards, background illustrations, animated radar or artificial live indicators.
- Reusable components: shell/nav, page heads, panels, semantic tables, badges, meters,
  scoped coverage graphs, graph nodes/inspector, paginated timelines, binding lists, native disclosures and dialogs.
- Dark and light use one token set and the same layouts/components. No theme-specific
  fork, dependency, network font, icon library or build pipeline.

Implementation responsibilities:

| Source | Responsibility |
| --- | --- |
| `tokens.css` | Theme, typography, spacing and state tokens |
| `layout.css` | Shell, hierarchy, desktop breakpoints, deliberate mobile shell |
| `components.css`, `components.js` | Shared components and accessible presentation |
| `fixtures.js` | Explicit synthetic data only; never genuine observations |
| `overview.js`, `portfolio.js`, `workspace.js`, `actions.js` | Four experience compositions |
| `app.js` | Frozen fixture checkpoint shell and local simulation state; no runtime calls |
| `frontend/src/`, generated `react.js` | React shared components, typed API boundary, owned reads and exact-subject rejection |
| `controlplane/web_design.go` | Authenticated embed delivery on the existing listener |

See the [reusable component and state inventory](components.md) and [observed-data contract](data-sources.md).

These small presentation functions are requested reusable components (§6 clause c),
not new domain interfaces. No new dependencies, deliberate algorithmic shortcuts, database migrations, policy changes or
backend action additions are introduced. The typed HTTP boundary satisfies §6(b); existing API
contracts and bearer authentication remain unchanged.

## Mobile and failure states

At 390 px, permanent bottom navigation replaces the desktop sidebar; More exposes
supplementary destinations. Objectives become stacked semantic records with title,
activity state, product, worker, blocker and observed handoff/review counts. The overview surfaces a compact human
attention summary first. WorkGraph branching stays parallel; it is not rendered as a
fictional serial workflow. Decision selection uses a compact native chooser above the
detail so an operator need not scroll through the inbox to change the exact request.
Approval/rejection controls appear in the first mobile viewport. Full technical binding stays in a
closed disclosure; confirmation repeats context and keeps its buttons visible when details expand.
Exact hashes wrap; wide evidence/plan tables retain contained, keyboard-accessible scroll.

The Preview state control exposes empty, loading, error, disconnected, stale, permission-refused,
failed-execution and scale scenarios
on every main view. Errors have bounded public explanations and recovery; no unknown
fact becomes a pass. Disconnected/stale retain historical fixture context and disable
decisions. Loading announces busy state, and empty does not silently start work.
The session-local decision reset and scenario recovery controls are explicitly simulations.

## Accessibility scope

The prototype implements WCAG 2.2 AA design requirements: semantic navigation/landmarks,
skip link, headings, accessible names, status announcements, full keyboard operation,
visible focus, native modal focus containment/Escape/return, reduced motion, text contrast,
non-color state labels, exact-subject reflow, graph button alternatives and chart data.
Interactive controls use at least 24 px targets or permitted inline-link spacing; mobile
primary actions use 44 px targets. Sticky navigation has scroll margins and document
padding so focused actions can be brought into view above it. There is no automatic
timeout or animation and no drag-only action.

The browser check measures **rendered text** contrast in both themes and verifies names,
reflow and key interactions. This is useful evidence, **not a WCAG conformance
certification**. Screen-reader testing, high zoom/text-spacing and production login/error
accessibility remain integration acceptance checks. The retained legacy login is not
being represented as a newly qualified accessible screen.

## Captures and revision

`screenshots/` contains actual browser captures for all four views at 1440×1080,
1920×1080 and 390×844. The corresponding `-full.png` images preserve all scroll content.
Additional captures cover all four light views, system/decision refusal states, failure context and expanded mobile binding.
`validation.json` records the current interaction and contrast results.
The [rendered gallery](screenshots.md) links the fixture captures and separately labeled isolated-runtime captures, including exact-subject
confirmation dialogs. Mobile confirmation bodies scroll while their action footer stays visible.

The initial prototype was revised after screenshot inspection. The human accepted its visual direction
and desktop information-architecture direction while keeping production design pending. This focused
refinement responds to that review:

1. Compact mobile decisions put consequence, restrictions and exact context before immediately visible buttons.
2. Shared native disclosures preserve complete bindings; confirmation keeps its action footer in view.
3. Percentage completion, fabricated throughput/trends and capacity ratios are removed. Scoped counts
   and honest Unavailable/Unknown states replace them; source gaps are documented.
4. Human authority requests are separated from runtime-owned bounded recovery. A failed plan is
   explicitly failed and does not restart automatically.
5. Secondary type has a 12 px floor; expanded evidence uses progressive disclosure. The preview
   navigation no longer links to the legacy dashboard, whose implementation is deliberately retained.
6. Shared 50-record pages support a 500-objective / 10,000-event scale fixture, including search and
   exact objective navigation. This measures browser interaction, not runtime-store performance.
7. Subsequent human review accepted the desktop decision composition. Neutral “Simulated” indicators
   replace green fixture-verification cues; request metadata is clearer; a compact recovery link is
   visible beside the filters. Confirmation names the protected action, and rejected/resolved receipts
   retain context without allowing duplicate submission.
8. A first refinement capture was inspected, then revised again for mobile chart-label readability
   and additional space above bottom navigation. Final captures include refusal/failure and expanded
   binding/confirmation states. Keyboard, contrast, reflow and fixture isolation checks were rerun.

Run the browser interaction/capture check with an **already-installed** Playwright module
and Chrome executable. No install command or new project test framework is required:

```sh
ZENCHRON_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs \
ZENCHRON_CHROME_EXECUTABLE=/absolute/path/to/chrome \
ZENCHRON_PREVIEW_TOKEN_FILE=/absolute/path/to/isolated-preview-state/control-plane.token \
node scripts/check_design_preview.mjs
```

Use an isolated empty state directory for captures; it requires an existing initialized
runtime database because the production command is read-only. Do not seed operational
fixtures into a real runtime store. Token contents are read into the test browser only,
never printed. Headless Chrome uses an isolated temporary profile, not the operator's.

## Human visual checkpoint

Review the actual browser prototype, not just test results. Review the focused changes to mobile decision ergonomics, data semantics, typography, failure
states and reusable foundations. The design gate is accepted; operational replacement acceptance
remains a separate decision. Real governed rejection is implemented against isolated test state;
approval of fixtures alone does not establish production
behavioral parity or authorize legacy deletion.

The [migration and deletion inventory](frontend-inventory.md) names every current page,
handler, asset, test, route, authentication dependency and API gap. It defines the
operational acceptance/removal gate and compatibility mappings. **No legacy code was
deleted and no complete frontend migration was performed.**
