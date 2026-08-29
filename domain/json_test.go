package domain_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestValidFixturesRoundTrip(t *testing.T) {
	files, err := filepath.Glob("../fixtures/v0.1/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			roundTripFixture(t, filepath.Base(file), data)
		})
	}
}

func TestInvalidFixturesRejected(t *testing.T) {
	files, err := filepath.Glob("../fixtures/v0.1/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeFixture(filepath.Base(file), data); err == nil {
				t.Fatal("expected invalid fixture to fail")
			}
		})
	}
}

func TestFactValuesRemainDistinct(t *testing.T) {
	values := []struct {
		json string
		want domain.FactValue
	}{
		{"true", domain.FactTrue},
		{"false", domain.FactFalse},
		{`"unknown"`, domain.FactUnknown},
		{`"unk\u006eown"`, domain.FactUnknown},
	}

	seen := make(map[domain.FactValue]bool)
	for _, test := range values {
		fact, err := domain.Decode[domain.EngineeringFact](factJSON(test.json))
		if err != nil {
			t.Fatalf("decode %s: %v", test.json, err)
		}
		if fact.Value != test.want {
			t.Fatalf("decode %s = %q, want %q", test.json, fact.Value, test.want)
		}
		seen[fact.Value] = true
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct fact values, want 3", len(seen))
	}

	missing := bytes.Replace(factJSON("true"), []byte("\n  \"value\":true,"), nil, 1)
	if _, err := domain.Decode[domain.EngineeringFact](missing); err == nil {
		t.Fatal("missing fact value must fail")
	}
	if _, err := domain.Encode(domain.EngineeringFact{}); err == nil {
		t.Fatal("zero fact value must fail")
	}
}

func TestDuplicateObjectMembersRejected(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		decode     func([]byte) error
		wantPath   string
		wantMember string
	}{
		{
			name: "root object",
			data: bytes.Replace(factJSON("true"), []byte(`"schema_version":"0.1"`), []byte(`"schema_version":"0.1","schema_version":"0.1"`), 1),
			decode: func(data []byte) error {
				_, err := domain.Decode[domain.EngineeringFact](data)
				return err
			},
			wantMember: "schema_version",
		},
		{
			name: "nested ordinary object",
			data: bytes.Replace(factJSON("true"), []byte(`"repository":"example/repo"`), []byte(`"repository":"example/a","repository":"example/b"`), 1),
			decode: func(data []byte) error {
				_, err := domain.Decode[domain.EngineeringFact](data)
				return err
			},
			wantPath:   "/subject",
			wantMember: "repository",
		},
		{
			name: "security evidence identity map",
			data: duplicateEvidenceJSON(),
			decode: func(data []byte) error {
				_, err := domain.Decode[domain.EvidenceBundle](data)
				return err
			},
			wantPath:   "/evidence",
			wantMember: "evidence-security-review",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.decode(test.data)
			var duplicate *domain.DuplicateMemberError
			if !errors.As(err, &duplicate) {
				t.Fatalf("expected DuplicateMemberError, got %v", err)
			}
			if duplicate.Path != test.wantPath || duplicate.Member != test.wantMember {
				t.Fatalf("duplicate = %#v, want path %q member %q", duplicate, test.wantPath, test.wantMember)
			}
		})
	}
}

func TestTrailingContentRejected(t *testing.T) {
	valid := factJSON("true")
	tests := map[string][]byte{
		"second JSON value": append(append([]byte{}, valid...), valid...),
		"garbage":           append(append([]byte{}, valid...), []byte(" garbage")...),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.Decode[domain.EngineeringFact](data); err == nil {
				t.Fatal("expected trailing content to fail")
			}
		})
	}
}

func TestEncodeRejectsSchemaInvalidState(t *testing.T) {
	data, err := os.ReadFile("../fixtures/v0.1/valid/trivial.authority-decision.json")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.Decode[domain.AuthorityDecision](data)
	if err != nil {
		t.Fatal(err)
	}
	decision.Permission.Status = domain.PermissionDenied
	if _, err := domain.Encode(decision); err == nil {
		t.Fatal("authorized decision with denied permission must fail")
	}
}

