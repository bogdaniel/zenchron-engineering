package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExecCapableScratchBase answers where THIS execution boundary allows the
// runtime to create scratch that Go may execute from.
//
// An environment that has already published runtime-owned exec-capable scratch
// wins: the verifier sandbox mounts one and names it in GOTMPDIR, and a future
// sandboxed worker would do the same. The variable is read from the runtime's
// OWN process environment, which is set by the operator or by the sandbox that
// launched it - never by a candidate, which has no way to reach it.
//
// Otherwise the caller's runtime-owned directory is used, and only if there is
// none does this fall back to the process default.
func ExecCapableScratchBase(preferred string) string {
	if published := strings.TrimSpace(os.Getenv("GOTMPDIR")); published != "" {
		return published
	}
	if trimmed := strings.TrimSpace(preferred); trimmed != "" {
		return trimmed
	}
	return os.TempDir()
}

// ExecutionScratchDir composes the per-attempt scratch path for one invocation.
//
// It is built from scheduler identity exactly as the attempt transcript and the
// reviewer result are, so two attempts never share a build directory and a
// replay arrives at the same path from the journal alone.
//
// It lives under the RUN, beside the candidate workspace and the assurance
// checkouts, because that is where the collector already looks. Scratch is not
// evidence and nothing reads it after the invocation ends, but it holds a Go
// build cache that a remediation attempt re-uses - deleting it per invocation
// would make every retry recompile the world, and this workload is bounded by
// wall time. It is retired with the run instead.
//
// The operation component is a DIGEST, not the encoded identity the transcript
// uses (#331). This path is the worker's TMPDIR, and an operation id encodes to
// ~150 bytes of %XX escapes: SQLite URI-decodes a "file:" path, and a Unix
// socket address is bounded near 104 bytes, so the worker's own tests broke on
// the directory rather than on the change. The path is recomputed from
// identity; it is not an identity or authority input and is never read back,
// so a 64-bit digest keeps it unique and replayable without the escapes.
func ExecutionScratchDir(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	if err := attempt.Validate(); err != nil {
		return "", err
	}
	operation := sha256.Sum256([]byte(attempt.OperationID))
	return filepath.Join(ExecCapableScratchBase(stateDir), "runs",
		encodePathComponent(attempt.RunID), executionScratchDir,
		hex.EncodeToString(operation[:8]),
		fmt.Sprintf("attempt-%d", attempt.Attempt)), nil
}
