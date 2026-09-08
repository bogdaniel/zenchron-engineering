package planning

// The ContextPack compiler.
//
// Each assignment receives the MINIMUM USEFUL context for the responsibility it
// carries, not the whole repository and not the previous worker's session. Two
// laws shape everything below:
//
//   - Different roles receive different context. An independent reviewer that
//     inherits the implementer's reasoning is not independent, it is the same
//     worker with a second name.
//   - A ContextPolicy may NARROW the optional classes and may never erase the
//     governance envelope. A worker that cannot see its obligations is a worker
//     nothing can hold to them.
//
// The boundary with #67 is structural rather than promised: this file consumes
// the compiled work contract, the ProjectModel v1 it was compiled against and
// the facts that were already established. It performs NO repository analysis
// of its own - no walk, no dependency graph, no file read - which is why it
// imports nothing that could reach a filesystem. When #67 enriches ProjectModel,
// this compiler consumes the richer facts unchanged.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ContextInput is everything the compiler is allowed to see. It is all DATA:
// there is no repository handle, no workspace path and no store, so "this
// compiler cannot go and look something up" is a property of the signature
// rather than a rule somebody has to remember.
type ContextInput struct {
	Stage    domain.PlanStage
	Role     domain.EngineeringRole
	Contract domain.EngineeringWorkContract
	// Model is ProjectModel v1, consumed exactly as it is.
	Model domain.ProjectModel
	Facts []domain.EngineeringFact
	// Policy is the operator-owned ContextPolicy resolved from the stage or the
	// profile. Nil means no narrowing, which is the honest default for an
	// operator who has not stated one.
	Policy *domain.ContextPolicy
	// Upstream is the accepted output of upstream stages. It is EXPLICIT: an
	// integrator consumes stated outputs rather than inheriting a conversation.
	Upstream []domain.UpstreamOutput
	// Independence is this stage's resolved independence obligations. They
	// reach the worker as sentences, so the boundary it is part of is something
	// it was told rather than something only the plan knows.
	Independence []domain.IndependenceBinding
}

// ContextError is the typed refusal for a context selection that would leave a
// worker unable to be held to its obligations, or would hand it another
// worker's reasoning.
type ContextError struct {
	Stage  string
	Detail string
}

func (e *ContextError) Error() string {
	if e.Stage == "" {
		return "context compilation: " + e.Detail
	}
	return "context compilation for stage " + e.Stage + ": " + e.Detail
}

// roleContextClasses is the stated role -> optional-class table.
//
// It is a TABLE rather than a chain of conditionals because "which role sees
// what" is the whole substance of context compilation: an operator reviewing
// this file should be able to read the policy off it, and a new role should be
// a new row rather than a new branch somewhere in a function.
//
// The required classes are deliberately absent from every row - they are added
// unconditionally, so no row can omit one by accident.
var roleContextClasses = map[domain.EngineeringRole][]domain.ContextClass{
	// Producers need to know where they may work, what shape the system has,
	// what upstream stages settled, and what previous attempts found.
	domain.RoleImplementer: {
		domain.ContextProjectFacts, domain.ContextArchitectureNotes,
		domain.ContextRepositoryPaths, domain.ContextUpstreamOutputs, domain.ContextPriorFindings,
	},
	domain.RoleIntegrator: {
		domain.ContextProjectFacts, domain.ContextArchitectureNotes,
		domain.ContextRepositoryPaths, domain.ContextUpstreamOutputs, domain.ContextPriorFindings,
	},
	domain.RoleTester: {
		domain.ContextProjectFacts, domain.ContextArchitectureNotes,
		domain.ContextRepositoryPaths, domain.ContextUpstreamOutputs, domain.ContextPriorFindings,
	},
	// Reviewers judge the CHANGE. They get the candidate diff, the policy
	// excerpts that say what the change is being judged against, and the stated
	// upstream outputs - and never the producer's reasoning, which is what
	// keeps an independent review independent.
	domain.RoleReviewer: {
		domain.ContextProjectFacts, domain.ContextPolicyExcerpts,
		domain.ContextUpstreamOutputs, domain.ContextCandidateDiff,
	},
	domain.RoleSecurityReviewer: {
		domain.ContextProjectFacts, domain.ContextPolicyExcerpts,
		domain.ContextUpstreamOutputs, domain.ContextCandidateDiff,
	},
	domain.RoleReleaseReviewer: {
		domain.ContextProjectFacts, domain.ContextPolicyExcerpts,
		domain.ContextUpstreamOutputs, domain.ContextCandidateDiff,
	},
	// Architects reason about shape before there is a change to look at, so
	// they get structure and no diff.
	domain.RoleSystemArchitect: {
		domain.ContextProjectFacts, domain.ContextArchitectureNotes, domain.ContextRepositoryPaths,
	},
	domain.RoleProductArchitect: {
		domain.ContextProjectFacts, domain.ContextArchitectureNotes, domain.ContextRepositoryPaths,
	},
	// The planner reasons about what work is required. It reads; it does not
	// judge a candidate and does not produce one.
	domain.RolePlanner: {
		domain.ContextProjectFacts, domain.ContextRepositoryPaths,
	},
}

