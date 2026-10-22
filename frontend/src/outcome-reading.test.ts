import assert from "node:assert/strict";
import test from "node:test";
import { noPullRequestReason, pullRequestURL } from "./outcome-reading.ts";

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
