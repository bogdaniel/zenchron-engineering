package runtime

// Source history across configuration identity spaces (#58).
//
// A run's identity hashes its configuration digest, which is right: the
// configuration is part of the authority a run executes under. But a SOURCE -
// one issue of one repository - outlives any one configuration. Deciding
// whether a source is new by probing only the current digest's identity space
// read every configuration change as "this issue was never seen", so a
// standing opt-in label re-enrolled finished work and an explicit start
// silently ran beside live work created under the previous configuration.
//
// This file is the one place that question is answered, from every run the
// store holds for the source, whatever configuration created it.

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// SourceRuns returns every run ever created for one source goal in one
// repository, under any configuration, generation, plan stage or batch.
//
// ponytail: filters on the JSON goal across the repository's runs; fine for
// thousands of runs, add an expression index on the goal if that grows.
func (s *SQLiteOperationStore) SourceRuns(repository, goal string) ([]EngineeringRun, error) {
	return s.queryRuns(` WHERE repository = ? AND json_extract(document, '$.goal') = ?`, repository, goal)
}

// ClaimUnseenSourceRun is ClaimRun conditioned on the source having NO run at
// all, decided in the same statement. It is what keeps two discovery passes -
// even under different configurations, which derive different run identities
// and so never collide on ClaimRun's key - from both deciding a source is new
// and both creating work for it.
func (s *SQLiteOperationStore) ClaimUnseenSourceRun(run EngineeringRun) (bool, error) {
	if run.ID == "" || run.Goal == "" {
		return false, fmt.Errorf("a source claim needs the run id and its source goal")
	}
	document, err := CanonicalJSON(run)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO runs (`+sqliteRunColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE repository = ? AND json_extract(document, '$.goal') = ?)
		ON CONFLICT(id) DO NOTHING`,
		run.ID, run.Repository, run.Base.ID, run.Base.Revision, run.Contract.ID, run.Contract.Revision,
		run.Candidate.Branch, run.Candidate.Revision, run.Candidate.Tree, run.ControllerSHA256,
		run.CreatedAt.UnixNano(), string(document), run.Repository, run.Goal)
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	return claimed == 1, err
}

