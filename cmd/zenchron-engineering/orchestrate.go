package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const orchestrateUsage = "usage: zenchron-engineering autonomy orchestrate " +
	"{issues <n> <n>... --agent <id> [--repo owner/name] [--text]|status <batch> [--text]} [--config <path>]"

// autonomyOrchestrate is basic explicit orchestration (#470): hand a running
// supervisor a bounded set of existing issues and one agent ONCE, and read the
// whole cohort back from one aggregate view.
func autonomyOrchestrate(args []string, stdout io.Writer) (int, error) {
	if len(args) < 2 {
		return runtime.ExitInvalid, errors.New(orchestrateUsage)
	}
	switch args[0] {
	case "issues":
		var issues []int
		rest := args[1:]
		for len(rest) > 0 {
			number, err := strconv.Atoi(rest[0])
			if err != nil {
				break
			}
			issues, rest = append(issues, number), rest[1:]
		}
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return orchestrateIssues(flags, issues, stdout)
	case "status":
		flags, err := parseAutonomyFlags(args[2:])
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return orchestrateStatus(flags, args[1], stdout)
	}
	return runtime.ExitInvalid, errors.New(orchestrateUsage)
}

func orchestrateIssues(flags autonomyFlags, issues []int, stdout io.Writer) (int, error) {
	// Refused here as well as in the supervisor, so an operator learns about a
	// malformed list before anything is sent.
	if _, err := orchestration.NormalizeIssues(issues); err != nil {
		return runtime.ExitInvalid, err
	}
	if flags.Agent == "" {
		return runtime.ExitInvalid, errors.New("orchestration names its execution agent explicitly: pass --agent <id>")
	}
	// Neither command takes the state directory's ownership lock: submitting
	// asks the supervisor that owns it, and status only reads, so both work
	// while `serve` runs - which is the only time submitting can.
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir := config.StateDir
	operator, operatorErr := config.ResolveOperator()
	repositories, repoErr := (&composition{config: config}).governedRepositories(flags)
	if err := errors.Join(operatorErr, repoErr); err != nil {
		return runtime.ExitInvalid, err
	}
	if !runtime.SupervisorRunning(stateDir) {
		return runtime.ExitInvalid, fmt.Errorf(
			"orchestration needs a supervisor to own the batch's runs; run `zenchron-engineering serve` first. State directory: %s", stateDir)
	}
	repository, err := batchTarget(repositories, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	delegated, payload, sent, err := delegatePayloadSent(stateDir, runtime.ControlRequest{
		Command: runtime.ControlOrchestrate, Repository: repository, Agent: flags.Agent,
		Issues: issues, Operator: operator.ID,
	})
	if !delegated {
		return runtime.ExitInvalid, fmt.Errorf("the supervisor on %s stopped while this request was being sent; run the command again", stateDir)
	}
	if err != nil {
		// The batch identity is a pure function of the request, so a lost
		// reply is answered from durable state rather than by guessing, and
		// re-sending finds the same batch instead of creating a second fleet.
		if id, idErr := orchestration.BatchID(repository, flags.Agent, issues); sent && idErr == nil && errors.Is(err, runtime.ErrControlReplyLost) {
			return runtime.ExitFailed, fmt.Errorf("%w; the batch, if it was created, is %s - read it with `autonomy orchestrate status %s`, and sending this request again is safe", err, id, id)
		}
		return runtime.ExitFailed, err
	}
	var view runtime.OrchestrationView
	if err := json.Unmarshal(payload, &view); err != nil {
		return runtime.ExitFailed, fmt.Errorf("the supervisor's answer is not an orchestration view: %w", err)
	}
	return renderOrchestration(flags, view, stdout)
}

func orchestrateStatus(flags autonomyFlags, batchID string, stdout io.Writer) (int, error) {
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return runtime.ExitFailed, err
	}
	defer store.Close()
	view, err := runtime.OrchestrationStatus(store, config.StateDir, batchID, time.Now().UTC())
	if err != nil {
		return runtime.ExitFailed, err
	}
	return renderOrchestration(flags, view, stdout)
}

func loadOrchestrationConfig(flags autonomyFlags) (runtime.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.Config{}, err
	}
	return runtime.LoadConfig(flags.Config, cwd)
}

func renderOrchestration(flags autonomyFlags, view runtime.OrchestrationView, stdout io.Writer) (int, error) {
	if !flags.Text {
		if err := writeJSON(stdout, view); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	c := view.Counts
	fmt.Fprintf(stdout, "ORCHESTRATION %s\nRepository: %s   Agent: %s\n", view.BatchID, view.Repository, view.AgentID)
	fmt.Fprintf(stdout, "Items: %d total: %d queued, %d running, %d waiting, %d handoff_pending, %d completed, %d failed, %d stopped, %d not_created, %d unknown\n\n",
		c.Total, c.Queued, c.Running, c.Waiting, c.HandoffPending, c.Completed, c.Failed, c.Stopped, c.NotCreated, c.Unknown)
	fmt.Fprintf(stdout, "%-7s %-16s %-38s %-9s %-24s %s\n", "ISSUE", "STATE", "RUN", "HANDOFF", "CANDIDATE / PR", "REASON")
	for _, item := range view.Items {
		location := orDash(shortRevision(item.CandidateRevision))
		if item.PullRequest > 0 {
			location = fmt.Sprintf("PR #%d %s", item.PullRequest, item.PRState)
		}
		state := string(item.State)
		if state == "" {
			state = "unknown"
		}
		fmt.Fprintf(stdout, "#%-6d %-16s %-38s %-9s %-24s %s\n", item.Issue, state, item.RunID, item.Handoff, location, item.Reason)
	}
	return runtime.ExitCompleted, nil
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
