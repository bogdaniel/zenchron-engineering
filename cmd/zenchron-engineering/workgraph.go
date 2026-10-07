package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const workgraphUsage = "usage: zenchron-engineering autonomy workgraph " +
	"{adopt <proposal.json> --agent <id> [--repo owner/name] [--text]|status <graph> [--text]} [--config <path>]"

// maxWorkGraphProposalBytes bounds the file this command will read at all. A
// proposal is untrusted input whether an operator or a model wrote it, and it
// has to fit inside one bounded control request line, so it is bounded well
// below that ceiling rather than refused at the socket.
const maxWorkGraphProposalBytes = 4 << 10

// autonomyWorkGraph is the WorkGraph operator surface (#472): adopt a proposed
// revision, or read one graph's units, dependencies and frontier.
func autonomyWorkGraph(args []string, stdout io.Writer) (int, error) {
	if len(args) < 2 {
		return runtime.ExitInvalid, errors.New(workgraphUsage)
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
	view, err := runtime.WorkGraphStatus(store, config.StateDir, graphID, time.Now().UTC())
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
	document, err := io.ReadAll(io.LimitReader(file, maxWorkGraphProposalBytes+1))
	if err != nil {
		return orchestration.WorkGraphProposal{}, err
	}
	if len(document) > maxWorkGraphProposalBytes {
		return orchestration.WorkGraphProposal{}, fmt.Errorf(
			"work graph proposal %s is above the %d byte bound one control request carries; a graph names at most %d units",
			path, maxWorkGraphProposalBytes, orchestration.MaxWorkGraphUnits)
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
