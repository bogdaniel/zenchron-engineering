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
// WrittenAtAll proves the durable store itself: a claimed-then-finalized
// receipt is read back exactly, and an operation id that was never claimed
// reports nil, never an error treated as success.
func TestArtifactActionReceiptStoreRoundTripsAndReportsUnknownForWhatWasNeverWrittenAtAll(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	r := validPlanningArtifactReceipt()
	claimed, err := store.ClaimArtifactActionOperation(r.OperationID, time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("the first claim on an unused operation id must succeed")
	}
	if err := store.FinalizeArtifactActionReceipt(r, time.Unix(100, 0).UTC()); err != nil {
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

	// The primary key refuses a second claim on the same operation outright.
	if reclaimed, err := store.ClaimArtifactActionOperation(r.OperationID, time.Unix(200, 0).UTC()); err != nil || reclaimed {
		t.Fatalf("a second claim for the same operation id must be refused: claimed=%v, err=%v", reclaimed, err)
	}
	// Finalizing an operation id with no outstanding claim is refused, not
	// a silent overwrite of an already-finalized receipt.
	if err := store.FinalizeArtifactActionReceipt(r, time.Unix(300, 0).UTC()); err == nil {
		t.Fatal("finalizing without a reserved, unfinalized claim must be refused")
	}
}

// TestArtifactActionReceiptClaimReportsUnknownUntilFinalizedAndReleaseFreesIt
// is the crash/concurrency contract itself: a claimed-but-not-yet-finalized
// operation reads as genuinely unknown (never a false negative claiming it
// never happened), a second claim attempt on it is refused (closing the
// race two concurrent requests sharing the same reference would otherwise
// hit), and releasing an unfinalized claim - the REFUSED-effect path - frees
// the operation id for a legitimate retry.
func TestArtifactActionReceiptClaimReportsUnknownUntilFinalizedAndReleaseFreesIt(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const opID = artifactReceiptTestID
	claimed, err := store.ClaimArtifactActionOperation(opID, time.Unix(100, 0).UTC())
	if err != nil || !claimed {
		t.Fatalf("claimed=%v, err=%v", claimed, err)
	}
	if read, err := store.ArtifactActionReceipt(opID); err != nil || read != nil {
		t.Fatalf("a claimed-but-unfinalized operation must read as unknown, not as a receipt: %+v, %v", read, err)
	}
	if reclaimed, err := store.ClaimArtifactActionOperation(opID, time.Unix(150, 0).UTC()); err != nil || reclaimed {
		t.Fatalf("a concurrent claim on the same in-flight operation must be refused: claimed=%v, err=%v", reclaimed, err)
	}

	if err := store.ReleaseArtifactActionClaim(opID); err != nil {
		t.Fatal(err)
	}
	if read, err := store.ArtifactActionReceipt(opID); err != nil || read != nil {
		t.Fatalf("a released claim must leave no trace: %+v, %v", read, err)
	}
	if claimed, err := store.ClaimArtifactActionOperation(opID, time.Unix(200, 0).UTC()); err != nil || !claimed {
		t.Fatalf("a retry after release must be able to claim the same operation id again: claimed=%v, err=%v", claimed, err)
	}
}
