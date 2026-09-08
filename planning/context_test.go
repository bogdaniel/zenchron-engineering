package planning_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// Context compilation is where "different roles receive different context"
// stops being a diagram. Every test here asserts on the pack a worker would
// actually be handed.

func contextFixture(role domain.EngineeringRole) planning.ContextInput {
	boundaries := map[string]domain.CriticalBoundary{
		"authentication": {Type: "security", Paths: []string{"internal/auth"}},
	}
	profiles := []string{"security-sensitive"}
	return planning.ContextInput{
		Stage: domain.PlanStage{ID: "stage-" + string(role), Kind: domain.StageAgent, Role: role},
		Role:  role,
		Contract: domain.EngineeringWorkContract{
			SchemaVersion:    domain.SchemaVersion,
			ID:               "contract-sso",
			Revision:         "3",
			Objective:        "Add enterprise SSO.",
			AcceptanceIntent: []string{"Invalid sessions remain rejected"},
			Scope: domain.ContractScope{
				Stage:        domain.StageObserved,
				AllowedPaths: []string{"internal/auth/session.go", "internal/auth/sso.go"},
			},
			Invariants: map[string]domain.Requirement{
				"session-boundary": {Statement: "Session validation stays inside internal/auth."},
			},
			Obligations: map[string]domain.Requirement{
				"security-review": {Statement: "An independent security review must pass.", RequiredClaims: []string{"claim-security-review"}, Material: true},
			},
			RequiredClaims: map[string]domain.RequiredClaim{
				"claim-security-review": {EvidenceClass: "security_review", IndependentFromChangeProducer: true},
			},
			Permissions:  []domain.Action{{Type: "git.pull_request.create", Target: "acme/payments"}},
			Prohibitions: []domain.Action{{Type: "secret.read", Target: "*"}},
			PlanRequirements: &domain.PlanRequirements{
				Roles: []domain.RoleRequirement{{
					Role:      domain.RoleSecurityReviewer,
					Statement: "A security reviewer independent of the material producer is required.",
				}},
			},
		},
		Model: domain.ProjectModel{
			SchemaVersion:      domain.SchemaVersion,
			ID:                 "project-acme-payments",
			Revision:           "1",
			Subject:            domain.Subject{Repository: "acme/payments", Revision: "rev-b"},
			CriticalBoundaries: &boundaries,
			PolicyProfiles:     &profiles,
		},
		Facts: []domain.EngineeringFact{{
			ID: "fact-auth-2", Key: "authentication.boundary_modified", Value: domain.FactTrue,
			Stage: domain.StageObserved, Confidence: domain.ConfidenceHigh,
			Provenance: domain.FactProvenance{Type: "static_analysis", Producer: "scope-analyzer"},
		}},
		Upstream: []domain.UpstreamOutput{
			{StageID: "implementation", RunID: "run-impl", Candidate: "c0ffee", Tree: "7ea7", Summary: "SSO endpoints."},
		},
	}
}

func included(pack domain.ContextPack) map[domain.ContextClass]bool {
	set := map[domain.ContextClass]bool{}
	for _, class := range pack.Included {
		set[class] = true
	}
	return set
}

// The governance envelope is unconditional. No role row and no operator policy
// can drop it, because a worker that cannot see its obligations is a worker
// nothing can hold to them.
func TestEveryRoleReceivesTheGovernanceEnvelope(t *testing.T) {
	for _, role := range domain.EngineeringRoles() {
		pack, err := planning.CompileContext(contextFixture(role))
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		present := included(pack)
		for _, class := range domain.RequiredContextClasses() {
			if !present[class] {
				t.Fatalf("role %q was not shown required class %q", role, class)
			}
		}
		if pack.Objective == "" || len(pack.AcceptanceCriteria) == 0 ||
			len(pack.Obligations) == 0 || len(pack.Permissions) == 0 || len(pack.Prohibitions) == 0 {
			t.Fatalf("role %q received an empty envelope: %#v", role, pack)
		}
	}
}