func TestMapSerializationIsDeterministic(t *testing.T) {
	data, err := os.ReadFile("../fixtures/v0.1/valid/security-sensitive.engineering-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	first, err := domain.Decode[domain.EngineeringPolicy](data)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Rules = make(map[string]domain.PolicyRule, len(first.Rules))
	ruleIDs := make([]string, 0, len(first.Rules))
	for id := range first.Rules {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ruleIDs)))
	for _, id := range ruleIDs {
		second.Rules[id] = first.Rules[id]
	}

	firstJSON, err := domain.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := domain.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("map insertion order changed serialization:\n%s\n%s", firstJSON, secondJSON)
	}
}

func roundTripFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	switch {
	case strings.HasSuffix(name, ".authority-decision.json"):
		roundTrip[domain.AuthorityDecision](t, data)
	case strings.HasSuffix(name, ".engineering-fact.json"):
		roundTrip[domain.EngineeringFact](t, data)
	case strings.HasSuffix(name, ".engineering-policy.json"):
		roundTrip[domain.EngineeringPolicy](t, data)
	case strings.HasSuffix(name, ".engineering-work-contract.json"):
		roundTrip[domain.EngineeringWorkContract](t, data)
	case strings.HasSuffix(name, ".evidence-bundle.json"):
		roundTrip[domain.EvidenceBundle](t, data)
	case strings.HasSuffix(name, ".project-model.json"):
		roundTrip[domain.ProjectModel](t, data)
	default:
		t.Fatalf("fixture %s has no contract suffix", name)
	}
}

func roundTrip[T domain.Contract](t *testing.T, data []byte) {
	t.Helper()
	first, err := domain.Decode[T](data)
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	encoded, err := domain.Encode(first)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	second, err := domain.Decode[T](encoded)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("round trip changed value:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	reencoded, err := domain.Encode(second)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("encoding was not deterministic:\n%s\n%s", encoded, reencoded)
	}
}

func decodeFixture(name string, data []byte) error {
	switch {
	case strings.HasSuffix(name, ".authority-decision.json"):
		_, err := domain.Decode[domain.AuthorityDecision](data)
		return err
	case strings.HasSuffix(name, ".engineering-fact.json"):
		_, err := domain.Decode[domain.EngineeringFact](data)
		return err
	case strings.HasSuffix(name, ".engineering-policy.json"):
		_, err := domain.Decode[domain.EngineeringPolicy](data)
		return err
	case strings.HasSuffix(name, ".engineering-work-contract.json"):
		_, err := domain.Decode[domain.EngineeringWorkContract](data)
		return err
	case strings.HasSuffix(name, ".evidence-bundle.json"):
		_, err := domain.Decode[domain.EvidenceBundle](data)
		return err
	case strings.HasSuffix(name, ".project-model.json"):
		_, err := domain.Decode[domain.ProjectModel](data)
		return err
	default:
		return fmt.Errorf("fixture %s has no contract suffix", name)
	}
}

func factJSON(value string) []byte {
	return []byte(fmt.Sprintf(`{
  "schema_version":"0.1",
  "id":"fact-test",
  "key":"authentication.boundary_modified",
  "value":%s,
  "stage":"observed",
  "confidence":"high",
  "subject":{"repository":"example/repo","revision":"abc123"},
  "provenance":{"type":"static_analysis","producer":"test-detector"}
}`, value))
}

func duplicateEvidenceJSON() []byte {
	return []byte(`{
  "schema_version":"0.1",
  "id":"evidence-auth-review",
  "revision":"1",
  "subject":{"repository":"example/repo","revision":"abc123"},
  "contract":{"id":"contract-auth","revision":"1"},
  "policy":{"id":"policy-baseline","revision":"1"},
  "evidence":{
    "evidence-security-review":{
      "claim_id":"claim-security-review",
      "evidence_class":"security_review",
      "producer":{"id":"reviewer-1","type":"human"},
      "environment":{"type":"code_review","identifier":"review-1"},
      "result":{"status":"pass"},
      "lifecycle":{"status":"valid"},
      "provenance":{"source":"review-system","recorded_at":"2026-08-27T12:00:00Z"}
    },
    "evidence-security-review":{
      "claim_id":"claim-security-review",
      "evidence_class":"security_review",
      "producer":{"id":"change-producer","type":"execution_provider"},
      "environment":{"type":"local_tooling","identifier":"untrusted-run"},
      "result":{"status":"fail"},
      "lifecycle":{"status":"valid"},
      "provenance":{"source":"local-runtime","recorded_at":"2026-08-27T12:01:00Z"}
    }
  }
}`)
}
