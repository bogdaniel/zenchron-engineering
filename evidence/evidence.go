// Package evidence manages the applicability of independently produced
// evidence. It deliberately does not evaluate authority or assign trust to a
// producer type; those are later policy and authority concerns.
package evidence

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Binding is the exact governance context to which an EvidenceBundle applies.
// A bundle cannot be silently rebound to a different Binding.
type Binding struct {
	Subject  domain.Subject
	Contract domain.ObjectRevision
	Policy   domain.ObjectRevision
}

// BindingOf returns the exact binding recorded in bundle.
func BindingOf(bundle domain.EvidenceBundle) Binding {
	return Binding{Subject: bundle.Subject, Contract: bundle.Contract, Policy: bundle.Policy}
}

// Matches reports whether bundle has the exact target subject, contract, and
// policy binding. A repository revision, contract revision, or policy revision
// change makes evidence inapplicable by default.
func Matches(bundle domain.EvidenceBundle, target Binding) bool {
	return BindingOf(bundle) == target
}

// HasApplicablePassingEvidence reports whether bundle contains passing valid
// evidence for claimID with the expected evidence class and exact binding.
// Stale, invalid, incomplete, failing, and inconclusive evidence is not
// applicable. A true result establishes only that applicable passing evidence
// exists; it does not establish change-producer independence, final claim
// satisfaction for authority, permission, or action authority.
func HasApplicablePassingEvidence(bundle domain.EvidenceBundle, target Binding, claimID string, expectedEvidenceClass domain.EvidenceClass) (bool, error) {
	if err := validateBundleAndBinding(bundle, target); err != nil {
		return false, err
	}
	if expectedEvidenceClass == "" {
		return false, fmt.Errorf("expected evidence class must not be empty")
	}
	if !Matches(bundle, target) {
		return false, nil
	}
	for _, item := range bundle.Evidence {
		if item.ClaimID == claimID && item.EvidenceClass == expectedEvidenceClass && item.Result.Status == domain.EvidencePassed && item.Lifecycle.Status == domain.EvidenceValid {
			return true, nil
		}
	}
	return false, nil
}

// MarkStaleForBindingChange creates the next revision of bundle after a
// material binding change. The returned bundle retains its original binding:
// rebinding old evidence to the new subject, contract, or policy would make it
// appear applicable when it is not. Valid evidence becomes stale; evidence
// already stale, invalid, or incomplete keeps its more specific lifecycle.
//
// Explicit dependency analysis that proves evidence can carry forward is
// intentionally not modeled in v0.1. Callers must therefore obtain fresh
// evidence for the target Binding.
func MarkStaleForBindingChange(bundle domain.EvidenceBundle, target Binding, nextRevision string) (domain.EvidenceBundle, error) {
	if err := validateBundleAndBinding(bundle, target); err != nil {
		return domain.EvidenceBundle{}, err
	}
	if Matches(bundle, target) {
		return domain.EvidenceBundle{}, fmt.Errorf("target binding is unchanged")
	}
	if nextRevision == "" || nextRevision == bundle.Revision {
		return domain.EvidenceBundle{}, fmt.Errorf("next evidence bundle revision must differ from the current revision")
	}

	updated := bundle
	updated.Revision = nextRevision
	updated.Evidence = make(map[string]domain.EvidenceItem, len(bundle.Evidence))
	reason := bindingChangeReason(BindingOf(bundle), target)
	for id, item := range bundle.Evidence {
		if item.Lifecycle.Status == domain.EvidenceValid {
			item.Lifecycle = domain.EvidenceLifecycle{Status: domain.EvidenceStale, Reason: &reason}
		}
		updated.Evidence[id] = item
	}
	if _, err := domain.Encode(updated); err != nil {
		return domain.EvidenceBundle{}, fmt.Errorf("stale evidence bundle is invalid: %w", err)
	}
	return updated, nil
}

func validateBundleAndBinding(bundle domain.EvidenceBundle, target Binding) error {
	if _, err := domain.Encode(bundle); err != nil {
		return fmt.Errorf("invalid evidence bundle: %w", err)
	}
	if target.Subject.Repository == "" || target.Subject.Revision == "" ||
		target.Contract.ID == "" || target.Contract.Revision == "" ||
		target.Policy.ID == "" || target.Policy.Revision == "" {
		return fmt.Errorf("target binding requires subject, contract, and policy identities and revisions")
	}
	return nil
}

func bindingChangeReason(before, after Binding) string {
	return fmt.Sprintf("Evidence binding changed from subject %q@%q, contract %q@%q, policy %q@%q to subject %q@%q, contract %q@%q, policy %q@%q.",
		before.Subject.Repository, before.Subject.Revision, before.Contract.ID, before.Contract.Revision, before.Policy.ID, before.Policy.Revision,
		after.Subject.Repository, after.Subject.Revision, after.Contract.ID, after.Contract.Revision, after.Policy.ID, after.Policy.Revision)
}
