package runtime

// The orchestration handoff channel (#470).
//
// An orchestrated worker that finishes is not finished merely because its
// provider exited: what it transfers to whatever comes next has to be STATED,
// through a typed document, and never inferred from transcript prose. This
// file is that channel, built exactly the way the reviewer verdict channel
// (reviewer_result.go) and the feedback resolution channel are:
//
//   - the slot is a runtime-owned file OUTSIDE the candidate workspace, named
//     by the exact physical attempt identity, so repository content cannot
//     pre-seed or predict it and no other attempt's document can be read as
//     this one's;
//   - the runtime empties it before the invocation starts;
//   - it is read by the RUNTIME after the provider returns, not by the
//     provider adapter, so any execution provider that can write a file can
//     carry it and none needs to know what it means;
//   - it is read only for an invocation the runtime has already decided
//     COMPLETED, so a cancelled, failed, deadline-revoked or otherwise
//     unfinished invocation contributes no handoff;
//   - the read is bounded, the decode strict, and the worker never restates a
//     fact the runtime owns (orchestration.HandoffReport has no member for one).
//
// What is journalled is an OBSERVATION about the attempt: the report's digest
// and outcome, or why there was none. The durable handoff that binds the
// report to the exact candidate the runtime committed is admitted afterwards,
// from the runtime's own records (orchestration_handoff.go).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// handoffReportFile is the basename of the runtime-owned handoff slot.
const handoffReportFile = "handoff.json"

// The two reasons a finished invocation transferred no admissible handoff.
const (
	HandoffMissing = "missing"
	HandoffInvalid = "invalid"
)

// HandoffReportedPayload records that a finished invocation wrote a valid
// handoff report. The report itself stays in its slot, beside the attempt's
// transcripts; the digest is what admission later checks it against.
type HandoffReportedPayload struct {
	OperationID  string `json:"operation_id"`
	Attempt      int    `json:"attempt"`
	Outcome      string `json:"outcome"`
	ReportSHA256 string `json:"report_sha256"`
}

// HandoffRefusedPayload records that a finished invocation transferred no
// admissible handoff, and why.
type HandoffRefusedPayload struct {
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	Kind        string `json:"kind"`
	Detail      string `json:"detail"`
}

// HandoffReportPath is the runtime-owned location for ONE invocation's
// handoff. It shares the producer's typed-result directory with the feedback
// resolution slot, because a sandboxed CLI is granted exactly one result
// directory and an orchestrated remediation may be given both slots.
func HandoffReportPath(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	return producerResultPath(stateDir, attempt, handoffReportFile)
}

// prepareHandoffSlot clears and returns this invocation's handoff slot, or ""
// for a run no orchestration batch created: such a run is given no path, so
// it has nowhere to write a handoff and nothing about it changes.
func (r *EngineeringRuntime) prepareHandoffSlot(state *runState, operationID string, attempt int) (string, error) {
	if state.run.Orchestration == nil {
		return "", nil
	}
	path, err := HandoffReportPath(r.deps.StateDir, ExecutionAttemptRef{
		RunID: state.run.ID, OperationID: operationID, Attempt: attempt,
	})
	if err != nil {
		return "", err
	}
	return path, clearResultSlot(path)
}

// readHandoffReport reads one handoff slot. present=false is an absent file,
// which is not an error and not a handoff. A slot holding anything but a
// regular file - a directory, or a link the worker planted to make the
// runtime read something else - is refused rather than followed.
func readHandoffReport(path string) (report orchestration.HandoffReport, digest string, present bool, err error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return orchestration.HandoffReport{}, "", false, nil
	}
	if err != nil {
		return orchestration.HandoffReport{}, "", true, err
	}
	if !info.Mode().IsRegular() {
		return orchestration.HandoffReport{}, "", true, fmt.Errorf("the handoff slot holds a %s, not a regular file", info.Mode().Type())
	}
	file, err := os.Open(path)
	if err != nil {
		return orchestration.HandoffReport{}, "", true, err
	}
	defer file.Close()
	// One byte past the bound is read so an oversized document is refused by
	// its size rather than mistaken for a truncated one.
	document, err := io.ReadAll(io.LimitReader(file, orchestration.MaxHandoffReportBytes+1))
	if err != nil {
		return orchestration.HandoffReport{}, "", true, err
	}
	sum := sha256.Sum256(document)
	report, err = orchestration.DecodeHandoffReport(document)
	return report, hex.EncodeToString(sum[:]), true, err
}

// handoffObservation is the journal entry for what one COMPLETED invocation
// transferred. It is never called for an invocation that did not complete.
func handoffObservation(path, operationID string, attempt int) journalEntry {
	report, digest, present, err := readHandoffReport(path)
	switch {
	case !present:
		return journalEntry{Type: EventHandoffRefused, Payload: HandoffRefusedPayload{
			OperationID: operationID, Attempt: attempt, Kind: HandoffMissing,
			Detail: "the invocation completed without writing a handoff report",
		}}
	case err != nil:
		return journalEntry{Type: EventHandoffRefused, Payload: HandoffRefusedPayload{
			OperationID: operationID, Attempt: attempt, Kind: HandoffInvalid,
			Detail: boundedDetail(err.Error()),
		}}
	}
	return journalEntry{Type: EventHandoffReported, Payload: HandoffReportedPayload{
		OperationID: operationID, Attempt: attempt, Outcome: report.Outcome, ReportSHA256: digest,
	}}
}

// handoffEnvelope states the REQUIRED OUTPUT of an orchestrated invocation.
// The example document is marshalled from the protocol type itself, so the
// member names a worker is shown cannot drift from the ones the strict
// decoder accepts.
func handoffEnvelope(r ExecutionRequest) string {
	if r.HandoffPath == "" {
		return ""
	}
	example, _ := json.Marshal(orchestration.HandoffReport{
		SchemaVersion: orchestration.HandoffSchemaVersion, Outcome: orchestration.OutcomeCompleted,
		Summary:         "what you changed and why, for the next engineer",
		RecommendedNext: []string{"an optional follow-up worth doing next"},
	})
	return fmt.Sprintf(
		"\n\nREQUIRED HANDOFF. This work is part of an orchestration batch, and it is not complete until you write a handoff. "+
			"Before you finish, write a JSON document to %s shaped exactly like %s. "+
			"outcome is %q when the objective is addressed (then name nothing in unresolved), or %q with an unresolved array naming what is not done. "+
			"summary is a short account of the change for whoever continues this work; unresolved and recommended_next are optional arrays of short strings. "+
			"Write no other member: do not restate the run, issue, branch, commit, tree, changed files or contract - the runtime records those itself. "+
			"Your final message is not read for this; only that file is.",
		r.HandoffPath, example, orchestration.OutcomeCompleted, orchestration.OutcomePartial)
}
