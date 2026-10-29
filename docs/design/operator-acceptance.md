# Engineering OS operator acceptance runbook

Execute against the actual `/app` React application, never `/design`. This is one
operator-owned checklist, not an authorization to execute work or publish.
Record the exact PR head, measured controller build, configuration digests,
browser/OS, operator, UTC times and observed IDs for every result. A fixture
capture or a green test is not a human acceptance result.

## 0. Environment and authorization gates

Use an isolated state directory, controller root, test repository, operator
configuration and narrowly scoped credentials. Do not copy production state or
credentials. The operator must provision an existing legitimate policy, project
model, installed agents/profiles, pinned assurance image and offline cache using
[getting started](../getting-started.md), [configuration](../configuration.md) and
[planning](../planning.md). Automatic discovery must be off for this exercise.

| Work | Isolated environment | Required authorization |
| --- | --- | --- |
| Build, read-only browser/keyboard/negative-state checks | Yes, no production access | Ordinary review scope |
| Start/restart a test controller, create/revise an objective, spend model budget | Yes, real configured workers | Operator authorizes named repo, issue, agents, budgets and process changes |
| Approve a plan or resolve a human authority request | Yes, real controller/journal | Human confirms the exact current subject; this guide supplies no answer |
| Authorize publication and produce a real PR | Dedicated disposable remote repo | Explicit human approval of repo, candidate/tree, protected action and consequence; normal kernel evidence still required |
| Merge, production cutover, legacy deletion | Outside this runbook | Separate authorization; never implied by acceptance |

The **serving controller and reader must both understand the new receipt events**.
An older trusted-main controller cannot provide correlation for creation,
revision, resume or refresh. A new frontend paired with that controller can
observe effects but must keep missing-receipt outcomes unknown. That is not a
passing end-to-end deployment.

Before mutation walkthroughs, the operator must supply a receipt-capable serving
controller whose measured generation matches the existing durable authority and
holds the role. Use only existing sanctioned controller deployment/adoption
procedures. An unmerged PR is not trusted main; a local build flag, direct SQLite
write, invented adoption record or edited provenance is not a substitute. If this
environment cannot be established without a separate authorized deployment step,
mark mutation/publication acceptance **BLOCKED** and run only the read/isolated
automation portions. Do not weaken the trust root to make the walkthrough run.

## 1. Build and launch the read application

From the reviewed checkout, set operator-supplied absolute paths. These examples
are meant to be run in separate terminals; they do not start production services.

```bash
export ZC_ACCEPTANCE_DIR=/private/tmp/zc-accept
export ZC_ACCEPTANCE_CONFIG=/absolute/path/to/isolated-operator.json
export ZC_ACCEPTANCE_REPO=owner/disposable-engineering-test
export ZC_ACCEPTANCE_STATE=/absolute/path/to/isolated-state
export ZENCHRON_CONTROLLER_ROOT=/absolute/path/to/isolated-controller-root
mkdir -p "$ZC_ACCEPTANCE_DIR"
chmod 700 "$ZC_ACCEPTANCE_DIR"
git rev-parse HEAD > "$ZC_ACCEPTANCE_DIR/head.txt"
npm --prefix frontend ci
npm --prefix frontend run build
git diff --exit-code -- controlplane/assets/design/react.js
go build -o "$ZC_ACCEPTANCE_DIR/zenchron-engineering" ./cmd/zenchron-engineering
```

This unadopted read-application binary does not become controller authority.
The state must already exist at the supported schema version. After the operator
has provisioned/authorized the compatible test controller, launch the Control
Plane on an unused loopback port:

```bash
"$ZC_ACCEPTANCE_DIR/zenchron-engineering" control-plane \
  --state-dir "$ZC_ACCEPTANCE_STATE" \
  --controller-root "$ZENCHRON_CONTROLLER_ROOT" --listen 127.0.0.1:8878
```

Open `http://127.0.0.1:8878/app#/overview`. Read the token from
`$ZC_ACCEPTANCE_STATE/control-plane.token` locally into the login field. Do not
paste it into reports, terminal logs or screenshots. Wrong-token login must refuse;
the code-only bootstrap must reveal no private observations. Correct login must
preserve the requested route. In Controller health, independently inspect durable
identity, live reachability, role, admission and generation match. Compare the
measured build with the sanctioned controller record. An attested but disconnected
controller must not be shown as serving or actionable.

