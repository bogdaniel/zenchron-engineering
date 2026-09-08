package schemas_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"encoding/json"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

var fixtureSchemas = map[string]string{
	"authority-decision":        "authority-decision.schema.json",
	"engineering-fact":          "engineering-fact.schema.json",
	"engineering-policy":        "engineering-policy.schema.json",
	"engineering-work-contract": "engineering-work-contract.schema.json",
	"evidence-bundle":           "evidence-bundle.schema.json",
	"project-model":             "project-model.schema.json",
	"agent-assignment":          "agent-assignment.schema.json",
	"agent-profile":             "agent-profile.schema.json",
	"context-policy":            "context-policy.schema.json",
	"engineering-plan":          "engineering-plan.schema.json",
	"engineering-plan-template": "engineering-plan-template.schema.json",
	"instruction-pack":          "instruction-pack.schema.json",
	"plan-revision-proposal":    "plan-revision-proposal.schema.json",
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
	// The M2 planning refusals. Each one is a boundary #64 freezes, expressed
	// where a malformed document can be refused before anything reads it.
	//
	// Instruction content is operator-owned: a repository cannot author it.
	"candidate-authored.instruction-pack.json": {"/source/type", "enum"},
	// A ContextPolicy selects from a CLOSED vocabulary; it is not a query
	// language over whatever a caller can name.
	"unknown-context-class.context-policy.json": {"/exclude/0", "enum"},
	// A profile specializes an existing worker. It cannot invent an ability,
	// which is the shape "customization may not escalate" takes in a schema.
	"invented-capability.agent-profile.json": {"/capabilities/0", "enum"},
	// Nested plans are not representable, in a template or in a plan.
	"nested-plan.engineering-plan-template.json": {"/stages/0/kind", "enum"},
	"nested-plan.engineering-plan.json":          {"/stages/0", "additionalProperties"},
	// A gate is not performed by a worker. Go refuses this too; the schema
	// refusing it as well is what stops such a document from being a plan at
	// all, rather than a plan the compiler happens to clean up.
	"gate-states-a-worker-role.engineering-plan.json": {"/stages/0", "then"},
	// Roles come from the catalogue; an assignment cannot name a
	// responsibility nothing can resolve.
	"unknown-role.agent-assignment.json": {"/role", "enum"},
	// A proposal without a validation verdict could be approved without ever
	// having been checked.
	"missing-validation.plan-revision-proposal.json": {"", "required"},
	// Policy states obligations, never which worker performs them: naming an
	// agent in a role obligation is the fixed-agent-workflow refusal again, in
	// the new vocabulary.
	"agent-workflow-obligation.engineering-policy.json": {"/rules/RULE-001/effect/engineering_requirements/roles/0", "additionalProperties"},
	// A gate is not an agent stage. Only `agent` stages become EngineeringRuns.
	"agent-gate-requirement.engineering-work-contract.json": {"/plan_requirements/gates/0/kind", "enum"},
}

func TestSchemasCompile(t *testing.T) {
	files, err := filepath.Glob("*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	// Every schema is either a document type with fixtures, or the shared
	// vocabulary those documents reference. A file that is neither is a schema
	// nothing validates against and nothing refers to.
	if want := len(fixtureSchemas) + 1; len(files) != want {
		t.Fatalf("found %d schemas, want %d (the document types plus the planning vocabulary)", len(files), want)
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
	// Every schema in the directory is registered first, under its path and its
	// $id, so a reference between two of them resolves here exactly as it does
	// in the package's own compiler - offline, against the files on disk.
	siblings, err := filepath.Glob(filepath.Join(filepath.Dir(file), "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sibling := range siblings {
		body, err := os.ReadFile(sibling)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("parse %s: %v", sibling, err)
		}
		if err := compiler.AddResource(sibling, document); err != nil {
			t.Fatalf("register %s: %v", sibling, err)
		}
		if id, ok := document.(map[string]any)["$id"].(string); ok && id != "" {
			if err := compiler.AddResource(id, document); err != nil {
				t.Fatalf("register %s by id: %v", sibling, err)
			}
		}
	}
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

// The schema vocabulary and the Go catalogue are ONE catalogue.
//
// Extracting the role and capability enums into a shared schema removed the
// six-place duplication, but a shared schema can still drift from the Go
// constants that produce and consume these documents - and that drift is
// silent in exactly the dangerous direction: a role the schema accepts and no
// resolver knows, or a capability the code emits and the schema refuses.
func TestTheSchemaVocabularyMatchesTheDomainCatalogue(t *testing.T) {
	body, err := os.ReadFile("planning-vocabulary.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var vocabulary struct {
		Defs struct {
			Role       struct{ Enum []string } `json:"role"`
			Capability struct{ Enum []string } `json:"capability"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &vocabulary); err != nil {
		t.Fatal(err)
	}

	roles := make([]string, 0, len(domain.EngineeringRoles()))
	for _, role := range domain.EngineeringRoles() {
		roles = append(roles, string(role))
	}
	capabilities := make([]string, 0, len(domain.EngineeringCapabilities()))
	for _, capability := range domain.EngineeringCapabilities() {
		capabilities = append(capabilities, string(capability))
	}
	if !slices.Equal(vocabulary.Defs.Role.Enum, roles) {
		t.Fatalf("the schema role catalogue is %v and the domain catalogue is %v", vocabulary.Defs.Role.Enum, roles)
	}
	if !slices.Equal(vocabulary.Defs.Capability.Enum, capabilities) {
		t.Fatalf("the schema capability catalogue is %v and the domain catalogue is %v", vocabulary.Defs.Capability.Enum, capabilities)
	}
}
