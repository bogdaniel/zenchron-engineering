import { useEffect, useState } from "react";
import * as api from "./api";
import * as ops from "./operations-api";
import * as workspace from "./workspace-api";
import { GovernedAction, type ActionReview } from "./governed-action";
import { Disclosure, Empty, Head, Panel, Link } from "./components";
import { CheckboxField, NumberField, SelectField } from "./form";

export async function freshController(signal: AbortSignal, expected?: string) {
  const live = await api.controller(signal),
    why = api.blocker(live);
  if (why || (expected && live.controller_binding !== expected))
    throw new Error(why || "Controller changed; refresh before reviewing");
  return live;
}
function planDetails(p: workspace.Preview): [string, string][] {
  return [
    ["Plan / revision", `${p.plan.plan_id} · r${p.plan.revision}`],
    ["Plan digest", p.plan.digest],
    ["Prospective assignments", p.assignments_digest],
    ["Serving controller", p.controller_binding],
    [
      "Budget envelope",
      `${p.plan.budget.max_child_runs} runs · ${p.plan.budget.max_concurrency} concurrent · ${p.plan.budget.max_provider_invocations} invocations`,
    ],
    ...p.plan.stages.map(
      (s) =>
        [
          s.stage_id,
          `${s.kind} · ${s.bound_profile || "Unbound"} · ${s.agent || "No worker"} · ${p.stage_blockers[s.stage_id] || "No blocker reported"}`,
        ] as [string, string],
    ),
  ];
}
export function PlanControls({
  plan,
  controller,
  stale,
  onSettled,
  compact = false,
  onBlocked,
}: {
  plan: api.Plan;
  controller?: api.Controller;
  stale: boolean;
  onSettled?: () => void;
  compact?: boolean;
  onBlocked?: (blocked: boolean) => void;
}) {
  const [deterministicRevision, setDeterministicRevision] = useState(false);
  const [preview, setPreview] = useState<workspace.Preview | null>(null),
    [error, setError] = useState(""),
    [load, setLoad] = useState(0);
  useEffect(() => {
    const owner = new AbortController();
    setPreview(null);
    setError("");
    workspace
      .preview(plan.plan_id, owner.signal)
      .then((p) => {
        if (p.plan.revision !== plan.revision || p.plan.digest !== plan.digest)
          throw new Error("Exact displayed revision differs from the controller preview");
        setPreview(p);
      })
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Preview unavailable");
      });
    return () => owner.abort();
  }, [plan.plan_id, plan.revision, plan.digest, load]);
  const why = stale
    ? "Snapshot stale; refresh first"
    : controller
      ? api.blocker(controller)
      : "Controller not yet observed";
  const approved = ["governing", "superseded", "rejected"].includes(plan.shown);
  const prepare = async (note: string, signal: AbortSignal, approve: boolean): Promise<ActionReview> => {
    const [p, live] = await Promise.all([
      workspace.preview(plan.plan_id, signal),
      freshController(signal, controller?.controller_binding),
    ]);
    if (
      !preview ||
      p.plan.revision !== plan.revision ||
      p.plan.digest !== plan.digest ||
      p.assignments_digest !== preview.assignments_digest ||
      (p.controller_binding && p.controller_binding !== live.controller_binding) ||
      p.controller_binding !== preview.controller_binding
    )
      throw new Error("Plan, assignment or controller changed; inspect the new preview");
    if (approve && (p.unbound.length || !p.assignments_digest))
      throw new Error("Assignment preview is not bound; approval unavailable");
    const action = approve ? "plan-approve" : "plan-revise";
    return {
      title: approve ? "Approve this exact plan and assignments?" : "Request a new plan revision?",
      subject: p.objective || `${p.plan.repository} · r${p.plan.revision}`,
      consequence: approve
        ? "Approval permits execution within the existing budget and policy ceilings. It grants no acceptance, publication or merge authority."
        : "Generate one new proposal. It remains unapproved; existing governed work keeps its approved revision. No privilege or budget is expanded.",
      details: planDetails({ ...p, controller_binding: live.controller_binding }),
      path: `/v1/plans/${encodeURIComponent(plan.plan_id)}/${approve ? "approve" : "revise"}`,
      action,
      target: { plan_id: plan.plan_id, revision: plan.revision, digest: plan.digest },
      body: {
        controller_binding: live.controller_binding,
        revision: plan.revision,
        digest: plan.digest,
        note,
        ...(approve ? { assignments_digest: p.assignments_digest } : { deterministic: deterministicRevision }),
      },
      timeout: approve ? 75000 : 1270000,
      ...(approve
        ? {
            readReceipt: async (signal: AbortSignal) => {
              const c = await api.context(plan.plan_id, signal, plan.revision);
              if (c.receipt?.status !== "approved") return null;
              return {
                title: "Historical plan approval receipt",
                detail: `${plan.plan_id} · r${plan.revision} · ${plan.digest}`,
                at: c.receipt.occurred_at,
              };
            },
          }
        : {}),
    };
  };
  const approval = (
    <GovernedAction
      label="Review & approve"
      reason={
        why ||
        error ||
        (!preview
          ? "Exact assignment preview required"
          : approved
            ? "This revision is historical/governing"
            : preview.unbound.length
              ? "Some assignments remain unbound"
              : "")
      }
      prepare={(note, signal) => prepare(note, signal, true)}
      onSettled={onSettled}
      onBlocked={onBlocked}
    />
  );
  if (compact) return approval;
  return (
    <>
      <button className="button" onClick={() => setLoad((v) => v + 1)}>
        Refresh controller preview
      </button>
      {preview ? (
        <Disclosure title="Controller-resolved plan & prospective assignments">
          <p>{preview.objective || "Objective prose not recorded"}</p>
          {preview.plan.stages.map((s) => (
            <section className="plan-preview-stage" key={s.stage_id}>
              <h3>{preview.stage_objectives[s.stage_id] || s.stage_id}</h3>
              <p>{preview.stage_rationales[s.stage_id]}</p>
              <p className="muted">
                {s.kind} · {s.bound_profile || "Profile unbound"} · {s.agent || "No worker"}
              </p>
              {preview.stage_blockers[s.stage_id] && <p>{preview.stage_blockers[s.stage_id]}</p>}
            </section>
          ))}
          <p className="mono exact-value">Assignments {preview.assignments_digest || "Unbound"}</p>
        </Disclosure>
      ) : (
        <p role="status">{error ? `Controller preview unavailable · ${error}` : "Reading exact controller preview…"}</p>
      )}
      <CheckboxField
        label="Use deterministic revision"
        checked={deterministicRevision}
        onChange={(e) => setDeterministicRevision(e.target.checked)}
      />
      <div className="plan-control-actions">
        {approval}
        <GovernedAction
          label="Review plan revision"
          recoveryKey={`plan/${plan.plan_id}/revise`}
          reason={why || error || (!preview ? "Exact preview required" : "")}
          prepare={(note, signal) => prepare(note, signal, false)}
          onSettled={onSettled}
        />
      </div>
    </>
  );
}
export function ObjectiveIntake() {
  const [options, setOptions] = useState<workspace.Options | null>(null),
    [controller, setController] = useState<api.Controller | null>(null),
    [products, setProducts] = useState<ops.ProductRow[]>([]),
    [error, setError] = useState("");
  const [product, setProduct] = useState(""),
    [repository, setRepository] = useState(""),
    [issue, setIssue] = useState(""),
    [agent, setAgent] = useState(""),
    [profile, setProfile] = useState(""),
    [template, setTemplate] = useState(""),
    [deterministic, setDeterministic] = useState(false);
  useEffect(() => {
    const owner = new AbortController();
    Promise.all([workspace.options(owner.signal), api.controller(owner.signal), ops.products(0, owner.signal)])
      .then(([o, c, p]) => {
        setOptions(o);
        setController(c);
        setProducts(p.products);
        setRepository(o.repositories[0] || "");
        setAgent(o.agents.find((a) => a.default && a.eligible)?.id || o.agents.find((a) => a.eligible)?.id || "");
      })
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Options unavailable");
      });
    return () => owner.abort();
  }, []);
  // A product narrows which repository this intake offers, and is sent so
  // the compiled WorkGraph can be associated with it; it is not recorded on
  // the plan itself - products associate with WorkGraphs, never with a Plan
  // or Run directly (product/association.go has no AssociatePlan).
  const chosenProduct = products.find((p) => p.product_id === product);
  const availableRepositories =
    chosenProduct && options ? options.repositories.filter((r) => chosenProduct.repositories.includes(r)) : options?.repositories || [];
  const number = Number(issue),
    why =
      error ||
      (!options || !controller
        ? "Reading controller-owned options"
        : api.blocker(controller) ||
          (!Number.isSafeInteger(number) || number < 1
            ? "Enter a positive existing issue number"
            : !repository || !agent
              ? "An eligible repository and worker are required"
              : ""));
  const prepare = async (note: string, signal: AbortSignal): Promise<ActionReview> => {
    const [fresh, live] = await Promise.all([
      workspace.options(signal),
      freshController(signal, controller?.controller_binding),
    ]);
    if (
      !fresh.repositories.includes(repository) ||
      !fresh.agents.some((a) => a.id === agent && a.eligible) ||
      (profile && !fresh.profiles.includes(profile)) ||
      (template && !fresh.templates.includes(template))
    )
      throw new Error("Controller options changed; reload intake");
    return {
      title: "Generate one bounded objective proposal?",
      subject: `${repository} · issue #${number}`,
      consequence:
        "Read the existing issue and generate a proposed plan once. Human plan approval is still required before execution. No publication or merge authority is granted.",
      details: [
        ["Product", chosenProduct?.name || "None selected"],
        ["Repository / issue", `${repository}#${number}`],
        ["Planning agent", agent],
        ["Profile", profile || "Controller resolution"],
        ["Template", template || "Controller default"],
        ["Planning mode", deterministic ? "Deterministic" : "Reasoned proposal"],
        ["Serving controller", live.controller_binding],
      ],
      path: "/v1/objectives",
      action: "plan-revise",
      target: {},
      body: {
        controller_binding: live.controller_binding,
        repository,
        issue: number,
        agent,
        profile,
        template,
        deterministic,
        note,
        product_id: product,
      },
      timeout: 1270000,
    };
  };
  return (
    <>
      <Head eyebrow="OPERATIONS / OBJECTIVE INTAKE" title="Create objective">
        Existing issue → controller proposal → explicit plan approval.
      </Head>
      <Panel title="Engineering intent">
        <div className="product-body intake-form">
          <SelectField
            label="Product"
            value={product}
            onChange={(e) => {
              const id = e.target.value;
              setProduct(id);
              const scoped = products.find((p) => p.product_id === id);
              const scopedRepositories =
                scoped && options ? options.repositories.filter((r) => scoped.repositories.includes(r)) : options?.repositories || [];
              if (!scopedRepositories.includes(repository)) setRepository(scopedRepositories[0] || "");
            }}
          >
            <option value="">Any governed repository</option>
            {products.map((p) => (
              <option key={p.product_id} value={p.product_id}>
                {p.name}
              </option>
            ))}
          </SelectField>
          <SelectField label="Repository" value={repository} onChange={(e) => setRepository(e.target.value)}>
            {availableRepositories.map((r) => (
              <option key={r}>{r}</option>
            ))}
          </SelectField>
          <NumberField label="Existing issue" min="1" value={issue} onChange={(e) => setIssue(e.target.value)} />
          <SelectField label="Planning agent" value={agent} onChange={(e) => setAgent(e.target.value)}>
            {options?.agents
              .filter((a) => a.eligible)
              .map((a) => (
                <option key={a.id} value={a.id}>
                  {a.id} · {a.kind}
                </option>
              ))}
          </SelectField>
          <p>
            The planning agent drafts the proposal and does not write code. Implementer and reviewer workers are assigned in the
            plan, and you approve those exact assignments before anything runs.
          </p>
          <SelectField label="Agent profile" value={profile} onChange={(e) => setProfile(e.target.value)}>
            <option value="">Resolve from policy and plan</option>
            {options?.profiles.map((p) => (
              <option key={p}>{p}</option>
            ))}
          </SelectField>
          <SelectField label="Plan template" value={template} onChange={(e) => setTemplate(e.target.value)}>
            <option value="">Controller default</option>
            {options?.templates.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </SelectField>
          <CheckboxField
            label="Use deterministic planning"
            checked={deterministic}
            onChange={(e) => setDeterministic(e.target.checked)}
          />
          {options && (
            <p>
              Envelope: {options.budget.max_child_runs} worker runs · {options.budget.max_concurrency} concurrent ·{" "}
              {options.budget.max_provider_invocations} invocations. Cost ceiling{" "}
              {options.budget.max_cost_micros === undefined ? "unknown" : options.budget.max_cost_micros + " micros"}.
            </p>
          )}
          <GovernedAction
            label="Review objective proposal"
            recoveryKey="objective-intake"
            reason={why}
            prepare={prepare}
          />
          <Link to="objectives">Back to objectives</Link>
        </div>
      </Panel>
    </>
  );
}
