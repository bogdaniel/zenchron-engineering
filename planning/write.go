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
func writeArtifact[T domain.Contract](dir, kind, id string, data []byte, seal func(path, id string, data []byte) (T, error)) (T, error) {
	var zero T
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

// WriteInstructionPack validates and durably writes one InstructionPack.
func WriteInstructionPack(dir, id string, data []byte) (domain.InstructionPack, error) {
	return writeArtifact(dir, instructionsDir, id, data, sealPack)
}

// WriteContextPolicy validates and durably writes one ContextPolicy.
func WriteContextPolicy(dir, id string, data []byte) (domain.ContextPolicy, error) {
	return writeArtifact(dir, contextDir, id, data, sealContextPolicy)
}

// WriteAgentProfile validates and durably writes one AgentProfile.
func WriteAgentProfile(dir, id string, data []byte) (domain.AgentProfile, error) {
	return writeArtifact(dir, profilesDir, id, data, sealProfile)
}

// WriteEngineeringPlanTemplate validates and durably writes one
// EngineeringPlanTemplate.
func WriteEngineeringPlanTemplate(dir, id string, data []byte) (domain.EngineeringPlanTemplate, error) {
	return writeArtifact(dir, templatesDir, id, data, sealTemplate)
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
