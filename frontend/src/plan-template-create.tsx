import { useState } from "react";
import * as api from "./api";
import { Panel } from "./components";
import { TextField, SelectField, CheckboxField } from "./form";
import { GovernedAction, useReusableGovernedAction, type ActionReview } from "./governed-action";
import { freshController } from "./planning";
import {
  type DraftStage,
  dependencyEdges,
  dependentStageIDs,
  emptyStage,
  renameStageID,
  templateDocumentFrom,
} from "./plan-template-document";

const KINDS = ["agent", "assurance_gate", "human_decision_gate"];
const ROLES = [
  "product_architect",
  "system_architect",
  "planner",
  "implementer",
  "tester",
  "security_reviewer",
  "reviewer",
  "integrator",
  "release_reviewer",
];

// PlanTemplateCreate proposes a new EngineeringPlanTemplate (ADR-0008): a
// structured stage builder, not a canvas. A dependency may only name an
// EARLIER stage in this list - there is no reordering - which keeps the
// template's own listed order a valid topological order without a second
// cycle check here; the server's seal/schema validation remains the one
// authority on whether the whole document is well-formed.
export function PlanTemplateCreate({
  profiles,
  controller,
  onSettled,
}: {
  profiles: string[];
  controller: api.Controller | null;
  onSettled?: () => void;
}) {
  const [id, setID] = useState(""),
    [description, setDescription] = useState(""),
    [stages, setStages] = useState<DraftStage[]>([emptyStage()]);
  const [instance, resetInstance] = useReusableGovernedAction();
  const updateStage = (index: number, patch: Partial<DraftStage>) =>
    setStages((prev) => prev.map((s, i) => (i === index ? { ...s, ...patch } : s)));
  const renameStage = (index: number, newID: string) => setStages((prev) => renameStageID(prev, index, newID));
  const addStage = () => setStages((prev) => [...prev, emptyStage()]);
  const removeStage = (index: number) => {
    const dependents = dependentStageIDs(stages, index);
    if (
      dependents.length &&
      !window.confirm(
        `Removing this stage will drop the dependency ${dependents.length > 1 ? "edges" : "edge"} ${dependents.join(", ")} ${dependents.length > 1 ? "have" : "has"} on it. Continue?`,
      )
    ) {
      return;
    }
    setStages((prev) => prev.filter((_, i) => i !== index));
  };
  const trimmedIDs = stages.map((s) => s.id.trim());
  const duplicateIDs = trimmedIDs.filter(Boolean).length !== new Set(trimmedIDs.filter(Boolean)).size;
  const incompleteStage = stages.some((s) => !s.id.trim());
  const why = !controller
    ? "Controller not yet observed"
    : api.blocker(controller) ||
      (!id.trim()
        ? "Name the template"
        : !stages.length
          ? "Add at least one stage"
          : incompleteStage
            ? "Every stage needs an id"
            : duplicateIDs
              ? "Stage ids must be unique"
              : "");
  const prepare = async (_note: string, signal: AbortSignal): Promise<ActionReview> => {
    const live = await freshController(signal, controller?.controller_binding);
    const { id: templateID, document } = templateDocumentFrom(id, description, stages);
    const edges = dependencyEdges(stages);
    return {
      title: "Create this plan template?",
      subject: templateID,
      consequence:
        "A reusable planning input an objective proposal may select. It grants no authority, removes no policy obligation and widens no budget beyond what policy already allows.",
      details: [
        ["Template id", templateID],
        // List order is NOT execution dependency - showing it as a single
        // arrow-joined chain would claim an edge between every adjacent pair
        // whether or not one exists. Each stage's row names only its real
        // depends_on, exactly what templateDocumentFrom is about to submit.
        ...(edges.length
          ? edges.map(({ id: stageID, dependsOn }): [string, string] => [
              `Stage: ${stageID}`,
              dependsOn.length ? `depends on ${dependsOn.join(", ")}` : "no dependencies",
            ])
          : ([["Stages", "none"]] as [string, string][])),
        ["Serving controller", live.controller_binding],
      ],
      path: "/v1/planning/artifacts",
      action: "planning-artifact-propose",
      target: {},
      body: { controller_binding: live.controller_binding, kind: "plan_template", id: templateID, document },
    };
  };
  return (
    <Panel title="Create a plan template">
      <div className="product-body intake-form">
        <TextField label="Template id" value={id} onChange={(e) => setID(e.target.value)} />
        <TextField
          label="Description"
          help="Optional."
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
        {stages.map((stage, index) => (
          <fieldset key={index} className="field-label">
            <legend>Stage {index + 1}</legend>
            <TextField label="Stage id" value={stage.id} onChange={(e) => renameStage(index, e.target.value)} />
            <SelectField label="Kind" value={stage.kind} onChange={(e) => updateStage(index, { kind: e.target.value })}>
              {KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </SelectField>
            {stage.kind === "agent" && (
              <>
                <SelectField label="Role" value={stage.role} onChange={(e) => updateStage(index, { role: e.target.value })}>
                  {ROLES.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </SelectField>
                <SelectField
                  label="Profile"
                  value={stage.profile}
                  onChange={(e) => updateStage(index, { profile: e.target.value })}
                >
                  <option value="">Resolve from policy and plan</option>
                  {profiles.map((p) => (
                    <option key={p}>{p}</option>
                  ))}
                </SelectField>
              </>
            )}
            {index > 0 && (
              <fieldset className="field-label">
                <legend>Depends on</legend>
                {stages.slice(0, index).map((earlier, earlierIndex) => (
                  <CheckboxField
                    key={earlierIndex}
                    label={earlier.id.trim() || `Stage ${earlierIndex + 1} (name it first)`}
                    disabled={!earlier.id.trim()}
                    checked={stage.dependsOn.includes(earlier.id.trim())}
                    onChange={(e) => {
                      const earlierID = earlier.id.trim();
                      const dependsOn = e.target.checked
                        ? [...stage.dependsOn, earlierID]
                        : stage.dependsOn.filter((d) => d !== earlierID);
                      updateStage(index, { dependsOn });
                    }}
                  />
                ))}
              </fieldset>
            )}
            {stages.length > 1 && (
              <button type="button" className="button" onClick={() => removeStage(index)}>
                Remove this stage
              </button>
            )}
          </fieldset>
        ))}
        <button type="button" className="button" onClick={addStage}>
          Add stage
        </button>
        <GovernedAction
          key={instance}
          label="Review & create"
          reason={why}
          prepare={prepare}
          noteAllowed={false}
          onSettled={() => {
            setID("");
            setDescription("");
            setStages([emptyStage()]);
            resetInstance();
            onSettled?.();
          }}
        />
      </div>
    </Panel>
  );
}
