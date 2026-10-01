package runtime

// The feedback resolution protocol.
//
// Provider return is not proof of semantic completion. An invocation that
// left the workspace unchanged and simply exited is NOT, by itself, evidence
// that admitted review feedback was addressed: it is indistinguishable from a
// provider that ran out of time, deferred to background work it never
// finished, or misread the feedback entirely. Treating "returned, nothing
// changed" as "nothing was required" is an INFERENCE, and #376 exists because
// that inference discharged a REQUEST_CHANGES review whose remediation had
// actually deferred to a background task that never completed.
//
// A legitimate no-change response must remain possible - a candidate can
// already satisfy feedback based on a misunderstanding - but it must be
// STATED, not inferred. This file is the typed, bound channel for stating it,
// built the same way the reviewer verdict channel (reviewer_result.go) is:
//
//	FeedbackResolution { schema_version, resolution, subject, keys, reason }
//
// It is bound to the EXACT feedback keys this invocation was given and the
// EXACT subject revision it was invoked against, through the same
// runtime-owned, outside-the-workspace channel the reviewer protocol uses.
// Unbound or partial claims are not admitted, and an unadmitted claim leaves
// every key it would have discharged outstanding - the same fail-closed
// default a provider that said nothing at all gets.
//
// #379 generalizes the same inference defect from feedback discharge to
// checkpoint continuation: a continuation invocation that inherits a
// runtime-owned checkpoint and returns having mutated nothing is exactly as
// unproven as a remediation that left admitted feedback outstanding, whether
// or not any feedback is involved. The SAME document, naming the keys (if
// any) this invocation was delivered and the exact checkpoint commit it was
// shown, is the one channel for stating either kind of completion; naming
// zero keys against zero delivered keys is a checkpoint-only claim.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FeedbackResolutionSchemaVersion is the protocol version a resolution must
// state.
const FeedbackResolutionSchemaVersion = "0.1"

// FeedbackResolutionNoChangeRequired is the only recognized resolution: the
// admitted feedback named by Keys requires no change to the exact Subject
// revision. There is no other value, by design - a provider that wants to
// discharge feedback has exactly one way to say so, and it is this one.
const FeedbackResolutionNoChangeRequired = "no_change_required"

// feedbackResolutionFile is the basename of the runtime-owned result artifact.
const feedbackResolutionFile = "feedback-resolution.json"

// maxFeedbackResolutionBytes bounds the file the runtime will read, for the
// same denial-of-service reason maxReviewerResultBytes does.
const maxFeedbackResolutionBytes = 64 << 10

// maxFeedbackResolutionKeys bounds how many keys one resolution may name.
const maxFeedbackResolutionKeys = maxPayloadListItems

// FeedbackResolution is what a producer invocation emits through the
// structured channel to explicitly discharge feedback without mutating the
// candidate.
type FeedbackResolution struct {
	SchemaVersion string `json:"schema_version"`
	// Resolution must be FeedbackResolutionNoChangeRequired. There is no
	// default: an absent or unrecognized resolution is not admitted.
	Resolution string `json:"resolution"`
	// Subject restates the exact candidate revision this invocation was shown.
	// It is a CLAIM at this point: AdmitFeedbackResolution refuses a claim
	// that disagrees with the revision the runtime actually invoked against.
	Subject string `json:"subject"`
	// Keys names every admitted feedback key this resolution discharges. It
	// must equal exactly the set of keys this invocation was delivered - not
	// a subset, and nothing outside it - because a partial claim would leave
	// the runtime guessing which of several delivered items the remaining
	// prose was about.
	Keys []string `json:"keys"`
	// Reason is a bounded human-facing summary. It is recorded and shown; it
	// is never parsed and never decides anything.
	Reason string `json:"reason,omitempty"`
}

// FeedbackResolutionPath is the runtime-owned location for ONE invocation's
// resolution. It lives beside the attempt's transcripts, under the runtime's
// state directory, and never inside the candidate workspace - the same
// unspoofable placement reviewer_result.go uses and for the same reason.
func FeedbackResolutionPath(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	if err := attempt.Validate(); err != nil {
		return "", err
	}
	prefix, err := attemptTranscriptPrefix("remediation", attempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "artifacts", prefix+"."+feedbackResolutionFile), nil
}

