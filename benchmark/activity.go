package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Arm names the benchmark mode a log entry belongs to (#66 baseline modes).
// Only the two arms of the first paired measurement exist here;
// zenchron_explicit is a different cohort and must not be mislabelled.
type Arm string

const (
	ArmPlanned Arm = "zenchron_planned"
	ArmDirect  Arm = "direct_agent"
)

// Kind classifies one interval of the operator's time.
type Kind string

const (
	KindActiveSupervision    Kind = "active_supervision"
	KindTaskPreparation      Kind = "task_preparation"
	KindReviewingOutput      Kind = "reviewing_output"
	KindAuthorityIntervened  Kind = "authority_intervention"
	KindManualRelay          Kind = "manual_relay"
	KindRecoveryWorkaround   Kind = "recovery_workaround"
	KindUnattendedWait       Kind = "unattended_wait"
	KindFinalAcceptanceMerge Kind = "final_acceptance_merge"
)

var kinds = map[Kind]bool{
	KindActiveSupervision: true, KindTaskPreparation: true, KindReviewingOutput: true,
	KindAuthorityIntervened: true, KindManualRelay: true, KindRecoveryWorkaround: true,
	KindUnattendedWait: true, KindFinalAcceptanceMerge: true,
}

// CountsAsHumanAttention is false only for unattended waiting: provider or CI
// time during which the human is not attending is never supervision time.
func (k Kind) CountsAsHumanAttention() bool { return k != KindUnattendedWait }

// RelayKind is the sub-kind of a manual relay: what was carried between agents.
type RelayKind string

const (
	RelayPrompt  RelayKind = "prompt"
	RelayContext RelayKind = "context"
	RelayFinding RelayKind = "finding"
	RelayStatus  RelayKind = "status"
)

var relayKinds = map[RelayKind]bool{RelayPrompt: true, RelayContext: true, RelayFinding: true, RelayStatus: true}

// Record operations. A log is a sequence of these lines; intervals are start/stop
// pairs, and an audit line attaches independent verification to a relay.
const (
	OpStart = "start"
	OpStop  = "stop"
	OpAudit = "audit"
)

// ActivitySchemaVersion versions one log line.
const ActivitySchemaVersion = "0.1"

const maxTextLength = 256

// Record is one JSONL line of the activity log.
type Record struct {
	SchemaVersion string    `json:"schema_version"`
	Op            string    `json:"op"`
	EntryID       string    `json:"entry_id"`
	At            time.Time `json:"at"`
	// start only
	Arm       Arm       `json:"arm,omitempty"`
	Operator  string    `json:"operator,omitempty"`
	Kind      Kind      `json:"kind,omitempty"`
	RelayKind RelayKind `json:"relay_kind,omitempty"`
	Note      string    `json:"note,omitempty"`
	// audit only
	Auditor        string `json:"auditor,omitempty"`
	EvidenceSHA256 string `json:"evidence_sha256,omitempty"`
}

// Audit is independent verification of a relay entry. A relay count is
// "audited" only for entries carrying one.
type Audit struct {
	Auditor        string    `json:"auditor"`
	EvidenceSHA256 string    `json:"evidence_sha256"`
	At             time.Time `json:"at"`
}

// Interval is one folded start/stop pair. End is nil while still open.
type Interval struct {
	ID        string     `json:"id"`
	Arm       Arm        `json:"arm"`
	Operator  string     `json:"operator"`
	Kind      Kind       `json:"kind"`
	RelayKind RelayKind  `json:"relay_kind,omitempty"`
	Note      string     `json:"note,omitempty"`
	Start     time.Time  `json:"start"`
	End       *time.Time `json:"end,omitempty"`
	Audit     *Audit     `json:"audit,omitempty"`
}

// Duration is zero for an open interval.
func (i Interval) Duration() time.Duration {
	if i.End == nil {
		return 0
	}
	return i.End.Sub(i.Start)
}

// EntryID derives an entry's identity from its content, so the same interval
// recorded twice is the same id and is refused as a duplicate.
func EntryID(arm Arm, operator string, kind Kind, relay RelayKind, start time.Time) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{string(arm), operator, string(kind), string(relay), start.UTC().Format(time.RFC3339Nano)}, "\x00")))
	return "act-" + hex.EncodeToString(sum[:8])
}

// Ledger is a validated log: intervals ordered by start time.
type Ledger struct {
	Intervals []Interval
}

