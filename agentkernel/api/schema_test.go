package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

const (
	schemaDir    = "../schemas"
	requestFile  = "execution-request.v0.2.schema.json"
	resultFile   = "execution-result.v0.2.schema.json"
	eventFile    = "event.v0.2.schema.json"
	commonFile   = "common.v0.2.schema.json"
	examplesDir  = "../schemas/examples"
	validDir     = examplesDir + "/valid"
	invalidDir   = examplesDir + "/invalid"
	rootPointer  = ""
	defsPrefix   = "/$defs/"
	refSeparator = "#"
)

// fixedNow is the validation time for every fixture; valid fixtures use far
// future deadlines and the past-deadline fixture a far past one.
var fixedNow = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// invalidRequestField is the ValidationError field each invalid fixture must
// be refused with. Every invalid fixture must appear here and vice versa.
var invalidRequestField = map[string]string{
	"request-unknown-version.json":            "version",
	"request-v0.1-version.json":               "version",
	"request-unknown-field.json":              "$",
	"request-duplicate-key.json":              "objective",
	"request-invalid-identifier.json":         "execution_id",
	"request-instruction-non-host-trust.json": "context[0]",
	"request-write-grant-read-only.json":      "grants[1]",
	"request-money-unpriced-eligible.json":    "providers[0]",
	"request-two-pinned.json":                 "providers",
	"request-money-unknown-write-rate.json":   "providers[0]",
	"request-bad-digest.json":                 "workspace.manifest_digest",
	"request-content-digest-mismatch.json":    "context[0]",
	"request-root-traversal.json":             "grants[0]",
	"request-zero-budget.json":                "budget.max_tool_calls",
	"request-past-deadline.json":              "budget.deadline",
	"request-unsupported-feature.json":        "constraints.required_features",
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob %s: %v (%d matches)", pattern, err, len(paths))
	}
	return paths
}

// strictDecode mirrors the request decoder's unknown-field and trailing-data
// refusal for the kernel-emitted result and event shapes.
func strictDecode[T any](data []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return v, errors.New("trailing data after JSON value")
	}
	return v, nil
}

// assertRoundTrip proves decode -> encode loses and invents nothing: the
// re-encoded document equals the fixture as generic JSON, and re-decodes to
// an equal value.
func assertRoundTrip[T any](t *testing.T, data []byte, decode func([]byte) (T, error)) {
	t.Helper()
	first, err := decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	second, err := decode(encoded)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("round trip changed value:\nfirst  %+v\nsecond %+v", first, second)
	}
	var original, again any
	if err := json.Unmarshal(data, &original); err != nil {
		t.Fatalf("generic decode: %v", err)
	}
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("generic re-decode: %v", err)
	}
	if !reflect.DeepEqual(original, again) {
		t.Fatalf("round trip changed JSON:\nfixture %s\nencoded %s", data, encoded)
	}
}

func TestValidRequestExamplesDecodeValidateAndRoundTrip(t *testing.T) {
	for _, path := range glob(t, filepath.Join(validDir, "request-*.json")) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data := readFile(t, path)
			request, err := api.DecodeRequest(data)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if err := request.Validate(fixedNow); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			assertRoundTrip(t, data, api.DecodeRequest)
		})
	}
}

func TestValidResultAndEventExamplesDecodeStrictlyAndRoundTrip(t *testing.T) {
	for _, path := range glob(t, filepath.Join(validDir, "result-*.json")) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, readFile(t, path), strictDecode[api.ExecutionResult])
		})
	}
	for _, path := range glob(t, filepath.Join(validDir, "event-*.json")) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, readFile(t, path), strictDecode[api.Event])
		})
	}
}

func TestInvalidRequestExamplesAreRefusedOnTheExpectedField(t *testing.T) {
	seen := map[string]bool{}
	for _, path := range glob(t, filepath.Join(invalidDir, "*.json")) {
		name := filepath.Base(path)
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			want, ok := invalidRequestField[name]
			if !ok {
				t.Fatalf("invalid fixture %s has no expected field in invalidRequestField", name)
			}
			err := decodeAndValidate(readFile(t, path))
			var verr *api.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *api.ValidationError on %q, got %v", want, err)
			}
			if verr.Field != want {
				t.Fatalf("refused on field %q (%s), want %q", verr.Field, verr.Reason, want)
			}
		})
	}
	for name := range invalidRequestField {
		if !seen[name] {
			t.Errorf("invalidRequestField names missing fixture %s", name)
		}
	}
}

func decodeAndValidate(data []byte) error {
	request, err := api.DecodeRequest(data)
	if err != nil {
		return err
	}
	return request.Validate(fixedNow)
}

// schemaSet holds every schema document by file name and resolves the
// relative "$ref" form the schemas use: "file#/$defs/x" or "#/$defs/x".
type schemaSet map[string]map[string]any

func loadSchemas(t *testing.T) schemaSet {
	t.Helper()
	set := schemaSet{}
	for _, path := range glob(t, filepath.Join(schemaDir, "*.schema.json")) {
		var doc map[string]any
		if err := json.Unmarshal(readFile(t, path), &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		set[filepath.Base(path)] = doc
	}
	return set
}

// node returns the schema object at a JSON pointer of the "/$defs/name" form.
func (s schemaSet) node(t *testing.T, file, pointer string) map[string]any {
	t.Helper()
	doc, ok := s[file]
	if !ok {
		t.Fatalf("no schema file %s", file)
	}
	if pointer == rootPointer {
		return doc
	}
	defs, _ := doc["$defs"].(map[string]any)
	def, ok := defs[strings.TrimPrefix(pointer, defsPrefix)].(map[string]any)
	if !strings.HasPrefix(pointer, defsPrefix) || !ok {
		t.Fatalf("%s: no definition at %s", file, pointer)
	}
	return def
}

// resolve follows a $ref relative to the file it appears in.
func (s schemaSet) resolve(t *testing.T, file, ref string) (string, string) {
	t.Helper()
	target, pointer, ok := strings.Cut(ref, refSeparator)
	if !ok {
		t.Fatalf("%s: unsupported $ref %q", file, ref)
	}
	if target == "" {
		target = file
	}
	s.node(t, target, pointer)
	return target, pointer
}
