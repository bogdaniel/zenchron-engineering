package main

// `autonomy plan ...` is the operator surface of #64.
//
// It is deliberately a small vocabulary over one durable lifecycle: propose,
// read, approve, reject, revise, follow. Nothing here decides anything - the
// planner compiles, the validator refuses, the operator approves and the plan
// reconciler inside `serve` executes what was approved.
//
// The one thing this file must never gain is an automatic approval. A proposed
// plan that executed because it parsed would make every other boundary in this
// product decorative.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const planUsage = "usage: zenchron-engineering autonomy plan {issue <number> [--template <id>] [--agent <id>] [--deterministic]|" +
	"show <plan>|approve <plan> --revision <n> --digest <sha256> [--note <text>]|" +
	"reject <plan> --revision <n> --digest <sha256> [--note <text>]|" +
	"revise <plan> [--template <id>] [--deterministic] [--substitute-human <stage>]|" +
	"status <plan>|list} [--text] [--repo owner/name] [--config <path>]"

// autonomyPlan dispatches the plan verbs.
func autonomyPlan(ctx context.Context, args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	if len(args) == 0 {
		return runtime.ExitInvalid, errors.New(planUsage)
	}
	verb, rest := args[0], args[1:]

	switch verb {
	case "issue":
		if len(rest) < 1 {
			return runtime.ExitInvalid, errors.New(planUsage)
		}
		issue, err := strconv.Atoi(rest[0])
		if err != nil || issue <= 0 {
			return runtime.ExitInvalid, fmt.Errorf("issue number must be a positive integer, got %q", rest[0])
		}
		flags, err := parseAutonomyFlags(rest[1:])
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return planPropose(ctx, flags, overrides, issue, "", stdout)
	case "list":
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return planList(flags, overrides, stdout)
	}

	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		return runtime.ExitInvalid, errors.New(planUsage)
	}
	planID, remaining := rest[0], rest[1:]
	flags, err := parseAutonomyFlags(remaining)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	switch verb {
	case "show", "status":
		return planShow(flags, overrides, planID, stdout)
	case "approve", "reject":
		return planDecide(flags, overrides, planID, verb, stdout)
	case "revise":
		if flags.SubstituteHuman != "" {
			return planSubstituteHuman(ctx, flags, overrides, planID, stdout)
		}
		return planRevise(ctx, flags, overrides, planID, stdout)
	default:
		return runtime.ExitInvalid, errors.New(planUsage)
	}
}

// planComposition is the wiring one plan command needs: the shared composition,
// an engine bound to the repository, and the plan service over the same store.
type planComposition struct {
	built   *composition
	engine  *runtime.EngineeringRuntime
	service runtime.PlanService
	target  runtime.RepositoryTarget
	release func()
}

func buildPlanComposition(flags autonomyFlags, overrides autonomyOverrides) (*planComposition, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return nil, err
	}
	built, engine, err := buildEngine(flags, overrides)
	if err != nil {
		return nil, err
	}
	registry := built.planning
	// The workforce as the planner sees it. Readiness is PROBED here because a
	// plan an operator is about to approve should say which workers can
	// actually take it - and probing costs nothing: an executable, a version
	// string and a credential file.
	agents := runtime.DescribeExecutionAgents(context.Background(), built.agents, func(agent runtime.ResolvedAgent) runtime.AgentProber {
		return runtime.AgentProberFor(agent, built.artifacts, built.config.StateDir)
	})
	// A profile that would escalate its worker is refused before any plan is
	// compiled against it, rather than at the moment work would start.
	if err := registry.Bind(agents); err != nil {
		built.release()
		return nil, err
	}
	return &planComposition{
		built: built, engine: engine, target: target, release: built.release,
		service: runtime.PlanService{
			Store: built.store, Clock: runtime.RealClock{}, Registry: registry,
			Agents: agents, DefaultAgent: built.agents.Default(),
			Envelope: built.config.PlanEnvelope(),
		},
	}, nil
}

