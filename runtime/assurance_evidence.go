package runtime

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A VERIFIER'S OUTPUT IS EVIDENCE, NOT INSTRUCTION, AND NOT A CONSTANT.
//
// Two separate laws meet in this file, and the third dogfood proved the runtime
// was honouring only one of them.
//
// The law it honoured: a candidate's own tests write into the verifier's
// stdout, so nothing read here is trusted. The verifier is runtime-owned; its
// OUTPUT is not, and text lifted from it can never reach a model as
// instruction, permission or authority.
//
// The law it broke: a remediation agent must be able to tell one failure from
// another. Collapsing every verdict into the verifier-definition digest handed
// five consecutive remediation attempts byte-identical input, and an agent that
// cannot see what failed cannot fix it. It edited blindly and the run died on
// its wall budget having never touched the failing test.
//
// Both are satisfied by separating the two halves of a finding. The TRUSTED
// half is machine tokens only - a class, a verifier identity, a digest, a
// runtime-composed artifact reference - and a digest is the one summary of
// attacker-writable bytes that cannot itself carry a sentence. The UNTRUSTED
// half is the excerpt, and it travels quoted, marked, and never formatted into
// the trusted envelope.

const (
	// maxFailureEvidenceLines bounds what is normalized for a signature, and
	// maxDiagnosticBytes bounds what is quoted to a model. The first failure is
	// kept rather than the last: `go test` reports in order and the earliest
	// failure is usually the cause of the ones after it.
	maxFailureEvidenceLines = 200
	maxDiagnosticBytes      = 4 << 10
	diagnosticCutMarker     = "[remaining verifier output omitted by runtime bound]"
	// untrustedSourceMarker delimits third-party text in an envelope. A
	// candidate that printed the terminator into a failing test's output would
	// otherwise close the quote early and have the rest of its output read as
	// trusted envelope, so an excerpt never carries the marker it travels
	// inside.
	untrustedSourceMarker = "UNTRUSTED-SOURCE"
	escapedSourceMarker   = "UNTRUSTED-SOURCE-ESCAPED-BY-RUNTIME"
)

// volatileEvidence are the parts of a transcript that differ between two
// identical verifications. They are normalized away so the SAME failure digests
// the same twice - otherwise a signature would be as useless as the constant it
// replaces, just noisier.
var volatileEvidence = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// The randomized build directory `go test` links through.
	{regexp.MustCompile(`go-build\d+`), "go-build"},
	// ONLY `go test`'s OWN timings: the parenthesised per-test duration and the
	// tab-delimited per-package one. A bare duration is NOT normalized, because
	// a duration is frequently the thing under assertion - "want 30s, got 45s"
	// and "want 60s, got 45s" are different failures, and an earlier version of
	// this list collapsed them into the same signature.
	{regexp.MustCompile(`\(\d+(\.\d+)?s\)`), "(Ns)"},
	{regexp.MustCompile(`\t\d+(\.\d+)?s$`), "\tNs"},
	// Stack-frame artifacts: the frame offset, and heap pointers, which on this
	// runtime are long and lowercase. Short hex is left alone on purpose -
	// "want 0xdeadbeef, got 0xcafebabe" is an assertion, not an address.
	//
	// ponytail: length is a heuristic for "this is a pointer". A test asserting
	// on a nine-digit lowercase hex literal would have it normalized away.
	// Narrow it by requiring stack-frame context if that ever bites.
	{regexp.MustCompile(`\+0x[0-9a-f]+`), "+0xOFF"},
	{regexp.MustCompile(`0x[0-9a-f]{9,}`), "0xADDR"},
	// Temporary directories a test created for itself.
	{regexp.MustCompile(`/tmp/[A-Za-z0-9_.-]*\d[A-Za-z0-9_.-]*`), "/tmp/PATH"},
	{regexp.MustCompile(`\bgoroutine \d+\b`), "goroutine N"},
}

