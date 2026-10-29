# Rendered review gallery

Action Required design accepted across desktop/mobile, dark/light and negative-state presentations.
The design images below use explicit fixture data. Two separately labeled runtime images at the end
use genuine API responses from an isolated empty state, not a production fleet. The viewport images show the
first screen; full-page captures show the scrollable experience. Review these alongside
the interactive `/design` prototype; passing checks are not visual approval. Viewport and dialog
images preserve native sticky/fixed chrome. Full-page mobile captures temporarily place that chrome
in document flow to avoid Chrome repeating it across screenshot tiles; content and styling are unchanged.

| Experience | 1440 desktop | 1920 desktop | 390 mobile | Light |
| --- | --- | --- | --- | --- |
| Engineering overview | [Viewport](screenshots/overview-1440-dark.png) · [Full](screenshots/overview-1440-dark-full.png) | [Viewport](screenshots/overview-1920-dark.png) · [Full](screenshots/overview-1920-dark-full.png) | [Viewport](screenshots/overview-390-dark.png) · [Full](screenshots/overview-390-dark-full.png) | [Full](screenshots/overview-1440-light.png) |
| Objectives portfolio | [Viewport](screenshots/portfolio-1440-dark.png) · [Full](screenshots/portfolio-1440-dark-full.png) | [Viewport](screenshots/portfolio-1920-dark.png) · [Full](screenshots/portfolio-1920-dark-full.png) | [Viewport](screenshots/portfolio-390-dark.png) · [Full](screenshots/portfolio-390-dark-full.png) | [Full](screenshots/portfolio-1440-light.png) |
| Objective workspace | [Viewport](screenshots/workspace-1440-dark.png) · [Full](screenshots/workspace-1440-dark-full.png) | [Viewport](screenshots/workspace-1920-dark.png) · [Full](screenshots/workspace-1920-dark-full.png) | [Viewport](screenshots/workspace-390-dark.png) · [Full](screenshots/workspace-390-dark-full.png) | [Full](screenshots/workspace-1440-light.png) |
| Action required | [Viewport](screenshots/actions-1440-dark.png) · [Full](screenshots/actions-1440-dark-full.png) | [Viewport](screenshots/actions-1920-dark.png) · [Full](screenshots/actions-1920-dark-full.png) | [Viewport](screenshots/actions-390-dark.png) · [Full](screenshots/actions-390-dark-full.png) | [Full](screenshots/actions-1440-light.png) |

System states: [Empty](screenshots/state-empty-1440.png),
[Loading](screenshots/state-loading-1440.png), [Error](screenshots/state-error-1440.png),
[Disconnected](screenshots/state-disconnected-1440.png), [Stale](screenshots/state-stale-1440.png).

Bound confirmation: [Desktop](screenshots/confirmation-1440-dark.png),
[Mobile](screenshots/confirmation-390-dark.png),
[Mobile with expanded binding](screenshots/confirmation-390-binding-expanded.png).

Decision states:

| State | Desktop | Mobile |
| --- | --- | --- |
| Disconnected | [View](screenshots/actions-disconnected-1440.png) | [View](screenshots/actions-disconnected-390.png) |
| Stale | [View](screenshots/actions-stale-1440.png) | [View](screenshots/actions-stale-390.png) |
| Permission refused | [View](screenshots/actions-permission-refused-1440.png) | [View](screenshots/actions-permission-refused-390.png) |
| Failed execution | [View](screenshots/actions-failed-execution-1440.png) | [View](screenshots/actions-failed-execution-390.png) |
| Rejected, retained subject | [View](screenshots/actions-rejected-1440.png) | [View](screenshots/actions-rejected-390.png) |
| Already resolved, retained subject | [View](screenshots/actions-resolved-1440.png) | [View](screenshots/actions-resolved-390.png) |

Runtime reads, separate from fixtures: [1440 desktop](screenshots/runtime-overview-1440.png),
[390 mobile](screenshots/runtime-overview-390.png). These show the real isolated empty-state API response.
See [runtime integration scope and limits](runtime-integration.md).

Progressive disclosure: [Mobile full binding](screenshots/actions-390-binding-expanded.png).
Failed-run context: [Workspace](screenshots/workspace-failed-execution-1440.png).

[Browser validation record](screenshots/validation.json) ·
[Design system and checkpoint](README.md) · [Shared components](components.md) · [Data sources](data-sources.md) · [Migration/removal inventory](frontend-inventory.md).

## React batch 01

[37 additional captures and genuine controller-path evidence](runtime-integration.md) use explicitly
labeled isolated test data. They do not represent production fleet measurements. The accepted fixture
checkpoint above and real operational workflow evidence remain separate.

## React batch 02

[Reviewed implementation and source classifications](batch-02-review.md) accompany the current
runtime captures. These images use explicitly labeled isolated test inputs and actual Go read APIs;
they are not production observations. The separate `operational-http-fixtures` gallery exercises
larger graphs and histories through labeled HTTP mocks. Login images use the actual Go listener.

| Experience | 1440 dark | 1920 dark | 390 dark |
| --- | --- | --- | --- |
| Overview | [View](screenshots/batch-02/overview-1440-dark.png) | [View](screenshots/batch-02/overview-1920-dark.png) | [View](screenshots/batch-02/overview-390-dark.png) |
| Objectives | [View](screenshots/batch-02/portfolio-1440-dark.png) | [View](screenshots/batch-02/portfolio-1920-dark.png) | [View](screenshots/batch-02/portfolio-390-dark.png) |
| Objective workspace | [View](screenshots/batch-02/workspace-1440-dark.png) | [View](screenshots/batch-02/workspace-1920-dark.png) | [View](screenshots/batch-02/workspace-390-dark.png) |
| Action required | [View](screenshots/batch-02/actions-1440-dark.png) | [View](screenshots/batch-02/actions-1920-dark.png) | [View](screenshots/batch-02/actions-390-dark.png) |

All four views and Run/WorkGraph/Product workspaces also have light captures at all three widths
in the same directory. Historical and controller restart states are retained with their validation
records. [Mobile confirmation with expanded binding](screenshots/batch-02/confirmation-390-expanded.png)
keeps the protected action and footer visible. [Full evidence and current limits](validation.md).

## Reconciliation actual application

[Latest 1440/1920/390 dark/light application renders and mobile confirmations](screenshots/reconciliation/README.md)

[Original-operation recovery and latest actual application renders](screenshots/action-recovery/README.md)
cover PR #594's focused P1 fix; [scope, proof and compatibility limits](action-recovery-review.md).
use the actual isolated governed controller. Fixture checkpoint and older runtime captures above
remain historical evidence; this gallery is the current review submission.
