// Existing sanitized /v1/ projections; transport and wire validation have one home in api.ts.
import { arr, bool, num, object, request, str, timestamp } from "./api";

export type Ref = { id: string; revision: string };
export type Fleet = {
  capacity: number;
  executing: number;
  active: number;
  supervisor_running: boolean;
  counts: Record<string, number>;
  classes: Record<string, number>;
};
export type RunDetail = {
  run_id: string;
  repository: string;
  phase: string;
  disposition: string;
  elapsed: number;
  active_elapsed: number;
  external_wait_elapsed: number;
  worker?: { agent: string; provider_kind: string; model: string; trust_mode: string; workspace_bound: boolean };
  source?: { repository: string; issue: number; url: string; intent_changed: boolean };
  controller?: { build_kind: string; build_version: string; changed: boolean };
  candidate?: { branch: string; revision: string; tree: string };
  base?: Ref;
  contract?: Ref;
  operation?: {
    kind: string;
    state: string;
    attempt: number;
    max_attempts: number;
    progress_source: string;
    started_at: string;
    heartbeat_at: string;
    last_progress_at: string;
    silent_for: number;
    inactivity_limit: number;
    inactivity_limit_unknown: boolean;
    inactivity_suspension: string;
    deadline: string;
    deadline_bound: string;
  };
  pull_request?: { number: number; state: string; merged: boolean; stale: boolean };
  assurance?: { passed: boolean; failure_class: string; stale: boolean };
  publication_authority?: { status: string; action_type: string; action_target: string };
  authority_request?: { status: string; requires: string[]; missing: string[]; stale: string[] };
  held_material?: {
    kind: string;
    revision: string;
    blocked_by: string;
    next_step: string;
    successor: string;
    successor_unavailable: string;
  };
  execution_diagnostic?: {
    stage: string;
    failure_class: string;
    bound: string;
    successor: string;
    successor_unavailable: string;
  };
  budgets?: {
    wall_limit: number;
    lifecycle_deadline: number;
    max_execution_attempts: number;
    max_execution_continuations: number;
    max_remediation_attempts: number;
    max_assurance_attempts: number;
  };
  budgets_unverifiable: string[];
  attempts: Record<string, number>;
  candidate_discard_refusals: number;
  candidate_discard_refused: string;
};
export type Event = { sequence: number; type: string; occurred_at: string };
export type Events = { subject_id: string; events: Event[]; next_after: number; has_more: boolean };
export type GraphRow = { graph_id: string; repository: string; agent_id: string; name: string; revision: number };
export type Graphs = { workgraphs: GraphRow[]; offset: number; has_more: boolean };
export type ProductRow = { product_id: string; name: string; revision: number; repositories: string[] };
export type Products = { products: ProductRow[]; offset: number; has_more: boolean };
export type ProductDetail = ProductRow & {
  configuration?: {
    revision: number;
    policy?: { id: string; revision: string };
    context_policy: string;
    settings: Record<string, string>;
  };
  graphs: GraphRow[];
  graphs_offset: number;
  graphs_has_more: boolean;
};
export type Unit = {
  unit_id: string;
  purpose: string;
  role: string;
  execution_kind: string;
  issue: number;
  depends_on: string[];
  state: string;
  reason: string;
  requires_review: boolean;
  review_approved: boolean;
  hold?: { reference: string; detail: string };
  run?: RunDetail;
  output?: {
    handoff_id: string;
    candidate_revision: string;
    candidate_tree: string;
    outcome: string;
    summary: string;
    unresolved: string[];
    recommended_next: string[];
  };
  review?: {
    verdict: string;
    candidate: string;
    reviewer: string;
    findings: { severity: string; signature: string; detail: string }[];
  };
  open_decisions: { id: string; purpose: string; body: string; requested_by: string; requested_at: string }[];
};
export type GraphDetail = GraphRow & {
  frontier: string[];
  counts: Record<string, number>;
  units: Unit[];
  product?: ProductRow;
};

