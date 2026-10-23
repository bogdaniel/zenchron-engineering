package runtime

import (
	"errors"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// fileSizeCheckProves reports whether the assurance observation PROVES the
// file-size claim for this exact candidate and governing base: it carries a
// completed, successful file-size record by the current checker, bound to the
// same commit and tree as the observation and to the same base the contract is
// governed by. Anything less is not proof, and in particular an observation
// with no record at all - every one written before the record existed - proves
// nothing about the file-size policy, however green its suite was.
func fileSizeCheckProves(observation *AssuranceObservation, commit, base string) bool {
	if observation == nil || base == "" || commit == "" {
		return false
	}
	for _, check := range observation.Checks {
		if check.Kind == CheckKindFileSizePolicy &&
			check.Outcome == CheckOutcomePassed &&
			check.Checker == fileSizeCheckerID && check.CheckerRevision == fileSizeCheckerRevision() &&
			check.Commit == commit && check.Commit == observation.Commit &&
			check.Tree != "" && check.Tree == observation.Tree &&
			check.BaseRevision == base {
			return true
		}
	}
	return false
}

// fileSizeEvidenceItem turns the generic passing-assurance item for the
// file-size claim into the evidence that actually proves it, or reports that no
// evidence can be produced. The producer and environment name the host-side
// checker that ran the check, not the sandbox verifier that ran the suite.
func fileSizeEvidenceItem(item domain.EvidenceItem, observation *AssuranceObservation, commit, base string) (domain.EvidenceItem, bool) {
	if !fileSizeCheckProves(observation, commit, base) {
		return domain.EvidenceItem{}, false
	}
	item.Producer = domain.EvidenceProducer{ID: fileSizeCheckerID, Type: domain.ProducerDeterministicTool}
	item.Environment = domain.EvidenceEnvironment{Type: "controller_host", Identifier: fileSizeHostEnvironmentID}
	return item, true
}

// policyListsFileSizeClaim is a cheap test of the policy the controller loaded:
// can any contract compiled under it require the claim?
func policyListsFileSizeClaim(policy domain.EngineeringPolicy) bool {
	for _, rule := range policy.Rules {
		if rule.Effect.RequiredClaims == nil {
			continue
		}
		if _, listed := (*rule.Effect.RequiredClaims)[FileSizePolicyClaimID]; listed {
			return true
		}
	}
	return false
}

// fileSizeBindingSuffix extends the assurance.go operation binding when, and
// only when, the contract requires the file-size claim. It is what makes a
// newly required check PLANNED: the binding for the same commit, tree and
// contract revision changes, so an assurance that ran before the requirement
// existed no longer satisfies it and the existing scheduler asks again, inside
// the same attempt and wall budgets. A run whose contract does not require the
// claim keeps exactly the binding it always had.
//
// The contract is rebuilt only when the loaded policy can require the claim at
// all, so a policy that does not list it costs nothing.
func (s *runState) fileSizeBindingSuffix() string {
	if s.rt == nil {
		return ""
	}
	return fileSizeBindingSuffixFor(s.rt.deps.Policy, s.pinnedBase(), func() (domain.EngineeringWorkContract, error) {
		return s.rt.contractFor(s)
	})
}

// fileSizeBindingSuffixFor is the pure decision. A contract that cannot be
// rebuilt leaves the binding as it was: it cannot be shown to require the
// claim, and discharging the claim is refused independently of planning.
func fileSizeBindingSuffixFor(policy domain.EngineeringPolicy, base string, rebuild func() (domain.EngineeringWorkContract, error)) string {
	if !policyListsFileSizeClaim(policy) {
		return ""
	}
	contract, err := rebuild()
	if err != nil || !fileSizePolicyRequired(contract) {
		return ""
	}
	return "|file-size-policy@" + base
}

// fileSizeBaseRefusal refuses verification of a contract that requires the
// file-size policy when the run has no base revision to read its rules from. A
// required check that cannot be run is not a check that may be skipped.
func fileSizeBaseRefusal(contract domain.EngineeringWorkContract, base string) error {
	if fileSizePolicyRequired(contract) && base == "" {
		return errors.New("the contract requires the file-size policy but the run has no base revision to read its rules from; refusing to verify without it")
	}
	return nil
}
