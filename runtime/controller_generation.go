package runtime

// WHICH CONTROLLER THIS PROCESS IS, and where the operator's stable entrypoint
// points.
//
// Both answers used to live outside the runtime: the identity was assembled in
// the CLI from its own link-time variables, and the entrypoint was whatever
// path an operator had typed most recently. That was survivable while a human
// performed every upgrade and read the output. It stops being survivable the
// moment a controller has to PROVE ITS OWN GENERATION to itself as part of an
// automated activation, because a process cannot prove anything by shelling out
// to a binary it has to assume is itself.
//
// So the identity is a runtime value that the CLI formats, rather than a CLI
// value the runtime cannot see. `controller inspect-self` prints exactly the
// build document it always printed - the adopted builder parses that output and
// must keep working - but it is now a presentation of this record instead of a
// second implementation of it.
//
// THE LAW BETWEEN THE TWO: the durable activation record is authority, and the
// stable entrypoint is a repairable projection of that authority. A pointer
// that disagrees with the record is wrong and is fixed from the record; a
// record that disagrees with the pointer is right. Nothing reads the pointer to
// decide what is true, which is what keeps a symlink from becoming a governance
// surface.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ControllerDeclaration is what a build CLAIMS about itself: the values stamped
// into it at link time. It is deliberately not a ControllerBuild, because a
// build document carries a binary digest and a claim cannot: a binary cannot
// contain its own final digest, so the digest is measured here and never
// declared.
type ControllerDeclaration struct {
	Kind           string
	Version        string
	SourceRevision string
	SourceTree     string
}

// ControllerSelfRecord is the canonical in-process answer to "which controller
// is running". It is one value so that the CLI, the doctor, an activation proof
// and a handoff all read the same measurement rather than each repeating it.
type ControllerSelfRecord struct {
	// Build is the provenance document, with the digest MEASURED from the
	// running artifact rather than declared by it.
	Build ControllerBuild `json:"build"`
	// ExecutablePath is the artifact this process was started from. It is
	// recorded because an activation has to say which file it proved, and
	// because an operator chasing generation drift needs the path more than
	// they need the digest.
	ExecutablePath string `json:"executable_path,omitempty"`
	// Measured is the digest of that file.
	Measured string `json:"measured_sha256,omitempty"`
	// Unattested reports a build that makes no claim. It is legal - `go build`
	// produces one - and it can never satisfy an activation proof.
	Unattested bool `json:"unattested,omitempty"`
}

// CurrentControllerIdentity measures the running artifact and returns the
// canonical self record.
//
// An unattested build measures nothing: there is no claim to check a
// measurement against, and hashing the artifact anyway would produce a number
// that looks like provenance while proving nothing about what produced it.
func CurrentControllerIdentity(declared ControllerDeclaration) (ControllerSelfRecord, error) {
	return ControllerIdentityFrom(declared, os.Executable, measureExecutable)
}

// ControllerIdentityFrom is the resolution itself, with the two pieces of
// measurement as parameters.
//
// They are parameters so the production path can be asserted exactly - that an
// attested build measures the running artifact exactly ONCE, and that an
// unattested one does not measure it at all - without a real -ldflags build and
// without hashing whatever binary is running a test.
func ControllerIdentityFrom(declared ControllerDeclaration, locate func() (string, error), measure func(string) (string, error)) (ControllerSelfRecord, error) {
	if declared.Kind == "" || declared.Kind == ControllerUnattested {
		return ControllerSelfRecord{Build: ControllerBuild{Kind: ControllerUnattested}, Unattested: true}, nil
	}
	path, err := locate()
	if err != nil {
		return ControllerSelfRecord{}, fmt.Errorf("cannot locate the running controller binary: %w", err)
	}
	measured, err := measure(path)
	if err != nil {
		return ControllerSelfRecord{}, fmt.Errorf("cannot measure the running controller binary: %w", err)
	}
	return ControllerSelfRecord{
		Build: ControllerBuild{
			Kind: declared.Kind, Version: declared.Version,
			SourceRevision: declared.SourceRevision, SourceTree: declared.SourceTree,
			BinarySHA256: measured,
		},
		ExecutablePath: path,
		Measured:       measured,
	}, nil
}

