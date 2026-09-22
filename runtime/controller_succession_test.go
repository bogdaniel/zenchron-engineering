package runtime

import (
	"fmt"
	"strings"
	"testing"
)

const (
	predecessorRevision = "1111111111111111111111111111111111111111"
	successorRevision   = "2222222222222222222222222222222222222222"
	strangerRevision    = "3333333333333333333333333333333333333333"
)

func adoptedBinding(revision, tree string, config ConfigDigest) ControllerBinding {
	build := attestedBuild(ControllerAdopted, revision, tree, strings.Repeat("ab", 32))
	return ControllerBinding{Controller: "zenchron-engineering", Build: &build, Config: config}
}

// runFor is the durable row a decision is checked against: the digest is the
// predecessor's, exactly as a real run records it.
func runFor(t *testing.T, binding ControllerBinding) EngineeringRun {
	t.Helper()
	digest, err := binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return EngineeringRun{ID: "run-succession", ControllerSHA256: digest}
}

func ancestorAlways(string, string) (bool, error) { return true, nil }

// THE DIGEST HAS EXACTLY ONE DEFINITION. Every run on disk records the digest
// the runtime composes at construction, so a succession decision that
// recomputed a different document would silently refuse every real run.
func TestControllerBindingReproducesTheRuntimeDigest(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.ControllerBuild = attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	rt, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	build := deps.ControllerBuild
	recomputed, err := ControllerBinding{
		Controller: deps.ControllerID, Build: &build, Config: deps.ConfigDigest,
	}.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != rt.controller {
		t.Fatalf("recomputed %s, runtime composed %s", shortSHA(recomputed), shortSHA(rt.controller))
	}
}

