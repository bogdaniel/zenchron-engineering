import { fixture, actionItems, recoveryItems } from "./fixtures.js";
import { esc, icon, badge, panel, pageHead, link, emptyState, disclosure } from "./components.js";
import { subjectBinding } from "./workspace.js";

// This only gates a fixture simulation. Production eligibility belongs to the server.
export function fixtureActionBlocker(item, scenario) {
  if (scenario === "permission-refused") return "Permission refused · the controller cannot authorize this action.";
  if (scenario === "failed-execution") return "Execution failed · required evidence is not established.";
  if (!["ready", "scale"].includes(scenario)) return "Current subject and controller eligibility are unavailable.";
  if (!item?.eligibilityVerified) return "Governed eligibility has not been verified.";
  return "";
}

function decisionChecks(item, blocker) {
  const labels =
    item.type === "Plan approval"
      ? ["Plan revision bound", "Assignments validated", "Execution only; ceilings unchanged"]
      : ["Candidate identity bound", "Independent evidence available", "Publication only; no merge authority"];
  return `<div class="decision-checks">${labels.map((label) => `<div>${icon("history", 16)}<span>${label}</span><strong class="${blocker ? "orange" : "secondary"}">${blocker ? "Unverified" : "Simulated"}</strong></div>`).join("")}</div>`;
}

function planBinding() {
  return `<dl class="binding-list"><dt>Source</dt><dd class="mono">${fixture.repository}#598 · pinned source</dd><dt>Assignments</dt><dd>implementer → eligibility-builder → codex-build<br>reviewer → independent-reviewer → claude-review</dd><dt>Stage kinds</dt><dd>2 agent stages · 1 assurance gate · 1 human decision gate</dd><dt>Budget envelope</dt><dd class="mono">4 child runs · 2 concurrent · 12 invocations · 90 active min</dd><dt>Reported cost</dt><dd>Unknown</dd><dt>Controller</dt><dd class="mono">${fixture.controller}</dd></dl>`;
}

function exactDecisionBinding(item) {
  return `<dl class="binding-list"><dt>Request / digest</dt><dd class="mono exact-value">${esc(item.binding)}</dd><dt>Action / target</dt><dd class="mono">${esc(item.action)}<br>${esc(item.target)}</dd><dt>Run / claim</dt><dd class="mono">${esc(item.run)}<br>${esc(item.claim)}</dd></dl>${item.type === "Plan approval" ? planBinding() : subjectBinding()}`;
}

