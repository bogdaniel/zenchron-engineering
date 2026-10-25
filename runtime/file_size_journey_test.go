package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// #663 / #671 acceptance. The unit tests of the size policy prove the rules; this
// proves the GOVERNED LIFECYCLE enforces them: a run whose contract requires the
// claim cannot reach authority or publication with an oversized candidate, is
// remediated through the ordinary route, and only a fresh exact-subject check
// record lets the claim - and so authority - through.
//
// Only the Docker suite is a double. The size check, its judged verdict, the
// artifact the remediating agent reads, the recorded check and its discharge are
// the production code, driven by the real reconciler.

// sizeAssurance is BaselineGoVerifier.Assure with the container replaced: the
// host-side size verdict is the real one, and the check record is appended
// exactly as Assure appends it after the suite.
type sizeAssurance struct {
	host      BaselineGoVerifier
	suite     *FakeAssuranceProvider
	requests  []AssuranceRequest
	suiteRuns int
}

func (a *sizeAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return a.suite.ProducedEvidenceClasses()
}

func (a *sizeAssurance) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	a.requests = append(a.requests, request)
	required := request.FileSizePolicyBase != ""
	if required {
		if result, done, err := a.host.fileSizeVerdict(request); done {
			return result, err
		}
	}
	a.suiteRuns++
	result, err := a.suite.Assure(ctx, request)
	if required {
		result.Checks = append(result.Checks, fileSizeCheck(request, CheckOutcomePassed))
	}
	return result, err
}

// withFileSizeClaim is the operator's policy change (docs/file-size-policy.md):
// the claim is required, and publication is conditioned on it.
//
// It is policy.withFileSizeObligation (policy/file_size_claim_test.go) applied
// rule for rule, because that helper lives in the external policy_test package
// and cannot be imported, and the runtime must not be given a production export
// for a test's sake. The additions are exactly the ones docs/file-size-policy.md
// lists, on every rule, keeping every existing member: the required claim, a
// MATERIAL obligation backed by it, the claim appended to each authority
// condition, and the claim appended to the acceptance-discharge claims.
func withFileSizeClaim(policy domain.EngineeringPolicy) domain.EngineeringPolicy {
	out := policy
	out.Revision = "2"
	out.Rules = map[string]domain.PolicyRule{}
	for id, rule := range policy.Rules {
		effect := rule.Effect
		claims := map[string]domain.RequiredClaim{}
		if effect.RequiredClaims != nil {
			for name, claim := range *effect.RequiredClaims {
				claims[name] = claim
			}
		}
		claims[FileSizePolicyClaimID] = domain.RequiredClaim{EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true}
		effect.RequiredClaims = &claims
		obligations := map[string]domain.PolicyRequirement{}
		if effect.Obligations != nil {
			for name, obligation := range *effect.Obligations {
				obligations[name] = obligation
			}
		}
		obligations[fileSizeObligationID] = domain.PolicyRequirement{
			Statement:      "The repository's file-size policy must hold on the exact candidate tree.",
			RequiredClaims: &[]string{FileSizePolicyClaimID},
			Material:       true,
		}
		effect.Obligations = &obligations
		if effect.AuthorityConditions != nil {
			conditions := make([]domain.AuthorityCondition, 0, len(*effect.AuthorityConditions))
			for _, condition := range *effect.AuthorityConditions {
				condition.RequiredClaims = append(append([]string(nil), condition.RequiredClaims...), FileSizePolicyClaimID)
				conditions = append(conditions, condition)
			}
			effect.AuthorityConditions = &conditions
		}
		discharge := []string{FileSizePolicyClaimID}
		if effect.AcceptanceDischargeClaims != nil {
			discharge = append(append([]string(nil), *effect.AcceptanceDischargeClaims...), FileSizePolicyClaimID)
		}
		effect.AcceptanceDischargeClaims = &discharge
		out.Rules[id] = domain.PolicyRule{When: rule.When, Effect: effect}
	}
	return out
}

const fileSizeObligationID = "file-size-policy"

