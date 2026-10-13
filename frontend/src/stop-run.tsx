import { useRef, useState } from "react";
import * as api from "./api";
import * as ops from "./operations-api";
import { Dialog, Disclosure } from "./components";
import type { RunDetail } from "./operations-api";

// UI narrowing only. The controller owns stop eligibility and durable cancellation.
function stopBlocker(r: RunDetail): string {
  if (["completed", "failed", "cancelled"].includes(r.disposition))
    return `Run is ${r.disposition}; no new stop offered`;
  if (!["active", "waiting"].includes(r.disposition)) return "Run disposition is not recognized as stoppable";
  return "";
}
export function StopRun({ run, controller, stale }: { run: RunDetail; controller: api.Controller; stale: boolean }) {
  const [review, setReview] = useState<{ run: RunDetail; binding: string } | null>(null);
  const [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [uncertain, setUncertain] = useState(false);
  const [resolved, setResolved] = useState(false);
  const submitting = useRef(false),
    opener = useRef<HTMLElement | null>(null),
    resultFocus = useRef<HTMLParagraphElement>(null);
  const why = stale ? "Snapshot stale; refresh before reviewing stop" : api.blocker(controller) || stopBlocker(run);
  const result = (text: string) => {
    setMessage(text);
    requestAnimationFrame(() => resultFocus.current?.focus());
  };
  const inspect = async (button: HTMLButtonElement) => {
    opener.current = button;
    setBusy(true);
    setMessage("");
    const owner = new AbortController();
    try {
      const [current, live] = await Promise.all([ops.run(run.run_id, owner.signal), api.controller(owner.signal)]);
      if (stopBlocker(current) || api.blocker(live) || live.controller_binding !== controller.controller_binding)
        throw new Error("Subject or controller changed; refresh before reviewing stop");
      setReview({ run: current, binding: live.controller_binding });
    } catch (e) {
      result(e instanceof Error ? e.message : "read_failed");
    } finally {
      setBusy(false);
    }
  };
  const reconcile = async () => {
    setBusy(true);
    try {
      const current = await ops.run(run.run_id, new AbortController().signal);
      if (current.disposition === "cancelled") {
        setResolved(true);
        setUncertain(false);
        result("Historical cancellation observed in durable run state. No request was resent.");
      } else
        result(
          `No durable cancellation observed · ${current.disposition}. Outcome remains unknown; stop stays disabled.`,
        );
    } catch {
      result("Durable run state unavailable. Outcome remains unknown; stop stays disabled.");
    } finally {
      setBusy(false);
    }
  };
  const confirm = async () => {
    if (!review || submitting.current) return;
    submitting.current = true;
    setBusy(true);
    let known = "";
    try {
      const response = api.object(
        (
          await api.request(`/v1/runs/${encodeURIComponent(review.run.run_id)}/stop`, new AbortController().signal, {
            controller_binding: review.binding,
          })
        ).value,
      );
      const target = api.object(response.target),
        outcome = api.str(response.outcome);
      if (
        api.str(response.action) !== "stop" ||
        api.str(target.run_id) !== review.run.run_id ||
        !["applied", "already_applied", "refused", "unreachable", "unknown"].includes(outcome)
      )
        throw new Error("invalid_response");
      if (outcome === "unknown") {
        setUncertain(true);
        known = "Stop outcome unknown; re-read durable state. Do not resend.";
      } else if (["applied", "already_applied"].includes(outcome)) {
        setResolved(true);
        known = `Stop ${outcome.replaceAll("_", " ")}; cancellation acknowledged. Refresh run state to inspect it.`;
      } else known = `Stop ${outcome} · ${api.str(response.code, "no code reported")}. Refresh before a new review.`;
    } catch {
      setUncertain(true);
      known = "Stop outcome unknown; response unavailable or invalid. Re-read durable state without resending.";
    } finally {
      setReview(null);
      setBusy(false);
      submitting.current = false;
      result(known);
    }
  };
  return (
    <div className="product-body">
      <p>
        Stop this run through the serving controller. It records cancellation; it grants no publication or merge
        authority.
      </p>
      <p className="muted">
        {resolved
          ? "Cancellation recorded; this control is historical."
          : uncertain
            ? "Unknown outcome; action disabled until cancellation is observed."
            : why || "Fresh review required before sending one stop request."}
      </p>
      <button
        className="button danger"
        disabled={busy || resolved || uncertain || !!why}
        onClick={(e) => inspect(e.currentTarget)}
      >
        Review stop
      </button>
      {message && (
        <p ref={resultFocus} tabIndex={-1} role="status">
          {message}
        </p>
      )}
      {uncertain && (
        <button className="button" disabled={busy} onClick={reconcile}>
          Re-read cancellation
        </button>
      )}
      {review && (
        <Dialog busy={busy} onClose={() => setReview(null)} returnFocus={opener.current}>
          <div className="confirmation-body">
            <div className="eyebrow">GOVERNED OPERATOR ACTION</div>
            <h2 id="confirmation-title">Stop this exact run?</h2>
            <p>
              {review.run.repository || "Repository not recorded"} · {review.run.phase} · {review.run.disposition}
            </p>
            <p className="mono exact-value">{review.run.run_id}</p>
            <p>
              Protected action: stop. The controller will record cancellation for this run. Existing evidence remains
              historical; nothing is published or merged.
            </p>
            <Disclosure title="Exact run & controller binding">
              <p>
                Run <span className="mono exact-value">{review.run.run_id}</span>
              </p>
              <p>
                Candidate <span className="mono exact-value">{review.run.candidate?.revision || "Not recorded"}</span>
              </p>
              <p>
                Serving controller <span className="mono exact-value">{review.binding}</span>
              </p>
            </Disclosure>
          </div>
          <div className="dialog-buttons">
            <button className="button" disabled={busy} onClick={() => setReview(null)}>
              Cancel
            </button>
            <button className="button danger" disabled={busy} onClick={confirm}>
              {busy ? "Stopping…" : "Confirm stop"}
            </button>
          </div>
        </Dialog>
      )}
    </div>
  );
}
