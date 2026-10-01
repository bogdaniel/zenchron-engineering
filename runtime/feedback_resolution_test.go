package runtime

// What a feedback resolution may NOT do.
//
// AdmitFeedbackResolution is the one way a producer invocation may discharge
// admitted feedback without mutating the candidate (#376). Every refusal case
// here is a way a claim could discharge feedback it was never bound to - the
// wrong revision, the wrong set of keys, an unrecognized shape - and every one
// of them must fail closed: the keys it would have discharged stay
// outstanding, exactly as if no resolution had been written at all.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const resolutionSubject = "1234567890123456789012345678901234567890"

func validResolution(keys ...string) *FeedbackResolution {
	return &FeedbackResolution{
		SchemaVersion: FeedbackResolutionSchemaVersion,
		Resolution:    FeedbackResolutionNoChangeRequired,
		Subject:       resolutionSubject,
		Keys:          keys,
	}
}

// TestAWellFormedResolutionDischargesExactlyWhatItWasDelivered is the positive
// case, so every refusal below is a refusal OF something that otherwise works.
func TestAWellFormedResolutionDischargesExactlyWhatItWasDelivered(t *testing.T) {
	resolved, err := AdmitFeedbackResolution([]string{"review:1", "review:2"}, resolutionSubject, validResolution("review:2", "review:1"))
	if err != nil {
		t.Fatalf("a well-formed resolution naming exactly what was delivered was refused: %v", err)
	}
	if len(resolved) != 2 || resolved[0] != "review:1" || resolved[1] != "review:2" {
		t.Fatalf("resolved keys were not the exact delivered set, sorted: %v", resolved)
	}
}

// TestNoResolutionAdmitsNothing is the fail-closed default a provider that
// said nothing at all gets: nil in, nil out, no error.
func TestNoResolutionAdmitsNothing(t *testing.T) {
	resolved, err := AdmitFeedbackResolution([]string{"review:1"}, resolutionSubject, nil)
	if err != nil || resolved != nil {
		t.Fatalf("an absent resolution was not treated as no claim at all: resolved=%v err=%v", resolved, err)
	}
}

// TestAFeedbackResolutionRefusalNamesTheExactReason proves every refusal is
// distinguishable - an operator or a retrying provider reading the diagnostic
// must see WHY the claim did not bind, not a bare "refused".
func TestAFeedbackResolutionRefusalNamesTheExactReason(t *testing.T) {
	delivered := []string{"review:1", "review:2"}
	cases := []struct {
		name     string
		result   *FeedbackResolution
		subject  string
		wantText string
	}{
		{
			name:     "wrong schema version",
			result:   &FeedbackResolution{SchemaVersion: "9.9", Resolution: FeedbackResolutionNoChangeRequired, Subject: resolutionSubject, Keys: delivered},
			subject:  resolutionSubject,
			wantText: "schema version",
		},
		{
			name:     "unrecognized resolution value",
			result:   &FeedbackResolution{SchemaVersion: FeedbackResolutionSchemaVersion, Resolution: "change_required", Subject: resolutionSubject, Keys: delivered},
			subject:  resolutionSubject,
			wantText: "is not",
		},
		{
			name:     "wrong subject revision",
			result:   validResolution(delivered...),
			subject:  "abcabcabcabcabcabcabcabcabcabcabcabcabc",
			wantText: "claims subject",
		},
		{
			name:     "no keys named",
			result:   validResolution(),
			subject:  resolutionSubject,
			wantText: "discharges nothing",
		},
		{
			name:     "partial keys: fewer than delivered",
			result:   validResolution("review:1"),
			subject:  resolutionSubject,
			wantText: "exactly what it was given",
		},
		{
			name:     "wrong keys: names an item never delivered",
			result:   validResolution("review:1", "review:9"),
			subject:  resolutionSubject,
			wantText: "never delivered",
		},
		{
			name:     "extra keys: more than delivered, all claimed valid",
			result:   validResolution("review:1", "review:2", "review:3"),
			subject:  resolutionSubject,
			wantText: "exactly what it was given",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := AdmitFeedbackResolution(delivered, tc.subject, tc.result)
			if err == nil {
				t.Fatalf("a malformed resolution (%s) was admitted: %v", tc.name, resolved)
			}
			if resolved != nil {
				t.Fatalf("a refused resolution discharged keys anyway: %v", resolved)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("refusal reason %q does not name the actual defect (want substring %q)", err.Error(), tc.wantText)
			}
		})
	}
}