// An implementer and an independent reviewer are given materially different
// context. If they were not, "independent review" would be one worker looking
// at the same desk twice.
func TestImplementerAndIndependentReviewerReceiveDifferentPacks(t *testing.T) {
	implementer, err := planning.CompileContext(contextFixture(domain.RoleImplementer))
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := planning.CompileContext(contextFixture(domain.RoleSecurityReviewer))
	if err != nil {
		t.Fatal(err)
	}
	implementerClasses, reviewerClasses := included(implementer), included(reviewer)

	if !implementerClasses[domain.ContextRepositoryPaths] || !implementerClasses[domain.ContextArchitectureNotes] {
		t.Fatalf("implementer was not shown where to work: %v", implementer.Included)
	}
	if implementerClasses[domain.ContextCandidateDiff] {
		t.Fatal("the implementer was handed a candidate diff to review rather than work to do")
	}
	if !reviewerClasses[domain.ContextCandidateDiff] || !reviewerClasses[domain.ContextPolicyExcerpts] {
		t.Fatalf("the reviewer was not shown the change or the standard: %v", reviewer.Included)
	}
	if len(reviewer.PolicyExcerpts) == 0 {
		t.Fatal("the reviewer received no policy excerpt to judge against")
	}
	if len(implementer.PolicyExcerpts) != 0 {
		t.Fatalf("the implementer received review-standard excerpts: %v", implementer.PolicyExcerpts)
	}
	if reflect.DeepEqual(implementer.Included, reviewer.Included) {
		t.Fatal("two different roles received identical context selections")
	}
}

// No role, and no policy, ever receives another worker's hidden reasoning.
func TestProducerReasoningIsNeverDelivered(t *testing.T) {
	for _, role := range domain.EngineeringRoles() {
		pack, err := planning.CompileContext(contextFixture(role))
		if err != nil {
			t.Fatal(err)
		}
		if included(pack)[domain.ContextProducerReasoning] {
			t.Fatalf("role %q was handed the producer's reasoning transcript", role)
		}
	}

	// Asking for it is refused rather than silently dropped: an operator who
	// wrote it down believes their reviewer is getting it.
	input := contextFixture(domain.RoleSecurityReviewer)
	input.Policy = &domain.ContextPolicy{
		ID: "leaky", Include: append(domain.RequiredContextClasses(), domain.ContextProducerReasoning),
	}
	_, err := planning.CompileContext(input)
	if err == nil || !strings.Contains(err.Error(), "never delivered to another worker") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	var contextErr *planning.ContextError
	if !errorsAs(err, &contextErr) {
		t.Fatalf("expected a typed ContextError, got %T", err)
	}
}

// A ContextPolicy narrows the OPTIONAL classes and nothing else.
func TestContextPolicyNarrowsExactlyTheOptionalClasses(t *testing.T) {
	input := contextFixture(domain.RoleSecurityReviewer)
	input.Policy = &domain.ContextPolicy{ID: "diff-only", Exclude: []domain.ContextClass{domain.ContextUpstreamOutputs, domain.ContextProjectFacts}}
	pack, err := planning.CompileContext(input)
	if err != nil {
		t.Fatal(err)
	}
	present := included(pack)
	if present[domain.ContextUpstreamOutputs] || present[domain.ContextProjectFacts] {
		t.Fatalf("excluded classes were still selected: %v", pack.Included)
	}
	if len(pack.UpstreamOutputs) != 0 || len(pack.Facts) != 0 {
		t.Fatalf("excluded classes still carried content: %#v", pack)
	}
	if !present[domain.ContextCandidateDiff] || !present[domain.ContextPolicyExcerpts] {
		t.Fatalf("narrowing removed a class the policy did not name: %v", pack.Included)
	}
	for _, class := range domain.RequiredContextClasses() {
		if !present[class] {
			t.Fatalf("narrowing removed required class %q", class)
		}
	}

	// An inclusion list cannot ADD a class the role was never entitled to:
	// that would make a context policy a quiet capability grant.
	widening := contextFixture(domain.RoleSecurityReviewer)
	widening.Policy = &domain.ContextPolicy{
		ID:      "wider",
		Include: append(domain.RequiredContextClasses(), domain.ContextRepositoryPaths, domain.ContextCandidateDiff),
	}
	wider, err := planning.CompileContext(widening)
	if err != nil {
		t.Fatal(err)
	}
	if included(wider)[domain.ContextRepositoryPaths] {
		t.Fatal("an inclusion list added a class the role's selection never contained")
	}
}

