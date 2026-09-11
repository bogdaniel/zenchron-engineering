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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const planUsage = "usage: zenchron-engineering autonomy plan {issue <number> [--template <id>] [--agent <id>] [--deterministic]|" +
	"show <plan> [--revision <n>]|approve <plan> --revision <n> --digest <sha256> --assignments <sha256> [--note <text>]|" +
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

// delegatePlanRevision sends a revision request to a running supervisor, for
// the same reason a decision goes there: it owns the work being revised.
func delegatePlanRevision(flags autonomyFlags, overrides autonomyOverrides, planID string, issue int, stdout io.Writer) (bool, int, error) {
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return false, 0, nil
	}
	stateDir := reader.config.StateDir
	requester, operatorErr := reader.config.ResolveOperator()
	reader.release()
	if operatorErr != nil {
		return true, runtime.ExitInvalid, operatorErr
	}
	// A FIRST proposal names its subject: the supervisor governs several
	// repositories and has no plan to read the answer from. It resolves the
	// same way the local path resolves it, and the supervisor still refuses a
	// repository it does not govern - naming one is a selection, never an
	// introduction.
	repository, defaultBranch := "", ""
	if planID == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return true, runtime.ExitInvalid, err
		}
		target, err := repositoryTarget(cwd, flags.Repo)
		if err != nil {
			return true, runtime.ExitInvalid, err
		}
		repository, defaultBranch = target.Identity, target.DefaultBranch
	}
	delegated, payload, err := delegatePayload(stateDir, runtime.ControlRequest{
		Command: runtime.ControlPlanRevise, PlanID: planID, Issue: issue,
		Repository: repository, DefaultBranch: defaultBranch, Template: flags.Template,
		Deterministic: flags.Deterministic, SubstituteHuman: flags.SubstituteHuman,
		Note: flags.Note, Operator: requester.ID,
	})
	if !delegated {
		return false, 0, nil
	}
	if err != nil {
		return true, exitFor(err, runtime.ExitFailed), err
	}
	code, err := renderDelegatedPlan(flags, payload, false, stdout)
	return true, code, err
}

// resolveRequestingOperator is this terminal's operator identity, resolved the
// same way the local decision path resolves it.
func resolveRequestingOperator(flags autonomyFlags, overrides autonomyOverrides) (string, error) {
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return "", err
	}
	defer reader.release()
	operator, err := reader.config.ResolveOperator()
	if err != nil {
		return "", err
	}
	return operator.ID, nil
}

// reportDelegatedDecision answers "did my decision happen" from the durable
// record when the supervisor's reply did not arrive.
func reportDelegatedDecision(flags autonomyFlags, overrides autonomyOverrides, planID, verb string, revision int, digest string, cause error, stdout io.Writer) (int, error) {
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return runtime.ExitFailed, cause
	}
	defer reader.release()
	// The QUESTION is historical: did this exact decision event land? It is not
	// "is this still the latest decision", which is what the snapshot's
	// approval slot answers - and that slot is reset to pending by the next
	// proposal. The supervisor's own reconcile appends one as soon as the
	// approval unblocks a decomposition, so consulting the slot reported "the
	// decision was NOT applied" for decisions that HAD been applied: the exact
	// inverse of the defect this path exists to prevent.
	events, err := reader.store.PlanEvents(planID)
	if err != nil {
		return runtime.ExitFailed, cause
	}
	decided, applied := decisionEventFor(events, verb, revision, digest)
	if !applied {
		return runtime.ExitFailed, fmt.Errorf("%w (the decision was NOT applied: no %s of revision %d at digest %s is recorded)",
			cause, verb, revision, digest)
	}
	fmt.Fprintf(stdout, "plan %s revision %d %s by %s\n", planID, decided.Revision, decided.Status, decided.Operator)
	fmt.Fprintf(stdout, "the supervisor applied it but its reply did not arrive (%v); the durable record above is what happened\n", cause)
	return runtime.ExitCompleted, nil
}

