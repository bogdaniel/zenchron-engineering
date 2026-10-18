package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ResultSchemaVersion is 0.2: the existing 0.1 record shape plus additive
// members (instrumentation, validity, supervision, runtime, sources,
// content_digest). Fields whose value may be unknown are {value, unknown_reason}
// objects in 0.2. A 0.1 record is historical and immutable; it is never rewritten.
const ResultSchemaVersion = "0.2"

const (
	benchmarkName = "zenchron-leverage"
	// InstrumentationVersion identifies this measurement code, so a result says
	// which instrument produced it.
	InstrumentationVersion = "66-minimal-slice/1"
	// ProtocolPlaceholder is recorded when no frozen protocol digest was supplied.
	ProtocolPlaceholder = "unfrozen-placeholder"
)

// Outcome results. A rejected or failed outcome is as measurable as an
// accepted one; nothing here is rewarded for success.
const (
	OutcomeAccepted  = "accepted"
	OutcomeRejected  = "rejected"
	OutcomeFailed    = "failed"
	OutcomeAbandoned = "abandoned"
)

var outcomes = map[string]bool{OutcomeAccepted: true, OutcomeRejected: true, OutcomeFailed: true, OutcomeAbandoned: true}

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Outcome is the operator-stated outcome of the arm. The runtime's own
// candidate and PR revisions are in the runtime section; this is what the human
// concluded, and says so.
type Outcome struct {
	Result            string `json:"result"`
	Source            string `json:"source"`
	Reason            string `json:"reason,omitempty"`
	Artifact          string `json:"artifact,omitempty"`
	CandidateRevision string `json:"candidate_revision,omitempty"`
}

// Case identifies the task, as in the existing records.
type Case struct {
	ID         string `json:"id"`
	Class      string `json:"class,omitempty"`
	Source     string `json:"source,omitempty"`
	Acceptance string `json:"acceptance,omitempty"`
}

// Subject is the repository and frozen base.
type Subject struct {
	Repository   string `json:"repository"`
	BaseRevision string `json:"base_revision"`
}

// Harness keeps the existing record's honesty statement.
type Harness struct {
	Implemented bool   `json:"implemented"`
	Note        string `json:"note"`
}

// Instrumentation is the provenance of the measurement itself.
type Instrumentation struct {
	Version        string `json:"version"`
	Arm            Arm    `json:"arm"`
	BaseSHA        string `json:"base_sha"`
	ProtocolDigest string `json:"protocol_digest"`
	ProtocolFrozen bool   `json:"protocol_frozen"`
}

// Validity says whether the run is usable evidence. An invalid run is still
// emitted, with the reasons, rather than dropped.
type Validity struct {
	Valid   bool     `json:"valid"`
	Reasons []string `json:"reasons,omitempty"`
}

// Cost keeps the existing record's cost shape; it is known only when the runtime
// recorded a figure for every consumption. amount_micros has no currency
// because the runtime records none.
type Cost struct {
	Known        bool   `json:"known"`
	AmountMicros *int64 `json:"amount_micros"`
	Reason       string `json:"reason,omitempty"`
}

// Measured uses the existing member names where one exists.
type Measured struct {
	ChildRuns           Measure[int]     `json:"child_runs"`
	OperationAttempts   Measure[int]     `json:"operation_attempts_all_kinds"`
	ExecutionAttempts   Measure[int]     `json:"execution_attempts"`
	RuntimeWallSeconds  Measure[float64] `json:"wall_seconds_runtime_first_create_to_last_end"`
	ArmWallSeconds      Measure[float64] `json:"wall_seconds_arm_first_entry_to_last_entry"`
	SupervisionMinutes  Measure[float64] `json:"supervision_minutes"`
	UnattendedWaitMins  Measure[float64] `json:"unattended_wait_minutes"`
	ReviewCycles        Measure[int]     `json:"review_observations"`
	RemediationCycles   Measure[int]     `json:"review_continuations"`
	DecisionDeliveries  Measure[int]     `json:"decision_deliveries"`
	Tokens              Measure[int64]   `json:"tokens"`
	Cost                Cost             `json:"cost"`
	PeakExecConcurrency Measure[int]     `json:"peak_execution_concurrency"`
	ExecOverlapSeconds  Measure[float64] `json:"execution_overlap_seconds"`
}

// SourceRef points at raw evidence by path and content digest.
type SourceRef struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Interpretation keeps the existing record's explicit non-claim.
type Interpretation struct {
	LeverageClaim string   `json:"leverage_claim"`
	Note          string   `json:"note"`
	Limitations   []string `json:"limitations,omitempty"`
}