func TestControllerSuccessionDecisionTable(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	predecessor := adoptedBinding(predecessorRevision, "tree-a", config)
	successor := adoptedBinding(successorRevision, "tree-b", config)
	trusted := RevisionRecord{Revision: successorRevision, Tree: "tree-b"}
	unattested := ControllerBinding{Controller: "zenchron-engineering", Config: config}

	for _, test := range []struct {
		name    string
		mutate  func(*ControllerSuccessionInput)
		want    ControllerSuccessionResult
		refusal string
	}{
		{"a strict adopted descendant on unchanged configuration", nil, SuccessionCompatible, ""},
		{"a predecessor the run does not record", func(in *ControllerSuccessionInput) {
			in.Predecessor = adoptedBinding(strangerRevision, "tree-x", config)
		}, SuccessionRefused, "source_lineage"},
		{"an unattested predecessor", func(in *ControllerSuccessionInput) {
			in.Predecessor = unattested
			in.Run = runFor(t, unattested)
		}, SuccessionRefused, "source_lineage"},
		{"an unattested successor", func(in *ControllerSuccessionInput) {
			in.Successor = unattested
		}, SuccessionRefused, "source_lineage"},
		{"a successor built from the same revision", func(in *ControllerSuccessionInput) {
			in.Successor = adoptedBinding(predecessorRevision, "tree-a", config)
			in.TrustedMain = RevisionRecord{Revision: predecessorRevision, Tree: "tree-a"}
		}, SuccessionRefused, "source_lineage"},
		{"a successor that does not descend from the predecessor", func(in *ControllerSuccessionInput) {
			in.IsAncestor = func(string, string) (bool, error) { return false, nil }
		}, SuccessionRefused, "source_lineage"},
		{"a lineage nobody can prove", func(in *ControllerSuccessionInput) {
			in.IsAncestor = func(string, string) (bool, error) { return false, fmt.Errorf("no repository") }
		}, SuccessionRefused, "source_lineage"},
		{"no lineage prover at all", func(in *ControllerSuccessionInput) {
			in.IsAncestor = nil
		}, SuccessionRefused, "source_lineage"},
		{"a successor that is not trusted main", func(in *ControllerSuccessionInput) {
			in.TrustedMain = RevisionRecord{Revision: strangerRevision, Tree: "tree-b"}
		}, SuccessionRefused, "trust_root"},
		{"a successor whose tree is not trusted main's", func(in *ControllerSuccessionInput) {
			in.TrustedMain = RevisionRecord{Revision: successorRevision, Tree: "tree-other"}
		}, SuccessionRefused, "trust_root"},
		{"no observed trusted main", func(in *ControllerSuccessionInput) {
			in.TrustedMain = RevisionRecord{}
		}, SuccessionRefused, "trust_root"},
		{"a changed configuration", func(in *ControllerSuccessionInput) {
			in.Successor = adoptedBinding(successorRevision, "tree-b", ConfigDigest{Global: "config-b"})
		}, SuccessionRefused, "configuration"},
		{"an event type this controller does not implement", func(in *ControllerSuccessionInput) {
			in.Events = []EngineeringEvent{{Type: "run.invented_by_a_newer_controller"}}
		}, SuccessionRefused, "state_vocabulary"},
		{"a journal that does not replay", func(in *ControllerSuccessionInput) {
			in.Events = []EngineeringEvent{{Type: EventRunCreated, RunID: "some-other-run", Sequence: 1}}
		}, SuccessionRefused, "durable_replay"},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := ControllerSuccessionInput{
				Run: runFor(t, predecessor), Predecessor: predecessor, Successor: successor,
				TrustedMain: trusted, IsAncestor: ancestorAlways,
			}
			if test.mutate != nil {
				test.mutate(&in)
			}
			decision := EvaluateControllerSuccession(in)
			if decision.Result != test.want {
				t.Fatalf("result = %q, want %q (refusals %v)", decision.Result, test.want, decision.Refusals())
			}
			if test.refusal == "" {
				if len(decision.Refusals()) != 0 {
					t.Fatalf("a compatible decision carries refusals: %v", decision.Refusals())
				}
				return
			}
			var named bool
			for _, refusal := range decision.Refusals() {
				if strings.HasPrefix(refusal, test.refusal+":") {
					named = true
				}
			}
			if !named {
				t.Fatalf("refusals %v do not name %q", decision.Refusals(), test.refusal)
			}
		})
	}
}

// admission builds the event a compatible decision produces, without a store,
// so the chain rule can be driven over exact shapes.
// everyHandoffActivated is the oracle for tests about the CHAIN rather than
// about the epoch: it says every transition activated, so a case can isolate
// the question it is actually asking. The epoch tests supply their own.
func everyHandoffActivated(string) bool { return true }

func admission(t *testing.T, from, to ControllerBinding) EngineeringEvent {
	t.Helper()
	decision := ControllerSuccessionDecision{
		RunID: "run-succession", HandoffID: "handoff-fixture", Predecessor: from, Successor: to,
		SourceLineage: passed("fixture"), TrustRoot: passed("fixture"), Configuration: passed("fixture"),
		StateVocabulary: passed("fixture"), DurableReplay: passed("fixture"),
	}
	decision.settle()
	payload, err := marshalPayloadJSON(decision)
	if err != nil {
		t.Fatal(err)
	}
	return EngineeringEvent{Type: EventControllerSuccessionAdmitted, Payload: payload}
}

