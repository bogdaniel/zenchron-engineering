package runtime

import (
	"encoding/json"
	"math"
	"testing"
)

func TestCanonicalJSONRFC8785ValuesVector(t *testing.T) {
	// RFC 8785 section 3.2.2's published primitive-serialization example.
	input := json.RawMessage(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/","literals":[null,true,false]}`)
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`
	got, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical JSON = %q, want %q", got, want)
	}
}

func TestCanonicalJSONOrdersObjectsButPreservesArrays(t *testing.T) {
	got, err := CanonicalJSON(json.RawMessage(`{"z":{"b":2,"a":1},"array":[{"d":4,"c":3},2,1],"a":0}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":0,"array":[{"c":3,"d":4},2,1],"z":{"a":1,"b":2}}`
	if string(got) != want {
		t.Fatalf("canonical JSON = %q, want %q", got, want)
	}
}

func TestCanonicalJSONUsesUTF16PropertyOrdering(t *testing.T) {
	// RFC 8785 section 3.2.3's published property-ordering test data.
	input := json.RawMessage(`{"€":"Euro Sign","\r":"Carriage Return","דּ":"Hebrew Letter Dalet With Dagesh","1":"One","😀":"Emoji: Grinning Face","":"Control","ö":"Latin Small Letter O With Diaeresis"}`)
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"דּ\":\"Hebrew Letter Dalet With Dagesh\"}"
	got, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical JSON = %q, want %q", got, want)
	}
}

func TestCanonicalJSONFixedPointAndSemanticEquivalence(t *testing.T) {
	a, err := CanonicalJSON(json.RawMessage(`{"b":1.0,"a":{"y":"<>&","x":2e-3}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalJSON(json.RawMessage(`{"a":{"x":0.002,"y":"\u003c\u003e\u0026"},"b":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != `{"a":{"x":0.002,"y":"<>&"},"b":1}` {
		t.Fatalf("JCS string escaping = %q", a)
	}
	if string(a) != string(b) {
		t.Fatalf("equivalent JSON differs: %q != %q", a, b)
	}
	fixed, err := CanonicalJSON(json.RawMessage(a))
	if err != nil {
		t.Fatal(err)
	}
	if string(fixed) != string(a) {
		t.Fatalf("canonicalization is not a fixed point: %q != %q", fixed, a)
	}
	da, err := Digest(json.RawMessage(`{"b":1,"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(json.RawMessage(`{"a":2,"b":1.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("equivalent digest differs: %s != %s", da, db)
	}
}

func TestDigestFieldExclusions(t *testing.T) {
	state := RunSnapshot{EngineeringRun: EngineeringRun{ID: "run", Cursor: Cursor{LastSequence: 2, LastEventID: "event", LastEventHash: "chain"}}, Operations: map[string]RunOperation{"op": {ID: "op"}}, StateSHA256: "old"}
	baseline, err := StateDigest(state)
	if err != nil {
		t.Fatal(err)
	}
	state.StateSHA256 = "different"
	state.Cursor = Cursor{LastSequence: 99, LastEventID: "other", LastEventHash: "other-chain"}
	got, err := StateDigest(state)
	if err != nil {
		t.Fatal(err)
	}
	if got != baseline {
		t.Fatalf("state digest included excluded fields: %s != %s", got, baseline)
	}

	event := EngineeringEvent{ID: "event", RunID: "run", Sequence: 1, Type: EventRunCreated, StateBefore: "before", StateAfter: "after", PreviousEventHash: "previous"}
	eventDigest, err := EventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	event.EventHash = "different"
	withSelfField, err := EventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	if withSelfField != eventDigest {
		t.Fatal("event hash included its own event_hash field")
	}
	event.StateAfter = "changed"
	changedBinding, err := EventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	if changedBinding == eventDigest {
		t.Fatal("event hash excluded required binding information")
	}
}

func TestCanonicalJSONRejectsNonIJSONInputs(t *testing.T) {
	cases := []any{
		map[string]string{"bad": string([]byte{0xff})},
		map[string]float64{"bad": math.Inf(1)},
		uint64(maxSafeInteger) + 1,
		json.RawMessage(`{"duplicate":1,"duplicate":2}`),
		json.RawMessage(`1e400`),
		json.RawMessage([]byte{'"', 0xff, '"'}),
	}
	for _, input := range cases {
		if _, err := CanonicalJSON(input); err == nil {
			t.Fatalf("CanonicalJSON(%T) unexpectedly succeeded", input)
		}
	}
}
