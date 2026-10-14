// Pure graph layout, no React/JSX - shared by WorkGraph's WorkUnit map
// (workgraphs.tsx) and the plan stage graph (dependency-graph.tsx), and
// directly testable with the plain Node test runner (no bundler needed).
export type DependencyNode = { id: string; depends_on: string[] };
export type DependencyPosition = { depth: number; x: number; y: number };
export const NODE_WIDTH = 220;
export const NODE_HEIGHT = 86;
export const COLUMN_GAP = 260;
export const ROW_GAP = 115;
export const MARGIN = 20;

// One pass over actual dependencies. A cycle, a duplicate id or a reference
// to an id outside this exact node set is a malformed observation, never an
// invented edge, a dropped node or a guessed ordering.
export function layoutDependencies<T extends DependencyNode>(nodes: T[]): Map<string, DependencyPosition> | null {
  const byId = new Map(nodes.map((n) => [n.id, { remaining: 0, children: [] as string[] }]));
  if (byId.size !== nodes.length) return null; // duplicate id
  const depth = new Map(nodes.map((n) => [n.id, 0]));
  for (const n of nodes) {
    const known = [...new Set(n.depends_on)].filter((p) => byId.has(p) && p !== n.id);
    byId.get(n.id)!.remaining = known.length;
    for (const parent of known) byId.get(parent)!.children.push(n.id);
  }
  const queue = nodes.filter((n) => byId.get(n.id)!.remaining === 0).map((n) => n.id);
  for (let i = 0; i < queue.length; i++) {
    const current = byId.get(queue[i])!;
    for (const child of current.children) {
      depth.set(child, Math.max(depth.get(child)!, depth.get(queue[i])! + 1));
      const next = byId.get(child)!;
      if (--next.remaining === 0) queue.push(child);
    }
  }
  if (queue.length !== nodes.length) return null; // cycle
  // Row order within a depth is assigned by id, not input array order, so
  // layout is identical across rerenders regardless of incidental fetch order.
  const ordered = [...nodes].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  const depths = [...new Set(ordered.map((n) => depth.get(n.id)!))].sort((a, b) => a - b);
  const rowAtDepth = new Map<number, number>();
  const result = new Map<string, DependencyPosition>();
  for (const n of ordered) {
    const d = depth.get(n.id)!,
      row = rowAtDepth.get(d) || 0;
    rowAtDepth.set(d, row + 1);
    result.set(n.id, { depth: d, x: depths.indexOf(d) * COLUMN_GAP + MARGIN, y: row * ROW_GAP + MARGIN });
  }
  return result;
}
export function dependencyGraphSize(layout: Map<string, DependencyPosition>) {
  const rows = new Map<number, number>();
  for (const { depth } of layout.values()) rows.set(depth, (rows.get(depth) || 0) + 1);
  return {
    width: Math.max(NODE_WIDTH + MARGIN * 2, (rows.size || 1) * COLUMN_GAP),
    height: Math.max(NODE_HEIGHT + MARGIN * 2, ...[...rows.values()].map((v) => v * ROW_GAP + MARGIN)),
  };
}
export function dependencyEdgePath(from: { x: number; y: number }, to: { x: number; y: number }) {
  const fy = from.y + NODE_HEIGHT / 2,
    ty = to.y + NODE_HEIGHT / 2;
  return `M${from.x + NODE_WIDTH},${fy} C${from.x + NODE_WIDTH + 25},${fy} ${to.x - 25},${ty} ${to.x},${ty}`;
}
