package planning

// The operator-owned customization registry.
//
// An operator builds a reusable engineering agent by writing four kinds of file
// into a directory THEY control:
//
//	<dir>/instructions/<id>.json   InstructionPack   model-visible instructions
//	<dir>/context/<id>.json        ContextPolicy     which context a role gets
//	<dir>/profiles/<id>.json       AgentProfile      a specialized worker
//	<dir>/templates/<id>.json      EngineeringPlanTemplate  a reusable process
//
// The directory is operator authority in exactly the way the operator
// configuration file already is, and it is deliberately NOT inside any
// candidate workspace. That is the whole trust boundary of this file: a
// repository being worked on may supply engineering CONTEXT, and may never
// supply instruction, because nothing here ever reads from a candidate.
//
// It is also a separate directory from the configuration FILE on purpose. A
// run's identity is derived from the operator configuration digest, so putting
// instruction text inline in that file would re-identify every run in flight
// whenever an operator edited a sentence of prose.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Directory layout. Each subdirectory holds one kind of artifact, one file per
// id, named for the id it defines.
const (
	instructionsDir = "instructions"
	contextDir      = "context"
	profilesDir     = "profiles"
	templatesDir    = "templates"
)

// RegistryError is the typed refusal for a customization artifact that cannot
// be trusted as written. It names the file, because an operator who cannot
// locate a bad artifact has an outage rather than a diagnostic.
type RegistryError struct {
	Path   string
	Detail string
}

func (e *RegistryError) Error() string {
	if e.Path == "" {
		return "planning registry: " + e.Detail
	}
	return "planning registry: " + e.Path + ": " + e.Detail
}

// Registry is the resolved, validated customization set. It is immutable once
// built, and every lookup is by exact id.
type Registry struct {
	dir       string
	packs     map[string]domain.InstructionPack
	contexts  map[string]domain.ContextPolicy
	profiles  map[string]domain.AgentProfile
	templates map[string]domain.EngineeringPlanTemplate
}

// Dir is the operator directory this registry was loaded from.
func (r Registry) Dir() string { return r.dir }

// LoadRegistry reads every artifact under dir.
//
// An absent directory is an EMPTY registry and not an error: an operator who
// has defined no custom agents has a valid configuration, and #64 must not
// require one before a plan can be compiled at all.
//
// Each artifact is SEALED as it is loaded: the loader fills in the provenance
// it can prove - the operator file the artifact came from - and the content
// digest it computes, then validates the completed document against the schema.
// An artifact that states a digest of its own must match, so an operator who
// wants to pin content can, and one who does not is not made to hand-compute a
// hash.
func LoadRegistry(dir string) (Registry, error) {
	registry := Registry{
		dir:       dir,
		packs:     map[string]domain.InstructionPack{},
		contexts:  map[string]domain.ContextPolicy{},
		profiles:  map[string]domain.AgentProfile{},
		templates: map[string]domain.EngineeringPlanTemplate{},
	}
	if strings.TrimSpace(dir) == "" {
		return registry, nil
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return registry, nil
	case err != nil:
		return Registry{}, &RegistryError{Path: dir, Detail: err.Error()}
	case !info.IsDir():
		return Registry{}, &RegistryError{Path: dir, Detail: "the planning directory is not a directory"}
	}

	if err := loadEach(dir, instructionsDir, func(path, id string, data []byte) error {
		pack, err := sealPack(path, id, data)
		if err != nil {
			return err
		}
		registry.packs[id] = pack
		return nil
	}); err != nil {
		return Registry{}, err
	}
	if err := loadEach(dir, contextDir, func(path, id string, data []byte) error {
		policy, err := sealContextPolicy(path, id, data)
		if err != nil {
			return err
		}
		registry.contexts[id] = policy
		return nil
	}); err != nil {
		return Registry{}, err
	}
	if err := loadEach(dir, profilesDir, func(path, id string, data []byte) error {
		profile, err := sealProfile(path, id, data)
		if err != nil {
			return err
		}
		registry.profiles[id] = profile
		return nil
	}); err != nil {
		return Registry{}, err
	}
	if err := loadEach(dir, templatesDir, func(path, id string, data []byte) error {
		template, err := sealTemplate(path, id, data)
		if err != nil {
			return err
		}
		registry.templates[id] = template
		return nil
	}); err != nil {
		return Registry{}, err
	}
	if err := registry.validateReferences(); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

// loadEach reads every *.json file in one subdirectory in deterministic order.
// The file name IS the artifact id: an artifact whose document names a
// different id is refused rather than silently renamed, so `profiles/x.json`
// can never define the profile a plan means by `y`.
func loadEach(dir, kind string, load func(path, id string, data []byte) error) error {
	root := filepath.Join(dir, kind)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &RegistryError{Path: root, Detail: err.Error()}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return &RegistryError{Path: path, Detail: err.Error()}
		}
		if err := load(path, strings.TrimSuffix(name, ".json"), data); err != nil {
			return err
		}
	}
	return nil
}