const optionalBool = (v: unknown) => (v === undefined ? false : bool(v));
const strings = (v: unknown) => (v === undefined || v === null ? [] : arr(v).map((x) => str(x)));
// Go durations are nanoseconds. Long observations can exceed JS exact integers;
// they are displayed approximately and never used to enforce a budget or deadline.
function duration(v: unknown): number {
  if (v === undefined) return 0;
  if (typeof v !== "number" || !Number.isFinite(v) || !Number.isInteger(v) || v < 0)
    throw new Error("invalid_response");
  return v;
}
function optionalTime(v: unknown): string {
  return v === undefined || v === null ? "" : timestamp(v);
}
function numbers(v: unknown): Record<string, number> {
  if (v === undefined || v === null) return {};
  return Object.fromEntries(Object.entries(object(v)).map(([k, n]) => [k, num(n)]));
}
export async function fleet(signal: AbortSignal): Promise<Fleet> {
  const r = object((await request("/v1/fleet", signal)).value);
  const counts = object(r.counts),
    classes = object(r.classes);
  return {
    capacity: num(r.capacity),
    executing: num(r.executing),
    active: num(r.active),
    supervisor_running: bool(r.supervisor_running),
    counts: Object.fromEntries(
      ["active", "waiting", "failed", "completed", "cancelled", "held"].map((k) => [k, num(counts[k])]),
    ),
    classes: Object.fromEntries(
      ["working", "observing", "runnable", "waiting", "paused", "unavailable"].map((k) => [k, num(classes[k])]),
    ),
  };
}
function ref(v: unknown): Ref {
  const r = object(v);
  return { id: str(r.id, ""), revision: str(r.revision, "") };
}
function optional<T>(v: unknown, decode: (r: Record<string, unknown>) => T): T | undefined {
  return v === undefined || v === null ? undefined : decode(object(v));
}
export function decodeRun(v: unknown): RunDetail {
  const r = object(v);
  if ([r.elapsed, r.active_elapsed, r.external_wait_elapsed, r.operation].some((v) => v === undefined))
    throw new Error("invalid_response");
  return {
    run_id: str(r.run_id),
    repository: str(r.repository, ""),
    phase: str(r.phase),
    disposition: str(r.disposition),
    elapsed: duration(r.elapsed),
    active_elapsed: duration(r.active_elapsed),
    external_wait_elapsed: duration(r.external_wait_elapsed),
    source: optional(r.source, (s) => ({
      repository: str(s.repository, ""),
      issue: num(s.issue, 0),
      url: str(s.url, ""),
      intent_changed: optionalBool(s.intent_changed),
    })),
    worker: optional(r.worker, (w) => ({
      agent: str(w.agent, ""),
      provider_kind: str(w.provider_kind, ""),
      model: str(w.model, ""),
      trust_mode: str(w.trust_mode, ""),
      workspace_bound: optionalBool(w.workspace_bound),
    })),
    candidate: optional(r.candidate, (c) => ({
      branch: str(c.branch, ""),
      revision: str(c.revision, ""),
      tree: str(c.tree, ""),
    })),
    base: optional(r.base, ref),
    contract: optional(r.contract, ref),
    controller: optional(r.controller, (c) => ({
      build_kind: str(c.build_kind, ""),
      build_version: str(c.build_version, ""),
      changed: optionalBool(c.changed),
    })),
    operation: optional(r.operation, (o) => ({
      kind: str(o.kind, ""),
      state: str(o.state),
      attempt: num(o.attempt),
      max_attempts: num(o.max_attempts, 0),
      progress_source: str(o.progress_source),
      started_at: optionalTime(o.started_at),
      heartbeat_at: optionalTime(o.heartbeat_at),
      last_progress_at: optionalTime(o.last_progress_at),
      silent_for: duration(o.silent_for),
      inactivity_limit: duration(o.inactivity_limit),
      inactivity_limit_unknown: bool(o.inactivity_limit_unknown),
      inactivity_suspension: str(o.inactivity_suspension),
      deadline: optionalTime(o.deadline),
      deadline_bound: str(o.deadline_bound, ""),
    })),
    pull_request: optional(r.pull_request, (p) => ({
      number: num(p.number),
      state: str(p.state, ""),
      merged: optionalBool(p.merged),
      stale: optionalBool(p.stale),
    })),
    assurance: optional(r.assurance, (a) => ({
      passed: bool(a.passed),
      failure_class: str(a.failure_class, ""),
      stale: optionalBool(a.stale),
    })),
    publication_authority: optional(r.publication_authority, (a) => ({
      status: str(a.status, ""),
      action_type: str(a.action_type, ""),
      action_target: str(a.action_target, ""),
    })),
    authority_request: optional(r.authority_request, (a) => ({
      status: str(a.status, ""),
      requires: strings(a.requires),
      missing: strings(a.missing),
      stale: strings(a.stale),
    })),
    held_material: optional(r.held_material, (h) => ({
      kind: str(h.kind),
      revision: str(h.revision, ""),
      blocked_by: str(h.blocked_by, ""),
      next_step: str(h.next_step, ""),
      successor: str(h.successor, ""),
      successor_unavailable: str(h.successor_unavailable, ""),
    })),
    execution_diagnostic: optional(r.execution_diagnostic, (d) => ({
      stage: str(d.stage, ""),
      failure_class: str(d.failure_class, ""),
      bound: str(d.bound, ""),
      successor: str(d.successor, ""),
      successor_unavailable: str(d.successor_unavailable, ""),
    })),
    budgets: optional(r.budgets, (b) => ({
      wall_limit: duration(b.wall_limit),
      lifecycle_deadline: duration(b.lifecycle_deadline),
      max_execution_attempts: num(b.max_execution_attempts, 0),
      max_execution_continuations: num(b.max_execution_continuations, 0),
      max_remediation_attempts: num(b.max_remediation_attempts, 0),
      max_assurance_attempts: num(b.max_assurance_attempts, 0),
    })),
    budgets_unverifiable: strings(r.budgets_unverifiable),
    attempts: numbers(r.attempts),
    candidate_discard_refusals: num(r.candidate_discard_refusals, 0),
    candidate_discard_refused: str(r.candidate_discard_refused, ""),
  };
}
export async function run(id: string, signal: AbortSignal): Promise<RunDetail> {
  const r = decodeRun((await request(`/v1/runs/${encodeURIComponent(id)}`, signal)).value);
  if (r.run_id !== id) throw new Error("subject_mismatch");
  return r;
}
export async function events(
  id: string,
  after: number,
  signal: AbortSignal,
  kind: "run" | "plan" = "run",
): Promise<Events> {
  pageNumber(after);
  const r = object(
    (
      await request(
        `/v1/${kind === "run" ? "runs" : "plans"}/${encodeURIComponent(id)}/events?limit=50&after=${after}`,
        signal,
      )
    ).value,
  );
  const rows = arr(r.events).map((v) => {
    const e = object(v);
    return { sequence: num(e.sequence), type: str(e.type), occurred_at: timestamp(e.occurred_at) };
  });
  let last = after;
  for (const e of rows) {
    if (e.sequence <= last) throw new Error("invalid_response");
    last = e.sequence;
  }
  if (str(r[kind === "run" ? "run_id" : "plan_id"]) !== id) throw new Error("subject_mismatch");
  if (rows.length > 50 || num(r.next_after) !== last || (bool(r.has_more) && !rows.length))
    throw new Error("invalid_response");
  return { subject_id: id, events: rows, next_after: last, has_more: bool(r.has_more) };
}
function graphRow(v: unknown): GraphRow {
  const g = object(v);
  return {
    graph_id: str(g.graph_id),
    name: str(g.name),
    repository: str(g.repository),
    agent_id: str(g.agent_id),
    revision: num(g.revision),
  };
}
function productRow(v: unknown): ProductRow {
  const p = object(v);
  return {
    product_id: str(p.product_id),
    name: str(p.name),
    revision: num(p.revision),
    repositories: arr(p.repositories).map((x) => str(x)),
  };
}
function pageNumber(n: number) {
  if (!Number.isSafeInteger(n) || n < 0) throw new Error("invalid_page");
}
export async function graphs(offset: number, signal: AbortSignal): Promise<Graphs> {
  pageNumber(offset);
  const r = object((await request(`/v1/workgraphs?limit=50&offset=${offset}`, signal)).value),
    rows = arr(r.workgraphs);
  if (rows.length > 50 || num(r.offset) !== offset) throw new Error("invalid_response");
  return { workgraphs: rows.map(graphRow), offset, has_more: bool(r.has_more) };
}
export async function graph(id: string, signal: AbortSignal): Promise<GraphDetail> {
  const g = object((await request(`/v1/workgraphs/${encodeURIComponent(id)}`, signal)).value);
  const summary = graphRow(g);
  if (summary.graph_id !== id) throw new Error("subject_mismatch");
  const units = (g.units === null ? [] : arr(g.units)).map((v) => {
    const u = object(v);
    return {
      unit_id: str(u.unit_id),
      purpose: str(u.purpose),
      role: str(u.role),
      issue: num(u.issue),
      execution_kind: str(u.execution_kind, ""),
      depends_on: strings(u.depends_on),
      state: str(u.state),
      reason: str(u.reason, ""),
      requires_review: optionalBool(u.requires_review),
      review_approved: optionalBool(u.review_approved),
      hold: optional(u.hold, (h) => ({ reference: str(h.reference, ""), detail: str(h.detail, "") })),
      run: optional(u.run, decodeRun),
      output: optional(u.output, (o) => ({
        handoff_id: str(o.handoff_id),
        candidate_revision: str(o.candidate_revision),
        candidate_tree: str(o.candidate_tree),
        outcome: str(o.outcome, ""),
        summary: str(o.summary, ""),
        unresolved: strings(o.unresolved),
        recommended_next: strings(o.recommended_next),
      })),
      review: optional(u.review, (r) => ({
        verdict: str(r.verdict),
        candidate: str(r.candidate),
        reviewer: str(r.reviewer, ""),
        findings:
          r.findings === undefined || r.findings === null
            ? []
            : arr(r.findings).map((v) => {
                const f = object(v);
                return { severity: str(f.severity), signature: str(f.signature), detail: str(f.detail, "") };
              }),
      })),
      open_decisions:
        u.open_decisions === undefined || u.open_decisions === null
          ? []
          : arr(u.open_decisions).map((v) => {
              const d = object(v);
              return {
                id: str(d.id),
                purpose: str(d.purpose, ""),
                body: str(d.body, ""),
                requested_by: str(d.requested_by, ""),
                requested_at: timestamp(d.requested_at),
              };
            }),
    };
  });
  const ids = new Set(units.map((u) => u.unit_id)),
    frontier = strings(g.frontier);
  if (
    ids.size !== units.length ||
    units.some((u) => u.depends_on.some((id) => !ids.has(id))) ||
    frontier.some((id) => !ids.has(id))
  )
    throw new Error("invalid_response");
  return { ...summary, units, frontier, counts: numbers(g.counts), product: optional(g.product, productRow) };
}
export async function products(offset: number, signal: AbortSignal): Promise<Products> {
  pageNumber(offset);
  const r = object((await request(`/v1/products?limit=50&offset=${offset}`, signal)).value),
    rows = arr(r.products);
  if (rows.length > 50 || num(r.offset) !== offset) throw new Error("invalid_response");
  return { products: rows.map(productRow), offset, has_more: bool(r.has_more) };
}
export async function product(id: string, offset: number, signal: AbortSignal): Promise<ProductDetail> {
  pageNumber(offset);
  const p = object(
    (await request(`/v1/products/${encodeURIComponent(id)}?graphs_limit=50&graphs_offset=${offset}`, signal)).value,
  );
  const summary = productRow(p),
    rows = arr(p.graphs);
  if (summary.product_id !== id) throw new Error("subject_mismatch");
  if (num(p.graphs_offset) !== offset || rows.length > 50) throw new Error("invalid_response");
  return {
    ...summary,
    graphs: rows.map(graphRow),
    graphs_offset: offset,
    graphs_has_more: bool(p.graphs_has_more),
    configuration: optional(p.configuration, (c) => ({
      revision: num(c.revision),
      context_policy: str(c.context_policy, ""),
      policy: optional(c.policy, (p) => ({ id: str(p.id), revision: str(p.revision) })),
      settings:
        c.settings === undefined || c.settings === null
          ? {}
          : Object.fromEntries(Object.entries(object(c.settings)).map(([k, v]) => [k, str(v)])),
    })),
  };
}
