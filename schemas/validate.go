// Package schemas embeds and validates the normative v0.1 JSON Schemas.
package schemas

import (
	"bytes"
	"embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Schema names accepted by Validate.
const (
	AuthorityDecision       = "authority-decision"
	EngineeringFact         = "engineering-fact"
	EngineeringPolicy       = "engineering-policy"
	EngineeringWorkContract = "engineering-work-contract"
	EvidenceBundle          = "evidence-bundle"
	ProjectModel            = "project-model"
	// The M2 planning artifacts. They follow exactly the same discipline as
	// the kernel contracts above - one schema, valid and invalid fixtures, one
	// validation path - because a second validation framework beside this one
	// would be a second definition of what a durable artifact is.
	AgentAssignment         = "agent-assignment"
	AgentProfile            = "agent-profile"
	ContextPolicy           = "context-policy"
	EngineeringPlan         = "engineering-plan"
	EngineeringPlanTemplate = "engineering-plan-template"
	InstructionPack         = "instruction-pack"
	PlanRevisionProposal    = "plan-revision-proposal"
	// PlanningVocabulary is not a document type. It is the one definition of
	// the role and capability catalogues, referenced by the schemas that used
	// to restate them, and it is compiled here so those references resolve.
	PlanningVocabulary = "planning-vocabulary"
)

//go:embed *.schema.json
var schemaFiles embed.FS

var validators = mustCompile()

// Validate checks an already decoded JSON value against a v0.1 schema.
func Validate(name string, value any) error {
	validator, ok := validators[name]
	if !ok {
		return fmt.Errorf("unknown schema %q", name)
	}
	return validator.Validate(value)
}

func mustCompile() map[string]*jsonschema.Schema {
	names := []string{
		AuthorityDecision,
		EngineeringFact,
		EngineeringPolicy,
		EngineeringWorkContract,
		EvidenceBundle,
		ProjectModel,
		AgentAssignment,
		AgentProfile,
		ContextPolicy,
		EngineeringPlan,
		EngineeringPlanTemplate,
		InstructionPack,
		PlanRevisionProposal,
		PlanningVocabulary,
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()

	for _, name := range names {
		file := name + ".schema.json"
		data, err := schemaFiles.ReadFile(file)
		if err != nil {
			panic(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			panic(err)
		}
		if err := compiler.AddResource(file, document); err != nil {
			panic(err)
		}
		// ALSO under the document's own $id. A `$ref` inside a schema resolves
		// against that schema's base URI, which is its $id - so a reference
		// between two of these files becomes the https form, and without this
		// the compiler would try to fetch it. Registering both names is what
		// lets one schema reference another with no network and no second copy
		// of the referenced definition.
		if id, ok := document.(map[string]any)["$id"].(string); ok && id != "" {
			if err := compiler.AddResource(id, document); err != nil {
				panic(err)
			}
		}
	}

	compiled := make(map[string]*jsonschema.Schema, len(names))
	for _, name := range names {
		if name == PlanningVocabulary {
			// A vocabulary is referenced, never validated against: no artifact
			// is "a planning vocabulary".
			continue
		}
		validator, err := compiler.Compile(name + ".schema.json")
		if err != nil {
			panic(err)
		}
		compiled[name] = validator
	}
	return compiled
}