// sealed is the shared sealing shape: decode leniently, fill provenance and
// digest, then validate the completed document against its schema.
func sealed[T domain.Contract](path, id string, data []byte, prepare func(*T) (string, string, error), digest func(T) (string, error), fill func(*T, string, domain.ArtifactSource)) (T, error) {
	var artifact T
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return artifact, &RegistryError{Path: path, Detail: err.Error()}
	}
	documentID, stated, err := prepare(&artifact)
	if err != nil {
		return artifact, &RegistryError{Path: path, Detail: err.Error()}
	}
	if documentID != "" && documentID != id {
		return artifact, &RegistryError{Path: path, Detail: fmt.Sprintf("declares id %q but its file names %q; the file name is the identity a plan refers to", documentID, id)}
	}
	source := domain.ArtifactSource{Type: domain.SourceOperatorFile, Location: path}
	fill(&artifact, "", source)
	computed, err := digest(artifact)
	if err != nil {
		return artifact, &RegistryError{Path: path, Detail: err.Error()}
	}
	if stated != "" && stated != computed {
		return artifact, &RegistryError{Path: path, Detail: fmt.Sprintf("states digest %s but its content digests to %s", short(stated), short(computed))}
	}
	fill(&artifact, computed, source)
	if _, err := domain.Encode(artifact); err != nil {
		return artifact, &RegistryError{Path: path, Detail: err.Error()}
	}
	return artifact, nil
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func sealPack(path, id string, data []byte) (domain.InstructionPack, error) {
	return sealed(path, id, data,
		func(pack *domain.InstructionPack) (string, string, error) {
			pack.SchemaVersion = domain.SchemaVersion
			if pack.Revision == "" {
				pack.Revision = "1"
			}
			stated := pack.Digest
			documentID := pack.ID
			pack.ID = id
			return documentID, stated, nil
		},
		func(pack domain.InstructionPack) (string, error) { return pack.ContentDigest() },
		func(pack *domain.InstructionPack, digest string, source domain.ArtifactSource) {
			pack.Digest, pack.Source = digest, source
		})
}

func sealContextPolicy(path, id string, data []byte) (domain.ContextPolicy, error) {
	policy, err := sealed(path, id, data,
		func(policy *domain.ContextPolicy) (string, string, error) {
			policy.SchemaVersion = domain.SchemaVersion
			if policy.Revision == "" {
				policy.Revision = "1"
			}
			stated := policy.Digest
			documentID := policy.ID
			policy.ID = id
			return documentID, stated, nil
		},
		func(policy domain.ContextPolicy) (string, error) { return policy.ContentDigest() },
		func(policy *domain.ContextPolicy, digest string, source domain.ArtifactSource) {
			policy.Digest, policy.Source = digest, source
		})
	if err != nil {
		return policy, err
	}
	// A ContextPolicy narrows. It can never remove the governance envelope: a
	// worker that cannot see its obligations is a worker nothing can hold to
	// them, and a policy that could erase them would be granting itself
	// freedom from the contract.
	if err := refuseErasureOfRequiredContext(policy); err != nil {
		return policy, &RegistryError{Path: path, Detail: err.Error()}
	}
	return policy, nil
}