// decisionEventFor finds the durable decision this invocation asked for, by
// revision, digest and verb. It reads the journal rather than a projection
// because a projection answers about the present and this question is about the
// past: a later proposal, a later decision, or a supersession all move the
// present without unmaking what already happened.
func decisionEventFor(events []runtime.EngineeringEvent, verb string, revision int, digest string) (runtime.PlanApproval, bool) {
	want := runtime.EventPlanApproved
	status := domain.ApprovalApproved
	if verb == "reject" {
		want, status = runtime.EventPlanRejected, domain.ApprovalRejected
	}
	for _, event := range events {
		if event.Type != want {
			continue
		}
		var payload runtime.PlanDecisionPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			// An unreadable decision record is not this decision. It is also
			// not a reason to claim one landed.
			continue
		}
		if payload.Revision != revision || payload.Digest != digest {
			continue
		}
		return runtime.PlanApproval{
			Revision: payload.Revision, Digest: payload.Digest,
			Status: status, Operator: payload.Operator, Note: payload.Note,
		}, true
	}
	return runtime.PlanApproval{}, false
}

// renderDelegatedPlan prints a supervisor's answer exactly as a locally applied
// decision prints: an operator should not be able to tell which process did it.
func renderDelegatedPlan(flags autonomyFlags, payload []byte, decided bool, stdout io.Writer) (int, error) {
	var view runtime.PlanView
	if err := json.Unmarshal(payload, &view); err != nil {
		return runtime.ExitFailed, err
	}
	if decided && flags.Text {
		decision := view.Snapshot.Approval
		fmt.Fprintf(stdout, "plan %s revision %d %s by %s\n",
			view.Plan.ID, decision.Revision, decision.Status, decision.Operator)
		return runtime.ExitCompleted, nil
	}
	if decided {
		if err := writeJSON(stdout, view.Snapshot); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	return planOutput(flags, view, stdout, "proposed")
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

// planReader is the READ-ONLY view of a plan: the durable store and the
// operator's own artifacts, and no runtime ownership at all.
//
// A reader that took ownership could not run while a supervisor owned the state
// directory in the same process, and - more to the point - reading a plan is
// not an act that owns anything. The store is WAL with a busy timeout, so a
// reader and a running supervisor are the designed case.
type planReader struct {
	config  runtime.Config
	store   *runtime.SQLiteOperationStore
	service runtime.PlanService
	release func()
}

func openPlanReader(flags autonomyFlags, overrides autonomyOverrides) (*planReader, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return nil, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return nil, err
	}
	registry, err := planning.LoadRegistry(config.PlanningDir)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	agents, err := config.AgentRegistry()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	described := runtime.DescribeExecutionAgents(context.Background(), agents, func(agent runtime.ResolvedAgent) runtime.AgentProber {
		return runtime.AgentProberFor(agent, runtime.ArtifactStore{Root: filepath.Join(config.StateDir, "artifacts")}, config.StateDir)
	})
	if err := registry.Bind(described); err != nil {
		_ = store.Close()
		return nil, err
	}
	return &planReader{
		config: config, store: store,
		service: runtime.PlanService{
			Store: store, Clock: runtime.RealClock{}, Registry: registry,
			Agents: described, DefaultAgent: agents.Default(), Envelope: config.PlanEnvelope(),
		},
		release: func() { _ = store.Close() },
	}, nil
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
	// EVERY proposal goes to the supervisor when one is running - a first one
	// as much as a revision.
	//
	// A first proposal does race nothing, but that was never the obstacle: the
	// local path builds a composition, and a composition takes the exclusive
	// ownership lock on the state directory, which a running `serve` already
	// holds. So an operator could revise and decide plans while `serve` ran but
	// could not START one without stopping the persistent runtime, which is the
	// runtime's whole point.
	if delegated, code, err := delegatePlanRevision(flags, overrides, planID, issue, stdout); delegated {
		return code, err
	}
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()
	return proposeWithComposition(ctx, composed, flags, issue, planID, stdout)
}

// proposeWithComposition is the propose itself, over an already-built
// composition. The supervisor uses it too, through its OWN composition, so a
// revision requested while `serve` is running is applied by the process that
// owns the work rather than by a second one racing it.
func proposeWithComposition(ctx context.Context, composed *planComposition, flags autonomyFlags, issue int, planID string, stdout io.Writer) (int, error) {
	return proposeSerialized(ctx, composed, flags, issue, planID, stdout, nil)
}

// proposeSerialized is the propose with an optional CRITICAL SECTION around the
// durable write alone.
//
// The supervisor passes one so an operator's decision cannot interleave with
// the write; it deliberately does not cover the planning invocation, which
// clones a repository and calls a provider. Holding a lock across that stalled
// every run in the fleet - the reconciler takes the same lock before it drives
// anything - so the long part runs unlocked and only the append is serialized.
func proposeSerialized(ctx context.Context, composed *planComposition, flags autonomyFlags, issue int, planID string, stdout io.Writer, under func(func() error) error) (int, error) {
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
		// Referenced same-repository issue context, read through the governed
		// forge boundary HERE rather than during intent compilation: it is
		// planning input for a MODEL, and a deterministic compilation has no
		// model to give it to. Recording it on a plan nothing reasoned about
		// would claim the planner was given context when no planner ran.
		references, err := composed.engine.HydrateReferences(ctx, intent)
		if err != nil {
			return runtime.ExitFailed, err
		}
		intent.References = references
		input.References = intent.ReferencePayloads()

		output, err := reasonAboutPlan(ctx, composed, intent, planID, flags.Template)
		// The transcripts travel WITH the proposal, and they travel with a
		// REFUSAL too. An answer the runtime could not read is the same product
		// event as a proposal it could not compile: an invocation was spent and
		// there is no plan, and the operator needs the same record of it.
		input.Reasoned, input.Evidence = output.Stages, output.Artifacts
		if output.Reasoning.AgentID != "" {
			reasoning := output.Reasoning
			input.Reasoning = &reasoning
		}
		if err != nil {
			return exitFor(err, runtime.ExitFailed), recordPlanningRefusal(composed, input, err)
		}
	}
	var plan domain.EngineeringPlan
	write := func() (err error) { plan, err = composed.service.Propose(ctx, input); return err }
	if under != nil {
		err = under(write)
	} else {
		err = write()
	}
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	view, err := composed.service.View(plan.ID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	return planOutput(flags, view, stdout, "proposed")
}

// recordPlanningRefusal preserves a refused reasoning invocation and returns the
// refusal to report.
//
// The refusal itself is what the operator gets back either way. Recording it is
// best-effort in one direction only: a record that could not be written is
// stated alongside the refusal rather than replacing it, because the answer to
// "why did planning fail" must not become "and also the evidence system failed".
func recordPlanningRefusal(composed *planComposition, input runtime.ProposeInput, cause error) error {
	attempt, err := composed.service.RecordPlanningRefusal(input, cause)
	if err != nil {
		return fmt.Errorf("%w (and the refused attempt could not be recorded: %v)", cause, err)
	}
	return fmt.Errorf("%w\nthe invocation and its evidence are preserved as plan attempt %s: read it with `autonomy plan show %s --text`",
		cause, attempt, input.PlanID)
}

// reasonAboutPlan performs one bounded planning invocation.
//
// The workspace is materialized from the exact trusted base, the invocation
// runs in the provider's own non-mutating mode, and the runtime verifies the
// workspace afterwards. A provider that cannot prove the mode is refused here,
// with the reason, rather than being run permissively.
func reasonAboutPlan(ctx context.Context, composed *planComposition, intent runtime.PlanIntent, planID, templateID string) (runtime.PlannerOutput, error) {
	// ELIGIBILITY FIRST, before anything is materialized. A provider with no
	// provable non-mutating mode is ineligible for planning, and discovering
	// that after cloning a repository would spend an operator's time and disk
	// to reach the same refusal.
	agent := composed.engine.PlanningAgent()
	if err := requirePlanningMode(composed, agent); err != nil {
		return runtime.PlannerOutput{}, err
	}
	workspace, err := composed.engine.MaterializePlanningWorkspace(planID, intent.Base.Revision)
	if err != nil {
		return runtime.PlannerOutput{}, err
	}
	defer workspace.Remove()

	// The operator's chosen process is PLANNING INPUT, so the planner is shown
	// it. A planner that never saw the template would silently replace the
	// operator's decision about how this work is done with its own.
	var template *domain.EngineeringPlanTemplate
	if templateID != "" {
		chosen, err := composed.service.Registry.Template(templateID)
		if err != nil {
			return runtime.PlannerOutput{}, err
		}
		template = &chosen
	}
	// InvokePlanner returns its provenance and transcripts even when it
	// refuses, and they are returned to the caller unchanged: a refused
	// invocation still happened, and the evidence of it is the point.
	output, err := runtime.InvokePlanner(ctx, runtime.PlannerInput{
		PlanID: planID, Revision: 1, Template: template,
		Agent: composed.engine.PlanningAgent(), Provider: composed.engine.PlanningProvider(),
		Workspace: workspace, Contract: intent.Contract, Objective: intent.Objective,
		Base: intent.Base, SourceSnapshot: intent.SourceSnapshot,
		ControllerID:          composed.engine.ControllerIdentityID(),
		AvailableRoles:        domain.EngineeringRoles(),
		AvailableCapabilities: domain.EngineeringCapabilities(),
		Artifacts:             composed.engine.PlanningArtifacts(),
		// The referenced same-repository issues, pinned by the runtime through
		// the governed forge boundary before this invocation. The provider
		// makes no network request of its own; it is SHOWN this text, as
		// untrusted engineering source, exactly as it is shown the primary
		// issue.
		References: intent.References,
	})
	return output, err
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
	if delegated, code, err := delegatePlanRevision(flags, overrides, planID, 0, stdout); delegated {
		return code, err
	}
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
	if delegated, code, err := delegatePlanRevision(flags, overrides, planID, 0, stdout); delegated {
		return code, err
	}
	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()
	return substituteHumanWithComposition(ctx, composed, flags, planID, stdout)
}

// substituteHumanWithComposition is the substitution itself, over an
// already-built composition, so the supervisor can perform it through its own.
func substituteHumanWithComposition(ctx context.Context, composed *planComposition, flags autonomyFlags, planID string, stdout io.Writer) (int, error) {
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
			"the work contract defines no claim that requires an independent producer AND can be answered by a person, so a human decision gate would state something no decision could ever satisfy; the independence obligation stands and needs a second worker")
	}
	stages, err := planning.SubstituteHumanReview(view.Plan, flags.SubstituteHuman, claims)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	// The substitution produces a REVISION of this plan, and a revision answers
	// the same source the plan does. `plan revise` refuses a plan with no
	// recorded source; this path dropped the answer and proposed anyway,
	// producing an approvable revision of a plan the supervisor will never
	// drive.
	_, issue, found, err := composed.built.store.PlanSource(planID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found || issue <= 0 {
		return runtime.ExitInvalid, fmt.Errorf("plan %s records no source issue, so it cannot be revised", planID)
	}
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
		// The claim must require independence AND be answerable by a person.
		// A claim discharged by a test result or a security review is not
		// discharged by someone saying so: handing those to a human decision
		// gate produced a gate no decision could ever satisfy, which blocked
		// everything downstream forever rather than refusing at the point the
		// operator chose it.
		if claim.IndependentFromChangeProducer && claim.EvidenceClass == runtime.HumanApprovalEvidenceClass {
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
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer reader.release()
	// `--revision N` shows one exact revision. The default is the governing
	// one, which is not always the revision an operator is being ASKED about:
	// a decomposition proposal is stored as a new unapproved revision while the
	// approved one keeps executing, and reading it is the whole point of being
	// asked.
	view, err := reader.service.ViewRevision(planID, flags.Revision)
	if err != nil {
		// A plan identity with no revision is not "no such plan": it is a plan
		// whose every reasoning attempt was refused. Reading it is the whole
		// point of preserving the attempts, so this falls through to them
		// rather than telling an operator that the work they just paid a
		// provider invocation for does not exist.
		//
		// Only when the identity itself is unknown does the original refusal
		// stand.
		attempts, attemptErr := reader.service.AttemptsView(planID)
		if attemptErr != nil || len(attempts.Attempts) == 0 {
			return exitFor(err, exitRunNotFound), err
		}
		return planAttemptsOutput(flags, attempts, stdout)
	}
	return planOutput(flags, view, stdout, "")
}

// planAttemptsOutput renders a plan that has no executable revision: every
// refused reasoning attempt, what it proposed and why the machine refused it.
//
// It answers, without a single grep through an artifact directory: which
// attempt, which reasoning agent, what provenance, which stages, which
// validation status, which typed errors, which evidence, and whether an
// executable EngineeringPlan exists at all.
func planAttemptsOutput(flags autonomyFlags, view runtime.PlanAttemptsView, stdout io.Writer) (int, error) {
	if !flags.Text {
		if err := writeJSON(stdout, view); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitFailed, nil
	}
	fmt.Fprintf(stdout, "plan %s (%s)\n", view.PlanID, view.Repository)
	if view.Issue > 0 {
		fmt.Fprintf(stdout, "  source          issue #%d\n", view.Issue)
	}
	fmt.Fprintln(stdout, "  executable plan NONE: every reasoning attempt so far was refused by deterministic validation")
	fmt.Fprintln(stdout, "  nothing here is approvable, and nothing here has executed")
	for _, attempt := range view.Attempts {
		fmt.Fprintf(stdout, "\nattempt %s (revision %d, origin %s", attempt.AttemptID, attempt.Revision, attempt.Origin)
		if attempt.RefusedAt != "" {
			fmt.Fprintf(stdout, ", refused %s", attempt.RefusedAt)
		}
		fmt.Fprintln(stdout, ")")
		if reasoning := attempt.Reasoning; reasoning != nil {
			fmt.Fprintf(stdout, "  reasoned by     %s (%s", reasoning.AgentID, reasoning.ProviderKind)
			if reasoning.Model != "" {
				fmt.Fprintf(stdout, ", model %s", reasoning.Model)
			}
			fmt.Fprintf(stdout, ", %s)\n", reasoning.InvocationMode)
			fmt.Fprintf(stdout, "  workspace       %s (%s -> %s)\n",
				map[bool]string{true: "unchanged", false: "CHANGED"}[reasoning.WorkspaceUnchanged],
				short(reasoning.WorkspaceDigestBefore), short(reasoning.WorkspaceDigestAfter))
		}
		fmt.Fprintln(stdout, "  validation      refused")
		for _, reason := range attempt.Errors {
			fmt.Fprintf(stdout, "    - %s\n", reason)
		}
		for _, stage := range attempt.Stages {
			line := fmt.Sprintf("    %s (%s", stage.ID, stage.Kind)
			if stage.Role != "" {
				line += ", role " + stage.Role
			}
			if len(stage.DependsOn) > 0 {
				line += ", after " + strings.Join(stage.DependsOn, " and ")
			}
			if stage.Independence != nil {
				// An independence over NOTHING is printed as such. It is the
				// state that explains this whole record, and rendering it as
				// though no independence had been asked for would hide the
				// thing an operator is reading this to find.
				peers := "nothing"
				if len(stage.Independence.DifferentFrom) > 0 {
					peers = strings.Join(stage.Independence.DifferentFrom, " and ")
				}
				line += fmt.Sprintf(", independent of %s in %s", peers, stage.Independence.Dimension)
			}
			fmt.Fprintln(stdout, line+")")
		}
		for _, reference := range attempt.References {
			if reference.Issue == 0 {
				fmt.Fprintf(stdout, "  context         %s\n", reference.Detail)
				continue
			}
			status := "pinned " + short(reference.Digest)
			if !reference.Available {
				status = "UNAVAILABLE: " + reference.Detail
			}
			fmt.Fprintf(stdout, "  context         %s issue #%d (%s)\n", reference.Repository, reference.Issue, status)
		}
		for _, evidence := range attempt.Evidence {
			fmt.Fprintf(stdout, "  evidence        %s\n", evidence.Path)
		}
	}
	return runtime.ExitFailed, nil
}

// planDecide records the operator's approval or rejection of the exact revision
// they were shown.
func planDecide(flags autonomyFlags, overrides autonomyOverrides, planID, verb string, stdout io.Writer) (int, error) {
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	_, found, err := reader.store.Plan(planID)
	stateDir := reader.config.StateDir
	awaiting, snapshotErr := reader.store.ReplayPlan(planID)
	reader.release()
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		return exitRunNotFound, fmt.Errorf("no such plan %q", planID)
	}
	if snapshotErr != nil {
		return runtime.ExitFailed, snapshotErr
	}
	// The decision names the revision the OPERATOR READ, not whatever is
	// highest when the command runs. Reading the plan here and deciding on
	// that same read compared a digest to itself: a decomposition proposal
	// stored between reading and deciding - which `serve` does on its own -
	// would be approved sight unseen. `plan show` prints the exact command.
	revision, digest := flags.Revision, strings.TrimSpace(flags.Digest)
	if revision < 1 || digest == "" {
		// The hint names the revision AWAITING a decision, which is not always
		// the highest stored one: a rejected proposal is still stored, and
		// hinting it would hand the operator a command to approve exactly what
		// they just turned down. Where nothing awaits a decision, the refusal
		// says so instead of offering a command.
		if awaiting.Approval.Status != domain.ApprovalPending || awaiting.Approval.Revision < 1 {
			return runtime.ExitInvalid, fmt.Errorf(
				"plan %s has no revision awaiting a decision (revision %d is %s); `autonomy plan show %s` shows its state",
				planID, awaiting.Approval.Revision, awaiting.Approval.Status, planID)
		}
		// An APPROVAL also names the assignment set it approves, and this path
		// has not resolved one - so the offered command names it as something
		// to fill in from `plan show` rather than printing a command that would
		// be refused. A rejection binds no assignments and needs none.
		set := ""
		if verb != "reject" {
			set = " --assignments <the assignments_digest `plan show` prints>"
		}
		return runtime.ExitInvalid, fmt.Errorf(
			"%s names the exact revision it decides: run `autonomy plan show %s` and use the command it prints (currently `autonomy plan %s %s --revision %d --digest %s%s`)",
			verb, planID, verb, planID, awaiting.Approval.Revision, awaiting.Approval.Digest, set)
	}
	// A DECISION about work a supervisor is executing goes to that supervisor.
	// It is the process that owns the work, so it applies the decision against
	// the state it is reconciling, in the order decisions arrive, rather than a
	// second process writing beside it. With no supervisor running, this
	// terminal is the owner and decides directly.
	command := runtime.ControlPlanApprove
	if verb == "reject" {
		command = runtime.ControlPlanReject
	}
	// A supervisor that is RUNNING but unreachable is not the same as no
	// supervisor. Falling through to the local path would apply a decision
	// beside a live reconciler, outside the lock that exists to stop exactly
	// that, because a socket dial happened to fail.
	// The decision records WHO made it, so the requester's own resolved
	// identity travels with the request rather than the supervisor recording
	// itself for a decision somebody else made.
	requester, err := resolveRequestingOperator(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	delegated, payload, sent, err := delegatePayloadSent(stateDir, runtime.ControlRequest{
		Command: command, PlanID: planID, Revision: revision, Digest: digest,
		AssignmentsDigest: strings.TrimSpace(flags.Assignments),
		Note:              flags.Note, Operator: requester,
	})
	if delegated {
		// Only a request that REACHED the supervisor can have been applied
		// without answering. One refused before any connection was made was
		// applied by nobody, and consulting the durable record for it would
		// report an unrelated earlier decision as this one's outcome.
		if err != nil && !sent {
			return runtime.ExitFailed, err
		}
		if err != nil {
			// The supervisor may have applied the decision and lost the reply -
			// a connection deadline, a restart. Reporting a bare failure for a
			// decision that WAS applied is the "effect without the answer" this
			// endpoint exists to avoid, so the durable record is consulted
			// before an operator is told it failed.
			return reportDelegatedDecision(flags, overrides, planID, verb, revision, digest, err, stdout)
		}
		return renderDelegatedPlan(flags, payload, true, stdout)
	}

	composed, err := buildPlanComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer composed.release()
	operator, err := composed.built.config.ResolveOperator()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	decide := composed.service.Approve
	if verb == "reject" {
		decide = composed.service.Reject
	}
	snapshot, err := decide(planID, revision, digest, strings.TrimSpace(flags.Assignments), operator.ID, flags.Note)
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
	reader, err := openPlanReader(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer reader.release()
	// Every plan IDENTITY, not only the ones that reached a revision. A plan
	// whose reasoning attempts were all refused has no document to join to, and
	// listing only documents is what answered "no plans" to an operator who had
	// just run the planner.
	identities, err := reader.store.PlanIdentities()
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
		// Attempts is how many reasoning proposals deterministic validation
		// refused for this plan, and Executable states whether an approvable
		// EngineeringPlan exists at all.
		Attempts   int  `json:"refused_attempts,omitempty"`
		Executable bool `json:"executable_plan_exists"`
	}
	summaries := make([]summary, 0, len(identities))
	for _, identity := range identities {
		snapshot, err := reader.store.ReplayPlan(identity.PlanID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		item := summary{
			PlanID: identity.PlanID, Approval: string(snapshot.Approval.Status),
			Stages: map[string]int{}, Consumed: snapshot.Consumed,
			Attempts: len(snapshot.Attempts), Executable: identity.Revision > 0,
		}
		for _, projection := range snapshot.Stages {
			item.Stages[string(projection.State)]++
		}
		if identity.Revision > 0 {
			plan, found, err := reader.store.PlanRevision(identity.PlanID, identity.Revision)
			if err != nil {
				return runtime.ExitFailed, err
			}
			if found {
				item.Revision, item.Objective = plan.Revision, plan.Objective
			}
		}
		summaries = append(summaries, item)
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
		if !item.Executable {
			// There is no revision, so there is no revision number and no
			// objective to print: naming one would name a document that does
			// not exist. What IS true is that planning was attempted and
			// refused, and that is what the row says.
			fmt.Fprintf(stdout, "%s  --  %-9s  no plan: %d refused reasoning attempt(s), read with `autonomy plan show %s`\n",
				item.PlanID, "unplanned", item.Attempts, item.PlanID)
			continue
		}
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
	fmt.Fprintf(stdout, "plan %s revision %d (%s)\n", view.Plan.ID, view.Plan.Revision, revisionStatus(view))
	// A revision that is not governing is shown as a PREVIEW: the state beside
	// each stage is what approving this revision would leave, not a report of
	// what is happening. Saying so is the difference between an approval
	// preview and a document decorated with another revision's execution.
	if preview := view.Preview; preview != nil {
		// A REJECTED revision is not awaiting approval, and offering "what
		// approving this would leave" beside a `(rejected)` header describes a
		// decision nobody is being asked for. The state shown is the same
		// hypothetical either way; what changes is whether it is on offer.
		if revisionStatus(view) == domain.ApprovalRejected {
			fmt.Fprintf(stdout, "preview: revision %d governs the work; this revision was rejected, and the state below is what it would have left\n",
				preview.GoverningRevision)
		} else {
			fmt.Fprintf(stdout, "preview: revision %d governs the work; the state below is what approving this revision would leave\n",
				preview.GoverningRevision)
		}
		if len(preview.Invalidated) > 0 {
			fmt.Fprintf(stdout, "approving would redo: %s\n", strings.Join(preview.Invalidated, ", "))
		}
	}
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
	// What the planner was actually given. A referenced issue the forge could
	// not return means this plan was reasoned from less than the engineering
	// input the primary issue names, and an operator deciding whether to
	// approve it needs to know that BEFORE they approve it.
	for _, reference := range view.Snapshot.References {
		if reference.Issue == 0 {
			fmt.Fprintf(stdout, "context: %s\n", reference.Detail)
			continue
		}
		if reference.Available {
			fmt.Fprintf(stdout, "context: %s issue #%d pinned at %s\n", reference.Repository, reference.Issue, short(reference.Digest))
			continue
		}
		fmt.Fprintf(stdout, "context: %s issue #%d WAS NOT AVAILABLE to the planner: %s\n",
			reference.Repository, reference.Issue, reference.Detail)
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
		// WHICH EXECUTION of this stage this is. A stage performed again
		// because its input moved is ordinary and automatic, and without this
		// the only record of that churn was the snapshot JSON.
		if projection, ok := view.Snapshot.Stages[stage.ID]; ok && projection.Generation > 0 {
			line += fmt.Sprintf(" execution %d", projection.Generation+1)
		}
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
		// The assignments digest is only printed where the view rendered IS
		// the revision awaiting the decision. Naming the set from a different
		// revision's view would refuse every approval.
		assignments := ""
		if awaiting.Revision == view.Plan.Revision && view.AssignmentsDigest != "" {
			assignments = " --assignments " + view.AssignmentsDigest
		}
		fmt.Fprintf(stdout, "nothing executes until it is approved: `autonomy plan approve %s --revision %d --digest %s%s`\n",
			view.Plan.ID, awaiting.Revision, awaiting.Digest, assignments)
	}
	return planExit(view), nil
}

// revisionStatus is the decision status OF THE RENDERED REVISION, which is not
// always the plan's latest decision: while a proposal waits, the approved
// revision is still approved, and printing the proposal's "pending" beside the
// governing revision's number said the executing plan was unapproved.
func revisionStatus(view runtime.PlanView) domain.ApprovalStatus {
	revision := view.Plan.Revision
	if approved, ok := view.Snapshot.ApprovedRevision(); ok && approved == revision {
		return domain.ApprovalApproved
	}
	if view.Snapshot.Rejected[revision] {
		return domain.ApprovalRejected
	}
	// A revision that GOVERNED and was replaced is superseded, not pending. It
	// was approved once, it ran, and calling it "pending" describes a revision
	// awaiting a decision nobody is being asked for.
	for _, governed := range view.Snapshot.GoverningHistory() {
		if governed == revision {
			return domain.ApprovalStatus("superseded")
		}
	}
	if view.Snapshot.Approval.Revision == revision {
		return view.Snapshot.Approval.Status
	}
	// A revision nobody has decided and that never governed is only "pending"
	// if it was validated; otherwise its verdict is what to say about it.
	if verdict, ok := view.Snapshot.Validations[revision]; ok && verdict.Status == domain.ProposalRefused {
		return domain.ApprovalStatus("refused")
	}
	return domain.ApprovalPending
}

// writePlanBudget prints the envelope beside what has been consumed. Unknown
// cost is printed as unknown: rendering "not reported" as zero would tell an
// operator something nobody measured.
func writePlanBudget(stdout io.Writer, view runtime.PlanView) {
	fmt.Fprintf(stdout, "budget: child runs %d/%d, provider invocations %d/%d, concurrency ceiling %d\n",
		view.Consumed.ChildRuns, view.Envelope.MaxChildRuns,
		view.Consumed.ProviderInvocations, view.Envelope.MaxProviderInvocations,
		view.Envelope.MaxConcurrency)
	// RE-PERFORMANCE HEADROOM, said out loud. A stage whose upstream candidate
	// is replaced is performed again as a new execution generation, and that
	// needs a child run - so an envelope with exactly one child run per agent
	// stage is legal, approvable, and blocks on budget the first time anything
	// upstream moves. An operator reading the envelope cannot see that from the
	// numbers alone.
	agentStages := 0
	for _, stage := range view.Plan.Stages {
		if stage.Kind == domain.StageAgent {
			agentStages++
		}
	}
	if agentStages > 0 && view.Envelope.MaxChildRuns <= agentStages {
		fmt.Fprintf(stdout, "no re-performance headroom: %d agent stages and %d child runs, so a stage whose input moves blocks on budget until a revision raises max_child_runs\n",
			agentStages, view.Envelope.MaxChildRuns)
	}
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

// singleLinePlan is one line of an objective, truncated by RUNES. The objective
// carries issue text, so cutting bytes can split a multi-byte character and
// print a broken rune into the operator's terminal.
func singleLinePlan(text string) string {
	line := strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
	if runes := []rune(line); len(runes) > 120 {
		return string(runes[:117]) + "..."
	}
	return line
}
