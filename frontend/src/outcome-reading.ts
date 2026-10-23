import type { RunDetail } from "./operations-api";

// A pull request is linked only for a repository identity that is exactly
// owner/name, so a malformed read can never become an arbitrary URL.
export const pullRequestURL = (repository: string, number: number) =>
  /^[\w.-]+\/[\w.-]+$/.test(repository) ? `https://github.com/${repository}/pull/${number}` : "";

// Why no pull request exists, from recorded authority state only. Anything the
// record does not say stays unsaid: an absent PR is never explained by a guess.
export function noPullRequestReason(d: Pick<RunDetail, "authority_request" | "publication_authority">): string {
  const request = d.authority_request;
  if (request && request.missing.length)
    return `awaiting human authority: ${request.missing.join(", ")} missing (${request.status})`;
  if (d.publication_authority?.status && d.publication_authority.status !== "authorized")
    return `publication authority is ${d.publication_authority.status}`;
  if (d.publication_authority?.status === "authorized")
    return "publication is authorized and the runtime has not yet recorded a pull request";
  return "no publication authority is recorded; publication is deferred or was not requested";
}

// Failure classes and stages are closed sets of identifiers the runtime itself
// names. They are shown verbatim and only when they look like one, so a
// malformed read can never put arbitrary text in the panel.
const identifier = (value: string) => (/^[a-z][a-z0-9_.-]{0,63}$/.test(value) ? value : "");

// Why a run ended failed, from its recorded diagnostic only. Nothing here
// suggests a recovery: none is recorded, and a missing field stays unsaid.
export function failedRunLines(d: Pick<RunDetail, "disposition" | "execution_diagnostic">): string[] {
  if (d.disposition !== "failed") return [];
  const x = d.execution_diagnostic;
  const stage = identifier(x?.stage ?? ""),
    failureClass = identifier(x?.failure_class ?? "");
  const lines = [
    failureClass ? `Failed${stage ? " at " + stage : ""}: ${failureClass}` : "Failed: no failure class is recorded",
  ];
  const unavailable = identifier(x?.successor_unavailable ?? ""),
    successor = identifier(x?.successor ?? "");
  if (unavailable) lines.push(`No retry is available: ${unavailable}`);
  else if (successor) lines.push(`Next recorded step: ${successor}`);
  return lines;
}

type StageEdge = { stage_id: string; state: string; depends_on: string[] };

// The failed stage a producer's verified candidate is waiting behind, if the
// record shows one: the candidate has a recorded revision and a PASSED
// assurance, no pull request exists, and a stage that depends on the producer
// (directly or through others) failed. Unrecorded assurance is not "verified",
// so nothing is claimed for it.
export function heldBehindFailedStage(
  producerId: string,
  stages: StageEdge[],
  d: Pick<RunDetail, "candidate" | "assurance" | "pull_request">,
): string {
  if (!d.candidate?.revision || d.assurance?.passed !== true || d.pull_request) return "";
  const downstream = new Set([producerId]);
  // ponytail: repeated scan, fine for a plan's handful of stages; index by edge if plans grow wide.
  for (let changed = true; changed; ) {
    changed = false;
    for (const s of stages)
      if (!downstream.has(s.stage_id) && s.depends_on.some((id) => downstream.has(id))) {
        downstream.add(s.stage_id);
        changed = true;
      }
  }
  return stages.find((s) => s.state === "failed" && s.stage_id !== producerId && downstream.has(s.stage_id))?.stage_id ?? "";
}
