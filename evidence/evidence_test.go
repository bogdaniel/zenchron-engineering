package evidence_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/evidence"
)

func TestEvidenceForRevisionAIsNotApplicableToRevisionB(t *testing.T) {
	bundle := fixture(t, "stale-evidence.evidence-bundle.json")
	target := evidence.Binding{
		Subject:  domain.Subject{Repository: "acme/payments", Revision: "rev-b"},
		Contract: domain.ObjectRevision{ID: "contract-auth-session", Revision: "1"},
		Policy:   domain.ObjectRevision{ID: "policy-engineering-baseline", Revision: "1"},
	}

	hasApplicablePassingEvidence, err := evidence.HasApplicablePassingEvidence(bundle, target, "claim-auth-regression-tests", "test_result")
	if err != nil {
		t.Fatal(err)
	}
	if hasApplicablePassingEvidence {
		t.Fatal("revision A evidence was applicable to revision B")
	}

	stale, err := evidence.MarkStaleForBindingChange(bundle, target, "3")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Subject.Revision != "rev-a" {
		t.Fatalf("stale bundle was rebound to %q", stale.Subject.Revision)
	}
	item := stale.Evidence["evidence-auth-tests-rev-a"]
	if item.Lifecycle.Status != domain.EvidenceStale || item.Lifecycle.Reason == nil || !strings.Contains(*item.Lifecycle.Reason, "rev-b") {
		t.Fatalf("lifecycle = %#v, want stale evidence identifying revision B", item.Lifecycle)
	}
}

func TestContractRevisionChangeMakesEvidenceInapplicable(t *testing.T) {
	bundle := fixture(t, "security-sensitive.evidence-bundle.json")
	target := evidence.BindingOf(bundle)
	target.Contract.Revision = "2"

	stale, err := evidence.MarkStaleForBindingChange(bundle, target, "2")
	if err != nil {
		t.Fatal(err)
	}
	for id, item := range stale.Evidence {
		if item.Lifecycle.Status != domain.EvidenceStale {
			t.Errorf("%s lifecycle = %q, want stale", id, item.Lifecycle.Status)
		}
	}
	hasApplicablePassingEvidence, err := evidence.HasApplicablePassingEvidence(stale, target, "claim-auth-regression-tests", "test_result")
	if err != nil {
		t.Fatal(err)
	}
	if hasApplicablePassingEvidence {
		t.Fatal("evidence from contract revision 1 was applicable to revision 2")
	}
}

func TestNonValidEvidenceIsNotApplicable(t *testing.T) {
	bundle := fixture(t, "trivial.evidence-bundle.json")
	target := evidence.BindingOf(bundle)
	item := bundle.Evidence["evidence-json-parse"]

	for _, status := range []domain.EvidenceLifecycleStatus{domain.EvidenceStale, domain.EvidenceInvalid, domain.EvidenceIncomplete} {
		reason := "test lifecycle"
		item.Lifecycle = domain.EvidenceLifecycle{Status: status, Reason: &reason}
		bundle.Evidence["evidence-json-parse"] = item
		hasApplicablePassingEvidence, err := evidence.HasApplicablePassingEvidence(bundle, target, item.ClaimID, item.EvidenceClass)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if hasApplicablePassingEvidence {
			t.Errorf("%s evidence was applicable", status)
		}
	}
}

func TestWrongEvidenceClassIsNotApplicable(t *testing.T) {
	bundle := fixture(t, "trivial.evidence-bundle.json")
	target := evidence.BindingOf(bundle)
	item := bundle.Evidence["evidence-json-parse"]
	item.ClaimID = "claim-security-owner-approval"
	bundle.Evidence["evidence-json-parse"] = item

	hasApplicablePassingEvidence, err := evidence.HasApplicablePassingEvidence(bundle, target, item.ClaimID, "human_approval")
	if err != nil {
		t.Fatal(err)
	}
	if hasApplicablePassingEvidence {
		t.Fatal("test-result evidence was applicable as human approval")
	}
}

func TestHasApplicablePassingEvidenceRejectsMalformedEvidenceMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.EvidenceItem)
	}{
		{
			name: "empty producer ID",
			mutate: func(item *domain.EvidenceItem) {
				item.Producer.ID = ""
			},
		},
		{
			name: "unsupported producer type",
			mutate: func(item *domain.EvidenceItem) {
				item.Producer.Type = domain.ProducerType("unrecognized")
			},
		},
		{
			name: "empty environment identifier",
			mutate: func(item *domain.EvidenceItem) {
				item.Environment.Identifier = ""
			},
		},
		{
			name: "invalid provenance recorded at",
			mutate: func(item *domain.EvidenceItem) {
				item.Provenance.RecordedAt = "not-a-timestamp"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := fixture(t, "trivial.evidence-bundle.json")
			target := evidence.BindingOf(bundle)
			item := bundle.Evidence["evidence-json-parse"]
			test.mutate(&item)
			bundle.Evidence["evidence-json-parse"] = item

			if _, err := evidence.HasApplicablePassingEvidence(bundle, target, item.ClaimID, item.EvidenceClass); err == nil {
				t.Fatal("malformed evidence metadata was accepted")
			}
		})
	}
}

func TestMarkStaleRequiresNewBindingAndRevision(t *testing.T) {
	bundle := fixture(t, "trivial.evidence-bundle.json")
	target := evidence.BindingOf(bundle)
	if _, err := evidence.MarkStaleForBindingChange(bundle, target, "2"); err == nil {
		t.Fatal("unchanged binding was accepted")
	}
	target.Subject.Revision = "rev-b"
	if _, err := evidence.MarkStaleForBindingChange(bundle, target, bundle.Revision); err == nil {
		t.Fatal("unchanged evidence bundle revision was accepted")
	}
}

func fixture(t *testing.T, name string) domain.EvidenceBundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := domain.Decode[domain.EvidenceBundle](data)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