// Result is the derived benchmark record.
type Result struct {
	SchemaVersion   string           `json:"schema_version"`
	Benchmark       string           `json:"benchmark"`
	CorpusVersion   string           `json:"corpus_version"`
	Mode            Arm              `json:"mode"`
	RecordedAt      time.Time        `json:"recorded_at"`
	Synthetic       bool             `json:"synthetic,omitempty"`
	Harness         Harness          `json:"harness"`
	Case            Case             `json:"case"`
	Subject         Subject          `json:"subject"`
	Instrumentation Instrumentation  `json:"instrumentation"`
	Validity        Validity         `json:"validity"`
	Supervision     *Supervision     `json:"supervision,omitempty"`
	Runtime         *RuntimeEvidence `json:"runtime,omitempty"`
	Measured        Measured         `json:"measured"`
	Unmeasured      []string         `json:"unmeasured"`
	Outcome         Outcome          `json:"outcome"`
	Sources         []SourceRef      `json:"sources"`
	Interpretation  Interpretation   `json:"interpretation"`
	// ContentDigest is the sha256 of the canonical (JCS) record with this member
	// empty. It is the record's identity; the same content always has the same one.
	ContentDigest string `json:"content_digest"`
}

// Input is everything Build needs. The ledger and runtime evidence are already
// read; Build performs no I/O.
type Input struct {
	Arm            Arm
	Case           Case
	CorpusVersion  string
	Repository     string
	BaseSHA        string
	ProtocolDigest string
	Ledger         Ledger
	LedgerErr      error
	Runtime        *RuntimeEvidence
	Outcome        Outcome
	Sources        []SourceRef
	RecordedAt     time.Time
	Synthetic      bool
}

// Build derives the Result, assessing validity and sealing its digest.
func Build(in Input) (Result, error) {
	protocol, frozen := in.ProtocolDigest, in.ProtocolDigest != ""
	if !frozen {
		protocol = ProtocolPlaceholder
	}
	if in.CorpusVersion == "" {
		in.CorpusVersion = "unversioned-single-observation"
	}
	in.Outcome.Source = "operator_stated"
	sup, hasSup := in.Ledger.Summarize(in.Arm)
	r := Result{
		SchemaVersion: ResultSchemaVersion, Benchmark: benchmarkName, CorpusVersion: in.CorpusVersion,
		Mode: in.Arm, RecordedAt: in.RecordedAt.UTC(), Synthetic: in.Synthetic,
		Harness: Harness{Implemented: false, Note: "Minimal #66 measurement slice only: an operator activity log, a read-only runtime collector and this emitter. There is no corpus and no ratio is computed here; one paired observation proves nothing about leverage."},
		Case:    in.Case, Subject: Subject{Repository: in.Repository, BaseRevision: in.BaseSHA},
		Instrumentation: Instrumentation{Version: InstrumentationVersion, Arm: in.Arm, BaseSHA: in.BaseSHA,
			ProtocolDigest: protocol, ProtocolFrozen: frozen},
		Runtime: in.Runtime, Outcome: in.Outcome, Sources: in.Sources,
		Interpretation: Interpretation{LeverageClaim: "none",
			Note: "A single arm is a single observation. No ratio is computed here and none may be inferred from this file."},
	}
	if hasSup {
		r.Supervision = &sup
	}
	r.Validity = assess(in, sup, hasSup)
	r.Measured = measure(in, sup, hasSup)
	r.Unmeasured = unmeasured(in, frozen)
	r.Interpretation.Limitations = limitations(in)
	digest, err := Digest(r)
	if err != nil {
		return Result{}, err
	}
	r.ContentDigest = digest
	return r, nil
}

