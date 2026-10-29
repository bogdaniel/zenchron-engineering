import { fixture, objectives, agents, timeline, scaleObjectives } from "./fixtures.js";
import {
  icon,
  esc,
  badge,
  link,
  panel,
  pageHead,
  budgetMeter,
  disclosure,
  pageWindow,
  pageControls,
} from "./components.js";

export const workspaceTabs = ["Lifecycle", "Plan", "WorkGraph", "Evidence", "Activity"];

export function subjectBinding() {
  return `<dl class="binding-list"><dt>Repository</dt><dd class="mono">${fixture.repository}</dd><dt>Candidate commit</dt><dd class="mono exact-value">${fixture.revision}</dd><dt>Candidate tree</dt><dd class="mono exact-value">${fixture.tree}</dd><dt>Contract / policy</dt><dd class="mono">${fixture.contract}<br>${fixture.policy}</dd><dt>Controller</dt><dd class="mono">${fixture.controller}</dd></dl>`;
}

function workGraph() {
  const units = [
    {
      id: "WU-612-A",
      title: "Credential path hardening",
      issue: 612,
      state: "Handoff admitted",
      tone: "green",
      agent: "codex-build",
      type: "Implementation",
      selected: true,
    },
    {
      id: "WU-613-B",
      title: "Independent boundary review",
      issue: 613,
      state: "Handoff admitted",
      tone: "green",
      agent: "claude-review",
      type: "Independent review",
    },
    {
      id: "WU-614-C",
      title: "Update operator guidance",
      issue: 614,
      state: "Runnable frontier",
      tone: "orange",
      agent: "claude-core",
      type: "Documentation",
    },
    {
      id: "WU-615-D",
      title: "Integrate accepted outputs",
      issue: 615,
      state: "Awaiting handoffs",
      tone: "neutral",
      agent: "Unassigned",
      type: "Integration",
    },
  ];
  return `<div class="graph-description"><span class="mono small muted">workgraph-credential-boundary · revision 2</span><span class="small">${badge("1 runnable unit", "orange")}</span></div>
    <div class="workgraph" role="group" aria-label="Issue-backed WorkGraph; A leads to B and C; B and C lead to D">
      <div class="graph-connectors" aria-hidden="true"><svg viewBox="0 0 760 224" preserveAspectRatio="none"><path d="M180 112H230V56H280 M230 112v56h50 M475 56h50v56h50 M475 168h50v-56"/></svg></div>
      ${units.map((unit, i) => `<button class="graph-node node-${i} ${unit.selected ? "selected" : ""}" data-unit="${unit.id}" aria-pressed="${Boolean(unit.selected)}"><span class="node-top"><span class="eyebrow">${unit.id}</span><span class="mono small">#${unit.issue}</span></span><strong>${unit.title}</strong><span class="small muted">${unit.type}</span><span class="node-bottom"><span class="status-dot ${unit.tone}"></span><span class="small ${unit.tone}">${unit.state}</span></span><span class="mono small muted">${unit.agent}</span></button>`).join("")}
    </div><div id="unit-detail" class="unit-inspector" role="status"><span class="mono orange">WU-612-A</span><span>Admitted handoff <code>handoff-612-A-r1</code> · exact candidate <code>81e4b9a</code></span><span class="muted small">Provider exit is not dependency satisfaction.</span></div>`;
}

function plan() {
  return `<div class="plan-banner"><div><div class="eyebrow">ENGINEERING PLAN · plan-612 · r3</div><h3>Approved immutable revision</h3><p class="muted small">Four agent stages and two gates. Approval grants bounded execution only.</p></div>${badge("Approved", "green")}</div>
    <div class="table-scroll" tabindex="0" role="region" aria-label="Plan stages"><table class="data-table plan-table"><thead><tr><th scope="col">Stage / kind</th><th scope="col">Role → profile → agent</th><th scope="col">Dependency</th><th scope="col">State</th></tr></thead><tbody>
    <tr><td>Implement isolation <small class="row-meta mono">agent · run-612-g1</small></td><td>implementer → boundary-builder<br><small class="muted mono">codex-build · operator_trusted</small></td><td class="muted">Approved revision</td><td>${badge("Completed", "green")}</td></tr>
    <tr><td>Independent review <small class="row-meta mono">agent · run-613-g1</small></td><td>reviewer → independent-reviewer<br><small class="muted mono">claude-review · operator_trusted</small></td><td>Implement isolation</td><td>${badge("Completed", "green")}</td></tr>
    <tr><td>Exact-tree assurance <small class="row-meta mono">assurance_gate · no worker run</small></td><td class="muted">Existing verifier evidence</td><td>Independent review</td><td>${badge("Satisfied", "green")}</td></tr>
    <tr><td>Publication owner decision <small class="row-meta mono">human_decision_gate · no worker run</small></td><td class="muted">Existing human evidence</td><td>Exact-tree assurance</td><td>${link("Decision required", "actions/OBJ-612")}</td></tr>
    </tbody></table></div><div class="budget-strip"><div><span class="eyebrow">CHILD RUNS</span><span class="mono">2 / 4</span></div><div><span class="eyebrow">INVOCATIONS</span><span class="mono">5 / 12</span></div><div><span class="eyebrow">ACTIVE WORK</span><span class="mono">38m / 90m</span></div><div><span class="eyebrow">COST</span><span>Unknown</span></div></div><p class="panel-footer muted small">This is a stage excerpt. Budget consumption carries across revisions and never resets.</p>`;
}

function evidence() {
  return `<div class="evidence-summary">${badge("3 applicable claims", "green")}<span class="muted small">Result and lifecycle are separate. Every claim binds to the exact subject below.</span></div><div class="table-scroll" tabindex="0" role="region" aria-label="Evidence"><table class="data-table"><thead><tr><th scope="col">Claim</th><th scope="col">Producer / environment</th><th scope="col">Result</th><th scope="col">Lifecycle</th></tr></thead><tbody>
    <tr><td>Credential boundary regression tests<br><small class="mono muted">claim-credential-tests</small></td><td>deterministic-verifier<br><small class="mono muted">detached-tree / Go toolchain</small></td><td>${badge("Pass", "green")}</td><td>${badge("Valid", "green")}</td></tr>
    <tr><td>Independent review<br><small class="mono muted">claim-independent-review</small></td><td>claude-review<br><small class="mono muted">review-env-613</small></td><td>${badge("Pass", "green")}</td><td>${badge("Valid", "green")}</td></tr>
    <tr><td>Scope invariants preserved<br><small class="mono muted">claim-scope-invariants</small></td><td>scope-verifier<br><small class="mono muted">reassessment-env-612</small></td><td>${badge("Pass", "green")}</td><td>${badge("Valid", "green")}</td></tr>
    <tr><td>Publication owner approval<br><small class="mono muted">claim-publication-owner</small></td><td class="muted">Human evidence required</td><td>${badge("Missing", "orange")}</td><td>${badge("Incomplete", "orange")}</td></tr>
    </tbody></table></div>${disclosure("Exact evidence subject", subjectBinding(), "Commit, tree, contract and policy")}`;
}

function activity(events = timeline, offset = 0, paginated = true) {
  const page = pageWindow(events, offset);
  return `<ol class="activity-timeline">${page.items.map(([time, title, description, tone]) => `<li><time>${time}</time><span class="timeline-dot ${tone}" aria-hidden="true"></span><div><strong>${title}</strong><p class="muted small">${description}</p></div></li>`).join("")}</ol>${paginated ? pageControls("activity", page) : ""}`;
}

export function workspace(id = "OBJ-612", tab = "Lifecycle", events = timeline, offset = 0, scenario = "ready") {
  const objective = (scenario === "scale" ? scaleObjectives : objectives).find((o) => o.id === id);
  if (!objective)
    return `${pageHead("OBJECTIVES / UNAVAILABLE", "Objective not found", "This fixture has no objective with that identity.", link("Return to portfolio", "objectives", "button"))}`;
  if (id !== "OBJ-612")
    return `${pageHead(`OBJECTIVES / ${objective.id}`, esc(objective.title), esc(objective.detail), badge(objective.state))}<div class="banner neutral">Fixture summary · the complete lifecycle demonstration is available on OBJ-612.</div>${panel("Objective context", `<dl class="binding-list summary-binding"><dt>Issue</dt><dd>#${objective.issue}</dd><dt>Product</dt><dd>${esc(objective.product)}</dd><dt>Worker</dt><dd class="mono">${esc(objective.agent)}</dd><dt>Next boundary</dt><dd>${esc(objective.blocker)}</dd></dl><div class="panel-footer">${link("Open complete lifecycle example", "objectives/OBJ-612", "button")}</div>`)}${panel("Related action", `<p class="panel-footer">${link("Inspect exact decision context", `actions/${id}`)}</p>`)}`;
  if (scenario === "failed-execution")
    return `${pageHead("OBJECTIVES / RUN-612-G1", esc(objective.title), "Failed execution · candidate retained; publication withheld.", link("Inspect human context", "actions/OBJ-612", "button"))}${panel("Execution failure", `<dl class="binding-list summary-binding"><dt>Run / candidate</dt><dd class="mono">run-612-g1 · 81e4b9a</dd><dt>Failure class</dt><dd>Execution attempt bound exhausted · fixture</dd><dt>Held material</dt><dd>Candidate retained for diagnosis; no acceptance or publication authority</dd><dt>Plan state</dt><dd>Failed · no automatic restart</dd><dt>Next step</dt><dd>Inspect causal activity and existing controller diagnostic paths. New execution requires the applicable governed decision.</dd></dl>`)}${panel("Prior causal activity", activity(events, offset), '<span class="eyebrow">HISTORICAL FIXTURE</span>')}`;
  return `<div class="breadcrumb">${link("Objectives", "objectives")}<span>/</span><span class="mono">${id}</span><span>/</span><span>Lifecycle</span></div>
    ${pageHead("ENGINEERING OS / CREDENTIAL BOUNDARY", esc(objective.title), `${id} · Issue #612 · Proposed change to the execution boundary`, `<a class="button primary" href="#/actions/OBJ-612">Review decision ${icon("arrow", 15)}</a>`)}
    <div class="workspace-status">${badge("Awaiting authority", "orange")}<span class="mono small muted">candidate 81e4b9a · contract r3 · plan r3</span><span class="muted small">Last fixture event 14:32 UTC</span></div>
    <ol class="lifecycle-strip" aria-label="Overlapping activity for this objective">${[
      ["Intent", "Pinned", "done"],
      ["Plan", "Approved r3", "done"],
      ["Execute", "2 of 4 handoffs", "active"],
      ["Assure", "Independent evidence", "done"],
      ["Authority", "Human evidence needed", "current"],
      ["Publish", "Withheld", "pending"],
    ]
      .map(
        ([title, label, state], i) =>
          `<li class="${state}"><span class="lifecycle-number">${state === "done" ? icon("check", 13) : String(i + 1).padStart(2, "0")}</span><span><strong>${title}</strong><small>${label}</small></span></li>`,
      )
      .join("")}</ol>
    <p class="flow-explanation muted">This objective has concurrent work. Publication applies to run-612-g1 only; the documentation unit can remain runnable.</p><div class="workspace-grid"><div class="workspace-main"><div class="tabs" aria-label="Objective sections">${workspaceTabs.map((t) => `<a href="#/objectives/${id}/${t.toLowerCase()}" ${tab === t ? 'aria-current="page"' : ""}>${t}</a>`).join("")}</div>
    <div id="workspace-section">${tab === "Plan" ? panel("EngineeringPlan", plan()) : tab === "Evidence" ? panel("Applicable evidence", evidence()) : tab === "Activity" ? panel("Causal activity", activity(events, offset), '<span class="eyebrow">UTC · FIXTURE</span>') : tab === "WorkGraph" ? panel("Operational WorkGraph", workGraph(), '<span class="eyebrow">ISSUE-BACKED UNITS</span>') : `${panel("Operational WorkGraph", workGraph(), '<span class="eyebrow">ISSUE-BACKED UNITS</span>')}${panel("Evidence & review", `<div class="evidence-mini"><div><span class="result-icon">${icon("check", 16)}</span><strong>Independent review</strong><span class="mono small">APPROVE · 81e4b9a</span>${badge("Valid", "green")}</div><div><span class="result-icon">${icon("check", 16)}</span><strong>Exact-tree assurance</strong><span class="mono small">46 checks · detached tree</span>${badge("Valid", "green")}</div><div><span class="orange">${icon("actions", 18)}</span><strong>Publication authority</strong><span class="muted small">Missing human approval claim</span>${badge("Awaiting authority", "orange")}</div></div>`, link("Inspect evidence", "objectives/OBJ-612/evidence"))}${panel("Recent activity", activity(timeline.slice(0, 3), 0, false), link("Full timeline", "objectives/OBJ-612/activity"))}`}</div></div>
    <aside class="workspace-aside">${panel("Next authority boundary", `<div class="boundary-body"><div class="eyebrow orange">HUMAN DECISION REQUIRED</div><h3>Open a pull request</h3><p class="muted small">Technical claims are applicable. Publication still needs your exact-subject approval.</p><a class="button primary" href="#/actions/OBJ-612">Inspect & decide ${icon("arrow", 14)}</a><p class="muted small">This does not grant merge or adoption authority.</p></div>`)}
    ${panel("Frozen envelope", `<dl class="envelope"><dt>Scope</dt><dd>runtime/credential_*</dd><dt>Prohibited</dt><dd>Policy and trust-root changes</dd><dt>Permissions</dt><dd>Candidate write; PR publication after authority</dd><dt>Budget used</dt><dd class="mono">38 / 90 active min</dd></dl>${budgetMeter(38, 90)}<p class="panel-footer small muted">No privilege expansion observed.</p>`)}
    ${panel(
      "Assigned workers",
      `<div class="assigned-list">${agents
        .slice(0, 3)
        .map(
          (a, i) =>
            `<button class="agent-row" data-inspect-agent="${a.id}"><span class="avatar">${a.initials}</span><span><strong>${a.id}</strong><small class="muted">${["implementer · boundary-builder", "integrator · core-integrator", "reviewer · independent-reviewer"][i]}</small></span></button>`,
        )
        .join(
          "",
        )}</div><p class="panel-footer small muted">Role → profile → execution agent. Trust and acceptance authority remain separate.</p>`,
    )}
    </aside></div>`;
}