// Fold validates records and folds them into a Ledger. It returns every
// violation found, joined, so an incomplete or inconsistent log is reported in
// full rather than at its first fault.
//
// Cross-entry rules: an entry id appears once; a stop follows its start
// strictly (zero and negative intervals are refused); intervals never overlap
// (one human has one attention stream, and unattended waiting is logged as its
// own interval); and one operator performs every arm.
func Fold(records []Record) (Ledger, error) {
	var problems []error
	byID := map[string]*Interval{}
	var order []string
	for n, r := range records {
		if err := foldOne(n+1, r, byID, &order); err != nil {
			problems = append(problems, err)
		}
	}
	ledger := Ledger{}
	for _, id := range order {
		ledger.Intervals = append(ledger.Intervals, *byID[id])
	}
	sort.SliceStable(ledger.Intervals, func(a, b int) bool { return ledger.Intervals[a].Start.Before(ledger.Intervals[b].Start) })
	problems = append(problems, overlaps(ledger.Intervals)...)
	problems = append(problems, operatorMismatch(ledger.Intervals)...)
	return ledger, errors.Join(problems...)
}

func foldOne(line int, r Record, byID map[string]*Interval, order *[]string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("log line %d: "+format, append([]any{line}, args...)...)
	}
	if r.SchemaVersion != ActivitySchemaVersion {
		return fail("unsupported schema_version %q", r.SchemaVersion)
	}
	if r.At.IsZero() {
		return fail("a timestamp is required")
	}
	switch r.Op {
	case OpStart:
		return foldStart(fail, r, byID, order)
	case OpStop:
		return foldStop(fail, r, byID)
	case OpAudit:
		return foldAudit(fail, r, byID)
	}
	return fail("unknown op %q", r.Op)
}

func foldStart(fail func(string, ...any) error, r Record, byID map[string]*Interval, order *[]string) error {
	switch {
	case r.Arm != ArmPlanned && r.Arm != ArmDirect:
		return fail("unknown arm %q", r.Arm)
	case strings.TrimSpace(r.Operator) == "" || len(r.Operator) > maxTextLength:
		return fail("an operator id is required")
	case !kinds[r.Kind]:
		return fail("invalid kind %q", r.Kind)
	case r.Kind == KindManualRelay && !relayKinds[r.RelayKind]:
		return fail("a manual_relay needs a relay_kind of prompt, context, finding or status")
	case r.Kind != KindManualRelay && r.RelayKind != "":
		return fail("relay_kind is only valid on a manual_relay")
	case len(r.Note) > maxTextLength:
		return fail("note exceeds %d bytes", maxTextLength)
	case r.EntryID != EntryID(r.Arm, r.Operator, r.Kind, r.RelayKind, r.At):
		return fail("entry id %q does not match the entry's content", r.EntryID)
	}
	if _, dup := byID[r.EntryID]; dup {
		return fail("duplicate entry %q", r.EntryID)
	}
	byID[r.EntryID] = &Interval{ID: r.EntryID, Arm: r.Arm, Operator: r.Operator, Kind: r.Kind,
		RelayKind: r.RelayKind, Note: r.Note, Start: r.At}
	*order = append(*order, r.EntryID)
	return nil
}

func foldStop(fail func(string, ...any) error, r Record, byID map[string]*Interval) error {
	in, ok := byID[r.EntryID]
	switch {
	case !ok:
		return fail("stop names unknown entry %q", r.EntryID)
	case in.End != nil:
		return fail("entry %q is already stopped", r.EntryID)
	case !r.At.After(in.Start):
		return fail("entry %q must end after it starts (zero or negative interval)", r.EntryID)
	}
	end := r.At
	in.End = &end
	return nil
}

func foldAudit(fail func(string, ...any) error, r Record, byID map[string]*Interval) error {
	in, ok := byID[r.EntryID]
	switch {
	case !ok:
		return fail("audit names unknown entry %q", r.EntryID)
	case in.Kind != KindManualRelay:
		return fail("only a manual_relay entry can be audited")
	case in.Audit != nil:
		return fail("entry %q is already audited", r.EntryID)
	case strings.TrimSpace(r.Auditor) == "" || len(r.Auditor) > maxTextLength:
		return fail("an auditor id is required")
	case r.Auditor == in.Operator:
		return fail("the auditor must not be the operator who performed the work")
	case !isSHA256(r.EvidenceSHA256):
		return fail("audit evidence must be a sha256 digest")
	}
	in.Audit = &Audit{Auditor: r.Auditor, EvidenceSHA256: r.EvidenceSHA256, At: r.At}
	return nil
}

// overlaps expects intervals sorted by start. An open interval extends
// indefinitely, so anything after it overlaps it. Touching intervals do not.
func overlaps(sorted []Interval) []error {
	var out []error
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if prev.End == nil || cur.Start.Before(*prev.End) {
			out = append(out, fmt.Errorf("entries %s and %s overlap", prev.ID, cur.ID))
		}
	}
	return out
}

func operatorMismatch(intervals []Interval) []error {
	if len(intervals) == 0 {
		return nil
	}
	first := intervals[0]
	var out []error
	for _, in := range intervals[1:] {
		if in.Operator != first.Operator {
			out = append(out, fmt.Errorf("entry %s (arm %s) is by operator %q, but the log's first entry (arm %s) is by %q: one human must operate every arm",
				in.ID, in.Arm, in.Operator, first.Arm, first.Operator))
		}
	}
	return out
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
