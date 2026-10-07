package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

type schemaLocation struct{ file, pointer string }

// structSchemas maps every Go struct reachable from the three contract roots
// to the schema object that mirrors it.
var structSchemas = map[reflect.Type]schemaLocation{
	reflect.TypeFor[api.ExecutionRequest]():   {requestFile, rootPointer},
	reflect.TypeFor[api.WorkspaceRef]():       {requestFile, "/$defs/workspace"},
	reflect.TypeFor[api.Constraints]():        {requestFile, "/$defs/constraints"},
	reflect.TypeFor[api.ContextItem]():        {requestFile, "/$defs/context_item"},
	reflect.TypeFor[api.Capability]():         {requestFile, "/$defs/capability"},
	reflect.TypeFor[api.CommandGrant]():       {requestFile, "/$defs/command_grant"},
	reflect.TypeFor[api.Budget]():             {requestFile, "/$defs/budget"},
	reflect.TypeFor[api.MoneyCeiling]():       {requestFile, "/$defs/money_ceiling"},
	reflect.TypeFor[api.ProviderBinding]():    {requestFile, "/$defs/provider_binding"},
	reflect.TypeFor[api.Pricing]():            {requestFile, "/$defs/pricing"},
	reflect.TypeFor[api.ArtifactRef]():        {commonFile, "/$defs/artifact_ref"},
	reflect.TypeFor[api.TokenUsage]():         {commonFile, "/$defs/token_usage"},
	reflect.TypeFor[api.ExecutionResult]():    {resultFile, rootPointer},
	reflect.TypeFor[api.Termination]():        {resultFile, "/$defs/termination"},
	reflect.TypeFor[api.Observation]():        {resultFile, "/$defs/observation"},
	reflect.TypeFor[api.Usage]():              {resultFile, "/$defs/usage"},
	reflect.TypeFor[api.Cost]():               {resultFile, "/$defs/cost"},
	reflect.TypeFor[api.ContextManifest]():    {resultFile, "/$defs/context_manifest"},
	reflect.TypeFor[api.ManifestEntry]():      {resultFile, "/$defs/manifest_entry"},
	reflect.TypeFor[api.Capacity]():           {resultFile, "/$defs/capacity"},
	reflect.TypeFor[api.TokenEstimate]():      {resultFile, "/$defs/token_estimate"},
	reflect.TypeFor[api.RoutingDecision]():    {resultFile, "/$defs/routing_decision"},
	reflect.TypeFor[api.RoutingCandidate]():   {resultFile, "/$defs/routing_candidate"},
	reflect.TypeFor[api.Provenance]():         {resultFile, "/$defs/provenance"},
	reflect.TypeFor[api.SessionObservation](): {resultFile, "/$defs/session_observation"},
	reflect.TypeFor[api.Event]():              {eventFile, rootPointer},
}

// enumSchemas maps every typed string constant set in api to its schema enum.
// Types listed in enumsOutsideSchemas are not part of the JSON contracts.
var enumSchemas = map[string]schemaLocation{
	"Mode":                   {requestFile, "/$defs/mode"},
	"CapabilityKind":         {requestFile, "/$defs/capability_kind"},
	"Isolation":              {commonFile, "/$defs/isolation"},
	"ContextKind":            {commonFile, "/$defs/context_kind"},
	"Trust":                  {commonFile, "/$defs/trust"},
	"EventKind":              {commonFile, "/$defs/event_kind"},
	"Outcome":                {resultFile, "/$defs/outcome"},
	"Cause":                  {resultFile, "/$defs/cause"},
	"BudgetDimension":        {resultFile, "/$defs/budget_dimension"},
	"CancellationProvenance": {resultFile, "/$defs/cancellation_provenance"},
}

var enumsOutsideSchemas = []string{"Role", "StopReason", "ProviderErrorClass", "ToolStatus"}

var timeType = reflect.TypeFor[time.Time]()

type jsonField struct {
	name     string
	required bool
	typ      reflect.Type
}

func jsonFields(t *testing.T, typ reflect.Type) []jsonField {
	t.Helper()
	var fields []jsonField
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no explicit json name", typ.Name(), f.Name)
		}
		fields = append(fields, jsonField{name: name, required: opts != "omitempty", typ: f.Type})
	}
	return fields
}

// structTarget returns the struct a field ultimately holds, if any.
func structTarget(typ reflect.Type) (reflect.Type, bool) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	return typ, typ.Kind() == reflect.Struct && typ != timeType
}

