package planning

// Governed authoring (ADR-0008): a second author of the exact same sealed
// JSON files LoadRegistry reads. No new store, table or journal - an
// artifact written here is validated with the identical seal function a
// hand-edited file is validated with on load, then written where
// LoadRegistry would find it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// idPattern is the filename-safety trust boundary: an id becomes
// <dir>/<kind>/<id>.json, so it may never carry a path separator or a
// traversal segment.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

func validateID(id string) error {
	if !idPattern.MatchString(id) {
		return &RegistryError{Detail: fmt.Sprintf("id %q is not a valid artifact filename", id)}
	}
	return nil
}

// writeArtifact validates data with the same seal function a hand-edited file
// uses, then atomically writes the encoded, sealed document to its path. A
// document that the loader would refuse is refused here too, before anything
// reaches disk.
//
// validate runs AFTER seal succeeds and BEFORE anything is written: it is how
// a profile or template's cross-references are checked against what is
// currently installed, so a dangling reference is refused at the mistake
// itself rather than discovered later, when LoadRegistry.validateReferences
// refuses the WHOLE registry - every other artifact's availability, not just
// this one. nil means the kind has no outgoing references to check.
func writeArtifact[T domain.Contract](dir, kind, id string, data []byte, seal func(path, id string, data []byte) (T, error), validate func(T) error) (T, error) {
	var zero T
	// LoadRegistry treats an empty dir as "no planning_dir configured, empty
	// registry" - a valid READ for an operator with no custom agents. A
	// WRITE has no such safe empty case: dir becomes the root every path in
	// this call joins against, and an empty or relative one would resolve
	// against the server process's current working directory instead of
	// the operator-controlled location #398/ADR-0003 require.
	if !filepath.IsAbs(dir) {
		return zero, &RegistryError{Detail: "planning_dir must be configured as an absolute, operator-controlled directory before an artifact can be written"}
	}
	if err := validateID(id); err != nil {
		return zero, err
	}
	if int64(len(data)) > maxPlanningArtifactBytes {
		return zero, &RegistryError{Path: id, Detail: "artifact exceeds the operator artifact size bound"}
	}
	root := filepath.Join(dir, kind)
	path := filepath.Join(root, id+".json")
	sealed, err := seal(path, id, data)
	if err != nil {
		return zero, err
	}
	if validate != nil {
		if err := validate(sealed); err != nil {
			return zero, err
		}
	}
	encoded, err := domain.Encode(sealed)
	if err != nil {
		return zero, &RegistryError{Path: path, Detail: err.Error()}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return zero, &RegistryError{Path: root, Detail: err.Error()}
	}
	if err := atomicWriteFile(path, encoded); err != nil {
		return zero, &RegistryError{Path: path, Detail: err.Error()}
	}
	return sealed, nil
}

// validateProfileReferences refuses an AgentProfile naming an instruction
// pack or context policy that is not currently installed. It reads the
// registry fresh rather than taking one as a parameter: a write is rare
// compared to a read, and threading a live Registry through every caller of
// WriteAgentProfile would be a wider change than this correctness fix needs.
func validateProfileReferences(dir string, profile domain.AgentProfile) error {
	if len(profile.Instructions) == 0 && profile.ContextPolicy == "" {
		return nil
	}
	registry, err := LoadRegistry(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, profilesDir, profile.ID+".json")
	for _, pack := range profile.Instructions {
		if _, err := registry.Pack(pack); err != nil {
			return &RegistryError{Path: path, Detail: fmt.Sprintf("names instruction pack %q, which is not installed", pack)}
		}
	}
	if profile.ContextPolicy != "" {
		if _, err := registry.ContextPolicy(profile.ContextPolicy); err != nil {
			return &RegistryError{Path: path, Detail: fmt.Sprintf("names context policy %q, which is not installed", profile.ContextPolicy)}
		}
	}
	return nil
}

