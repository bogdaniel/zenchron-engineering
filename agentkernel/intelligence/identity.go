package intelligence

import (
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"maps"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strconv"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// SchemaVersion versions the persisted snapshot shape.
const SchemaVersion = "agentkernel.intelligence/v1"

// ExtractorVersion versions the extraction rules. Bump it whenever a rule
// change would produce different facts from identical input, so stale caches
// miss instead of serving facts the current extractor would not produce.
const ExtractorVersion = "go-structural/1"

var (
	goVersionPattern = regexp.MustCompile(`^go1\.([0-9]+)(\.[0-9]+|rc[0-9]+|beta[0-9]+)?$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)
)

// Settings are the extraction settings that decide which files are analysed.
// They are always supplied by the caller; nothing is read from the host
// environment, so two hosts with different GOOS or GOFLAGS cannot silently
// produce different snapshots under one identity.
type Settings struct {
	// GoVersion is the target toolchain, e.g. "go1.25.3". It selects the
	// goN.M release tags that build constraints are evaluated against.
	GoVersion  string   `json:"go_version"`
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	BuildTags  []string `json:"build_tags,omitempty"`
	CgoEnabled bool     `json:"cgo_enabled"`
}

func (s Settings) canonical() (Settings, error) {
	if !goVersionPattern.MatchString(s.GoVersion) {
		return Settings{}, fmt.Errorf("intelligence: go_version %q is not a toolchain version like go1.25.3", s.GoVersion)
	}
	if !tagPattern.MatchString(s.GOOS) || !tagPattern.MatchString(s.GOARCH) {
		return Settings{}, fmt.Errorf("intelligence: goos %q and goarch %q must be non-empty tags", s.GOOS, s.GOARCH)
	}
	tags := slices.Clone(s.BuildTags)
	slices.Sort(tags)
	tags = slices.Compact(tags)
	for _, t := range tags {
		if !tagPattern.MatchString(t) {
			return Settings{}, fmt.Errorf("intelligence: invalid build tag %q", t)
		}
	}
	if len(tags) == 0 {
		tags = nil
	}
	s.BuildTags = tags
	return s, nil
}

func (s Settings) equal(o Settings) bool {
	return s.GoVersion == o.GoVersion && s.GOOS == o.GOOS && s.GOARCH == o.GOARCH &&
		s.CgoEnabled == o.CgoEnabled && slices.Equal(s.BuildTags, o.BuildTags)
}

// buildContext evaluates build constraints against workspace-relative paths
// through open, never against the host GOPATH or environment.
func (s Settings) buildContext(open func(string) (io.ReadCloser, error)) *build.Context {
	minor, _ := strconv.Atoi(goVersionPattern.FindStringSubmatch(s.GoVersion)[1]) // validated by canonical
	release := make([]string, 0, minor)
	for i := 1; i <= minor; i++ {
		release = append(release, fmt.Sprintf("go1.%d", i))
	}
	return &build.Context{
		GOOS:        s.GOOS,
		GOARCH:      s.GOARCH,
		CgoEnabled:  s.CgoEnabled,
		BuildTags:   s.BuildTags,
		ReleaseTags: release,
		Compiler:    "gc",
		JoinPath:    path.Join,
		OpenFile:    open,
	}
}

// Identity is everything a snapshot's facts depend on. Two snapshots with
// equal keys are interchangeable; any difference is a cache miss.
type Identity struct {
	Schema    string `json:"schema"`
	Extractor string `json:"extractor"`
	// ExtractorRuntime is the Go runtime whose go/parser and go/types produced
	// the facts. It is recorded explicitly because the analysis library, not
	// only the target toolchain, can change results.
	ExtractorRuntime string   `json:"extractor_runtime"`
	ManifestDigest   string   `json:"manifest_digest"`
	Settings         Settings `json:"settings"`
	// GoMods maps each go.mod path in the manifest to its content digest.
	GoMods map[string]string `json:"go_mods,omitempty"`
	Scope  Scope             `json:"scope"`
}

func newIdentity(m Manifest, settings Settings, scope Scope, modules []Module) Identity {
	mods := map[string]string{}
	for _, mod := range modules {
		mods[path.Join(mod.Dir, "go.mod")] = mod.GoModDigest
	}
	if len(mods) == 0 {
		mods = nil
	}
	return Identity{
		Schema:           SchemaVersion,
		Extractor:        ExtractorVersion,
		ExtractorRuntime: runtime.Version(),
		ManifestDigest:   m.Digest(),
		Settings:         settings,
		GoMods:           mods,
		Scope:            scope,
	}
}

// Key is the canonical digest of the identity. encoding/json sorts map keys
// and every slice here is canonicalised, so equal identities have equal keys.
func (id Identity) Key() string {
	b, err := json.Marshal(id)
	if err != nil {
		panic("intelligence: identity is plain data and always encodes: " + err.Error())
	}
	return api.Digest(b)
}

func (id Identity) clone() Identity {
	id.Settings.BuildTags = slices.Clone(id.Settings.BuildTags)
	id.GoMods = maps.Clone(id.GoMods)
	id.Scope = id.Scope.clone()
	return id
}
