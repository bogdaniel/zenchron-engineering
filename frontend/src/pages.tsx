import { PlanControls } from "./planning";
import { GraphWorkspace } from "./workgraphs";
import type { GraphDetail } from "./operations-api";
import { useState } from "react";
import type { Context, Controller, PlanIndex, Runs } from "./api";
import { blocker } from "./api";
import {
  Badge,
  Binding,
  ControllerFacts,
  Disclosure,
  Empty,
  FilterBar,
  Head,
  Link,
  Pagination,
  Panel,
  SearchInput,
} from "./components";
import { NumberField } from "./form";
import { PlanDependencyGraph } from "./dependency-graph";
import { Facts } from "./runs";
import { JournalHistory } from "./history";
import { ObjectiveOutcome } from "./outcome";
import type { Fleet } from "./operations-api";

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
                <Link to={`${p.current_revision ? "objectives" : "attempts"}/${encodeURIComponent(p.plan_id)}`}>
                  {p.source_issue ? `Issue #${p.source_issue} · ${p.repository}` : p.repository}
                </Link>
                <small className="row-meta mono">
                  {p.plan_id} · {p.current_revision ? `r${p.current_revision}` : "No compiled revision"}
                </small>
              </td>
              <td data-label="Revision state">
                <Badge>{p.shown}</Badge>
                <small className="row-meta">
                  Controller: {p.lifecycle && !p.lifecycle.unavailable ? p.lifecycle.state : "Unavailable"}
                </small>
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
  fleet,
}: {
  controller: Controller;
  index: PlanIndex;
  runs: Runs;
  stale: boolean;
  fleet?: Fleet;
}) {
  const pending = index.plans.filter((p) => p.pending);
  const executing = runs.runs.filter((r) => !r.unavailable && r.executing).length;
  const unavailable = runs.runs.filter((r) => r.unavailable).length;
  const dimensions = [
    ["Intent", index.plans.length.toString(), "Plan source contexts loaded"],
    ["Plan", pending.length.toString(), "Revisions awaiting a human decision"],
    [
      "Execute",
      `${unavailable ? "≥" : ""}${executing}`,
      `Executing runs loaded${unavailable ? ` · ${unavailable} unknown` : ""}`,
    ],
    ["Assure", "Unavailable", "No aggregate evidence projection"],
    [
      "Authorize",
      stale || blocker(controller) ? "0" : pending.length.toString(),
      "Plan rejections available at read time",
    ],
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
        Activity-strip counts cover at most 50 plans and 50 runs
        {index.has_more || runs.has_more ? "; further pages exist" : ""}. Reads are independent observations. No
        completion percentage is inferred.
      </p>
      <div className="overview-grid">
        <Panel title="Active objective contexts" extra={<Link to="objectives">Browse portfolio</Link>}>
          <PlanTable index={index} />
        </Panel>
        <Panel title="Human authority" extra={<Badge tone="orange">{pending.length} pending</Badge>}>
          <div className="product-body">
            <p>{blocker(controller) || "Plan rejection may be offered; the server verifies every submission."}</p>
            <p>
              Approve exact plan assignments or inspect graph-owned decision/publication requests in Action Required.
            </p>
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
            {fleet && <p className="muted small">Fleet aggregate · all indexed runs · separate read</p>}
            {fleet && (
              <Facts
                rows={[
                  ["Nonterminal runs", fleet.active],
                  [
                    "Executing",
                    `${fleet.classes.unavailable ? "≥" : ""}${fleet.executing} · ${fleet.classes.unavailable} unavailable observations`,
                  ],
                  ["Work concurrency ceiling", fleet.capacity || "Unknown to this reader"],
                  ["Supervisor role presence", fleet.supervisor_running ? "Present at read time" : "Not reported"],
                ]}
              />
            )}
            <p>
              {executing} known executing runs in loaded page
              {unavailable ? ` · ${unavailable} unknown observations` : ""} · installed-agent capacity ceiling
              unavailable.
            </p>
            {runs.runs.length ? (
              <ul className="diagnostic-list">
                {runs.runs.slice(0, 5).map((r) => (
                  <li key={r.run_id}>
                    <Link to={`runs/${encodeURIComponent(r.run_id)}`}>{r.run_id}</Link>
                    <br />
                    {r.unavailable
                      ? "Observation unavailable · execution and result unknown"
                      : `${r.phase} · ${r.disposition}`}
                    <br />
                    Agent {r.agent || "Identity unavailable"}
                  </li>
                ))}
              </ul>
            ) : (
              <Empty title="No runs observed">An empty page does not imply successful execution.</Empty>
            )}
            {fleet && (
              <Disclosure title="Fleet dispositions & capacity classes">
                <Facts
                  rows={[
                    ...Object.entries(fleet.counts).map(([key, n]): [string, number] => [`Disposition / ${key}`, n]),
                    ...Object.entries(fleet.classes).map(([key, n]): [string, number] => [`Capacity / ${key}`, n]),
                  ]}
                />
                <p>Capacity classes partition nonterminal runs; they are separate from objective lifecycle stages.</p>
              </Disclosure>
            )}
            <Link to="runs">Inspect executions</Link>
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
      <Head
        eyebrow="OPERATIONS / PORTFOLIO"
        title="Objectives"
        extra={
          <Link to="intake" className="button primary">
            Create objective
          </Link>
        }
      >
        Plan-governed source contexts. Private objective prose and product association are not exposed by this read API.
      </Head>
      <Panel title="Objective source contexts" extra={<span className="muted small">50-row page bound</span>}>
        <FilterBar
          as="form"
          onSubmit={(e) => {
            e.preventDefault();
            onQuery(draft);
          }}
        >
          <SearchInput label="Search objectives" value={draft} maxLength={200} onChange={setDraft} placeholder="Repository, issue, plan ID" />
          <button className="button" type="submit">
            Search
          </button>
          <select aria-label="Request filter" value={filter} onChange={(e) => onFilter(e.target.value)}>
            <option value="all">All in loaded page</option>
            <option value="pending">Pending human requests</option>
            <option value="other">No pending request</option>
          </select>
        </FilterBar>
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

export function Workspace({
  context,
  graph,
  controller,
  stale,
  refresh,
}: {
  context: Context;
  graph?: GraphDetail;
  controller?: Controller;
  stale: boolean;
  refresh: () => void;
}) {
  const p = context.plan;
  const [selected, setSelected] = useState(p.stages[0]?.stage_id || "");
  const [revision, setRevision] = useState(p.revision);
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
      <FilterBar
        as="form"
        className="revision-picker"
        onSubmit={(e) => {
          e.preventDefault();
          location.hash = `#/objectives/${encodeURIComponent(p.plan_id)}/${revision}`;
        }}
      >
        <NumberField
          label="Plan revision"
          value={revision}
          min={1}
          max={2147483647}
          required
          onChange={(e) => setRevision(Number(e.target.value))}
        />
        <button className="button">Inspect revision</button>
        <Link to={`objectives/${encodeURIComponent(p.plan_id)}`}>Latest revision</Link>
        {p.governing_revision > 0 && (
          <Link to={`objectives/${encodeURIComponent(p.plan_id)}/${p.governing_revision}`}>
            Governing r{p.governing_revision}
          </Link>
        )}
      </FilterBar>
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel title="Engineering lifecycle">
            <div className="product-body">
              <p>
                Current controller projection:{" "}
                {context.lifecycle && !context.lifecycle.unavailable ? context.lifecycle.state : "Unavailable"} ·
                approved revision {context.lifecycle ? context.lifecycle.approved_revision || "none" : "unknown"}.
              </p>
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
              {(p.base_change || p.invalidated.length > 0) && (
                <Disclosure title="Revision change preview">
                  <Facts
                    rows={[
                      ["Invalidated stages", p.invalidated.join(", ") || "None reported"],
                      ["Previous base revision", p.base_change?.from || "Not recorded"],
                      ["Offered base revision", p.base_change?.to || "Not recorded"],
                      ["Compared plan revision", p.base_change?.from_revision ?? "Not recorded"],
                    ]}
                  />
                </Disclosure>
              )}
            </div>
          </Panel>
          <Panel
            title="Plan dependency inspection"
            extra={<span className="muted small">Plan stages · separate from WorkGraph</span>}
          >
            <PlanDependencyGraph stages={p.stages} selected={selected} onSelect={setSelected} />
            {stage && (
              <div className="product-body" role="region" aria-label="Selected stage">
                <h3>{stage.stage_id}</h3>
                <dl className="binding-list">
                  <dt>Prerequisite stages</dt>
                  <dd>{stage.depends_on.join(", ") || "No prerequisite stages"}</dd>
                  <dt>Dependent stages</dt>
                  <dd>
                    {p.stages.filter((s) => s.depends_on.includes(stage.stage_id)).map((s) => s.stage_id).join(", ") ||
                      "No dependent stages"}
                  </dd>
                  <dt>Required role</dt>
                  <dd>{stage.role || "Not recorded"}</dd>
                  <dt>Bound profile</dt>
                  <dd>{stage.bound_profile || "Not recorded"}</dd>
                  <dt>Required profile</dt>
                  <dd>{stage.profile || "No explicit profile required"}</dd>
                  <dt>Context policy</dt>
                  <dd>{stage.context_policy || "Not recorded"}</dd>
                  <dt>Assignment identity</dt>
                  <dd className="mono exact-value">{stage.assignment_id || "Not recorded"}</dd>
                  <dt>Execution agent</dt>
                  <dd>{stage.agent || "Not recorded"}</dd>
                  <dt>Provider / trust</dt>
                  <dd>
                    {stage.provider_kind || "Unknown"} / {stage.trust_mode || "Unknown"}
                  </dd>
                  <dt>Run</dt>
                  <dd>
                    {stage.run_id ? (
                      <Link to={`runs/${encodeURIComponent(stage.run_id)}`}>{stage.run_id}</Link>
                    ) : stage.kind === "agent" ? (
                      "Run not recorded"
                    ) : (
                      "Gate creates no run"
                    )}
                  </dd>
                </dl>
                {stage.review && (
                  <p>
                    Review {stage.review.verdict} · candidate{" "}
                    <span className="mono exact-value">{stage.review.candidate}</span> · tree{" "}
                    <span className="mono exact-value">{stage.review.tree}</span>
                  </p>
                )}
                {stage.gate && (
                  <Disclosure title="Gate evidence & authority references">
                    <Facts
                      rows={[
                        ["Satisfied claims", stage.gate.claims.join(", ") || "Not recorded"],
                        ["Evidence identity", stage.gate.evidence?.id || "Not recorded"],
                        ["Evidence revision", stage.gate.evidence?.revision || "Not recorded"],
                        ["Authority decision", stage.gate.decision?.id || "Not recorded"],
                        ["Decision revision", stage.gate.decision?.revision || "Not recorded"],
                        ["Human evidence", stage.gate.human_evidence_id || "Not recorded"],
                        ["Proven run heads", stage.gate.proven_heads.join(", ") || "Not recorded"],
                      ]}
                    />
                    <p>
                      Recorded gate satisfaction refers to these exact subjects. It is not a fresh verification of a
                      different candidate.
                    </p>
                  </Disclosure>
                )}
              </div>
            )}
          </Panel>
          <ObjectiveOutcome plan={p} pending={context.pending} />
          <Panel title="WorkGraph">
            {graph ? (
              <>
                {context.graph_binding_mismatch ? (
                  <p className="banner orange" role="status">
                    This graph binds a different exact plan revision/digest. Inspect the bound context before acting.
                  </p>
                ) : (
                  <p className="product-body">
                    Explicit objective binding · {graph.objective_plan_id} · r{graph.objective_plan_revision}
                  </p>
                )}
                <GraphWorkspace
                  graph={graph}
                  controller={controller}
                  stale={stale || context.graph_binding_mismatch}
                  embedded
                />
              </>
            ) : (
              <Empty title="No operational graph is bound">No association is inferred from a shared repository.</Empty>
            )}
          </Panel>
          <Panel title="Decision receipt">
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
                Inspect the plan journal below. An absent receipt grants no authority.
              </Empty>
            )}
          </Panel>
          <JournalHistory kind="plan" id={p.plan_id} />
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
              <dt>Concurrency ceiling</dt>
              <dd>{p.budget.max_concurrency}</dd>
              <dt>Wall seconds</dt>
              <dd>
                {p.budget.wall_seconds} / {p.budget.max_wall_seconds || "No ceiling recorded"}
              </dd>
            </dl>
          </Panel>
          <Panel title="Exact subject">
            <Disclosure title="Technical binding">
              <Binding plan={p} />
            </Disclosure>
          </Panel>
          <Panel title="Plan approval & revision">
            <div className="product-body">
              <PlanControls plan={p} controller={controller} stale={stale} onSettled={refresh} />
            </div>
          </Panel>
        </aside>
      </div>
    </>
  );
}