// requireCompleteFileSizeConfiguration asserts the whole operator configuration
// reached what the run is governed by: the rebuilt contract requires the claim,
// carries the MATERIAL obligation it backs, gates publication on it, and the
// policy the contract was compiled from lists the claim among the claims that
// discharge acceptance.
func (f *phase8Fixture) requireCompleteFileSizeConfiguration(state *runState) domain.EngineeringWorkContract {
	f.t.Helper()
	kernel, err := f.runtime.buildKernel(state)
	if err != nil {
		f.t.Fatal(err)
	}
	contract := kernel.Contract
	if claim, ok := contract.RequiredClaims[FileSizePolicyClaimID]; !ok || claim.EvidenceClass != AssuranceEvidenceClass {
		f.t.Fatalf("the contract does not require the claim: %+v", contract.RequiredClaims)
	}
	obligation, ok := contract.Obligations[fileSizeObligationID]
	if !ok || !obligation.Material {
		f.t.Fatalf("the contract does not carry the file-size obligation as material: %+v", contract.Obligations)
	}
	var gated bool
	for _, condition := range contract.AuthorityConditions {
		for _, id := range condition.RequiredClaims {
			gated = gated || id == FileSizePolicyClaimID
		}
	}
	if !gated {
		f.t.Fatalf("no protected action waits on the claim: %+v", contract.AuthorityConditions)
	}
	for id, rule := range f.deps.Policy.Rules {
		var discharges bool
		if rule.Effect.AcceptanceDischargeClaims != nil {
			for _, claim := range *rule.Effect.AcceptanceDischargeClaims {
				discharges = discharges || claim == FileSizePolicyClaimID
			}
		}
		if !discharges {
			f.t.Fatalf("the policy the contract was compiled from does not list the claim among rule %q's acceptance-discharge claims", id)
		}
	}
	return contract
}