// TestAFeedbackResolutionCollapsesDuplicateKeysBeforeComparison proves the
// exact-match rule is against the DISTINCT claimed set, not the literal list -
// a provider that named one key twice is not thereby claiming two keys.
func TestAFeedbackResolutionCollapsesDuplicateKeysBeforeComparison(t *testing.T) {
	resolved, err := AdmitFeedbackResolution([]string{"review:1"}, resolutionSubject, validResolution("review:1", "review:1", " "))
	if err != nil {
		t.Fatalf("a resolution with a duplicated key and blank padding was refused: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "review:1" {
		t.Fatalf("duplicate collapse produced the wrong set: %v", resolved)
	}
}

// TestReadFeedbackResolutionAbsentIsNotAClaim is the other half of the
// fail-closed default: a provider that wrote nothing leaves (nil, nil), the
// same shape AdmitFeedbackResolution treats as no claim at all.
func TestReadFeedbackResolutionAbsentIsNotAClaim(t *testing.T) {
	dir := t.TempDir()
	result, err := ReadFeedbackResolution(filepath.Join(dir, "never-written.json"))
	if err != nil || result != nil {
		t.Fatalf("an absent resolution file was not read as nothing: result=%v err=%v", result, err)
	}
	if result, err := ReadFeedbackResolution(""); err != nil || result != nil {
		t.Fatalf("an empty path was not read as nothing: result=%v err=%v", result, err)
	}
}

// TestReadFeedbackResolutionRejectsMalformedDocuments proves the decoder
// fails closed on shapes a hand-written or truncated file could produce,
// rather than silently reading a partial or wrong document as a claim.
func TestReadFeedbackResolutionRejectsMalformedDocuments(t *testing.T) {
	cases := map[string]string{
		"not json at all":         "not a json document",
		"unknown field":           `{"schema_version":"0.1","resolution":"no_change_required","subject":"a","keys":["k"],"unexpected":true}`,
		"trailing data after doc": `{"schema_version":"0.1","resolution":"no_change_required","subject":"a","keys":["k"]}garbage`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "resolution.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if result, err := ReadFeedbackResolution(path); err == nil {
				t.Fatalf("a malformed document (%s) was accepted as a resolution: %+v", name, result)
			}
		})
	}
}

// TestReadFeedbackResolutionRejectsOversizedDocuments bounds what the runtime
// will read at all, for the same denial-of-service reason the reviewer result
// channel is bounded.
func TestReadFeedbackResolutionRejectsOversizedDocuments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolution.json")
	if err := os.WriteFile(path, make([]byte, maxFeedbackResolutionBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := ReadFeedbackResolution(path); err == nil {
		t.Fatalf("an oversized resolution document was read: %+v", result)
	}
}

// TestReadFeedbackResolutionRoundTripsAWellFormedDocument proves the decoder
// and AdmitFeedbackResolution compose exactly the way CLIAgentProvider and
// invokeExecution actually use them: write, read, admit.
func TestReadFeedbackResolutionRoundTripsAWellFormedDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolution.json")
	document, err := json.Marshal(validResolution("review:1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadFeedbackResolution(path)
	if err != nil || result == nil {
		t.Fatalf("a well-formed document was not read back: result=%v err=%v", result, err)
	}
	resolved, admitErr := AdmitFeedbackResolution([]string{"review:1"}, resolutionSubject, result)
	if admitErr != nil || len(resolved) != 1 || resolved[0] != "review:1" {
		t.Fatalf("a round-tripped well-formed document did not admit: resolved=%v err=%v", resolved, admitErr)
	}
}
