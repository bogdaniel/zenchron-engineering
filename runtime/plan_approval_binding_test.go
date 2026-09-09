package runtime

// Approval binds the assignments it was given.
//
// The operator approval surface shows resolved AgentAssignments, and resolution
// was recomputed from the live registry and workforce on every look. An
// unstarted stage therefore resolved AGAIN when it finally became
// dependency-ready, so an edited profile, an edited instruction pack, an edited
// context policy or a changed default worker between the approval and the first
// run meant generation 0 froze and executed a configuration nobody had
// approved.
//
// What the operator sees as the assignment when approving is what execution is
// authorized to use.

import (
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// The profile an approval bound is the profile that executes, even after the
// operator edits the one it names.
//
// The instruction pack is the sharpest case: the profile document's id, version
// and digest are unchanged by editing a pack it references, so nothing about
// the profile itself moves. Only the pack's own content digest does - and that
// digest is what the assignment freezes, what the runtime checks before it
// hands a worker any instruction text, and what a re-resolution after the
// approval would have replaced.
func TestApprovalBindsTheAssignmentTheOperatorSaw(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			Profile: "zenchron-reviewer", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	// An operator-defined reviewer profile over the registered `claude` worker,
	// naming one instruction pack by id. The pack's CONTENT digest is resolved
	// and frozen separately from the profile document.
	dir := t.TempDir()
	writeRegistryFile(t, dir, "instructions/review.json", `{"instructions": ["Review the diff; do not implement."]}`)
	writeRegistryFile(t, dir, "profiles/zenchron-reviewer.json", `{
	  "execution_agent": "claude", "capabilities": ["repository_analysis", "security_review", "verification"],
	  "instructions": ["review"]
	}`)
	useRegistry(t, fixture, dir)

	// What the operator sees when they approve.
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	shown, ok := assignmentFor(view.Assigned, "review")
	if !ok {
		t.Fatalf("the approval surface showed no assignment for the review stage: %#v", view.Blocked)
	}
	if shown.Profile.ID != "zenchron-reviewer" || len(shown.Profile.Instructions) != 1 {
		t.Fatalf("the review stage did not resolve onto the operator's profile: %#v", shown.Profile)
	}
	fixture.approve(t)

	// The binding is durable, and the approval names it.
	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bound["review"]; !ok {
		t.Fatalf("the approval bound no assignment for the review stage: %#v", bound)
	}
	if bound["review"].Profile.ID != shown.Profile.ID || bound["review"].Agent != shown.Agent {
		t.Fatalf("the approval bound something other than what was shown:\n%#v\n%#v", shown, bound["review"])
	}
	digest, err := fixture.store.ApprovedAssignmentsDigest(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("the approval recorded no assignment-set digest")
	}
	if journalled := approvalAssignmentsDigest(t, fixture); journalled != digest {
		t.Fatalf("the approval event names assignment set %q and the stored set digests to %q", journalled, digest)
	}

	// The operator edits the pack IN PLACE, after approving. The profile
	// document is untouched; only what the worker would be told has moved.
	writeRegistryFile(t, dir, "instructions/review.json", `{"instructions": ["Rewrite the whole module."]}`)
	useRegistry(t, fixture, dir)
	if edited, ok := assignmentFor(liveResolution(t, fixture), "review"); !ok {
		t.Fatal("the edited registry resolves nothing for the review stage")
	} else if packSummary(edited.Profile.Instructions) == packSummary(shown.Profile.Instructions) {
		t.Fatalf("the edit did not move what a re-resolution would produce, so this test proves nothing: %#v", edited.Profile.Instructions)
	}

	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")
	fixture.reconcile(t)

	executed, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "review")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the review stage started without freezing an assignment")
	}
	if executed.Profile.ID != shown.Profile.ID ||
		executed.Profile.Version != shown.Profile.Version ||
		executed.Profile.Digest != shown.Profile.Digest ||
		packSummary(executed.Profile.Instructions) != packSummary(shown.Profile.Instructions) ||
		packSummary(contextPolicyRefs(executed.Profile)) != packSummary(contextPolicyRefs(shown.Profile)) ||
		executed.Agent != shown.Agent {
		t.Fatalf("execution froze a configuration the operator never approved:\napproved %#v\nexecuted %#v", shown, executed)
	}
	// The upstream candidate, which the approval could not know, IS bound at
	// start: an approval that froze the empty upstream would hand an
	// independent reviewer nothing to review.
	if len(executed.Context.UpstreamOutputs) == 0 || executed.Context.UpstreamOutputs[0].Candidate != "aaaaaaaaaaaa" {
		t.Fatalf("the dependency-delayed stage did not bind the settled upstream candidate: %#v", executed.Context.UpstreamOutputs)
	}
}

