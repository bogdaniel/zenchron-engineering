export type DraftStage = { id: string; kind: string; role: string; profile: string; dependsOn: string[] };

export function emptyStage(): DraftStage {
  return { id: "", kind: "agent", role: "implementer", profile: "", dependsOn: [] };
}

// templateDocumentFrom builds the submitted document from draft stages: ids
// are trimmed, a dependency naming a stage that was renamed or removed since
// it was checked is dropped rather than submitted dangling, and only an
// "agent" stage carries role/profile.
export function templateDocumentFrom(id: string, description: string, stages: DraftStage[]) {
  const validIDs = new Set(stages.map((s) => s.id.trim()).filter(Boolean));
  return {
    id: id.trim(),
    document: {
      ...(description.trim() ? { description: description.trim() } : {}),
      stages: stages.map((s) => ({
        id: s.id.trim(),
        kind: s.kind,
        ...(s.kind === "agent" && s.role ? { role: s.role } : {}),
        ...(s.kind === "agent" && s.profile ? { profile: s.profile } : {}),
        ...(s.dependsOn.filter((d) => validIDs.has(d)).length
          ? { depends_on: s.dependsOn.filter((d) => validIDs.has(d)) }
          : {}),
      })),
    },
  };
}
