// Package orchestration owns the semantics of basic explicit orchestration
// (#470): which explicit issues belong to one batch, what a batch item's state
// is as a projection over its child EngineeringRun, and the typed handoff a
// worker transfers to whatever comes next.
//
// It is the functional core. It imports no runtime, store, forge or provider
// package, so nothing here can branch on which execution agent did the work,
// and nothing here can own a lease, an attempt, a candidate or an authority
// decision - those stay with the runtime that already owns them. The runtime is
// the imperative shell that reads files, journals events and persists records.
package orchestration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// HandoffSchemaVersion is the protocol version a worker must state. An
// unrecognized version is refused rather than interpreted.
const HandoffSchemaVersion = "0.1"

// The two outcomes a worker may report. There is no default: an absent or
// unrecognized outcome is not a handoff.
const (
	// OutcomeCompleted states the objective was addressed and names nothing
	// unresolved.
	OutcomeCompleted = "completed"
	// OutcomePartial states the worker finished its invocation with named
	// work still unresolved.
	OutcomePartial = "partial"
)

// Bounds on a worker-written report. They are refusals, never truncations: a
// report this build would have to cut is not the report the worker wrote.
const (
	// MaxHandoffReportBytes bounds the file the runtime will read at all.
	MaxHandoffReportBytes = 32 << 10
	maxSummaryBytes       = 4 << 10
	maxHandoffListItems   = 16
	maxHandoffItemBytes   = 1 << 10
)

// HandoffReport is the small semantic document a worker writes through the
// runtime-owned result slot.
//
// It deliberately carries no run id, issue, base, candidate, tree, changed
// paths, contract or agent identity. The runtime already knows every one of
// those, and asking the worker to restate them would create a second answer
// to a question that has one. The strict decoder refuses such members as
// unknown, so a worker cannot even attempt to supply them.
type HandoffReport struct {
	SchemaVersion   string   `json:"schema_version"`
	Outcome         string   `json:"outcome"`
	Summary         string   `json:"summary"`
	Unresolved      []string `json:"unresolved,omitempty"`
	RecommendedNext []string `json:"recommended_next,omitempty"`
}

