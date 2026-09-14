package runtime

// What a reviewer result may NOT do.
//
// The verdict is authority-bearing: it settles a stage, gates the assurance
// below it, and sends a producer back to work. So the interesting tests are the
// refusals. Every case here is a way something could claim reviewer authority
// without having it, and every one of them must fail closed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// reviewerFixture is one admission question: a reviewer stage, its frozen
// assignment, the run binding it was created under, and the exact candidate the
// runtime materialized for it.
type reviewerFixture struct {
	stage      domain.PlanStage
	assignment domain.AgentAssignment
	binding    *RunPlanBinding
	subject    domain.UpstreamOutput
}

func newReviewerFixture() reviewerFixture {
	return reviewerFixture{
		stage: domain.PlanStage{
			ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			InvocationMode: domain.InvocationModeMutating,
		},
		assignment: domain.AgentAssignment{
			ID: "assignment-review", StageID: "review", Role: domain.RoleReviewer,
			Agent: domain.AgentBinding{ID: "claude", VendorFamily: "anthropic"},
			Independence: []domain.IndependenceBinding{{
				Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"implementation"},
				Class: "claude", OtherClasses: []string{"codex"},
			}},
			Context: domain.ContextPack{UpstreamOutputs: []domain.UpstreamOutput{{
				StageID: "implementation", RunID: "run-producer",
				Candidate: strings.Repeat("b", 40), Tree: strings.Repeat("t", 40),
			}}},
		},
		binding: &RunPlanBinding{
			PlanID: "plan-1", Revision: 1, StageID: "review", AssignmentID: "assignment-review",
		},
		subject: domain.UpstreamOutput{
			StageID: "implementation", RunID: "run-producer",
			Candidate: strings.Repeat("b", 40), Tree: strings.Repeat("t", 40),
		},
	}
}

func (f reviewerFixture) admit(result *ReviewerResult) (PlanStageReviewedPayload, error) {
	return AdmitReviewerResult(f.stage, f.assignment, f.binding, f.subject, "run-review", "claude", result)
}

func accepting() *ReviewerResult {
	return &ReviewerResult{SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewAccepted}
}

func blocking() *ReviewerResult {
	return &ReviewerResult{
		SchemaVersion: ReviewerResultSchemaVersion, Verdict: StageReviewBlocked,
		Findings: []ReviewerFinding{{Signature: "review:defect", Detail: "the change is wrong"}},
	}
}

// The positive case, so every refusal below is a refusal OF something that
// otherwise works.
func TestAnAdmittedVerdictIsBoundToTheRuntimesOwnCandidate(t *testing.T) {
	fixture := newReviewerFixture()
	payload, err := fixture.admit(blocking())
	if err != nil {
		t.Fatalf("a well-formed blocking verdict was refused: %v", err)
	}
	if payload.Verdict != StageReviewBlocked {
		t.Fatalf("verdict %q", payload.Verdict)
	}
	// The binding comes from the ASSIGNMENT, not from the result - the result
	// named no candidate at all.
	if payload.Candidate != fixture.subject.Candidate || payload.Tree != fixture.subject.Tree {
		t.Fatalf("the verdict was not bound to the materialized candidate: %+v", payload)
	}
	if payload.UpstreamRunID != "run-producer" || payload.RunID != "run-review" {
		t.Fatalf("the verdict does not name both runs: %+v", payload)
	}
	if len(payload.Findings) != 1 || !strings.Contains(payload.Findings[0], "review:defect") {
		t.Fatalf("the findings were not carried: %+v", payload.Findings)
	}
}

// PROSE IS NOT A VERDICT. A reviewer that wrote nothing to the structured path
// produced no result, and the stage stays unsettled rather than being guessed
// at in either direction.
func TestAReviewerThatWroteOnlyProseProducesNoVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviewer-result.json")
	result, err := ReadReviewerResult(path)
	if err != nil {
		t.Fatalf("an absent result is not an error: %v", err)
	}
	if result != nil {
		t.Fatalf("an absent result produced a verdict: %+v", result)
	}
	// And admitting nothing records nothing.
	payload, err := newReviewerFixture().admit(nil)
	if err != nil || payload.StageID != "" {
		t.Fatalf("admitting no result produced %+v (err=%v)", payload, err)
	}
}