// Changing the workforce or the operator's default between the approval and the
// first run does not silently move the work to another worker.
func TestApprovalBindsTheWorkerAndADefaultChangeDoesNotMoveIt(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	shown, ok := assignmentFor(view.Assigned, "implementation")
	if !ok {
		t.Fatal("the approval surface showed no assignment")
	}
	if shown.Agent.ID != "codex" {
		t.Fatalf("the fixture's default resolved %q, and this test needs the default to be codex", shown.Agent.ID)
	}
	fixture.approve(t)

	// The operator changes their default worker before anything runs.
	fixture.service.DefaultAgent = "claude"
	fixture.reconciler.Service = fixture.service
	fixture.reconcile(t)

	executed, found, err := fixture.store.PlanAssignment(fixture.plan.ID, fixture.plan.Revision, 0, "implementation")
	if err != nil || !found {
		t.Fatalf("the stage started without freezing an assignment: found=%v err=%v", found, err)
	}
	if executed.Agent != shown.Agent {
		t.Fatalf("a default change moved approved work from %#v to %#v", shown.Agent, executed.Agent)
	}
	if len(fixture.engineCalls) == 0 || fixture.engineCalls[len(fixture.engineCalls)-1] != shown.Agent.ID {
		t.Fatalf("the engine was built for %v, want the approved worker %s", fixture.engineCalls, shown.Agent.ID)
	}
	// And the operator's view says the same thing the runtime will do, rather
	// than describing the worker that would be chosen today.
	after, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rendered, _ := assignmentFor(after.Assigned, "implementation"); rendered.Agent != shown.Agent {
		t.Fatalf("the operator is shown worker %s while %s is authorized", rendered.Agent.ID, shown.Agent.ID)
	}
}