func refuseErasureOfRequiredContext(policy domain.ContextPolicy) error {
	required := map[domain.ContextClass]bool{}
	for _, class := range domain.RequiredContextClasses() {
		required[class] = true
	}
	for _, class := range policy.Exclude {
		if required[class] {
			return fmt.Errorf("excludes required context class %q: a ContextPolicy may narrow optional context and may never remove the governance envelope", class)
		}
	}
	if len(policy.Include) == 0 {
		return nil
	}
	included := map[domain.ContextClass]bool{}
	for _, class := range policy.Include {
		included[class] = true
	}
	for _, class := range domain.RequiredContextClasses() {
		if !included[class] {
			return fmt.Errorf("includes a set that omits required context class %q: an inclusion list narrows the OPTIONAL classes, never the required ones", class)
		}
	}
	// The producer's own reasoning transcript is never inheritable. Naming it
	// in an inclusion list is an attempt to hand one worker another's hidden
	// reasoning, which is what independent review exists to prevent.
	if included[domain.ContextProducerReasoning] {
		return fmt.Errorf("includes %q: a producer's hidden reasoning transcript is never delivered to another worker", domain.ContextProducerReasoning)
	}
	return nil
}

func sealProfile(path, id string, data []byte) (domain.AgentProfile, error) {
	return sealed(path, id, data,
		func(profile *domain.AgentProfile) (string, string, error) {
			profile.SchemaVersion = domain.SchemaVersion
			if profile.Version == 0 {
				profile.Version = 1
			}
			if profile.TrustRequirement == "" {
				// An unstated trust requirement is the WEAKER one. Defaulting
				// to protected would let a profile demand an isolation its
				// worker cannot prove merely by staying silent.
				profile.TrustRequirement = domain.TrustRequirementOperatorTrusted
			}
			stated := profile.Digest
			documentID := profile.ID
			profile.ID = id
			return documentID, stated, nil
		},
		func(profile domain.AgentProfile) (string, error) { return profile.ContentDigest() },
		func(profile *domain.AgentProfile, digest string, source domain.ArtifactSource) {
			profile.Digest, profile.Source = digest, source
		})
}

func sealTemplate(path, id string, data []byte) (domain.EngineeringPlanTemplate, error) {
	template, err := sealed(path, id, data,
		func(template *domain.EngineeringPlanTemplate) (string, string, error) {
			template.SchemaVersion = domain.SchemaVersion
			if template.Version == 0 {
				template.Version = 1
			}
			stated := template.Digest
			documentID := template.ID
			template.ID = id
			return documentID, stated, nil
		},
		func(template domain.EngineeringPlanTemplate) (string, error) { return template.ContentDigest() },
		func(template *domain.EngineeringPlanTemplate, digest string, source domain.ArtifactSource) {
			template.Digest, template.Source = digest, source
		})
	if err != nil {
		return template, err
	}
	if err := validateTemplateGraph(template); err != nil {
		return template, &RegistryError{Path: path, Detail: err.Error()}
	}
	return template, nil
}

// validateReferences refuses a registry whose artifacts point at each other's
// absences. A profile naming a pack nobody installed would otherwise fail at
// the moment work started, which is the most expensive moment to discover it.
func (r Registry) validateReferences() error {
	for _, id := range r.ProfileIDs() {
		profile := r.profiles[id]
		for _, pack := range profile.Instructions {
			if _, ok := r.packs[pack]; !ok {
				return &RegistryError{Path: r.pathFor(profilesDir, id), Detail: fmt.Sprintf("names instruction pack %q, which is not installed", pack)}
			}
		}
		if profile.ContextPolicy != "" {
			if _, ok := r.contexts[profile.ContextPolicy]; !ok {
				return &RegistryError{Path: r.pathFor(profilesDir, id), Detail: fmt.Sprintf("names context policy %q, which is not installed", profile.ContextPolicy)}
			}
		}
	}
	for _, id := range r.TemplateIDs() {
		template := r.templates[id]
		for _, stage := range template.Stages {
			if stage.Profile != "" {
				if _, ok := r.profiles[stage.Profile]; !ok {
					return &RegistryError{Path: r.pathFor(templatesDir, id), Detail: fmt.Sprintf("stage %q prefers profile %q, which is not installed", stage.ID, stage.Profile)}
				}
			}
			if stage.ContextPolicy != "" {
				if _, ok := r.contexts[stage.ContextPolicy]; !ok {
					return &RegistryError{Path: r.pathFor(templatesDir, id), Detail: fmt.Sprintf("stage %q names context policy %q, which is not installed", stage.ID, stage.ContextPolicy)}
				}
			}
		}
	}
	return nil
}

