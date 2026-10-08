package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const workgraphUsage = "usage: zenchron-engineering autonomy workgraph " +
	"{adopt <proposal.json> --agent <id> [--repo owner/name] [--text]|status <graph> [--text]|" +
	"hold <graph> <unit> [--note <purpose>]|resolve <request-id> <outcome> [--note <reason>]} [--config <path>]"

// autonomyWorkGraph is the WorkGraph operator surface (#472, #508): adopt a
// proposed revision, read one graph's units, dependencies and frontier, place
// an operator's hold on a unit before its first activation, or resolve a live
// decision request - the hold's own, or a worker's #473 decision_request.
func autonomyWorkGraph(args []string, stdout io.Writer) (int, error) {
	// The subject - a graph id, a proposal path, or a hold's/decision's two
	// positionals - comes first; a flag there is a usage error, never read
	// as one. hold and resolve NAME TWO subjects, so their flags start one
	// position later than adopt's and status's single subject.
	if len(args) < 2 || strings.HasPrefix(args[1], "--") {
		return runtime.ExitInvalid, errors.New(workgraphUsage)
	}
	if args[0] == "hold" || args[0] == "resolve" {
		if len(args) < 3 || strings.HasPrefix(args[2], "--") {
			return runtime.ExitInvalid, errors.New(workgraphUsage)
		}
		flags, err := parseAutonomyFlags(args[3:])
		if err != nil {
			return runtime.ExitInvalid, err
		}
		if args[0] == "resolve" {
			return resolveDecision(flags, args[1], args[2], stdout)
		}
		return holdWorkUnit(flags, args[1], args[2], stdout)
	}
	flags, err := parseAutonomyFlags(args[2:])
	if err != nil {
		return runtime.ExitInvalid, err
	}
	switch args[0] {
	case "adopt":
		return adoptWorkGraph(flags, args[1], stdout)
	case "status":
		return workGraphStatus(flags, args[1], stdout)
	}
	return runtime.ExitInvalid, errors.New(workgraphUsage)
}

// holdWorkUnit is the governed action (#508) that places #472's
// readiness-owner seam on one not-yet-activated unit: an operator's gate, not
// a worker's, since a unit has no run to write one from before its first
// activation.
func holdWorkUnit(flags autonomyFlags, graphID, unitID string, stdout io.Writer) (int, error) {
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	operator, err := config.ResolveOperator()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	if !runtime.SupervisorRunning(config.StateDir) {
		return runtime.ExitInvalid, fmt.Errorf(
			"a work unit hold is placed against the supervisor that owns the graph; run `zenchron-engineering serve` first. State directory: %s", config.StateDir)
	}
	delegated, payload, sent, err := delegatePayloadSent(config.StateDir, runtime.ControlRequest{
		Command: runtime.ControlWorkGraphHold, GraphID: graphID, UnitID: unitID, Note: flags.Note, Operator: operator.ID,
	})
	if !delegated {
		return runtime.ExitInvalid, fmt.Errorf("the supervisor on %s stopped while this request was being sent; run the command again", config.StateDir)
	}
	if err != nil {
		// A hold's identity is deterministic from the graph and unit alone
		// (orchestration.WorkUnitHoldID), so a lost reply is safe to retry:
		// the identical request finds the hold already placed instead of
		// refusing it as a conflict.
		if id, idErr := orchestration.WorkUnitHoldID(graphID, unitID); sent && idErr == nil && errors.Is(err, runtime.ErrControlReplyLost) {
			return runtime.ExitFailed, fmt.Errorf("%w; the hold, if it was placed, is %s", err, id)
		}
		return runtime.ExitFailed, err
	}
	var hold orchestration.WorkUnitHold
	if err := json.Unmarshal(payload, &hold); err != nil {
		return runtime.ExitFailed, fmt.Errorf("the supervisor's answer is not a work unit hold: %w", err)
	}
	if err := writeJSON(stdout, hold); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}

// dispatchDecisionControl is the supervisor-side half of the two #508
// governed verbs: placing a hold, and resolving the live request it (or a
// #473 message) names. Kept out of serve.go's own dispatch switch, which
// every other verb already shares, so adding these two costs it one line.
func dispatchDecisionControl(supervisor *runtime.Supervisor, request runtime.ControlRequest) runtime.ControlResponse {
	if request.Command == runtime.ControlWorkGraphHold {
		hold, err := supervisor.PlaceWorkUnitHold(request)
		if err != nil {
			return controlError(err)
		}
		return controlOK(hold)
	}
	view, err := supervisor.ResolveDecision(request)
	if err != nil {
		return controlError(err)
	}
	return controlOK(view)
}