// newSizeJourney is the standard governed-run fixture over a base that carries
// both size manifests, with the size-aware verifier standing in for assurance.
func newSizeJourney(t *testing.T) (*phase8Fixture, *sizeAssurance) {
	t.Helper()
	fixture := newPhase8Fixture(t, func(origin string) {
		for name, body := range map[string]string{
			fileSizeBaselineFile: "", fileSizeExceptionFile: "", "ok.go": "package ok\n",
		} {
			if err := os.WriteFile(filepath.Join(origin, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	assurance := &sizeAssurance{
		host:  BaselineGoVerifier{ArtifactStore: fixture.deps.Artifacts},
		suite: passingAssurance(),
	}
	fixture.useAssurance(assurance)
	return fixture, assurance
}

func (f *phase8Fixture) requireFileSizeClaim() {
	f.t.Helper()
	f.deps.Policy = withFileSizeClaim(f.deps.Policy)
	f.runtime = f.newRuntime(f.deps)
}

// reconcileUntil drives the real reconciler until done reports true or the run
// stops, and fails the test if neither happens within a bounded number of passes.
func (f *phase8Fixture) reconcileUntil(runID string, done func(*runState) bool) {
	f.t.Helper()
	for pass := 0; pass < 60; pass++ {
		outcome := f.reconcile(runID)
		if done(f.state(runID)) {
			return
		}
		if outcome.Disposition == Failed {
			f.t.Fatalf("the run failed before the expected state: %+v", outcome)
		}
	}
	f.t.Fatalf("the run did not reach the expected state in 60 passes: %v", journalTypes(journalOf(f.t, f.runtime, runID)))
}

func firstSequence(events []EngineeringEvent, eventType string) int64 {
	for _, event := range events {
		if event.Type == eventType {
			return event.Sequence
		}
	}
	return 0
}

// THE JOURNEY. An oversized candidate is judged before the suite and goes back
// to its producer with the file, the line count and the limit; a candidate that
// also edits the exceptions manifest to pass is refused all the same, because the
// base revision's manifests govern; a compliant candidate gets a fresh check
// record bound to its exact commit, tree and base, and only then reaches
// authority and publication.
func TestAnOversizedCandidateCannotReachAuthorityUntilItsFileSizeCheckPasses(t *testing.T) {
	fixture, assurance := newSizeJourney(t)
	fixture.requireFileSizeClaim()
	invocation := 0
	fixture.provider.mutate = func(dir string) error {
		invocation++
		write := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		switch invocation {
		case 1: // a producer that ships a 1,200-line file
			write("grown.go", "package grown\n"+manyLines(1199))
		case 2: // the same file again, and the exceptions manifest raised to let it through
			write("grown.go", "package grown\n"+manyLines(1198))
			write(fileSizeExceptionFile, "grown.go\t5000\tpretend this file is approved\n")
		default: // remediated for real: two cohesive files, the manifests as they were
			if err := os.Remove(filepath.Join(dir, "grown.go")); err != nil {
				t.Fatal(err)
			}
			write(fileSizeExceptionFile, "")
			write("grown_a.go", "package grown\n"+manyLines(599))
			write("grown_b.go", "package grown\n"+manyLines(599))
		}
		return nil
	}

	runID := fixture.start()
	fixture.reconcileUntil(runID, func(state *runState) bool { return state.published() })
	events := journalOf(t, fixture.runtime, runID)
	observed := automatedAssurancePayloads(t, events)

	var failures, passes []AssuranceObservedPayload
	for _, payload := range observed {
		if payload.Passed {
			passes = append(passes, payload)
		} else {
			failures = append(failures, payload)
		}
	}
	if len(passes) != 1 || len(failures) < 2 {
		t.Fatalf("want at least two refused candidates then one that passes, got %d refused and %d passing: %+v", len(failures), len(passes), observed)
	}

	// (1) Judged by the size policy before any suite ran, as a verification
	// failure that yields no evidence bundle. The only record it carries is a
	// FAILED check bound to the exact commit, tree and base: a refusal proves
	// nothing for the claim.
	for _, failure := range failures {
		if failure.FailureClass != FailureVerification || failure.Bundle != (Ref{}) || len(failure.Checks) != 1 {
			t.Fatalf("a refused candidate was not a verification failure with one failed check and no evidence: %+v", failure)
		}
		check := failure.Checks[0]
		if check.Kind != CheckKindFileSizePolicy || check.Outcome != CheckOutcomeFailed ||
			check.Commit != failure.Commit || check.Tree != failure.Tree || check.BaseRevision != fixture.base {
			t.Fatalf("the refusal is not bound to the exact candidate and base: %+v", check)
		}
	}
	if assurance.suiteRuns != 1 {
		t.Fatalf("the suite ran %d times; only the compliant candidate may reach it", assurance.suiteRuns)
	}

	// (2) The producer is shown the finding and the route is the ordinary
	// remediation: file, line count and limit are named, and the manifests are
	// declared off limits.
	if len(fixture.provider.requests) != 3 {
		t.Fatalf("the producer ran %d times, want the original and two remediations", len(fixture.provider.requests))
	}
	for i, request := range fixture.provider.requests[1:] {
		if request.Purpose != InvocationRemediation {
			t.Fatalf("invocation %d was %q, want a remediation", i+2, request.Purpose)
		}
		var diagnostic string
		for _, finding := range request.Findings {
			if finding.Classification == FailureVerification {
				diagnostic += finding.Diagnostic
			}
		}
		for _, want := range []string{"grown.go", "exceeds hard cap 1000", "do not edit " + fileSizeBaselineFile + " or " + fileSizeExceptionFile} {
			if !strings.Contains(diagnostic, want) {
				t.Fatalf("remediation %d was not told %q: %q", i+1, want, diagnostic)
			}
		}
	}

	// (4) The candidate that raised its own limit was still refused, on the base
	// revision's rules: its report quotes the hard cap and its own line count, not
	// the 5,000 it wrote into the manifest.
	if !strings.Contains(findingsText(fixture.provider.requests[1]), `FAIL 1200 "grown.go"`) ||
		!strings.Contains(findingsText(fixture.provider.requests[2]), `FAIL 1199 "grown.go": exceeds hard cap 1000`) {
		t.Fatalf("the findings do not describe each refused candidate: %q / %q",
			findingsText(fixture.provider.requests[1]), findingsText(fixture.provider.requests[2]))
	}

	// (3) The compliant candidate carries a fresh record bound to its exact
	// commit, tree and the base the rules came from, and only then does the run
	// reach authority, push and a pull request.
	pass := passes[0]
	if len(pass.Checks) != 1 {
		t.Fatalf("the compliant candidate has %d check records, want one: %+v", len(pass.Checks), pass.Checks)
	}
	check := pass.Checks[0]
	if check.Kind != CheckKindFileSizePolicy || check.Outcome != CheckOutcomePassed ||
		check.Commit != pass.Commit || check.Tree != pass.Tree || check.BaseRevision != fixture.base {
		t.Fatalf("the check record is not bound to the exact subject and base: %+v (candidate %s/%s, base %s)", check, pass.Commit, pass.Tree, fixture.base)
	}
	authorityAt := firstSequence(events, EventAuthorityEvaluated)
	if authorityAt == 0 {
		t.Fatalf("a published run has no authority evaluation: %v", journalTypes(events))
	}
	// Every observation, the refusals and the passing one, precedes the first
	// authority evaluation: authority was reached only through the check record.
	for _, event := range events {
		if event.Type == EventAssuranceObserved && event.Sequence > authorityAt {
			t.Fatalf("an assurance observation arrived after authority was already evaluated: %v", journalTypes(events))
		}
	}
	for _, evaluated := range journalPayloads[AuthorityEvaluatedPayload](t, events, EventAuthorityEvaluated) {
		if evaluated.Status != domain.AuthorityAuthorized {
			t.Fatalf("authority was evaluated on a candidate that did not pass: %+v", evaluated)
		}
	}
	state := fixture.state(runID)
	contract := fixture.requireCompleteFileSizeConfiguration(state)
	bundles, err := fixture.runtime.evidenceBundles(state, contract)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]domain.EvidenceItem{}
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			claims[item.ClaimID] = item
		}
	}
	if item, ok := claims[FileSizePolicyClaimID]; !ok || item.Producer.ID != fileSizeCheckerID || item.Environment.Identifier != fileSizeHostEnvironmentID {
		t.Fatalf("the claim is not discharged by the host-side check record: %+v", claims)
	}
	if countMethod(fixture.forge.Calls, "CreatePullRequest") != 1 {
		t.Fatalf("want exactly one pull request, for the compliant candidate: %v", fixture.forge.Calls)
	}
}

func findingsText(request ExecutionRequest) string {
	var out strings.Builder
	for _, finding := range request.Findings {
		out.WriteString(finding.Diagnostic + "\n")
	}
	return out.String()
}

// THE HANDOVER. A run was assured while the policy did not ask for the size
// claim: its passing observation carries no check record. A controller that then
// loads a policy requiring the claim must not read that observation as proof; the
// assurance operation's binding changes, so the missing verification is planned
// through the ordinary lifecycle for the SAME candidate, and only the fresh
// exact-subject record discharges the claim and lets the run reach authority.
func TestARunAssuredBeforeTheRequirementExistedIsVerifiedAgainWhenItDoes(t *testing.T) {
	fixture, assurance := newSizeJourney(t)
	runID := fixture.start()
	fixture.crash(runID, func(call GitHubCall) bool {
		return call.Method == "RefSHA" && strings.HasPrefix(call.Ref, "zenchron/")
	})

	state := fixture.state(runID)
	oldKey, _ := bindAssuranceGo(state)
	before := automatedAssurancePayloads(t, journalOf(t, fixture.runtime, runID))
	if len(before) != 1 || !before[0].Passed || len(before[0].Checks) != 0 || assurance.requests[0].FileSizePolicyBase != "" {
		t.Fatalf("the earlier observation should be a plain passing suite with no record: %+v", before)
	}
	if !state.satisfied(OpAssuranceGo, oldKey) || strings.Contains(oldKey, "file-size-policy@") {
		t.Fatalf("the earlier assurance was not satisfied under a binding without the size check: %q", oldKey)
	}

	// The handover: the same journal, a controller whose policy now requires the claim.
	fixture.requireFileSizeClaim()
	state = fixture.state(runID)
	newKey, _ := bindAssuranceGo(state)
	if !strings.Contains(newKey, "|file-size-policy@") || newKey == oldKey || state.satisfied(OpAssuranceGo, newKey) {
		t.Fatalf("the requirement did not make verification eligible again: old %q new %q", oldKey, newKey)
	}
	contract := fixture.requireCompleteFileSizeConfiguration(state)
	bundles, err := fixture.runtime.evidenceBundles(state, contract)
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			if item.ClaimID == FileSizePolicyClaimID {
				t.Fatalf("an observation with no check record discharged the file-size claim: %+v", item)
			}
		}
	}

	// The run carries on: assurance is re-planned for the same candidate, runs the
	// size check, and only then does the run reach publication.
	fixture.reconcileUntil(runID, func(state *runState) bool { return state.published() })
	events := journalOf(t, fixture.runtime, runID)
	after := automatedAssurancePayloads(t, events)
	if len(after) != 2 || len(assurance.requests) != 2 || assurance.suiteRuns != 2 {
		t.Fatalf("want the old observation and one fresh verification, got %d observations, %d requests, %d suite runs",
			len(after), len(assurance.requests), assurance.suiteRuns)
	}
	fresh := after[1]
	if fresh.Commit != before[0].Commit || fresh.Tree != before[0].Tree {
		t.Fatalf("the candidate changed between the two verifications: %s/%s -> %s/%s", before[0].Commit, before[0].Tree, fresh.Commit, fresh.Tree)
	}
	if len(fresh.Checks) != 1 || fresh.Checks[0].Outcome != CheckOutcomePassed || fresh.Checks[0].Commit != fresh.Commit ||
		fresh.Checks[0].Tree != fresh.Tree || fresh.Checks[0].BaseRevision != fixture.base || assurance.requests[1].FileSizePolicyBase != fixture.base {
		t.Fatalf("the fresh verification is not an exact-subject check record: %+v", fresh.Checks)
	}
	state = fixture.state(runID)
	contract = fixture.requireCompleteFileSizeConfiguration(state)
	bundles, err = fixture.runtime.evidenceBundles(state, contract)
	if err != nil {
		t.Fatal(err)
	}
	discharged := false
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			discharged = discharged || (item.ClaimID == FileSizePolicyClaimID && item.Result.Status == domain.EvidencePassed)
		}
	}
	if !discharged {
		t.Fatal("the fresh record did not discharge the claim")
	}
}
