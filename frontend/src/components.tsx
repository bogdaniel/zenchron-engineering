import { useEffect, useRef, type ReactNode } from "react";
import type { Controller, Plan } from "./api";

export function Badge({ children, tone = "neutral" }: { children: ReactNode; tone?: string }) {
  return (
    <span className={`badge ${tone}`}>
      <span className="status-dot" aria-hidden="true" />
      {children}
    </span>
  );
}
export function Link({
  to,
  children,
  className = "text-link",
}: {
  to: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <a href={`#/${to}`} className={className}>
      {children} <span aria-hidden="true">↗</span>
    </a>
  );
}
export function Panel({ title, children, extra }: { title: string; children: ReactNode; extra?: ReactNode }) {
  return (
    <section className="panel">
      <header className="panel-head">
        <h2>{title}</h2>
        {extra}
      </header>
      {children}
    </section>
  );
}
export function Head({
  eyebrow,
  title,
  children,
  extra,
}: {
  eyebrow: string;
  title: string;
  children: ReactNode;
  extra?: ReactNode;
}) {
  return (
    <header className="page-head">
      <div>
        <p className="eyebrow">{eyebrow}</p>
        <h1>{title}</h1>
        <p className="page-description">{children}</p>
      </div>
      {extra && <div className="page-actions">{extra}</div>}
    </header>
  );
}
export function Empty({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="empty-state">
      <h3>{title}</h3>
      <p className="muted">{children}</p>
    </div>
  );
}
export function Disclosure({ title, children }: { title: string; children: ReactNode }) {
  return (
    <details className="disclosure">
      <summary>
        <span>{title}</span>
        <span aria-hidden="true">⌄</span>
      </summary>
      <div className="disclosure-body">{children}</div>
    </details>
  );
}
export function Binding({ plan, binding, action }: { plan: Plan; binding?: string; action?: string }) {
  return (
    <dl className="binding-list">
      <dt>Plan / revision</dt>
      <dd className="mono exact-value">
        {plan.plan_id} · r{plan.revision}
      </dd>
      <dt>Exact digest</dt>
      <dd className="mono exact-value">{plan.digest}</dd>
      <dt>Repository / base</dt>
      <dd className="mono exact-value">
        {plan.repository}
        <br />
        {plan.base_revision || "Unavailable"}
      </dd>
      {action && (
        <>
          <dt>Protected action</dt>
          <dd>{action} · this exact revision only</dd>
        </>
      )}
      {binding !== undefined && (
        <>
          <dt>Serving controller binding</dt>
          <dd className="mono exact-value">{binding || "Unattested / unobserved"}</dd>
        </>
      )}
    </dl>
  );
}
export function ControllerFacts({ controller: c, stale = false }: { controller: Controller; stale?: boolean }) {
  return (
    <dl className="binding-list summary-binding">
      <dt>Live endpoint</dt>
      <dd className={stale || !c.live_reachable ? "orange" : "secondary"}>
        {stale ? "Unobserved now · retained snapshot" : c.live_reachable ? "Reachable at read time" : "Unreachable"}
      </dd>
      <dt>Durable generation</dt>
      <dd>
        {c.durable_generation?.kind || "Not recorded"}
        <small className="row-meta">Identity record; does not prove reachability</small>
      </dd>
      <dt>Live identity</dt>
      <dd>{c.controller_binding ? "Attested at read time" : "Unattested / unobserved"}</dd>
      <dt>Serving / role</dt>
      <dd>
        {c.serving} / {c.role || "Unknown"}
      </dd>
      <dt>Generation match</dt>
      <dd>{c.generation_match}</dd>
      <dt>Work admission</dt>
      <dd>{c.work_admission || "Unknown"}</dd>
      <dt>Durable consistency</dt>
      <dd>{c.durable_consistency}</dd>
    </dl>
  );
}
export function Pagination({
  offset,
  more,
  count,
  onPage,
}: {
  offset: number;
  more: boolean;
  count: number;
  onPage: (v: number) => void;
}) {
  return (
    <div className="page-controls panel-footer" role="group" aria-label="Plan pagination">
      <span>
        {count} loaded · offset {offset}
        {more ? " · more available" : ""}
      </span>
      <button className="button" disabled={!offset} onClick={() => onPage(Math.max(0, offset - 50))}>
        Previous
      </button>
      <button className="button" disabled={!more} onClick={() => onPage(offset + 50)}>
        Next
      </button>
    </div>
  );
}
export function Dialog({
  children,
  onClose,
  busy,
  returnFocus,
}: {
  children: ReactNode;
  onClose: () => void;
  busy: boolean;
  returnFocus?: HTMLElement | null;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const opener = returnFocus || document.activeElement;
    const dialog = ref.current;
    dialog?.showModal();
    return () => {
      dialog?.close();
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      aria-labelledby="confirmation-title"
      onKeyDown={(event) => {
        if (event.key !== "Tab") return;
        const controls = [
          ...event.currentTarget.querySelectorAll<HTMLElement>("button,summary,textarea,input,a[href]"),
        ].filter((node) => node.getClientRects().length && !node.matches(":disabled"));
        const first = controls[0],
          last = controls.at(-1);
        if (event.shiftKey && document.activeElement === first) {
          event.preventDefault();
          last?.focus();
        }
        if (!event.shiftKey && document.activeElement === last) {
          event.preventDefault();
          first?.focus();
        }
      }}
      onCancel={(e) => {
        e.preventDefault();
        if (!busy) onClose();
      }}
    >
      <div className="decision-confirmation">{children}</div>
    </dialog>
  );
}
