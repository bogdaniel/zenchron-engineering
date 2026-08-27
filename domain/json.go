package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/schemas"
)

// Contract is the set of durable v0.1 root contracts.
type Contract interface {
	ProjectModel | EngineeringFact | EngineeringPolicy | EngineeringWorkContract | EvidenceBundle | AuthorityDecision
}

// DuplicateMemberError reports an ambiguous JSON object member. Path is the
// JSON Pointer of the containing object; the root object uses an empty path.
type DuplicateMemberError struct {
	Path   string
	Member string
}

func (e *DuplicateMemberError) Error() string {
	path := e.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("duplicate JSON object member %q at %s", e.Member, path)
}

// Decode strictly decodes and validates exactly one v0.1 contract.
func Decode[T Contract](data []byte) (T, error) {
	var zero T
	name := schemaName[T]()
	instance, err := decodeStrictJSON(data)
	if err != nil {
		return zero, fmt.Errorf("decode %s: %w", name, err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, fmt.Errorf("decode %s representation: %w", name, err)
	}
	if err := schemas.Validate(name, instance); err != nil {
		return zero, fmt.Errorf("validate %s: %w", name, err)
	}
	return value, nil
}

// Encode serializes and validates one v0.1 contract without added whitespace.
func Encode[T Contract](value T) ([]byte, error) {
	name := schemaName[T]()
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", name, err)
	}
	instance, err := decodeStrictJSON(data)
	if err != nil {
		return nil, fmt.Errorf("verify encoded %s: %w", name, err)
	}
	if err := schemas.Validate(name, instance); err != nil {
		return nil, fmt.Errorf("validate encoded %s: %w", name, err)
	}
	return data, nil
}

func schemaName[T Contract]() string {
	var value T
	switch any(value).(type) {
	case ProjectModel:
		return schemas.ProjectModel
	case EngineeringFact:
		return schemas.EngineeringFact
	case EngineeringPolicy:
		return schemas.EngineeringPolicy
	case EngineeringWorkContract:
		return schemas.EngineeringWorkContract
	case EvidenceBundle:
		return schemas.EvidenceBundle
	case AuthorityDecision:
		return schemas.AuthorityDecision
	default:
		panic("unreachable contract type")
	}
}

func decodeStrictJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, "")
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("input must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("invalid trailing content: %w", err)
	}
	return value, nil
}

func readJSONValue(decoder *json.Decoder, path string) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object member name must be a string")
			}
			if _, exists := object[key]; exists {
				return nil, &DuplicateMemberError{Path: path, Member: key}
			}
			value, err := readJSONValue(decoder, joinJSONPointer(path, key))
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := readJSONValue(decoder, joinJSONPointer(path, strconv.Itoa(len(array))))
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func joinJSONPointer(path, token string) string {
	token = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	return path + "/" + token
}
