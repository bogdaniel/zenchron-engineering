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