func TestControllerSuccessionChain(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	a := adoptedBinding(predecessorRevision, "tree-a", config)
	b := adoptedBinding(successorRevision, "tree-b", config)
	c := adoptedBinding(strangerRevision, "tree-c", config)
	digest := func(binding ControllerBinding) string {
		value, err := binding.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	run := runFor(t, a)

	for _, test := range []struct {
		name       string
		events     []EngineeringEvent
		controller string
		want       bool
	}{
		{"the controller that created it", nil, digest(a), true},
		{"a different controller with no admission", nil, digest(b), false},
		{"an admitted successor", []EngineeringEvent{admission(t, a, b)}, digest(b), true},
		{"a controller two steps along an admitted chain",
			[]EngineeringEvent{admission(t, a, b), admission(t, b, c)}, digest(c), true},
		// The chain is followed rather than searched: an admission whose
		// predecessor is not where the run stands is not a shortcut into it.
		{"an admission that skips a step", []EngineeringEvent{admission(t, b, c)}, digest(c), false},
		{"the predecessor after an admitted succession",
			[]EngineeringEvent{admission(t, a, b)}, digest(a), true},
		{"a refused decision admits nothing", []EngineeringEvent{func() EngineeringEvent {
			event := admission(t, a, b)
			var decision ControllerSuccessionDecision
			if err := decodeJSON(event.Payload, &decision); err != nil {
				t.Fatal(err)
			}
			decision.Configuration = refusedCheck("the configuration changed")
			decision.settle()
			payload, err := marshalPayloadJSON(decision)
			if err != nil {
				t.Fatal(err)
			}
			event.Payload = payload
			return event
		}()}, digest(b), false},
		{"an unreadable admission", []EngineeringEvent{
			{Type: EventControllerSuccessionAdmitted, Payload: []byte(`{"predecessor":`)},
		}, digest(b), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ControllerSuccessionContinues(run, test.events, test.controller, everyHandoffActivated); got != test.want {
				t.Fatalf("continues = %v, want %v", got, test.want)
			}
		})
	}
}

// The journal is the only thing that changes. A successor inherits the right to
// append; it does not inherit authorship of what is already there.
func TestAdmittedSuccessionRewritesNoHistory(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.ControllerBuild = attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	predecessorRuntime, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = predecessorRuntime
	runID := fixture.start()

	before, ok, err := fixture.store.Run(runID)
	if err != nil || !ok {
		t.Fatalf("run %s: %v", runID, err)
	}
	eventsBefore, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}

	successorDeps := deps
	successorDeps.ControllerBuild = attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	successorRuntime, err := NewEngineeringRuntime(successorDeps)
	if err != nil {
		t.Fatal(err)
	}

	// The successor is the one that evaluates, against the state it would have
	// to read, exactly as the handoff preflight will.
	predecessorBuild, successorBuild := deps.ControllerBuild, successorDeps.ControllerBuild
	decision := EvaluateControllerSuccession(ControllerSuccessionInput{
		Run:         before,
		Events:      eventsBefore,
		Predecessor: ControllerBinding{Controller: deps.ControllerID, Build: &predecessorBuild, Config: deps.ConfigDigest},
		Successor:   ControllerBinding{Controller: successorDeps.ControllerID, Build: &successorBuild, Config: successorDeps.ConfigDigest},
		TrustedMain: RevisionRecord{Revision: successorRevision, Tree: "tree-b"},
		IsAncestor:  ancestorAlways,
	})
	if decision.Result != SuccessionCompatible {
		t.Fatalf("decision refused: %v", decision.Refusals())
	}
	// An admission is evidence for one transition. The record that would carry
	// it is written by the handoff protocol; here the transition is named
	// directly and treated as activated, so this test stays about history.
	decision.HandoffID = "handoff-history"

	// BEFORE the admission the successor is a stranger to this run.
	if state, err := successorRuntime.load(runID); err != nil {
		t.Fatal(err)
	} else if !state.controllerChanged {
		t.Fatal("an unadmitted successor was treated as the run's controller")
	}

	// THE TRANSITION THIS EVIDENCE BELONGS TO. Until it activates the admission
	// means nothing, which is the point of binding the two.
	successorDigest, err := decision.Successor.Digest()
	if err != nil {
		t.Fatal(err)
	}
	transition := ControllerHandoff{
		ID: decision.HandoffID, Phase: HandoffSuccessorAcquired,
		Predecessor:   HandoffParty{Binding: decision.Predecessor},
		Successor:     HandoffParty{Binding: decision.Successor},
		RecoveryOwner: before.ControllerSHA256, UpdatedAt: fixture.clock.Now(),
	}
	if wrote, err := fixture.store.PutControllerHandoff(transition, ""); err != nil || !wrote {
		t.Fatalf("record the transition: %v wrote=%v", err, wrote)
	}

	if err := predecessorRuntime.AdmitControllerSuccession(runID, decision); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a repeated upgrade request is ordinary, and idempotence must
	// not depend on the transition having activated.
	if err := predecessorRuntime.AdmitControllerSuccession(runID, decision); err != nil {
		t.Fatal(err)
	}
	// Written, and still inert: authority follows the transition.
	if state, err := successorRuntime.load(runID); err != nil {
		t.Fatal(err)
	} else if !state.controllerChanged {
		t.Fatal("an admission whose transition has not activated promoted the successor")
	}
	activated := transition
	activated.Phase, activated.RecoveryOwner = HandoffActivated, successorDigest
	if wrote, err := fixture.store.PutControllerHandoff(activated, HandoffSuccessorAcquired); err != nil || !wrote {
		t.Fatalf("activate the transition: %v wrote=%v", err, wrote)
	}

	after, ok, err := fixture.store.Run(runID)
	if err != nil || !ok {
		t.Fatalf("run %s: %v", runID, err)
	}
	if after.ControllerSHA256 != before.ControllerSHA256 {
		t.Fatalf("the run row was rewritten: %s -> %s",
			shortSHA(before.ControllerSHA256), shortSHA(after.ControllerSHA256))
	}
	eventsAfter, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore)+1 {
		t.Fatalf("admission appended %d event(s), want exactly 1", len(eventsAfter)-len(eventsBefore))
	}
	for i, event := range eventsBefore {
		if eventsAfter[i].EventHash != event.EventHash || eventsAfter[i].Type != event.Type {
			t.Fatalf("event %d was rewritten: %q -> %q", i, event.Type, eventsAfter[i].Type)
		}
	}

	// AFTER the admission the successor may drive it, and the predecessor is
	// still recorded as having performed everything up to here.
	state, err := successorRuntime.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if state.controllerChanged {
		t.Fatal("an admitted successor is still refused as a different controller")
	}
	if state.run.ControllerSHA256 != before.ControllerSHA256 {
		t.Fatal("the run stopped naming the controller that created it")
	}
}

