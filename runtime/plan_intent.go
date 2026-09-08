package runtime

// Engineering intent, compiled for planning.
//
// A plan is compiled BEFORE any run exists, so it cannot read the contract a
// run's pipeline compiles. This file builds the same thing the same way: the
// pinned source snapshot, the predicted facts from the same analyzer, and the
// contract from the same policy compiler. There is one compiler and one
// analyzer in this product, and planning uses them rather than approximating
// them.

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

// PlanIntent is everything a plan is compiled from, as observed once.
type PlanIntent struct {
	// Objective is runtime-authored text derived from the pinned source. The
	// untrusted title and body reach it only inside the same delimiters every
	// other consumer sees them in.
	Objective string
	Subject   domain.Subject
	Contract  domain.EngineeringWorkContract
	Model     domain.ProjectModel
	Facts     []domain.EngineeringFact
	// Base is the trusted revision the plan - and any planning workspace - is
	// bound to.
	Base Ref
	// SourceSnapshot identifies the pinned source, so a plan records which
	// exact issue state it answered.
	SourceSnapshot Ref
	// Source is the durable snapshot record, kept so a planning invocation can
	// show the same untrusted text a run's worker would see.
	Source sourceRecord
}

// CompilePlanIntent observes the issue, pins it, and compiles the predicted
// work contract a plan is planned against.
//
// It performs the same two external reads a run's first operation performs -
// the issue and the trusted base - and nothing else. It creates no run, no
// candidate and no durable run state: planning must be able to happen without
// committing an operator to work they have not approved.
func (r *EngineeringRuntime) CompilePlanIntent(ctx context.Context, issue int) (PlanIntent, error) {
	if issue <= 0 {
		return PlanIntent{}, fmt.Errorf("a plan answers a source issue, and %d is not one", issue)
	}
	observed, err := r.deps.GitHub.Issue(ctx, r.repo, issue)
	if err != nil {
		return PlanIntent{}, err
	}
	base, err := r.deps.GitHub.RefSHA(ctx, r.repo, r.deps.Repository.DefaultBranch)
	if err != nil {
		return PlanIntent{}, err
	}
	if !base.Exists {
		return PlanIntent{}, fmt.Errorf("default branch %q not found in %s", r.deps.Repository.DefaultBranch, r.repo)
	}
	record, err := newSourceRecord(r.deps.Repository.Identity, observed, base.SHA)
	if err != nil {
		return PlanIntent{}, err
	}
	if record.SnapshotPath, err = r.storeUntrustedSource(observed, record); err != nil {
		return PlanIntent{}, err
	}
	text, err := r.untrustedSource(record)
	if err != nil {
		return PlanIntent{}, err
	}
	subject := domain.Subject{Repository: r.deps.Repository.Identity, Revision: base.SHA}
	model := r.deps.ProjectModel
	model.Subject = subject

	source := SourceSnapshot{
		ID:               fmt.Sprintf("issue-%s-%d", r.deps.Repository.Identity, issue),
		Objective:        untrustedObjective(record, text),
		AcceptanceIntent: runtimeAcceptanceIntent,
		PredictedPaths:   []string{predictedScopePlaceholder},
	}
	kernel, err := r.flow.Compile(source, model, r.deps.Policy, planContractID(r.deps.Repository.Identity, issue), "1")
	if err != nil {
		return PlanIntent{}, err
	}
	// The facts are recomputed from the SAME analyzer and the same intent the
	// compiler used. They are needed here because a template's conditional
	// stages are evaluated against them, and a second source of facts would be
	// a second answer to what the change is.
	analyzer := r.flow.Analyzer
	if analyzer.IsZero() {
		analyzer = analysis.NewAnalyzer()
	}
	predicted, err := analyzer.Predict(model, subject, analysis.Intent{
		Objective: source.Objective, AcceptanceIntent: source.AcceptanceIntent,
		AffectedPaths: source.PredictedPaths, PathsKnown: source.PathsKnown,
	})
	if err != nil {
		return PlanIntent{}, err
	}
	return PlanIntent{
		Objective:      source.Objective,
		Subject:        subject,
		Contract:       kernel.Contract,
		Model:          model,
		Facts:          predicted.Sorted(),
		Base:           Ref{ID: r.deps.Repository.DefaultBranch, Revision: base.SHA},
		SourceSnapshot: Ref{ID: source.ID, Revision: record.Digest},
		Source:         record,
	}, nil
}

// planContractID is the contract identity a plan is compiled against. It is
// derived from the repository and the issue, so re-planning the same source
// produces the same contract identity rather than a new one each time.
func planContractID(repository string, issue int) string {
	return fmt.Sprintf("contract-plan-%s-%d", repository, issue)
}

// PlanID is the durable identity of the plan for one source.
//
// It is derived, like a run identity, from facts that do not move: the
// repository, the issue and the operator configuration. Two processes that
// decide to plan the same source therefore compute the same id before either
// touches the database, so "two plans for one source" is unrepresentable rather
// than a race to lose.
func (r *EngineeringRuntime) PlanID(issue int) (string, error) {
	digest, err := Digest(struct {
		Repository string       `json:"repository"`
		Issue      int          `json:"issue"`
		Config     ConfigDigest `json:"config"`
	}{r.deps.Repository.Identity, issue, r.deps.ConfigDigest})
	if err != nil {
		return "", err
	}
	return "plan-" + digest[:32], nil
}

// PlanningSource is the repository the planning workspace is materialized from.
// It is the governed remote the runtime already clones candidates from, never
// the controller checkout.
func (r *EngineeringRuntime) PlanningSource() string { return r.deps.Repository.Remote }

// PlanningAgent is the agent this runtime drives, for a planning invocation.
func (r *EngineeringRuntime) PlanningAgent() ResolvedAgent { return r.deps.Agent }

// PlanningProvider is the execution provider bound to that agent.
func (r *EngineeringRuntime) PlanningProvider() ExecutionProvider { return r.deps.Provider }

// PlanningArtifacts is the artifact store a planning transcript is filed in.
func (r *EngineeringRuntime) PlanningArtifacts() ArtifactStore { return r.deps.Artifacts }

// ControllerIdentityID is the controller identity recorded on a planning
// invocation.
func (r *EngineeringRuntime) ControllerIdentityID() string { return r.deps.ControllerID }

// StateDirectory is where runtime-owned state - including a planning workspace
// - lives.
func (r *EngineeringRuntime) StateDirectory() string { return r.deps.StateDir }

// PlanningRegistry is the operator customization registry this runtime was
// built with.
func (r *EngineeringRuntime) PlanningRegistry() interface{ Dir() string } { return r.deps.Planning }
