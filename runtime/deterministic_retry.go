package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
)

// ReasonDeterministicFailureUnchanged means another attempt would repeat a
// known local contradiction. It waits for relevant input, never for time.
const ReasonDeterministicFailureUnchanged = "deterministic_failure_unchanged"

// OperationFailure is runtime-owned identity, not a provider diagnosis. Code
// identifies a deterministic check; Signature includes that check and the
// operation binding. BindingSHA256 identifies its relevant durable inputs AFTER
// failure, including any partial mutation the attempt must preserve.
type OperationFailure struct {
	Classification string `json:"classification"`
	Code           string `json:"code"`
	Signature      string `json:"signature"`
	BindingSHA256  string `json:"binding_sha256"`
	BindingError   string `json:"binding_error,omitempty"`
}

const deterministicLocal = "deterministic_local"

// deterministicError is constructed only where a local check established a
// contradiction. Arbitrary filesystem, subprocess and provider errors must
// not acquire this type merely because their diagnostics repeat.
type deterministicError struct {
	code string
	err  error
}

func (e *deterministicError) Error() string { return e.err.Error() }
func (e *deterministicError) Unwrap() error { return e.err }
func deterministicRefusal(code string, err error) error {
	return &deterministicError{code: code, err: err}
}

func deterministicCode(err error) string {
	var local *deterministicError
	if errors.As(err, &local) {
		return local.code
	}
	var compilation *policy.CompilationError
	if errors.As(err, &compilation) {
		return "policy.compile"
	}
	var integrity *WorkspaceIntegrityError
	if errors.As(err, &integrity) {
		return "candidate.metadata_integrity"
	}
	var credential *CredentialMaterialError
	if errors.As(err, &credential) && (credential.Kind == CredentialMaterialFile || credential.Kind == CredentialMaterialValue) {
		return "candidate.credential_" + string(credential.Kind)
	}
	return ""
}

func (f *OperationFailure) validate() error {
	if f == nil {
		return nil
	}
	if f.Classification != deterministicLocal || f.Code == "" || len(f.Code) > maxPayloadFieldBytes ||
		!isSHA256Hex(f.Signature) ||
		(f.BindingError == "" && !isSHA256Hex(f.BindingSHA256)) ||
		(f.BindingError != "" && (f.BindingSHA256 != "" || len(f.BindingError) > maxPayloadFieldBytes)) {
		return errors.New("invalid deterministic operation failure identity")
	}
	return nil
}

func failureSignature(code string, op RunOperation) (string, error) {
	return Digest(struct{ Code, Kind, Binding string }{code, op.Kind, op.IdempotencyKey})
}

// failureBinding excludes bookkeeping: attempts, journal sequence, waits,
// leases and elapsed time cannot resolve a local contradiction. Actual source,
// contract, model, policy, controller environment and candidate bytes can.
func (r *EngineeringRuntime) failureBinding(s *runState, code string) (string, error) {
	basis := struct {
		Source                                             *sourceRecord
		Contract                                           Ref
		CandidateRevision, CandidateTree, Base, Controller string
		Model                                              domain.ProjectModel
		Policy                                             domain.EngineeringPolicy
		Workspace                                          string
	}{s.source, s.projection.Contract, s.projection.CandidateRevision, s.projection.CandidateTree,
		s.pinnedBase(), r.controller, r.projectModel(s), r.deps.Policy, ""}
	dir := candidateDir(r.deps.StateDir, s.run.ID)
	if _, err := os.Lstat(dir); err == nil {
		workspace, err := retryWorkspaceBinding(dir, code)
		if err != nil {
			return "", err
		}
		basis.Workspace = workspace
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return Digest(basis)
}

// Read-only observations of durable candidate state. No write-tree, add,
// commit, restore, reset or cleanup is performed by the retry decision.
func retryWorkspaceBinding(dir, code string) (string, error) {
	if code == "policy.compile" {
		// Observed policy resolution reads the committed subject, never the
		// index, worktree, or scratch a producer left beside that subject.
		head, err := readHead(dir)
		if err != nil {
			return "", err
		}
		return Digest(head)
	}
	metadata, err := gitMetadataDigest(dir)
	if err != nil {
		return "", err
	}
	index, err := gitOutput(dir, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return "", err
	}
	listed, err := gitOutput(dir, "ls-files", "--cached", "--others", "-z")
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimRight(listed, "\x00"), "\x00")
	if listed == "" {
		paths = nil
	}
	content := workspaceContentDigest(dir, paths)
	if len(paths) > 0 && content == "" {
		return "", errors.New("retry candidate content is unreadable")
	}
	return Digest(struct{ Metadata, Index, Content string }{metadata, index, content})
}

func (r *EngineeringRuntime) recordDeterministicFailure(s *runState, op *RunOperation, code string) error {
	op.Failure = nil
	if op.State != OperationFailed || code == "" {
		return nil
	}
	signature, err := failureSignature(code, *op)
	if err != nil {
		return err
	}
	binding, err := r.failureBinding(s, code)
	op.Failure = &OperationFailure{Classification: deterministicLocal, Code: code, Signature: signature, BindingSHA256: binding}
	if err != nil {
		op.Failure.BindingError = boundedDetail(err.Error())
		return fmt.Errorf("bind deterministic failure: %w", err)
	}
	return nil
}

func (r *EngineeringRuntime) identicalDeterministicFailure(s *runState, op RunOperation) (bool, error) {
	prior, ok := s.snapshot.Operations[op.ID]
	if !ok || prior.State != OperationFailed || prior.Failure == nil {
		return false, nil
	}
	if err := prior.Failure.validate(); err != nil {
		return false, err
	}
	signature, err := failureSignature(prior.Failure.Code, op)
	if err != nil {
		return false, err
	}
	binding, err := r.failureBinding(s, prior.Failure.Code)
	if err != nil {
		return false, fmt.Errorf("compare deterministic failure binding: %w", err)
	}
	return prior.Failure.Signature == signature && prior.Failure.BindingSHA256 == binding, nil
}
