import { useEffect, useState } from "react";
import * as ops from "./operations-api";
import { Empty, Panel } from "./components";

export function JournalHistory({
  id,
  initialAfter = 0,
  kind = "run",
}: {
  id: string;
  initialAfter?: number;
  kind?: "run" | "plan";
}) {
  const [after, setAfter] = useState(initialAfter),
    [previous, setPrevious] = useState<number[]>([]);
  const [page, setPage] = useState<ops.Events | null>(null),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(true);
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const owner = new AbortController();
    setBusy(true);
    setError("");
    setPage(null);
    ops
      .events(id, after, owner.signal, kind)
      .then((v) => {
        if (!owner.signal.aborted) setPage(v);
      })
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "read_failed");
      })
      .finally(() => {
        if (!owner.signal.aborted) setBusy(false);
      });
    return () => owner.abort();
  }, [id, after, refresh, kind]);
  return (
    <Panel
      title={kind === "run" ? "Run activity" : "Plan activity"}
      extra={<span className="muted small">Journal metadata · no payloads</span>}
    >
      {busy ? (
        <div className="product-body" role="status">
          Reading events after sequence {after}…
        </div>
      ) : error ? (
        <Empty title="Activity unavailable">{error}. Missing events are not interpreted as an empty history.</Empty>
      ) : page?.events.length ? (
        <ol className="activity-timeline run-timeline" tabIndex={0} aria-label={`${kind} event history`}>
          {page.events.map((e) => (
            <li key={e.sequence}>
              <time dateTime={e.occurred_at}>
                {new Date(e.occurred_at).toISOString().slice(0, 10)}
                <br />
                {e.occurred_at.slice(11, 19)} UTC
              </time>
              <span className="timeline-dot muted" aria-hidden="true" />
              <div>
                <strong>{e.type}</strong>
                <p className="muted mono">Sequence {e.sequence}</p>
              </div>
            </li>
          ))}
        </ol>
      ) : (
        <Empty title="No later events recorded">
          No journal events after sequence {after} in this read. Operation heartbeat is a separate observation.
        </Empty>
      )}
      <div className="page-controls panel-footer" role="group" aria-label="Event pagination">
        <span>{page ? `${page.events.length} events · after ${after}` : `After ${after}`}</span>
        <button
          className="button"
          disabled={busy || !previous.length}
          onClick={() => {
            setAfter(previous.at(-1)!);
            setPrevious((p) => p.slice(0, -1));
          }}
        >
          Previous events
        </button>
        <button
          className="button"
          disabled={busy || !page?.has_more}
          onClick={() => {
            setPrevious((p) => [...p, after]);
            setAfter(page!.next_after);
          }}
        >
          Next events
        </button>
        <button className="button" disabled={busy} onClick={() => setRefresh((k) => k + 1)}>
          Refresh events
        </button>
      </div>
    </Panel>
  );
}