func stringSet(t *testing.T, value any) []string {
	t.Helper()
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string entry %v", v)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestSchemaPropertiesMirrorGoJSONFields(t *testing.T) {
	schemas := loadSchemas(t)
	for typ, loc := range structSchemas {
		t.Run(typ.Name(), func(t *testing.T) {
			node := schemas.node(t, loc.file, loc.pointer)
			if node["additionalProperties"] != false {
				t.Errorf("additionalProperties must be false")
			}
			props, _ := node["properties"].(map[string]any)
			var names, required []string
			for _, f := range jsonFields(t, typ) {
				names = append(names, f.name)
				if f.required {
					required = append(required, f.name)
				}
				checkNestedRef(t, schemas, loc.file, f, props[f.name])
			}
			slices.Sort(names)
			slices.Sort(required)
			if got := slices.Sorted(maps.Keys(props)); !slices.Equal(got, names) {
				t.Errorf("properties %v, Go json fields %v", got, names)
			}
			if got := stringSet(t, node["required"]); !slices.Equal(got, required) {
				t.Errorf("required %v, Go non-omitempty fields %v", got, required)
			}
		})
	}
}

// checkNestedRef asserts a struct-valued field points at the schema mapped to
// that struct, so a property cannot be wired to the wrong definition.
func checkNestedRef(t *testing.T, schemas schemaSet, file string, f jsonField, prop any) {
	t.Helper()
	target, ok := structTarget(f.typ)
	if !ok {
		return
	}
	schema, _ := prop.(map[string]any)
	if f.typ.Kind() == reflect.Slice {
		schema, _ = schema["items"].(map[string]any)
	}
	ref, _ := schema["$ref"].(string)
	if ref == "" {
		t.Errorf("%s: struct field has no $ref", f.name)
		return
	}
	gotFile, gotPointer := schemas.resolve(t, file, ref)
	want, mapped := structSchemas[target]
	if !mapped {
		t.Errorf("%s: Go type %s has no schema mapping", f.name, target.Name())
		return
	}
	if gotFile != want.file || gotPointer != want.pointer {
		t.Errorf("%s: $ref %q, want %s#%s", f.name, ref, want.file, want.pointer)
	}
}

func TestEverySchemaStructIsMapped(t *testing.T) {
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		target, ok := structTarget(typ)
		if !ok {
			return
		}
		if _, mapped := structSchemas[target]; !mapped {
			t.Errorf("struct %s is reachable from a contract root but has no schema mapping", target.Name())
			return
		}
		for _, f := range jsonFields(t, target) {
			walk(f.typ)
		}
	}
	for _, root := range []reflect.Type{
		reflect.TypeFor[api.ExecutionRequest](), reflect.TypeFor[api.ExecutionResult](), reflect.TypeFor[api.Event](),
	} {
		walk(root)
	}
}

// goStringConstants parses the non-test api sources and returns every typed
// string constant grouped by type name, so a new constant cannot be missed.
func goStringConstants(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	consts := map[string][]string{}
	for _, path := range glob(t, "*.go") {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(path), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				collectTypedString(t, spec.(*ast.ValueSpec), consts)
			}
		}
	}
	return consts
}

func collectTypedString(t *testing.T, spec *ast.ValueSpec, consts map[string][]string) {
	t.Helper()
	ident, ok := spec.Type.(*ast.Ident)
	if !ok {
		return
	}
	for _, value := range spec.Values {
		lit, ok := value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquote %s: %v", lit.Value, err)
		}
		consts[ident.Name] = append(consts[ident.Name], s)
	}
}

func TestSchemaEnumsEqualGoConstantSets(t *testing.T) {
	schemas := loadSchemas(t)
	for typeName, values := range goStringConstants(t) {
		if slices.Contains(enumsOutsideSchemas, typeName) {
			continue
		}
		loc, ok := enumSchemas[typeName]
		if !ok {
			t.Errorf("typed constants of %s have no schema enum mapping", typeName)
			continue
		}
		slices.Sort(values)
		if got := stringSet(t, schemas.node(t, loc.file, loc.pointer)["enum"]); !slices.Equal(got, values) {
			t.Errorf("%s: schema enum %v, Go constants %v", typeName, got, values)
		}
	}
	constraints := schemas.node(t, requestFile, "/$defs/constraints")
	features := constraints["properties"].(map[string]any)["required_features"].(map[string]any)
	want := slices.Sorted(slices.Values(api.SupportedFeatures))
	if got := stringSet(t, features["items"].(map[string]any)["enum"]); !slices.Equal(got, want) {
		t.Errorf("required_features enum %v, api.SupportedFeatures %v", got, want)
	}
}

func TestSchemaVersionConstMatchesGo(t *testing.T) {
	if got := loadSchemas(t).node(t, commonFile, "/$defs/version")["const"]; got != api.ExecutionVersion {
		t.Fatalf("schema version %v, api.ExecutionVersion %s", got, api.ExecutionVersion)
	}
}