// ProvesGeneration reports whether this process IS the controller a handoff
// record names as its successor.
//
// Every field is compared rather than the digest alone. A binary digest that
// matches while the recorded revision does not is not a passing proof with a
// cosmetic discrepancy; it is two documents describing different things, and an
// activation that accepted it would be recording a generation nobody built.
func (r ControllerSelfRecord) ProvesGeneration(successor ControllerBinding) error {
	if r.Unattested {
		return fmt.Errorf("an unattested controller cannot prove an adopted generation")
	}
	if successor.Build == nil {
		return fmt.Errorf("the handoff names no successor build to prove against")
	}
	declared := *successor.Build
	switch {
	case r.Build.BinarySHA256 != declared.BinarySHA256:
		return fmt.Errorf("the running binary measures %s and the successor record names %s",
			shortSHA(r.Build.BinarySHA256), shortSHA(declared.BinarySHA256))
	case r.Build.Kind != declared.Kind:
		return fmt.Errorf("the running controller is %q and the successor record names %q", r.Build.Kind, declared.Kind)
	case r.Build.SourceRevision != declared.SourceRevision:
		return fmt.Errorf("the running controller is built from %s and the successor record names %s",
			shortSHA(r.Build.SourceRevision), shortSHA(declared.SourceRevision))
	case r.Build.SourceTree != declared.SourceTree:
		return fmt.Errorf("the running controller's tree is %s and the successor record names %s",
			shortSHA(r.Build.SourceTree), shortSHA(declared.SourceTree))
	case r.Build.Version != declared.Version:
		return fmt.Errorf("the running controller is version %q and the successor record names %q",
			r.Build.Version, declared.Version)
	}
	return nil
}

// StableEntrypointName is the operator-facing pointer inside the controller
// root. It sits BESIDE the immutable generation directories rather than in a
// repository bin/: bin/ is build output, and an operator's authority surface
// must not be something a build can overwrite.
const StableEntrypointName = "current"

// handoffReader is the durable authority an activation consults. It is an
// interface because activation must be able to refuse from a read alone.
type handoffReader interface {
	ControllerHandoff(id string) (ControllerHandoff, bool, error)
	currentActivationReader
}

// ActivateControllerGeneration points the stable entrypoint at the running
// generation, and only then.
//
// TWO PROOFS ARE REQUIRED AND NEITHER SUBSTITUTES FOR THE OTHER. The durable
// record must already say this handoff reached `activated` - the pointer is a
// projection of authority and may not be the thing that creates it - and the
// running process must prove it is the generation that record names. A process
// that flipped the pointer on its own say-so would be asserting an activation
// rather than reflecting one, which is the same defect as a candidate
// controller adopting itself.
//
// It is idempotent, and that is what makes it REPAIR: a crash between the
// activation record and the pointer leaves an operator aimed at the previous
// artifact while the durable truth already names the successor, and running
// this again fixes the projection without deciding anything.
func ActivateControllerGeneration(store handoffReader, handoffID string, self ControllerSelfRecord, controllerRoot string) (string, error) {
	record, found, err := store.ControllerHandoff(handoffID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no handoff %q is recorded, and the stable entrypoint follows the record", handoffID)
	}
	if record.Phase != HandoffActivated {
		return "", fmt.Errorf("handoff %s is at phase %q; the stable entrypoint moves only after a durable activation",
			handoffID, record.Phase)
	}
	// AND IT MUST STILL GOVERN. A historically activated transition is not a
	// licence to repoint the entrypoint at the generation it once activated:
	// that is how a superseded controller rolls the projection back to itself.
	if err := governsNow(store, handoffID); err != nil {
		return "", err
	}
	if err := self.ProvesGeneration(record.Successor.Binding); err != nil {
		return "", fmt.Errorf("this process is not the activated generation: %w", err)
	}
	artifact := record.Successor.ArtifactPath
	if artifact == "" {
		return "", fmt.Errorf("handoff %s records no successor artifact to point at", handoffID)
	}
	// The pointer names the generation DIRECTORY, so the stable path is
	// <root>/current/zenchron-engineering and the operator's command line never
	// carries a revision.
	generation := filepath.Dir(artifact)
	if _, err := os.Stat(artifact); err != nil {
		return "", fmt.Errorf("the activated artifact %s is not readable: %w", artifact, err)
	}
	pointer := filepath.Join(controllerRoot, StableEntrypointName)
	if err := replaceSymlinkAtomically(generation, pointer); err != nil {
		return "", err
	}
	return filepath.Join(pointer, filepath.Base(artifact)), nil
}