func (r Registry) pathFor(kind, id string) string {
	return filepath.Join(r.dir, kind, id+".json")
}

// ---------------------------------------------------------------------------
// Lookups
// ---------------------------------------------------------------------------

// ProfileIDs, TemplateIDs, PackIDs and ContextPolicyIDs list installed
// artifacts in deterministic order.
func (r Registry) ProfileIDs() []string       { return sortedKeys(r.profiles) }
func (r Registry) TemplateIDs() []string      { return sortedKeys(r.templates) }
func (r Registry) PackIDs() []string          { return sortedKeys(r.packs) }
func (r Registry) ContextPolicyIDs() []string { return sortedKeys(r.contexts) }

// Profile resolves one installed profile.
func (r Registry) Profile(id string) (domain.AgentProfile, error) {
	profile, ok := r.profiles[id]
	if !ok {
		return domain.AgentProfile{}, &RegistryError{Detail: fmt.Sprintf("profile %q is not installed; installed profiles are %s", id, list(r.ProfileIDs()))}
	}
	return profile, nil
}

// Profiles lists every installed profile in deterministic order.
func (r Registry) Profiles() []domain.AgentProfile {
	profiles := make([]domain.AgentProfile, 0, len(r.profiles))
	for _, id := range r.ProfileIDs() {
		profiles = append(profiles, r.profiles[id])
	}
	return profiles
}

// Template resolves one installed template.
func (r Registry) Template(id string) (domain.EngineeringPlanTemplate, error) {
	template, ok := r.templates[id]
	if !ok {
		return domain.EngineeringPlanTemplate{}, &RegistryError{Detail: fmt.Sprintf("plan template %q is not installed; installed templates are %s", id, list(r.TemplateIDs()))}
	}
	return template, nil
}

// Pack resolves one installed instruction pack.
func (r Registry) Pack(id string) (domain.InstructionPack, error) {
	pack, ok := r.packs[id]
	if !ok {
		return domain.InstructionPack{}, &RegistryError{Detail: fmt.Sprintf("instruction pack %q is not installed", id)}
	}
	return pack, nil
}

// ContextPolicy resolves one installed context policy.
func (r Registry) ContextPolicy(id string) (domain.ContextPolicy, error) {
	policy, ok := r.contexts[id]
	if !ok {
		return domain.ContextPolicy{}, &RegistryError{Detail: fmt.Sprintf("context policy %q is not installed", id)}
	}
	return policy, nil
}

// Instructions is the model-visible instruction text one profile contributes,
// in the order the profile names its packs. It is the ONLY path by which
// operator instruction reaches a worker.
func (r Registry) Instructions(profile domain.AgentProfile) ([]string, error) {
	var instructions []string
	for _, id := range profile.Instructions {
		pack, err := r.Pack(id)
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, pack.Instructions...)
	}
	return instructions, nil
}

// Binding freezes the exact identities that will have performed the work: the
// profile version and digest, every instruction pack digest, and the context
// policy identity. Editing any of them afterwards produces a different digest
// and therefore cannot rewrite an assignment that already exists.
func (r Registry) Binding(profile domain.AgentProfile) (domain.ProfileBinding, error) {
	binding := domain.ProfileBinding{
		ID: profile.ID, Version: profile.Version, Digest: profile.Digest,
		Capabilities: profile.Capabilities, TrustRequirement: profile.TrustRequirement,
	}
	for _, id := range profile.Instructions {
		pack, err := r.Pack(id)
		if err != nil {
			return domain.ProfileBinding{}, err
		}
		binding.Instructions = append(binding.Instructions, domain.PackRef{ID: pack.ID, Revision: pack.Revision, Digest: pack.Digest})
	}
	if profile.ContextPolicy != "" {
		policy, err := r.ContextPolicy(profile.ContextPolicy)
		if err != nil {
			return domain.ProfileBinding{}, err
		}
		binding.ContextPolicy = &domain.PackRef{ID: policy.ID, Revision: policy.Revision, Digest: policy.Digest}
	}
	return binding, nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func list(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ", ")
}
