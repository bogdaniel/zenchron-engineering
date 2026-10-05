package runtime

import (
	"errors"
	"fmt"
	"os"

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
	CheckPath      []byte `json:"check_path_base64,omitempty"`
	Signature      string `json:"signature"`
	BindingSHA256  string `json:"binding_sha256"`
	BindingScope   string `json:"binding_scope,omitempty"`
	BindingError   string `json:"binding_error,omitempty"`
}

const deterministicLocal = "deterministic_local"
const deterministicBindingScope = "check-input-v1"

// deterministicError is constructed only where a local check established a
// contradiction. Arbitrary filesystem, subprocess and provider errors must
// not acquire this type merely because their diagnostics repeat.
type deterministicError struct {
	code string
	path string
	err  error
}

func (e *deterministicError) Error() string { return e.err.Error() }
func (e *deterministicError) Unwrap() error { return e.err }
func deterministicRefusal(code string, err error) error {
	return &deterministicError{code: code, err: err}
}

func deterministicPathRefusal(code, path string, err error) error {
	return &deterministicError{code: code, path: path, err: err}
}

func deterministicIdentity(err error) *OperationFailure {
	code := deterministicCode(err)
	if code == "" {
		return nil
	}
	f := &OperationFailure{Classification: deterministicLocal, Code: code, BindingScope: deterministicBindingScope}
	var local *deterministicError
	if errors.As(err, &local) && local.path != "" {
		f.CheckPath = []byte(local.path)
	}
	var credential *CredentialMaterialError
	if len(f.CheckPath) == 0 && errors.As(err, &credential) {
		f.CheckPath = []byte(credential.Path)
	}
	return f
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
	if errors.As(err, &credential) && credential.Kind == CredentialMaterialFile {
		return "candidate.credential_" + string(credential.Kind)
	}
	return ""
}

func (f *OperationFailure) validate() error {
	if f == nil {
		return nil
	}
	if len(f.CheckPath) > 0 {
		if _, err := normalizedCandidatePath(string(f.CheckPath)); err != nil {
			return fmt.Errorf("invalid deterministic check path: %w", err)
		}
	}
	if f.Classification != deterministicLocal || f.Code == "" || len(f.Code) > maxPayloadFieldBytes ||
		len(f.BindingScope) > maxPayloadFieldBytes ||
		!isSHA256Hex(f.Signature) ||
		(f.BindingError == "" && !isSHA256Hex(f.BindingSHA256)) ||
		(f.BindingError != "" && (f.BindingSHA256 != "" || len(f.BindingError) > maxPayloadFieldBytes)) {
		return errors.New("invalid deterministic operation failure identity")
	}
	return nil
}

func failureSignature(f OperationFailure, op RunOperation) (string, error) {
	var path []byte
	if len(f.CheckPath) > 0 {
		path = f.CheckPath
	}
	return Digest(struct {
		Code, Kind, Binding string
		Path                []byte
	}{f.Code, op.Kind, op.IdempotencyKey, path})
}

// failureBinding dispatches on the check that owns the contradiction. An
// operation's signature is separate: sharing an operation does not make its
// policy, source or controller configuration inputs to a candidate-only check.
func (r *EngineeringRuntime) failureBinding(s *runState, op RunOperation, code string) (string, error) {
	dir := candidateDir(r.deps.StateDir, s.run.ID)
	switch code {
	case "policy.compile":
		var head observedHead
		if _, err := os.Lstat(dir); err == nil {
			if head, err = readHead(dir); err != nil {
				return "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return Digest(struct {
			Compiler string
			Source   *sourceRecord
			Contract Ref
			Model    domain.ProjectModel
			Policy   domain.EngineeringPolicy
			Subject  observedHead
		}{policy.CompilerVersion, s.source, s.projection.Contract, r.projectModel(s), r.deps.Policy, head})
	case "kernel.source_unpinned":
		return Digest(s.source != nil)
	case "contract.semantic_claim_missing":
		return Digest(s.semanticClaims())
	case "format.go_paths_missing":
		paths, err := candidatePaths(dir, s.pinnedBase(), s.projection.CandidateRevision)
		if err != nil {
			return "", err
		}
		return Digest(FormatPaths(paths))
	case "operation.handler_missing":
		return Digest(ControllerBuildBinding(r.deps.ControllerBuild))
	case "candidate.metadata_integrity":
		metadata, err := gitMetadataDigest(dir)
		if err != nil {
			return "", err
		}
		var result commitFailure
		if len(op.Result) > 0 {
			if err := decodeJSON(op.Result, &result); err != nil {
				return "", err
			}
		}
		// Recovery checks commit identity, path proof, exclusions and metadata.
		// The producing operation and checkpoint label cannot fix integrity.
		if result.RuntimeCommit != nil {
			proof := *result.RuntimeCommit
			proof.Producing, proof.Checkpoint, proof.PathCount = "", false, 0
			result.RuntimeCommit = &proof
		}
		expected := s.projection.CandidateRevision
		if expected == "" {
			expected = s.baseRevision()
		}
		return Digest(struct {
			ExpectedRevision, ExpectedMetadata string
			RecordedCommit                     *runtimeCommit
			ObservedMetadata                   string
		}{expected, s.projection.CandidateMetadata, result.RuntimeCommit, metadata})
	default:
		return candidateFailureBinding(dir, code, string(op.Failure.CheckPath), op.Result)
	}
}

func (r *EngineeringRuntime) recordDeterministicFailure(s *runState, op *RunOperation, identity *OperationFailure) error {
	op.Failure = nil
	if op.State != OperationFailed || identity == nil {
		return nil
	}
	failure := *identity
	op.Failure = &failure
	signature, err := failureSignature(failure, *op)
	if err != nil {
		return err
	}
	binding, err := r.failureBinding(s, *op, failure.Code)
	op.Failure.Signature, op.Failure.BindingSHA256 = signature, binding
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
	if prior.Failure.BindingScope != deterministicBindingScope {
		return false, errors.New("deterministic failure uses an unsupported input binding scope")
	}
	signature, err := failureSignature(*prior.Failure, op)
	if err != nil {
		return false, err
	}
	binding, err := r.failureBinding(s, prior, prior.Failure.Code)
	if err != nil {
		return false, fmt.Errorf("compare deterministic failure binding: %w", err)
	}
	return prior.Failure.Signature == signature && prior.Failure.BindingSHA256 == binding, nil
}