// ClaimRunUnlessSourceLive is ClaimRun conditioned on the source having no
// LIVE run, decided in the same statement. It is the ordinary start's claim:
// finished history does not stop an explicit start, but two starts under
// different configurations - different run identities, so ClaimRun alone
// cannot collide them - must not both create live work for one source.
//
// Liveness here is the run row's disposition. A row the journal has since
// settled reads live until it is projected, which only ever refuses a start
// that could have proceeded, never admits one that should not.
func (s *SQLiteOperationStore) ClaimRunUnlessSourceLive(run EngineeringRun) (bool, error) {
	if run.ID == "" || run.Goal == "" {
		return false, fmt.Errorf("a source claim needs the run id and its source goal")
	}
	document, err := CanonicalJSON(run)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO runs (`+sqliteRunColumns+`)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM runs WHERE repository = ? AND json_extract(document, '$.goal') = ?
			AND COALESCE(json_extract(document, '$.disposition'), '') NOT IN (?, ?, ?))
		ON CONFLICT(id) DO NOTHING`,
		run.ID, run.Repository, run.Base.ID, run.Base.Revision, run.Contract.ID, run.Contract.Revision,
		run.Candidate.Branch, run.Candidate.Revision, run.Candidate.Tree, run.ControllerSHA256,
		run.CreatedAt.UnixNano(), string(document), run.Repository, run.Goal,
		string(Completed), string(Failed), string(Cancelled))
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	return claimed == 1, err
}

// SourceState is what a source's whole durable history says about it.
type SourceState string

const (
	// SourceUnseen: no run was ever created for this source.
	SourceUnseen SourceState = "unseen"
	// SourceLive: a live run exists in THIS controller's identity space.
	SourceLive SourceState = "live"
	// SourceLiveElsewhere: a live run exists, and none of them in this
	// identity space - another configuration, plan stage or batch owns it.
	SourceLiveElsewhere SourceState = "live_elsewhere"
	// SourceSettled: runs exist and every one of them is terminal.
	SourceSettled SourceState = "settled"
)

// SourceDecision is the classification and the run it names, when one.
type SourceDecision struct {
	State SourceState
	// RunID is the live run: this space's for SourceLive, another's for
	// SourceLiveElsewhere.
	RunID string
	// History is every run of the source with its journalled disposition, so
	// a narrower policy (a requeue predicate, #49) decides over the whole of
	// it rather than re-deriving a part.
	History []SourceRun
}

// SourceRun is one historical run of a source as its journal settles it.
type SourceRun struct {
	Run         EngineeringRun
	Disposition Disposition
}

// SourceLiveElsewhereError refuses to start a source whose live work belongs
// to another identity space. Silently running beside it would be two runs on
// one issue; adopting it would continue another configuration's work here.
type SourceLiveElsewhereError struct {
	Issue int
	RunID string
}

func (e *SourceLiveElsewhereError) Error() string {
	return fmt.Sprintf("issue %d already has live run %s under another configuration, plan stage or batch; continue it with the controller that owns it, or start a new generation explicitly", e.Issue, e.RunID)
}

// classifySource is the decision, over a history whose dispositions the
// journal settled. current names this controller's own identity space. The
// order is deliberate: work in this space is resumed, live work anywhere else
// is never paralleled, and only a source with no history at all is new.
func classifySource(history []SourceRun, current map[string]bool) SourceDecision {
	decision := SourceDecision{State: SourceUnseen, History: history}
	if len(history) == 0 {
		return decision
	}
	decision.State = SourceSettled
	for _, entry := range history {
		if terminalDisposition(entry.Disposition) {
			continue
		}
		if current[entry.Run.ID] {
			return SourceDecision{State: SourceLive, RunID: entry.Run.ID, History: history}
		}
		if decision.State != SourceLiveElsewhere {
			decision.State, decision.RunID = SourceLiveElsewhere, entry.Run.ID
		}
	}
	return decision
}

// sourceDecision reads one source's whole history and classifies it. A run
// whose journal cannot be replayed is an error, never an absence: history this
// process cannot read is not history it may treat as missing.
func (r *EngineeringRuntime) sourceDecision(issue int) (SourceDecision, error) {
	goal := issueGoal(r.deps.Repository.Identity, issue)
	runs, err := r.deps.Store.SourceRuns(r.deps.Repository.Identity, goal)
	if err != nil {
		return SourceDecision{}, err
	}
	history := make([]SourceRun, 0, len(runs))
	for _, run := range runs {
		snapshot, err := r.deps.Store.Replay(run.ID)
		if err != nil {
			return SourceDecision{}, fmt.Errorf("source history for issue %d is unreadable at %s: %w", issue, run.ID, err)
		}
		history = append(history, SourceRun{Run: run, Disposition: snapshot.Disposition})
	}
	current := map[string]bool{}
	for generation := 0; generation < maxRunGenerations; generation++ {
		id, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return SourceDecision{}, err
		}
		current[id] = true
	}
	return classifySource(history, current), nil
}

// claimUnseenSource creates the first run of a source that has NO history, or
// reports false when the source turned out not to be unseen by the time the
// claim was decided. It is discovery's only way to create work.
func (r *EngineeringRuntime) claimUnseenSource(ctx context.Context, issue int) (string, bool, error) {
	runID, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, 0)
	if err != nil {
		return "", false, err
	}
	goal := issueGoal(r.deps.Repository.Identity, issue)
	created, err := r.createRun(ctx, runID, goal, nil, nil, domain.StageBudget{}, r.deps.Store.ClaimUnseenSourceRun)
	if err != nil {
		return "", false, err
	}
	// createRun reports a lost claim as success with the id; whether a run
	// with that id now exists is what says this claim created it or a
	// concurrent claimer did - and either way it is this source's run.
	if _, found, err := r.deps.Store.Run(created); err != nil || !found {
		return "", false, err
	}
	return created, true, nil
}

// liveRunInThisSpace resolves this controller's OWN live run for a source by
// probing only its own identity space, replaying only those runs. It is what a
// protective transition - consent withdrawn, a credential lost - targets: such
// a stop must land on the work this controller drives even when some
// unrelated historical run elsewhere cannot be read, so it deliberately does
// not depend on the whole source history the way starting work does.
func (r *EngineeringRuntime) liveRunInThisSpace(issue int) (string, bool, error) {
	goal := issueGoal(r.deps.Repository.Identity, issue)
	for generation := 0; generation < maxRunGenerations; generation++ {
		id, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return "", false, err
		}
		run, ok, err := r.deps.Store.Run(id)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return "", false, nil
		}
		if run.Repository != r.deps.Repository.Identity || run.Goal != goal {
			return "", false, &RunConflictError{RunID: id, Detail: "durable run describes different work"}
		}
		snapshot, err := r.deps.Store.Replay(id)
		if err != nil {
			return "", false, err
		}
		if !terminalDisposition(snapshot.Disposition) {
			return id, true, nil
		}
	}
	return "", false, nil
}

// liveSourceRunError names the live run an ordinary start's claim lost to - the
// one ClaimRunUnlessSourceLive saw, read the same way it read it.
func (r *EngineeringRuntime) liveSourceRunError(issue int, goal string) error {
	runs, err := r.deps.Store.SourceRuns(r.deps.Repository.Identity, goal)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if !terminalDisposition(run.Disposition) {
			return &SourceLiveElsewhereError{Issue: issue, RunID: run.ID}
		}
	}
	return fmt.Errorf("issue %d: the start lost its claim, and no live run of the source is visible now; retry", issue)
}
