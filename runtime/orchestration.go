package runtime

// Basic explicit orchestration (#470): an operator names a bounded set of
// existing issues and one execution agent ONCE, and the supervisor creates one
// ordinary EngineeringRun per issue and keeps the whole cohort readable from
// one place.
//
// What this file deliberately does NOT do is the point of it. It schedules
// nothing - every child is an ordinary run, driven by the existing tick loop
// and leased by the existing scheduler under the existing capacity ceilings,
// so a batch can never raise concurrency. It owns no attempt, retry, lease,
// wait, candidate, evidence or authority: each child keeps all of those. And
// it decides no item state into storage: the batch document is written once
// and every item's state is projected from its child on each read.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// OrchestrationConflictError refuses a batch that would put a second live run
// on an issue that already has one. Orchestration never adopts, supersedes or
// races work it did not create.
type OrchestrationConflictError struct {
	Issue int
	RunID string
	// Batch names another batch that already owns RunID but whose creation of
	// it has not landed yet, when that is the conflict.
	Batch string
}

func (e *OrchestrationConflictError) Error() string {
	if e.Batch != "" {
		return fmt.Sprintf("issue %d is already an item of orchestration batch %s (run %s, not yet created); a run identity belongs to one batch", e.Issue, e.Batch, e.RunID)
	}
	return fmt.Sprintf("issue %d already has live run %s; an orchestration batch does not adopt or race work it did not create - let that run finish or stop it, then submit the batch again", e.Issue, e.RunID)
}

// Orchestrate is the explicit intake for a batch: one durable batch, one
// ordinary child run per named issue, all through the same admission gate,
// agent registry, readiness probe and engine a single Submit uses.
//
// It is idempotent. The batch identity is a pure function of the request, so a
// request whose reply was lost finds the batch it already wrote, and creates
// only the children whose creation had not yet landed.
func (s *Supervisor) Orchestrate(ctx context.Context, request ControlRequest) (OrchestrationView, error) {
	repository, governed := s.governedRepository(request.Repository)
	if !governed {
		return OrchestrationView{}, fmt.Errorf("repository %q is not governed by this supervisor; enrolment is operator configuration, not a request", request.Repository)
	}
	issues, err := orchestration.NormalizeIssues(request.Issues)
	if err != nil {
		return OrchestrationView{}, err
	}
	// The agent is EXPLICIT. A batch spends one agent's capacity on every
	// issue it names, and falling back to whichever agent is the default
	// would be a hidden choice of who does that work.
	if strings.TrimSpace(request.Agent) == "" {
		return OrchestrationView{}, errors.New("an orchestration batch names its execution agent explicitly")
	}
	agent, err := s.deps.Agents.Agent(request.Agent)
	if err != nil {
		return OrchestrationView{}, err
	}
	if s.deps.AgentProber != nil {
		if err := RefuseUnlessInvocable(ctx, agent, s.deps.AgentProber(agent)); err != nil {
			return OrchestrationView{}, err
		}
	}
	engine, err := s.engine(repository.String(), agent.ID)
	if err != nil {
		return OrchestrationView{}, err
	}
	// The handoff is MANDATORY, so the ability to write it is a precondition
	// of the work, not a hope expressed in a prompt.
	if !writesTypedResults(engine.deps.Provider) {
		return OrchestrationView{}, fmt.Errorf("agent %q cannot write the runtime-owned typed result directory, so it can never satisfy an orchestration batch's mandatory handoff", agent.ID)
	}
	id, err := orchestration.BatchID(repository.String(), agent.ID, issues)
	if err != nil {
		return OrchestrationView{}, err
	}
	var batch orchestration.Batch
	var created map[int]error
	err = s.admission.admit(func() error {
		// Serialized so two batches naming the same issue cannot both decide
		// they own its free run identity.
		s.orchestrationMu.Lock()
		defer s.orchestrationMu.Unlock()
		stored, found, err := s.deps.Store.OrchestrationBatch(id)
		if err != nil {
			return err
		}
		if !found {
			reserved, err := reservedRunIDs(s.deps.Store)
			if err != nil {
				return err
			}
			planned, err := engine.planOrchestrationBatch(id, issues, request.Operator, reserved)
			if err != nil {
				return err
			}
			if stored, _, err = s.deps.Store.CreateOrchestrationBatch(planned); err != nil {
				return err
			}
		}
		batch = stored
		created = engine.materializeBatch(ctx, batch)
		return nil
	})
	if err != nil {
		return OrchestrationView{}, err
	}
	view, err := OrchestrationStatus(s.deps.Store, s.deps.StateDir, batch.ID, s.deps.Clock.Now())
	if err != nil {
		return OrchestrationView{}, err
	}
	for i := range view.Items {
		if itemErr := created[view.Items[i].Issue]; itemErr != nil {
			view.Items[i].Reason = boundedDetail("child run creation did not complete and is retried by the supervisor: " + itemErr.Error())
		}
	}
	return view, nil
}

