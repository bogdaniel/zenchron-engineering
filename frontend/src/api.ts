// Sanitized wire projections only. Domain policy and authority stay in Go.
export type Controller = {
  live_reachable: boolean;
  serving: string;
  controller_binding: string;
  generation_match: string;
  role: string;
  work_admission: string;
  durable_consistency: string;
  projection: string;
  findings: string[];
  durable_generation?: { kind: string; source_revision: string; version: string };
  live_build?: { kind: string; source_revision: string; version: string };
};
export type PlanRow = {
  plan_id: string;
  repository: string;
  current_revision: number;
  source_issue: number;
  pending: boolean;
  shown: string;
  lifecycle?: { state: string; approved_revision: number; unavailable: boolean };
};
export type PlanIndex = { plans: PlanRow[]; offset: number; has_more: boolean };
export type Stage = {
  stage_id: string;
  kind: string;
  depends_on: string[];
  state: string;
  role: string;
  profile: string;
  context_policy: string;
  assignment_id: string;
  agent: string;
  bound_profile: string;
  provider_kind: string;
  trust_mode: string;
  run_id: string;
  review?: { verdict: string; candidate: string; tree: string };
  gate?: {
    claims: string[];
    evidence?: { id: string; revision: string };
    decision?: { id: string; revision: string };
    human_evidence_id: string;
    proven_heads: string[];
  };
};
export type Plan = {
  plan_id: string;
  revision: number;
  digest: string;
  repository: string;
  base_revision: string;
  governing_revision: number;
  shown: string;
  stages: Stage[];
  template: string;
  invalidated: string[];
  base_change?: { from_revision: number; from: string; to: string };
  budget: {
    child_runs: number;
    max_child_runs: number;
    max_concurrency: number;
    wall_seconds: number;
    max_wall_seconds: number;
    provider_invocations: number;
    max_provider_invocations: number;
    cost_known: boolean;
    cost_micros?: number;
  };
};
export type Receipt = { status: string; revision: number; digest: string; sequence: number; occurred_at: string };
export type Context = {
  plan: Plan;
  pending: boolean;
  receipt?: Receipt;
  graph?: unknown;
  graph_binding_mismatch: boolean;
  lifecycle?: { state: string; approved_revision: number; unavailable: boolean };
};
export type Run = {
  run_id: string;
  repository: string;
  issue: number;
  agent: string;
  phase: string;
  disposition: string;
  executing: boolean;
  unavailable: boolean;
  provider_kind: string;
  trust_mode: string;
  model: string;
};
export type Runs = { runs: Run[]; has_more: boolean; offset: number };
export type ActionResult = {
  action: string;
  target: { plan_id: string; revision: number; digest: string };
  outcome: string;
  code: string;
};

