import { PlanControls } from "./planning";
import { HumanRequests } from "./human-requests";
import { useEffect, useRef, useState } from "react";
import * as api from "./api";
import { Badge, Binding, Dialog, Disclosure, Empty, Head, Link, Pagination, Panel } from "./components";
import { TextareaField } from "./form";

type Confirmation = { plan: api.Plan; binding: string };
export function Decisions({
  index,
  context,
  controller,
  stale,
  refresh,
  onPage,
}: {
  index: api.PlanIndex;
  context?: api.Context;
  controller: api.Controller;
  stale: boolean;
  refresh: () => void;
  onPage: (offset: number) => void;
}) {
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [busy, setBusy] = useState(false);
  const [approvalBlocked, setApprovalBlocked] = useState(false);
  const [message, setMessage] = useState("");
  const [note, setNote] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const [resolved, setResolved] = useState<api.Context | null>(null);
  const lock = useRef(false);
  const receiptFocus = useRef<HTMLHeadingElement>(null);
  const rejectFocus = useRef<HTMLButtonElement>(null);
  const focusReceipt = useRef(false);
  const current = resolved || context;
  const p = current?.plan;
  const pageSuffix = index.offset ? `?offset=${index.offset}` : "";
  const pending = index.plans.filter((row) => row.pending);
  const previouslyPending = useRef(current?.pending === true);
  const reason = stale ? "Snapshot stale · refresh before review" : api.blocker(controller);
  const actionable = reason
    ? 0
    : pending.filter(
        (row) =>
          !((uncertain || approvalBlocked) && row.plan_id === p?.plan_id && row.current_revision === p?.revision),
      ).length;
  useEffect(() => {
    if ((focusReceipt.current || previouslyPending.current) && current?.receipt) {
      receiptFocus.current?.focus({ preventScroll: true });
      focusReceipt.current = false;
    }
    previouslyPending.current = current?.pending === true;
  }, [current?.receipt, confirmation]);
  const review = async () => {
    if (!p || lock.current || reason || !current?.pending || uncertain) return;
    lock.current = true;
    setBusy(true);
    setMessage("");
    try {
      const signal = new AbortController().signal;
      const [fresh, live] = await Promise.all([api.context(p.plan_id, signal, p.revision), api.controller(signal)]);
      if (!fresh.pending || fresh.plan.digest !== p.digest || live.controller_binding !== controller.controller_binding)
        throw new Error("Subject or controller changed · refresh this request");
      if (api.blocker(live)) throw new Error(api.blocker(live));
      setConfirmation({ plan: fresh.plan, binding: live.controller_binding });
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "Review unavailable");
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  const submit = async () => {
    if (!confirmation || lock.current) return;
    lock.current = true;
    setBusy(true);
    setMessage("");
    const bound = confirmation;
    let observedResult: api.ActionResult | undefined;
    try {
      // Exactly one POST; never re-target, retry or interpret a lost reply as
      // failure. The server revalidates the binding and pending revision.
      const result = await api.reject(bound.plan, bound.binding, note, new AbortController().signal);
      observedResult = result;
      if (result.outcome === "unknown") setUncertain(true);
      setMessage(`${result.outcome}${result.code ? " · " + result.code : ""}`);
      const fresh = await api.context(bound.plan.plan_id, new AbortController().signal, bound.plan.revision);
      if (fresh.receipt) {
        focusReceipt.current = true;
        setResolved(fresh);
        setUncertain(false);
        setConfirmation(null);
        setMessage("Durable historical decision receipt re-read");
        refresh();
      } else if (["applied", "already_applied"].includes(result.outcome)) {
        setUncertain(true);
        setMessage("Applied response · durable receipt unavailable; reconcile before another action");
        setConfirmation(null);
      } else {
        setConfirmation(null);
        refresh();
      }
    } catch {
      // A transport or validation failure after submission cannot establish
      // whether the controller applied it. Refresh reconciles; never resend.
      setUncertain(true);
      setMessage(
        observedResult
          ? `${observedResult.outcome}${observedResult.code ? " · " + observedResult.code : ""} response · durable context unavailable; reconcile before another action`
          : "Outcome unknown · re-read the exact revision before any further action",
      );
      setConfirmation(null);
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  const reconcile = async () => {
    if (!p || lock.current) return;
    lock.current = true;
    setBusy(true);
    try {
      const fresh = await api.context(p.plan_id, new AbortController().signal, p.revision);
      setResolved(fresh);
      setUncertain(!fresh.receipt);
      focusReceipt.current = !!fresh.receipt;
      setMessage(
        fresh.receipt
          ? "Historical receipt recovered"
          : "No durable receipt yet · previous outcome unresolved; recheck without resubmitting",
      );
      refresh();
    } catch {
      setMessage("Reconciliation unavailable · no retry was submitted");
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  return (
    <div className="actions-page">
      <Head
        eyebrow="OPERATIONS / HUMAN AUTHORITY"
        title="Action required"
        extra={
          <Badge tone="orange">
            {pending.length} pending · {actionable} actionable
          </Badge>
        }
      >
        Human requests only. Recovery stays with the runtime.
      </Head>
      <div className="request-summary small muted">
        {pending.length} pending · {actionable} actionable · plan decisions
      </div>
      <div className="inbox-toolbar">
        <span className="muted small">
          Loaded plan page{index.has_more ? " · more available" : ""} · plan decisions.
        </span>
        <button
          className="recovery-shortcut"
          onClick={() => {
            const section = document.getElementById("runtime-recovery");
            section?.scrollIntoView({ block: "start" });
            section?.focus({ preventScroll: true });
          }}
        >
          Runtime recovery ↘
        </button>
      </div>
      <label className="mobile-decision-picker">
        Choose a decision
        <select
          aria-label="Choose a decision"
          value={p ? `${encodeURIComponent(p.plan_id)}/${p.revision}` : ""}
          onChange={(e) => {
            location.hash = "#/actions/" + e.target.value + pageSuffix;
          }}
        >
          <option value="" disabled>
            Select request
          </option>
          {p && !pending.some((row) => row.plan_id === p.plan_id && row.current_revision === p.revision) && (
            <option value={`${encodeURIComponent(p.plan_id)}/${p.revision}`}>
              {p.plan_id} · r{p.revision} · historical context
            </option>
          )}
          {pending.map((row) => (
            <option key={row.plan_id} value={`${encodeURIComponent(row.plan_id)}/${row.current_revision}`}>
              {row.plan_id} · r{row.current_revision}
            </option>
          ))}
        </select>
      </label>
      <div className="inbox-grid">
        <section className="decision-list" aria-label="Pending human requests">
          {pending.map((row) => (
            <a
              key={row.plan_id}
              className={`decision-item ${row.plan_id === p?.plan_id && row.current_revision === p?.revision ? "selected" : ""}`}
              href={`#/actions/${encodeURIComponent(row.plan_id)}/${row.current_revision}${pageSuffix}`}
            >
              <div className="decision-item-meta">
                <span className="eyebrow">Plan decision</span>
                <span className="small muted">r{row.current_revision}</span>
              </div>
              <h2>
                {row.source_issue ? `Issue #${row.source_issue}` : "Plan revision"} · {row.repository}
              </h2>
              <div className="decision-item-foot">
                <span className="mono small muted exact-value">{row.plan_id}</span>
                <span>
                  {reason || (uncertain && row.plan_id === p?.plan_id && row.current_revision === p?.revision)
                    ? "Pending · unavailable"
                    : "Reject available"}
                </span>
              </div>
            </a>
          ))}

          {!pending.length && (
            <Empty title="No pending requests in this page">
              Historical receipts remain accessible by exact revision.
            </Empty>
          )}
        </section>
        {p ? (
          <article className="decision-detail">
            <div className="decision-detail-head">
              <span className="eyebrow">Plan revision decision</span>
              <Badge tone={current?.receipt ? "neutral" : "orange"}>
                {current?.receipt
                  ? "Historical receipt"
                  : uncertain
                    ? "Outcome unresolved"
                    : current?.pending
                      ? reason
                        ? "Pending · unavailable"
                        : "Action needed"
                      : "No pending request"}
              </Badge>
            </div>
            <h2 className="decision-title">Review plan revision {p.revision}</h2>
            <p className="decision-description secondary">
              Approve bounded execution or reject this proposal while retaining the governing revision. No publication,
              merge or privilege authority is granted.
            </p>
            <div className="decision-subject">
              <span>{p.repository}</span>
              <span className="mono" title={p.plan_id}>
                Plan {p.plan_id.length > 18 ? p.plan_id.slice(0, 18) + "…" : p.plan_id}
              </span>
              <span>Digest {p.digest.slice(0, 12)}</span>
              <Link to={`objectives/${encodeURIComponent(p.plan_id)}`}>Workspace</Link>
            </div>
            {current?.receipt ? (
              <div className="banner neutral">
                <h3 id="decision-receipt" ref={receiptFocus} tabIndex={-1}>
                  Historical decision receipt
                </h3>
                <p>
                  Plan {current.receipt.status} · r{current.receipt.revision}
                </p>
                <p className="small muted">
                  {current.receipt.occurred_at} · journal sequence {current.receipt.sequence}. Past decision; no current
                  eligibility implied.
                </p>
              </div>
            ) : (
              <div className="decision-checks">
                <div>
                  <span>Immutable revision / digest</span>
                  <strong>Observed</strong>
                </div>
                <div>
                  <span>Live endpoint / identity</span>
                  <strong className={reason ? "orange" : "secondary"}>
                    {reason ? "Unavailable" : "Observed at read time"}
                  </strong>
                </div>
                <div>
                  <span>Existing policy and budget ceilings</span>
                  <strong>Restricted</strong>
                </div>
              </div>
            )}
            {message && (
              <p className="orange" role="status">
                {message}
              </p>
            )}
            {uncertain && (
              <button className="button" disabled={busy} onClick={reconcile}>
                Reconcile exact revision
              </button>
            )}
            <div className="decision-footer">
              {current?.receipt ? (
                <Link to="actions" className="button">
                  Other human requests
                </Link>
              ) : (
                <>
                  <p className="small muted">{reason || "Controller rechecks exact assignments."}</p>
                  <div className="decision-buttons">
                    <button
                      ref={rejectFocus}
                      className="button"
                      onClick={review}
                      disabled={busy || approvalBlocked || !!reason || !current?.pending || uncertain}
                    >
                      {busy ? "Checking…" : "Reject"}
                    </button>
                    <PlanControls
                      key={`${p.plan_id}/${p.revision}/${p.shown}`}
                      plan={p}
                      controller={controller}
                      stale={stale || busy || uncertain}
                      onSettled={refresh}
                      compact
                      onBlocked={setApprovalBlocked}
                    />
                  </div>
                </>
              )}
            </div>
            <Disclosure title="Technical binding and evidence">
              <Binding
                plan={p}
                binding={controller.controller_binding}
                action={current?.receipt?.status === "approved" ? "plan-approve (historical)" : "plan-reject"}
              />
              <p>Plan decision does not assert candidate assurance. Rejection requires no approval assignment set.</p>
            </Disclosure>
          </article>
        ) : (
          <Panel title="Decision context">
            <Empty title="Select an exact request">
              Inspect its subject and consequence before deciding. No request is selected automatically after
              resolution.
            </Empty>
          </Panel>
        )}
      </div>
      <Pagination
        label="Plan request pagination"
        offset={index.offset}
        count={index.plans.length}
        more={index.has_more}
        onPage={onPage}
      />
      <HumanRequests controller={controller} stale={stale} />
      <section id="runtime-recovery" className="recovery-section" tabIndex={-1}>
        <Panel title="Runtime-owned recovery">
          <Empty title="Recovery aggregate unavailable">
            No automatic runtime failure is promoted into a human authority request. Inspect individual runs for failure
            classes, held material and successors; no aggregate count is inferred.
          </Empty>
          <div className="panel-footer">
            <Link to="runs">Inspect run recovery</Link>
            <Link to="workgraphs">Inspect WorkUnit human requests</Link>
          </div>
        </Panel>
      </section>
      {confirmation && (
        <Dialog busy={busy} onClose={() => setConfirmation(null)} returnFocus={rejectFocus.current}>
          <div className="confirmation-body">
            <p className="eyebrow">GOVERNED PLAN REJECTION</p>
            <h2 id="confirmation-title">Reject plan r{confirmation.plan.revision}?</h2>
            <p className="dialog-consequence">
              Withhold approval for this exact proposal. Keep any previous governing revision. No execution, publication
              or merge is authorized.
            </p>
            <div className="confirmation-action">
              <strong>Protected action: plan-reject</strong>
              <span className="mono exact-value">
                {confirmation.plan.plan_id} · r{confirmation.plan.revision}
              </span>
              <span className="mono exact-value">Digest {confirmation.plan.digest}</span>
            </div>
            <Disclosure title="Exact technical binding">
              <Binding plan={confirmation.plan} binding={confirmation.binding} action="plan-reject" />
            </Disclosure>
            <TextareaField
              label="Operator note (optional, 500 characters)"
              value={note}
              maxLength={500}
              rows={2}
              onChange={(e) => setNote(e.target.value)}
              help="The controller independently checks the subject and its authority before writing a durable decision."
            />
          </div>
          <div className="dialog-buttons">
            <button className="button" disabled={busy} onClick={() => setConfirmation(null)}>
              Cancel
            </button>
            <button className="button primary" disabled={busy} onClick={submit}>
              {busy ? "Submitting once…" : "Confirm rejection"}
            </button>
          </div>
        </Dialog>
      )}
    </div>
  );
}
