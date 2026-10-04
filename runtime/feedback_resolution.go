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
// runtime-owned checkpoint is exactly as unproven as a remediation that left
// admitted feedback outstanding, WHETHER OR NOT it went on to mutate the
// candidate further. Mutation proves work happened; it does not prove the
// inherited work is finished - a continuation that addresses half of what a
// checkpoint owed and then defers the rest to background work it never
// finishes returns exactly as "successfully" as one that changed nothing at
// all. So a continuation's completion claim is a SEPARATE resolution value,
// FeedbackResolutionCheckpointComplete, admitted independently of
// record.Mutated and bound to BOTH the exact checkpoint revision and tree the
// invocation was shown - never reused from the "no change was needed" value,
// which would misstate a claim made after further mutation.

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

// FeedbackResolutionNoChangeRequired states that the admitted feedback named
// by Keys requires no change to the exact Subject revision. It is admitted
// only when the workspace agrees nothing changed (see AdmitFeedbackResolution)
// and is never used by a continuation invocation (#379): a continuation's
// completion claim is FeedbackResolutionCheckpointComplete, whatever it
// mutated.
const FeedbackResolutionNoChangeRequired = "no_change_required"

// FeedbackResolutionCheckpointComplete states that a continuation's inherited
// checkpoint - including any admitted feedback named by Keys - is fully
// complete, independent of whether this invocation mutated the candidate
// further. It is the one way a continuation may settle the checkpoint it
// inherited (#379); see AdmitCheckpointCompletion.
const FeedbackResolutionCheckpointComplete = "checkpoint_complete"

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
	// Tree restates the exact candidate tree this invocation was shown. It is
	// required, and independently admitted, only for a
	// FeedbackResolutionCheckpointComplete claim (#379): a commit names a tree,
	// but admitting the claim against the commit alone would let it be read as
	// binding to any tree that commit could name rather than the one the
	// runtime actually invoked against.
	Tree string `json:"tree,omitempty"`
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
	return producerResultPath(stateDir, attempt, feedbackResolutionFile)
}

// producerResultPath is where a PRODUCER invocation's typed results live: one
// runtime-owned directory per attempt, outside the candidate workspace. Every
// producer slot (feedback resolution, orchestration handoff) is named here so
// they share that one directory, which is the only one a sandboxed CLI is
// granted (cli_agent.go ResultDir).
func producerResultPath(stateDir string, attempt ExecutionAttemptRef, file string) (string, error) {
	if err := attempt.Validate(); err != nil {
		return "", err
	}
	prefix, err := attemptTranscriptPrefix("remediation", attempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "artifacts", prefix+"."+file), nil
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
	return path, clearResultSlot(path)
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
// discharge feedback with a FeedbackResolutionNoChangeRequired claim, and
// returns the exact keys it admits.
//
// A refusal is never promoted to an operation failure: a claim that does not
// bind is treated exactly like no claim at all, and the keys it would have
// discharged stay outstanding. That is deliberate - this function's failure
// mode IS the invariant #376 states, not an exception to it.
func AdmitFeedbackResolution(deliveredKeys []string, subject string, result *FeedbackResolution) ([]string, error) {
	return admitResolution(deliveredKeys, FeedbackResolutionNoChangeRequired, subject, "", false, result)
}

// AdmitCheckpointCompletion decides whether one structured resolution may
// settle the checkpoint a continuation inherited, and returns the exact
// feedback keys (if any) it also discharges (#379).
//
// It is the same fail-closed channel AdmitFeedbackResolution is, bound to a
// different claim and a stronger identity: a continuation's completion is
// admitted independently of whether it mutated the candidate further, and the
// claim must bind BOTH the exact revision and the exact tree this invocation
// was shown, checked independently, so it cannot be admitted against a
// different materialization of the same commit.
func AdmitCheckpointCompletion(deliveredKeys []string, subjectRevision, subjectTree string, result *FeedbackResolution) ([]string, error) {
	return admitResolution(deliveredKeys, FeedbackResolutionCheckpointComplete, subjectRevision, subjectTree, true, result)
}

// admitResolution is the shared fail-closed admission check behind
// AdmitFeedbackResolution and AdmitCheckpointCompletion: schema, resolution
// value, subject identity and the exact claimed key set must all bind, or
// nothing is admitted.
func admitResolution(deliveredKeys []string, want, subjectRevision, subjectTree string, requireTree bool, result *FeedbackResolution) ([]string, error) {
	if result == nil {
		return nil, nil
	}
	if result.SchemaVersion != FeedbackResolutionSchemaVersion {
		return nil, fmt.Errorf("feedback resolution schema version %q is not %q", result.SchemaVersion, FeedbackResolutionSchemaVersion)
	}
	if result.Resolution != want {
		return nil, fmt.Errorf("feedback resolution %q is not %q", result.Resolution, want)
	}
	if subjectRevision == "" || result.Subject != subjectRevision {
		return nil, fmt.Errorf("the resolution claims subject %s and the invocation was shown %s", short12(result.Subject), short12(subjectRevision))
	}
	// The tree is checked independently of the revision (#379): a commit names
	// a tree, but admitting the claim only against the commit would let it bind
	// without ever confirming which tree the invocation actually saw.
	if requireTree && (subjectTree == "" || result.Tree != subjectTree) {
		return nil, fmt.Errorf("the resolution claims tree %s and the invocation was shown %s", short12(result.Tree), short12(subjectTree))
	}
	// Naming zero keys is refused only when this invocation was actually
	// delivered feedback: a claim that discharges nothing while feedback sits
	// outstanding is not a resolution of it. Zero keys against zero delivered
	// keys is not that - it is a continuation's claim that the CHECKPOINT it
	// inherited (#379), which carried no feedback obligation at all, is
	// complete. The length check just below refuses every other mismatch
	// between what was claimed and what was actually delivered.
	if len(result.Keys) == 0 && len(deliveredKeys) > 0 {
		return nil, fmt.Errorf("a %s resolution names no feedback key, so it discharges nothing", want)
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
		return nil, fmt.Errorf("the resolution names %d key(s) and this invocation was delivered %d: a %s resolution must name exactly what it was given", len(claimed), len(deliveredKeys), want)
	}
	for _, key := range claimed {
		if !delivered[key] {
			return nil, fmt.Errorf("the resolution names feedback key %q, which this invocation was never delivered", key)
		}
	}
	sort.Strings(claimed)
	return claimed, nil
}
