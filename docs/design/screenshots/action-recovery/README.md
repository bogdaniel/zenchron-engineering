# Exact original-operation recovery

Actual React application rendered by `TestReactEngineeringOSGovernedWorkspace` on an isolated
Go Control Plane/controller/socket/journal. External issue and evidence producers are offline test
inputs. These are test-state observations, never production measurements or operator acceptance.
The original request is retained across navigation and reload; each recovered command was sent once.

| Interaction | Historical delivery receipt |
| --- | --- |
| Objective creation | [1440](recovered-objective.png) |
| Plan revision | [1440](recovered-revision.png) |
| Run resume | [1440](recovered-resume.png) |
| Run refresh | [1440](recovered-refresh.png) |

The newer plan page retains the unresolved original revision: [390 collapsed](unresolved-revision-390-collapsed.png)
and [390 expanded](unresolved-revision-390-expanded.png). The driver asserts revision 1 remains bound
while the page shows revision 2. A missing receipt or mismatched digest does not unlock submission.

| Application | 1440 dark | 1920 dark | 390 dark | 390 light |
| --- | --- | --- | --- | --- |
| Overview | [View](overview-1440-dark.png) | [View](overview-1920-dark.png) | [View](overview-390-dark.png) | [View](overview-390-light.png) |
| Objectives | [View](portfolio-1440-dark.png) | [View](portfolio-1920-dark.png) | [View](portfolio-390-dark.png) | [View](portfolio-390-light.png) |
| Workspace | [View](workspace-1440-dark.png) | [View](workspace-1920-dark.png) | [View](workspace-390-dark.png) | [View](workspace-390-light.png) |
| Action Required | [View](actions-1440-dark.png) | [View](actions-1920-dark.png) | [View](actions-390-dark.png) | [View](actions-390-light.png) |

[Browser results](react-browser-results.json) · [implementation, proof and compatibility limits](../../action-recovery-review.md).

The driver checks accessible names, contrast, reflow and historical receipt focus. Manual
screen-reader/high-zoom qualification and real operator acceptance remain open. These captures
supplement the preserved prior galleries; no visual baseline has been self-approved.