// normalizeFailureEvidence keeps the lines of a transcript that say something
// about a FAILURE and drops the ones that only say something passed. It is
// deliberately a subtraction rather than a pattern match on failure: a verifier
// that fails in a way this runtime has never seen must still produce evidence,
// and a keep-list would silently return nothing for it.
func normalizeFailureEvidence(transcript []byte) []string {
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	kept := make([]string, 0, 32)
	for scanner.Scan() && len(kept) < maxFailureEvidenceLines {
		line := strings.TrimRight(scanner.Text(), " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A package that passed, or has no tests, is not evidence about a
		// failure and is the bulk of a green transcript.
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "ok ") ||
			strings.HasPrefix(trimmed, "ok\t") || strings.HasPrefix(trimmed, "?") ||
			strings.HasPrefix(trimmed, "--- PASS") || strings.HasPrefix(trimmed, "=== RUN") ||
			strings.HasPrefix(trimmed, "=== PAUSE") || strings.HasPrefix(trimmed, "=== CONT") ||
			trimmed == "PASS" {
			continue
		}
		for _, volatile := range volatileEvidence {
			line = volatile.pattern.ReplaceAllString(line, volatile.replacement)
		}
		kept = append(kept, line)
	}
	return kept
}

// AssuranceFailureSignature is a stable identity for ONE failure, derived from
// normalized evidence rather than from the verifier that observed it.
//
// It is a digest on purpose. The bytes it summarizes are partly written by the
// candidate, and a digest is the only rendering of them that is safe to place
// in the trusted half of a worker envelope while still distinguishing this
// failure from the next one.
func AssuranceFailureSignature(transcript []byte) (string, error) {
	lines := normalizeFailureEvidence(transcript)
	if len(lines) == 0 {
		return "", nil
	}
	digest, err := Digest(lines)
	if err != nil {
		return "", err
	}
	return "failure:" + digest[:16], nil
}

// AssuranceDiagnostic reads the bounded, sanitized excerpt a remediation agent
// is shown for one finding.
//
// The reference is composed from runtime identity by AttemptRef and is joined
// under the artifact root here, so a value that ever came from a provider or a
// candidate cannot address a file outside it. The sanitized candidate is read
// rather than the raw forensic transcript, which is the same choice
// PriorExecutionAttemptContext makes for the same reason.
func (s ArtifactStore) AssuranceDiagnostic(ref, runID string) (string, error) {
	if s.Root == "" || !validAssuranceEvidenceRef(ref, runID) {
		return "", nil
	}
	path := filepath.Join(s.Root, filepath.Clean("/"+ref)+".sanitized-candidate.log")
	if !strings.HasPrefix(path, filepath.Clean(s.Root)+string(os.PathSeparator)) {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Evidence can be absent - a verifier that could not start writes none.
		// Saying nothing is correct; inventing a diagnostic is not.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return boundDiagnostic(normalizeFailureEvidence(raw)), nil
}

// validAssuranceEvidenceRef refuses anything that is not an evidence identity
// this runtime itself composed for THIS run.
//
// ArtifactRef must be an opaque evidence identifier, never a file-read
// primitive. It reaches here from a durable payload written from an
// AssuranceProvider's result, and a provider is an interface: a hostile or
// merely broken implementation could put any string there. Containment under
// the artifact root is necessary and not sufficient, because it would still
// permit reading another RUN's evidence.
//
// So the shape is checked rather than trusted: the exact five segments
// attemptTranscriptPrefix produces, and a run component equal to the run being
// remediated. Anything else yields no diagnostic at all, which is the same
// degradation as unreadable evidence.
func validAssuranceEvidenceRef(ref, runID string) bool {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(runID) == "" {
		return false
	}
	segments := strings.Split(ref, "/")
	if len(segments) != 5 || segments[0] != "provider" {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	if segments[2] != encodePathComponent(runID) {
		return false
	}
	if !strings.HasPrefix(segments[4], "attempt-") {
		return false
	}
	for _, digit := range segments[4][len("attempt-"):] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return len(segments[4]) > len("attempt-")
}

// boundDiagnostic renders the excerpt under a byte ceiling, and says so when it
// cuts. A model reading a truncated transcript that does not admit it was
// truncated will reason about the absence of a failure it was simply not shown.
func boundDiagnostic(lines []string) string {
	var out strings.Builder
	for _, line := range lines {
		line = strings.ReplaceAll(line, untrustedSourceMarker, escapedSourceMarker)
		if out.Len()+len(line)+1 > maxDiagnosticBytes {
			out.WriteString(diagnosticCutMarker)
			return out.String()
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}
