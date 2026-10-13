// Sanitized wire projections only. Domain policy and authority stay in Go.
export type Controller = {
  live_reachable: boolean;
  serving: string;
  controller_binding: string;
  generation_match: string;
  role: string;
  work_admission: string;
  durable_consistency: string;
  durable_generation?: { kind: string; source_revision: string };
};
export type PlanRow = {
  plan_id: string;
  repository: string;
  current_revision: number;
  source_issue: number;
  pending: boolean;
  shown: string;
};
export type PlanIndex = { plans: PlanRow[]; offset: number; has_more: boolean };
export type Stage = {
  stage_id: string;
  kind: string;
  depends_on: string[];
  state: string;
  role: string;
  agent: string;
  bound_profile: string;
  provider_kind: string;
  trust_mode: string;
  run_id: string;
  review?: { verdict: string; candidate: string; tree: string };
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
  budget: {
    child_runs: number;
    max_child_runs: number;
    provider_invocations: number;
    max_provider_invocations: number;
    cost_known: boolean;
    cost_micros?: number;
  };
};
export type Receipt = { status: string; revision: number; digest: string; sequence: number; occurred_at: string };
export type Context = { plan: Plan; pending: boolean; receipt?: Receipt };
export type Run = {
  run_id: string;
  repository: string;
  agent: string;
  phase: string;
  disposition: string;
  executing: boolean;
};
export type Runs = { runs: Run[]; has_more: boolean; offset: number };
export type ActionResult = {
  action: string;
  target: { plan_id: string; revision: number; digest: string };
  outcome: string;
  code: string;
};

function object(v: unknown): Record<string, unknown> {
  if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("invalid_response");
  return v as Record<string, unknown>;
}
function str(v: unknown, absent?: string): string {
  if (v === undefined && absent !== undefined) return absent;
  if (typeof v !== "string") throw new Error("invalid_response");
  return v;
}
function num(v: unknown, absent?: number): number {
  if (v === undefined && absent !== undefined) return absent;
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < 0) throw new Error("invalid_response");
  return v;
}
function bool(v: unknown): boolean {
  if (typeof v !== "boolean") throw new Error("invalid_response");
  return v;
}
function arr(v: unknown): unknown[] {
  if (!Array.isArray(v)) throw new Error("invalid_response");
  return v;
}
function timestamp(v: unknown): string {
  const value = str(v);
  if (!Number.isFinite(Date.parse(value))) throw new Error("invalid_response");
  return value;
}
function credential(): string {
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
async function request(path: string, signal: AbortSignal, body?: unknown): Promise<{ value: unknown; ok: boolean }> {
  const response = await fetch(path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      Authorization: `Bearer ${credential()}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    signal: AbortSignal.any([signal, AbortSignal.timeout(body === undefined ? 5000 : 75000)]),
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  if (response.status === 401 || response.status === 403)
    throw new Error(response.status === 401 ? "unauthorized" : "permission_refused");
  if (!response.headers.get("Content-Type")?.includes("application/json")) throw new Error("invalid_response");
  const value: unknown = await response.json();
  if (!response.ok && body === undefined) {
    const code = str(object(value).error);
    if (!["not_found", "invalid_page", "read_failed", "snapshot_changed"].includes(code))
      throw new Error("invalid_response", { cause: { status: response.status } });
    throw new Error(code, { cause: { status: response.status } });
  }
  return { value, ok: response.ok };
}
export async function controller(signal: AbortSignal): Promise<Controller> {
  const r = object((await request("/v1/controller", signal)).value);
  const durable = r.durable_generation === undefined ? undefined : object(r.durable_generation);
  return {
    live_reachable: bool(r.live_reachable),
    serving: str(r.serving),
    controller_binding: str(r.controller_binding, ""),
    generation_match: str(r.generation_match),
    role: str(r.role, ""),
    work_admission: str(r.work_admission, ""),
    durable_consistency: str(r.durable_consistency),
    ...(durable
      ? { durable_generation: { kind: str(durable.kind, ""), source_revision: str(durable.source_revision, "") } }
      : {}),
  };
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
      };
    }),
  };
}
export async function context(id: string, signal: AbortSignal, revision = 0): Promise<Context> {
  if (!id || !Number.isSafeInteger(revision) || revision < 0) throw new Error("invalid_subject");
  const r = object((await request(`/v1/plans/${encodeURIComponent(id)}/context?revision=${revision}`, signal)).value);
  const p = object(r.plan),
    b = object(p.budget);
  const plan: Plan = {
    plan_id: str(p.plan_id),
    revision: num(p.revision),
    digest: str(p.digest),
    repository: str(p.repository),
    base_revision: str(p.base_revision),
    governing_revision: num(p.governing_revision),
    shown: str(p.shown),
    stages: arr(p.stages).map((v) => {
      const s = object(v),
        review = s.review === undefined ? undefined : object(s.review);
      return {
        stage_id: str(s.stage_id),
        kind: str(s.kind),
        depends_on: arr(s.depends_on).map((v) => str(v)),
        state: str(s.state, ""),
        role: str(s.role, ""),
        agent: str(s.agent, ""),
        run_id: str(s.run_id, ""),
        bound_profile: str(s.bound_profile, ""),
        provider_kind: str(s.provider_kind, ""),
        trust_mode: str(s.trust_mode, ""),
        ...(review
          ? { review: { verdict: str(review.verdict), candidate: str(review.candidate), tree: str(review.tree) } }
          : {}),
      };
    }),
    budget: {
      child_runs: num(b.child_runs),
      max_child_runs: num(b.max_child_runs),
      provider_invocations: num(b.provider_invocations),
      max_provider_invocations: num(b.max_provider_invocations),
      cost_known: bool(b.cost_known),
      ...(b.cost_micros === undefined ? {} : { cost_micros: num(b.cost_micros) }),
    },
  };
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
  return { plan, pending, receipt };
}
export async function runs(signal: AbortSignal): Promise<Runs> {
  const r = object((await request("/v1/runs?limit=50&offset=0", signal)).value),
    rows = arr(r.runs);
  if (rows.length > 50 || num(r.offset) !== 0) throw new Error("invalid_response");
  return {
    offset: 0,
    has_more: bool(r.has_more),
    runs: rows.map((v) => {
      const x = object(v);
      return {
        run_id: str(x.run_id),
        repository: str(x.repository, ""),
        agent: str(x.agent, ""),
        phase: str(x.phase),
        disposition: str(x.disposition),
        executing: bool(x.executing),
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