// A transcript that CONTAINS verdict-shaped JSON is still a transcript. The
// structured channel is a file the runtime named; nothing reads stdout for a
// verdict, so there is no path by which prose becomes one.
func TestVerdictShapedProseInATranscriptIsNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "attempt-1.raw.log")
	document, err := json.Marshal(accepting())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, append([]byte("I reviewed it and it is fine.\n"), document...), 0o600); err != nil {
		t.Fatal(err)
	}
	// The result path is a DIFFERENT file, and it does not exist.
	result, err := ReadReviewerResult(filepath.Join(dir, "reviewer-result.json"))
	if err != nil || result != nil {
		t.Fatalf("a transcript was read as a verdict: %+v (err=%v)", result, err)
	}
}

// A CANDIDATE-CONTROLLED FILE cannot become the verdict. The result path is
// outside the candidate workspace, so a repository that ships a file of that
// name ships a file in its own tree and nothing more.
func TestARepositoryCannotPreSeedTheResult(t *testing.T) {
	stateDir := t.TempDir()
	attempt := ExecutionAttemptRef{RunID: "run-1", OperationID: "run-1:execution.invoke:x", Attempt: 1}
	path, err := ReviewerResultPath(stateDir, attempt)
	if err != nil {
		t.Fatal(err)
	}
	candidateWorkspace := candidateDir(stateDir, "run-1")
	if strings.HasPrefix(path, candidateWorkspace) {
		t.Fatalf("the result path %s is inside the candidate workspace %s, so repository content could pre-seed it",
			path, candidateWorkspace)
	}
	// And the slot is EMPTIED before an invocation, so a result left by an
	// earlier attempt cannot be inherited by one that produced nothing.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := json.Marshal(accepting())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareReviewerResult(stateDir, attempt); err != nil {
		t.Fatal(err)
	}
	result, err := ReadReviewerResult(path)
	if err != nil || result != nil {
		t.Fatalf("a previous attempt's result survived preparation: %+v (err=%v)", result, err)
	}
}

