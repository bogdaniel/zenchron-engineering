// anchorID is the stable identity renameStageID tracks a stage's
// dependents by, separate from the live, possibly-empty text in id: an
// operator retyping a name (select-all, backspace, type the new one) sends
// one onChange per keystroke, including an empty intermediate value, and
// that empty value must never become what dependents are redirected to.
export type DraftStage = { id: string; kind: string; role: string; profile: string; dependsOn: string[]; anchorID?: string };

export function emptyStage(): DraftStage {
  return { id: "", kind: "agent", role: "implementer", profile: "", dependsOn: [] };
}

// renameStageID changes one stage's id AND rewrites every OTHER stage's
// dependsOn entries pointing at its stable anchorID to the new one, so a
// rename preserves the dependency the operator actually set instead of
// letting it go dangling until templateDocumentFrom silently drops it at
// submit - INCLUDING across an empty intermediate value (a -> "" -> a2):
// anchorID holds "a" across the empty step, where id itself does not.
export function renameStageID(stages: DraftStage[], index: number, newID: string): DraftStage[] {
  const current = stages[index];
  if (!current) return stages;
  const anchor = current.anchorID ?? current.id.trim();
  const trimmedNewID = newID.trim();
  const renamed = stages.map((s, i) =>
    i === index ? { ...s, id: newID, anchorID: trimmedNewID || anchor } : s,
  );
  if (!anchor || !trimmedNewID || trimmedNewID === anchor) return renamed;
  return renamed.map((s, i) => (i === index ? s : { ...s, dependsOn: s.dependsOn.map((d) => (d === anchor ? trimmedNewID : d)) }));
}

// dependentStageIDs names the stages that currently depend on stages[index],
// by id - what removing it would silently drop a dependency edge for. A
// cleared-then-not-yet-retyped id (the empty intermediate value renameStageID
// tracks through) still owns its dependents under its stable anchorID, so
// this falls back to that - otherwise clearing a name right before clicking
// remove would read as having no dependents at all.
export function dependentStageIDs(stages: DraftStage[], index: number): string[] {
  const current = stages[index];
  const id = current?.id.trim() || current?.anchorID;
  if (!id) return [];
  return stages.filter((s, i) => i !== index && s.dependsOn.includes(id)).map((s) => s.id.trim() || "(unnamed)");
}

// dependencyEdges is the one place "what does this stage actually depend
// on" is computed: a trimmed id, and dependsOn narrowed to ids that still
// name a CURRENT stage (a rename or removal since a checkbox was ticked
// drops the stale reference here too, same as templateDocumentFrom - this
// is the exact input it reuses, so confirmation display and submitted
// document can never disagree about which edges are real).
export function dependencyEdges(stages: DraftStage[]): { id: string; dependsOn: string[] }[] {
  const validIDs = new Set(stages.map((s) => s.id.trim()).filter(Boolean));
  return stages
    .filter((s) => s.id.trim())
    .map((s) => ({ id: s.id.trim(), dependsOn: s.dependsOn.filter((d) => validIDs.has(d)) }));
}

// templateDocumentFrom builds the submitted document from draft stages: ids
// are trimmed, a dependency naming a stage that was renamed or removed since
// it was checked is dropped rather than submitted dangling, and only an
// "agent" stage carries role/profile.
export function templateDocumentFrom(id: string, description: string, stages: DraftStage[]) {
  const edges = new Map(dependencyEdges(stages).map((e) => [e.id, e.dependsOn]));
  return {
    id: id.trim(),
    document: {
      ...(description.trim() ? { description: description.trim() } : {}),
      stages: stages.map((s) => {
        const dependsOn = edges.get(s.id.trim()) ?? [];
        return {
          id: s.id.trim(),
          kind: s.kind,
          ...(s.kind === "agent" && s.role ? { role: s.role } : {}),
          ...(s.kind === "agent" && s.profile ? { profile: s.profile } : {}),
          ...(dependsOn.length ? { depends_on: dependsOn } : {}),
        };
      }),
    },
  };
}
