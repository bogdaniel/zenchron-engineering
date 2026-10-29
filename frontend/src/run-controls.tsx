import * as api from "./api";
import * as ops from "./operations-api";
import { GovernedAction, type ActionReview } from "./governed-action";
import { freshController } from "./planning";
export function RunControls({
  run,
  controller,
  stale,
  onSettled,
}: {
  run: ops.RunDetail;
  controller: api.Controller;
  stale: boolean;
  onSettled?: () => void;
}) {
  const terminal = ["completed", "failed", "cancelled"].includes(run.disposition),
    why = stale ? "Snapshot stale; refresh first" : api.blocker(controller);
  const prepare = async (command: string, note: string, signal: AbortSignal): Promise<ActionReview> => {
    const [current, live] = await Promise.all([
      ops.run(run.run_id, signal),
      freshController(signal, controller.controller_binding),
    ]);
    if (command !== "refresh" && ["completed", "failed", "cancelled"].includes(current.disposition))
      throw new Error("Run is terminal; no lifecycle action offered");
    return {
      title: `${command[0].toUpperCase() + command.slice(1)} this exact run?`,
      subject: `${current.repository} · ${current.run_id}`,
      consequence:
        command === "refresh"
          ? "Create a successor through the existing controller. Consumed budgets and policy ceilings remain authoritative; this grants no publication or merge authority."
          : command === "pause"
            ? "Record an operator pause. No privilege or budget expands; only the serving controller changes run behavior."
            : command === "unpause"
              ? "Remove the operator pause. The existing scheduler and policy still govern execution."
              : "Request continuation of this exact nonterminal run through the existing controller. Consumed budget is preserved.",
      details: [
        ["Run", current.run_id],
        ["Observed phase / disposition", `${current.phase} · ${current.disposition}`],
        ["Candidate", current.candidate?.revision || "Not recorded"],
        ["Contract", current.contract?.revision || "Not recorded"],
        ["Serving controller", live.controller_binding],
      ],
      path: `/v1/runs/${encodeURIComponent(run.run_id)}/${command}`,
      action: command,
      target: { run_id: run.run_id },
      body: {
        controller_binding: live.controller_binding,
        ...(["pause", "unpause"].includes(command) ? { note } : {}),
      },
      timeout: command === "resume" ? 1270000 : 75000,
      ...(["pause", "unpause"].includes(command)
        ? {
            readReceipt: async (signal: AbortSignal) => {
              const state = await ops.run(run.run_id, signal);
              return state.paused === (command === "pause")
                ? {
                    title: "Observed run control state",
                    detail: `${run.run_id} · ${state.paused ? "paused" : "unpaused"}`,
                  }
                : null;
            },
          }
        : {}),
    };
  };
  return (
    <div className="run-control-actions">
      {["pause", "unpause", "resume", "refresh"].map((command) => (
        <GovernedAction
          key={command}
          label={`Review ${command}`}
          recoveryKey={["resume", "refresh"].includes(command) ? `run/${run.run_id}/${command}` : undefined}
          reason={why || (terminal && command !== "refresh" ? `Run is ${run.disposition}` : "")}
          prepare={(note, signal) => prepare(command, note, signal)}
          noteAllowed={["pause", "unpause"].includes(command)}
          onSettled={onSettled}
        />
      ))}
    </div>
  );
}
