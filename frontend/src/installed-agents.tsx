import { useEffect, useState } from "react";
import * as api from "./api";
import * as workspace from "./workspace-api";
import { Empty, Panel } from "./components";
import { TextField, SelectField, CheckboxField } from "./form";
import { GovernedAction, useReusableGovernedAction, type ActionReview } from "./governed-action";
import { freshController } from "./planning";
import { PlanTemplateCreate } from "./plan-template-create";

const CAPABILITIES = [
  "requirements_analysis",
  "architecture_reasoning",
  "repository_analysis",
  "code_change",
  "verification",
  "security_review",
];

// AgentProfileCreate proposes a new AgentProfile (ADR-0008): operator names
// it, picks an installed execution agent and a capability set, the
// controller validates and durably writes it. Instruction packs and context
// policy are offered as pickers over ALREADY INSTALLED ids (workspace.Options,
// not free text): a profile naming one that doesn't exist refuses the WHOLE
// reload (planning.Registry.validateReferences), so the only safe way to
// offer them here is to make typing a bad reference impossible.
function AgentProfileCreate({
  agents,
  instructionPacks,
  contextPolicies,
  controller,
  onSettled,
}: {
  agents: workspace.Options["agents"];
  instructionPacks: string[];
  contextPolicies: string[];
  controller: api.Controller | null;
  onSettled?: () => void;
}) {
  const [id, setID] = useState(""),
    [executionAgent, setExecutionAgent] = useState(agents[0]?.id || ""),
    [model, setModel] = useState(""),
    [capabilities, setCapabilities] = useState(""),
    [trustRequirement, setTrustRequirement] = useState("operator_trusted"),
    [selectedPacks, setSelectedPacks] = useState<string[]>([]),
    [contextPolicy, setContextPolicy] = useState("");
  const [instance, resetInstance] = useReusableGovernedAction();
  const parsedCapabilities = capabilities
    .split(",")
    .map((c) => c.trim())
    .filter(Boolean);
  const why = !controller
    ? "Controller not yet observed"
    : api.blocker(controller) ||
      (!id.trim()
        ? "Name the profile"
        : !executionAgent
          ? "Select an execution agent"
          : !parsedCapabilities.length
            ? "Name at least one capability"
            : "");
  const prepare = async (_note: string, signal: AbortSignal): Promise<ActionReview> => {
    const live = await freshController(signal, controller?.controller_binding);
    const document = {
      execution_agent: executionAgent,
      ...(model.trim() ? { model: model.trim() } : {}),
      capabilities: parsedCapabilities,
      ...(selectedPacks.length ? { instructions: selectedPacks } : {}),
      ...(contextPolicy ? { context_policy: contextPolicy } : {}),
      trust_requirement: trustRequirement,
    };
    return {
      title: "Create this agent profile?",
      subject: id.trim(),
      consequence:
        "Specializes an installed worker and advertises a capability set. Grants no execution trust, credential or permission bypass beyond what that worker already holds.",
      details: [
        ["Profile id", id.trim()],
        ["Execution agent", executionAgent],
        ["Capabilities", parsedCapabilities.join(", ")],
        ["Instruction packs", selectedPacks.join(", ") || "None"],
        ["Context policy", contextPolicy || "None"],
        ["Trust requirement", trustRequirement],
        ["Serving controller", live.controller_binding],
      ],
      path: "/v1/planning/artifacts",
      action: "planning-artifact-propose",
      target: {},
      body: { controller_binding: live.controller_binding, kind: "agent_profile", id: id.trim(), document },
    };
  };
  return (
    <Panel title="Create an agent profile">
      <div className="product-body intake-form">
        <TextField label="Profile id" value={id} onChange={(e) => setID(e.target.value)} />
        <SelectField label="Execution agent" value={executionAgent} onChange={(e) => setExecutionAgent(e.target.value)}>
          {agents.map((a) => (
            <option key={a.id} value={a.id}>
              {a.id} · {a.kind}
            </option>
          ))}
        </SelectField>
        <TextField
          label="Model"
          help="Optional. A preference over the agent's configured default, never a new provider."
          value={model}
          onChange={(e) => setModel(e.target.value)}
        />
        <TextField
          label="Capabilities"
          help={`Comma-separated: ${CAPABILITIES.join(", ")}`}
          value={capabilities}
          onChange={(e) => setCapabilities(e.target.value)}
        />
        <SelectField
          label="Trust requirement"
          value={trustRequirement}
          onChange={(e) => setTrustRequirement(e.target.value)}
        >
          <option value="operator_trusted">Operator trusted</option>
          <option value="protected">Protected</option>
        </SelectField>
        <fieldset className="field-label">
          <legend>Instruction packs</legend>
          {instructionPacks.length ? (
            instructionPacks.map((pack) => (
              <CheckboxField
                key={pack}
                label={pack}
                checked={selectedPacks.includes(pack)}
                onChange={(e) =>
                  setSelectedPacks((prev) => (e.target.checked ? [...prev, pack] : prev.filter((p) => p !== pack)))
                }
              />
            ))
          ) : (
            <p className="muted small">None installed.</p>
          )}
        </fieldset>
        <SelectField label="Context policy" value={contextPolicy} onChange={(e) => setContextPolicy(e.target.value)}>
          <option value="">None</option>
          {contextPolicies.map((p) => (
            <option key={p}>{p}</option>
          ))}
        </SelectField>
        <GovernedAction
          key={instance}
          label="Review & create"
          reason={why}
          prepare={prepare}
          noteAllowed={false}
          onSettled={() => {
            setID("");
            setModel("");
            setCapabilities("");
            setSelectedPacks([]);
            setContextPolicy("");
            resetInstance();
            onSettled?.();
          }}
        />
      </div>
    </Panel>
  );
}

