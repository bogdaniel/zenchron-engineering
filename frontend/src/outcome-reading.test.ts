import assert from "node:assert/strict";
import test from "node:test";
import type { RunDetail } from "./operations-api.ts";
import { failedRunLines, heldBehindFailedStage, noPullRequestReason, pullRequestURL } from "./outcome-reading.ts";

test("a missing pull request is explained only by recorded authority state", () => {
  assert.match(
    noPullRequestReason({ authority_request: { status: "awaiting", requires: [], missing: ["human_approval"], stale: [] } }),
    /awaiting human authority: human_approval missing/,
  );
  assert.match(
    noPullRequestReason({ publication_authority: { status: "incomplete", action_type: "", action_target: "" } }),
    /authority is incomplete/,
  );
  assert.match(
    noPullRequestReason({ publication_authority: { status: "authorized", action_type: "", action_target: "" } }),
    /authorized and the runtime has not yet recorded/,
  );
  assert.match(noPullRequestReason({}), /no publication authority is recorded/);
});

test("a pull request links only for an exact owner/name repository", () => {
  assert.equal(pullRequestURL("acme/shop", 7), "https://github.com/acme/shop/pull/7");
  assert.equal(pullRequestURL("acme/shop/../../evil", 7), "");
  assert.equal(pullRequestURL("javascript:alert(1)", 7), "");
});

const diagnostic = (over: Partial<NonNullable<RunDetail["execution_diagnostic"]>>) => ({
  stage: "",
  failure_class: "",
  bound: "",
  successor: "",
  successor_unavailable: "",
  ...over,
});

test("a failed run says its recorded class and that no retry exists, and nothing else", () => {
  // The #659 reviewer run, as the projection reports it.
  assert.deepEqual(
    failedRunLines({
      disposition: "failed",
      execution_diagnostic: diagnostic({
        stage: "provider_result",
        failure_class: "reviewer_protocol_incomplete",
        successor_unavailable: "execution_attempts_exhausted",
      }),
    }),
    ["Failed at provider_result: reviewer_protocol_incomplete", "No retry is available: execution_attempts_exhausted"],
  );
  // A recorded successor is a recorded next step; its absence is never read as "none".
  assert.deepEqual(
    failedRunLines({
      disposition: "failed",
      execution_diagnostic: diagnostic({ failure_class: "verification", successor: "remediation" }),
    }),
    ["Failed: verification", "Next recorded step: remediation"],
  );
  // No diagnostic at all stays unknown, not "no failure".
  assert.deepEqual(failedRunLines({ disposition: "failed" }), ["Failed: no failure class is recorded"]);
  // A run that did not fail says nothing, and text that is not an identifier is not shown.
  assert.deepEqual(failedRunLines({ disposition: "waiting" }), []);
  assert.deepEqual(
    failedRunLines({
      disposition: "failed",
      execution_diagnostic: diagnostic({
        stage: "<b>x</b>",
        failure_class: "<script>",
        successor_unavailable: "see /Users/me/secret",
      }),
    }),
    ["Failed: no failure class is recorded"],
  );
});

test("a verified candidate is held only behind a downstream stage that recorded failure", () => {
  const stages = [
    { stage_id: "write", state: "completed", depends_on: [] },
    { stage_id: "review", state: "failed", depends_on: ["write"] },
    { stage_id: "gate", state: "", depends_on: ["review"] },
    { stage_id: "elsewhere", state: "failed", depends_on: [] },
  ];
  const passed = { passed: true, failure_class: "", stale: false };
  const verified = { candidate: { branch: "b", revision: "abc", tree: "t" }, assurance: passed };
  assert.equal(heldBehindFailedStage("write", stages, verified), "review");
  // Unrecorded or failed assurance is not "verified"; a PR means nothing is held; an unrelated failure is no reason.
  assert.equal(heldBehindFailedStage("write", stages, { candidate: verified.candidate }), "");
  assert.equal(heldBehindFailedStage("write", stages, { ...verified, assurance: { ...passed, passed: false } }), "");
  const pr = { number: 1, state: "open", merged: false, stale: false };
  assert.equal(heldBehindFailedStage("write", stages, { ...verified, pull_request: pr }), "");
  assert.equal(heldBehindFailedStage("write", [stages[0], stages[3]], verified), "");
  // A failure two edges away still holds it.
  const deep = [stages[0], { ...stages[1], state: "completed" }, { ...stages[2], state: "failed" }];
  assert.equal(heldBehindFailedStage("write", deep, verified), "gate");
});