export function actionInbox(selectedObjective = "", resolved = new Map(), filter = "All", scenario = "ready") {
  if (selectedObjective && !actionItems.some((item) => item.objective === selectedObjective)) {
    const recovery = recoveryItems.find((item) => item.objective === selectedObjective);
    return `${pageHead("OPERATIONS / HUMAN AUTHORITY", "No human decision for this objective", "Recovery and engineering waits stay with their runtime owner.")}${panel("Runtime-owned work", emptyState(recovery?.title || "Inspect the objective’s engineering blocker", recovery?.detail || "No exact human authority request exists in this fixture.", link("Return to the objective", `objectives/${selectedObjective}`, "button")))}`;
  }
  const open = actionItems.filter((item) => !resolved.has(item.id));
  const actionable = open.filter((item) => !fixtureActionBlocker(item, scenario)).length;
  const items = open.filter((item) => filter === "All" || item.type === filter);
  const requested = actionItems.find((item) => item.objective === selectedObjective);
  const outcome = requested && resolved.get(requested.id);
  const selected = outcome ? requested : items.find((item) => item.objective === selectedObjective) || items[0];
  const blocker = fixtureActionBlocker(selected, scenario);
  const pickerItems = outcome ? [selected, ...items] : items;
  return `<div class="actions-page">${pageHead("OPERATIONS / HUMAN AUTHORITY", "Action required", "Human authority requests only. Bounded recovery stays with the runtime.", badge(`${open.length} pending · ${actionable} actionable`, "orange"))}
    <div class="request-summary small muted">${open.length} pending · ${actionable} actionable · simulation</div>
    <div class="inbox-toolbar"><div class="inbox-filters" aria-label="Decision type filters">${["All", "Publication", "Plan approval"].map((type) => `<button class="filter-chip" data-inbox-filter="${type}" aria-pressed="${filter === type}">${type}</button>`).join("")}</div><button class="recovery-shortcut" data-scroll-recovery aria-label="Inspect bounded runtime recovery"><span class="desktop-recovery">${recoveryItems.length} bounded recovery items</span><span class="short-recovery">${recoveryItems.length} recovery items</span>${icon("arrow", 14)}</button></div>
    ${selected ? `<label class="mobile-decision-picker" for="mobile-decision"><span>Choose a decision</span><select id="mobile-decision">${pickerItems.map((item) => `<option value="${item.objective}" ${selected.id === item.id ? "selected" : ""}>${item.type} · ${item.objective}</option>`).join("")}</select></label>` : ""}
    ${
      selected
        ? `<div class="inbox-grid"><section class="decision-list" aria-label="Human decisions">${items.map((item) => `<a class="decision-item ${selected.id === item.id ? "selected" : ""}" href="#/actions/${item.objective}" ${selected.id === item.id ? 'aria-current="page"' : ""}><div class="decision-item-meta"><span class="eyebrow">${item.type}</span><span class="small muted">${item.age}</span></div><h2>${esc(item.title)}</h2><div class="decision-item-foot"><span class="mono small muted">${item.id} · ${item.objective}</span>${icon("arrow", 16)}</div></a>`).join("")}</section>
    <article class="decision-detail"><div class="decision-detail-head"><span class="eyebrow">${selected.type} approval</span>${badge(outcome ? (outcome.outcome === "reject" ? "Rejected · simulation" : "Resolved · simulation") : blocker ? "Decision unavailable" : "Action needed", outcome ? "neutral" : "orange")}</div>
      <h2 class="decision-title">${esc(selected.title)}</h2><p class="decision-description secondary">${esc(selected.consequence)}</p>
      <div class="decision-subject"><span class="mono">${selected.objective}</span><span>${selected.type === "Plan approval" ? "Plan r2 · fixed assignments and budget" : `Candidate ${fixture.revision.slice(0, 7)} · contract r3`}</span>${link("Workspace", `objectives/${selected.objective}`)}</div>
      ${outcome ? `<div class="banner neutral" role="status"><h3 id="decision-receipt" tabindex="-1">Historical decision receipt</h3><strong>${outcome.outcome === "reject" ? "Simulated rejection recorded" : "Simulated decision already resolved"}</strong><p class="small muted">Past simulated outcome · ${esc(outcome.recordedAt || "simulation time unavailable")} · exact subject retained</p><p>${outcome.outcome === "reject" ? "Publication remains withheld in this example. No recovery is authorized." : "This is a local receipt, not a runtime authorization or publication."}</p>${outcome.note ? `<p>${esc(outcome.note)}</p>` : ""}</div>` : decisionChecks(selected, blocker)}
      ${outcome ? `<div class="decision-footer">${link("Other human requests", "actions", "button")}</div>` : `<div class="decision-footer">${blocker ? `<p class="orange" role="status">${esc(blocker)}</p>` : '<p class="small muted">Preview only · simulated eligibility, no runtime action</p>'}<div class="decision-buttons"><button class="button" data-decision="reject" data-request="${selected.id}" ${blocker ? "disabled" : ""}>Reject</button><button class="button primary" data-decision="approve" data-request="${selected.id}" ${blocker ? "disabled" : ""}>${selected.type === "Plan approval" ? "Review & approve plan" : "Review & approve"} ${icon("arrow", 14)}</button></div></div>`}
      ${disclosure("Technical binding and evidence", exactDecisionBinding(selected), "Exact subject")}
    </article></div>`
        : panel(
            "Decision inbox",
            emptyState(
              "No open human decisions in this view",
              "Fixture decisions are recorded only in this browser session.",
              '<button class="button" data-reset-decisions>Reset fixture decisions</button>',
            ),
          )
    }
    <section id="runtime-recovery" tabindex="-1" class="recovery-summary" aria-label="Runtime recovery"><h2>Runtime recovery</h2><p class="muted small">${recoveryItems.length} bounded recovery items · no human approval requested</p>${recoveryItems.map((item) => `<div class="recovery-row"><div><strong>${item.title}</strong><p class="muted small">${item.detail}</p></div>${badge(item.state)}${link("Inspect", `objectives/${item.objective}/activity`)}</div>`).join("")}</section>
    ${resolved.size ? `<div class="panel fixture-decision-history" aria-label="Local fixture decision outcomes">${[...resolved].map(([id, result]) => `<p><span class="mono orange">${id}</span> · ${result.outcome === "approve" ? "Fixture human decision recorded" : "Fixture rejection recorded"}${result.note ? `<br><span class="muted">${esc(result.note)}</span>` : ""}</p>`).join("")}</div><div class="resolution-log"><span>${resolved.size} local fixture outcomes</span><button class="text-button" data-reset-decisions>Reset simulation</button></div>` : ""}</div>`;
}

export function decisionDialog(requestID, verb, scenario) {
  const item = actionItems.find((candidate) => candidate.id === requestID);
  if (!item) return "";
  const blocker = fixtureActionBlocker(item, scenario);
  return `<div class="decision-confirmation"><div class="confirmation-body"><div class="dialog-head"><span class="eyebrow orange">FIXTURE HUMAN DECISION</span><button class="icon-button" data-close-dialog aria-label="Close confirmation">${icon("close", 20)}</button></div>
    <h2 id="dialog-title">${verb === "reject" ? "Reject this exact request?" : item.type === "Plan approval" ? "Approve this plan revision?" : "Record publication approval?"}</h2>
    <p class="secondary">${esc(item.title)}</p><div class="decision-subject"><span class="mono">${item.id} · ${item.objective}</span><span>${item.type === "Plan approval" ? "Plan revision 2" : `Candidate ${fixture.revision.slice(0, 7)} · contract r3`}</span></div>
    <p class="decision-description">${verb === "reject" ? "Record failing human evidence against this request only. It does not disappear or authorize recovery." : esc(item.consequence)}</p>${decisionChecks(item, blocker)}
    <div class="confirmation-action"><span class="muted">Protected action</span><strong class="mono">${esc(item.action)}</strong><span>${esc(item.target)}</span></div>
    ${disclosure("Technical binding and evidence", exactDecisionBinding(item), "Unchanged context")}
    <label class="note-label" for="decision-note">Decision note <span class="muted">(optional)</span></label><textarea id="decision-note" maxlength="500" rows="2" placeholder="Context for this exact request…"></textarea>
    <p class="small ${blocker ? "orange" : "muted"}" role="status">${blocker || "Fixture simulation only. No authority record is created."}</p></div>
    <div class="dialog-buttons"><button class="button" data-close-dialog>Cancel</button><button class="button ${verb === "approve" ? "primary" : ""}" data-confirm-decision="${verb}" data-request="${item.id}" data-binding="${esc(item.binding)}" data-revision="${item.revision}" ${blocker ? "disabled" : ""}>Confirm fixture ${verb === "approve" ? "decision" : "rejection"}</button></div></div>`;
}