## 2. Overview and portfolio

At 1440 and 1920 desktop, then 390 mobile, use both themes. Verify operational
counts against the same authenticated observed reads, with observation age visible.
Concurrent pipeline dimensions must not read as exclusive objective phases. No
fixture progress percentage or unsupported throughput measurement may appear.
Navigate Overview → Objectives → the exact objective → Back. Search/filter/page
through real observations, including a pending plan beyond offset 50 when such
state exists. An empty page is not proof that every later page is empty. Record
readiness and navigation time for the actual test data; do not label fixture
timings production performance. Missing data must say unrecorded/unknown.

## 3. Objective creation, revision and exact plan approval

After explicit operator permission to plan the designated existing issue, select
**Create objective**. Inspect repository/issue, installed agent/profile/template,
deterministic choice and budget. Select **Review objective proposal**, inspect the
exact confirmation, then the human may confirm this proposal only. Record its
plan ID, revision and digest; proposal must remain unapproved. Reload and inspect
the same objective. Revision creates a new unapproved immutable proposal.

Read the controller preview and assignments. Inspect the policy/budget and all
agent stages versus assurance/human gates. Cancel once: no approval or work must
be recorded. Only after the human agrees to that exact plan may they select
**Review & approve** and confirm. Check the durable approval receipt and the exact
revision/assignments digest. A changed preview, controller, subject or unavailable
read must refuse; do not refresh hashes manually to force an approval.

## 4. WorkGraph execution and agent activity

Inspect the explicitly objective-bound graph, admitted handoffs, dependency
frontier, selected unit and run. Verify the installed agent ID, provider kind and
trust mode remain distinct. Agent execution cannot imply independent acceptance.
After the approved plan admits work, inspect activity and causal event history;
only agent stages create EngineeringRuns. Assurance and human gates use their
own evidence/request state. Navigate older graph windows and event pages; no
missing node or older event may silently be treated as completed. Reload selected
unit/run routes. Record whether declared dependency-ready work actually continues
through the existing scheduler. A graph absent or bound to another revision must
close contextual actions instead of inferring an association.

## 5. Human decision and continuation

In Action Required, distinguish pending requests from currently actionable
decisions and bounded automatic recovery. Open a genuine request. Inspect its
exact objective/unit/run and allowed outcome grammar. Verify mobile actions and
summary remain usable with technical detail collapsed. Cancel once without effect.
The human chooses the substantive answer; then review/confirm only that exact
request. Check the durable resolution, historical subject and focus after the
pending row disappears. It must not silently select a different request. Confirm
continuation occurs through the existing dependency/readiness/scheduler path,
with no new bypass run. Exercise allow/deny, selected-option and text grammars
only when real requests of each type exist; otherwise mark them NOT EXERCISED.

## 6. Review, remediation and integration

Use a designated test issue that legitimately produces an independent review
finding or an existing such run. Inspect the finding, evidence and exact candidate
binding. Producer output alone must not accept its own change. Distinguish a
human authority request from a bounded remediation the runtime can admit. Follow
the remediation epoch/run, review outcome and integration handoff; inspect
retained failed attempts. Do not manufacture a finding, edit the journal or mark
missing evidence passed. An integration conflict must retain diagnostic context
and block its protected outcome until existing remediation/evidence resolves it.
Mark unavailable scenarios NOT EXERCISED, not PASS.

## 7. Publication and a real PR outcome — human-authorized gate

Stop here unless the human explicitly authorizes publication to the disposable
remote. Record repo, run/request ID, candidate commit/tree, contract revision,
protected action and intended PR target in the restricted acceptance report.
Verify independent assurance is current and publication policy is satisfied.

In the browser, inspect the request summary and disclosed exact binding. The
human may select **Review & authorize publication** and confirm the current
subject. The recorded receipt is human evidence, not a publication guarantee and
not merge authority. Observe the runtime's subsequent authority decision and
candidate push/PR-create or update result. Follow the real PR link and compare
repository, branch, head SHA and run provenance. Record the actual PR number and
outcome. If still blocked, record the legitimate unmet requirement. No frontend
or CLI fallback counts as a passing browser authorization workflow. Do not merge.

## 8. Stale, disconnected, failed and lost responses

