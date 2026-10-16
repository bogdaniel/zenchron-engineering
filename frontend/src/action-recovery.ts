import { useEffect, useState } from "react";
import * as api from "./api";
import type { ActionReview } from "./governed-action";
import type { Receipt } from "./workspace-api";

type PendingAction = {
  version: 1;
  scope: string;
  operation_id: string;
  request_digest: string;
  controller_binding: string;
  action: string;
  subject: string;
  path: string;
  bindings: Record<string, string | number | boolean>;
};
const reference = /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/;
async function digest(value: string) {
  return [...new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value)))]
    .map((v) => v.toString(16).padStart(2, "0"))
    .join("");
}
function decode(value: unknown): PendingAction {
  const r = api.object(value);
  if (
    r.version !== 1 ||
    !reference.test(api.str(r.operation_id)) ||
    !/^[a-f0-9]{64}$/.test(api.str(r.request_digest)) ||
    !["plan-revise", "resume", "refresh", "planning-artifact-propose", "product-propose"].includes(api.str(r.action))
  )
    throw new Error("Invalid saved action reference");
  return {
    version: 1,
    scope: api.str(r.scope),
    operation_id: api.str(r.operation_id),
    request_digest: api.str(r.request_digest),
    controller_binding: api.str(r.controller_binding),
    action: api.str(r.action),
    subject: api.str(r.subject),
    path: api.str(r.path),
    bindings: Object.fromEntries(
      Object.entries(api.object(r.bindings)).map(([k, v]) => {
        if (!["string", "number", "boolean"].includes(typeof v)) throw new Error("Invalid original binding");
        return [k, v as string | number | boolean];
      }),
    ),
  };
}

export async function readActionReceipt(p: PendingAction, signal: AbortSignal): Promise<Receipt | null> {
  let r: Record<string, unknown>;
  try {
    r = api.object(
      (await api.request(`/v1/operator-action-receipts/${p.operation_id}?digest=${p.request_digest}`, signal)).value,
    );
  } catch (e) {
    if (e instanceof Error && e.message === "not_found") return null;
    throw e;
  }
  if (
    r.operation_id !== p.operation_id ||
    r.request_digest !== p.request_digest ||
    r.action !== p.action ||
    r.controller_binding !== p.controller_binding
  )
    throw new Error("Recovered receipt does not match the original request");
  let destination: string | undefined;
  if (p.action === "plan-revise") {
    const id = api.str(r.plan_id),
      revision = api.num(r.revision),
      planDigest = api.str(r.plan_digest);
    if (!id || revision < 1 || !/^[a-f0-9]{64}$/.test(planDigest)) throw new Error("Invalid recovered plan reference");
    if (
      (p.bindings.plan_id && (id !== p.bindings.plan_id || revision <= Number(p.bindings.revision))) ||
      (p.bindings.repository && (r.repository !== p.bindings.repository || r.source_issue !== p.bindings.issue))
    )
      throw new Error("Recovered plan differs from the original exact subject");
    destination = `objectives/${encodeURIComponent(id)}/${revision}`;
  } else if (p.action === "resume" || p.action === "refresh") {
    const id = api.str(r.run_id);
    if (
      !id ||
      r.source_run_id !== p.bindings.run_id ||
      (p.action === "resume" && id !== p.bindings.run_id) ||
      (p.action === "refresh" && id === p.bindings.run_id)
    )
      throw new Error("Invalid recovered run reference");
    destination = `runs/${encodeURIComponent(id)}`;
  } else if (p.action === "planning-artifact-propose") {
    const kind = api.str(r.artifact_kind),
      id = api.str(r.artifact_id),
      artifactDigest = api.str(r.artifact_digest);
    if (!kind || !id || !/^[a-f0-9]{64}$/.test(artifactDigest)) throw new Error("Invalid recovered artifact reference");
    if (
      (p.bindings.kind && kind !== p.bindings.kind) ||
      (p.bindings.id && id !== p.bindings.id)
    )
      throw new Error("Recovered artifact differs from the original exact subject");
  } else {
    const id = api.str(r.product_id),
      revision = api.num(r.product_revision);
    if (!id || revision < 1) throw new Error("Invalid recovered product reference");
    if (p.bindings.revision && revision !== Number(p.bindings.revision))
      throw new Error("Recovered product differs from the original exact subject");
    destination = `products/${encodeURIComponent(id)}`;
  }
  return {
    title: "Historical operator action receipt",
    detail: `${p.action} · ${p.subject} · request ${p.operation_id}`,
    destination,
  };
}

