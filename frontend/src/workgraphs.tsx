import { HumanDecision } from "./human-requests";
import * as api from "./api";
import type { Controller } from "./api";
import { useMemo, useState } from "react";
import type { GraphDetail, GraphRow, Graphs, ProductDetail, Products, Unit } from "./operations-api";
import { Badge, Disclosure, Empty, FilterBar, Head, Link, Pagination, Panel } from "./components";
import { TextField } from "./form";
import { GovernedAction, useReusableGovernedAction, type ActionReview } from "./governed-action";
import { freshController } from "./planning";
import {
  COLUMN_GAP,
  MARGIN,
  ROW_GAP,
  dependencyEdgePath,
  layoutDependencies,
  resolvedDependencies,
  unresolvedDependencies,
  useDependencyArrow,
} from "./dependency-graph";
import { Facts } from "./runs";

export function GraphTable({ rows }: { rows: GraphRow[] }) {
  return rows.length ? (
    <div className="table-scroll" tabIndex={0} role="region" aria-label="WorkGraph inventory">
      <table className="data-table">
        <thead>
          <tr>
            {["WorkGraph", "Repository", "Agent / revision"].map((v) => (
              <th key={v} scope="col">
                {v}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((g) => (
            <tr key={g.graph_id}>
              <td data-label="WorkGraph">
                <Link to={`workgraphs/${encodeURIComponent(g.graph_id)}`}>{g.name || "Unnamed WorkGraph"}</Link>
                <small className="row-meta mono exact-value">{g.graph_id}</small>
              </td>
              <td data-label="Repository">{g.repository}</td>
              <td data-label="Agent / revision">
                {g.agent_id || "Not recorded"} · r{g.revision}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  ) : (
    <Empty title="No WorkGraphs in this page">
      WorkGraphs remain separate from engineering plans. No relationship is inferred from a shared repository.
    </Empty>
  );
}
export function GraphPortfolio({ graphs, onPage }: { graphs: Graphs; onPage: (v: number) => void }) {
  return (
    <>
      <Head eyebrow="OPERATIONS / DEPENDENCIES" title="WorkGraphs">
        Issue-backed WorkUnits, admitted handoffs and the controller's runnable frontier.
      </Head>
      <Panel title="Operational graph inventory">
        <GraphTable rows={graphs.workgraphs} />
        <Pagination
          label="WorkGraph pagination"
          offset={graphs.offset}
          more={graphs.has_more}
          count={graphs.workgraphs.length}
          onPage={onPage}
        />
      </Panel>
    </>
  );
}
function DependencyMap({
  graph,
  rows,
  selected,
  select,
}: {
  graph: GraphDetail;
  rows: Unit[];
  selected: string;
  select: (v: string) => void;
}) {
  // Depth comes from the full graph's topology (a unit's position in the
  // overall dependency order); row packing is recomputed from just this
  // page's window so off-screen units never leave blank rows in the display.
  const layout = useMemo(
    () => layoutDependencies(graph.units.map((u) => ({ id: u.unit_id, depends_on: u.depends_on }))),
    [graph],
  );
  const depths = [...new Set(rows.map((u) => layout?.get(u.unit_id)?.depth || 0))].sort((a, b) => a - b);
  const heights = new Map<number, number>();
  const points = new Map(
    rows.map((u) => {
      const depth = layout?.get(u.unit_id)?.depth || 0,
        row = heights.get(depth) || 0;
      heights.set(depth, row + 1);
      return [u.unit_id, { x: depths.indexOf(depth) * COLUMN_GAP + MARGIN, y: row * ROW_GAP + MARGIN }];
    }),
  );
  const { markerId, defs } = useDependencyArrow();
  // Surfaced above the graph, not only on the one affected node - see the
  // matching comment in PlanDependencyGraph.
  const malformed = layout
    ? rows.filter((u) => unresolvedDependencies(u.unit_id, u.depends_on, (id) => layout.has(id)).length > 0)
    : [];
  const node = (u: Unit) => (
    <>
      <span className="eyebrow">
        {u.role || "Role not recorded"} · #{u.issue}
      </span>
      <strong>{u.purpose}</strong>
      <span className="small">
        {u.state} {graph.frontier.includes(u.unit_id) ? "· frontier" : ""}
      </span>
      <span className="small muted">After: {u.depends_on.join(", ") || "No dependency"}</span>
    </>
  );
  return (
    <>
      {malformed.length > 0 && (
        <p className="banner red" role="alert">
          {malformed.length} unit{malformed.length > 1 ? "s" : ""} record a dependency not present in this graph
          ({malformed.map((u) => u.unit_id).join(", ")}). This graph's dependency order is not fully recorded as
          valid - inspect the flagged unit{malformed.length > 1 ? "s" : ""} below before treating it as runnable.
        </p>
      )}
      <FilterBar>
        <label htmlFor="workunit-picker">Inspect unit</label>
        <select id="workunit-picker" value={selected} onChange={(e) => select(e.target.value)}>
          {rows.map((u) => (
            <option key={u.unit_id} value={u.unit_id}>
              {u.purpose} · {u.state}
            </option>
          ))}
        </select>
      </FilterBar>
      {layout ? (
        <div className="dependency-map" tabIndex={0} role="region" aria-label="Dependency map; scroll to inspect">
          <svg
            width={Math.max(560, depths.length * COLUMN_GAP)}
            height={Math.max(160, ...[...heights.values()].map((v) => v * ROW_GAP + MARGIN))}
            role="group"
            aria-label="WorkUnit dependency edges"
          >
            <defs>{defs}</defs>
            {rows.flatMap((u) =>
              resolvedDependencies(u.unit_id, u.depends_on, (p) => points.has(p)).map((p) => {
                const from = points.get(p)!,
                  to = points.get(u.unit_id)!;
                return (
                  <path
                    className="dependency-edge"
                    key={`${p}/${u.unit_id}`}
                    d={dependencyEdgePath(from, to)}
                    markerEnd={`url(#${markerId})`}
                  />
                );
              }),
            )}
            {rows.map((u) => {
              const p = points.get(u.unit_id)!;
              // Full-graph scoped, not window-scoped: a dependency outside this
              // page's 50-unit window is expected (see the caption above) and
              // not flagged; only a reference layout itself could not resolve
              // anywhere in the graph is malformed data.
              const unresolved = unresolvedDependencies(u.unit_id, u.depends_on, (id) => layout?.has(id) ?? false);
              return (
                <g
                  key={u.unit_id}
                  className={`dependency-node ${selected === u.unit_id ? "selected" : ""} ${unresolved.length ? "unresolved" : ""}`}
                  transform={`translate(${p.x},${p.y})`}
                  role="button"
                  tabIndex={0}
                  aria-pressed={selected === u.unit_id}
                  aria-label={`${u.purpose} · ${u.state} · after ${u.depends_on.join(", ") || "no dependency"}${unresolved.length ? ` · unresolved dependency reference: ${unresolved.join(", ")}` : ""}`}
                  onClick={() => select(u.unit_id)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      select(u.unit_id);
                    }
                  }}
                >
                  <rect width="220" height="86" rx="4" />
                  <text x="12" y="21" className="dependency-meta">
                    {u.role.slice(0, 25)} · #{u.issue}
                  </text>
                  <text x="12" y="44">
                    {u.purpose.length > 26 ? `${u.purpose.slice(0, 25)}…` : u.purpose}
                  </text>
                  <text x="12" y="67" className="dependency-meta">
                    {unresolved.length
                      ? `Unresolved dependency: ${unresolved.join(", ")}`
                      : `${u.state}${graph.frontier.includes(u.unit_id) ? " · frontier" : ""}`}
                  </text>
                </g>
              );
            })}
          </svg>
        </div>
      ) : (
        <div className="product-body" role="alert">
          Dependency ordering unavailable: a cycle was observed. Inspect each unit's recorded dependencies.
        </div>
      )}
      <div className={`dependency-mobile plan-dependency-grid ${layout ? "" : "dependency-fallback"}`}>
        {rows
          .filter((u) => u.unit_id === selected)
          .map((u) => (
            <button
              key={u.unit_id}
              className={`graph-node ${selected === u.unit_id ? "selected" : ""}`}
              aria-pressed={selected === u.unit_id}
              onClick={() => select(u.unit_id)}
            >
              {node(u)}
            </button>
          ))}
      </div>
    </>
  );
}
function UnitInspector({
  unit: u,
  graph,
  controller,
  stale,
}: {
  unit: Unit;
  graph: GraphDetail;
  controller?: Controller;
  stale: boolean;
}) {
  return (
    <div className="product-body" role="region" aria-label="Selected WorkUnit">
      <h3>{u.purpose}</h3>
      <Facts
        rows={[
          ["Unit identity", u.unit_id],
          ["Role / execution kind", `${u.role || "Unknown"} / ${u.execution_kind || "Not recorded"}`],
          ["Observed state", u.state],
          ["Blocker", u.reason || "None reported"],
          ["Dependencies", u.depends_on.join(", ") || "No dependencies"],
          [
            "Readiness hold",
            u.hold ? `${u.hold.reference || "No reference"} · ${u.hold.detail || "No detail"}` : "None reported",
          ],
          [
            "Independent review",
            u.requires_review
              ? u.review_approved
                ? "Admitted"
                : "Required; not admitted"
              : "Not required by this unit",
          ],
        ]}
      />
      {u.run ? (
        <Link to={`runs/${encodeURIComponent(u.run.run_id)}`}>
          Inspect run · {u.run.phase} · {u.run.disposition}
        </Link>
      ) : (
        <p className="muted">Run not recorded; no worker execution is inferred.</p>
      )}
      {u.output && (
        <Disclosure title="Admitted handoff & producer report">
          <Facts
            rows={[
              ["Handoff", u.output.handoff_id],
              ["Candidate revision", u.output.candidate_revision],
              ["Candidate tree", u.output.candidate_tree],
              ["Reported outcome", u.output.outcome],
            ]}
          />
          <p>Producer-authored report; this prose grants no authority.</p>
          <p>{u.output.summary || "Summary not recorded"}</p>
          <p>Unresolved: {u.output.unresolved.join("; ") || "None reported"}</p>
          <p>Recommended next: {u.output.recommended_next.join("; ") || "None reported"}</p>
        </Disclosure>
      )}
      {u.review && (
        <Disclosure title="Independent review evidence">
          <Facts
            rows={[
              ["Verdict", u.review.verdict],
              ["Exact candidate", u.review.candidate],
              ["Reviewer identity", u.review.reviewer || "Not recorded"],
            ]}
          />
          <ul className="diagnostic-list">
            {u.review.findings.map((f, i) => (
              <li key={i}>
                {f.severity} · {f.signature}
                <p>{f.detail}</p>
              </li>
            ))}
          </ul>
        </Disclosure>
      )}
      {controller && (
        <>
          {(u.hold
            ? [
                {
                  id: u.hold.reference,
                  purpose: "Operator readiness hold",
                  body: u.hold.detail,
                  requested_by: "",
                  requested_at: "",
                  expected_outcome_kind: "allow_deny",
                  permitted_options: [],
                },
                ...u.open_decisions,
              ]
            : u.open_decisions
          )
            .filter((d) => !!d.id)
            .map((d) => (
              <HumanDecision
                key={d.id}
                row={{
                  kind: "decision",
                  id: d.id,
                  graph_id: graph.graph_id,
                  graph_name: graph.name,
                  graph_revision: graph.revision,
                  unit_id: u.unit_id,
                  repository: graph.repository,
                  objective_plan_id: graph.objective_plan_id,
                  objective_plan_revision: graph.objective_plan_revision,
                  objective_plan_digest: graph.objective_plan_digest,
                  unavailable: "",
                  decision: d,
                }}
                controller={controller}
                stale={stale}
              />
            ))}
        </>
      )}
    </div>
  );
}
export function GraphWorkspace({
  graph: g,
  controller,
  stale = false,
  embedded = false,
}: {
  graph: GraphDetail;
  controller?: Controller;
  stale?: boolean;
  embedded?: boolean;
}) {
  const [offset, setOffset] = useState(0),
    [selected, setSelected] = useState(g.units[0]?.unit_id || "");
  const rows = g.units.slice(offset, offset + 50),
    unit = g.units.find((u) => u.unit_id === selected);
  return (
    <>
      {!embedded && (
        <div className="breadcrumb">
          <Link to="workgraphs">WorkGraphs</Link>
          <span>/</span>
          <span>Operational dependencies</span>
        </div>
      )}
      {!embedded && (
        <Head
          eyebrow="WORKGRAPH / OPERATIONAL WORKSPACE"
          title={g.name || "Unnamed WorkGraph"}
          extra={<Badge>Revision {g.revision}</Badge>}
        >
          {g.repository} · authoritative unit states and admitted dependency handoffs.
        </Head>
      )}
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel
            title="WorkUnit dependency graph"
            extra={
              <span className="muted small">
                {g.units.length} units · {g.frontier.length} frontier
              </span>
            }
          >
            <div className="graph-description">
              <p>
                Edges are recorded dependencies; readiness comes from the controller.{" "}
                {g.units.length > 50
                  ? "Only edges within this 50-unit window are drawn; full dependency identities remain in the inspector."
                  : "Select a unit to inspect its exact output and blockers."}
              </p>
            </div>
            {rows.length ? (
              <DependencyMap graph={g} rows={rows} selected={selected} select={setSelected} />
            ) : (
              <Empty title="No WorkUnits recorded">An empty graph has no inferred execution.</Empty>
            )}
            <Pagination
              label="WorkUnit pagination"
              offset={offset}
              more={offset + 50 < g.units.length}
              count={rows.length}
              onPage={(v) => {
                setOffset(v);
                setSelected(g.units[v]?.unit_id || "");
              }}
            />
            {unit && <UnitInspector key={unit.unit_id} unit={unit} graph={g} controller={controller} stale={stale} />}
          </Panel>
        </div>
        <aside className="workspace-aside">
          <Panel title="Runtime counts">
            <Facts rows={Object.entries(g.counts)} />
          </Panel>
          <Panel title="Scope & identity">
            <Facts
              rows={[
                ["Graph", g.graph_id],
                ["Repository", g.repository],
                ["Execution agent", g.agent_id || "Not recorded"],
              ]}
            />
            <div className="product-body">
              {g.product ? (
                <Link to={`products/${encodeURIComponent(g.product.product_id)}`}>Product · {g.product.name}</Link>
              ) : (
                <p>No product association recorded.</p>
              )}
            </div>
          </Panel>
          <Panel title="Runnable frontier">
            <div className="product-body">
              {g.frontier.length ? (
                <ul className="diagnostic-list">
                  {g.frontier.map((id) => (
                    <li className="mono exact-value" key={id}>
                      {id}
                    </li>
                  ))}
                </ul>
              ) : (
                <p>No dependency-ready unit reported.</p>
              )}
              <p className="muted">Frontier membership does not grant execution or acceptance authority.</p>
            </div>
          </Panel>
        </aside>
      </div>
    </>
  );
}
// ProductCreate proposes a new Product's first revision (#476): operator
// names it and its repositories, the controller composes identity and
// adopts it through the same AdoptProductRevision invariant a later revise
// uses, reachable from the dashboard rather than hand-edited configuration.
function ProductCreate({
  controller,
  stale,
  onSettled,
}: {
  controller?: Controller;
  stale: boolean;
  onSettled?: () => void;
}) {
  const [name, setName] = useState(""),
    [repositories, setRepositories] = useState("");
  const [instance, resetInstance] = useReusableGovernedAction();
  const parsedRepositories = repositories
    .split(",")
    .map((r) => r.trim())
    .filter(Boolean);
  const why = stale
    ? "Snapshot stale; refresh first"
    : controller
      ? api.blocker(controller)
      : "Controller not yet observed";
  const reason = why || (!name.trim() ? "Name the product" : !parsedRepositories.length ? "Name at least one repository" : "");
  const prepare = async (_note: string, signal: AbortSignal): Promise<ActionReview> => {
    const live = await freshController(signal, controller?.controller_binding);
    return {
      title: "Create this product?",
      subject: name.trim(),
      consequence:
        "Names which repositories belong together. Grants no execution, acceptance, publication or merge authority.",
      details: [
        ["Name", name.trim()],
        ["Repositories", parsedRepositories.join(", ")],
        ["Serving controller", live.controller_binding],
      ],
      path: "/v1/products",
      action: "product-propose",
      target: {},
      body: { controller_binding: live.controller_binding, name: name.trim(), revision: 1, repositories: parsedRepositories },
    };
  };
  return (
    <Panel title="Create a product">
      <div className="product-body intake-form">
        <TextField label="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <TextField
          label="Repositories"
          help="Comma-separated, e.g. acme/checkout, acme/checkout-worker"
          value={repositories}
          onChange={(e) => setRepositories(e.target.value)}
        />
        <GovernedAction
          key={instance}
          label="Review & create"
          reason={reason}
          prepare={prepare}
          onSettled={() => {
            setName("");
            setRepositories("");
            resetInstance();
            onSettled?.();
          }}
        />
      </div>
    </Panel>
  );
}
export function ProductPortfolio({
  products,
  onPage,
  controller,
  stale = false,
  onSettled,
}: {
  products: Products;
  onPage: (v: number) => void;
  controller?: Controller;
  stale?: boolean;
  onSettled?: () => void;
}) {
  return (
    <>
      <Head eyebrow="OPERATIONS / PRODUCT SCOPE" title="Products">
        Recorded product scope and its associated operational graphs.
      </Head>
      <ProductCreate controller={controller} stale={stale} onSettled={onSettled} />
      <Panel title="Product inventory">
        {products.products.length ? (
          <div className="table-scroll" tabIndex={0} role="region" aria-label="Products">
            <table className="data-table">
              <thead>
                <tr>
                  {["Product", "Repositories", "Revision"].map((v) => (
                    <th key={v} scope="col">
                      {v}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {products.products.map((p) => (
                  <tr key={p.product_id}>
                    <td data-label="Product">
                      <Link to={`products/${encodeURIComponent(p.product_id)}`}>{p.name}</Link>
                      <small className="row-meta mono exact-value">{p.product_id}</small>
                    </td>
                    <td data-label="Repositories">{p.repositories.join(", ") || "None recorded"}</td>
                    <td data-label="Revision">r{p.revision}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <Empty title="No products in this page">Repository names alone do not establish product membership.</Empty>
        )}
        <Pagination
          label="Product pagination"
          offset={products.offset}
          more={products.has_more}
          count={products.products.length}
          onPage={onPage}
        />
      </Panel>
    </>
  );
}
export function ProductWorkspace({ product: p, onPage }: { product: ProductDetail; onPage: (v: number) => void }) {
  return (
    <>
      <div className="breadcrumb">
        <Link to="products">Products</Link>
        <span>/</span>
        <span>Product scope</span>
      </div>
      <Head eyebrow="PRODUCT / SCOPE WORKSPACE" title={p.name} extra={<Badge>Revision {p.revision}</Badge>}>
        {p.repositories.join(", ") || "No repositories recorded"}
      </Head>
      <div className="workspace-grid">
        <div className="workspace-main">
          <Panel title="Associated WorkGraphs">
            <GraphTable rows={p.graphs} />
            <Pagination
              label="Associated WorkGraph pagination"
              offset={p.graphs_offset}
              more={p.graphs_has_more}
              count={p.graphs.length}
              onPage={onPage}
            />
          </Panel>
        </div>
        <aside className="workspace-aside">
          <Panel title="Configuration references">
            {p.configuration ? (
              <>
                <Facts
                  rows={[
                    ["Product identity", p.product_id],
                    ["Configuration revision", p.configuration.revision],
                    ["Policy identity", p.configuration.policy?.id || "Not recorded"],
                    ["Policy revision", p.configuration.policy?.revision ?? "Not recorded"],
                    ["Context policy", p.configuration.context_policy || "Not recorded"],
                  ]}
                />
                <Disclosure title="Recorded settings">
                  <Facts rows={Object.entries(p.configuration.settings)} />
                </Disclosure>
              </>
            ) : (
              <Empty title="Configuration not recorded">
                Product presence does not imply a policy or context selection.
              </Empty>
            )}
          </Panel>
        </aside>
      </div>
    </>
  );
}