// validateTemplateReferences refuses an EngineeringPlanTemplate whose stage
// prefers a profile or names a context policy that is not currently
// installed, for the same reason validateProfileReferences does.
func validateTemplateReferences(dir string, template domain.EngineeringPlanTemplate) error {
	needsCheck := false
	for _, stage := range template.Stages {
		if stage.Profile != "" || stage.ContextPolicy != "" {
			needsCheck = true
			break
		}
	}
	if !needsCheck {
		return nil
	}
	registry, err := LoadRegistry(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, templatesDir, template.ID+".json")
	for _, stage := range template.Stages {
		if stage.Profile != "" {
			if _, err := registry.Profile(stage.Profile); err != nil {
				return &RegistryError{Path: path, Detail: fmt.Sprintf("stage %q prefers profile %q, which is not installed", stage.ID, stage.Profile)}
			}
		}
		if stage.ContextPolicy != "" {
			if _, err := registry.ContextPolicy(stage.ContextPolicy); err != nil {
				return &RegistryError{Path: path, Detail: fmt.Sprintf("stage %q names context policy %q, which is not installed", stage.ID, stage.ContextPolicy)}
			}
		}
	}
	return nil
}

// atomicWriteFile writes data to path by creating a sibling temp file in the
// same directory (same filesystem, so the rename below is atomic) and
// renaming it into place. A reader never observes a partially written file.
func atomicWriteFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// WriteInstructionPack validates and durably writes one InstructionPack. It
// names no other artifact, so there is nothing to cross-reference.
func WriteInstructionPack(dir, id string, data []byte) (domain.InstructionPack, error) {
	return writeArtifact(dir, instructionsDir, id, data, sealPack, nil)
}

// WriteContextPolicy validates and durably writes one ContextPolicy. It
// names no other artifact, so there is nothing to cross-reference.
func WriteContextPolicy(dir, id string, data []byte) (domain.ContextPolicy, error) {
	return writeArtifact(dir, contextDir, id, data, sealContextPolicy, nil)
}

// WriteAgentProfile validates and durably writes one AgentProfile, refusing
// one that names an instruction pack or context policy not currently
// installed.
func WriteAgentProfile(dir, id string, data []byte) (domain.AgentProfile, error) {
	return writeArtifact(dir, profilesDir, id, data, sealProfile, func(p domain.AgentProfile) error {
		return validateProfileReferences(dir, p)
	})
}

// WriteEngineeringPlanTemplate validates and durably writes one
// EngineeringPlanTemplate, refusing one whose stage prefers a profile or
// names a context policy not currently installed.
func WriteEngineeringPlanTemplate(dir, id string, data []byte) (domain.EngineeringPlanTemplate, error) {
	return writeArtifact(dir, templatesDir, id, data, sealTemplate, func(tpl domain.EngineeringPlanTemplate) error {
		return validateTemplateReferences(dir, tpl)
	})
}

// ArtifactKind is the closed vocabulary a governed write names.
type ArtifactKind string

const (
	KindInstructionPack         ArtifactKind = "instruction_pack"
	KindContextPolicy           ArtifactKind = "context_policy"
	KindAgentProfile            ArtifactKind = "agent_profile"
	KindEngineeringPlanTemplate ArtifactKind = "plan_template"
)

// ArtifactWrite is one governed write request: an id and the raw document an
// operator would otherwise hand-write into the matching file.
type ArtifactWrite struct {
	Kind     ArtifactKind    `json:"kind"`
	ID       string          `json:"id"`
	Document json.RawMessage `json:"document"`
}

// Write validates and durably writes the named artifact, returning its
// sealed form encoded as JSON (the same shape a GET of the registry already
// projects).
func Write(dir string, request ArtifactWrite) ([]byte, error) {
	switch request.Kind {
	case KindInstructionPack:
		pack, err := WriteInstructionPack(dir, request.ID, request.Document)
		if err != nil {
			return nil, err
		}
		return domain.Encode(pack)
	case KindContextPolicy:
		policy, err := WriteContextPolicy(dir, request.ID, request.Document)
		if err != nil {
			return nil, err
		}
		return domain.Encode(policy)
	case KindAgentProfile:
		profile, err := WriteAgentProfile(dir, request.ID, request.Document)
		if err != nil {
			return nil, err
		}
		return domain.Encode(profile)
	case KindEngineeringPlanTemplate:
		template, err := WriteEngineeringPlanTemplate(dir, request.ID, request.Document)
		if err != nil {
			return nil, err
		}
		return domain.Encode(template)
	default:
		return nil, &RegistryError{Detail: fmt.Sprintf("unknown artifact kind %q", request.Kind)}
	}
}
