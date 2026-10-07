package benchmark

import (
	"encoding/json"
	"testing"
)

func TestObservationRoundTrip(t *testing.T) {
	data, err := json.Marshal(dataset())
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	if err = json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	if _, err = Evaluate(in); err != nil {
		t.Fatal(err)
	}
	var task Task
	if err = json.Unmarshal([]byte(`{"id":"missing outcomes"}`), &task); err == nil {
		t.Fatal("missing measurements accepted")
	}
}
