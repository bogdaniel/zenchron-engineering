import { useEffect, useState } from "react";
import type { Plan, Stage } from "./api";
import { Empty, Panel } from "./components";
import { Facts } from "./runs";
import * as ops from "./operations-api";
import { noPullRequestReason, pullRequestURL } from "./outcome-reading";

type Loaded = { detail?: ops.RunDetail; error?: string };

// The producers whose output a person receives: agent stages that are not
// reviews and that were given a run.
const deliveredBy = (stages: Stage[]) => stages.filter((s) => s.kind === "agent" && !s.review && s.run_id);

function RunOutcome({ stage, loaded }: { stage: Stage; loaded?: Loaded }) {
  const d = loaded?.detail;
  if (!d)
    return (
      <Facts
        rows={[
          ["Producer run", stage.run_id],
          ["Outcome", loaded?.error ? "Unavailable: " + loaded.error : "Reading run"],
        ]}
      />
    );
  const pr = d.pull_request;
  const url = pr ? pullRequestURL(d.repository, pr.number) : "";
  return (
    <>
      <Facts
        rows={[
          ["Producer run", `${stage.run_id} · ${stage.agent || "agent not recorded"}`],
          ["Candidate revision", d.candidate?.revision || "Not recorded"],
          [
            "Assurance",
            d.assurance
              ? `${d.assurance.passed ? "Passed" : "Failed"}${d.assurance.stale ? " · stale" : ""}${d.assurance.failure_class ? " · " + d.assurance.failure_class : ""}`
              : "Not recorded",
          ],
          ["Publication authority", d.publication_authority?.status || "Not recorded"],
        ]}
      />
      <p>
        {pr ? (
          <>
            Pull request{" "}
            {url ? (
              <a href={url} rel="noopener noreferrer">
                #{pr.number}
              </a>
            ) : (
              `#${pr.number}`
            )}{" "}
            · {pr.state || "state unknown"}
            {pr.merged ? " · merged" : ""}
            {pr.stale ? " · observation stale" : ""}
          </>
        ) : (
          <>No pull request yet: {noPullRequestReason(d)}.</>
        )}
      </p>
    </>
  );
}

export function ObjectiveOutcome({ plan, pending }: { plan: Plan; pending: boolean }) {
  const producers = deliveredBy(plan.stages);
  const key = producers.map((s) => s.run_id).join(",");
  const [runs, setRuns] = useState<Record<string, Loaded>>({});
  useEffect(() => {
    const owner = new AbortController();
    for (const stage of deliveredBy(plan.stages))
      ops
        .run(stage.run_id, owner.signal)
        .then((detail) => setRuns((r) => ({ ...r, [stage.run_id]: { detail } })))
        .catch((e) => {
          if (!owner.signal.aborted)
            setRuns((r) => ({ ...r, [stage.run_id]: { error: e instanceof Error ? e.message : "read failed" } }));
        });
    return () => owner.abort();
    // The plan object changes on every refresh; the producer runs are what to re-read.
  }, [key, plan.stages]);
  const reviews = plan.stages.filter((s) => s.review);
  const gates = plan.stages.filter((s) => s.kind === "assurance_gate");
  const awaiting = Object.values(runs).flatMap((r) =>
    r.detail?.authority_request?.missing.length ? [r.detail.run_id] : [],
  );
  const waitingOn = [pending && "a decision on this plan revision", ...awaiting.map((id) => `publication authority for ${id}`)].filter(
    Boolean,
  );
  return (
    <Panel title="Outcome">
      <div className="product-body">
        <p className="muted small">
          A completed plan does not mean the change was published. This panel reads each fact from its own record.
        </p>
        {producers.length === 0 && reviews.length === 0 ? (
          <Empty title="No delivered work recorded">No producer run has been recorded for this plan revision.</Empty>
        ) : null}
        {reviews.map((s) => (
          <Facts
            key={s.stage_id}
            rows={[
              ["Independent review", `${s.review?.verdict || "Not recorded"} · ${s.agent || "reviewer not recorded"}`],
              ["Reviewed candidate", s.review?.candidate || "Not recorded"],
            ]}
          />
        ))}
        {gates.map((s) => (
          <Facts key={s.stage_id} rows={[["Assurance gate", s.state || "Not recorded"]]} />
        ))}
        {producers.map((s) => (
          <RunOutcome key={s.stage_id} stage={s} loaded={runs[s.run_id]} />
        ))}
        <p>Awaiting a human: {waitingOn.length ? waitingOn.join("; ") : "nothing recorded"}.</p>
      </div>
    </Panel>
  );
}