// CompileContext builds one assignment's ContextPack.
//
// It is deterministic: every collection is emitted in a stated canonical order,
// so the same inputs produce a byte-identical document and a plan revision
// digest means what it says.
func CompileContext(input ContextInput) (domain.ContextPack, error) {
	refuse := func(detail string) (domain.ContextPack, error) {
		return domain.ContextPack{}, &ContextError{Stage: input.Stage.ID, Detail: detail}
	}
	optional, known := roleContextClasses[input.Role]
	if !known {
		return refuse(fmt.Sprintf("role %q has no stated context selection; a role nothing states context for would receive whatever happened to be in scope", input.Role))
	}

	selected, err := selectClasses(optional, input.Policy)
	if err != nil {
		return refuse(err.Error())
	}

	objective := strings.TrimSpace(input.Stage.Objective)
	if objective == "" {
		objective = strings.TrimSpace(input.Contract.Objective)
	}
	if objective == "" {
		return refuse("neither the stage nor the contract states an objective, so the worker would be asked for nothing in particular")
	}
	acceptance := sortedUniqueStrings(input.Contract.AcceptanceIntent)
	if len(acceptance) == 0 {
		return refuse("the contract carries no acceptance criterion, so nothing would say what finishing means")
	}

	pack := domain.ContextPack{
		Objective:          objective,
		AcceptanceCriteria: acceptance,
		// The governance envelope is unconditional. It is added before any
		// role or policy selection is consulted, so no table row and no
		// operator policy can drop it.
		Obligations:  append(requirementStatements(input.Contract.Obligations), independenceStatements(input.Stage.ID, input.Independence)...),
		Permissions:  actionStatements(input.Contract.Permissions),
		Prohibitions: actionStatements(input.Contract.Prohibitions),
	}
	pack.Obligations = sortedUniqueStrings(pack.Obligations)

	if selected[domain.ContextProjectFacts] {
		pack.Facts = factStatements(input.Facts)
	}
	if selected[domain.ContextArchitectureNotes] {
		pack.ArchitectureNotes = architectureNotes(input.Contract, input.Model)
	}
	if selected[domain.ContextPolicyExcerpts] {
		pack.PolicyExcerpts = policyExcerpts(input.Contract, input.Model)
	}
	if selected[domain.ContextRepositoryPaths] {
		pack.RepositoryPaths = sortedUniqueStrings(input.Contract.Scope.AllowedPaths)
	}
	if selected[domain.ContextUpstreamOutputs] {
		pack.UpstreamOutputs = sortedUpstream(input.Upstream)
	}

	// The audit trail. Included is what this assignment was selected to
	// receive; Excluded is everything in the vocabulary it was not. Both are
	// recorded even where a selected class happened to carry nothing, because
	// "the reviewer was not shown the diff" and "there was no diff yet" are
	// different facts and a reviewer of the plan needs to tell them apart.
	//
	// ContextCandidateDiff and ContextPriorFindings have no member of their own
	// here: the runtime delivers the candidate workspace and the findings
	// through the mechanisms that already exist. Their presence in Included is
	// what states that this assignment is entitled to them.
	for _, class := range domain.ContextClasses() {
		if selected[class] {
			pack.Included = append(pack.Included, class)
			continue
		}
		pack.Excluded = append(pack.Excluded, class)
	}
	return pack, nil
}

// selectClasses resolves the optional classes for one role through the
// operator's ContextPolicy.
//
// The required classes are added here too, so Included tells the whole truth
// about what the worker receives rather than only the optional part.
func selectClasses(optional []domain.ContextClass, policy *domain.ContextPolicy) (map[domain.ContextClass]bool, error) {
	selected := make(map[domain.ContextClass]bool, len(optional)+len(domain.RequiredContextClasses()))
	for _, class := range domain.RequiredContextClasses() {
		selected[class] = true
	}
	for _, class := range optional {
		selected[class] = true
	}
	if policy == nil {
		// The producer's own reasoning is never selectable, so no role table
		// row can contain it and no default can reintroduce it.
		delete(selected, domain.ContextProducerReasoning)
		return selected, nil
	}
	required := make(map[domain.ContextClass]bool, len(domain.RequiredContextClasses()))
	for _, class := range domain.RequiredContextClasses() {
		required[class] = true
	}
	// The registry refuses a policy like this at load time. This is the second
	// layer: a policy reaching the compiler from any other path - a test, a
	// future caller, a widened loader - still cannot erase the envelope.
	for _, class := range policy.Exclude {
		if required[class] {
			return nil, fmt.Errorf("context policy %q excludes required class %q: the governance envelope is not narrowable", policy.ID, class)
		}
	}
	if len(policy.Include) > 0 {
		narrowed := make(map[domain.ContextClass]bool, len(policy.Include))
		for class := range required {
			narrowed[class] = true
		}
		for _, class := range policy.Include {
			// An inclusion list NARROWS the role's optional set. It cannot add
			// a class the role was never entitled to, which is what stops a
			// context policy from being a quiet capability grant.
			if selected[class] {
				narrowed[class] = true
			}
		}
		selected = narrowed
	}
	for _, class := range policy.Exclude {
		delete(selected, class)
	}
	// Whatever a policy asked for, one worker never receives another's hidden
	// reasoning. It is refused rather than silently dropped, because an
	// operator who wrote it down believes their reviewer is getting it.
	if requestsProducerReasoning(policy) {
		return nil, fmt.Errorf("context policy %q requests %q: a producer's hidden reasoning transcript is never delivered to another worker, and a review that inherited one would not be independent",
			policy.ID, domain.ContextProducerReasoning)
	}
	delete(selected, domain.ContextProducerReasoning)
	return selected, nil
}

