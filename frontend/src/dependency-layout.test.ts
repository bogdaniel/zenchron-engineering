import { test } from "node:test";
import assert from "node:assert/strict";
import { layoutDependencies, dependencyGraphSize, type DependencyNode } from "./dependency-layout.ts";

function n(id: string, ...depends_on: string[]): DependencyNode {
  return { id, depends_on };
}

test("linear chain places each stage in its own column, in order", () => {
  const layout = layoutDependencies([n("a"), n("b", "a"), n("c", "b")]);
  assert(layout);
  assert.equal(layout.get("a")!.depth, 0);
  assert.equal(layout.get("b")!.depth, 1);
  assert.equal(layout.get("c")!.depth, 2);
  assert.ok(layout.get("a")!.x < layout.get("b")!.x);
  assert.ok(layout.get("b")!.x < layout.get("c")!.x);
});

test("parallel branches share a column and converge at the join", () => {
  const layout = layoutDependencies([n("a"), n("b", "a"), n("c", "a"), n("d", "b", "c")]);
  assert(layout);
  assert.equal(layout.get("b")!.depth, 1);
  assert.equal(layout.get("c")!.depth, 1);
  assert.equal(layout.get("b")!.x, layout.get("c")!.x);
  assert.notEqual(layout.get("b")!.y, layout.get("c")!.y);
  assert.equal(layout.get("d")!.depth, 2);
});

test("multiple independent roots both start at depth 0", () => {
  const layout = layoutDependencies([n("a"), n("b"), n("c", "a"), n("d", "b")]);
  assert(layout);
  assert.equal(layout.get("a")!.depth, 0);
  assert.equal(layout.get("b")!.depth, 0);
});

test("an unknown dependency reference is dropped, not a crash", () => {
  const layout = layoutDependencies([n("a", "missing-stage")]);
  assert(layout);
  assert.equal(layout.get("a")!.depth, 0);
});

test("a self-dependency is dropped, not an infinite wait", () => {
  const layout = layoutDependencies([n("a", "a")]);
  assert(layout);
  assert.equal(layout.get("a")!.depth, 0);
});

test("a cycle reports no layout rather than an invented ordering", () => {
  const layout = layoutDependencies([n("a", "b"), n("b", "a")]);
  assert.equal(layout, null);
});

test("a duplicate stage id reports no layout", () => {
  const layout = layoutDependencies([n("a"), n("a", "a")]);
  assert.equal(layout, null);
});

test("an empty graph lays out to an empty map, not an error", () => {
  const layout = layoutDependencies([]);
  assert(layout);
  assert.equal(layout.size, 0);
});

test("row order is independent of input array order", () => {
  const forward = layoutDependencies([n("a"), n("b"), n("c", "a"), n("d", "b")]);
  const shuffled = layoutDependencies([n("d", "b"), n("c", "a"), n("b"), n("a")]);
  assert(forward && shuffled);
  for (const id of ["a", "b", "c", "d"]) assert.deepEqual(forward.get(id), shuffled.get(id));
});

test("dependencyGraphSize grows with column and row count", () => {
  const layout = layoutDependencies([n("a"), n("b", "a"), n("c", "a")])!;
  const size = dependencyGraphSize(layout);
  assert.ok(size.width > 0 && size.height > 0);
});