func adoptWorkGraph(flags autonomyFlags, path string, stdout io.Writer) (int, error) {
	if flags.Agent == "" {
		return runtime.ExitInvalid, errors.New("a work graph names its execution agent explicitly: pass --agent <id>")
	}
	proposal, err := readWorkGraphProposal(path)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	// Adopting needs the supervisor that owns the state directory: it is the
	// one writer that admits a revision against the activations it is
	// reconciling. Status only reads, so it works either way.
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	operator, operatorErr := config.ResolveOperator()
	repositories, repoErr := (&composition{config: config}).governedRepositories(flags)
	if err := errors.Join(operatorErr, repoErr); err != nil {
		return runtime.ExitInvalid, err
	}
	if !runtime.SupervisorRunning(config.StateDir) {
		return runtime.ExitInvalid, fmt.Errorf(
			"a work graph needs a supervisor to own its units' runs; run `zenchron-engineering serve` first. State directory: %s", config.StateDir)
	}
	repository, err := batchTarget(repositories, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	delegated, payload, sent, err := delegatePayloadSent(config.StateDir, runtime.ControlRequest{
		Command: runtime.ControlWorkGraph, Repository: repository, Agent: flags.Agent,
		WorkGraph: &proposal, Operator: operator.ID,
	})
	if !delegated {
		return runtime.ExitInvalid, fmt.Errorf("the supervisor on %s stopped while this request was being sent; run the command again", config.StateDir)
	}
	if err != nil {
		// The graph identity is a pure function of repository, agent and name,
		// and a revision's identity is its contents, so a lost reply is
		// answered from durable state rather than by guessing.
		if id, idErr := orchestration.WorkGraphID(repository, flags.Agent, proposal.Name); sent && idErr == nil &&
			errors.Is(err, runtime.ErrControlReplyLost) {
			return runtime.ExitFailed, fmt.Errorf("%w; the graph, if the revision was adopted, is %s - read it with `autonomy workgraph status %s`, and sending this request again is safe", err, id, id)
		}
		return runtime.ExitFailed, err
	}
	var view runtime.WorkGraphView
	if err := json.Unmarshal(payload, &view); err != nil {
		return runtime.ExitFailed, fmt.Errorf("the supervisor's answer is not a work graph view: %w", err)
	}
	return renderWorkGraph(flags, view, stdout)
}

func workGraphStatus(flags autonomyFlags, graphID string, stdout io.Writer) (int, error) {
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return runtime.ExitFailed, err
	}
	defer store.Close()
	// Holds (#508) are read the same way with or without a running
	// supervisor: the store, not an in-memory source, is their only owner.
	holds, err := store.WorkGraphHolds(graphID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	view, err := runtime.WorkGraphStatus(store, config.StateDir, graphID, time.Now().UTC(), holds)
	if err != nil {
		return runtime.ExitFailed, err
	}
	return renderWorkGraph(flags, view, stdout)
}

// readWorkGraphProposal decodes a bounded proposal document strictly. An unknown
// member is refused rather than ignored: a proposal that tried to state the
// repository, the agent or a run identity would otherwise look accepted while
// that part of it was silently dropped.
func readWorkGraphProposal(path string) (orchestration.WorkGraphProposal, error) {
	file, err := os.Open(path)
	if err != nil {
		return orchestration.WorkGraphProposal{}, err
	}
	defer file.Close()
	// ONE bound, the core's. A proposal this reader accepts is a proposal the
	// core accepts is a proposal one control request carries; there is no
	// second number here to drift from that one.
	document, err := io.ReadAll(io.LimitReader(file, orchestration.MaxWorkGraphDocumentBytes+1))
	if err != nil {
		return orchestration.WorkGraphProposal{}, err
	}
	if len(document) > orchestration.MaxWorkGraphDocumentBytes {
		return orchestration.WorkGraphProposal{}, fmt.Errorf(
			"work graph proposal %s is above the %d byte bound one control request carries",
			path, orchestration.MaxWorkGraphDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var proposal orchestration.WorkGraphProposal
	if err := decoder.Decode(&proposal); err != nil {
		return orchestration.WorkGraphProposal{}, fmt.Errorf("work graph proposal %s is not a valid document: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return orchestration.WorkGraphProposal{}, fmt.Errorf("work graph proposal %s must contain exactly one JSON value", path)
	}
	return proposal, nil
}

func renderWorkGraph(flags autonomyFlags, view runtime.WorkGraphView, stdout io.Writer) (int, error) {
	if !flags.Text {
		if err := writeJSON(stdout, view); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	c := view.Counts
	fmt.Fprintf(stdout, "WORK GRAPH %s (%s) revision %d\nRepository: %s   Agent: %s\n",
		view.Name, view.GraphID, view.Revision, view.Repository, view.AgentID)
	fmt.Fprintf(stdout, "Units: %d total: %d blocked, %d ready, %d invalidated, %d unknown; activated: %d running, %d waiting, %d queued, %d handoff_pending, %d partial, %d completed, %d failed, %d stopped, %d not_created\n",
		c.Total, c.Blocked, c.Ready, c.Invalidated, c.Unknown,
		c.Activated.Running, c.Activated.Waiting, c.Activated.Queued, c.Activated.HandoffPending,
		c.Activated.Partial, c.Activated.Completed, c.Activated.Failed, c.Activated.Stopped, c.Activated.NotCreated)
	if len(view.Frontier) > 0 {
		fmt.Fprintf(stdout, "Runnable now: %v (the scheduler decides when each executes)\n", view.Frontier)
	}
	fmt.Fprintf(stdout, "\n%-16s %-7s %-16s %-16s %-38s %s\n", "UNIT", "ISSUE", "STATE", "DEPENDS ON", "RUN", "REASON")
	for _, unit := range view.Units {
		depends := "-"
		if len(unit.DependsOn) > 0 {
			depends = fmt.Sprint(unit.DependsOn)
		}
		reason := unit.Reason
		if unit.Child != nil && unit.Child.HandoffReason != "" && unit.Child.HandoffReason != reason {
			reason = fmt.Sprintf("%s; handoff: %s", reason, unit.Child.HandoffReason)
		}
		if unit.Output != nil {
			reason = fmt.Sprintf("output %s %s", unit.Output.CandidateRevision[:min(12, len(unit.Output.CandidateRevision))], reason)
		}
		fmt.Fprintf(stdout, "%-16s #%-6d %-16s %-16s %-38s %s\n",
			unit.UnitID, unit.Issue, unit.State, depends, orDash(unit.RunID), reason)
	}
	return runtime.ExitCompleted, nil
}
