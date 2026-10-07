package api_test

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

var relativePathCases = []struct {
	path  string
	valid bool
}{
	{".", true},
	{"src", true},
	{"src/pkg", true},
	{"a.b/c-d_e", true},
	{".hidden/file", true},
	{"..dots", true},
	{"", false},
	{"..", false},
	{"../x", false},
	{"a/../b", false},
	{"a/..", false},
	{"./a", false},
	{"a/.", false},
	{"a/./b", false},
	{"/etc", false},
	{"/", false},
	{"a//b", false},
	{"a/", false},
	{`a\b`, false},
	{`..\x`, false},
	{"C:", false},
	{"C:/x", false},
	{"c:x", false},
	{"a\x00b", false},
}

func TestValidRelativePath(t *testing.T) {
	for _, tc := range relativePathCases {
		if got := api.ValidRelativePath(tc.path); got != tc.valid {
			t.Errorf("ValidRelativePath(%q) = %v, want %v", tc.path, got, tc.valid)
		}
	}
}

// TestRelativePathSchemaAgreesWithGo evaluates the schema's relative_path rule
// (const "." or not matching the forbidden pattern) with Go's regexp, which
// accepts this RE2/ECMA-262 common subset, against the same cases.
func TestRelativePathSchemaAgreesWithGo(t *testing.T) {
	def := loadSchemas(t).node(t, requestFile, "/$defs/relative_path")
	anyOf := def["anyOf"].([]any)
	dot := anyOf[0].(map[string]any)["const"].(string)
	forbidden := regexp.MustCompile(anyOf[1].(map[string]any)["not"].(map[string]any)["pattern"].(string))
	for _, tc := range relativePathCases {
		got := tc.path != "" && (tc.path == dot || !forbidden.MatchString(tc.path))
		if got != tc.valid {
			t.Errorf("schema relative_path(%q) = %v, want %v", tc.path, got, tc.valid)
		}
	}
}

func minimalRequest(t *testing.T) string {
	t.Helper()
	return string(readFile(t, filepath.Join(validDir, "request-minimal-read-only.json")))
}

func replaceOnce(t *testing.T, doc, old, replacement string) []byte {
	t.Helper()
	if strings.Count(doc, old) != 1 {
		t.Fatalf("fixture must contain %q exactly once", old)
	}
	return []byte(strings.Replace(doc, old, replacement, 1))
}

func TestDecodeRequestRefusesDuplicateKeys(t *testing.T) {
	doc := minimalRequest(t)
	cases := map[string]struct {
		data  []byte
		field string
	}{
		"top level": {
			replaceOnce(t, doc, `"mode": "read_only",`, `"mode": "read_only", "mode": "read_write",`), "mode",
		},
		"nested object": {
			replaceOnce(t, doc, `"id": "ws-1",`, `"id": "ws-1", "id": "ws-2",`), "id",
		},
		"object inside array": {
			replaceOnce(t, doc, `"kind": "file.read",`, `"kind": "file.read", "kind": "file.write",`), "kind",
		},
		"after a nested value": {
			replaceOnce(t, doc, `"dirty": false`, `"dirty": false, "manifest_digest": "sha256:00"`), "manifest_digest",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := api.DecodeRequest(tc.data)
			var verr *api.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Fatalf("want duplicate refusal on %q, got %v", tc.field, err)
			}
		})
	}
}

// The same key in sibling objects, and in successive array elements, is not a
// duplicate: the minimal fixture already repeats "id" across objects, and a
// second grant repeats every grant key.
func TestDecodeRequestAllowsSameKeyInSiblingObjects(t *testing.T) {
	doc := minimalRequest(t)
	second := `{"handle": "read-all", "kind": "file.read", "roots": ["."]},
    {"handle": "search-all", "kind": "file.search", "roots": ["."]}`
	data := replaceOnce(t, doc, `{"handle": "read-all", "kind": "file.read", "roots": ["."]}`, second)
	request, err := api.DecodeRequest(data)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if err := request.Validate(fixedNow); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(request.Grants) != 2 {
		t.Fatalf("want 2 grants, got %d", len(request.Grants))
	}
}

func TestDecodeRequestRefusesTrailingData(t *testing.T) {
	_, err := api.DecodeRequest([]byte(minimalRequest(t) + "{}"))
	var verr *api.ValidationError
	if !errors.As(err, &verr) || verr.Field != "$" {
		t.Fatalf("want trailing-data refusal on \"$\", got %v", err)
	}
}

func TestDecodeRefusesNonCanonicalKeys(t *testing.T) {
	for _, doc := range []string{
		`{"mode":"read_only","Mode":"read_write"}`,
		`{"OBJECTIVE":"x"}`,
	} {
		if _, err := api.DecodeRequest([]byte(doc)); err == nil {
			t.Fatalf("DecodeRequest(%s) accepted a non-canonical key", doc)
		}
	}
}

func TestContextItemRefIsValidated(t *testing.T) {
	it := api.ContextItem{
		ID: "a", Kind: api.ContextSourceCode, Trust: api.TrustWorkspace,
		Content: "x", ContentDigest: api.Digest([]byte("x")),
		Ref: &api.ArtifactRef{Digest: "nope", Size: -5},
	}
	if err := it.Validate(); err == nil {
		t.Fatal("a malformed ref was accepted")
	}
}
