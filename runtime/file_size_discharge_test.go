package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	dischargeCommit = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	dischargeTree   = "7777777777777777777777777777777777777777"
	dischargeBase   = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
)

func dischargeContract() domain.EngineeringWorkContract {
	return domain.EngineeringWorkContract{
		ID: "contract-run-d", Revision: "1-" + dischargeCommit,
		Subject:    domain.Subject{Repository: "owner/repo", Revision: dischargeCommit},
		Provenance: domain.ContractProvenance{Policy: domain.ObjectRevision{ID: "policy", Revision: "1"}},
		RequiredClaims: map[string]domain.RequiredClaim{
			"claim-validation":    {EvidenceClass: AssuranceEvidenceClass},
			FileSizePolicyClaimID: {EvidenceClass: AssuranceEvidenceClass},
		},
	}
}

func goodSizeCheck() VerificationCheck {
	return VerificationCheck{
		Kind: CheckKindFileSizePolicy, Checker: fileSizeCheckerID, CheckerRevision: fileSizeCheckerRevision(),
		Outcome: CheckOutcomePassed, Commit: dischargeCommit, Tree: dischargeTree, BaseRevision: dischargeBase,
	}
}

// journalFor builds the events a run records for one candidate and its passing
// assurance observation, as a journal would hold them.
func journalFor(t *testing.T, checks []VerificationCheck) []EngineeringEvent {
	t.Helper()
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	event := func(sequence int64, kind string, payload any) EngineeringEvent {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return EngineeringEvent{SchemaVersion: "0.1", ID: kind + "-" + string(rune('a'+sequence)), RunID: "run-d", Sequence: sequence, Type: kind, OccurredAt: at, Payload: raw}
	}
	return []EngineeringEvent{
		event(1, EventCandidateCommitted, CandidateCommittedPayload{Commit: dischargeCommit, Tree: dischargeTree}),
		event(2, EventAssuranceObserved, AssuranceObservedPayload{
			ProviderID: "baseline-go", VerifierDefinition: "vd", Passed: true, Commit: dischargeCommit, Tree: dischargeTree, Checks: checks,
		}),
	}
}

// bundleClaims replays the journal into a projection and reports which claims
// the evidence bundle discharges, the way a restarted controller would.
func bundleClaims(t *testing.T, events []EngineeringEvent, base string) map[string]domain.EvidenceItem {
	t.Helper()
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{projection: projection, events: events, run: EngineeringRun{ID: "run-d"}}
	if base != "" {
		state.sources = []sourceRecord{{BaseRevision: base}}
	}
	runtime := &EngineeringRuntime{deps: Dependencies{Assurance: BaselineGoVerifier{}}}
	bundles, err := runtime.evidenceBundles(state, dischargeContract())
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]domain.EvidenceItem{}
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			claims[item.ClaimID] = item
		}
	}
	return claims
}

