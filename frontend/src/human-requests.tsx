import { useEffect, useState } from "react";
import * as api from "./api";
import * as ops from "./operations-api";
import * as workspace from "./workspace-api";
import { GovernedAction, type ActionReview } from "./governed-action";
import { freshController } from "./planning";
import { Disclosure, Empty, Link, Pagination, Panel } from "./components";

async function verifyContext(row: workspace.HumanRequest, signal: AbortSignal) {
  const graph = await ops.graph(row.graph_id, signal);
  if (
    graph.revision !== row.graph_revision ||
    graph.objective_plan_id !== row.objective_plan_id ||
    graph.objective_plan_revision !== row.objective_plan_revision ||
    graph.objective_plan_digest !== row.objective_plan_digest
  )
    throw new Error("Owning graph or exact plan context changed; refresh");
  if (row.objective_plan_id) {
    const c = await api.context(row.objective_plan_id, signal, row.objective_plan_revision);
    if (c.graph_binding_mismatch || c.plan.digest !== row.objective_plan_digest)
      throw new Error("Exact objective/graph binding is unverified");
  }
  return graph;
}
export function HumanDecision({
  row,
  controller,
  stale,
  onSettled,
}: {
  row: workspace.HumanRequest;
  controller: api.Controller;
  stale: boolean;
  onSettled?: () => void;
}) {
  const request = row.decision!,
    [value, setValue] = useState("");
  const options =
    request.expected_outcome_kind === "allow_deny"
      ? ["allow", "deny"]
      : request.expected_outcome_kind === "selected_option"
        ? request.permitted_options
        : [];
  const supported = options.length > 0 || request.expected_outcome_kind === "text";
  const why = stale
    ? "Snapshot stale; refresh first"
    : row.unavailable ||
      api.blocker(controller) ||
      (new TextEncoder().encode(value).length > (request.expected_outcome_kind === "selected_option" ? 64 : 2000)
        ? "Outcome exceeds the request text bound"
        : !supported
          ? "Request outcome grammar is unavailable"
          : !value.trim()
            ? "Choose the exact requested outcome"
            : "");
  const prepare = async (note: string, signal: AbortSignal): Promise<ActionReview> => {
    const [graph, live] = await Promise.all([
      verifyContext(row, signal),
      freshController(signal, controller.controller_binding),
    ]);
    const unit = graph.units.find((u) => u.unit_id === row.unit_id),
      current = unit?.open_decisions.find((d) => d.id === request.id);
    if (current) {
      if (JSON.stringify(current) !== JSON.stringify(request))
        throw new Error("Decision subject or permitted outcomes changed");
    } else if (!(unit?.hold?.reference === request.id && request.expected_outcome_kind === "allow_deny"))
      throw new Error("This exact request is no longer pending");
    return {
      title: "Resolve this exact human request?",
      subject: request.purpose || request.id,
      consequence: `Record ${request.expected_outcome_kind}: ${value}. The controller decides whether this satisfies the request; it grants no publication or merge authority.`,
      details: [
        ["Request", request.id],
        ["Graph / revision", `${row.graph_id} · r${row.graph_revision}`],
        ["WorkUnit", row.unit_id],
        ["Objective / revision", `${row.objective_plan_id || "Standalone graph"} · r${row.objective_plan_revision}`],
        ["Plan digest", row.objective_plan_digest || "No plan binding"],
        ["Outcome", `${request.expected_outcome_kind}: ${value}`],
        ["Serving controller", live.controller_binding],
        ...Object.entries(request.subject || {}),
      ],
      path: `/v1/decisions/${encodeURIComponent(request.id)}/resolve`,
      action: "decision-resolve",
      target: { decision_id: request.id },
      body: {
        controller_binding: live.controller_binding,
        outcome_kind: request.expected_outcome_kind,
        outcome_value: value,
        note,
      },
      readReceipt: (signal) => workspace.resolution(request.id, request.expected_outcome_kind, value, signal),
    };
  };
  return (
    <div className="product-body">
      <h3>{request.purpose || "Human decision"}</h3>
      <p>{request.body || "This request is owned by the selected WorkUnit."}</p>
      <p className="muted">
        {row.repository} · {row.unit_id} · {request.requested_at}
      </p>
      <Disclosure title="Exact request & subject">
        <p className="mono exact-value">{request.id}</p>
        {Object.entries(request.subject || {}).map(([k, v]) => (
          <p className="mono exact-value" key={k}>
            {k}: {v}
          </p>
        ))}
      </Disclosure>
      <label className="field-label">
        Requested outcome
        {options.length ? (
          <select aria-label="Requested outcome" value={value} onChange={(e) => setValue(e.target.value)}>
            <option value="">Choose outcome</option>
            {options.map((v) => (
              <option key={v}>{v}</option>
            ))}
          </select>
        ) : (
          <input
            aria-label="Requested outcome"
            maxLength={2000}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={!supported}
          />
        )}
      </label>
      <GovernedAction label="Review decision" reason={why} prepare={prepare} onSettled={onSettled} />
      <Link to={`workgraphs/${encodeURIComponent(row.graph_id)}`}>Inspect owning WorkGraph</Link>
    </div>
  );
}
export function PublicationControls({
  runId,
  controller,
  stale,
  row,
  onSettled,
}: {
  runId: string;
  controller: api.Controller;
  stale: boolean;
  row?: workspace.HumanRequest;
  onSettled?: () => void;
}) {
  const [request, setRequest] = useState<workspace.Authority | null>(null),
    [error, setError] = useState(""),
    [loading, setLoading] = useState(true),
    [load, setLoad] = useState(0);
  useEffect(() => {
    const owner = new AbortController();
    setLoading(true);
    setError("");
    setRequest(null);
    workspace
      .authority(runId, owner.signal)
      .then(setRequest)
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Authority unavailable");
      })
      .finally(() => {
        if (!owner.signal.aborted) setLoading(false);
      });
    return () => owner.abort();
  }, [runId, load]);
  const why = stale
    ? "Snapshot stale; refresh first"
    : row?.unavailable ||
      api.blocker(controller) ||
      error ||
      (loading
        ? "Reading publication request"
        : !request
          ? "No outstanding human publication request"
          : !request.requires.length
            ? "No human requirement is outstanding"
            : "");
  const prepare = async (note: string, signal: AbortSignal, decision: string): Promise<ActionReview> => {
    if (row) await verifyContext(row, signal);
    const [fresh, live] = await Promise.all([
      workspace.authority(runId, signal),
      freshController(signal, controller.controller_binding),
    ]);
    if (!request || !fresh || fresh.id !== request.id || fresh.digest !== request.digest || !fresh.requires.length)
      throw new Error("Publication subject or evidence context changed; read the new request");
    return {
      title: decision === "approve" ? "Authorize this exact publication request?" : "Reject this publication request?",
      subject: `${fresh.repository} · ${fresh.action.type} · ${fresh.action.target}`,
      consequence: `Record human ${decision} evidence for this exact candidate and contract. The kernel still checks independent evidence, policy and eligibility before publication. This action does not publish or merge.`,
      details: [
        ["Request / digest", `${fresh.id} · ${fresh.digest}`],
        ["Run", fresh.run_id],
        ["Protected action", `${fresh.action.type} · ${fresh.action.target}`],
        ["Candidate", fresh.candidate.revision],
        ["Candidate tree", fresh.candidate.tree],
        ["Contract", `${fresh.contract.id} · ${fresh.contract.revision}`],
        ["Pinned source", `${fresh.source.id} · ${fresh.source.revision}`],
        ["Base", fresh.base.revision],
        ["Required human claims", fresh.requires.join(", ")],
        [
          "Independent/evidence references",
          fresh.evidence.map((e) => `${e.id}@${e.revision}`).join(", ") || "None reported",
        ],
        ["Serving controller", live.controller_binding],
      ],
      path: `/v1/runs/${encodeURIComponent(runId)}/authorize`,
      action: "authorize",
      target: { run_id: runId, request_id: fresh.id, digest: fresh.digest },
      body: { controller_binding: live.controller_binding, request_id: fresh.id, digest: fresh.digest, decision, note },
      readReceipt: (signal) => workspace.publicationReceipt(fresh, decision, signal),
    };
  };
  return (
    <div className="product-body">
      <h3>Publication authorization</h3>
      <p>
        {loading
          ? "Reading exact publication request…"
          : error
            ? `Publication unavailable · ${error}`
            : request
              ? `${request.repository} · ${request.action.type} · ${request.requires.join(", ")}`
              : "No outstanding human publication request"}
      </p>
      {request && (
        <>
          <p>Candidate identity is bound to this request. Independent evidence remains a separate requirement.</p>
          <Disclosure title="Technical binding & evidence">
            <p className="mono exact-value">
              {request.id} · {request.digest}
            </p>
            <p className="mono exact-value">
              Candidate {request.candidate.revision} · tree {request.candidate.tree}
            </p>
            <p>Evidence references: {request.evidence.map((e) => e.id).join(", ") || "None reported"}</p>
            <p>Other missing requirements: {request.missing.join(", ") || "None reported"}</p>
          </Disclosure>
        </>
      )}
      <button className="button" onClick={() => setLoad((v) => v + 1)} disabled={loading}>
        Refresh publication request
      </button>
      <div className="publication-buttons">
        <GovernedAction
          label="Review & authorize publication"
          reason={why}
          prepare={(note, signal) => prepare(note, signal, "approve")}
          alternatives={[{ label: "Reject publication", prepare: (note, signal) => prepare(note, signal, "reject") }]}
          onSettled={onSettled}
        />
      </div>
    </div>
  );
}
export function HumanRequests({ controller, stale }: { controller: api.Controller; stale: boolean }) {
  const [page, setPage] = useState<workspace.Requests | null>(null),
    [offset, setOffset] = useState(0),
    [graphsOffset, setGraphsOffset] = useState(0),
    [reload, setReload] = useState(0),
    [error, setError] = useState(""),
    [selected, setSelected] = useState<workspace.HumanRequest | null>(null),
    [loading, setLoading] = useState(true);
  useEffect(() => {
    const owner = new AbortController();
    setLoading(true);
    setError("");
    workspace
      .requests(offset, graphsOffset, owner.signal)
      .then(setPage)
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Requests unavailable");
      })
      .finally(() => {
        if (!owner.signal.aborted) setLoading(false);
      });
    return () => owner.abort();
  }, [offset, graphsOffset, reload, controller]);
  const refresh = () => setReload((v) => v + 1);
  return (
    <Panel
      title="WorkGraph decisions & publication requests"
      extra={
        <button className="button" disabled={loading} onClick={refresh}>
          Refresh graph requests
        </button>
      }
    >
      <div className="product-body">
        <p>
          Graph-owned human requests, separately paged from plan approvals. Runtime recovery never enters this list.
        </p>
        {error && <p role="alert">Request read unavailable · {error}. Retained requests are historical.</p>}
        {page?.requests.map((r) => (
          <button
            className={`decision-item request-picker ${selected?.id === r.id ? "selected" : ""}`}
            key={`${r.kind}/${r.id}`}
            data-request-id={r.id}
            onClick={() => setSelected(r)}
          >
            <span className="eyebrow">{r.kind}</span>
            <strong>{r.decision?.purpose || `${r.repository} · publication`}</strong>
            <span className="muted small">
              {r.graph_name} · {r.unavailable || "Fresh review required"}
            </span>
          </button>
        ))}
        {page && !page.requests.length && (
          <Empty title="No human graph requests in this page">Other graph pages remain independently navigable.</Empty>
        )}
        {page && (
          <>
            <Pagination
              label="Human request pagination"
              offset={page.offset}
              count={page.requests.length}
              more={page.has_more}
              onPage={setOffset}
            />
            <div className="page-controls">
              <span>
                Graphs {page.graphs_offset}–{page.graphs_offset + 50};{" "}
                {page.graphs_has_more ? "more graph pages available" : "end of graph index"}
              </span>
              <button
                className="button"
                disabled={!graphsOffset}
                onClick={() => {
                  setGraphsOffset((v) => Math.max(0, v - 50));
                  setOffset(0);
                }}
              >
                Previous graph page
              </button>
              <button
                className="button"
                disabled={!page.graphs_has_more}
                onClick={() => {
                  setGraphsOffset((v) => v + 50);
                  setOffset(0);
                }}
              >
                Next graph page
              </button>
            </div>
          </>
        )}
      </div>
      {selected &&
        (selected.kind === "decision" ? (
          <HumanDecision
            key={selected.id}
            row={selected}
            controller={controller}
            stale={stale || loading || !!error}
            onSettled={refresh}
          />
        ) : (
          <PublicationControls
            key={selected.id}
            runId={selected.id}
            controller={controller}
            stale={stale || loading || !!error}
            row={selected}
            onSettled={refresh}
          />
        ))}
    </Panel>
  );
}
