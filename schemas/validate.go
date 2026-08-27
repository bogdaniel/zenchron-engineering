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
	}

	compiled := make(map[string]*jsonschema.Schema, len(names))
	for _, name := range names {
		validator, err := compiler.Compile(name + ".schema.json")
		if err != nil {
			panic(err)
		}
		compiled[name] = validator
	}
	return compiled
}