// planPropose compiles a plan for one issue and records it awaiting approval.
func planPropose(ctx context.Context, flags autonomyFlags, overrides autonomyOverrides, issue int, planID string, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()

	intent, err := composed.engine.CompilePlanIntent(ctx, issue)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if planID == "" {
		if planID, err = composed.engine.PlanID(issue); err != nil {
			return runtime.ExitFailed, err
		}
	}
	input := runtime.ProposeInput{
		PlanID: planID, Objective: intent.Objective, Subject: intent.Subject,
		Repository: composed.target.Identity, Contract: intent.Contract,
		Model: intent.Model, Facts: intent.Facts, Template: flags.Template, Issue: issue,
	}
	// REASONING is the default, and it runs through a registered agent in a
	// verified non-mutating mode. `--deterministic` is the honest alternative
	// for an operator who does not want to spend an invocation: it compiles the
	// same obligations with no model at all, and says so.
	if !flags.Deterministic {
		stages, reasoning, err := reasonAboutPlan(ctx, composed, intent, planID, flags.Template)
		if err != nil {
			return runtime.ExitFailed, err
		}
		input.Reasoned, input.Reasoning = stages, reasoning
	}
	plan, err := composed.service.Propose(ctx, input)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	view, err := composed.service.View(plan.ID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	return planOutput(flags, view, stdout, "proposed")
}

// reasonAboutPlan performs one bounded planning invocation.
//
// The workspace is materialized from the exact trusted base, the invocation
// runs in the provider's own non-mutating mode, and the runtime verifies the
// workspace afterwards. A provider that cannot prove the mode is refused here,
// with the reason, rather than being run permissively.
func reasonAboutPlan(ctx context.Context, composed *planComposition, intent runtime.PlanIntent, planID, templateID string) ([]domain.PlanStage, *domain.PlanReasoningProvenance, error) {
	// ELIGIBILITY FIRST, before anything is materialized. A provider with no
	// provable non-mutating mode is ineligible for planning, and discovering
	// that after cloning a repository would spend an operator's time and disk
	// to reach the same refusal.
	agent := composed.engine.PlanningAgent()
	if err := requirePlanningMode(composed, agent); err != nil {
		return nil, nil, err
	}
	workspace, err := composed.engine.MaterializePlanningWorkspace(planID, intent.Base.Revision)
	if err != nil {
		return nil, nil, err
	}
	defer workspace.Remove()

	// The operator's chosen process is PLANNING INPUT, so the planner is shown
	// it. A planner that never saw the template would silently replace the
	// operator's decision about how this work is done with its own.
	var template *domain.EngineeringPlanTemplate
	if templateID != "" {
		chosen, err := composed.service.Registry.Template(templateID)
		if err != nil {
			return nil, nil, err
		}
		template = &chosen
	}
	output, err := runtime.InvokePlanner(ctx, runtime.PlannerInput{
		PlanID: planID, Revision: 1, Template: template,
		Agent: composed.engine.PlanningAgent(), Provider: composed.engine.PlanningProvider(),
		Workspace: workspace, Contract: intent.Contract, Objective: intent.Objective,
		Base: intent.Base, SourceSnapshot: intent.SourceSnapshot,
		ControllerID:          composed.engine.ControllerIdentityID(),
		AvailableRoles:        domain.EngineeringRoles(),
		AvailableCapabilities: domain.EngineeringCapabilities(),
		Artifacts:             composed.engine.PlanningArtifacts(),
	})
	if err != nil {
		return nil, nil, err
	}
	reasoning := output.Reasoning
	return output.Stages, &reasoning, nil
}

// requirePlanningMode refuses a planner whose adapter cannot enter a provable
// non-mutating mode. It is the resolver's invocation-mode eligibility rule,
// applied to the one stage that happens before a plan exists.
func requirePlanningMode(composed *planComposition, agent runtime.ResolvedAgent) error {
	for _, descriptor := range composed.service.Agents {
		if descriptor.ID != agent.ID {
			continue
		}
		if descriptor.SupportsInvocationMode(domain.InvocationModeNonMutatingPlanning) {
			return nil
		}
		break
	}
	return &runtime.InvocationModeUnsupportedError{
		AgentID: agent.ID, Kind: agent.Kind, Mode: domain.InvocationModeNonMutatingPlanning,
		Detail: "this adapter exposes no mode whose non-mutating boundary the runtime can prove; " +
			"select another agent with --agent, or compile the plan without a model using --deterministic",
	}
}

// planRevise re-proposes a plan as a NEW revision. An operator edit is never an
// in-place change: it goes through the same validation and the same approval as
// any other revision.
func planRevise(ctx context.Context, flags autonomyFlags, overrides autonomyOverrides, planID string, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	repository, issue, found, err := composed.built.store.PlanSource(planID)
	composed.release()
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		return exitRunNotFound, fmt.Errorf("no such plan %q", planID)
	}
	if issue <= 0 {
		return runtime.ExitInvalid, fmt.Errorf("plan %s records no source issue, so it cannot be revised", planID)
	}
	if flags.Repo == "" {
		flags.Repo = repository
	}
	return planPropose(ctx, flags, overrides, issue, planID, stdout)
}

