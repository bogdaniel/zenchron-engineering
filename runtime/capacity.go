package runtime

import "sort"

// CapacityClass is WHICH BOUNDED RESOURCE an operation consumes (#85). It is
// not a statement about cost: an observation is in its own class because it
// performs no work on the candidate, not because it is cheap.
type CapacityClass string

const (
	// CapacityWork is bounded by max_concurrent_runs.
	CapacityWork CapacityClass = "work"
	// CapacityObservation is bounded by max_concurrent_observations. It is
	// also the only class a waiting run may perform.
	CapacityObservation CapacityClass = "observation"
)

// operationCapacityClasses is the ONE vocabulary. Every production operation
// kind is listed so that its class is a decision rather than a default; the
// capacity test fails for a kind that is missing. Only source.observe and
// github.observe are observation: each reads the forge and journals what it
// saw, and neither invokes a provider or verifier nor mutates candidate,
// remote or forge state.
var operationCapacityClasses = map[string]CapacityClass{
	OpSourceObserve:     CapacityObservation,
	OpGitHubObserve:     CapacityObservation,
	OpContractCompile:   CapacityWork,
	OpCandidateCreate:   CapacityWork,
	OpExecutionInvoke:   CapacityWork,
	OpRemediationGofmt:  CapacityWork,
	OpCandidateCommit:   CapacityWork,
	OpAssuranceGo:       CapacityWork,
	OpAssuranceSemantic: CapacityWork,
	OpAuthorityEvaluate: CapacityWork,
	OpBaseIntegrate:     CapacityWork,
	OpCandidatePush:     CapacityWork,
	OpPullRequestCreate: CapacityWork,
	OpPullRequestUpdate: CapacityWork,
}

// OperationCapacityClass classifies one operation kind. It FAILS CLOSED: any
// kind not in the vocabulary, including one added later, is work.
func OperationCapacityClass(kind string) CapacityClass {
	if class, ok := operationCapacityClasses[kind]; ok {
		return class
	}
	return CapacityWork
}

// observationKindList lists the observation-class kinds in a stable order, for
// the store's SQL. It is derived from the vocabulary, never restated.
func observationKindList() []string {
	var kinds []string
	for kind, class := range operationCapacityClasses {
		if class == CapacityObservation {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

// DefaultMaxConcurrentObservations is the observation ceiling when no layer
// states one.
const DefaultMaxConcurrentObservations = 2

// resolveMaxConcurrentObservations applies the default and the floor of one:
// zero or less means unstated.
func resolveMaxConcurrentObservations(ceiling int) int {
	if ceiling <= 0 {
		return DefaultMaxConcurrentObservations
	}
	return ceiling
}
