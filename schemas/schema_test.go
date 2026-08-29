package schemas_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var fixtureSchemas = map[string]string{
	"authority-decision":        "authority-decision.schema.json",
	"engineering-fact":          "engineering-fact.schema.json",
	"engineering-policy":        "engineering-policy.schema.json",
	"engineering-work-contract": "engineering-work-contract.schema.json",
	"evidence-bundle":           "evidence-bundle.schema.json",
	"project-model":             "project-model.schema.json",
}

type invalidExpectation struct {
	instanceLocation string
	keyword          string
}

var invalidExpectations = map[string]invalidExpectation{
	"ambiguous-unknown.engineering-fact.json":                   {"/value", "oneOf"},
	"array-evidence-basis.authority-decision.json":              {"/basis/evidence_bundles", "type"},
	"array-identities.engineering-policy.json":                  {"/rules", "type"},
	"array-identities.engineering-work-contract.json":           {"/required_claims", "type"},
	"array-identities.evidence-bundle.json":                     {"/evidence", "type"},
	"array-identities.project-model.json":                       {"/critical_boundaries", "type"},
	"authorized-with-denied-permission.authority-decision.json": {"/permission/status", "const"},
	"fixed-agent-workflow.engineering-policy.json":              {"/rules/RULE-001", "additionalProperties"},
	"fixed-agent-workflow.engineering-work-contract.json":       {"", "additionalProperties"},
	"missing-environment.evidence-bundle.json":                  {"/evidence/evidence-security-review", "required"},
	"missing-change-producer.authority-decision.json":           {"/basis", "required"},
	"missing-evidence-class.evidence-bundle.json":               {"/evidence/evidence-json-parse", "required"},
	"missing-subject-revision.evidence-bundle.json":             {"/subject", "required"},
	"missing-subject-revision.project-model.json":               {"/subject", "required"},
	"stale-without-reason.evidence-bundle.json":                 {"/evidence/evidence-auth-tests/lifecycle", "required"},
}

func TestSchemasCompile(t *testing.T) {
	files, err := filepath.Glob("*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(fixtureSchemas) {
		t.Fatalf("found %d schemas, want %d", len(files), len(fixtureSchemas))
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			compileSchema(t, file)
		})
	}
}

func TestValidFixtures(t *testing.T) {
	validateFixtures(t, "../fixtures/v0.1/valid", true)
}

func TestInvalidFixtures(t *testing.T) {
	validateFixtures(t, "../fixtures/v0.1/invalid", false)
}

func validateFixtures(t *testing.T, dir string, wantValid bool) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures found in %s", dir)
	}

	compiled := make(map[string]*jsonschema.Schema, len(fixtureSchemas))
	counts := make(map[string]int, len(fixtureSchemas))
	for name, file := range fixtureSchemas {
		compiled[name] = compileSchema(t, file)
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			base := filepath.Base(file)
			name, err := schemaName(file)
			if err != nil {
				t.Fatal(err)
			}
			counts[name]++
			instanceJSON, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}

			instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(instanceJSON))
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			err = compiled[name].Validate(instance)
			if wantValid && err != nil {
				t.Fatalf("expected valid fixture: %v", err)
			}
			if !wantValid && err == nil {
				t.Fatal("expected invalid fixture to fail validation")
			}
			if !wantValid {
				want, ok := invalidExpectations[base]
				if !ok {
					t.Fatal("invalid fixture has no expected failure")
				}
				if !matchesValidationError(err, want) {
					t.Fatalf("expected error at %q for %q; got %v", want.instanceLocation, want.keyword, err)
				}
			}
		})
	}
	if !wantValid && len(files) != len(invalidExpectations) {
		t.Errorf("found %d invalid fixtures, want %d expectations", len(files), len(invalidExpectations))
	}

	for name := range fixtureSchemas {
		if counts[name] == 0 {
			t.Errorf("no fixtures for %s in %s", name, dir)
		}
	}
}

func matchesValidationError(err error, want invalidExpectation) bool {
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return false
	}
	return matchesOutput(validationErr.BasicOutput(), want)
}

func matchesOutput(output *jsonschema.OutputUnit, want invalidExpectation) bool {
	if output.InstanceLocation == want.instanceLocation && strings.HasSuffix(output.KeywordLocation, "/"+want.keyword) {
		return true
	}
	for i := range output.Errors {
		if matchesOutput(&output.Errors[i], want) {
			return true
		}
	}
	return false
}

func compileSchema(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schema, err := compiler.Compile(file)
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	return schema
}

func schemaName(file string) (string, error) {
	base := filepath.Base(file)
	for name := range fixtureSchemas {
		if strings.HasSuffix(base, "."+name+".json") {
			return name, nil
		}
	}
	return "", fmt.Errorf("fixture %s has no schema suffix", base)
}