// A refused decision is not evidence, and the journal must not be able to hold
// one: the admission event is the only thing that moves authority.
func TestRefusedSuccessionIsNeverAdmitted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.ControllerBuild = attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	rt, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = rt
	runID := fixture.start()

	build := deps.ControllerBuild
	binding := ControllerBinding{Controller: deps.ControllerID, Build: &build, Config: deps.ConfigDigest}
	refused := ControllerSuccessionDecision{
		RunID: runID, HandoffID: "handoff-refused", Predecessor: binding, Successor: binding,
		SourceLineage: refusedCheck("same revision"), TrustRoot: passed("x"), Configuration: passed("x"),
		StateVocabulary: passed("x"), DurableReplay: passed("x"),
	}
	refused.settle()
	if err := rt.AdmitControllerSuccession(runID, refused); err == nil {
		t.Fatal("a refused decision was admitted")
	}

	// Even hand-written, the payload schema refuses it at the journal boundary.
	payload, err := marshalPayloadJSON(refused)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload(EngineeringEvent{
		ID: runID + "-forced", RunID: runID, Type: EventControllerSuccessionAdmitted, Payload: payload,
	}); err == nil {
		t.Fatal("the journal accepted a refused succession")
	}
}

// AN ADMISSION IS EVIDENCE FOR ONE OCCASION. A transition that wrote admissions
// and then failed must not leave a standing capability behind, and a LATER
// transition between the same two generations must not be able to cash in the
// earlier one's evidence.
func TestAdmissionIsBoundToItsTransition(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	a := adoptedBinding(predecessorRevision, "tree-a", config)
	b := adoptedBinding(successorRevision, "tree-b", config)
	successor, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	run := runFor(t, a)

	first := admission(t, a, b)
	var decision ControllerSuccessionDecision
	if err := decodeJSON(first.Payload, &decision); err != nil {
		t.Fatal(err)
	}
	decision.HandoffID = "handoff-first"
	payload, err := marshalPayloadJSON(decision)
	if err != nil {
		t.Fatal(err)
	}
	first.Payload = payload

	// The first transition FAILED: its id never becomes activated.
	failed := func(id string) bool { return false }
	if ControllerSuccessionContinues(run, []EngineeringEvent{first}, successor, failed) {
		t.Fatal("a failed transition's admission still promotes the successor")
	}

	// A SECOND transition begins between the same two generations and
	// activates. The first transition's evidence is still not its evidence.
	secondOnly := func(id string) bool { return id == "handoff-second" }
	if ControllerSuccessionContinues(run, []EngineeringEvent{first}, successor, secondOnly) {
		t.Fatal("a later transition cashed in an earlier transition's admission")
	}

	// Its own admission, under its own id, is what promotes the successor.
	second := admission(t, a, b)
	if err := decodeJSON(second.Payload, &decision); err != nil {
		t.Fatal(err)
	}
	decision.HandoffID = "handoff-second"
	if payload, err = marshalPayloadJSON(decision); err != nil {
		t.Fatal(err)
	}
	second.Payload = payload
	if !ControllerSuccessionContinues(run, []EngineeringEvent{first, second}, successor, secondOnly) {
		t.Fatal("the second transition's own admission did not promote its successor")
	}

	// And an admission naming no transition at all grants nothing, whatever
	// the oracle says.
	unbound := admission(t, a, b)
	if err := decodeJSON(unbound.Payload, &decision); err != nil {
		t.Fatal(err)
	}
	decision.HandoffID = ""
	if payload, err = marshalPayloadJSON(decision); err != nil {
		t.Fatal(err)
	}
	unbound.Payload = payload
	if ControllerSuccessionContinues(run, []EngineeringEvent{unbound}, successor, everyHandoffActivated) {
		t.Fatal("an admission naming no transition promoted the successor")
	}
}