// planSubstituteHuman proposes a revision in which one blocked agent stage
// becomes a human decision gate.
//
// It is the operator ACTING on an independence shortage that policy permits a
// person to fill. It applies nothing: the substitution produces a proposed
// revision that goes through the same validation and the same approval as any
// other, because replacing a worker with a person changes what the plan is.
func planSubstituteHuman(ctx context.Context, flags autonomyFlags, overrides autonomyOverrides, planID string, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()

	view, err := composed.service.View(planID)
	if err != nil {
		return exitFor(err, exitRunNotFound), err
	}
	contract, found, err := composed.built.store.PlanContract(planID, view.Plan.Revision)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		return runtime.ExitFailed, fmt.Errorf("plan %s revision %d has no stored contract, so the claims a person would answer are unknown", planID, view.Plan.Revision)
	}
	// The claims the person answers are the contract's own INDEPENDENT claims:
	// the ones policy already said an independent producer must supply. A
	// substitution changes who answers them, never what they are.
	claims := independentClaims(contract)
	if len(claims) == 0 {
		return runtime.ExitInvalid, fmt.Errorf(
			"the work contract defines no claim requiring an independent producer, so there is nothing for a human decision gate to answer")
	}
	stages, err := planning.SubstituteHumanReview(view.Plan, flags.SubstituteHuman, claims)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	repository, issue, _, err := composed.built.store.PlanSource(planID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	_ = repository
	plan, err := composed.service.Propose(ctx, runtime.ProposeInput{
		PlanID: planID, Objective: view.Plan.Objective, Subject: view.Plan.Subject,
		Contract: contract, Reasoned: stages, Issue: issue,
		Origin: "operator_edit",
	})
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	revised, err := composed.service.View(plan.ID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	return planOutput(flags, revised, stdout, "proposed")
}

// independentClaims are the contract's claims that require a producer
// independent of the change producer - which is exactly what a human
// substitution is being asked to supply.
func independentClaims(contract domain.EngineeringWorkContract) []string {
	var claims []string
	for id, claim := range contract.RequiredClaims {
		if claim.IndependentFromChangeProducer {
			claims = append(claims, id)
		}
	}
	sort.Strings(claims)
	return claims
}

// planShow is the approval view: the exact revision, its state, the assignments
// resolution would produce, every blocked stage with its reason, and the
// budget - known and unknown alike.
func planShow(flags autonomyFlags, overrides autonomyOverrides, planID string, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()
	view, err := composed.service.View(planID)
	if err != nil {
		return exitFor(err, exitRunNotFound), err
	}
	return planOutput(flags, view, stdout, "")
}

// planDecide records the operator's approval or rejection of the exact revision
// they were shown.
func planDecide(flags autonomyFlags, overrides autonomyOverrides, planID, verb string, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()

	operator, err := composed.built.config.ResolveOperator()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	pending, found, err := composed.built.store.Plan(planID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		return exitRunNotFound, fmt.Errorf("no such plan %q", planID)
	}
	// The decision names the revision the OPERATOR READ, not whatever is
	// highest when the command runs. Reading the plan here and deciding on
	// that same read compared a digest to itself: a decomposition proposal
	// stored between reading and deciding - which `serve` does on its own -
	// would be approved sight unseen. `plan show` prints the exact command.
	revision, digest := flags.Revision, strings.TrimSpace(flags.Digest)
	if revision < 1 || digest == "" {
		return runtime.ExitInvalid, fmt.Errorf(
			"%s names the exact revision it decides: run `autonomy plan show %s` and use the command it prints (currently `autonomy plan %s %s --revision %d --digest %s`)",
			verb, planID, verb, planID, pending.Revision, pending.Digest)
	}
	decide := composed.service.Approve
	if verb == "reject" {
		decide = composed.service.Reject
	}
	snapshot, err := decide(planID, revision, digest, operator.ID, flags.Note)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	if flags.Text {
		fmt.Fprintf(stdout, "plan %s revision %d %s by %s\n", planID, revision, snapshot.Approval.Status, operator.ID)
		return runtime.ExitCompleted, nil
	}
	if err := writeJSON(stdout, snapshot); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}

// planList is the fleet view for plans.
func planList(flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()
	plans, err := composed.built.store.Plans()
	if err != nil {
		return runtime.ExitFailed, err
	}
	type summary struct {
		PlanID    string                 `json:"plan_id"`
		Revision  int                    `json:"revision"`
		Objective string                 `json:"objective"`
		Approval  string                 `json:"approval"`
		Stages    map[string]int         `json:"stages"`
		Consumed  domain.PlanConsumption `json:"consumed"`
	}
	summaries := make([]summary, 0, len(plans))
	for _, plan := range plans {
		snapshot, err := composed.built.store.ReplayPlan(plan.ID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		states := map[string]int{}
		for _, projection := range snapshot.Stages {
			states[string(projection.State)]++
		}
		summaries = append(summaries, summary{
			PlanID: plan.ID, Revision: plan.Revision, Objective: plan.Objective,
			Approval: string(snapshot.Approval.Status), Stages: states, Consumed: snapshot.Consumed,
		})
	}
	if !flags.Text {
		if err := writeJSON(stdout, summaries); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	if len(summaries) == 0 {
		fmt.Fprintln(stdout, "no plans")
		return runtime.ExitCompleted, nil
	}
	for _, item := range summaries {
		fmt.Fprintf(stdout, "%s  r%d  %-9s  %s\n", item.PlanID, item.Revision, item.Approval, singleLinePlan(item.Objective))
	}
	return runtime.ExitCompleted, nil
}

// planOutput writes the approval view.
func planOutput(flags autonomyFlags, view runtime.PlanView, stdout io.Writer, action string) (int, error) {
	if !flags.Text {
		if err := writeJSON(stdout, view); err != nil {
			return runtime.ExitFailed, err
		}
		return planExit(view), nil
	}
	if action != "" {
		fmt.Fprintf(stdout, "%s plan %s revision %d\n", action, view.Plan.ID, view.Plan.Revision)
	}
	fmt.Fprintf(stdout, "plan %s revision %d (%s)\n", view.Plan.ID, view.Plan.Revision, view.Snapshot.Approval.Status)
	fmt.Fprintf(stdout, "objective: %s\n", singleLinePlan(view.Plan.Objective))
	if reasoning := view.Plan.Provenance.Reasoning; reasoning != nil {
		fmt.Fprintf(stdout, "planned by: %s (%s, %s) in %s mode; workspace verified unchanged: %v\n",
			reasoning.AgentID, reasoning.ProviderKind, reasoning.VendorFamily, reasoning.InvocationMode, reasoning.WorkspaceUnchanged)
	} else {
		fmt.Fprintln(stdout, "planned by: the deterministic compiler; no model was invoked")
	}
	if template := view.Plan.Provenance.Template; template != nil {
		fmt.Fprintf(stdout, "template: %s v%d\n", template.ID, template.Version)
	}
	fmt.Fprintln(stdout, "stages:")
	assignments := map[string]domain.AgentAssignment{}
	for _, assignment := range view.Assigned {
		assignments[assignment.StageID] = assignment
	}
	for _, stage := range view.Plan.Stages {
		state := "pending"
		if projection, ok := view.Snapshot.Stages[stage.ID]; ok && projection.State != "" {
			state = string(projection.State)
		}
		line := fmt.Sprintf("  %-18s %-20s %-11s", stage.ID, stage.Kind, state)
		switch {
		case stage.Kind != domain.StageAgent:
			line += fmt.Sprintf(" references claims %s", strings.Join(stage.RequiredClaims, ", "))
		default:
			line += fmt.Sprintf(" role=%s", stage.Role)
			if assignment, ok := assignments[stage.ID]; ok {
				line += fmt.Sprintf(" profile=%s agent=%s (%s)", assignment.Profile.ID, assignment.Agent.ID, assignment.Agent.VendorFamily)
			}
			if stage.Independence != nil {
				line += fmt.Sprintf(" independent-of=%s in %s", strings.Join(stage.Independence.DifferentFrom, ","), stage.Independence.Dimension)
			}
		}
		fmt.Fprintln(stdout, line)
	}
	for _, blocked := range view.Blocked {
		fmt.Fprintf(stdout, "blocked: %s (%s) %s\n", blocked.StageID, blocked.Kind, blocked.Reason)
		if blocked.HumanSubstitutionPermitted {
			fmt.Fprintln(stdout, "         policy permits an independent human review in place of this worker; approving that is an operator decision through `plan revise`")
		}
	}
	writePlanBudget(stdout, view)
	// The decision command names the EXACT revision awaiting one, with its
	// digest. The revision rendered above is the one that GOVERNS, which is not
	// always the one being asked about: a decomposition proposal is stored as a
	// new unapproved revision while the approved one keeps executing. Printing
	// the command with both is what lets an operator approve what they read.
	if awaiting := view.Snapshot.Approval; awaiting.Status == domain.ApprovalPending && awaiting.Revision > 0 {
		if awaiting.Revision != view.Plan.Revision {
			fmt.Fprintf(stdout, "awaiting a decision: revision %d (digest %s)\n", awaiting.Revision, awaiting.Digest)
		}
		fmt.Fprintf(stdout, "nothing executes until it is approved: `autonomy plan approve %s --revision %d --digest %s`\n",
			view.Plan.ID, awaiting.Revision, awaiting.Digest)
	}
	return planExit(view), nil
}

// writePlanBudget prints the envelope beside what has been consumed. Unknown
// cost is printed as unknown: rendering "not reported" as zero would tell an
// operator something nobody measured.
func writePlanBudget(stdout io.Writer, view runtime.PlanView) {
	fmt.Fprintf(stdout, "budget: child runs %d/%d, provider invocations %d/%d, concurrency ceiling %d\n",
		view.Consumed.ChildRuns, view.Envelope.MaxChildRuns,
		view.Consumed.ProviderInvocations, view.Envelope.MaxProviderInvocations,
		view.Envelope.MaxConcurrency)
	switch {
	case view.Consumed.CostKnown && view.Consumed.CostMicros != nil:
		fmt.Fprintf(stdout, "cost: %d micros reported\n", *view.Consumed.CostMicros)
	default:
		fmt.Fprintln(stdout, "cost: unknown - no configured provider reported one, and unknown is not zero")
	}
}

// planExit maps the view onto a process status. A plan awaiting approval is
// WAITING rather than completed: the runtime is waiting on a person.
func planExit(view runtime.PlanView) int {
	switch view.Snapshot.Approval.Status {
	case domain.ApprovalApproved:
		return runtime.ExitCompleted
	case domain.ApprovalRejected:
		return runtime.ExitCompleted
	default:
		return runtime.ExitWaiting
	}
}

func singleLinePlan(text string) string {
	line := strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
	if len(line) > 120 {
		return line[:117] + "..."
	}
	return line
}