// THE HANDOVER SCENARIO. An observation written before the requirement existed
// proves what its suite proved and nothing about the file-size policy; one that
// carries the completed check proves the claim; and replaying the journal
// preserves both outcomes.
func TestAnOldPassingObservationCannotDischargeTheNewFileSizeClaim(t *testing.T) {
	old := journalFor(t, nil)
	claims := bundleClaims(t, old, dischargeBase)
	if _, ok := claims["claim-validation"]; !ok {
		t.Fatal("an earlier passing observation stopped discharging the claim it always discharged")
	}
	if _, ok := claims[FileSizePolicyClaimID]; ok {
		t.Fatal("a passing suite with no recorded size check discharged the file-size claim")
	}

	fresh := journalFor(t, []VerificationCheck{goodSizeCheck()})
	claims = bundleClaims(t, fresh, dischargeBase)
	item, ok := claims[FileSizePolicyClaimID]
	if !ok || item.Result.Status != domain.EvidencePassed {
		t.Fatalf("fresh exact-subject verification did not discharge the claim: %+v", claims)
	}
	if item.Producer.ID != fileSizeCheckerID || item.Environment.Identifier != fileSizeHostEnvironmentID {
		t.Fatalf("the claim's evidence does not name the host-side checker that proved it: %+v %+v", item.Producer, item.Environment)
	}
	if suite := claims["claim-validation"]; suite.Producer.ID != "baseline-go" || suite.Environment.Identifier != "vd" {
		t.Fatalf("the suite claim's provenance changed: %+v", suite)
	}

	// Restart and replay: the same journal, serialized and read back, gives the
	// same two answers. Nothing is invented for the old observation.
	for name, events := range map[string][]EngineeringEvent{"old": old, "fresh": fresh} {
		raw, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		var replayed []EngineeringEvent
		if err := json.Unmarshal(raw, &replayed); err != nil {
			t.Fatal(err)
		}
		_, got := bundleClaims(t, replayed, dischargeBase)[FileSizePolicyClaimID]
		if want := name == "fresh"; got != want {
			t.Fatalf("after replay the %s journal discharges the file-size claim = %v, want %v", name, got, want)
		}
	}
}