// A revision that materially CHANGES a started stage binds that stage too.
//
// This is the ordinary flow - revise a plan while it is executing - and it was
// the hole the first version of this boundary left. The supersession an
// approval records resets an invalidated stage to a zero projection: lifecycle
// cleared, generation back to zero, no assignment. So reading the pre-approval
// snapshot saw the stage as "already started" and left it unbound, and then
// nothing governed it at all - the privilege comparison engages only above
// generation zero, and the supersession had just taken it back to zero. The
// changed stage live-resolved from whatever the registry said at first run.
//
// The binding is taken against the state approving WOULD leave, which is the
// same computation `plan show --revision N` renders.
func TestARevisionThatChangesAStartedStageBindsItToo(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	started, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Stages["implementation"].AssignmentID == "" {
		t.Fatal("this test needs the stage to have STARTED under revision 1, and it did not")
	}

	// Revision 2 changes that stage, so approving it discards the work and
	// performs the stage again.
	second := fixture.plan
	second.Revision = 2
	previous := fixture.plan.Revision
	second.Provenance.PreviousRevision = &previous
	second.Stages = append([]domain.PlanStage(nil), fixture.plan.Stages...)
	second.Stages[0].Objective = "Do the work, differently."
	digest, digestErr := second.ContentDigest()
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	second.Digest = digest
	if _, err := fixture.store.PutPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutPlanContract(second.ID, second.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}

	// WHAT THE OPERATOR IS SHOWN when they read the proposal: the preview,
	// which resolves the stage this revision will redo rather than decorating
	// it with the previous revision's performance.
	preview, err := fixture.service.ViewRevision(fixture.plan.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	shown, ok := assignmentFor(preview.Assigned, "implementation")
	if !ok {
		t.Fatalf("the preview showed no assignment for the stage it will redo: %#v", preview.Blocked)
	}
	if _, err := fixture.service.Approve(second.ID, second.Revision, second.Digest, "", "operator", ""); err != nil {
		t.Fatal(err)
	}
	fixture.plan = second

	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bound["implementation"]; !ok {
		t.Fatalf("the revision that redoes this stage bound nothing for it: %#v", bound)
	}

	// The operator's default moves before the replacement performance starts.
	fixture.service.DefaultAgent = "claude"
	fixture.reconciler.Service = fixture.service
	fixture.reconcile(t)

	executed, found, err := fixture.store.PlanAssignment(fixture.plan.ID, 2, 0, "implementation")
	if err != nil || !found {
		t.Fatalf("the replacement performance froze nothing: found=%v err=%v", found, err)
	}
	if executed.Agent != shown.Agent {
		t.Fatalf("the replacement executed worker %#v and the approval surface showed %#v", executed.Agent, shown.Agent)
	}
	if len(fixture.engineCalls) == 0 || fixture.engineCalls[len(fixture.engineCalls)-1] != shown.Agent.ID {
		t.Fatalf("the engine was built for %v, want the approved worker %s", fixture.engineCalls, shown.Agent.ID)
	}
}

// An agent id re-pointed at a different provider is refused rather than
// executed under the approved worker's name.
//
// This is the one governance identity the assignment cannot enforce by carrying
// it: the engine is built from live agent configuration, so a `codex` that is
// now a Claude adapter would run anthropic work under an independence
// obligation that says openai.
func TestAWorkerRepointedAtAnotherProviderIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)

	drifted := planAgents()
	for i := range drifted {
		if drifted[i].ID == "codex" {
			drifted[i].ProviderKind = "claude_code"
			drifted[i].VendorFamily = "anthropic"
		}
	}
	fixture.service.Agents = drifted
	fixture.reconciler.Service = fixture.service

	report := fixture.reconcile(t)
	blocked := false
	for _, block := range report.Blocked {
		if block.StageID != "implementation" {
			continue
		}
		blocked = true
		if block.Kind != "authority" {
			t.Fatalf("a re-pointed worker blocked as %q: %s", block.Kind, block.Reason)
		}
		if !strings.Contains(block.Reason, "provider kind") && !strings.Contains(block.Reason, "vendor family") {
			t.Fatalf("the refusal does not name what drifted: %s", block.Reason)
		}
	}
	if !blocked {
		t.Fatal("a stage ran on a worker that had been re-pointed at another provider")
	}
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Stages["implementation"].RunID != "" {
		t.Fatalf("the refused stage started run %s anyway", snapshot.Stages["implementation"].RunID)
	}
}

// A decision may NAME the assignment set it decides, and then it decides that
// set or nothing.
//
// The revision digest binds the plan document an operator read. It does not
// move when a profile, an instruction pack or the workforce is edited, so an
// edit landing between `plan show` and `plan approve` was bound as "what the
// operator saw" - true only in the sense that nobody looked again. The set
// digest the view prints closes that the same way the revision digest closed
// deciding a document nobody read.
func TestADecisionMayNameTheAssignmentSetItDecides(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.AssignmentsDigest == "" {
		t.Fatal("the approval surface printed no assignment set to name")
	}

	// The operator's default moves between reading and deciding.
	fixture.service.DefaultAgent = "claude"
	_, err = fixture.service.Approve(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest,
		view.AssignmentsDigest, "operator", "")
	var refused *PlanRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a decision naming a set that had moved was applied: %v", err)
	}
	if !strings.Contains(refused.Detail, "who would perform this work has changed") {
		t.Fatalf("the refusal does not say what moved: %s", refused.Detail)
	}
	if bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision); err != nil {
		t.Fatal(err)
	} else if len(bound) != 0 {
		t.Fatalf("the refused decision bound something anyway: %#v", bound)
	}

	// Read it again, decide on what is there now: accepted, and bound to it.
	after, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AssignmentsDigest == view.AssignmentsDigest {
		t.Fatal("the edit did not move the set, so this test proves nothing")
	}
	if _, err := fixture.service.Approve(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest,
		after.AssignmentsDigest, "operator", ""); err != nil {
		t.Fatalf("a decision naming the current set was refused: %v", err)
	}
	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if bound["implementation"].Agent.ID != "claude" {
		t.Fatalf("the approval bound %q and the operator read claude", bound["implementation"].Agent.ID)
	}
}

