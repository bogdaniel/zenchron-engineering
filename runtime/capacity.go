package runtime

import (
	"slices"
	"sort"
)

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
	OpHandoffRepair:     CapacityWork,
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

// verificationKinds are the WORK operations that also consume host
// verification capacity (#490): each runs an expensive local verifier on the
// host - assurance.go runs gofmt, go vet and go test against the exact tree.
// A verification operation is still work, so it is bounded by
// max_concurrent_runs AND by max_concurrent_verifications; the second ceiling
// can only narrow how many runs drive work at once, never widen it.
//
// Provider reasoning (execution.invoke), semantic assurance (a provider call)
// and the cheap gofmt remediation are not listed: what they consume is the
// provider or nothing expensive locally. A kind not listed here is never
// gated by verification capacity, which is the direction that cannot starve
// work; the capacity test states every listed kind.
var verificationKinds = []string{OpAssuranceGo}

// consumesVerification reports whether kind needs a verification slot.
func consumesVerification(kind string) bool { return slices.Contains(verificationKinds, kind) }

// holdsVerificationSlot is the durable occupancy the store counts: a leased or
// running verification operation that still carries its lease.
func holdsVerificationSlot(op RunOperation) bool {
	return op.Lease != nil && (op.State == Leased || op.State == Running) && consumesVerification(op.Kind)
}

// ReasonVerificationCapacity is the wait of a run whose next operation is a
// verification that was refused because every verification slot is held by
// another run (#490). It is not a verdict, not a failure and not a reason to
// remediate: nothing ran, and no attempt or retry budget was spent.
const ReasonVerificationCapacity = "verification_capacity_unavailable"

// DefaultMaxConcurrentVerifications is the verification ceiling when no layer
// states one. Two is the low end of the band #490's dogfood ruling named: one
// go test of this size already spreads across every core, ten at once
// overloaded the host into ten-minute test timeouts, and one would serialize
// every verification of a fleet behind the slowest.
const DefaultMaxConcurrentVerifications = 2

// resolveMaxConcurrentVerifications applies the default and the floor of one:
// zero or less means unstated.
func resolveMaxConcurrentVerifications(ceiling int) int {
	if ceiling <= 0 {
		return DefaultMaxConcurrentVerifications
	}
	return ceiling
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
