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