export function object(v: unknown): Record<string, unknown> {
  if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("invalid_response");
  return v as Record<string, unknown>;
}
export function str(v: unknown, absent?: string): string {
  if (v === undefined && absent !== undefined) return absent;
  if (typeof v !== "string") throw new Error("invalid_response");
  return v;
}
export function num(v: unknown, absent?: number): number {
  if (v === undefined && absent !== undefined) return absent;
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < 0) throw new Error("invalid_response");
  return v;
}
export function bool(v: unknown): boolean {
  if (typeof v !== "boolean") throw new Error("invalid_response");
  return v;
}
export function arr(v: unknown): unknown[] {
  if (!Array.isArray(v)) throw new Error("invalid_response");
  return v;
}
export function timestamp(v: unknown): string {
  const value = str(v);
  if (!Number.isFinite(Date.parse(value))) throw new Error("invalid_response");
  return value;
}
export function credential(): string {
  const cookie = document.cookie
    .split(";")
    .map((v) => v.trim())
    .find((v) => v.startsWith("zenchron_control_plane_token="));
  let token = "";
  try {
    token = cookie ? decodeURIComponent(cookie.slice(cookie.indexOf("=") + 1)) : "";
  } catch {
    throw new Error("unauthorized");
  }
  if (!/^[a-f0-9]{64}$/.test(token)) throw new Error("unauthorized");
  return token;
}
export async function request(
  path: string,
  signal: AbortSignal,
  body?: unknown,
  token = credential(),
  timeoutMs = body === undefined ? 5000 : 75000,
  operationID?: string,
): Promise<{ value: unknown; ok: boolean }> {
  const response = await fetch(path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      ...(operationID ? { "X-Zenchron-Operation-ID": operationID } : {}),
    },
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    signal: AbortSignal.any([signal, AbortSignal.timeout(timeoutMs)]),
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  if (response.status === 401 || response.status === 403)
    throw new Error(response.status === 401 ? "unauthorized" : "permission_refused");
  if (!response.headers.get("Content-Type")?.includes("application/json")) throw new Error("invalid_response");
  const value: unknown = await response.json();
  if (!response.ok && body === undefined) {
    const code = str(object(value).error);
    if (
      ![
        "not_found",
        "invalid_page",
        "read_failed",
        "snapshot_changed",
        "subject_mismatch",
        "controller_unreachable",
        "runtime_refused",
        "invalid_controller_response",
      ].includes(code)
    )
      throw new Error("invalid_response", { cause: { status: response.status } });
    throw new Error(code, { cause: { status: response.status } });
  }
  return { value, ok: response.ok };
}
export async function controller(signal: AbortSignal): Promise<Controller> {
  return decodeController((await request("/v1/controller", signal)).value);
}
function decodeController(value: unknown): Controller {
  const r = object(value);
  const durable = r.durable_generation === undefined ? undefined : object(r.durable_generation);
  const live = r.live_build === undefined ? undefined : object(r.live_build);
  return {
    live_reachable: bool(r.live_reachable),
    serving: str(r.serving),
    controller_binding: str(r.controller_binding, ""),
    generation_match: str(r.generation_match),
    role: str(r.role, ""),
    work_admission: str(r.work_admission, ""),
    durable_consistency: str(r.durable_consistency),
    projection: str(r.projection, "Unknown"),
    findings: r.findings === undefined ? [] : arr(r.findings).map((v) => str(v)),
    ...(durable
      ? {
          durable_generation: {
            kind: str(durable.kind, ""),
            source_revision: str(durable.source_revision, ""),
            version: str(durable.version, ""),
          },
        }
      : {}),
    ...(live
      ? {
          live_build: {
            kind: str(live.kind, ""),
            source_revision: str(live.source_revision, ""),
            version: str(live.version, ""),
          },
        }
      : {}),
  };
}
export function hasCredential(): boolean {
  try {
    credential();
    return true;
  } catch {
    return false;
  }
}
export function disconnect() {
  document.cookie = "zenchron_control_plane_token=; path=/; SameSite=Strict; Max-Age=0";
}
export async function connect(value: string, signal: AbortSignal): Promise<void> {
  const token = value.trim();
  if (!/^[a-f0-9]{64}$/.test(token)) throw new Error("invalid_token");
  decodeController((await request("/v1/controller", signal, undefined, token)).value);
  // This is the existing credential, set only after the bearer API accepts it.
  document.cookie = `zenchron_control_plane_token=${token}; path=/; SameSite=Strict`;
  if (!hasCredential() || credential() !== token) throw new Error("session_unavailable");
}
export async function plans(signal: AbortSignal, offset = 0, query = ""): Promise<PlanIndex> {
  if (!Number.isSafeInteger(offset) || offset < 0 || query.length > 200) throw new Error("invalid_page");
  const r = object((await request(`/v1/plans?limit=50&offset=${offset}&q=${encodeURIComponent(query)}`, signal)).value);
  const rows = arr(r.plans);
  if (rows.length > 50 || num(r.offset) !== offset) throw new Error("invalid_response");
  return {
    offset,
    has_more: bool(r.has_more),
    plans: rows.map((v) => {
      const p = object(v);
      return {
        plan_id: str(p.plan_id),
        repository: str(p.repository),
        current_revision: num(p.current_revision),
        source_issue: num(p.source_issue, 0),
        pending: bool(p.pending),
        shown: str(p.shown),
        ...(p.lifecycle === undefined ? {} : { lifecycle: decodeLifecycle(p.lifecycle) }),
      };
    }),
  };
}
export function decodePlan(value: unknown): Plan {
  const p = object(value),
    b = object(p.budget);
  const changedBase = p.base_change === undefined ? undefined : object(p.base_change);
  const plan: Plan = {
    plan_id: str(p.plan_id),
    revision: num(p.revision),
    digest: str(p.digest),
    repository: str(p.repository),
    base_revision: str(p.base_revision),
    governing_revision: num(p.governing_revision),
    shown: str(p.shown),
    template: str(p.template, ""),
    invalidated: p.invalidated === undefined ? [] : arr(p.invalidated).map((v) => str(v)),
    ...(changedBase
      ? {
          base_change: {
            from_revision: num(changedBase.from_revision),
            from: str(changedBase.from),
            to: str(changedBase.to),
          },
        }
      : {}),
    stages: arr(p.stages).map((v) => {
      const s = object(v),
        review = s.review === undefined ? undefined : object(s.review),
        gate = s.gate === undefined ? undefined : object(s.gate);
      const gateRef = (v: unknown) => {
        const r = object(v);
        return { id: str(r.id), revision: str(r.revision) };
      };
      const kind = str(s.kind),
        runID = str(s.run_id, "");
      if (!["agent", "assurance_gate", "human_decision_gate"].includes(kind) || (kind !== "agent" && runID))
        throw new Error("invalid_response");
      return {
        stage_id: str(s.stage_id),
        kind,
        depends_on: arr(s.depends_on).map((v) => str(v)),
        state: str(s.state, ""),
        role: str(s.role, ""),
        profile: str(s.profile, ""),
        context_policy: str(s.context_policy, ""),
        assignment_id: str(s.assignment_id, ""),
        agent: str(s.agent, ""),
        run_id: runID,
        bound_profile: str(s.bound_profile, ""),
        provider_kind: str(s.provider_kind, ""),
        trust_mode: str(s.trust_mode, ""),
        ...(review
          ? { review: { verdict: str(review.verdict), candidate: str(review.candidate), tree: str(review.tree) } }
          : {}),
        ...(gate
          ? {
              gate: {
                claims: gate.claims === undefined ? [] : arr(gate.claims).map((v) => str(v)),
                ...(gate.evidence === undefined ? {} : { evidence: gateRef(gate.evidence) }),
                ...(gate.decision === undefined ? {} : { decision: gateRef(gate.decision) }),
                human_evidence_id: str(gate.human_evidence_id, ""),
                proven_heads: gate.proven_heads === undefined ? [] : arr(gate.proven_heads).map((v) => str(v)),
              },
            }
          : {}),
      };
    }),
    budget: {
      child_runs: num(b.child_runs),
      max_child_runs: num(b.max_child_runs),
      max_concurrency: num(b.max_concurrency),
      wall_seconds: num(b.wall_seconds),
      max_wall_seconds: num(b.max_wall_seconds, 0),
      provider_invocations: num(b.provider_invocations),
      max_provider_invocations: num(b.max_provider_invocations),
      cost_known: bool(b.cost_known),
      ...(b.cost_micros === undefined ? {} : { cost_micros: num(b.cost_micros) }),
    },
  };
  return plan;
}
function decodeLifecycle(value: unknown) {
  const l = object(value);
  return {
    state: str(l.state),
    approved_revision: num(l.approved_revision),
    unavailable: l.unavailable === undefined ? false : bool(l.unavailable),
  };
}
export async function context(id: string, signal: AbortSignal, revision = 0): Promise<Context> {
  if (!id || !Number.isSafeInteger(revision) || revision < 0) throw new Error("invalid_subject");
  const r = object((await request(`/v1/plans/${encodeURIComponent(id)}/context?revision=${revision}`, signal)).value);
  const plan = decodePlan(r.plan);
  if (plan.plan_id !== id || !plan.digest || (revision > 0 && plan.revision !== revision))
    throw new Error("subject_mismatch");
  let receipt: Receipt | undefined;
  if (r.receipt !== undefined) {
    const s = object(r.receipt);
    receipt = {
      status: str(s.status),
      revision: num(s.revision),
      digest: str(s.digest),
      sequence: num(s.sequence),
      occurred_at: timestamp(s.occurred_at),
    };
    if (
      receipt.revision !== plan.revision ||
      receipt.digest !== plan.digest ||
      !["approved", "rejected"].includes(receipt.status)
    )
      throw new Error("subject_mismatch");
  }
  const pending = bool(r.pending);
  if (plan.revision < 1 || !/^[a-f0-9]{64}$/.test(plan.digest)) throw new Error("invalid_response");
  if (pending && receipt) throw new Error("invalid_response");
  return {
    plan,
    pending,
    receipt,
    graph: r.graph,
    graph_binding_mismatch: r.graph_binding_mismatch === undefined ? false : bool(r.graph_binding_mismatch),
    ...(r.lifecycle === undefined
      ? {}
      : {
          lifecycle: decodeLifecycle(r.lifecycle),
        }),
  };
}
export async function runs(signal: AbortSignal, offset = 0, status = "", source = ""): Promise<Runs> {
  if (!Number.isSafeInteger(offset) || offset < 0 || source.length > 200) throw new Error("invalid_page");
  const query = new URLSearchParams({
    limit: "50",
    offset: String(offset),
    ...(status ? { status } : {}),
    ...(source ? { source } : {}),
  });
  const r = object((await request(`/v1/runs?${query}`, signal)).value),
    rows = arr(r.runs);
  if (rows.length > 50 || num(r.offset) !== offset) throw new Error("invalid_response");
  return {
    offset,
    has_more: bool(r.has_more),
    runs: rows.map((v) => {
      const x = object(v);
      return {
        run_id: str(x.run_id),
        repository: str(x.repository, ""),
        issue: num(x.issue, 0),
        agent: str(x.agent, ""),
        phase: str(x.phase),
        disposition: str(x.disposition),
        executing: bool(x.executing),
        unavailable: x.unavailable === undefined ? false : bool(x.unavailable),
        provider_kind: str(x.provider_kind, ""),
        trust_mode: str(x.trust_mode, ""),
        model: str(x.model, ""),
      };
    }),
  };
}
// This guard narrows display only. The POST independently re-observes the
// controller; serve proves the binding and role on the same request.
export function blocker(c: Controller): string {
  if (!c.live_reachable || c.serving !== "serving") return "Controller endpoint unreachable or not serving";
  if (!c.controller_binding) return "Serving controller identity unattested";
  if (c.role !== "held") return "Controller role not held";
  if (c.generation_match !== "match") return "Controller differs from the durable active generation";
  return "";
}
export async function reject(p: Plan, binding: string, note: string, signal: AbortSignal): Promise<ActionResult> {
  const r = object(
    (
      await request(`/v1/plans/${encodeURIComponent(p.plan_id)}/reject`, signal, {
        revision: p.revision,
        digest: p.digest,
        controller_binding: binding,
        note,
      })
    ).value,
  );
  const t = object(r.target);
  const target = { plan_id: str(t.plan_id), revision: num(t.revision), digest: str(t.digest) };
  const result = { action: str(r.action), target, outcome: str(r.outcome), code: str(r.code, "") };
  if (
    result.action !== "plan-reject" ||
    target.plan_id !== p.plan_id ||
    target.revision !== p.revision ||
    target.digest !== p.digest ||
    !["applied", "already_applied", "refused", "unreachable", "unknown"].includes(result.outcome)
  )
    throw new Error("invalid_response");
  return result;
}
