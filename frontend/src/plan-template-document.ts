export type DraftStage = { id: string; kind: string; role: string; profile: string; dependsOn: string[] };

export function emptyStage(): DraftStage {
  return { id: "", kind: "agent", role: "implementer", profile: "", dependsOn: [] };
}

// renameStageID changes one stage's id AND rewrites every OTHER stage's
// dependsOn entries pointing at the old id to the new one, so a rename
// preserves the dependency the operator actually set instead of letting it
// go dangling until templateDocumentFrom silently drops it at submit.
export function renameStageID(stages: DraftStage[], index: number, newID: string): DraftStage[] {
  const previousID = stages[index]?.id.trim();
  const renamed = stages.map((s, i) => (i === index ? { ...s, id: newID } : s));
  if (!previousID || newID.trim() === previousID) return renamed;
  return renamed.map((s, i) => (i === index ? s : { ...s, dependsOn: s.dependsOn.map((d) => (d === previousID ? newID.trim() : d)) }));
}

// dependentStageIDs names the stages that currently depend on stages[index],
// by id - what removing it would silently drop a dependency edge for.
export function dependentStageIDs(stages: DraftStage[], index: number): string[] {
  const id = stages[index]?.id.trim();
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