// PrepareFeedbackResolution clears any leftover resolution and returns the
// path this invocation must write to, for the same reason
// PrepareReviewerResult clears its slot: a result surviving from an earlier
// attempt must never be inherited by this one.
func PrepareFeedbackResolution(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	path, err := FeedbackResolutionPath(stateDir, attempt)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

// ReadFeedbackResolution reads and decodes the resolution a provider wrote.
//
// An absent file is not an error and not a resolution: it is (nil, nil), and
// every key this invocation was delivered stays outstanding. That is the
// case a provider that returned without stating anything falls into - which
// is exactly the gap #376 closes: a provider merely returning is never read
// as a resolution.
func ReadFeedbackResolution(path string) (*FeedbackResolution, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("feedback resolution at %s is a directory", path)
	}
	if info.Size() > maxFeedbackResolutionBytes {
		return nil, fmt.Errorf("feedback resolution is %d bytes, above the %d byte bound", info.Size(), maxFeedbackResolutionBytes)
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.DisallowUnknownFields()
	var result FeedbackResolution
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("feedback resolution is not a valid %s document: %w", feedbackResolutionFile, err)
	}
	if strings.Trim(string(document[decoder.InputOffset():]), " \t\r\n") != "" {
		return nil, fmt.Errorf("feedback resolution carries trailing data after its JSON value")
	}
	return &result, nil
}

// AdmitFeedbackResolution decides whether one structured resolution may
// discharge feedback, and returns the exact keys it admits.
//
// A refusal is never promoted to an operation failure: a claim that does not
// bind is treated exactly like no claim at all, and the keys it would have
// discharged stay outstanding. That is deliberate - this function's failure
// mode IS the invariant #376 states, not an exception to it.
func AdmitFeedbackResolution(deliveredKeys []string, subject string, result *FeedbackResolution) ([]string, error) {
	if result == nil {
		return nil, nil
	}
	if result.SchemaVersion != FeedbackResolutionSchemaVersion {
		return nil, fmt.Errorf("feedback resolution schema version %q is not %q", result.SchemaVersion, FeedbackResolutionSchemaVersion)
	}
	if result.Resolution != FeedbackResolutionNoChangeRequired {
		return nil, fmt.Errorf("feedback resolution %q is not %q", result.Resolution, FeedbackResolutionNoChangeRequired)
	}
	if subject == "" || result.Subject != subject {
		return nil, fmt.Errorf("the resolution claims subject %s and the invocation was shown %s", short12(result.Subject), short12(subject))
	}
	// Naming zero keys is refused only when this invocation was actually
	// delivered feedback: a claim that discharges nothing while feedback sits
	// outstanding is not a resolution of it. Zero keys against zero delivered
	// keys is not that - it is a continuation's claim that the CHECKPOINT it
	// inherited (#379), which carried no feedback obligation at all, needs no
	// further change. The length check just below refuses every other
	// mismatch between what was claimed and what was actually delivered.
	if len(result.Keys) == 0 && len(deliveredKeys) > 0 {
		return nil, fmt.Errorf("a no-change resolution names no feedback key, so it discharges nothing")
	}
	if len(result.Keys) > maxFeedbackResolutionKeys {
		return nil, fmt.Errorf("the resolution names %d keys, above the %d key bound", len(result.Keys), maxFeedbackResolutionKeys)
	}
	claimed := make([]string, 0, len(result.Keys))
	seen := map[string]bool{}
	for _, key := range result.Keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		claimed = append(claimed, key)
	}
	delivered := map[string]bool{}
	for _, key := range deliveredKeys {
		delivered[key] = true
	}
	if len(claimed) != len(deliveredKeys) {
		return nil, fmt.Errorf("the resolution names %d key(s) and this invocation was delivered %d: a no-change resolution must name exactly what it was given", len(claimed), len(deliveredKeys))
	}
	for _, key := range claimed {
		if !delivered[key] {
			return nil, fmt.Errorf("the resolution names feedback key %q, which this invocation was never delivered", key)
		}
	}
	sort.Strings(claimed)
	return claimed, nil
}
