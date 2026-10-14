// The existing governed workspace API owns planning, authority and execution.
import * as api from "./api";
import { decodeDecision, decodeRun, type DecisionRef, type RunDetail } from "./operations-api";
const { object, str, num, bool, arr, timestamp, request } = api;
const strings = (v: unknown) => (v === null || v === undefined ? [] : arr(v).map((x) => str(x)));
const stringMap = (v: unknown) => Object.fromEntries(Object.entries(object(v)).map(([k, v]) => [k, str(v)]));
export type Preview = {
  plan: api.Plan;
  objective: string;
  stage_objectives: Record<string, string>;
  stage_rationales: Record<string, string>;
  stage_blockers: Record<string, string>;
  controller_binding: string;
  assignments_digest: string;
  work_graph_id: string;
  unbound: string[];
};
export type Options = {
  repositories: string[];
  agents: { id: string; kind: string; trust_mode: string; model: string; default: boolean; eligible: boolean }[];
  profiles: string[];
  templates: string[];
  budget: {
    max_child_runs: number;
    max_concurrency: number;
    max_provider_invocations: number;
    max_wall_seconds: number;
    max_cost_micros?: number;
  };
};
export type Authority = {
  id: string;
  digest: string;
  run_id: string;
  repository: string;
  action: { type: string; target: string };
  controller: { id: string; revision: string };
  source: { id: string; revision: string };
  base: { id: string; revision: string };
  contract: { id: string; revision: string };
  decision: { id: string; revision: string };
  candidate: { branch: string; revision: string; tree: string };
  status: string;
  requires: string[];
  missing: string[];
  stale: string[];
  blocking: string[];
  evidence: { id: string; revision: string }[];
  disposition: string;
  observed_at: string;
};
export type HumanRequest = {
  kind: "decision" | "publication";
  id: string;
  graph_id: string;
  graph_name: string;
  graph_revision: number;
  unit_id: string;
  repository: string;
  objective_plan_id: string;
  objective_plan_revision: number;
  objective_plan_digest: string;
  unavailable: string;
  decision?: DecisionRef;
  run?: RunDetail;
};
export type Requests = {
  requests: HumanRequest[];
  offset: number;
  has_more: boolean;
  graphs_offset: number;
  graphs_has_more: boolean;
};
export type Result = { action: string; outcome: string; code: string; state?: unknown };
export type Receipt = { title: string; detail: string; at?: string };
export async function preview(id: string, signal: AbortSignal): Promise<Preview> {
  const p = object((await request(`/v1/plans/${encodeURIComponent(id)}/preview`, signal)).value),
    plan = api.decodePlan(p);
  if (plan.plan_id !== id || plan.revision < 1 || !/^[a-f0-9]{64}$/.test(plan.digest))
    throw new Error("subject_mismatch");
  return {
    plan,
    objective: str(p.objective),
    stage_objectives: stringMap(p.stage_objectives),
    stage_rationales: stringMap(p.stage_rationales),
    stage_blockers: stringMap(p.stage_blockers),
    controller_binding: str(p.controller_binding),
    assignments_digest: str(p.assignments_digest),
    work_graph_id: str(p.work_graph_id, ""),
    unbound: strings(p.unbound),
  };
}
export async function options(signal: AbortSignal): Promise<Options> {
  const o = object((await request("/v1/workspace/options", signal)).value),
    b = object(o.budget);
  return {
    repositories: strings(o.repositories),
    profiles: strings(o.profiles),
    templates: strings(o.templates),
    agents: arr(o.agents).map((v) => {
      const a = object(v);
      return {
        id: str(a.id),
        kind: str(a.kind),
        trust_mode: str(a.trust_mode),
        model: str(a.model, ""),
        default: bool(a.default),
        eligible: bool(a.eligible),
      };
    }),
    budget: {
      max_child_runs: num(b.max_child_runs),
      max_concurrency: num(b.max_concurrency),
      max_provider_invocations: num(b.max_provider_invocations),
      max_wall_seconds: num(b.max_wall_seconds, 0),
      ...(b.max_cost_micros === undefined ? {} : { max_cost_micros: num(b.max_cost_micros) }),
    },
  };
}
export async function authority(id: string, signal: AbortSignal): Promise<Authority | null> {
  const value = (await request(`/v1/runs/${encodeURIComponent(id)}/authority`, signal)).value;
  if (value === null) return null;
  const a = object(value),
    candidate = object(a.candidate),
    action = object(a.action);
  if (str(a.run_id) !== id) throw new Error("subject_mismatch");
  const ref = (value: unknown) => {
    const r = object(value);
    return { id: str(r.id), revision: str(r.revision) };
  };
  return {
    id: str(a.id),
    digest: str(a.digest),
    run_id: id,
    repository: str(a.repository),
    action: { type: str(action.type), target: str(action.target) },
    controller: ref(a.controller),
    source: ref(a.source),
    base: ref(a.base),
    contract: ref(a.contract),
    decision: ref(a.decision),
    candidate: {
      branch: str(candidate.branch, ""),
      revision: str(candidate.revision, ""),
      tree: str(candidate.tree, ""),
    },
    status: str(a.status),
    requires: strings(a.requires),
    missing: strings(a.missing),
    stale: strings(a.stale),
    blocking: strings(a.blocking),
    evidence: a.evidence === undefined ? [] : arr(a.evidence).map(ref),
    disposition: str(a.disposition),
    observed_at: timestamp(a.observed_at),
  };
}
export async function requests(offset: number, graphsOffset: number, signal: AbortSignal): Promise<Requests> {
  if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isSafeInteger(graphsOffset) || graphsOffset < 0)
    throw new Error("invalid_page");
  const r = object(
      (await request(`/v1/workspace/requests?limit=50&offset=${offset}&graphs_offset=${graphsOffset}`, signal)).value,
    ),
    rows = arr(r.requests);
  if (rows.length > 50 || num(r.offset) !== offset || num(r.graphs_offset) !== graphsOffset)
    throw new Error("invalid_response");
  return {
    offset,
    graphs_offset: graphsOffset,
    has_more: bool(r.has_more),
    graphs_has_more: bool(r.graphs_has_more),
    requests: rows.map((value) => {
      const r = object(value),
        kind = str(r.kind);
      if (kind !== "decision" && kind !== "publication") throw new Error("invalid_response");
      const row: HumanRequest = {
        kind,
        id: str(r.id),
        graph_id: str(r.graph_id),
        graph_name: str(r.graph_name),
        graph_revision: num(r.graph_revision),
        unit_id: str(r.unit_id, ""),
        repository: str(r.repository),
        objective_plan_id: str(r.objective_plan_id, ""),
        objective_plan_revision: num(r.objective_plan_revision, 0),
        objective_plan_digest: str(r.objective_plan_digest, ""),
        unavailable: str(r.unavailable, ""),
      };
      if (kind === "decision") {
        row.decision = decodeDecision(r.decision);
        if (row.decision.id !== row.id) throw new Error("subject_mismatch");
      } else {
        row.run = decodeRun(r.run);
        if (row.run.run_id !== row.id) throw new Error("subject_mismatch");
      }
      return row;
    }),
  };
}
export async function send(
  path: string,
  action: string,
  target: Record<string, string | number>,
  body: unknown,
  signal: AbortSignal,
  timeout = 75000,
): Promise<Result> {
  const r = object((await request(path, signal, body, undefined, timeout)).value),
    actual = object(r.target),
    outcome = str(r.outcome);
  if (
    str(r.action) !== action ||
    !["applied", "already_applied", "refused", "unreachable", "unknown"].includes(outcome) ||
    Object.entries(target).some(([k, v]) => actual[k] !== v)
  )
    throw new Error("invalid_response");
  return { action, outcome, code: str(r.code, ""), state: r.state };
}
export async function resolution(
  id: string,
  kind: string,
  value: string,
  signal: AbortSignal,
): Promise<Receipt | null> {
  let r: Record<string, unknown>;
  try {
    r = object((await request(`/v1/decisions/${encodeURIComponent(id)}/resolution`, signal)).value);
  } catch (e) {
    if (e instanceof Error && e.message === "not_found") return null;
    throw e;
  }
  const outcome = object(r.outcome);
  if (str(r.request_id) !== id || str(outcome.kind) !== kind || str(outcome.value) !== value)
    throw new Error("subject_mismatch");
  return { title: "Historical decision receipt", detail: `${id} · ${kind}: ${value}`, at: timestamp(r.resolved_at) };
}
export async function publicationReceipt(a: Authority, decision: string, signal: AbortSignal): Promise<Receipt | null> {
  const r = object(
    (
      await request(
        `/v1/runs/${encodeURIComponent(a.run_id)}/authority-receipts?request_id=${encodeURIComponent(a.id)}&digest=${encodeURIComponent(a.digest)}`,
        signal,
      )
    ).value,
  );
  if (str(r.run_id) !== a.run_id) throw new Error("subject_mismatch");
  const records = arr(r.receipts).map((value) => {
    const r = object(value);
    if (str(r.request_id) !== a.id || str(r.digest) !== a.digest) throw new Error("subject_mismatch");
    return { decision: str(r.decision), sequence: num(r.sequence), at: timestamp(r.occurred_at) };
  });
  const receipt = records.find((r) => r.decision === decision);
  return receipt
    ? {
        title: "Historical publication decision receipt",
        detail: `${a.id} · ${decision} · event ${receipt.sequence}`,
        at: receipt.at,
      }
    : null;
}
