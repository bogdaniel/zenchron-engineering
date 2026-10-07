package runtime

// What a reviewer result may NOT do.
//
// The verdict is authority-bearing: it settles a stage, gates the assurance
// below it, and sends a producer back to work. So the interesting tests are the
// refusals. Every case here is a way something could claim reviewer authority
// without having it, and every one of them must fail closed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

// THE PROTOCOL DEFINITION IS ONE STRUCT, READ TWICE, NOT TWO DESCRIPTIONS
// (#374).
//
// reviewerEnvelope (sandbox.go) builds the instructions a reviewer reads from
// ReviewerResultMembers/ReviewerFindingMembers/reviewerResultStatedMembers
// rather than typing member names a second time, and indexes into their
// result by position. That only stays correct if this exact field order,
// shape and set survive - so it is pinned here the same way
// TestProviderExecutionResultCarriesNoAuthorityBearingField pins
// ExecutionResult: a field added, removed, renamed or reordered on
// ReviewerResult or ReviewerFinding fails this test, loudly, rather than
// silently producing prose that no longer matches what ReadReviewerResult
// accepts.
func TestReviewerEnvelopeMembersMatchTheProtocolStructFields(t *testing.T) {
	wantResult := []string{"schema_version", "verdict", "candidate", "tree", "findings", "reason"}
	if got := ReviewerResultMembers(); !reflect.DeepEqual(got, wantResult) {
		t.Fatalf("ReviewerResult JSON members = %v, want %v (update deliberately, together with reviewerEnvelope's positional use of them)", got, wantResult)
	}
	wantStated := []string{"schema_version", "verdict", "findings", "reason"}
	if got := reviewerResultStatedMembers(); !reflect.DeepEqual(got, wantStated) {
		t.Fatalf("reviewerResultStatedMembers() = %v, want %v", got, wantStated)
	}
	wantFinding := []string{"signature", "detail"}
	if got := ReviewerFindingMembers(); !reflect.DeepEqual(got, wantFinding) {
		t.Fatalf("ReviewerFinding JSON members = %v, want %v", got, wantFinding)
	}
}

// A DOCUMENT BUILT FROM EXACTLY WHAT reviewerEnvelope TELLS A REVIEWER TO
// WRITE ROUND-TRIPS THROUGH THE STRICT DECODER, CONTENT INTACT (#374). This is
// the positive mirror of TestAMalformedResultIsRefused: not merely "the two
// descriptions currently agree" but "a document shaped from the live member
// list is accepted and nothing in it is lost."
func TestTheEnvelopesStatedMembersRoundTripThroughTheStrictDecoder(t *testing.T) {
	document := map[string]any{}
	for _, name := range reviewerResultStatedMembers() {
		switch name {
		case "schema_version":
			document[name] = ReviewerResultSchemaVersion
		case "verdict":
			document[name] = StageReviewBlocked
		case "findings":
			finding := map[string]any{}
			for _, member := range ReviewerFindingMembers() {
				switch member {
				case "signature":
					finding[member] = "review:defect"
				case "detail":
					finding[member] = "the change is wrong"
				default:
					t.Fatalf("unhandled finding member %q - extend this test alongside ReviewerFinding", member)
				}
			}
			document[name] = []map[string]any{finding}
		case "reason":
			document[name] = "because the obligations were not met"
		default:
			t.Fatalf("unhandled result member %q - extend this test alongside ReviewerResult", name)
		}
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reviewer-result.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadReviewerResult(path)
	if err != nil {
		t.Fatalf("a document built from exactly the envelope's stated members was refused: %v (document: %s)", err, raw)
	}
	if result.Verdict != StageReviewBlocked || len(result.Findings) != 1 ||
		result.Findings[0].Signature != "review:defect" || result.Findings[0].Detail != "the change is wrong" {
		t.Fatalf("round trip lost content: %#v", result)
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
//
// Each case also states whether its refusal is a PROTOCOL refusal or an
// AUTHORITY one (#374). The two cannot be told apart by trying it again with
// better-formed JSON: a protocol refusal can be, because nothing about it
// depended on who asked or what they claimed to have reviewed; an authority
// refusal cannot, because rewriting the document changes none of the facts
// that caused it. operations.go's admitReview reads exactly this field to
// decide between FailureReviewerProtocolIncomplete (bounded correction) and
// FailureVerification (a verdict on the invocation, not on the candidate) -
// so a case drifting to the wrong side here is the #374 regression returning
// through this exact table.
func TestReviewerResultAdmissionRefusals(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*reviewerFixture, *ReviewerResult)
		protocol bool
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
			name:     "an unrecognized schema version",
			mutate:   func(_ *reviewerFixture, r *ReviewerResult) { r.SchemaVersion = "99.0" },
			protocol: true,
		},
		{
			name:     "no schema version at all",
			mutate:   func(_ *reviewerFixture, r *ReviewerResult) { r.SchemaVersion = "" },
			protocol: true,
		},
		{
			name:     "an unrecognized verdict",
			mutate:   func(_ *reviewerFixture, r *ReviewerResult) { r.Verdict = "probably-fine" },
			protocol: true,
		},
		{
			name:     "no verdict at all",
			mutate:   func(_ *reviewerFixture, r *ReviewerResult) { r.Verdict = "" },
			protocol: true,
		},
		{
			// TWO ANSWERS AT ONCE. A gate reads the verdict, so accepting while
			// naming defects would let the permissive half decide.
			name: "an acceptance naming blocking findings",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Verdict = StageReviewAccepted
				r.Findings = []ReviewerFinding{{Signature: "review:defect"}}
			},
			protocol: true,
		},
		{
			// A BLOCK REMEDIATION CANNOT ACT ON.
			name:     "a block naming no finding",
			mutate:   func(_ *reviewerFixture, r *ReviewerResult) { r.Findings = nil },
			protocol: true,
		},
		{
			name: "a finding with no signature",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Findings = []ReviewerFinding{{Detail: "something is wrong"}}
			},
			protocol: true,
		},
		{
			name: "more findings than the bound allows",
			mutate: func(_ *reviewerFixture, r *ReviewerResult) {
				r.Findings = make([]ReviewerFinding, maxReviewerFindings+1)
				for i := range r.Findings {
					r.Findings[i] = ReviewerFinding{Signature: "review:defect"}
				}
			},
			protocol: true,
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
			if refused.Protocol != tc.protocol {
				t.Fatalf("refusal.Protocol = %v, want %v: a case must be classified as correctable protocol failure (one bounded retry) xor authority failure (never corrected), see FailureReviewerProtocolIncomplete vs FailureVerification in operations.go", refused.Protocol, tc.protocol)
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

// The ADAPTER's own ordering law, tested where the repair lives.
//
// A reviewer can write its verdict and then die. The file is on disk either
// way, and the adapter must not carry it out of a failed invocation - nor let a
// malformed one CHANGE the diagnosis of why the invocation died.
//
// The second half is tested by comparison rather than by naming a class: the
// same failure is run twice, once with a well-formed verdict on disk and once
// with an unreadable one, and the classification must be identical. Asserting a
// particular class would only restate whatever classifyAgentFailure happens to
// return for an empty transcript.
func TestTheAdapterReadsNoVerdictOutOfAFailedInvocation(t *testing.T) {
	cases := map[string]struct {
		cancel bool
		runErr error
	}{
		"cancelled":       {cancel: true},
		"process failure": {runErr: errors.New("exit status 1")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			classify := func(document string) *ProviderFailure {
				provider, request, fake := agentFixture(t, AgentKindClaudeCode)
				fake.block, fake.err = tc.cancel, tc.runErr
				path, err := PrepareReviewerResult(t.TempDir(), request.AttemptRef())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
					t.Fatal(err)
				}
				request.ReviewerResultPath = path
				ctx := context.Background()
				if tc.cancel {
					bounded, cancel := context.WithCancel(ctx)
					cancel()
					ctx = bounded
				}
				result, typed, _ := executeWithHostSlots(ctx, provider, request)
				if typed.Review != nil {
					t.Fatalf("a failed invocation carried a verdict out of the adapter: %+v", typed.Review)
				}
				if result.Outcome == Succeeded || result.Failure == nil {
					t.Fatalf("the invocation did not report a failure: %+v", result)
				}
				return result.Failure
			}
			wellFormed := classify(`{"schema_version":"0.1","verdict":"accepted"}`)
			malformed := classify(`{not json`)
			if wellFormed.Classification != malformed.Classification {
				t.Fatalf("an unreadable verdict changed the diagnosis from %q to %q",
					wellFormed.Classification, malformed.Classification)
			}
		})
	}
}