// The compiler still fails closed if a policy that erases the envelope reaches
// it from any path other than the registry loader.
func TestPolicyErasingTheEnvelopeIsRefusedAtCompileTimeToo(t *testing.T) {
	input := contextFixture(domain.RoleImplementer)
	input.Policy = &domain.ContextPolicy{ID: "no-obligations", Exclude: []domain.ContextClass{domain.ContextObligations}}
	if _, err := planning.CompileContext(input); err == nil || !strings.Contains(err.Error(), "not narrowable") {
		t.Fatalf("expected the envelope to be unnarrowable, got %v", err)
	}
}

// Included and Excluded together are the whole vocabulary, and they are
// disjoint: "not shown the diff" and "there was no diff" have to be
// distinguishable by a reader of the plan.
func TestIncludedAndExcludedAreATruthfulPartition(t *testing.T) {
	pack, err := planning.CompileContext(contextFixture(domain.RoleReviewer))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[domain.ContextClass]int{}
	for _, class := range pack.Included {
		seen[class]++
	}
	for _, class := range pack.Excluded {
		seen[class]++
	}
	for _, class := range domain.ContextClasses() {
		if seen[class] != 1 {
			t.Fatalf("class %q appears %d times across included/excluded", class, seen[class])
		}
	}
	if len(seen) != len(domain.ContextClasses()) {
		t.Fatalf("the audit trail covers %d classes, the vocabulary has %d", len(seen), len(domain.ContextClasses()))
	}
}

// The independence obligation reaches the worker as a sentence. A reviewer that
// does not know it is the independent leg cannot behave like one.
func TestIndependenceBindingsAreStatedToTheWorker(t *testing.T) {
	input := contextFixture(domain.RoleSecurityReviewer)
	input.Independence = []domain.IndependenceBinding{{
		Dimension: domain.IndependenceVendorFamily, DifferentFrom: []string{"implementation"},
		Class: "anthropic", OtherClasses: []string{"openai"}, SatisfiedBy: domain.IndependenceSatisfiedByWorker,
	}}
	pack, err := planning.CompileContext(input)
	if err != nil {
		t.Fatal(err)
	}
	var stated bool
	for _, obligation := range pack.Obligations {
		if strings.Contains(obligation, "independent leg") && strings.Contains(obligation, "vendor_family") {
			stated = true
		}
	}
	if !stated {
		t.Fatalf("the independence boundary was never stated to the worker: %v", pack.Obligations)
	}
}

// Same inputs, byte-identical document. A plan revision digest is only worth
// something if the context it covers is stable.
func TestContextCompilationIsDeterministic(t *testing.T) {
	first, err := planning.CompileContext(contextFixture(domain.RoleImplementer))
	if err != nil {
		t.Fatal(err)
	}
	second, err := planning.CompileContext(contextFixture(domain.RoleImplementer))
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := domain.CanonicalJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := domain.CanonicalJSON(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("context compilation is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
}

// The #67 boundary, checked structurally rather than promised. ContextPack v0
// consumes ProjectModel v1 and the compiled contract AS THEY ARE; it must not
// grow a repository walk, a dependency graph or any other analysis of its own.
// A compiler that cannot import a filesystem cannot perform one.
func TestContextCompilerCannotReachAFilesystemOrNetwork(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "context.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{`"os"`, `"io"`, `"io/fs"`, `"path/filepath"`, `"net"`, `"net/http"`, `"os/exec"`, `"database/sql"`}
	for _, imported := range file.Imports {
		for _, deny := range forbidden {
			if imported.Path.Value == deny {
				t.Fatalf("the context compiler imports %s: repository analysis belongs to #67, and this compiler consumes established facts only", deny)
			}
		}
	}
}

// errorsAs keeps the assertion above readable without importing errors twice.
func errorsAs(err error, target **planning.ContextError) bool {
	for err != nil {
		if typed, ok := err.(*planning.ContextError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
