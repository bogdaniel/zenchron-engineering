package runtime

import "testing"

// The permit's journal event id is <permit>/<state>, so a second actor making
// the same transition from the same revision collides on the event insert
// before its compare-and-swap can miss. That loss must read as "not acquired",
// like any other lost race, and not as a persistence failure (#690).
func TestALostPermitTransitionRaceIsNotAcquiredNotAnError(t *testing.T) {
	_, store, s := toolFixture(t, 1)
	parent := nestedParent(t, store, s, "producer")
	ref := ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}
	request, err := s.RequestVerification(ref, "tool", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(request); err != nil || !ok {
		t.Fatalf("grant: %v/%v", ok, err)
	}
	granted, revision, found, err := store.VerificationPermit(request.ID)
	if err != nil || !found {
		t.Fatalf("read granted permit: %v/%v", found, err)
	}
	now := s.Clock.Now()
	released := granted
	released.State, released.ReleasedAt = VerificationReleased, &now
	if ok, err := store.TransitionVerificationPermit(released, revision, 1); err != nil || !ok {
		t.Fatalf("first release: %v/%v", ok, err)
	}
	ok, err := store.TransitionVerificationPermit(released, revision, 1)
	if err != nil || ok {
		t.Fatalf("second release from the same revision = %v/%v, want not acquired without error", ok, err)
	}
}
