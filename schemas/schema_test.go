package schemas_test

import (
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
			name, err := schemaName(file)
			if err != nil {
				t.Fatal(err)
			}
			counts[name]++
			instanceFile, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer instanceFile.Close()

			instance, err := jsonschema.UnmarshalJSON(instanceFile)
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
		})
	}

	for name := range fixtureSchemas {
		if counts[name] == 0 {
			t.Errorf("no fixtures for %s in %s", name, dir)
		}
	}
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