// Digest is the identity of a record's content: sha256 of its JCS form with
// content_digest emptied.
func Digest(r Result) (string, error) {
	r.ContentDigest = ""
	canonical, err := domain.CanonicalJSON(r)
	if err != nil {
		return "", fmt.Errorf("canonicalize result: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func assess(in Input, sup Supervision, hasSup bool) Validity {
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }
	if in.Arm != ArmPlanned && in.Arm != ArmDirect {
		add("unknown arm %q", in.Arm)
	}
	if in.LedgerErr != nil {
		add("activity log is invalid: %s", flatten(in.LedgerErr))
	}
	switch {
	case !hasSup:
		add("the activity log has no entries for arm %s: human attention is unmeasured, not zero", in.Arm)
	case sup.OpenIntervals > 0:
		add("%d activity interval(s) were never stopped", sup.OpenIntervals)
	}
	if !revisionPattern.MatchString(in.BaseSHA) {
		add("base revision %q is not a 40-hex commit id", in.BaseSHA)
	}
	if !outcomes[in.Outcome.Result] {
		add("no valid outcome was recorded (accepted, rejected, failed or abandoned)")
	}
	reasons = append(reasons, runtimeReasons(in)...)
	return Validity{Valid: len(reasons) == 0, Reasons: reasons}
}

func runtimeReasons(in Input) []string {
	switch {
	case in.Arm == ArmDirect && in.Runtime != nil:
		return []string{"a direct_agent result must not carry Zenchron runtime evidence"}
	case in.Arm != ArmPlanned:
		return nil
	case in.Runtime == nil:
		return []string{"no runtime evidence was collected for a zenchron_planned arm"}
	}
	reasons := append([]string(nil), in.Runtime.Problems...)
	if len(in.Runtime.Runs) == 0 {
		reasons = append(reasons, "the runtime scope contained no readable runs")
	}
	return reasons
}

func measure(in Input, sup Supervision, hasSup bool) Measured {
	m := Measured{
		ChildRuns:           Unknown[int]("no runtime evidence for this arm"),
		OperationAttempts:   Unknown[int]("no runtime evidence for this arm"),
		RuntimeWallSeconds:  Unknown[float64]("no runtime evidence for this arm"),
		ReviewCycles:        Unknown[int]("no runtime evidence for this arm"),
		RemediationCycles:   Unknown[int]("no runtime evidence for this arm"),
		DecisionDeliveries:  Unknown[int]("no runtime evidence for this arm"),
		PeakExecConcurrency: Unknown[int]("no runtime evidence for this arm"),
		ExecOverlapSeconds:  Unknown[float64]("no runtime evidence for this arm"),
		ExecutionAttempts:   Unknown[int]("no runtime evidence for this arm"),
		Tokens:              Unknown[int64]("direct_agent arm: provider usage is not instrumented"),
		Cost:                Cost{Reason: "direct_agent arm: provider cost is not instrumented; unknown is not zero"},
		SupervisionMinutes:  Unknown[float64]("no activity log entries for this arm"),
		UnattendedWaitMins:  Unknown[float64]("no activity log entries for this arm"),
		ArmWallSeconds:      Unknown[float64]("no completed activity log entries for this arm"),
	}
	if hasSup {
		m.SupervisionMinutes = Known(sup.ActiveHumanSeconds / 60)
		m.UnattendedWaitMins = Known(sup.UnattendedWaitSeconds / 60)
		if sup.FirstStart != nil && sup.LastEnd != nil {
			m.ArmWallSeconds = Known(sup.LastEnd.Sub(*sup.FirstStart).Seconds())
		}
	}
	if in.Runtime != nil {
		runtimeMeasures(&m, in.Runtime)
	}
	return m
}

func runtimeMeasures(m *Measured, rt *RuntimeEvidence) {
	var attempts, execAttempts, reviews, continuations, decisions int
	var first, last *time.Time
	allTerminal := true
	for _, run := range rt.Runs {
		attempts += len(run.Operations)
		execAttempts += run.ExecutionAttempts
		reviews += len(run.Reviews)
		continuations += run.ReviewContinuations
		decisions += run.DecisionDeliveries
		allTerminal = allTerminal && run.EndedAt != nil
		if first == nil || run.CreatedAt.Before(*first) {
			created := run.CreatedAt
			first = &created
		}
		if run.EndedAt != nil && (last == nil || run.EndedAt.After(*last)) {
			last = run.EndedAt
		}
	}
	m.ChildRuns, m.OperationAttempts, m.ExecutionAttempts = Known(len(rt.Runs)), Known(attempts), Known(execAttempts)
	m.ReviewCycles, m.RemediationCycles, m.DecisionDeliveries = Known(reviews), Known(continuations), Known(decisions)
	m.PeakExecConcurrency = Unknown[int](rt.Execution.UnknownReason)
	m.ExecOverlapSeconds = Unknown[float64](rt.Execution.UnknownReason)
	if rt.Execution.Established {
		m.PeakExecConcurrency, m.ExecOverlapSeconds = Known(rt.Execution.Peak), Known(rt.Execution.OverlapSeconds)
	}
	m.Tokens = rt.Tokens
	m.Cost = Cost{Known: rt.CostMicros.IsKnown(), AmountMicros: rt.CostMicros.Value, Reason: rt.CostMicros.UnknownReason}
	switch {
	case first == nil || last == nil || !allTerminal:
		m.RuntimeWallSeconds = Unknown[float64]("at least one run was not terminal at collection")
	default:
		m.RuntimeWallSeconds = Known(last.Sub(*first).Seconds())
	}
}

func unmeasured(in Input, frozen bool) []string {
	out := []string{
		"relay completeness: a relay count is self_reported unless an audit record is attached; a count of zero can only be self_reported",
		"leverage ratio: needs both arms and a corpus; none is computed",
	}
	if !frozen {
		out = append(out, "protocol: no frozen protocol digest was supplied, so the protocol is not bound to this record")
	}
	if in.Arm == ArmDirect {
		out = append(out, "tokens and cost: the direct_agent arm has no Zenchron runtime to read them from")
	}
	return out
}

func limitations(in Input) []string {
	out := []string{"n=1: one paired observation is not a representative result",
		"order effect: the second arm benefits from the operator having already learned the task; this is not removed by measurement"}
	if in.Runtime != nil {
		out = append(out, in.Runtime.Limitations...)
	}
	return out
}

func flatten(err error) string { return strings.ReplaceAll(err.Error(), "\n", "; ") }
