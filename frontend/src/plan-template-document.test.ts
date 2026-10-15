import { test } from "node:test";
import assert from "node:assert/strict";
import { templateDocumentFrom, emptyStage, type DraftStage } from "./plan-template-document.ts";

function stage(id: string, overrides: Partial<DraftStage> = {}): DraftStage {
  return { ...emptyStage(), id, ...overrides };
}

test("ids and the template id are trimmed", () => {
  const { id, document } = templateDocumentFrom("  checkout  ", "", [stage("  a  ")]);
  assert.equal(id, "checkout");
  assert.equal(document.stages[0].id, "a");
});

test("an agent stage carries role and profile; a gate carries neither", () => {
  const { document } = templateDocumentFrom("t", "", [
    stage("a", { kind: "agent", role: "implementer", profile: "reviewer" }),
    stage("b", { kind: "assurance_gate", role: "implementer", profile: "reviewer" }),
  ]);
  assert.deepEqual(document.stages[0], { id: "a", kind: "agent", role: "implementer", profile: "reviewer" });
  assert.deepEqual(document.stages[1], { id: "b", kind: "assurance_gate" });
});

test("a dependency naming a stage that was renamed or removed is dropped, not submitted dangling", () => {
  const { document } = templateDocumentFrom("t", "", [
    stage("a"),
    stage("b", { dependsOn: ["a", "ghost"] }),
  ]);
  assert.deepEqual(document.stages[1].depends_on, ["a"]);
});

test("a stage with no surviving dependency carries no depends_on key at all", () => {
  const { document } = templateDocumentFrom("t", "", [stage("a", { dependsOn: ["ghost"] })]);
  assert.equal("depends_on" in document.stages[0], false);
});

test("description is omitted when blank, present when stated", () => {
  assert.equal("description" in templateDocumentFrom("t", "   ", [stage("a")]).document, false);
  assert.equal(templateDocumentFrom("t", "  do the thing  ", [stage("a")]).document.description, "do the thing");
});
