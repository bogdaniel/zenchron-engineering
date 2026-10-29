package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// #327 extends PlanReasoningProvenance with an optional invocation member. A
// plan revision persisted before it existed must still decode, re-encode to the
// same canonical bytes (so its content digest is unchanged), and read back as
// NO observation rather than a zero-valued one; a revision persisted after it
// must round-trip every observed field.
func TestPlanRevisionsBeforeAndAfterInvocationProvenanceBothRoundTrip(t *testing.T) {
	current, err := os.ReadFile("../fixtures/v0.1/valid/enterprise-sso.engineering-plan.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(current, &document); err != nil {
		t.Fatal(err)
	}
	delete(document["provenance"].(map[string]any)["reasoning"].(map[string]any), "invocation")
	legacy, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}

	for name, persisted := range map[string][]byte{"before #327": legacy, "after #327": current} {
		t.Run(name, func(t *testing.T) {
			plan, err := Decode[EngineeringPlan](persisted)
			if err != nil {
				t.Fatalf("a persisted revision no longer decodes: %v", err)
			}
			encoded, err := Encode(plan)
			if err != nil {
				t.Fatal(err)
			}
			want, err := CanonicalJSON(json.RawMessage(persisted))
			if err != nil {
				t.Fatal(err)
			}
			got, err := CanonicalJSON(json.RawMessage(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the revision does not round-trip, so its digest would move:\n got %s\nwant %s", got, want)
			}
			invocation := plan.Provenance.Reasoning.Invocation
			if name == "before #327" {
				if invocation != nil {
					t.Fatalf("a revision without the member read back as an observation: %+v", invocation)
				}
				return
			}
			if invocation == nil || invocation.TerminationCause != "provider_returned" ||
				invocation.ProgressMode != "structured_claude_events" || invocation.InactivityLimit != 10*time.Minute ||
				invocation.StructuredEvents != 41 || len(invocation.PermissionDeniedTools) != 1 {
				t.Fatalf("the observation did not survive: %+v", invocation)
			}
		})
	}
}
