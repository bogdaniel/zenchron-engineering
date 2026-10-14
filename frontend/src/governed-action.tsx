import { useEffect, useRef, useState, type ReactNode } from "react";
import { Dialog, Disclosure } from "./components";
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
  after?: (result: workspace.Result) => void;
};
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
}: {
  label: string;
  reason: string;
  prepare: (note: string, signal: AbortSignal) => Promise<ActionReview>;
  onSettled?: () => void;
  noteAllowed?: boolean;
  children?: ReactNode;
  alternatives?: { label: string; prepare: (note: string, signal: AbortSignal) => Promise<ActionReview> }[];
  onBlocked?: (blocked: boolean) => void;
}) {
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
    onBlocked?.(busy || !!review || !!unknown);
  }, [busy, review, unknown, onBlocked]);
  useEffect(() => () => onBlocked?.(false), [onBlocked]);
  const begin = async (button: HTMLButtonElement, choose = prepare) => {
    if (lock.current || reason || unknown || acknowledged) return;
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
    try {
      outcome = await workspace.send(
        bound.path,
        bound.action,
        bound.target,
        bound.body,
        owner.current.signal,
        bound.timeout,
      );
      setReview(null);
      announce(`${outcome.outcome}${outcome.code ? ` · ${outcome.code}` : ""}`);
      if (["applied", "already_applied"].includes(outcome.outcome)) {
        setAcknowledged(true);
        bound.after?.(outcome);
        await read(bound, true);
        onSettled?.();
      } else if (outcome.outcome === "unknown") {
        setUnknown(bound);
        await read(bound, false);
      } else onSettled?.();
    } catch {
      setReview(null);
      setUnknown(bound);
      announce(
        `${outcome ? outcome.outcome + " response · " : ""}Outcome unresolved. Re-read the exact subject; do not resend.`,
      );
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
        disabled={busy || !!reason || !!unknown || acknowledged}
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
      {(message || receipt) && (
        <section className="governed-feedback" ref={result} tabIndex={-1} role="status">
          {receipt ? (
            <>
              <h3>{receipt.title}</h3>
              <p className="mono exact-value">{receipt.detail}</p>
              {receipt.at && <time>{receipt.at}</time>}
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
              <label className="field-label">
                Decision note (optional)
                <textarea
                  aria-label="Decision note (optional)"
                  maxLength={2000}
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                />
                <small className="muted">
                  The note is bound to this single submission; it cannot change its subject.
                </small>
              </label>
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