func requestsProducerReasoning(policy *domain.ContextPolicy) bool {
	for _, class := range policy.Include {
		if class == domain.ContextProducerReasoning {
			return true
		}
	}
	return false
}

// independenceStatements tell the worker the boundary it is part of. A reviewer
// that does not know it is the independent leg cannot behave like one.
func independenceStatements(stageID string, bindings []domain.IndependenceBinding) []string {
	statements := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		peers := sortedUniqueStrings(binding.DifferentFrom)
		statements = append(statements, fmt.Sprintf(
			"This stage (%s) is the independent leg for %s: its producer differs in the %s dimension, and the work of stage %s may not be treated as its own evidence.",
			stageID, strings.Join(peers, ", "), binding.Dimension, strings.Join(peers, ", ")))
	}
	return statements
}

// architectureNotes come from the CONTRACT's invariants and the ProjectModel's
// critical boundaries. Neither is discovered: both were established before this
// compiler ran, which is what keeps repository intelligence in #67.
func architectureNotes(contract domain.EngineeringWorkContract, model domain.ProjectModel) []string {
	notes := requirementStatements(contract.Invariants)
	if model.CriticalBoundaries != nil {
		for name, boundary := range *model.CriticalBoundaries {
			notes = append(notes, fmt.Sprintf("Critical boundary %s (%s) covers %s.",
				name, boundary.Type, strings.Join(sortedUniqueStrings(boundary.Paths), ", ")))
		}
	}
	return sortedUniqueStrings(notes)
}

// policyExcerpts are what the change is being JUDGED against: the claims policy
// requires, the plan-shaped obligations it compiled, and the profiles the
// project declares. A reviewer needs the standard, not the whole policy.
func policyExcerpts(contract domain.EngineeringWorkContract, model domain.ProjectModel) []string {
	var excerpts []string
	for id, claim := range contract.RequiredClaims {
		excerpt := fmt.Sprintf("Claim %s requires evidence of class %s.", id, claim.EvidenceClass)
		if claim.IndependentFromChangeProducer {
			excerpt = fmt.Sprintf("Claim %s requires evidence of class %s, independent of the change producer.", id, claim.EvidenceClass)
		}
		excerpts = append(excerpts, excerpt)
	}
	if contract.PlanRequirements != nil {
		for _, role := range contract.PlanRequirements.Roles {
			excerpts = append(excerpts, role.Statement)
		}
		for _, gate := range contract.PlanRequirements.Gates {
			excerpts = append(excerpts, gate.Statement)
		}
	}
	if model.PolicyProfiles != nil {
		for _, profile := range *model.PolicyProfiles {
			excerpts = append(excerpts, fmt.Sprintf("The project declares policy profile %s.", profile))
		}
	}
	return sortedUniqueStrings(excerpts)
}

// factStatements render the established facts as the uncertainty-preserving
// sentences they are. A fact's stage and confidence travel with it: a predicted
// low-confidence fact must not read like an observed one.
func factStatements(facts []domain.EngineeringFact) []string {
	statements := make([]string, 0, len(facts))
	for _, fact := range facts {
		statements = append(statements, fmt.Sprintf("%s = %v (%s, %s confidence, from %s %s)",
			fact.Key, fact.Value, fact.Stage, fact.Confidence, fact.Provenance.Type, fact.Provenance.Producer))
	}
	return sortedUniqueStrings(statements)
}

func requirementStatements(requirements map[string]domain.Requirement) []string {
	statements := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		statements = append(statements, requirement.Statement)
	}
	return sortedUniqueStrings(statements)
}

func actionStatements(actions []domain.Action) []string {
	statements := make([]string, 0, len(actions))
	for _, action := range actions {
		statements = append(statements, action.Type+":"+action.Target)
	}
	return sortedUniqueStrings(statements)
}

// sortedUpstream orders upstream outputs by the stage that produced them, so a
// pack is stable whatever order the reconciler happened to collect them in.
func sortedUpstream(outputs []domain.UpstreamOutput) []domain.UpstreamOutput {
	if len(outputs) == 0 {
		return nil
	}
	sorted := append([]domain.UpstreamOutput(nil), outputs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StageID < sorted[j].StageID })
	return sorted
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sorted := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			sorted = append(sorted, value)
		}
	}
	if len(sorted) == 0 {
		return nil
	}
	sort.Strings(sorted)
	unique := sorted[:1]
	for _, value := range sorted[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}
