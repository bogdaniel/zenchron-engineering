import { useId, useMemo } from "react";
import type { Stage } from "./api";
import { Empty, FilterBar } from "./components";
import {
  NODE_HEIGHT,
  NODE_WIDTH,
  dependencyEdgePath,
  dependencyGraphSize,
  layoutDependencies,
  resolvedDependencies,
  unresolvedDependencies,
} from "./dependency-layout";
export {
  NODE_HEIGHT,
  NODE_WIDTH,
  COLUMN_GAP,
  ROW_GAP,
  MARGIN,
  dependencyEdgePath,
  dependencyGraphSize,
  layoutDependencies,
  resolvedDependencies,
  unresolvedDependencies,
} from "./dependency-layout";
export type { DependencyNode, DependencyPosition } from "./dependency-layout";

// One <marker> per rendered graph (own id via useId) - a literal shared id
// would let a second graph on the same page silently reuse the first
// instance's <defs>, which is undefined per the SVG spec for duplicate ids.
export function useDependencyArrow() {
  const markerId = `dep-arrow-${useId()}`;
  const defs = (
    <marker id={markerId} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
      <path className="dependency-arrowhead" d="M0,0 L10,5 L0,10 z" />
    </marker>
  );
  return { markerId, defs };
}
// Replaces the plain .plan-dependency-grid card grid: real edges with
// arrows, deterministic left-to-right stage order, parallel branches side by
// side - derived only from each stage's own depends_on, never hardcoded.
export function PlanDependencyGraph({
  stages,
  selected,
  onSelect,
}: {
  stages: Stage[];
  selected: string;
  onSelect: (id: string) => void;
}) {
  const layout = useMemo(
    () => layoutDependencies(stages.map((s) => ({ id: s.stage_id, depends_on: s.depends_on }))),
    [stages],
  );
  const { markerId, defs } = useDependencyArrow();
  if (!stages.length) return <Empty title="No stages recorded">An empty plan has no dependency structure to show.</Empty>;
  const size = layout && dependencyGraphSize(layout);
  // Surfaced above the graph, not only on the one affected node: a stage
  // whose recorded depends_on cannot be fully resolved means this plan's
  // dependency order is not fully recorded as valid, which an operator
  // scanning past one flagged box among several could otherwise miss.
  const malformed = layout
    ? stages.filter((s) => unresolvedDependencies(s.stage_id, s.depends_on, (p) => layout.has(p)).length > 0)
    : [];
  const node = (s: Stage) => (
    <>
      <span className="eyebrow">{s.kind}</span>
      <strong>{s.stage_id}</strong>
      <span className="small muted">{s.state || "State not recorded"}</span>
      <span className="small">After: {s.depends_on.join(", ") || "No stage dependency"}</span>
    </>
  );
  return (
    <>
      {malformed.length > 0 && (
        <p className="banner red" role="alert">
          {malformed.length} stage{malformed.length > 1 ? "s" : ""} record a dependency not present in this plan
          ({malformed.map((s) => s.stage_id).join(", ")}). This plan's dependency order is not fully recorded as
          valid - inspect the flagged stage{malformed.length > 1 ? "s" : ""} below before treating it as runnable.
        </p>
      )}
      <FilterBar>
        <label htmlFor="stage-picker">Inspect stage</label>
        <select id="stage-picker" value={selected} onChange={(e) => onSelect(e.target.value)}>
          {stages.map((s) => (
            <option key={s.stage_id} value={s.stage_id}>
              {s.stage_id} · {s.kind}
            </option>
          ))}
        </select>
      </FilterBar>
      {layout && size ? (
        <div className="dependency-map" tabIndex={0} role="region" aria-label="Plan dependency graph; scroll to inspect">
          <svg width={size.width} height={size.height} role="group" aria-label="Plan stage dependency edges">
            <defs>{defs}</defs>
            {stages.flatMap((s) =>
              resolvedDependencies(s.stage_id, s.depends_on, (p) => layout.has(p)).map((p) => {
                const from = layout.get(p)!,
                  to = layout.get(s.stage_id)!;
                return (
                  <path
                    className="dependency-edge"
                    key={`${p}/${s.stage_id}`}
                    d={dependencyEdgePath(from, to)}
                    markerEnd={`url(#${markerId})`}
                  />
                );
              }),
            )}
            {stages.map((s) => {
              const p = layout.get(s.stage_id)!;
              const unresolved = unresolvedDependencies(s.stage_id, s.depends_on, (id) => layout.has(id));
              return (
                <g
                  key={s.stage_id}
                  className={`dependency-node ${selected === s.stage_id ? "selected" : ""} ${unresolved.length ? "unresolved" : ""}`}
                  transform={`translate(${p.x},${p.y})`}
                  role="button"
                  tabIndex={0}
                  aria-pressed={selected === s.stage_id}
                  aria-label={`${s.stage_id} · ${s.kind} · ${s.state || "state not recorded"} · after ${s.depends_on.join(", ") || "no stage dependency"}${unresolved.length ? ` · unresolved dependency reference: ${unresolved.join(", ")}` : ""}`}
                  onClick={() => onSelect(s.stage_id)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onSelect(s.stage_id);
                    }
                  }}
                >
                  <rect width={NODE_WIDTH} height={NODE_HEIGHT} rx="4" />
                  <text x="12" y="21" className="dependency-meta">
                    {s.kind}
                  </text>
                  <text x="12" y="44">
                    {s.stage_id.length > 26 ? `${s.stage_id.slice(0, 25)}…` : s.stage_id}
                  </text>
                  <text x="12" y="67" className="dependency-meta">
                    {unresolved.length
                      ? `Unresolved dependency: ${unresolved.join(", ")}`
                      : s.state || "State not recorded"}
                  </text>
                </g>
              );
            })}
          </svg>
        </div>
      ) : (
        <div className="product-body" role="alert">
          Dependency ordering unavailable: a cycle or duplicate stage id was observed. Inspect each stage's recorded
          dependencies below.
        </div>
      )}
      <div className={`dependency-mobile plan-dependency-grid ${layout ? "" : "dependency-fallback"}`}>
        {stages
          .filter((s) => s.stage_id === selected)
          .map((s) => (
            <button
              key={s.stage_id}
              className={`graph-node ${selected === s.stage_id ? "selected" : ""}`}
              aria-pressed={selected === s.stage_id}
              onClick={() => onSelect(s.stage_id)}
            >
              {node(s)}
            </button>
          ))}
      </div>
    </>
  );
}
