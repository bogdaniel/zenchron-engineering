import { test } from "node:test";
import assert from "node:assert/strict";
import {
  templateDocumentFrom,
  emptyStage,
  renameStageID,
  dependentStageIDs,
  dependencyEdges,
  type DraftStage,
} from "./plan-template-document.ts";

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

test("renaming a stage propagates the new id into every dependent's dependsOn", () => {
  const renamed = renameStageID([stage("a"), stage("b", { dependsOn: ["a"] })], 0, "a-renamed");
  assert.equal(renamed[0].id, "a-renamed");
  assert.deepEqual(renamed[1].dependsOn, ["a-renamed"]);
});

test("renaming a stage leaves a dependent naming a DIFFERENT stage untouched", () => {
  const renamed = renameStageID([stage("a"), stage("b", { dependsOn: ["c"] }), stage("c")], 0, "a2");
  assert.deepEqual(renamed[1].dependsOn, ["c"]);
});

test("renaming to the same trimmed id is a no-op on dependents", () => {
  const renamed = renameStageID([stage("a"), stage("b", { dependsOn: ["a"] })], 0, "  a  ");
  assert.deepEqual(renamed[1].dependsOn, ["a"]);
});

test("dependentStageIDs names every stage that depends on the given one", () => {
  const stages = [stage("a"), stage("b", { dependsOn: ["a"] }), stage("c", { dependsOn: ["a"] }), stage("d")];
  assert.deepEqual(dependentStageIDs(stages, 0), ["b", "c"]);
  assert.deepEqual(dependentStageIDs(stages, 3), []);
});

test("dependencyEdges reflects real edges, not list order - a later stage may skip an earlier one entirely", () => {
  // b has no dependency on a; c depends on a only, not on b. Joining ids by
  // list order ("a -> b -> c") would wrongly imply b sits between them.
  const edges = dependencyEdges([stage("a"), stage("b"), stage("c", { dependsOn: ["a"] })]);
  assert.deepEqual(edges, [
    { id: "a", dependsOn: [] },
    { id: "b", dependsOn: [] },
    { id: "c", dependsOn: ["a"] },
  ]);
});