// reconcileOrchestration is the supervisor pass's batch step: it creates any
// child whose creation did not land (a crash or a lost reply between writing
// the batch and creating its runs), and admits every handoff that can now be
// bound. It returns one bounded line per problem; a problem with one batch or
// item never stops the others.
//
// ponytail: replays each batch child's journal every pass; fine for a few
// batches of tens of items, skip items whose latest handoff is already
// admitted if that ever shows up in a profile.
func (s *Supervisor) reconcileOrchestration(ctx context.Context) []string {
	batches, err := s.deps.Store.OrchestrationBatches()
	if err != nil {
		return []string{boundedDetail(err.Error())}
	}
	var problems []string
	now := s.deps.Clock.Now()
	for _, batch := range batches {
		// CREATING a child needs the engine: it is new work under the named
		// agent. FINALIZING a handoff does not - it reads only the batch, the
		// run's journal and the state directory - so an agent retired or a
		// repository unconfigured after its worker reported still has that
		// report settled, and no execution authority is needed or granted.
		if engine, err := s.engine(batch.Repository, batch.AgentID); err != nil {
			problems = append(problems, boundedDetail(batch.ID+": "+err.Error()))
		} else {
			for issue, itemErr := range engine.materializeBatch(ctx, batch) {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s issue %d: %v", batch.ID, issue, itemErr)))
			}
		}
		for _, item := range batch.Items {
			if err := admitOrchestratedHandoff(s.deps.Store, s.deps.StateDir, batch, item, now); err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s issue %d: %v", batch.ID, item.Issue, err)))
			}
		}
		// Messages after handoffs: a Finding names an admitted handoff.
		if err := admitOrchestratedMessages(s.deps.Store, s.deps.StateDir, batch, now); err != nil {
			problems = append(problems, boundedDetail(batch.ID+" messages: "+err.Error()))
		}
	}
	return problems
}

// planOrchestrationBatch decides every item's child run identity BEFORE the
// batch is written, and refuses the whole batch - writing nothing - when any
// issue already has a live run. Deciding identities first is what makes the
// batch document immutable and recovery exact: a restart creates precisely the
// runs the batch names, never re-derives them under a configuration that may
// have changed since.
//
// reserved names every run identity an existing batch already owns. A batch
// whose child creation has not landed yet holds an identity no run row shows,
// and a second batch must not decide the same one.
func (r *EngineeringRuntime) planOrchestrationBatch(id string, issues []int, requestedBy string, reserved map[string]string) (orchestration.Batch, error) {
	live, err := r.deps.Store.ActiveRuns()
	if err != nil {
		return orchestration.Batch{}, err
	}
	liveByGoal := map[string]string{}
	for _, run := range live {
		if !terminalDisposition(run.Disposition) {
			liveByGoal[run.Goal] = run.ID
		}
	}
	batch := orchestration.Batch{
		SchemaVersion: orchestration.BatchSchemaVersion, ID: id,
		Repository: r.deps.Repository.Identity, AgentID: r.deps.Agent.ID,
		RequestedBy: BoundedNote(requestedBy), CreatedAt: r.deps.Clock.Now(),
	}
	for _, issue := range issues {
		goal := issueGoal(r.deps.Repository.Identity, issue)
		if runID, ok := liveByGoal[goal]; ok {
			return orchestration.Batch{}, &OrchestrationConflictError{Issue: issue, RunID: runID}
		}
		runID, err := r.freeIssueRunID(issue, goal)
		if err != nil {
			return orchestration.Batch{}, err
		}
		if owner, taken := reserved[runID]; taken {
			return orchestration.Batch{}, &OrchestrationConflictError{Issue: issue, RunID: runID, Batch: owner}
		}
		batch.Items = append(batch.Items, orchestration.BatchItem{Issue: issue, RunID: runID})
	}
	return batch, nil
}