On isolated processes only, use browser DevTools offline to interrupt reads.
Retained observations must age to stale/disconnected, never remain live assurance;
all protected actions close. Restore networking and perform fresh reads. Stop the
**verified test controller** through its ordinary shutdown path while retaining
the Control Plane; durable identity must remain historical, reachability unknown
and actions closed. Restart only that controller under the same sanctioned
build/config. A legitimately failed test run must show diagnostic cause, historical
attempts and eligible recovery, not invented success.

For post-commit reply loss, ordinary offline mode is insufficient to prove that
the command reached the controller. Run the maintained real-controller fault
driver below: it fetches the actual successful response and then discards it.
It covers creation/revision/resume/refresh, navigation/reload, wrong/missing
receipts and storage refusal. The human also inspects its retained subject/focus.
No missing receipt proves non-execution or permits resend. A crash between effect
and receipt remains unknown; do not clear recovery storage to enable a duplicate.

## 9. Accessibility and responsive qualification

Use keyboard only for login, sidebar/bottom navigation, search/filter/pager,
WorkGraph inspection, disclosures and all confirmations. Skip to main must focus
the landmark without changing the route. Verify visible focus, natural order,
dialog focus containment, Escape/cancel return and result/receipt focus. No action
may become unreachable while focused content disappears.

Use VoiceOver/Safari or NVDA/Firefox on a supported host. Record what is announced
for page landmarks, table headers, request context, disabled reason, disclosure,
dialog title/subject, refusal, unknown result and historical receipt. Check 200%
and 400% native browser zoom plus a 390px viewport, with enlarged text, both
themes and reduced motion. Verify reflow, contrast and pointer targets; no
horizontal page scroll, covered decision footer or clipped exact binding. Automated
names/contrast checks do not qualify spoken announcements or native zoom. Attach
the human's result, not a guessed accessibility pass.

## 10. Reload, process restart and historical evidence

Reload exact objective/revision, decision and run URLs; selection must remain exact.
After authorized isolated restart, verify pause/control state, plan approval,
decision resolution and original delivery receipts persist without another POST.
Inspect older event pages, source-run and refresh successor links, failed attempts
and publication evidence. Historical authorization must not become authority for
a newer candidate. Preserve unknown delivery references and controller binding
across reconnect. Never reset budgets, grant stale authority or silently retry.

## Executable isolated automation

Use already-installed Playwright and Chrome; no new test framework is required.
The two drivers use disposable real Go/socket/role/journal state with explicitly
offline forge/provider inputs. They cannot establish a real remote PR outcome.

```bash
export ZENCHRON_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs
export ZENCHRON_CHROME_EXECUTABLE=/absolute/path/to/chrome
export ZENCHRON_E2E_CHROME="$ZENCHRON_CHROME_EXECUTABLE"
export ZENCHRON_REACT_BROWSER_E2E=1
export ZENCHRON_BROWSER_E2E=1
export ZENCHRON_E2E_ARTIFACTS="$ZC_ACCEPTANCE_DIR/browser-artifacts"
export ZENCHRON_LIVE_SCREENSHOTS="$ZC_ACCEPTANCE_DIR/restart-artifacts"
go test -race ./cmd/zenchron-engineering -count=1 -timeout 5m \
  -run '^(TestReactEngineeringOSGovernedWorkspace|TestBrowserWorkspace|TestEngineeringOSPlanRejectEndToEnd)$' -v
```

The nonempty `ZENCHRON_PLAYWRIGHT_MODULE` enables the rejection driver's browser
invocation; a skipped browser invocation is NOT evidence. The React driver checks
actual 1440/1920/390 dark/light layouts, contrast/reflow and
historical focus. The preserved legacy driver checks continued browser parity.
The rejection driver checks socket loss and an actual OS-process crash/restart.

## Acceptance receipt

For each section record `PASS`, `FAIL`, `BLOCKED` or `NOT EXERCISED`, exact IDs,
expected/observed behavior and an artifact path. Keep fixture observations,
automated checks and genuine operator observations separately labelled. Include
authorization references for every material action; exclude tokens and private
notes from shared artifacts. List unresolved failures, scenario gaps and any lost
legacy operation. The human signs the final operator receipt independently.
CI must be green at the exact submitted head before release qualification.
Production cutover, legacy retirement and merge require separate decisions.
