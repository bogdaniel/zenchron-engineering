import { useState } from "react";
import type { Context, Controller, PlanIndex, Runs } from "./api";
import { blocker } from "./api";
import { Badge, Binding, ControllerFacts, Disclosure, Empty, Head, Link, Pagination, Panel } from "./components";

export function PlanTable({ index }: { index: PlanIndex }) {
  return index.plans.length ? (
    <div className="table-scroll" tabIndex={0} role="region" aria-label="Objective source contexts">
      <table className="data-table">
        <thead>
          <tr>
            <th scope="col">Source / plan</th>
            <th scope="col">Revision state</th>
            <th scope="col">Human request</th>
            <th scope="col">Navigation</th>
          </tr>
        </thead>
        <tbody>
          {index.plans.map((p) => (
            <tr key={p.plan_id}>
              <td data-label="Source / plan">
                <Link to={`objectives/${encodeURIComponent(p.plan_id)}`}>
                  {p.source_issue ? `Issue #${p.source_issue} · ${p.repository}` : p.repository}
                </Link>
                <small className="row-meta mono">
                  {p.plan_id} · {p.current_revision ? `r${p.current_revision}` : "No compiled revision"}
                </small>
              </td>
              <td data-label="Revision state">
                <Badge>{p.shown}</Badge>
              </td>
              <td data-label="Human request">
                {p.pending ? "Pending · eligibility checked separately" : "None pending on this revision"}
              </td>
              <td data-label="Navigation">
                {p.pending && (
                  <Link to={`actions/${encodeURIComponent(p.plan_id)}/${p.current_revision}`}>Review request</Link>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  ) : (
    <Empty title="No objective source contexts in this page">
      Search repository, source issue or plan ID. A refused planning attempt has no executable revision.
    </Empty>
  );
}

export function Overview({
  controller,
  index,
  runs,
  stale,
}: {
  controller: Controller;
  index: PlanIndex;
  runs: Runs;
  stale: boolean;
}) {
  const pending = index.plans.filter((p) => p.pending);
  const dimensions = [
    ["Intent", index.plans.length.toString(), "Plan source contexts loaded"],
    ["Plan", pending.length.toString(), "Revisions awaiting a human decision"],
    ["Execute", runs.runs.filter((r) => r.executing).length.toString(), "Executing runs loaded"],
    ["Assure", "Unavailable", "No aggregate evidence projection"],
    ["Authorize", stale || blocker(controller) ? "0" : pending.length.toString(), "Rejections available at read time"],
    ["Publish", "Unavailable", "No accepted-change series"],
  ];
  return (
    <>
      <Head eyebrow="OPERATIONS / FACTORY FLOOR" title="Engineering overview">
        Observed API data · independent scopes; activity overlaps.
      </Head>
      <div className="section-label">
        <span>ENGINEERING ACTIVITY</span>
        <span>Loaded pages; not exclusive lifecycle states</span>
      </div>
      <section className="pipeline" aria-label="Overlapping engineering activity">
        {dimensions.map(([label, value, note]) => (
          <div className="pipeline-cell" key={label}>
            <span className="eyebrow">{label}</span>
            <div className={value === "Unavailable" ? "runtime-unavailable" : "pipeline-value"}>{value}</div>
            <p>{note}</p>
          </div>
        ))}
      </section>
      <p className="flow-explanation muted">
        Counts cover at most 50 plans and 50 runs{index.has_more || runs.has_more ? "; further pages exist" : ""}. Reads
        are independent observations. No completion percentage is inferred.
      </p>
      <div className="overview-grid">
        <Panel title="Active objective contexts" extra={<Link to="objectives">Browse portfolio</Link>}>
          <PlanTable index={index} />
        </Panel>
        <Panel title="Human authority" extra={<Badge tone="orange">{pending.length} pending</Badge>}>
          <div className="product-body">
            <p>{blocker(controller) || "Plan rejection may be offered; the server verifies every submission."}</p>
            <p>Approval and publication remain unavailable in this migration batch.</p>
            <Link to="actions" className="button">
              Inspect requests
            </Link>
          </div>
        </Panel>
      </div>
      <div className="overview-grid">
        <Panel title="Controller dimensions">
          <ControllerFacts controller={controller} stale={stale} />
        </Panel>
        <Panel title="Execution capacity & recent results">
          <div className="product-body">
            <p>
              {runs.runs.filter((r) => r.executing).length} executing runs in loaded page · installed-agent capacity
              ceiling unavailable.
            </p>
            {runs.runs.length ? (
              <ul className="diagnostic-list">
                {runs.runs.slice(0, 5).map((r) => (
                  <li key={r.run_id}>
                    <span className="mono exact-value">{r.run_id}</span>
                    <br />
                    {r.phase} · {r.disposition}
                    <br />
                    Agent {r.agent || "unassigned"}
                  </li>
                ))}
              </ul>
            ) : (
              <Empty title="No runs observed">An empty page does not imply successful execution.</Empty>
            )}
          </div>
        </Panel>
      </div>
      <Disclosure title="Measurement availability">
        <p>
          Throughput, trends, cycle time, supervision hours and total agent capacity have no complete source here.
          Missing measurements stay unavailable.
        </p>
      </Disclosure>
    </>
  );
}

export function Portfolio({
  index,
  query,
  filter,
  onQuery,
  onFilter,
  onPage,
}: {
  index: PlanIndex;
  query: string;
  filter: string;
  onQuery: (q: string) => void;
  onFilter: (v: string) => void;
  onPage: (v: number) => void;
}) {
  const [draft, setDraft] = useState(query);
  const visible = {
    ...index,
    plans: index.plans.filter((p) => filter === "all" || (filter === "pending" ? p.pending : !p.pending)),
  };
  return (
    <>
      <Head eyebrow="OPERATIONS / PORTFOLIO" title="Objectives">
        Plan-governed source contexts. Private objective prose and product association are not exposed by this read API.
      </Head>
      <form
        className="portfolio-toolbar"
        onSubmit={(e) => {
          e.preventDefault();
          onQuery(draft);
        }}
      >
        <label className="search-field">
          Search source contexts
          <input
            type="search"
            aria-label="Search objectives"
            value={draft}
            maxLength={200}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="Repository, issue, plan ID"
          />
        </label>
        <button className="button" type="submit">
          Search
        </button>
        <label>
          Request filter{" "}
          <select aria-label="Request filter" value={filter} onChange={(e) => onFilter(e.target.value)}>
            <option value="all">All in loaded page</option>
            <option value="pending">Pending human requests</option>
            <option value="other">No pending request</option>
          </select>
        </label>
      </form>
      <Panel title="Objective source contexts" extra={<span className="muted small">50-row page bound</span>}>
        <PlanTable index={visible} />
        <Pagination offset={index.offset} more={index.has_more} count={index.plans.length} onPage={onPage} />
      </Panel>
      <p className="muted small">
        Search applies to the full persisted plan index. The request filter applies to this page. A pending request is
        not a claim of current authority.
      </p>
    </>
  );
}

export function Workspace({ context }: { context: Context }) {
  const p = context.plan;
  const [selected, setSelected] = useState(p.stages[0]?.stage_id || "");
  const stage = p.stages.find((s) => s.stage_id === selected);
  return (
    <>
      <Head
        eyebrow="OBJECTIVE / GOVERNED PLAN CONTEXT"
        title={`${p.repository} · plan r${p.revision}`}
        extra={
          context.pending && (
            <Link to={`actions/${encodeURIComponent(p.plan_id)}/${p.revision}`} className="button primary">
              Review human request
            </Link>
          )
        }
      >
        {p.repository} · shown r{p.revision} {p.shown} · governing{" "}
        {p.governing_revision ? `r${p.governing_revision}` : "none"}
      </Head>
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel title="Engineering lifecycle">
            <div className="product-body">
              <p>
                {context.pending
                  ? "Awaiting an operator decision on this exact plan revision."
                  : context.receipt
                    ? `Historical ${context.receipt.status} decision recorded.`
                    : "No pending decision on this revision."}
              </p>
              <p>
                A proposal does not govern execution. Only agent stages create runs; assurance and human gates reference
                existing evidence and authority.
              </p>
            </div>
          </Panel>
          <Panel
            title="Plan dependency inspection"
            extra={<span className="muted small">Plan stages · separate from WorkGraph</span>}
          >
            <div className="plan-dependency-grid">
              {p.stages.map((s) => (
                <button
                  key={s.stage_id}
                  className={`graph-node ${s.stage_id === selected ? "selected" : ""}`}
                  aria-pressed={s.stage_id === selected}
                  onClick={() => setSelected(s.stage_id)}
                >
                  <span className="eyebrow">{s.kind}</span>
                  <strong>{s.stage_id}</strong>
                  <span className="small muted">{s.state || "State not recorded"}</span>
                  <span className="small">After: {s.depends_on.join(", ") || "No stage dependency"}</span>
                </button>
              ))}
            </div>
            {stage && (
              <div className="product-body" role="region" aria-label="Selected stage">
                <h3>{stage.stage_id}</h3>
                <dl className="binding-list">
                  <dt>Required role</dt>
                  <dd>{stage.role || "No agent role"}</dd>
                  <dt>Bound profile</dt>
                  <dd>{stage.bound_profile || "Not bound"}</dd>
                  <dt>Execution agent</dt>
                  <dd>{stage.agent || "Not bound"}</dd>
                  <dt>Provider / trust</dt>
                  <dd>
                    {stage.provider_kind || "Unknown"} / {stage.trust_mode || "Unknown"}
                  </dd>
                  <dt>Run</dt>
                  <dd>{stage.run_id || (stage.kind === "agent" ? "Not started" : "Gate creates no run")}</dd>
                </dl>
                {stage.review && (
                  <p>
                    Review {stage.review.verdict} · candidate{" "}
                    <span className="mono exact-value">{stage.review.candidate}</span> · tree{" "}
                    <span className="mono exact-value">{stage.review.tree}</span>
                  </p>
                )}
              </div>
            )}
          </Panel>
          <Panel title="WorkGraph">
            <Empty title="No plan-to-WorkGraph association exposed">
              A WorkUnit is issue-backed operational work; a PlanStage is a planning responsibility. This application
              does not fabricate an association. WorkGraph migration remains in the parity inventory.
            </Empty>
          </Panel>
          <Panel title="Decisions & activity">
            {context.receipt ? (
              <ol className="activity-timeline">
                <li>
                  <time dateTime={context.receipt.occurred_at}>{context.receipt.occurred_at.slice(11, 16)}</time>
                  <span className="timeline-dot muted" />
                  <div>
                    <strong>
                      Plan {context.receipt.status} · r{context.receipt.revision}
                    </strong>
                    <p className="muted small">Historical journal sequence {context.receipt.sequence}</p>
                    <Link to={`actions/${encodeURIComponent(p.plan_id)}/${p.revision}`}>Inspect exact receipt</Link>
                  </div>
                </li>
              </ol>
            ) : (
              <Empty title="No decision receipt on this revision">
                Full plan activity pagination remains a migration gap; missing history is not an empty authoritative
                timeline.
              </Empty>
            )}
          </Panel>
        </div>
        <aside className="workspace-aside">
          <Panel title="Budget envelope">
            <dl className="binding-list summary-binding">
              <dt>Child runs</dt>
              <dd>
                {p.budget.child_runs} / {p.budget.max_child_runs}
              </dd>
              <dt>Invocations</dt>
              <dd>
                {p.budget.provider_invocations} / {p.budget.max_provider_invocations}
              </dd>
              <dt>Cost</dt>
              <dd>
                {p.budget.cost_known && p.budget.cost_micros !== undefined ? `${p.budget.cost_micros} µUSD` : "Unknown"}
              </dd>
            </dl>
          </Panel>
          <Panel title="Exact subject">
            <Disclosure title="Technical binding">
              <Binding plan={p} />
            </Disclosure>
          </Panel>
          <Panel title="Publication">
            <div className="product-body">
              <p>
                No publication or merge authority is granted by this page. Plan approval integration is pending
                assignment-binding parity.
              </p>
            </div>
          </Panel>
        </aside>
      </div>
    </>
  );
}
