package intelligence

// Method names how a relation was established.
type Method string

const (
	// MethodSyntax: read directly from the parsed file (declarations, imports).
	MethodSyntax Method = "ast-syntax"
	// MethodTypes: resolved by go/types over the indexed packages only.
	MethodTypes Method = "go-types"
	// MethodNameHeuristic: TestXxx associated with Xxx by name.
	MethodNameHeuristic Method = "heuristic-name"
	// MethodTestCall: a test function statically calls the symbol.
	MethodTestCall Method = "test-static-call"
)

// Confidence states whether a relation is a fact or a guess. Inferred
// relations are never labelled deterministic.
type Confidence string

const (
	ConfidenceDeterministic Confidence = "deterministic"
	ConfidenceInferred      Confidence = "inferred"
	ConfidenceUnresolved    Confidence = "unresolved"
)

// Evidence is the provenance every relation carries.
type Evidence struct {
	File       string     `json:"file"`
	Line       int        `json:"line"`
	Method     Method     `json:"method"`
	Confidence Confidence `json:"confidence"`
}

// Module is one go.mod in the manifest.
type Module struct {
	Path        string `json:"path"`
	Dir         string `json:"dir"`
	GoModDigest string `json:"go_mod_digest"`
	GoDirective string `json:"go_directive,omitempty"`
}

// Package is one type-checked package. An external test package
// (package foo_test) is its own Package with Test set.
type Package struct {
	ImportPath string   `json:"import_path"`
	Dir        string   `json:"dir"`
	Name       string   `json:"name"`
	Module     string   `json:"module,omitempty"`
	Test       bool     `json:"test,omitempty"`
	Files      []string `json:"files"`
}

// File is one Go file and whether the current settings include it.
type File struct {
	Path     string `json:"path"`
	Digest   string `json:"digest"`
	Package  string `json:"package,omitempty"`
	Test     bool   `json:"test,omitempty"`
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
}

// Symbol kinds.
const (
	KindFunc   = "func"
	KindMethod = "method"
	KindType   = "type"
	KindConst  = "const"
	KindVar    = "var"
)

// Symbol is a package-level declaration. ID is "importpath.Name" or
// "importpath.Recv.Name"; init functions are "importpath.init@file:line".
type Symbol struct {
	ID       string   `json:"id"`
	Package  string   `json:"package"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Receiver string   `json:"receiver,omitempty"`
	Exported bool     `json:"exported"`
	Evidence Evidence `json:"evidence"`
}

// Import statuses.
const (
	ImportIndexed            = "indexed"
	ImportModuleNotIndexed   = "module_not_indexed"
	ImportStdlibNotIndexed   = "stdlib_not_indexed"
	ImportExternalNotIndexed = "external_not_indexed"
	ImportCgo                = "cgo"
)

// Import is one import spec; Package is the importer.
type Import struct {
	Package  string   `json:"package"`
	Path     string   `json:"path"`
	Status   string   `json:"status"`
	Evidence Evidence `json:"evidence"`
}

// Reference is a resolved use of a package-level symbol (or method) of an
// indexed package. One record per (From, To); Evidence names the first use.
type Reference struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Evidence Evidence `json:"evidence"`
}

// CallClass separates proven call edges from everything else.
type CallClass string

const (
	// CallStatic: a direct call to a function or concrete method of an
	// indexed package, resolved by go/types.
	CallStatic CallClass = "static"
	// CallPossible: an interface (or type-parameter) method call; Callee is
	// the abstract method and the concrete target is chosen at run time.
	CallPossible CallClass = "possible"
	// CallDynamic: a call through a function value, field, computed
	// expression or reflection.
	CallDynamic CallClass = "dynamic"
	// CallUnresolved: the callee could not be resolved, usually because it is
	// in a package that is not indexed.
	CallUnresolved CallClass = "unresolved"
)

// Call is one call relation, deduplicated per (Caller, Callee, Class).
type Call struct {
	Caller   string    `json:"caller"`
	Callee   string    `json:"callee"`
	Class    CallClass `json:"class"`
	Reason   string    `json:"reason,omitempty"`
	Evidence Evidence  `json:"evidence"`
}

// CoverageUnknown is the limit every test association carries.
const CoverageUnknown = "coverage unknown: association is a name/call heuristic, not execution evidence; " +
	"suggested tests never replace mandatory assurance"

// TestAssociation links a test function to a symbol it probably exercises.
type TestAssociation struct {
	Test     string   `json:"test"`
	Target   string   `json:"target"`
	Limit    string   `json:"limit"`
	Evidence Evidence `json:"evidence"`
}

// Incomplete-analysis kinds.
const (
	IncompleteParseError    = "parse_error"
	IncompleteBuildExcluded = "build_excluded"
	IncompleteCgo           = "cgo"
	IncompleteTypeErrors    = "type_errors"
	IncompleteNoModule      = "no_module"
	IncompleteBadGoMod      = "bad_go_mod"
	IncompleteImportCycle   = "import_cycle"
	IncompletePackageName   = "package_name_mismatch"
)

// Incomplete records analysis that did not happen or did not finish. Its
// presence is why a missing edge is never proof of no dependency.
type Incomplete struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

// DirFacts are the facts extracted from one package directory: the unit an
// overlay either reuses whole or re-extracts.
type DirFacts struct {
	Dir        string       `json:"dir"`
	Files      []File       `json:"files"`
	Packages   []Package    `json:"packages"`
	Symbols    []Symbol     `json:"symbols"`
	Imports    []Import     `json:"imports"`
	References []Reference  `json:"references"`
	Calls      []Call       `json:"calls"`
	Incomplete []Incomplete `json:"incomplete,omitempty"`
}

// OverlayBinding is what an execution-scoped overlay was derived from.
type OverlayBinding struct {
	BaseKey string `json:"base_key"`
	// Dirty maps each changed or added path to its content digest.
	Dirty   map[string]string `json:"dirty,omitempty"`
	Deleted []string          `json:"deleted,omitempty"`
	// Renamed maps new path to old path.
	Renamed map[string]string `json:"renamed,omitempty"`
	// Ignored lists changed paths outside the snapshot scope.
	Ignored  []string `json:"ignored,omitempty"`
	Settings Settings `json:"settings"`
	// Invalidation is "files" (changed packages and their importers),
	// "module" (a go.mod changed) or "all" (settings changed).
	Invalidation string `json:"invalidation"`
}

// Snapshot is the complete, serialisable content of an index.
type Snapshot struct {
	Identity   Identity          `json:"identity"`
	Manifest   Manifest          `json:"manifest"`
	Modules    []Module          `json:"modules"`
	Dirs       []DirFacts        `json:"dirs"`
	Tests      []TestAssociation `json:"tests"`
	Incomplete []Incomplete      `json:"incomplete,omitempty"`
	Overlay    *OverlayBinding   `json:"overlay,omitempty"`
}
