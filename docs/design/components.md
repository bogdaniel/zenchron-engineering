# Reusable presentation foundation

The four fixture experiences retain their shared components, themes and responsive shell.
The React implementation reuses the same CSS and provides shared typed components and a governed
rejection workflow. Complete operational parity remains pending.

| Component / owner | Reuse | Behavior and accessibility |
| --- | --- | --- |
| Theme/type/spacing tokens | Every screen | Shared CSS variables; 12 px secondary floor, 13–14 px body narration, explicit light colors; system sans and technical-only monospace |
| Application shell / `app.js`, `layout.css` | Every destination | One navigation, skip/main landmarks; desktop sidebar and mobile bottom bar with inert, keyboard-contained More drawer |
| `pageHead`, `panel` | All four experiences and system sketches | Consistent hierarchy, compact headings, contextual actions; no duplicated page-specific card styles |
| `badge`, `icon`, `link`, `emptyState` | All four experiences | Textual state labels, decorative SVG hidden from assistive technology, escaped labels and safe hash destinations |
| `objectiveRows`, `groundedProgress` | Overview and Portfolio | One semantic table becomes mobile records without losing headings; exact blockers link to human context only when a request exists |
| `pageWindow`, `pageControls` | Portfolio and full Activity | At most 50 records rendered; named Previous/Next groups and totals. Search spans the synthetic index, not just the visible page |
| `disclosure` | Overview metric explanation, evidence binding, inbox binding and confirmation | Native details/summary, default closed, keyboard accessible; exact cryptographic context remains reachable |
| `budgetMeter` | Workspace envelope | Actual consumed/ceiling ratio with meter semantics; never objective percentage completion |
| `coverageChart` | Overview | Scoped counts in readable HTML, paired with decorative SVG bars; no invented time series |
| WorkGraph nodes / inspector | Lifecycle and WorkGraph routes | Same dependency graph, semantic selection buttons and text inspector; branching remains parallel on mobile |
| `activity` | Workspace lifecycle summary and Activity route | Shared causal timeline; full history paginated, recent summary compact |
| `subjectBinding` | Workspace evidence, publication request and confirmation | Same exact fixture candidate/tree/contract/policy/controller context; content hidden only by disclosure, never removed |
| `decisionChecks`, `fixtureActionBlocker` | Inbox and confirmation | Consistent context and explicit eligibility refusals; only local fixture simulations, never server authority |
| `openDialog` / native dialog | Decisions, worker provenance and search | Visible focus, Escape/return, visible-control keyboard wrap; decision footer fixed while body scrolls |

The fixture compositions remain the frozen checkpoint. Runtime implementation now uses React
shared components in `frontend/src/components.tsx`, the authoritative CSS above, and one typed
API boundary in `frontend/src/api.ts`. Local route/filter/selection/confirmation state stays separate
from observed server DTOs; no local state transition grants authority. The superseded read-only
`api.js`/`runtime.js` adapters were removed. Reusable presentation satisfies repository §6(c),
the explicitly requested component foundation; the HTTP boundary satisfies §6(b).
See [the precise React component and state inventory](runtime-integration.md).

## Interaction states

| Preview state | Default presentation | Decision behavior |
| --- | --- | --- |
| Connected fixture | Clearly labeled synthetic snapshot | Review/Reject open a bound fixture-only confirmation |
| Empty | Contextual empty state, explicit exploration | No requests or implicit execution |
| Loading | Busy announcement and skeleton; explicit fixture completion | No decision controls |
| Error | Bounded read failure, diagnostics and explicit retry | No success-shaped empty data |
| Disconnected | Historical retained snapshot, reachability warning | Refused; controller authority cannot be established |
| Stale | Last-observation warning; history not current eligibility | Refused until exact subjects are refreshed |
| Permission refused | Explicit scope refusal and controller context | Disabled in inbox and confirmation; no escalation or implied retry |
| Failed execution | Retained candidate, assurance/publication withheld; failed plan stays failed | Disabled; causal activity and diagnostics available |
| Scale fixture | 500 objective records and 10,000 event metadata records | Same authority semantics; 50-record rendering bound |

The ordinary fixture verifies eligibility only for its two synthetic human requests. The shared
blocker also handles absent/unverified eligibility. Production must replace this with server facts,
subject-specific refusals, independent freshness and governed action responses; the design checkpoint
has deliberately not built that integration. See [the data contract](data-sources.md).

## Mobile decision anatomy

Neutral Simulated labels distinguish preview checks from real assurance. Request metadata uses
stronger secondary contrast and 13 px type; recovery has a compact link beside the filters.

Default: request type → action title → consequence/restrictions → concise subject → three checks →
Reject / Review & approve → closed technical binding. The two buttons fit the initial 390×844 viewport.
A native chooser changes request without scrolling through the desktop list. Bounded recovery follows
as a separate read-only section.

Confirmation: same request ID/objective/candidate or plan, same consequence and restrictions, technical
disclosure, optional note and persistent Cancel/Confirm footer. Expanding full bindings never displaces
the footer; the body scrolls. Protected action and target appear in confirmation by default. Changed bindings are refused;
rejected/resolved receipts retain the subject and cannot be submitted again. No local confirmation
authorizes a material runtime outcome. Real approval needs complete governed assignment/evidence bindings and remains disabled. The first
live workflow is governed plan rejection, with server-side exact-subject verification and durable receipts.

The browser check verifies first-screen mobile buttons, exact confirmation context, closed/expanded
bindings, disabled scenarios, keyboard focus and local filter preservation. Rendered screenshots and
human review remain required; these structural checks alone do not establish production acceptance.
