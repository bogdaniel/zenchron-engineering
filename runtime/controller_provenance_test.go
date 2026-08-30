package runtime

// Controller provenance. The defect these tests exist for is forensic: a failed
// self-hosting run recorded its controller as
//
//	controller: zenchron-engineering/dev sha=88e178019bde...
//
// and that digest was Digest({controller: "zenchron-engineering/dev", config})
// over a HARDCODED version literal. It therefore contained no source commit, no
// source tree, and no binary digest, so durable state could not answer the one
// question a self-hosting run has to answer: was this run driven by the
// unadopted build under test, or by the adopted controller on the trusted
// branch?
//
// Everything below asserts that answer is now IN the run, not reconstructible
// from an adjacent file, a build log, or an operator's memory.

import (
	"context"
	"strings"
	"testing"
)

// attestedBuild is a complete, valid provenance. Tests mutate one field at a
// time from here, so what distinguishes two controllers is always explicit.
func attestedBuild(kind, revision, tree, binary string) ControllerBuild {
	return ControllerBuild{
		Kind:           kind,
		Version:        "dev",
		SourceRevision: revision,
		SourceTree:     tree,
		BinarySHA256:   binary,
	}
}

func digestOf(t *testing.T, seed string) string {
	t.Helper()
	d, err := Digest(seed)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// runWithBuild creates one run under one controller build and returns the run
// identity together with the durable row.
func runWithBuild(t *testing.T, fixture *phase8Fixture, build ControllerBuild, issue int) (string, EngineeringRun) {
	t.Helper()
	deps := fixture.deps
	deps.ControllerBuild = build
	rt := fixture.newRuntime(deps)
	runID, err := rt.StartOrResumeIssueRun(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := fixture.store.Run(runID)
	if err != nil || !ok {
		t.Fatalf("run %s is not durable: ok=%t err=%v", runID, ok, err)
	}
	return runID, run
}

// TestControllerProvenanceIsDurable is the defect itself. Every field the
// forensic record lacked is asserted to survive a replay by a DIFFERENT
// process: the reading runtime is deliberately unattested, so nothing reported
// can have come from the reader's own build.
func TestControllerProvenanceIsDurable(t *testing.T) {
	fixture := newPhase8Fixture(t)
	build := attestedBuild(ControllerPreAdoptionBuild,
		"9f1d2b3c4e5f60718293a4b5c6d7e8f901234567",
		"1122334455667788990011223344556677889900",
		strings.Repeat("ab", 32))

	runID, run := runWithBuild(t, fixture, build, fixture.issue)

	// The reader has no build metadata of its own.
	reader := fixture.newRuntime(fixture.deps)
	report, err := reader.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	recorded := report.Controller.Build
	if recorded.Kind != ControllerPreAdoptionBuild {
		t.Fatalf("kind is not persisted: %q", recorded.Kind)
	}
	if recorded.SourceRevision != build.SourceRevision {
		t.Fatalf("source revision is not persisted: %q", recorded.SourceRevision)
	}
	if recorded.SourceTree != build.SourceTree {
		t.Fatalf("source tree is not persisted: %q", recorded.SourceTree)
	}
	if recorded.BinarySHA256 != build.BinarySHA256 {
		t.Fatalf("binary sha-256 is not persisted: %q", recorded.BinarySHA256)
	}
	if recorded != build {
		t.Fatalf("recorded provenance %+v is not the build that created the run %+v", recorded, build)
	}
	// The durable digest still binds the identity the reconciler compares.
	if len(run.ControllerSHA256) != 64 {
		t.Fatalf("ControllerSHA256 stopped being a digest: %q", run.ControllerSHA256)
	}
	// ...and the provenance is in the journal, not only in a projection.
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != EventRunCreated || len(events[0].Payload) == 0 {
		t.Fatalf("the genesis event carries no provenance: %+v", events)
	}
	if !strings.Contains(string(events[0].Payload), build.SourceRevision) {
		t.Fatalf("the genesis payload does not carry the source revision: %s", events[0].Payload)
	}
}

// TestTwoBinariesFromOneSourceAreNotConfusable constructs the exact collision
// the old scheme allowed: two builds of the SAME source, with the same version
// string, under the same configuration. The old digest cannot tell them apart,
// so only the measured binary can.
func TestTwoBinariesFromOneSourceAreNotConfusable(t *testing.T) {
	fixture := newPhase8Fixture(t)
	revision, tree := digestOf(t, "revision"), digestOf(t, "tree")
	first := attestedBuild(ControllerPreAdoptionBuild, revision, tree, digestOf(t, "binary-a"))
	second := attestedBuild(ControllerPreAdoptionBuild, revision, tree, digestOf(t, "binary-b"))
	if first.BinarySHA256 == second.BinarySHA256 {
		t.Fatal("the fixture does not describe two different binaries")
	}

	// The old scheme, recomputed exactly: controller id plus configuration.
	old := func() string {
		d, err := Digest(struct {
			Controller string       `json:"controller"`
			Config     ConfigDigest `json:"config"`
		}{fixture.deps.ControllerID, fixture.deps.ConfigDigest})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if old() != old() {
		t.Fatal("the old scheme is not deterministic")
	}
	// One digest for both binaries: that IS the defect.

	_, runA := runWithBuild(t, fixture, first, fixture.issue)
	_, runB := runWithBuild(t, fixture, second, fixture.issue+1)

	if runA.ControllerSHA256 == old() || runB.ControllerSHA256 == old() {
		t.Fatal("the run still records the old controller/config digest")
	}
	if runA.ControllerSHA256 == runB.ControllerSHA256 {
		t.Fatalf("two different binaries share one controller identity %s", runA.ControllerSHA256)
	}
}

// TestConfigurationDoesNotMasqueradeAsADifferentBinary is the separation the
// repair must not lose. Two configurations, one binary: the configuration
// digest moves, the durable controller identity moves with it, and the
// provenance of the binary is byte-identical in both runs.
func TestConfigurationDoesNotMasqueradeAsADifferentBinary(t *testing.T) {
	fixture := newPhase8Fixture(t)
	build := attestedBuild(ControllerAdopted, digestOf(t, "revision"), digestOf(t, "tree"), digestOf(t, "binary"))

	deps := fixture.deps
	deps.ControllerBuild = build
	first := fixture.newRuntime(deps)

	other := deps
	other.ConfigDigest = ConfigDigest{Global: "g2", Repository: "r2"}
	second := fixture.newRuntime(other)

	firstRun, err := first.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := second.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if firstRun == secondRun {
		t.Fatal("a configuration change did not produce a different run")
	}

	firstReport, err := first.Status(firstRun)
	if err != nil {
		t.Fatal(err)
	}
	secondReport, err := second.Status(secondRun)
	if err != nil {
		t.Fatal(err)
	}
	// The binary identity is unchanged by configuration.
	if firstReport.Controller.Build != secondReport.Controller.Build {
		t.Fatalf("a configuration change changed the binary identity: %+v vs %+v",
			firstReport.Controller.Build, secondReport.Controller.Build)
	}
	// The configuration is still represented independently...
	if firstReport.Controller.ConfigDigest == secondReport.Controller.ConfigDigest {
		t.Fatal("the configuration digest is no longer independently represented")
	}
	// ...and the durable identity the reconciler compares still binds both.
	if firstReport.Controller.SHA256 == secondReport.Controller.SHA256 {
		t.Fatal("a configuration change no longer changes the controller identity")
	}
}

// TestPreAdoptionBuildIsDistinguishableFromAdopted is #32's requirement in one
// assertion: the controller under test and the trusted controller differ in
// durable state even when they are built from the same source.
func TestPreAdoptionBuildIsDistinguishableFromAdopted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	revision, tree, binary := digestOf(t, "revision"), digestOf(t, "tree"), digestOf(t, "binary")
	unadopted := attestedBuild(ControllerPreAdoptionBuild, revision, tree, binary)
	adopted := attestedBuild(ControllerAdopted, revision, tree, binary)

	unadoptedID, unadoptedRun := runWithBuild(t, fixture, unadopted, fixture.issue)
	adoptedID, adoptedRun := runWithBuild(t, fixture, adopted, fixture.issue+1)

	if unadoptedRun.ControllerSHA256 == adoptedRun.ControllerSHA256 {
		t.Fatal("an unadopted build and an adopted build share one controller identity")
	}
	reader := fixture.newRuntime(fixture.deps)
	for _, want := range []struct {
		runID string
		kind  string
	}{{unadoptedID, ControllerPreAdoptionBuild}, {adoptedID, ControllerAdopted}} {
		report, err := reader.Status(want.runID)
		if err != nil {
			t.Fatal(err)
		}
		if report.Controller.Build.Kind != want.kind {
			t.Fatalf("run %s reports kind %q, want %q", want.runID, report.Controller.Build.Kind, want.kind)
		}
	}
}

// TestUnattestedControllerClaimsNothing keeps the absence of provenance honest.
// A plain build records no claim and reads back as unattested - never as
// adopted, which would be the one lie that matters.
func TestUnattestedControllerClaimsNothing(t *testing.T) {
	fixture := newPhase8Fixture(t)
	rt := fixture.newRuntime(fixture.deps)
	runID, err := rt.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	report, err := rt.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Controller.Build.Kind != ControllerUnattested {
		t.Fatalf("an unattested controller reported kind %q", report.Controller.Build.Kind)
	}
	if report.Controller.Build.Attested() {
		t.Fatalf("an unattested controller recorded a claim: %+v", report.Controller.Build)
	}
}

// TestPartialProvenanceIsRefused is the fail-closed edge: a build that names a
// source revision but cannot say which binary ran is exactly the gap this type
// closes, so it is refused at construction rather than recorded.
func TestPartialProvenanceIsRefused(t *testing.T) {
	fixture := newPhase8Fixture(t)
	for _, refused := range []struct {
		name  string
		build ControllerBuild
	}{
		{"no binary digest", ControllerBuild{Kind: ControllerAdopted, Version: "dev", SourceRevision: "r", SourceTree: "t"}},
		{"short binary digest", attestedBuild(ControllerAdopted, "r", "t", "abcdef")},
		{"upper-case binary digest", attestedBuild(ControllerAdopted, "r", "t", strings.ToUpper(strings.Repeat("ab", 32)))},
		{"no source revision", ControllerBuild{Kind: ControllerAdopted, Version: "dev", SourceTree: "t", BinarySHA256: strings.Repeat("ab", 32)}},
		{"no source tree", ControllerBuild{Kind: ControllerAdopted, Version: "dev", SourceRevision: "r", BinarySHA256: strings.Repeat("ab", 32)}},
		{"unknown kind", attestedBuild("trust-me", "r", "t", strings.Repeat("ab", 32))},
		{"unattested kind with a claim", attestedBuild(ControllerUnattested, "r", "t", strings.Repeat("ab", 32))},
	} {
		t.Run(refused.name, func(t *testing.T) {
			deps := fixture.deps
			deps.ControllerBuild = refused.build
			if _, err := NewEngineeringRuntime(deps); err == nil {
				t.Fatalf("partial provenance %+v was accepted", refused.build)
			}
		})
	}
}

// TestRecordedProvenanceIsValidatedAtTheJournal closes the other end: the
// journal itself refuses an incomplete provenance payload, so durable state
// cannot hold a claim that would not have been accepted at construction.
func TestRecordedProvenanceIsValidatedAtTheJournal(t *testing.T) {
	fixture := newPhase8Fixture(t)
	partial, err := marshalPayloadJSON(ControllerBuild{Kind: ControllerAdopted, Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PutRun(EngineeringRun{SchemaVersion: SchemaVersion, ID: "run-x", Repository: "acme/repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "run-x-created", RunID: "run-x",
		Type: EventRunCreated, OccurredAt: fixture.clock.Now(), Payload: partial,
	}); err == nil {
		t.Fatal("the journal accepted an incomplete controller provenance")
	}
}