// The named set means WHO performs the work under WHAT configuration, and not
// the candidate they will consume.
//
// A producer settling between reading a proposal and deciding on it moves the
// upstream a dependent assignment carries. That is an execution fact the
// approval does not bind - it is rebound from the settled producer when the
// stage starts - so a digest that moved with it would refuse the decision while
// saying that who would perform the work had changed. It had not.
func TestTheNamedSetDoesNotMoveWithTheUpstreamCandidate(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review it.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification}},
	})
	fixture.approve(t)
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	implementation := snapshot.Stages["implementation"].RunID
	if implementation == "" {
		t.Fatal("the implementation stage created no run")
	}

	// A second revision, read while the producer has settled nothing.
	second := approveableRevision(t, fixture)
	before, err := fixture.service.ViewRevision(fixture.plan.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}

	// The producer settles while the operator is reading.
	recordCandidateAndAssurance(t, fixture, implementation, "aaaaaaaaaaaa")
	settleRunAtGoalState(t, fixture, implementation, "aaaaaaaaaaaa")

	after, err := fixture.service.ViewRevision(fixture.plan.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := assignmentFor(after.Assigned, "review"); len(resolved.Context.UpstreamOutputs) == 0 {
		t.Fatal("this test needs the producer to have moved what the reviewer would consume, and it did not")
	}
	if after.AssignmentsDigest != before.AssignmentsDigest {
		t.Fatalf("the named set moved with the upstream candidate: %s becomes %s", before.AssignmentsDigest, after.AssignmentsDigest)
	}
	// And the decision the operator was offered still applies.
	if _, err := fixture.service.Approve(second.ID, second.Revision, second.Digest,
		before.AssignmentsDigest, "operator", ""); err != nil {
		t.Fatalf("a decision naming the set that was read was refused because a producer settled: %v", err)
	}
}

// A rejection binds nothing, so it cannot name a set to bind. Accepting the
// argument and ignoring it would report a check nobody performed.
func TestARejectionCannotNameAnAssignmentSet(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.Reject(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest,
		view.AssignmentsDigest, "operator", "")
	var refused *PlanRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a rejection naming an assignment set was accepted: %v", err)
	}
	if !strings.Contains(refused.Detail, "binds no assignments") {
		t.Fatalf("the refusal does not say why: %s", refused.Detail)
	}
}

// "Nothing is bindable here" is itself a set an operator can name, so a set
// that APPEARS between reading and deciding is refused like any other move.
func TestAnEmptyNamedSetIsPinnable(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", Profile: "not-installed",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Assigned) != 0 {
		t.Fatal("this test needs the plan to resolve nothing, and it resolved something")
	}
	if view.AssignmentsDigest == "" {
		t.Fatal("a set of nothing has no digest, so it cannot be named")
	}

	// The blocker clears before the decision: a set has appeared where the
	// operator read none.
	dir := t.TempDir()
	writeRegistryFile(t, dir, "profiles/not-installed.json", `{
	  "execution_agent": "claude", "capabilities": ["repository_analysis", "code_change"]
	}`)
	useRegistry(t, fixture, dir)

	_, err = fixture.service.Approve(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest,
		view.AssignmentsDigest, "operator", "")
	var refused *PlanRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a set that appeared after the operator read none was bound unchecked: %v", err)
	}
	if !strings.Contains(refused.Detail, "who would perform this work has changed") {
		t.Fatalf("the refusal does not say what moved: %s", refused.Detail)
	}
}

// approveableRevision stores a second revision carrying every stage forward,
// so a test can read and decide one while the first is executing.
func approveableRevision(t *testing.T, fixture *planRunFixture) domain.EngineeringPlan {
	t.Helper()
	previous := fixture.plan.Revision
	next := fixture.plan
	next.Revision = previous + 1
	next.Provenance.PreviousRevision = &previous
	next.Objective = fixture.plan.Objective + " Again."
	digest, err := next.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	next.Digest = digest
	if _, err := fixture.store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutPlanContract(next.ID, next.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
		t.Fatal(err)
	}
	return next
}

