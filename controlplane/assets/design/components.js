export function esc(value) {
  return String(value).replace(
    /[&<>"']/g,
    (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char],
  );
}

const paths = {
  overview: "M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z",
  objectives: "M5 4h14v17H5z M9 4V2h6v2 M9 9h6 M9 13h6 M9 17h4",
  actions: "M12 3 2 20h20L12 3Z M12 9v5 M12 17v1",
  products: "M3 8 12 3l9 5v10l-9 5-9-5V8Z M3 8l9 5 9-5 M12 13v10",
  agents: "M7 7h10v10H7z M10 10h.1 M14 10h.1 M10 14h4 M12 3v4 M12 17v4 M3 12h4 M17 12h4",
  controller: "M3 6h18v12H3z M7 10h.1 M7 14h.1 M11 10h6 M11 14h6",
  arrow: "M5 12h14 M14 7l5 5-5 5",
  search: "M11 18a7 7 0 1 1 0-14 7 7 0 0 1 0 14Z M16 16l5 5",
  theme: "M12 3a9 9 0 1 0 9 9 7 7 0 0 1-9-9Z",
  check: "M5 12l4 4L19 6",
  close: "M6 6l12 12 M18 6 6 18",
  menu: "M4 6h16 M4 12h16 M4 18h16",
  graph: "M4 3h6v6H4z M14 15h6v6h-6z M4 15h6v6H4z M7 9v6 M10 6h7v9",
  history: "M3 11a9 9 0 1 1 3 8 M3 4v7h7 M12 7v5l3 3",
  help: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20Z M9 8a3 3 0 1 1 5 2c-2 1-2 2-2 4 M12 17h.1",
};
export function icon(name, size = 18) {
  return `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="${paths[name] || paths.arrow}"/></svg>`;
}

export function badge(label, tone) {
  const resolved =
    tone ||
    {
      Executing: "green",
      Accepted: "green",
      Review: "neutral",
      Decision: "orange",
      Planning: "neutral",
      Remediation: "orange",
      Queued: "neutral",
      "Stale subject": "orange",
      "Ready for decision": "green",
    }[label] ||
    "neutral";
  return `<span class="badge ${resolved}"><span class="status-dot" aria-hidden="true"></span>${esc(label)}</span>`;
}

export function link(label, destination, className = "text-link") {
  return `<a class="${className}" href="#/${esc(destination)}">${esc(label)} ${icon("arrow", 14)}</a>`;
}

export function panel(title, content, extra = "", className = "") {
  return `<section class="panel ${className}"><header class="panel-head"><h2>${title}</h2>${extra}</header>${content}</section>`;
}

export function budgetMeter(consumed, limit) {
  return `<span class="progress" role="meter" aria-label="Active work budget consumed, not completion" aria-valuemin="0" aria-valuemax="${limit}" aria-valuenow="${consumed}"><svg viewBox="0 0 100 3" preserveAspectRatio="none" aria-hidden="true"><rect width="${(100 * consumed) / limit}" height="3"/></svg></span>`;
}

export function disclosure(title, content, hint = "") {
  return `<details class="disclosure"><summary><span>${esc(title)}</span><small>${esc(hint)}</small>${icon("arrow", 16)}</summary><div class="disclosure-body">${content}</div></details>`;
}

export const viewPageSize = 50;
export function pageWindow(items, offset = 0) {
  const last = Math.max(0, Math.floor((items.length - 1) / viewPageSize) * viewPageSize);
  const start = Number.isInteger(offset) && offset >= 0 ? Math.min(offset, last) : 0;
  return { items: items.slice(start, start + viewPageSize), offset: start, total: items.length };
}
export function pageControls(scope, page) {
  return `<div class="page-controls" role="group" aria-label="${esc(scope)} pagination"><span>${page.total ? page.offset + 1 : 0}–${page.offset + page.items.length} of ${page.total}</span><button class="button" data-page="${scope}" data-offset="${Math.max(0, page.offset - viewPageSize)}" ${page.offset === 0 ? "disabled" : ""}>Previous</button><button class="button" data-page="${scope}" data-offset="${page.offset + viewPageSize}" ${page.offset + page.items.length >= page.total ? "disabled" : ""}>Next</button></div>`;
}

export function coverageChart(handoffs, reviews) {
  const rows = [
    ["Admitted handoffs", handoffs.admitted, handoffs.total],
    ["Independent reviews", reviews.completed, reviews.total],
  ];
  return `<div class="coverage-chart"><p class="muted">OBJ-612 · WorkGraph revision 2 · counts may overlap</p>${rows.map(([label, count, total]) => `<div class="coverage-row"><span>${label}</span><span class="mono">${count} of ${total}</span></div><svg viewBox="0 0 100 3" preserveAspectRatio="none" aria-hidden="true"><rect width="100" height="3" class="chart-track"/><rect width="${(100 * count) / total}" height="3" class="chart-point"/></svg>`).join("")}<p class="small muted">A handoff satisfies a dependency. A review binds to its candidate. Neither measures remaining engineering effort.</p></div>`;
}

export function groundedProgress(objective) {
  if (objective.handoffs) return `${objective.handoffs.admitted} of ${objective.handoffs.total} admitted handoffs`;
  if (objective.reviews) return `${objective.reviews.completed} of ${objective.reviews.total} reviews completed`;
  return "Awaiting plan approval";
}

export function objectiveRows(items, compact = false) {
  return `<div class="table-scroll" tabindex="0" role="region" aria-label="Objectives table"><table class="data-table"><caption class="sr-only">Fixture engineering objectives</caption><thead><tr><th scope="col">Objective</th><th scope="col">Lifecycle</th>${compact ? "" : '<th scope="col">Product scope</th><th scope="col">Worker</th>'}<th scope="col">Next boundary</th><th scope="col">Observed milestones</th></tr></thead><tbody>
  ${items.map((o) => `<tr><td><a class="objective-title" href="#/objectives/${o.id}">${esc(o.title)}</a><div class="row-meta"><span class="mono">${o.id}</span><span>#${o.issue}</span><span>${o.updated === "Now" ? "Just now" : esc(o.updated) + " ago"}</span></div></td><td>${badge(o.state)}</td>${compact ? "" : `<td>${esc(o.product)}</td><td class="mono small">${esc(o.agent)}</td>`}<td>${o.blocker === "—" ? '<span class="muted">Within contract</span>' : link(o.blocker, o.decisionNeeded ? `actions/${o.id}` : `objectives/${o.id}/${o.state === "Queued" ? "workgraph" : "evidence"}`)}</td><td><span class="milestone-count">${esc(groundedProgress(o))}</span></td></tr>`).join("")}
  </tbody></table></div>`;
}

export function pageHead(eyebrow, title, description, actions = "") {
  return `<div class="page-head"><div><div class="eyebrow">${eyebrow}</div><h1>${title}</h1><p class="page-description">${description}</p></div><div class="page-actions">${actions}</div></div>`;
}

export function emptyState(title, description, action = "") {
  return `<div class="empty-state">${icon("objectives", 28)}<h2>${esc(title)}</h2><p>${esc(description)}</p>${action}</div>`;
}