// Browser delivery references survive navigation/reload; they are never action
// eligibility or a retry queue. Credentials, bodies and notes are not persisted.
export function useActionRecovery(scope?: string) {
  const [prefix, setPrefix] = useState(""),
    [pending, setPending] = useState<PendingAction[]>([]),
    [error, setError] = useState(""),
    [loading, setLoading] = useState(!!scope);
  const read = (prefix: string) => {
    const rows: PendingAction[] = [];
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i)!;
      if (!key.startsWith(prefix)) continue;
      const raw = localStorage.getItem(key)!;
      if (raw.length > 8192) throw new Error("Saved recovery reference exceeds its bound");
      const row = decode(JSON.parse(raw));
      if (key !== prefix + row.operation_id) throw new Error("Saved recovery identity mismatch");
      if (row.scope === scope) rows.push(row);
    }
    setPending(rows);
    return rows;
  };
  useEffect(() => {
    if (!scope) return;
    let cancelled = false;
    let key = "";
    setLoading(true);
    setError("");
    const reload = (prefix: string) => {
      try {
        read(prefix);
      } catch {
        setError("Recovery storage unavailable or invalid; actions remain closed");
      }
    };
    const changed = () => {
      if (key) reload(key);
    };
    Promise.resolve()
      .then(() => digest(api.credential()))
      .then((namespace) => {
        if (cancelled) return;
        key = `zenchron.action-recovery.${namespace}.`;
        setPrefix(key);
        reload(key);
        setLoading(false);
      })
      .catch(() => {
        if (!cancelled) {
          setError("Recovery identity unavailable; actions remain closed");
          setLoading(false);
        }
      });
    addEventListener("storage", changed);
    return () => {
      cancelled = true;
      removeEventListener("storage", changed);
    };
  }, [scope]);
  const remember = async (bound: ActionReview, signal: AbortSignal) => {
    if (!scope || !prefix || error || read(prefix).length) throw new Error("Resolve the saved original request first");
    const p: PendingAction = {
      version: 1,
      scope,
      operation_id: crypto.randomUUID(),
      request_digest: await digest(bound.path + "\n" + JSON.stringify(bound.body)),
      controller_binding: api.str(bound.body.controller_binding),
      action: bound.action,
      subject: bound.subject.slice(0, 1000),
      path: bound.path,
      bindings: Object.fromEntries(
        Object.entries({ ...bound.target, ...bound.body }).filter(([k]) =>
          [
            "plan_id",
            "run_id",
            "revision",
            "digest",
            "repository",
            "issue",
            "agent",
            "profile",
            "template",
            "deterministic",
            "controller_binding",
            "kind",
            "id",
            "name",
          ].includes(k),
        ),
      ) as Record<string, string | number | boolean>,
    };
    if (signal.aborted) throw new Error("Cancelled before submission");
    const encoded = JSON.stringify(p);
    if (encoded.length > 8192) throw new Error("Original recovery reference exceeds its bound");
    localStorage.setItem(prefix + p.operation_id, encoded);
    read(prefix);
    return p;
  };
  const forget = (p: PendingAction) => {
    localStorage.removeItem(prefix + p.operation_id);
    read(prefix);
  };
  return { pending, error, loading, remember, forget };
}