// Without an authority oracle the chain rule answers no. A caller that cannot
// say which transitions activated cannot be told that one did.
func TestSuccessionRefusesWithoutAnAuthorityOracle(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	a := adoptedBinding(predecessorRevision, "tree-a", config)
	b := adoptedBinding(successorRevision, "tree-b", config)
	successor, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	run := runFor(t, a)
	if ControllerSuccessionContinues(run, []EngineeringEvent{admission(t, a, b)}, successor, nil) {
		t.Fatal("succession was granted with no way to check the transition")
	}
	// The run's own creator never needs the oracle.
	predecessor, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !ControllerSuccessionContinues(run, nil, predecessor, nil) {
		t.Fatal("a run stopped being continuable by the controller that created it")
	}
}

// The journal refuses evidence that names no transition.
func TestAdmissionPayloadRequiresATransition(t *testing.T) {
	config := ConfigDigest{Global: "config-a"}
	a := adoptedBinding(predecessorRevision, "tree-a", config)
	b := adoptedBinding(successorRevision, "tree-b", config)
	event := admission(t, a, b)
	var decision ControllerSuccessionDecision
	if err := decodeJSON(event.Payload, &decision); err != nil {
		t.Fatal(err)
	}
	decision.HandoffID = ""
	payload, err := marshalPayloadJSON(decision)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload(EngineeringEvent{
		ID: "run-x-admission", RunID: "run-x", Type: EventControllerSuccessionAdmitted, Payload: payload,
	}); err == nil {
		t.Fatal("the journal accepted an admission naming no transition")
	}
}
