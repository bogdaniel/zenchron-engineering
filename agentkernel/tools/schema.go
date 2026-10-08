package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/strictjson"
)

// The broker enforces exactly this JSON Schema subset, and NewBroker refuses
// any tool schema outside it, so no keyword can be shown to a provider and
// then silently go unenforced:
//
//	{"type":"object","additionalProperties":false,"required":[...],
//	 "properties":{"<name>":{"type":"string"|"integer"|"boolean","description":"..."}
//	              |{"type":"array","items":{"type":"string"},"description":"..."}}}
type objectSchema struct {
	props    map[string]string // property name -> type ("array" means array of string)
	required []string
}

type rawSchema struct {
	Type                 string             `json:"type"`
	Properties           map[string]rawProp `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties *bool              `json:"additionalProperties"`
}

type rawProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Items       *struct {
		Type string `json:"type"`
	} `json:"items"`
}

func parseSchema(data json.RawMessage) (objectSchema, error) {
	var raw rawSchema
	if err := strictjson.Decode(data, &raw); err != nil {
		return objectSchema{}, fmt.Errorf("input schema: %w", err)
	}
	if raw.Type != "object" || raw.AdditionalProperties == nil || *raw.AdditionalProperties {
		return objectSchema{}, errors.New("input schema must be an object with additionalProperties false")
	}
	s := objectSchema{props: map[string]string{}, required: raw.Required}
	for name, p := range raw.Properties {
		isArray := p.Type == "array"
		if isArray != (p.Items != nil) || (isArray && p.Items.Type != "string") {
			return objectSchema{}, fmt.Errorf("property %q: only arrays of strings take items", name)
		}
		if !isArray && p.Type != "string" && p.Type != "integer" && p.Type != "boolean" {
			return objectSchema{}, fmt.Errorf("property %q: unsupported type %q", name, p.Type)
		}
		s.props[name] = p.Type
	}
	for i, r := range raw.Required {
		if _, ok := s.props[r]; !ok || slices.Contains(raw.Required[:i], r) {
			return objectSchema{}, fmt.Errorf("required %q must name one declared property once", r)
		}
	}
	return s, nil
}

// validate refuses arguments that are not exactly an object of the declared
// fields: unknown fields, wrong types, nulls, duplicate keys and trailing data.
func (s objectSchema) validate(args json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := strictjson.Decode(args, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("arguments must be a JSON object")
	}
	for name, value := range fields {
		typ, ok := s.props[name]
		if !ok {
			return fmt.Errorf("unknown field %q", name)
		}
		if !hasType(value, typ) {
			return fmt.Errorf("field %q must be %s", name, typ)
		}
	}
	for _, r := range s.required {
		if _, ok := fields[r]; !ok {
			return fmt.Errorf("missing required field %q", r)
		}
	}
	return nil
}

var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func hasType(value json.RawMessage, typ string) bool {
	v := bytes.TrimSpace(value)
	switch typ {
	case "string":
		return len(v) > 0 && v[0] == '"'
	case "boolean":
		return string(v) == "true" || string(v) == "false"
	case "integer":
		_, err := strconv.ParseInt(string(v), 10, 64)
		return integerLiteral.Match(v) && err == nil
	case "array":
		var items []json.RawMessage
		if len(v) == 0 || v[0] != '[' || json.Unmarshal(v, &items) != nil {
			return false
		}
		return !slices.ContainsFunc(items, func(it json.RawMessage) bool { return !hasType(it, "string") })
	}
	return false
}
