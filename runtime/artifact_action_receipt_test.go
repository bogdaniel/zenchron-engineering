package runtime

import (
	"testing"
	"time"
)

const artifactReceiptTestID = "01234567-89ab-4cde-8fab-0123456789ab"
const artifactReceiptTestDigest = "1111111111111111111111111111111111111111111111111111111111111111"
const artifactReceiptTestArtifactDigest = "2222222222222222222222222222222222222222222222222222222222222222"

func validPlanningArtifactReceipt() ArtifactActionReceipt {
	return ArtifactActionReceipt{
		OperationID: artifactReceiptTestID, RequestDigest: artifactReceiptTestDigest,
		Action: ControlPlanningArtifactPropose, ControllerBinding: "binding",
		ArtifactKind: "agent_profile", ArtifactID: "reviewer", ArtifactDigest: artifactReceiptTestArtifactDigest,
	}
}

func validProductReceipt() ArtifactActionReceipt {
	return ArtifactActionReceipt{
		OperationID: artifactReceiptTestID, RequestDigest: artifactReceiptTestDigest,
		Action: ControlProductPropose, ControllerBinding: "binding",
		ProductID: "widget", ProductRevision: 1,
	}
}

// TestArtifactActionReceiptValidateRefusesACrossedSubject proves the two
// action kinds cannot carry each other's subject fields - a planning-
// artifact receipt naming a product, or vice versa, is a document this
// receipt type can never represent, not merely an unusual one.
func TestArtifactActionReceiptValidateRefusesACrossedSubject(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ArtifactActionReceipt)
	}{
		{"missing artifact kind", func(r *ArtifactActionReceipt) { r.ArtifactKind = "" }},
		{"missing artifact id", func(r *ArtifactActionReceipt) { r.ArtifactID = "" }},
		{"malformed artifact digest", func(r *ArtifactActionReceipt) { r.ArtifactDigest = "not-a-digest" }},
		{"carries a product id too", func(r *ArtifactActionReceipt) { r.ProductID = "widget" }},
		{"carries a product revision too", func(r *ArtifactActionReceipt) { r.ProductRevision = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validPlanningArtifactReceipt()
			tc.edit(&r)
			if err := r.validate(); err == nil {
				t.Fatalf("%s: must be refused", tc.name)
			}
		})
	}

	productCases := []struct {
		name string
		edit func(*ArtifactActionReceipt)
	}{
		{"missing product id", func(r *ArtifactActionReceipt) { r.ProductID = "" }},
		{"missing product revision", func(r *ArtifactActionReceipt) { r.ProductRevision = 0 }},
		{"carries an artifact kind too", func(r *ArtifactActionReceipt) { r.ArtifactKind = "agent_profile" }},
	}
	for _, tc := range productCases {
		t.Run(tc.name, func(t *testing.T) {
			r := validProductReceipt()
			tc.edit(&r)
			if err := r.validate(); err == nil {
				t.Fatalf("%s: must be refused", tc.name)
			}
		})
	}

	if err := validPlanningArtifactReceipt().validate(); err != nil {
		t.Fatalf("a well-formed planning-artifact receipt must validate: %v", err)
	}
	if err := validProductReceipt().validate(); err != nil {
		t.Fatalf("a well-formed product receipt must validate: %v", err)
	}
}

// TestArtifactActionReceiptStoreRoundTripsAndReportsUnknownForWhatWasNever
// WrittenAtAll proves the durable store itself: a receipt written is read
// back exactly, and an operation id that was never written reports nil,
// never an error treated as success.
func TestArtifactActionReceiptStoreRoundTripsAndReportsUnknownForWhatWasNeverWrittenAtAll(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	r := validPlanningArtifactReceipt()
	if err := store.PutArtifactActionReceipt(r, time.Unix(100, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	read, err := store.ArtifactActionReceipt(r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if read == nil || *read != r {
		t.Fatalf("read back %+v, wrote %+v", read, r)
	}

	never, err := store.ArtifactActionReceipt("00000000-0000-4000-8000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if never != nil {
		t.Fatalf("an operation never written must report no receipt, got %+v", never)
	}

	// The primary key refuses a second row for the same operation outright.
	if err := store.PutArtifactActionReceipt(r, time.Unix(200, 0).UTC()); err == nil {
		t.Fatal("a second receipt for the same operation id must be refused")
	}
}