// A malformed result on a SUCCESSFUL invocation still fails it: a reviewer that
// tried to answer and produced something unreadable has not declined to answer.
func TestAMalformedVerdictFailsAnOtherwiseSuccessfulInvocation(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindClaudeCode)
	path, err := PrepareReviewerResult(t.TempDir(), request.AttemptRef())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	request.ReviewerResultPath = path

	result, typed, err := executeWithHostSlots(context.Background(), provider, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OperationFailed || result.Failure == nil {
		t.Fatalf("a malformed verdict did not fail the invocation: %+v", result)
	}
	// Never FailureVerification (#374): nothing was judged, so nothing about
	// the candidate failed review - this is the reviewer's OWN invocation
	// failing to cross the result protocol.
	if result.Failure.Classification != FailureReviewerProtocolIncomplete {
		t.Fatalf("classification %q, want %q", result.Failure.Classification, FailureReviewerProtocolIncomplete)
	}
	if typed.Review != nil {
		t.Fatalf("a malformed verdict was carried out anyway: %+v", typed.Review)
	}
	// The exact reason is retained rather than discarded down to a bare
	// classification, so a bounded retry can tell the reviewer what was wrong.
	if typed.ReviewRefusal == nil || !strings.Contains(typed.ReviewRefusal.Detail, "not a valid") {
		t.Fatalf("the exact decode reason was not retained: %+v", typed.ReviewRefusal)
	}
}

// The finding bound admission enforces IS the durable payload bound, so a
// result admission accepts is a result the journal can hold.
func TestTheReviewerFindingBoundIsTheDurableBound(t *testing.T) {
	if maxReviewerFindings != maxPayloadListItems {
		t.Fatalf("the admission bound is %d and the durable payload bound is %d: two numbers that drift are one bug",
			maxReviewerFindings, maxPayloadListItems)
	}
	for _, count := range []int{maxReviewerFindings - 1, maxReviewerFindings, maxReviewerFindings + 1} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			fixture := newReviewerFixture()
			result := blocking()
			result.Findings = make([]ReviewerFinding, count)
			for i := range result.Findings {
				result.Findings[i] = ReviewerFinding{Signature: "review:defect"}
			}
			payload, admitErr := fixture.admit(result)
			if count > maxReviewerFindings {
				if admitErr == nil {
					t.Fatal("a result above the bound was admitted")
				}
				return
			}
			if admitErr != nil {
				t.Fatalf("a result at or below the bound was refused: %v", admitErr)
			}
			// AND THE JOURNAL ACCEPTS IT. Admission that the durable validator
			// then refuses is the mismatch this test exists to prevent.
			document, err := CanonicalJSON(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := eventPayloads[EventPlanStageReviewed](document); err != nil {
				t.Fatalf("admission accepted %d findings the durable payload validator refuses: %v", count, err)
			}
		})
	}
}