// replaceSymlinkAtomically points link at target by RENAMING a new link over
// the old one.
//
// The target is never rewritten in place. An in-place update has a window in
// which the pointer names nothing - or half of something - and that window is
// exactly when an operator's shell resolves it. Rename is atomic within a
// filesystem, so a reader sees either the previous generation or the new one.
//
// The containing directory is synced afterwards: a rename that the kernel has
// accepted but not yet persisted is a pointer that a crash can revert, and the
// whole point of the projection is that it can be trusted between crashes.
func replaceSymlinkAtomically(target, link string) error {
	directory := filepath.Dir(link)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	// A DIRECTORY AT THE POINTER PATH IS THE ONE STATE THIS WILL NOT RESOLVE.
	// Rename cannot replace it, and removing it would mean deleting whatever an
	// operator put there to find out whether it mattered. Every other
	// occupant - a stale link, a dangling one, a regular file somebody copied
	// over it - is replaced without being read, because the projection is
	// written from the record and never derived from what it finds.
	if existing, err := os.Lstat(link); err == nil && existing.IsDir() {
		return fmt.Errorf("%s is a directory; the stable entrypoint is a link and this will not delete an operator's directory to become one", link)
	}
	staging, err := os.MkdirTemp(directory, ".entrypoint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // best-effort: a leftover staging dir points at nothing
	candidate := filepath.Join(staging, StableEntrypointName)
	if err := os.Symlink(target, candidate); err != nil {
		return fmt.Errorf("the stable entrypoint could not be staged: %w", err)
	}
	if err := os.Rename(candidate, link); err != nil {
		return fmt.Errorf("the stable entrypoint could not be replaced atomically: %w", err)
	}
	return syncDirectory(directory)
}

// syncDirectory persists a rename. A failure to open the directory is reported;
// a failure to sync it is not fatal on filesystems that do not support it, and
// the rename has already happened either way.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	_ = directory.Sync()
	return nil
}

// ControllerGenerationStatus is what an operator reads to see whether the three
// things that can drift - the durable record, the stable pointer and the
// running process - currently agree.
type ControllerGenerationStatus struct {
	Running   ControllerSelfRecord `json:"running"`
	Pointer   string               `json:"pointer,omitempty"`
	PointsAt  string               `json:"points_at,omitempty"`
	Handoff   *ControllerHandoff   `json:"handoff,omitempty"`
	Drifted   bool                 `json:"drifted"`
	CheckedAt time.Time            `json:"checked_at"`
}

// DescribeControllerGeneration reports that agreement without changing
// anything. Drift is not an error here: reporting it is the point, and #234's
// evidence is that it goes unnoticed precisely because nothing ever said it out
// loud.
func DescribeControllerGeneration(self ControllerSelfRecord, controllerRoot string, record *ControllerHandoff, now time.Time) ControllerGenerationStatus {
	status := ControllerGenerationStatus{Running: self, Handoff: record, CheckedAt: now}
	status.Pointer = filepath.Join(controllerRoot, StableEntrypointName)
	target, err := os.Readlink(status.Pointer)
	if err != nil {
		status.Drifted = true
		return status
	}
	status.PointsAt = target
	if self.ExecutablePath != "" && filepath.Dir(self.ExecutablePath) != target {
		status.Drifted = true
	}
	if record != nil && record.Phase == HandoffActivated {
		if err := self.ProvesGeneration(record.Successor.Binding); err != nil {
			status.Drifted = true
		}
	}
	return status
}