// ReloadPlanningRegistry re-reads the planning directory (ADR-0008) so a
// just-written artifact is available to a new plan, without a restart.
// Approved or in-flight work is unaffected either way: a pinned pack or
// profile that no longer matches is refused at execution, exactly as it
// already is across an ordinary restart.
function ReloadPlanningRegistry({ controller, onSettled }: { controller: api.Controller | null; onSettled?: () => void }) {
  const [instance, resetInstance] = useReusableGovernedAction();
  const prepare = async (_note: string, signal: AbortSignal): Promise<ActionReview> => {
    const live = await freshController(signal, controller?.controller_binding);
    return {
      title: "Reload the planning registry?",
      subject: "Installed instruction packs, context policies, profiles and templates",
      consequence:
        "Re-reads the planning directory so a newly written or edited artifact is available to a new plan. Nothing already approved or executing is affected.",
      details: [["Serving controller", live.controller_binding]],
      path: "/v1/planning/reload",
      action: "planning-reload",
      target: {},
      body: { controller_binding: live.controller_binding },
    };
  };
  return (
    <GovernedAction
      key={instance}
      label="Reload registry"
      reason={controller ? api.blocker(controller) : "Controller not yet observed"}
      prepare={prepare}
      noteAllowed={false}
      onSettled={() => {
        resetInstance();
        onSettled?.();
      }}
    />
  );
}

export function InstalledAgents() {
  const [options, setOptions] = useState<workspace.Options | null>(null),
    [controller, setController] = useState<api.Controller | null>(null),
    [error, setError] = useState(""),
    [reload, setReload] = useState(0);
  useEffect(() => {
    const owner = new AbortController();
    // Stale data stays visible across a reload rather than being cleared
    // first: the create forms below are rendered only when options is
    // present, so clearing it here would unmount them - and the GovernedAction
    // success message they were just showing - on every post-create refresh.
    setError("");
    Promise.all([workspace.options(owner.signal), api.controller(owner.signal)])
      .then(([o, c]) => {
        setOptions(o);
        setController(c);
      })
      .catch((e) => {
        if (!owner.signal.aborted) setError(e instanceof Error ? e.message : "Options unavailable");
      });
    return () => owner.abort();
  }, [reload]);
  const refresh = () => setReload((v) => v + 1);
  return (
    <>
      <Panel
        title="Installed execution agents"
        extra={
          <button className="button" onClick={refresh}>
            Refresh installed configuration
          </button>
        }
      >
        <div className="product-body">
          <p>Controller-owned configuration. Eligibility grants no acceptance or publication authority.</p>
          {error ? (
            <p role="alert">Installed inventory unavailable · {error}</p>
          ) : !options ? (
            <p role="status">Reading installed agents…</p>
          ) : options.agents.length ? (
            <div className="table-scroll" tabIndex={0} role="region" aria-label="Installed agent inventory">
              <table className="data-table">
                <thead>
                  <tr>
                    {["Agent identity", "Provider / model", "Execution trust", "Eligibility"].map((h) => (
                      <th key={h} scope="col">
                        {h}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {options.agents.map((a) => (
                    <tr key={a.id}>
                      <td data-label="Agent identity">
                        {a.id}
                        {a.default ? " · default" : ""}
                      </td>
                      <td data-label="Provider / model">
                        {a.kind} / {a.model || "Default not reported"}
                      </td>
                      <td data-label="Execution trust">{a.trust_mode}</td>
                      <td data-label="Eligibility">{a.eligible ? "Eligible by configuration" : "Not eligible"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <Empty title="No installed agents reported">This is the current configuration response.</Empty>
          )}
        </div>
      </Panel>
      <Panel title="Agent profiles & plan templates" extra={<ReloadPlanningRegistry controller={controller} onSettled={refresh} />}>
        <div className="product-body">
          {options ? (
            <>
              <h3>Profiles</h3>
              <ul>
                {options.profiles.map((p) => (
                  <li className="mono" key={p}>
                    {p}
                  </li>
                ))}
              </ul>
              <h3>Templates</h3>
              <ul>
                {options.templates.map((t) => (
                  <li className="mono" key={t}>
                    {t}
                  </li>
                ))}
              </ul>
              <p>These specialize existing workers and planning inputs; they grant no authority.</p>
            </>
          ) : (
            <p>Configuration has not been observed.</p>
          )}
        </div>
      </Panel>
      {options && (
        <AgentProfileCreate
          agents={options.agents}
          instructionPacks={options.instruction_packs}
          contextPolicies={options.context_policies}
          controller={controller}
          onSettled={refresh}
        />
      )}
      {options && <PlanTemplateCreate profiles={options.profiles} controller={controller} onSettled={refresh} />}
    </>
  );
}