// reservedRunIDs maps every run identity a stored batch names to that batch.
func reservedRunIDs(store *SQLiteOperationStore) (map[string]string, error) {
	batches, err := store.OrchestrationBatches()
	if err != nil {
		return nil, err
	}
	reserved := map[string]string{}
	for _, batch := range batches {
		for _, item := range batch.Items {
			reserved[item.RunID] = batch.ID
		}
	}
	return reserved, nil
}

// freeIssueRunID is the first unused identity in the issue's ordinary
// generation space - the same space StartIssueRun probes, so an orchestrated
// child is an ordinary run that a later `run issue` adopts rather than races.
func (r *EngineeringRuntime) freeIssueRunID(issue int, goal string) (string, error) {
	for generation := 0; generation < maxRunGenerations; generation++ {
		runID, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return "", err
		}
		existing, found, err := r.deps.Store.Run(runID)
		if err != nil {
			return "", err
		}
		if !found {
			return runID, nil
		}
		if existing.Repository != r.deps.Repository.Identity || existing.Goal != goal {
			return "", &RunConflictError{RunID: runID, Detail: "durable run describes different work"}
		}
		if !terminalDisposition(existing.Disposition) {
			return "", &OrchestrationConflictError{Issue: issue, RunID: runID}
		}
	}
	return "", fmt.Errorf("issue %d has exhausted %d run generations", issue, maxRunGenerations)
}

// materializeBatch makes sure every item's child run exists, bound to this
// batch, and returns the items it could not settle. It is idempotent: an
// existing child is verified, never recreated.
func (r *EngineeringRuntime) materializeBatch(ctx context.Context, batch orchestration.Batch) map[int]error {
	failed := map[int]error{}
	for _, item := range batch.Items {
		if err := r.materializeOrchestratedRun(ctx, batch.ID, item); err != nil {
			failed[item.Issue] = err
		}
	}
	return failed
}

func (r *EngineeringRuntime) materializeOrchestratedRun(ctx context.Context, batchID string, item orchestration.BatchItem) error {
	goal := issueGoal(r.deps.Repository.Identity, item.Issue)
	_, found, err := r.deps.Store.Run(item.RunID)
	if err != nil {
		return err
	}
	if !found {
		// A lost claim returns without error, so the row is verified below
		// whichever process created it.
		if _, err := r.createRun(ctx, item.RunID, goal, nil, &RunOrchestrationBinding{BatchID: batchID}, domain.StageBudget{}, r.deps.Store.ClaimRun); err != nil {
			return err
		}
	}
	existing, found, err := r.deps.Store.Run(item.RunID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return &RunConflictError{RunID: item.RunID, Detail: "the child run was not created"}
	case existing.Repository != r.deps.Repository.Identity || existing.Goal != goal:
		return &RunConflictError{RunID: item.RunID, Detail: "durable run describes different work"}
	case existing.Orchestration == nil || existing.Orchestration.BatchID != batchID:
		return &RunConflictError{RunID: item.RunID, Detail: "durable run was not created by orchestration batch " + batchID}
	case existing.AgentID != r.deps.Agent.ID:
		return &RunConflictError{RunID: item.RunID, Detail: "durable run is bound to agent " + existing.AgentID}
	}
	// Driving the child is the tick loop's job, under the controller checks
	// every run gets there; this only closes the claimed-but-not-yet-assigned
	// window createRun documents, exactly as StartIssueRun does.
	return r.repairAgentBinding(item.RunID, existing)
}