// Only a record that is complete, successful, by the current checker, and bound
// to this commit, this tree and this base proves the claim.
func TestOnlyAMatchingSuccessfulSizeCheckProvesTheClaim(t *testing.T) {
	mutations := map[string]func(*VerificationCheck){
		"another commit":      func(c *VerificationCheck) { c.Commit = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2" },
		"another tree":        func(c *VerificationCheck) { c.Tree = "8888888888888888888888888888888888888888" },
		"another base":        func(c *VerificationCheck) { c.BaseRevision = "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3" },
		"no base":             func(c *VerificationCheck) { c.BaseRevision = "" },
		"a failed outcome":    func(c *VerificationCheck) { c.Outcome = CheckOutcomeFailed },
		"another kind":        func(c *VerificationCheck) { c.Kind = "something_else" },
		"another checker":     func(c *VerificationCheck) { c.Checker = "baseline-go" },
		"another checker rev": func(c *VerificationCheck) { c.CheckerRevision = "sha256:an-older-rules-version" },
	}
	for name, mutate := range mutations {
		check := goodSizeCheck()
		mutate(&check)
		if _, ok := bundleClaims(t, journalFor(t, []VerificationCheck{check}), dischargeBase)[FileSizePolicyClaimID]; ok {
			t.Errorf("a record with %s discharged the claim", name)
		}
	}
	// A matching record cannot be vouched for by a run with no base revision.
	if _, ok := bundleClaims(t, journalFor(t, []VerificationCheck{goodSizeCheck()}), "")[FileSizePolicyClaimID]; ok {
		t.Error("the claim was discharged with no governing base revision")
	}
	// A contract that does not require the claim is unaffected by all of this.
	contract := dischargeContract()
	delete(contract.RequiredClaims, FileSizePolicyClaimID)
	if fileSizePolicyRequired(contract) || fileSizePolicyBase(contract, dischargeBase) != "" {
		t.Error("a contract without the claim became subject to the check")
	}
}

// A required check with no base revision refuses verification; it is never
// silently skipped.
func TestARequiredCheckWithNoBaseRefusesVerification(t *testing.T) {
	if err := fileSizeBaseRefusal(dischargeContract(), ""); err == nil {
		t.Fatal("a required file-size check with no base revision was skipped")
	}
	if err := fileSizeBaseRefusal(dischargeContract(), dischargeBase); err != nil {
		t.Fatalf("a required check with a base was refused: %v", err)
	}
	without := dischargeContract()
	delete(without.RequiredClaims, FileSizePolicyClaimID)
	if err := fileSizeBaseRefusal(without, ""); err != nil {
		t.Fatalf("a contract that does not require the check was refused: %v", err)
	}
}

// The new requirement changes the wanted-operation binding, so the existing
// scheduler plans the missing verification; a run that does not require it
// keeps the binding it always had.
func TestARequirementThatAppearsChangesTheAssuranceBinding(t *testing.T) {
	listing := domain.EngineeringPolicy{Rules: map[string]domain.PolicyRule{"r": {Effect: domain.PolicyEffect{
		RequiredClaims: &map[string]domain.RequiredClaim{FileSizePolicyClaimID: {EvidenceClass: AssuranceEvidenceClass}},
	}}}}
	silent := domain.EngineeringPolicy{Rules: map[string]domain.PolicyRule{"r": {Effect: domain.PolicyEffect{
		RequiredClaims: &map[string]domain.RequiredClaim{"claim-validation": {EvidenceClass: AssuranceEvidenceClass}},
	}}}}
	rebuild := func(contract domain.EngineeringWorkContract) func() (domain.EngineeringWorkContract, error) {
		return func() (domain.EngineeringWorkContract, error) { return contract, nil }
	}
	required := dischargeContract()
	notRequired := dischargeContract()
	delete(notRequired.RequiredClaims, FileSizePolicyClaimID)

	if got := fileSizeBindingSuffixFor(listing, dischargeBase, rebuild(required)); got != "|file-size-policy@"+dischargeBase {
		t.Fatalf("a newly required check does not change the binding: %q", got)
	}
	for name, got := range map[string]string{
		"a policy that does not list the claim": fileSizeBindingSuffixFor(silent, dischargeBase, rebuild(required)),
		"a contract compiled without the claim": fileSizeBindingSuffixFor(listing, dischargeBase, rebuild(notRequired)),
		"a contract that cannot be rebuilt": fileSizeBindingSuffixFor(listing, dischargeBase, func() (domain.EngineeringWorkContract, error) {
			return domain.EngineeringWorkContract{}, context.Canceled
		}),
		"a run whose runtime is not yet attached": (&runState{}).fileSizeBindingSuffix(),
		"a policy with no rules":                  fileSizeBindingSuffixFor(domain.EngineeringPolicy{}, dischargeBase, rebuild(required)),
	} {
		if got != "" {
			t.Errorf("%s changed the binding: %q", name, got)
		}
	}
	// A rebuild that must not even be attempted when the policy cannot require the claim.
	attempted := false
	fileSizeBindingSuffixFor(silent, dischargeBase, func() (domain.EngineeringWorkContract, error) {
		attempted = true
		return required, nil
	})
	if attempted {
		t.Error("the contract was rebuilt although the loaded policy never lists the claim")
	}
}

// Older events stay readable and valid; the optional record is bounded and
// complete.
func TestTheCheckRecordIsOptionalBoundedAndComplete(t *testing.T) {
	validate := func(payload string) error {
		return validateEventPayload(EngineeringEvent{Type: EventAssuranceObserved, RunID: "run-d", Payload: json.RawMessage(payload)})
	}
	const head = `{"provider_id":"go","verifier_definition":"vd","passed":true,"commit":"c1","tree":"t1"`
	if err := validate(head + `}`); err != nil {
		t.Fatalf("an observation written before the record existed became invalid: %v", err)
	}
	decoded, err := decodePayload[AssuranceObservedPayload](json.RawMessage(head + `}`))
	if err != nil || decoded.Checks != nil {
		t.Fatalf("replay invented a record for an old observation: %+v %v", decoded.Checks, err)
	}
	good := `"checks":[{"kind":"file_size_policy","checker":"c","checker_revision":"r","outcome":"passed","commit":"c1","tree":"t1","base_revision":"b"}]`
	if err := validate(head + `,` + good + `}`); err != nil {
		t.Fatalf("a complete record was refused: %v", err)
	}
	for name, bad := range map[string]string{
		"no tree":         `"checks":[{"kind":"file_size_policy","checker":"c","checker_revision":"r","outcome":"passed","commit":"c1"}]`,
		"unknown outcome": `"checks":[{"kind":"file_size_policy","checker":"c","checker_revision":"r","outcome":"maybe","commit":"c1","tree":"t1"}]`,
		"unknown member":  `"checks":[{"kind":"file_size_policy","checker":"c","checker_revision":"r","outcome":"passed","commit":"c1","tree":"t1","approved":true}]`,
		"too many":        `"checks":[` + strings.TrimSuffix(strings.Repeat(`{"kind":"k","checker":"c","checker_revision":"r","outcome":"passed","commit":"c1","tree":"t1"},`, maxVerificationChecks+1), ",") + `]`,
	} {
		if err := validate(head + `,` + bad + `}`); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The verifier records the check it completed on whatever result follows, and
// a refusal records a failed one and never starts the suite.
func TestTheVerifierRecordsTheCheckItCompleted(t *testing.T) {
	dir := t.TempDir()
	base, _ := sizeRepo(t, dir, map[string]string{"ok.go": "package x\n"}, "base")
	cleanCommit, cleanTree := sizeRepo(t, dir, map[string]string{"small.go": "package y\n"}, "clean")
	grownCommit, grownTree := sizeRepo(t, dir, map[string]string{"grown.go": manyLines(1200)}, "grown")
	verifier := BaselineGoVerifier{ArtifactStore: ArtifactStore{Root: t.TempDir()}}
	request := func(commit, tree string) AssuranceRequest {
		mustGit(t, dir, "checkout", "-q", "--detach", commit) // the assurance checkout is AT the exact candidate
		return AssuranceRequest{
			RunID: "run-r", Commit: commit, Tree: tree, CheckoutDir: dir, Attempt: 1, Contract: Ref{ID: "c", Revision: "1"},
			Policy: Ref{ID: "p", Revision: "1"}, FileSizePolicyBase: base,
		}
	}

	// The suite cannot start here (no dependency cache), which is the point:
	// the completed host-side check is still recorded on the result.
	passed, _ := verifier.Assure(context.Background(), request(cleanCommit, cleanTree))
	if len(passed.Checks) != 1 || passed.Checks[0] != (VerificationCheck{
		Kind: CheckKindFileSizePolicy, Checker: fileSizeCheckerID, CheckerRevision: fileSizeCheckerRevision(),
		Outcome: CheckOutcomePassed, Commit: cleanCommit, Tree: cleanTree, BaseRevision: base,
	}) {
		t.Fatalf("the completed check was not recorded on the result: %+v", passed.Checks)
	}

	refused, err := verifier.Assure(context.Background(), request(grownCommit, grownTree))
	if err != nil || refused.Passed || len(refused.Checks) != 1 || refused.Checks[0].Outcome != CheckOutcomeFailed {
		t.Fatalf("a refusal did not record a failed check: %+v %v", refused, err)
	}
	if refused.Checks[0].Tree != grownTree || refused.Checks[0].BaseRevision != base {
		t.Fatalf("the failed record is not bound to the exact subject: %+v", refused.Checks[0])
	}

	// A checkout that is not the exact candidate is not read, and is not a
	// verdict about the candidate: nothing is recorded as completed.
	mismatched := request(cleanCommit, cleanTree)
	mustGit(t, dir, "checkout", "-q", "--detach", grownCommit)
	if result, done, err := verifier.fileSizeVerdict(mismatched); !done || err == nil || result.FailureClass != FailureAssurancePrerequisite || len(result.Checks) != 0 {
		t.Fatalf("a checkout that is not the exact candidate was read: done=%v err=%v %+v", done, err, result)
	}

	// A request that carries no requirement records no check at all.
	plain := request(cleanCommit, cleanTree)
	plain.FileSizePolicyBase = ""
	if result, _ := verifier.Assure(context.Background(), plain); len(result.Checks) != 0 {
		t.Fatalf("a check was recorded for a request that did not require it: %+v", result.Checks)
	}
}