// A stage that showed a BLOCKER is bound to nothing, and the view says so
// rather than rendering it later as though an approval had covered it.
func TestAStageThatShowedABlockerIsReportedUnbound(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", Profile: "not-installed",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	view, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Blocked) == 0 {
		t.Fatal("this test needs the stage to be BLOCKED at approval, and it resolved")
	}
	if len(view.Unbound) != 1 || view.Unbound[0] != "implementation" {
		t.Fatalf("the view does not report the stage as unbound: %#v", view.Unbound)
	}
	fixture.approve(t)
	after, err := fixture.service.View(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Unbound) != 1 || after.Unbound[0] != "implementation" {
		t.Fatalf("after approval the unbound stage is no longer reported: %#v", after.Unbound)
	}
}

// A revision approved before this boundary existed has no binding and resolves
// live, exactly as it did then. Historical durable state stays operable.
func TestARevisionApprovedWithoutABindingStillResolves(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	// The approval as an older binary wrote it: the event, and nothing else.
	if err := fixture.service.appendPlanEvent(fixture.plan.ID, EventPlanApproved, PlanDecisionPayload{
		Revision: fixture.plan.Revision, Digest: fixture.plan.Digest, Operator: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	if bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision); err != nil {
		t.Fatal(err)
	} else if len(bound) != 0 {
		t.Fatalf("a historical approval was given a binding it never had: %#v", bound)
	}
	fixture.reconcile(t)
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Stages["implementation"].RunID == "" {
		t.Fatal("a plan approved before the binding existed no longer executes")
	}
}

// The binding is immutable. Re-approving the same revision after the registry
// has moved does not rewrite what the first approval bound.
func TestARebindingOfAnApprovedRevisionIsRefused(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)
	bound, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	other := bound["implementation"]
	other.Agent.ID = "claude"
	if _, err := fixture.store.PutApprovedAssignments(fixture.plan.ID, fixture.plan.Revision,
		[]domain.AgentAssignment{other}); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("an approval's assignments were rewritten: %v", err)
	}
	again, err := fixture.store.ApprovedAssignments(fixture.plan.ID, fixture.plan.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if again["implementation"].Agent.ID != bound["implementation"].Agent.ID {
		t.Fatalf("the binding moved to %q", again["implementation"].Agent.ID)
	}
}

func assignmentFor(assignments []domain.AgentAssignment, stageID string) (domain.AgentAssignment, bool) {
	for _, assignment := range assignments {
		if assignment.StageID == stageID {
			return assignment, true
		}
	}
	return domain.AgentAssignment{}, false
}

// approvalAssignmentsDigest is the digest the approval EVENT carries, read back
// out of the journal rather than out of the table it describes.
func approvalAssignmentsDigest(t *testing.T, fixture *planRunFixture) string {
	t.Helper()
	events, err := fixture.store.PlanEvents(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventPlanApproved {
			continue
		}
		payload, err := decodePayload[PlanDecisionPayload](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if payload.Revision == fixture.plan.Revision {
			return payload.AssignmentsDigest
		}
	}
	t.Fatal("no approval event was recorded")
	return ""
}

// useRegistry points the fixture's service AND its reconciler at the operator
// artifacts in one directory. Both hold the service by value, so setting one
// leaves the other resolving against the registry it was built with.
func useRegistry(t *testing.T, fixture *planRunFixture, dir string) {
	t.Helper()
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Registry = registry
	fixture.reconciler.Service = fixture.service
}

// liveResolution is what the resolver would produce for the approved revision
// from the registry as it stands now, ignoring what the approval bound. It
// exists so a test can prove the operator's edit actually moved something.
func liveResolution(t *testing.T, fixture *planRunFixture) []domain.AgentAssignment {
	t.Helper()
	contract, found, err := fixture.store.PlanContract(fixture.plan.ID, fixture.plan.Revision)
	if err != nil || !found {
		t.Fatalf("read the plan contract: found=%v err=%v", found, err)
	}
	resolution, err := planning.Resolve(planning.ResolveInput{
		Plan: fixture.plan, Registry: fixture.service.Registry, Agents: fixture.service.Agents,
		Contract: contract, DefaultAgent: fixture.service.DefaultAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolution.Assignments
}