// DecodeHandoffReport strictly decodes and validates one report document:
// exactly one JSON object, no unknown member, no trailing data, within every
// bound, and internally consistent.
func DecodeHandoffReport(document []byte) (HandoffReport, error) {
	if len(document) > MaxHandoffReportBytes {
		return HandoffReport{}, fmt.Errorf("handoff report is %d bytes, above the %d byte bound", len(document), MaxHandoffReportBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var report HandoffReport
	if err := decoder.Decode(&report); err != nil {
		return HandoffReport{}, fmt.Errorf("handoff report is not a valid document: %w", err)
	}
	if len(bytes.TrimSpace(document[decoder.InputOffset():])) != 0 {
		return HandoffReport{}, errors.New("handoff report carries trailing data after its JSON value")
	}
	if err := report.Validate(); err != nil {
		return HandoffReport{}, err
	}
	return report, nil
}

// Validate refuses a report that is not one answer this build can act on.
func (r HandoffReport) Validate() error {
	if r.SchemaVersion != HandoffSchemaVersion {
		return fmt.Errorf("handoff schema version %q is not %q", r.SchemaVersion, HandoffSchemaVersion)
	}
	if err := boundedText("summary", r.Summary, maxSummaryBytes); err != nil {
		return err
	}
	if err := boundedItems("unresolved", r.Unresolved); err != nil {
		return err
	}
	if err := boundedItems("recommended_next", r.RecommendedNext); err != nil {
		return err
	}
	switch r.Outcome {
	case OutcomeCompleted:
		// COMPLETED AND UNRESOLVED ARE TWO ANSWERS. A downstream consumer that
		// read only the outcome would inherit the more permissive half.
		if len(r.Unresolved) > 0 {
			return fmt.Errorf("a %q handoff names %d unresolved item(s): completed and unresolved are different answers", OutcomeCompleted, len(r.Unresolved))
		}
	case OutcomePartial:
		if len(r.Unresolved) == 0 {
			return fmt.Errorf("a %q handoff names nothing unresolved, so it says nothing a consumer could act on", OutcomePartial)
		}
	default:
		return fmt.Errorf("handoff outcome %q must be %q or %q", r.Outcome, OutcomeCompleted, OutcomePartial)
	}
	return nil
}

func boundedText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("handoff %s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("handoff %s is %d bytes, above the %d byte bound", name, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("handoff %s is not valid UTF-8", name)
	}
	return nil
}

func boundedItems(name string, items []string) error {
	if len(items) > maxHandoffListItems {
		return fmt.Errorf("handoff %s names %d items, above the %d item bound", name, len(items), maxHandoffListItems)
	}
	for _, item := range items {
		if err := boundedText(name+" item", item, maxHandoffItemBytes); err != nil {
			return err
		}
	}
	return nil
}

// EngineeringHandoffSchemaVersion versions the durable admitted record.
const EngineeringHandoffSchemaVersion = "0.1"

// EngineeringHandoff is the durable, immutable record the runtime admits for
// one finished worker invocation of one batch item.
//
// Everything except ProducerReport is RUNTIME-OWNED: read from the run's own
// journal, operation records and frozen binding, never from the worker. The
// producer report stays exactly what it is - a claim - and nothing here turns
// it into evidence or authority.
type EngineeringHandoff struct {
	SchemaVersion string            `json:"schema_version"`
	ID            string            `json:"id"`
	BatchID       string            `json:"batch_id"`
	Issue         int               `json:"issue"`
	RunID         string            `json:"run_id"`
	Producer      HandoffProducer   `json:"producer"`
	Subject       HandoffSubject    `json:"subject"`
	Governance    HandoffGovernance `json:"governance"`
	Observed      HandoffObserved   `json:"observed"`
	// ReportSHA256 is the digest of the exact document the worker wrote, as
	// journalled when the invocation completed. Admission re-reads the
	// document and refuses one that no longer has this digest.
	ReportSHA256   string        `json:"report_sha256"`
	ProducerReport HandoffReport `json:"producer_report"`
	AdmittedAt     time.Time     `json:"admitted_at"`
}

// HandoffProducer is which worker invocation produced the report.
type HandoffProducer struct {
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
}

// HandoffSubject is the exact output the runtime observed and committed.
type HandoffSubject struct {
	BaseRevision      string `json:"base_revision"`
	CandidateRevision string `json:"candidate_revision"`
	CandidateTree     string `json:"candidate_tree"`
}

// HandoffGovernance is the contract the committed candidate was reassessed
// under.
type HandoffGovernance struct {
	ContractID       string `json:"contract_id"`
	ContractRevision string `json:"contract_revision"`
}

// HandoffObserved is what the runtime itself recorded about the change.
type HandoffObserved struct {
	ChangedPathCount   int    `json:"changed_path_count"`
	ChangedPathsDigest string `json:"changed_paths_digest"`
}

// Validate refuses a record missing any runtime-owned binding. A handoff that
// cannot name exactly what it describes is not admitted.
func (h EngineeringHandoff) Validate() error {
	if h.SchemaVersion != EngineeringHandoffSchemaVersion {
		return fmt.Errorf("engineering handoff schema version %q is not %q", h.SchemaVersion, EngineeringHandoffSchemaVersion)
	}
	for name, value := range map[string]string{
		"id": h.ID, "batch_id": h.BatchID, "run_id": h.RunID,
		"producer.agent_id": h.Producer.AgentID, "producer.operation_id": h.Producer.OperationID,
		"subject.base_revision": h.Subject.BaseRevision, "subject.candidate_revision": h.Subject.CandidateRevision,
		"subject.candidate_tree": h.Subject.CandidateTree, "governance.contract_id": h.Governance.ContractID,
		"governance.contract_revision": h.Governance.ContractRevision, "report_sha256": h.ReportSHA256,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("engineering handoff %s is required", name)
		}
	}
	if h.Issue <= 0 || h.Producer.Attempt <= 0 || h.Observed.ChangedPathCount < 0 || h.AdmittedAt.IsZero() {
		return errors.New("engineering handoff issue, attempt, observed path count and admission time must be valid")
	}
	return h.ProducerReport.Validate()
}

// HandoffID is the identity of the handoff one exact worker invocation
// transfers. One invocation can be admitted at most once.
func HandoffID(runID, operationID string, attempt int) (string, error) {
	if runID == "" || operationID == "" || attempt <= 0 {
		return "", errors.New("a handoff identity needs the run, operation and physical attempt")
	}
	digest, err := domain.Digest(struct {
		Run       string `json:"run"`
		Operation string `json:"operation"`
		Attempt   int    `json:"attempt"`
	}{runID, operationID, attempt})
	if err != nil {
		return "", err
	}
	return "handoff-" + digest[:32], nil
}
