import { RunControls } from "./run-controls";
import { PublicationControls } from "./human-requests";
import { useState } from "react";
import type { Controller, Runs } from "./api";
import * as ops from "./operations-api";
import { Badge, Disclosure, Empty, Head, Link, Pagination, Panel } from "./components";
import { JournalHistory } from "./history";
import { StopRun } from "./stop-run";

export function Facts({ rows }: { rows: [string, string | number][] }) {
  return (
    <dl className="binding-list summary-binding runtime-facts">
      {rows.map(([label, value]) => (
        <div className="fact-pair" key={label}>
          <dt>{label}</dt>
          <dd className="exact-value">{value === "" ? "Not recorded" : value}</dd>
        </div>
      ))}
    </dl>
  );
}
export function elapsed(ns: number) {
  const seconds = Math.floor(ns / 1e9);
  if (seconds < 60) return `≈ ${seconds}s`;
  if (seconds < 3600) return `≈ ${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  return `≈ ${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}
function sourceLink(value: string) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password && !url.search && !url.hash ? url.href : "";
  } catch {
    return "";
  }
}
export function RunPortfolio({
  runs,
  onPage,
  status,
  source,
  onStatus,
  onSource,
}: {
  runs: Runs;
  onPage: (v: number) => void;
  status: string;
  source: string;
  onStatus: (v: string) => void;
  onSource: (v: string) => void;
}) {
  const [query, setQuery] = useState(""),
    [draft, setDraft] = useState(source);
  const rows = runs.runs.filter(
    (r) => !query || [r.run_id, r.repository, r.agent].some((v) => v.toLowerCase().includes(query.toLowerCase())),
  );
  return (
    <>
      <Head eyebrow="OPERATIONS / EXECUTION" title="Runs & activity">
        Source and disposition filters apply to the full fleet before paging. Keyword filtering applies to the loaded
        page.
      </Head>
      <Panel title="Run inventory" extra={<Badge>{runs.runs.length} loaded</Badge>}>
        <div className="filter-bar">
          <label className="search-input">
            <span className="sr-only">Filter loaded runs</span>
            <input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Repository, run or agent"
            />
          </label>
          <select aria-label="Run disposition" value={status} onChange={(e) => onStatus(e.target.value)}>
            {["all", "active", "waiting", "completed", "failed", "cancelled", "held"].map((s) => (
              <option value={s} key={s}>
                {s === "all" ? "All dispositions" : s}
              </option>
            ))}
          </select>
        </div>
        <form
          className="filter-bar"
          onSubmit={(e) => {
            e.preventDefault();
            onSource(draft);
          }}
        >
          <label className="search-input">
            <span className="sr-only">Source issue</span>
            <input
              type="search"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              maxLength={200}
              placeholder="owner/repo#123 or 123"
            />
          </label>
          <button className="button">Filter source</button>
        </form>
        {rows.length ? (
          <div className="table-scroll" tabIndex={0} role="region" aria-label="Run inventory">
            <table className="data-table">
              <thead>
                <tr>
                  {["Run / source", "Lifecycle", "Execution agent", "Observed operation"].map((s) => (
                    <th scope="col" key={s}>
                      {s}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.run_id}>
                    <td data-label="Run / source">
                      <Link to={`runs/${encodeURIComponent(r.run_id)}`}>
                        {r.repository || "Source not recorded"}
                        {r.issue ? ` · #${r.issue}` : ""}
                      </Link>
                      <small className="row-meta mono exact-value">{r.run_id}</small>
                    </td>
                    <td data-label="Lifecycle">
                      <Badge>{r.unavailable ? "Observation unavailable" : `${r.phase} · ${r.disposition}`}</Badge>
                    </td>
                    <td data-label="Execution agent">{r.agent || "Identity not recorded"}</td>
                    <td data-label="Observed operation">
                      {r.unavailable
                        ? "Unknown"
                        : r.executing
                          ? "Executing at read time"
                          : "Not executing at read time"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty title="No matching runs in this page">
            Adjust the local filters or browse another page. This does not prove that the fleet is idle.
          </Empty>
        )}
        <Pagination
          label="Run pagination"
          offset={runs.offset}
          more={runs.has_more}
          count={runs.runs.length}
          onPage={onPage}
        />
      </Panel>
    </>
  );
}
export function RunWorkspace({
  run: r,
  controller,
  stale,
  after,
}: {
  run: ops.RunDetail;
  controller: Controller;
  stale: boolean;
  after: number;
}) {
  const sourceURL = sourceLink(r.source?.url || "");
  return (
    <>
      <div className="breadcrumb">
        <Link to="runs">Runs & activity</Link>
        <span>/</span>
        <span>Execution inspection</span>
      </div>
      <Head
        eyebrow="EXECUTION / RUN WORKSPACE"
        title={r.source?.issue ? `${r.repository} · issue #${r.source.issue}` : r.repository || "Run workspace"}
        extra={
          <Badge>
            {r.phase} · {r.disposition}
          </Badge>
        }
      >
        Execution, evidence and authority are distinct. This read does not grant publication or merge authority.
      </Head>
      <div className="metric-strip runtime-metrics">
        {[
          ["Elapsed", r.elapsed],
          ["Active", r.active_elapsed],
          ["External wait", r.external_wait_elapsed],
        ].map(([label, value]) => (
          <div key={label}>
            <span className="eyebrow">{label}</span>
            <strong className="mono">{elapsed(Number(value))}</strong>
            <small>Recorded duration</small>
          </div>
        ))}
      </div>
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel title="Governed control">
            <StopRun run={r} controller={controller} stale={stale} />
            <RunControls run={r} controller={controller} stale={stale} />
            <PublicationControls runId={r.run_id} controller={controller} stale={stale} />
          </Panel>
          <Panel title="Execution observation">
            <Facts
              rows={[
                ["Run identity", r.run_id],
                ["Source repository", r.source?.repository || "Not recorded"],
                ["Agent", r.worker?.agent || ""],
                ["Provider / model", `${r.worker?.provider_kind || "Unknown"} / ${r.worker?.model || "Unknown"}`],
                ["Execution trust", r.worker?.trust_mode || "Unknown"],
                [
                  "Run controller build",
                  r.controller
                    ? `${r.controller.build_kind || "Unknown"} / ${r.controller.build_version || "Not recorded"}`
                    : "Not recorded",
                ],
                [
                  "Run controller continuity",
                  r.controller
                    ? r.controller.changed
                      ? "Controller differs from the run's recorded generation"
                      : "No change reported"
                    : "Not recorded",
                ],
                ["Workspace binding", r.worker ? (r.worker.workspace_bound ? "Bound" : "Not bound") : "Unknown"],
                [
                  "Source intent",
                  r.source
                    ? r.source.intent_changed
                      ? "Changed; reassessment required"
                      : "No change reported"
                    : "Not recorded",
                ],
              ]}
            />
            {sourceURL && (
              <div className="panel-footer">
                <a className="text-link" href={sourceURL} target="_blank" rel="noopener noreferrer">
                  Open recorded source issue ↗
                </a>
              </div>
            )}
          </Panel>
          <Panel title="Evidence & publication">
            <Facts
              rows={[
                [
                  "Assurance",
                  r.assurance
                    ? `${r.assurance.passed ? "Passed" : "Failed"}${r.assurance.stale ? " · stale" : ""}`
                    : "Not recorded",
                ],
                ["Failure class", r.assurance?.failure_class || "Not recorded"],
                ["Publication authority", r.publication_authority?.status || "Not recorded"],
                ["Protected action", r.publication_authority?.action_type || "Not recorded"],
                ["Action target", r.publication_authority?.action_target || "Not recorded"],
                ["Authority request", r.authority_request?.status || "Not recorded"],
                ["Required obligations", r.authority_request?.requires.join(", ") || "Not recorded"],
                [
                  "Missing / stale",
                  `${r.authority_request?.missing.join(", ") || "None reported"} / ${r.authority_request?.stale.join(", ") || "None reported"}`,
                ],
                [
                  "Pull request",
                  r.pull_request
                    ? `#${r.pull_request.number} · ${r.pull_request.state || "Unknown"}${r.pull_request.merged ? " · merged" : ""}${r.pull_request.stale ? " · stale" : ""}`
                    : "Not recorded",
                ],
              ]}
            />
            <div className="panel-footer">
              <p>
                Human publication evidence is recorded through <span className="mono">autonomy authorize</span>. This
                browser has no publication or approval action.
              </p>
            </div>
          </Panel>
          <Panel title="Recovery & held material">
            {r.held_material || r.execution_diagnostic || r.candidate_discard_refusals ? (
              <>
                <Facts
                  rows={[
                    ["Held material", r.held_material?.kind || "None reported"],
                    ["Held revision", r.held_material?.revision || ""],
                    ["Blocked by", r.held_material?.blocked_by || ""],
                    ["Next step", r.held_material?.next_step || ""],
                    ["Diagnostic stage", r.execution_diagnostic?.stage || ""],
                    [
                      "Failure / bound",
                      `${r.execution_diagnostic?.failure_class || "Unknown"} / ${r.execution_diagnostic?.bound || "Unknown"}`,
                    ],
                    ["Successor", r.held_material?.successor || r.execution_diagnostic?.successor || "Not recorded"],
                    [
                      "Successor unavailable",
                      r.held_material?.successor_unavailable ||
                        r.execution_diagnostic?.successor_unavailable ||
                        "None reported",
                    ],
                    ["Discard refusals", r.candidate_discard_refusals],
                    ["Discard refusal", r.candidate_discard_refused || "None reported"],
                  ]}
                />
              </>
            ) : (
              <Empty title="No recovery or held material reported">
                A run's terminal state and evidence remain independently inspectable.
              </Empty>
            )}
          </Panel>
          <JournalHistory key={`${r.run_id}/${after}`} id={r.run_id} initialAfter={after} />
        </div>
        <aside className="workspace-aside">
          <Panel title="Operation & liveness">
            {r.operation ? (
              <Facts
                rows={[
                  ["Operation", `${r.operation.kind || "Unknown"} · ${r.operation.state}`],
                  ["Attempt", `${r.operation.attempt} / ${r.operation.max_attempts || "Unknown ceiling"}`],
                  ["Progress source", r.operation.progress_source],
                  ["Started", r.operation.started_at || "Not recorded"],
                  ["Last heartbeat", r.operation.heartbeat_at || "Not recorded"],
                  ["Last progress", r.operation.last_progress_at || "Not recorded"],
                  ["Silence", elapsed(r.operation.silent_for)],
                  [
                    "Inactivity ceiling",
                    r.operation.inactivity_limit_unknown ? "Unknown" : elapsed(r.operation.inactivity_limit),
                  ],
                  ["Suspension", r.operation.inactivity_suspension || "None reported"],
                  ["Deadline", r.operation.deadline || "Not recorded"],
                  ["Deadline bound", r.operation.deadline_bound || "Not recorded"],
                ]}
              />
            ) : (
              <Empty title="No active operation recorded">Heartbeat absence does not prove controller failure.</Empty>
            )}
          </Panel>
          <Panel title="Exact subject">
            <Disclosure title="Candidate & contract binding">
              <Facts
                rows={[
                  ["Candidate revision", r.candidate?.revision || ""],
                  ["Candidate tree", r.candidate?.tree || ""],
                  ["Branch", r.candidate?.branch || ""],
                  ["Base identity", r.base?.id || ""],
                  ["Base revision", r.base?.revision || ""],
                  ["Contract identity", r.contract?.id || ""],
                  ["Contract revision", r.contract?.revision || ""],
                ]}
              />
            </Disclosure>
          </Panel>
          <Panel title="Budget observations">
            {r.budgets ? (
              <Facts
                rows={[
                  ["Wall ceiling", r.budgets.wall_limit ? elapsed(r.budgets.wall_limit) : "Not recorded"],
                  [
                    "Lifecycle bound",
                    r.budgets.lifecycle_deadline ? elapsed(r.budgets.lifecycle_deadline) : "Not recorded",
                  ],
                  ["Execution attempts", r.budgets.max_execution_attempts],
                  ["Continuations", r.budgets.max_execution_continuations],
                  ["Remediation attempts", r.budgets.max_remediation_attempts],
                  ["Assurance attempts", r.budgets.max_assurance_attempts],
                  ...Object.entries(r.attempts).map(([key, value]): [string, number] => [`Consumed ${key}`, value]),
                ]}
              />
            ) : (
              <Empty title="Budget ceilings unavailable">
                {r.budgets_unverifiable.join(", ") || "No verifiable run policy observed."}
              </Empty>
            )}
          </Panel>
        </aside>
      </div>
    </>
  );
}