// Malformed, oversized and unknown-member documents are refused rather than
// interpreted. A result this build cannot read is not a verdict it may act on.
func TestAMalformedResultIsRefused(t *testing.T) {
	cases := map[string]string{
		"not json":             "{this is not json",
		"two values":           `{"schema_version":"0.1","verdict":"accepted"} {"schema_version":"0.1","verdict":"blocked"}`,
		"unknown member":       `{"schema_version":"0.1","verdict":"accepted","authority":"granted"}`,
		"wrong member type":    `{"schema_version":"0.1","verdict":["accepted"]}`,
		"findings not objects": `{"schema_version":"0.1","verdict":"blocked","findings":["x"]}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reviewer-result.json")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadReviewerResult(path); err == nil {
				t.Fatal("a malformed result was accepted")
			}
		})
	}
}

func TestAnOversizedResultIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviewer-result.json")
	if err := os.WriteFile(path, make([]byte, maxReviewerResultBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReviewerResult(path); err == nil {
		t.Fatal("an oversized result was read")
	}
}

// Every admission refusal, one case each. They share a table because the shape
// of the assertion is the same: this claim does not become lifecycle state.
func TestReviewerResultAdmissionRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reviewerFixture, *ReviewerResult)
	}{
		{
			// AN IMPLEMENTER CANNOT BECOME A REVIEWER by writing matching JSON.
			name: "an implementer emitting a reviewer result",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) {
				f.stage.Role = domain.RoleImplementer
				f.assignment.Role = domain.RoleImplementer
			},
		},
		{
			name: "a stage that is not an agent stage",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) {
				f.stage.Kind = domain.StageAssuranceGate
			},
		},
		{
			name:   "a run bound to no plan stage",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) { f.binding = nil },
		},
		{
			name:   "a result for a different stage",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) { f.assignment.StageID = "other-review" },
		},
		{
			name:   "an assignment the run was not created under",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) { f.binding.AssignmentID = "assignment-something-else" },
		},
		{
			// A DIFFERENT WORKER than the approved reviewer.
			name:   "a provider that is not the frozen reviewer",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) { f.assignment.Agent.ID = "codex" },
		},
		{
			// INDEPENDENCE NO LONGER HOLDS: the reviewer has become the
			// producer's own worker, so its verdict is not an independent one
			// whatever it says.
			name: "a reviewer that shares the producer's class",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) {
				f.assignment.Independence[0].OtherClasses = []string{"claude"}
			},
		},
		{
			name:   "a result claiming a different candidate",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.Candidate = strings.Repeat("9", 40) },
		},
		{
			name:   "a result claiming a different tree",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.Tree = strings.Repeat("9", 40) },
		},
		{
			// A STALE SUBJECT: the assignment froze nothing to have reviewed.
			name:   "an assignment with no exact candidate",
			mutate: func(f *reviewerFixture, _ *ReviewerResult) { f.subject = domain.UpstreamOutput{} },
		},
		{
			name:   "an unrecognized schema version",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.SchemaVersion = "99.0" },
		},
		{
			name:   "no schema version at all",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.SchemaVersion = "" },
		},
		{
			name:   "an unrecognized verdict",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.Verdict = "probably-fine" },
		},
		{
			name:   "no verdict at all",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.Verdict = "" },
		},
		{
			// TWO ANSWERS AT ONCE. A gate reads the verdict, so accepting while
			// naming defects would let the permissive half decide.
			name: "an acceptance naming blocking findings",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Verdict = StageReviewAccepted
				r.Findings = []ReviewerFinding{{Signature: "review:defect"}}
			},
		},
		{
			// A BLOCK REMEDIATION CANNOT ACT ON.
			name:   "a block naming no finding",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) { r.Findings = nil },
		},
		{
			name: "a finding with no signature",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Findings = []ReviewerFinding{{Detail: "something is wrong"}}
			},
		},
		{
			name: "more findings than the bound allows",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Findings = make([]ReviewerFinding, maxReviewerFindings+1)
				for i := range r.Findings {
					r.Findings[i] = ReviewerFinding{Signature: "review:defect"}
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newReviewerFixture()
			result := blocking()
			tc.mutate(&fixture, result)
			payload, err := fixture.admit(result)
			if err == nil {
				t.Fatalf("the claim was admitted: %+v", payload)
			}
			var refused *ReviewerResultRefusedError
			if !asReviewerRefusal(err, &refused) {
				t.Fatalf("the refusal is untyped: %v", err)
			}
		})
	}
}

// An ACCEPTANCE with no findings is the other well-formed shape, and it is
// admitted. Without this the table above would be consistent with a build that
// refuses everything.
func TestAWellFormedAcceptanceIsAdmitted(t *testing.T) {
	payload, err := newReviewerFixture().admit(accepting())
	if err != nil {
		t.Fatalf("a well-formed acceptance was refused: %v", err)
	}
	if payload.Verdict != StageReviewAccepted || len(payload.Findings) != 0 {
		t.Fatalf("the acceptance was not admitted cleanly: %+v", payload)
	}
}

// A verdict grants NO merge or release authority. It settles a stage and gates
// an assurance obligation; the payload carries nothing else, and there is no
// member on it a publication decision could read.
func TestAVerdictCarriesNoPublicationAuthority(t *testing.T) {
	payload, err := newReviewerFixture().admit(accepting())
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"merge", "publish", "release", "authority", "approve"} {
		if strings.Contains(strings.ToLower(string(document)), forbidden) {
			t.Fatalf("an admitted verdict carries a %q member: %s", forbidden, document)
		}
	}
}

// asReviewerRefusal is errors.As without importing errors into every case.
func asReviewerRefusal(err error, target **ReviewerResultRefusedError) bool {
	refused, ok := err.(*ReviewerResultRefusedError)
	if ok {
		*target = refused
	}
	return ok
}
