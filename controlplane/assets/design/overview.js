import { objectives, agents, actionItems } from "./fixtures.js";
import { icon, badge, link, panel, coverageChart, objectiveRows, pageHead, disclosure } from "./components.js";

export function overview(resolved = new Map()) {
  const open = actionItems.filter((item) => !resolved.has(item.id));
  const executing = agents.reduce((sum, agent) => sum + agent.used, 0);
  const dimensions = [
    ["Intent", String(objectives.length), "Objective contexts", "Fixture intent index", "neutral", "objectives"],
    ["Plan", "01", "Unapproved plan revisions", "Distinct from running plans", "orange", "actions/OBJ-598"],
    ["Execute", String(executing), "Executing runs now", "One objective can own several", "green", "objectives"],
    ["Assure", "02", "Runs awaiting assurance", "May overlap execution elsewhere", "green", "objectives"],
    [
      "Authorize",
      String(open.length),
      "Human authority requests",
      "Recovery is not a human request",
      "orange",
      "actions",
    ],
    ["Publish", "03", "PRs in this snapshot", "Current identities; not 7d throughput", "green", "objectives"],
  ];
  return `${pageHead("OPERATIONS / FACTORY FLOOR", "Engineering overview", "Engineering activity across independently governed work.", badge("Controller G42 · fixture"))}
    <a class="mobile-action-summary" href="#/actions"><span class="mono orange">${String(open.length).padStart(2, "0")}</span><span>Human decisions awaiting attention<small>Bounded recovery stays with the runtime</small></span>${icon("arrow", 16)}</a>
    <div class="section-label"><span>ENGINEERING ACTIVITY</span><span class="muted">Snapshot dimensions · scopes differ and overlap</span></div>
    <section class="pipeline" aria-label="Overlapping engineering activity dimensions">${dimensions.map(([title, count, label, note, tone, destination]) => `<a class="pipeline-cell" href="#/${destination}"><div class="pipeline-top"><span class="eyebrow">${title}</span></div><div class="pipeline-value ${tone}">${count}</div><p>${label}</p><small class="muted">${note}</small></a>`).join("")}</section>
    <p class="flow-explanation muted">These are overlapping observations, not six exclusive lifecycle states. Do not sum them or infer objective completion.</p>
    <section class="metric-strip" aria-label="Measurement availability">
      <div><span class="eyebrow">CHANGES / SUPERVISION HR</span><span class="metric-value cost-unknown">Unavailable</span><small class="muted">Supervision time is not measured</small></div>
      <div><span class="eyebrow">FIRST-PASS ACCEPTANCE</span><span class="metric-value cost-unknown">Unavailable</span><small class="muted">Accepted revision cohort not defined</small></div>
      <div><span class="eyebrow">OBJECTIVE CYCLE TIME</span><span class="metric-value cost-unknown">Unavailable</span><small class="muted">Only run active/wait durations exist</small></div>
      <div><span class="eyebrow">PROVIDER COST</span><span class="metric-value cost-unknown">Unknown</span><small class="muted">Missing reported cost is not zero</small></div>
    </section>
    <div class="overview-grid">
      ${panel("Handoff & review coverage", coverageChart(objectives[0].handoffs, objectives[0].reviews), '<span class="eyebrow">SCOPED COUNTS · FIXTURE</span>', "throughput-panel")}
      ${panel("Human action queue", `<div class="attention-summary"><span class="big-number mono orange">${String(open.length).padStart(2, "0")}</span><div><strong>Explicit authority requests</strong><p class="muted small">Each decision names an exact subject.</p></div></div><div class="queue-list">${open.map((item) => `<a class="queue-row" href="#/actions/${item.objective}"><div><span class="eyebrow">${item.id} · ${item.age}</span><h3>${item.type === "Publication" ? "Publication approval" : "Plan approval"}</h3><span class="muted small">${item.title}</span></div>${icon("arrow", 16)}</a>`).join("")}</div>`, link("Open decision inbox", "actions"), "attention-panel")}
      ${panel("Active objectives", objectiveRows(objectives.slice(0, 4), true), link("All objectives", "objectives"), "active-panel")}
      ${panel("Execution activity", `<div class="capacity-total"><span class="mono">${executing}</span><span class="muted small">executing runs · fixture</span></div><div class="agents-list">${agents.map((agent) => `<button class="agent-row" data-inspect-agent="${agent.id}"><span class="avatar">${agent.initials}</span><span class="agent-name"><strong>${agent.id}</strong><span class="muted small mono">${agent.kind}</span></span><span class="mono small">${agent.used} active</span></button>`).join("")}</div><footer class="panel-footer muted small">Capacity ceiling unavailable. Per-agent slot limits are not exposed.</footer>`, link("Inspect workers", "agents"), "capacity-panel")}
    </div>
    ${disclosure("Throughput and trends: measurement unavailable", "<p>Current PR identities are observable, but an accepted-change series needs an exact revision, acceptance event, producer, timestamp and deduplication rule. Supervision time also needs a measured denominator. Do not derive these from provider completion.</p>", "Data source requirements")}
    ${panel(
      "Recent results",
      `<div class="results-list">${[
        ["PR #590", "Bounded event replay", "External review recorded", "14:04"],
        ["PR #581", "Evidence lifecycle clarified", "Exact candidate review recorded", "13:45"],
        ["PR #576", "Controller binding preserved", "Publication observed; merge external", "13:18"],
      ]
        .map(
          ([id, title, note, time]) =>
            `<a href="#/objectives/${id === "PR #581" ? "OBJ-581" : "OBJ-590"}" class="result-row"><span class="result-icon">${icon("check", 16)}</span><strong>${title}</strong><span class="mono small muted">${id}</span><span class="muted small result-note">${note}</span><time class="small">${time}</time>${icon("arrow", 14)}</a>`,
        )
        .join("")}</div>`,
      '<span class="eyebrow">FIXTURE HISTORY</span>',
    )}`;
}
