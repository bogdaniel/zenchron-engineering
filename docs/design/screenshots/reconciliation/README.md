# Actual application — reconciliation review

These are real browser renders of `/app` against an isolated Go/controller/socket/journal fixture.
The header labels isolated controller test data. They are not production measurements or operator
acceptance. Source and external producer/forge boundaries are described in the batch review.

| View | 1440 dark | 1920 dark | 390 dark | 390 light |
| --- | --- | --- | --- | --- |
| Overview | [image](overview-1440-dark.png) | [image](overview-1920-dark.png) | [image](overview-390-dark.png) | [image](overview-390-light.png) |
| Objectives | [image](portfolio-1440-dark.png) | [image](portfolio-1920-dark.png) | [image](portfolio-390-dark.png) | [image](portfolio-390-light.png) |
| Workspace | [image](workspace-1440-dark.png) | [image](workspace-1920-dark.png) | [image](workspace-390-dark.png) | [image](workspace-390-light.png) |
| Action Required | [image](actions-1440-dark.png) | [image](actions-1920-dark.png) | [image](actions-390-dark.png) | [image](actions-390-light.png) |
| Run controls | [image](run-1440-dark.png) | [image](run-1920-dark.png) | [image](run-390-dark.png) | [image](run-390-light.png) |
| WorkGraph | [image](graph-1440-dark.png) | [image](graph-1920-dark.png) | [image](graph-390-dark.png) | [image](graph-390-light.png) |
| Objective intake | [image](intake-1440-dark.png) | [image](intake-1920-dark.png) | [image](intake-390-dark.png) | [image](intake-390-light.png) |

Desktop light variants are also included. Mobile [collapsed confirmation](decision-confirmation-390-collapsed.png)
and [expanded binding](decision-confirmation-390-expanded.png) retain the footer. The
[historical typed decision](resolved-decision-390-dark.png) and [publication receipt](publication-receipt-1440-dark.png)
follow real durable writes; publication approval records evidence only and creates no PR.

[Actual browser results](react-browser-results.json), [separate presentation/load fixture report](presentation-validation.json)
and Chromium accessibility trees for [inbox](actions-390-accessibility-tree.json) and
[workspace](workspace-390-accessibility-tree.json) are included. Browser trees do not qualify manual
screen-reader speech. Runtime screenshots are new review artifacts, not a self-approved baseline.
The approved `/design` checkpoint remains separate; its comparison report is included alongside.
