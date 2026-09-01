package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReduceDeterministicAndMergedPrecedence(t *testing.T) {
	r := EngineeringRun{SchemaVersion: SchemaVersion, ID: "r", Disposition: Active}
	p, _ := json.Marshal(map[string]string{"reason": "merged"})
	e := EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e", RunID: "r", Sequence: 1, Type: EventRunCompleted, OccurredAt: time.Unix(1, 0), Payload: p}
	h, _ := EventDigest(e)
	e.EventHash = h
	a, err := Reduce(r, []EngineeringEvent{e})
	if err != nil || a.Disposition != Completed {
		t.Fatal(err, a)
	}
	b, _ := Reduce(r, []EngineeringEvent{e})
	if a.StateSHA256 != b.StateSHA256 {
		t.Fatal("nondeterministic")
	}
	d, why := MergePrecedence(true, true)
	if d != Completed || why != "merged" {
		t.Fatal(d, why)
	}
}
func TestRawArtifactCannotPublish(t *testing.T) {
	if ValidateArtifact(Artifact{Path: "raw", Publishable: true}) == nil {
		t.Fatal("raw publish")
	}
	if !VerificationSurfaceChanged([]string{"go.mod"}) {
		t.Fatal("module surface")
	}
}
