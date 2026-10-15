import { useEffect, useRef, useState, type ReactNode } from "react";
import { Dialog, Disclosure, Link } from "./components";
import { TextareaField } from "./form";
import { useActionRecovery, readActionReceipt } from "./action-recovery";
import * as workspace from "./workspace-api";
export type ActionReview = {
  title: string;
  subject: string;
  consequence: string;
  details: [string, string][];
  path: string;
  action: string;
  target: Record<string, string | number>;
  body: Record<string, unknown>;
  timeout?: number;
  readReceipt?: (signal: AbortSignal) => Promise<workspace.Receipt | null>;
};
// useReusableGovernedAction gives a repeatable CREATE action (a new product,
// profile or template - never "approve this exact revision") back its usable
// state after a success, by remounting GovernedAction under a fresh key.
// GovernedAction marks itself permanently acknowledged after one success:
// correct for an exact subject an operator never re-approves, wrong for an
// action with no subject at all. The remount is delayed so the success
// message a caller's onSettled leaves showing stays visible first, rather
// than vanishing the instant the remount replaces it.
export function useReusableGovernedAction(delayMs = 4000) {
  const [instance, setInstance] = useState(0);
  const pending = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(pending.current), []);
  const reset = () => {
    pending.current = setTimeout(() => setInstance((v) => v + 1), delayMs);
  };
  return [instance, reset] as const;
}
// One local confirmation/submission owner. Server projections and controller
// actions remain authoritative; an unknown write is never retried.
export function GovernedAction({
  label,
  reason,
  prepare,
  onSettled,
  noteAllowed = true,
  children,
  alternatives = [],
  onBlocked,
  recoveryKey,
}: {
  label: string;
  reason: string;
  prepare: (note: string, signal: AbortSignal) => Promise<ActionReview>;
  onSettled?: () => void;
  noteAllowed?: boolean;
  children?: ReactNode;
  alternatives?: { label: string; prepare: (note: string, signal: AbortSignal) => Promise<ActionReview> }[];
  onBlocked?: (blocked: boolean) => void;
  recoveryKey?: string;
}) {
  const recovery = useActionRecovery(recoveryKey);
  const [review, setReview] = useState<ActionReview | null>(null),
    [busy, setBusy] = useState(false),
    [note, setNote] = useState(""),
    [message, setMessage] = useState(""),
    [unknown, setUnknown] = useState<ActionReview | null>(null),
    [receipt, setReceipt] = useState<workspace.Receipt | null>(null),
    [acknowledged, setAcknowledged] = useState(false);
  const lock = useRef(false),
    owner = useRef<AbortController | null>(null),
    opener = useRef<HTMLElement | null>(null),
    result = useRef<HTMLElement | null>(null);
  useEffect(() => () => owner.current?.abort(), []);
  const announce = (text: string) => {
    setMessage(text);
    requestAnimationFrame(() => result.current?.focus());
  };
  useEffect(() => {
    onBlocked?.(busy || !!review || !!unknown || recovery.pending.length > 0 || !!recovery.error);
  }, [busy, review, unknown, recovery.pending.length, recovery.error, onBlocked]);
  useEffect(() => () => onBlocked?.(false), [onBlocked]);
  const begin = async (button: HTMLButtonElement, choose = prepare) => {
    if (
      lock.current ||
      reason ||
      unknown ||
      acknowledged ||
      recovery.loading ||
      recovery.error ||
      recovery.pending.length
    )
      return;
    lock.current = true;
    setBusy(true);
    setMessage("");
    opener.current = button;
    owner.current = new AbortController();
    try {
      setReview(await choose(note, owner.current.signal));
    } catch (e) {
      if (!owner.current.signal.aborted) announce(e instanceof Error ? e.message : "Review unavailable");
    } finally {
      lock.current = false;
      if (!owner.current.signal.aborted) setBusy(false);
    }
  };
  const read = async (bound: ActionReview, known: boolean) => {
    if (!bound.readReceipt) {
      if (known) setAcknowledged(true);
      else setUnknown(bound);
      return;
    }
    const value = await bound.readReceipt(owner.current!.signal);
    if (value) {
      setReceipt(value);
      setUnknown(null);
      setAcknowledged(true);
      announce("Exact-subject receipt or state observed. No request was resent.");
      onSettled?.();
    } else {
      setUnknown(bound);
      announce("No durable receipt observed for this exact subject. Re-read without resending.");
    }
  };
  const submit = async () => {
    if (!review || lock.current) return;
    lock.current = true;
    setBusy(true);
    owner.current = new AbortController();
    const bound = { ...review, body: { ...review.body, ...(noteAllowed && "note" in review.body ? { note } : {}) } };
    let outcome: workspace.Result | undefined;
    let saved: Awaited<ReturnType<typeof recovery.remember>> | undefined;
    try {
      if (recoveryKey) saved = await recovery.remember(bound, owner.current.signal);
      outcome = await workspace.send(
        bound.path,
        bound.action,
        bound.target,
        bound.body,
        owner.current.signal,
        bound.timeout,
        saved?.operation_id,
      );
      setReview(null);
      announce(`${outcome.outcome}${outcome.code ? ` · ${outcome.code}` : ""}`);
      if (["applied", "already_applied"].includes(outcome.outcome)) {
        if (saved) {
          const receipt = await readActionReceipt(saved, owner.current.signal);
          if (!receipt) throw new Error("Exact delivery receipt unavailable");
          recovery.forget(saved);
          setReceipt(receipt);
          setAcknowledged(true);
          announce("Exact original request receipt observed. No request was resent.");
          if (receipt.destination) location.hash = "#/" + receipt.destination;
          return;
        }
        setAcknowledged(true);
        await read(bound, true);
        onSettled?.();
      } else if (outcome.outcome === "unknown") {
        setUnknown(bound);
        await read(bound, false);
      } else {
        // refused or unreachable: nothing changed server-side, and the
        // operator still needs to see why and correct their draft - calling
        // onSettled here would let a caller's cleanup (clearing a create
        // form's fields) discard that draft on a FAILED submission.
        if (saved && ["refused", "unreachable"].includes(outcome.outcome)) recovery.forget(saved);
      }
    } catch {
      setReview(null);
      if (recoveryKey && !saved) {
        announce("Recovery reference could not be saved. Nothing was sent.");
        return;
      }
      setUnknown(bound);
      announce(
        `${outcome ? outcome.outcome + " response · " : ""}Outcome unresolved. Re-read the exact subject; do not resend.`,
      );
    } finally {
      lock.current = false;
      if (!owner.current.signal.aborted) setBusy(false);
    }
  };
  const recoverSaved = async (saved: (typeof recovery.pending)[number]) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    owner.current = new AbortController();
    try {
      const value = await readActionReceipt(saved, owner.current.signal);
      if (!value) {
        announce("No exact delivery receipt observed. Outcome remains unknown; no request was resent.");
        return;
      }
      recovery.forget(saved);
      setReceipt(value);
      setUnknown(null);
      setAcknowledged(true);
      announce("Exact original request receipt observed. No request was resent.");
      onSettled?.();
    } catch {
      announce("Reconciliation unavailable. Outcome remains unknown; no request was resent.");
    } finally {
      lock.current = false;
      if (!owner.current.signal.aborted) setBusy(false);
    }
  };
  const reconcile = async () => {
    if (!unknown || lock.current) return;
    lock.current = true;
    setBusy(true);
    owner.current = new AbortController();
    try {
      await read(unknown, false);
    } catch {
      announce("Reconciliation unavailable. Outcome remains unknown; no request was resent.");
    } finally {
      lock.current = false;
      if (!owner.current.signal.aborted) setBusy(false);
    }
  };
  return (
    <>
      {children}
      <button
        className="button"
        disabled={
          busy ||
          !!reason ||
          !!unknown ||
          acknowledged ||
          recovery.loading ||
          !!recovery.error ||
          !!recovery.pending.length
        }
        onClick={(e) => void begin(e.currentTarget)}
      >
        {busy ? "Reading…" : label}
      </button>
      {alternatives.map((a) => (
        <button
          className="button"
          key={a.label}
          disabled={busy || !!reason || !!unknown || acknowledged}
          onClick={(e) => void begin(e.currentTarget, a.prepare)}
        >
          {a.label}
        </button>
      ))}
      {reason && <p className="muted small governed-reason">{reason}</p>}
      {recovery.error && <p role="alert">{recovery.error}</p>}
      {recovery.pending.map((saved) => (
        <section className="governed-feedback" role="status" key={saved.operation_id}>
          <h3>Unresolved original operation</h3>
          <p>
            {saved.action} · {saved.subject}
          </p>
          <Disclosure title="Original delivery reference">
            <p className="mono exact-value">
              {saved.operation_id} · {saved.request_digest}
            </p>
            <p className="mono exact-value">
              {saved.path} · controller {saved.controller_binding}
            </p>
            {Object.entries(saved.bindings).map(([k, v]) => (
              <p className="mono exact-value" key={k}>
                {k}: {String(v)}
              </p>
            ))}
          </Disclosure>
          <button className="button" disabled={busy} onClick={() => void recoverSaved(saved)}>
            Re-read exact receipt
          </button>
        </section>
      ))}
      {(message || receipt) && (
        <section className="governed-feedback" ref={result} tabIndex={-1} role="status">
          {receipt ? (
            <>
              <h3>{receipt.title}</h3>
              <p className="mono exact-value">{receipt.detail}</p>
              {receipt.at && <time>{receipt.at}</time>}
              {receipt.destination && <Link to={receipt.destination}>Inspect recorded result</Link>}
            </>
          ) : (
            <p>{message}</p>
          )}
          {unknown && unknown.readReceipt && (
            <button className="button" disabled={busy} onClick={() => void reconcile()}>
              Re-read exact receipt
            </button>
          )}
        </section>
      )}
      {review && (
        <Dialog busy={busy} onClose={() => setReview(null)} returnFocus={opener.current}>
          <div className="confirmation-body">
            <p className="eyebrow">GOVERNED OPERATOR ACTION</p>
            <h2 id="confirmation-title">{review.title}</h2>
            <p className="exact-value">{review.subject}</p>
            <p>{review.consequence}</p>
            <Disclosure title="Exact subject, evidence & controller">
              <dl className="binding-list">
                {review.details.map(([name, value]) => (
                  <div key={name}>
                    <dt>{name}</dt>
                    <dd className="mono exact-value">{value}</dd>
                  </div>
                ))}
              </dl>
            </Disclosure>
            {noteAllowed && (
              <TextareaField
                label="Decision note (optional)"
                maxLength={2000}
                value={note}
                onChange={(e) => setNote(e.target.value)}
                help="The note is bound to this single submission; it cannot change its subject."
              />
            )}
          </div>
          <div className="dialog-buttons">
            <button className="button" disabled={busy} onClick={() => setReview(null)}>
              Cancel
            </button>
            <button className="button primary" disabled={busy} onClick={() => void submit()}>
              Confirm {review.action.replaceAll("-", " ")}
            </button>
          </div>
        </Dialog>
      )}
    </>
  );
}
