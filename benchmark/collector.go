package benchmark

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// Collection bounds. A larger scope or journal is reported as truncated, which
// invalidates the result, rather than silently sampled.
const (
	maxRuns         = 64
	eventsPageSize  = 500
	maxEventsPerRun = 5000
	maxPlanEvents   = 5000
)

// RuntimeEvidenceSchemaVersion versions the raw runtime collection record.
const RuntimeEvidenceSchemaVersion = "0.1"

// RuntimeSource is the read-only port the collector needs. *runtime.ReadStore
// satisfies it: its SQLite handle is opened mode=ro with query_only, so the
// collector cannot write to, migrate or repair runtime state. Gate: external
// I/O boundary (database) that tests must fake.
type RuntimeSource interface {
	WorkGraphStatus(graphID string, now time.Time) (runtime.WorkGraphView, error)
	Status(runID string, now time.Time) (runtime.StatusReport, error)
	EventsPage(runID string, after int64, limit int) ([]runtime.EngineeringEvent, bool, error)
	PlanEvents(planID string) ([]runtime.EngineeringEvent, error)
}

// Scope selects what to collect: one WorkGraph (its units' runs), explicit run
// ids (the direct-agent arm has none), or both.
type Scope struct {
	GraphID string
	RunIDs  []string
}

// RuntimeEvidence is everything read from durable state, with nothing invented.
// Problems make the result invalid; Limitations are honest caveats that do not.
type RuntimeEvidence struct {
	SchemaVersion string         `json:"schema_version"`
	CollectedAt   time.Time      `json:"collected_at"`
	Graph         *GraphEvidence `json:"graph,omitempty"`
	Plan          *PlanEvidence  `json:"plan,omitempty"`
	Runs          []RunEvidence  `json:"runs"`
	RunSpans      Concurrency    `json:"run_concurrency"`
	OperationSpan Concurrency    `json:"operation_concurrency"`
	Tokens        Measure[int64] `json:"tokens"`
	CostMicros    Measure[int64] `json:"cost_micros"`
	Problems      []string       `json:"problems,omitempty"`
	Limitations   []string       `json:"limitations,omitempty"`
}

// GraphEvidence is the WorkGraph and its work units as the runtime projects them.
type GraphEvidence struct {
	ID             string         `json:"id"`
	Revision       int            `json:"revision"`
	RevisionDigest string         `json:"revision_digest"`
	ObjectivePlan  string         `json:"objective_plan,omitempty"`
	Units          []UnitEvidence `json:"units"`
}

// UnitEvidence is one work unit and its assignment.
type UnitEvidence struct {
	UnitID           string   `json:"unit_id"`
	Role             string   `json:"role"`
	Issue            int      `json:"issue"`
	DependsOn        []string `json:"depends_on,omitempty"`
	State            string   `json:"state"`
	Reason           string   `json:"reason,omitempty"`
	RunID            string   `json:"run_id,omitempty"`
	AwaitingDecision bool     `json:"awaiting_decision,omitempty"`
	HandoffID        string   `json:"handoff_id,omitempty"`
	OutputRevision   string   `json:"output_revision,omitempty"`
	RequiresReview   bool     `json:"requires_review,omitempty"`
	ReviewApproved   bool     `json:"review_approved,omitempty"`
}

// PlanEvidence is the plan stream's own facts. Budget cost is summed only when
// every consumption reported one.
type PlanEvidence struct {
	ID          string         `json:"id"`
	Revision    int            `json:"revision"`
	Digest      string         `json:"digest"`
	EventCounts map[string]int `json:"event_counts"`
}

// RunEvidence is one run's identity, assignment, timing and review history.
type RunEvidence struct {
	RunID             string           `json:"run_id"`
	Repository        string           `json:"repository"`
	Agent             string           `json:"agent,omitempty"`
	ProviderKind      string           `json:"provider_kind,omitempty"`
	TrustMode         string           `json:"trust_mode,omitempty"`
	Model             string           `json:"model,omitempty"`
	Phase             string           `json:"phase"`
	Disposition       string           `json:"disposition"`
	Reason            string           `json:"reason,omitempty"`
	Terminal          bool             `json:"terminal"`
	CreatedAt         time.Time        `json:"created_at"`
	EndedAt           *time.Time       `json:"ended_at,omitempty"`
	WallSeconds       Measure[float64] `json:"wall_seconds"`
	Branch            string           `json:"branch,omitempty"`
	CandidateRevision string           `json:"candidate_revision,omitempty"`
	CandidateTree     string           `json:"candidate_tree,omitempty"`
	PullRequest       *PullRequestRef  `json:"pull_request,omitempty"`
	Attempts          map[string]int   `json:"attempts,omitempty"`
	Operations        []OperationSpan  `json:"operations"`
	HandoffsReported  int              `json:"handoffs_reported"`
	HandoffsRefused   int              `json:"handoffs_refused"`
	Reviews           []ReviewSighting `json:"reviews"`
	// ReviewContinuations counts review.continuation_granted events: each is a
	// remediation invocation the runtime granted after a review.
	ReviewContinuations int `json:"review_continuations"`
	// DecisionDeliveries counts decision.delivered events: resolved decisions
	// that an attempt actually carried. It is not a count of questions asked.
	DecisionDeliveries    int  `json:"decision_deliveries"`
	HumanAuthorityRecords int  `json:"human_authority_records"`
	Events                int  `json:"events"`
	EventsTruncated       bool `json:"events_truncated,omitempty"`
}

// PullRequestRef is the exact PR/head the runtime observed.
type PullRequestRef struct {
	Number       int    `json:"number"`
	HeadRevision string `json:"head_revision"`
	State        string `json:"state"`
	Merged       bool   `json:"merged"`
}

// OperationSpan is one executed attempt of a bounded operation.
type OperationSpan struct {
	OperationID string    `json:"operation_id"`
	Kind        string    `json:"kind"`
	Attempt     int       `json:"attempt"`
	State       string    `json:"state"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}

// ReviewSighting is one journalled review observation of an exact head.
type ReviewSighting struct {
	At           time.Time `json:"at"`
	HeadRevision string    `json:"head_revision"`
	State        string    `json:"state"`
	FindingCount int       `json:"finding_count"`
}

const (
	tokensUnknown = "no durable per-run token record exists in runtime state"
	costUnknown   = "no plan stream consumption with a reported cost; a subscription CLI reports none, and unknown is not zero"
)

// Collect reads durable state through src. It never writes, never estimates and
// does not stop at a per-run read failure: it records the failure as a Problem
// so the emitted result is flagged invalid instead of silently smaller.
func Collect(src RuntimeSource, scope Scope, now time.Time) RuntimeEvidence {
	ev := RuntimeEvidence{SchemaVersion: RuntimeEvidenceSchemaVersion, CollectedAt: now.UTC(), Runs: []RunEvidence{},
		Tokens: Unknown[int64](tokensUnknown), CostMicros: Unknown[int64](costUnknown)}
	runIDs := append([]string(nil), scope.RunIDs...)
	var planID string
	if scope.GraphID != "" {
		view, err := src.WorkGraphStatus(scope.GraphID, now)
		if err != nil {
			ev.Problems = append(ev.Problems, fmt.Sprintf("work graph %q cannot be read: %v", scope.GraphID, err))
		} else {
			ev.Graph, planID = graphEvidence(view), ""
			if view.ObjectivePlan != nil {
				planID = view.ObjectivePlan.ID
				ev.Plan = &PlanEvidence{ID: planID, Revision: view.ObjectivePlan.Revision, Digest: view.ObjectivePlan.Digest}
			}
			for _, u := range view.Units {
				if u.RunID != "" {
					runIDs = append(runIDs, u.RunID)
				}
			}
		}
	}
	runIDs = dedupe(runIDs)
	if len(runIDs) > maxRuns {
		ev.Problems = append(ev.Problems, fmt.Sprintf("scope names %d runs; the collector reads at most %d", len(runIDs), maxRuns))
		runIDs = runIDs[:maxRuns]
	}
	var runSpans, opSpans []Span
	// ponytail: one Status plus paged journal read per run, sequential; fine at
	// the 64-run bound. Batch the reads if a cohort ever needs more.
	for _, id := range runIDs {
		run, ops, problem := collectRun(src, id, now)
		if problem != "" {
			ev.Problems = append(ev.Problems, problem)
			continue
		}
		ev.Runs = append(ev.Runs, run)
		if run.EndedAt != nil {
			runSpans = append(runSpans, Span{Label: id, Start: run.CreatedAt, End: *run.EndedAt})
		} else {
			ev.Limitations = append(ev.Limitations, fmt.Sprintf("run %s was not terminal at collection and is excluded from run concurrency", id))
		}
		if run.EventsTruncated {
			ev.Problems = append(ev.Problems, fmt.Sprintf("run %s has more than %d events; the journal read was truncated", id, maxEventsPerRun))
		}
		opSpans = append(opSpans, ops...)
	}
	ev.RunSpans, ev.OperationSpan = ComputeConcurrency(runSpans), ComputeConcurrency(opSpans)
	if planID != "" {
		collectPlan(src, &ev)
	}
	return ev
}

func graphEvidence(v runtime.WorkGraphView) *GraphEvidence {
	g := &GraphEvidence{ID: v.GraphID, Revision: v.Revision, RevisionDigest: v.RevisionDigest, Units: []UnitEvidence{}}
	if v.ObjectivePlan != nil {
		g.ObjectivePlan = fmt.Sprintf("%s@%d", v.ObjectivePlan.ID, v.ObjectivePlan.Revision)
	}
	for _, u := range v.Units {
		ue := UnitEvidence{UnitID: u.UnitID, Role: string(u.Role), Issue: u.Issue, DependsOn: u.DependsOn,
			State: string(u.State), Reason: u.Reason, RunID: u.RunID, AwaitingDecision: u.AwaitingDecision != nil,
			RequiresReview: u.RequiresReview, ReviewApproved: u.ReviewApproved}
		if u.Output != nil {
			ue.HandoffID, ue.OutputRevision = u.Output.HandoffID, u.Output.CandidateRevision
		}
		g.Units = append(g.Units, ue)
	}
	return g
}

func collectRun(src RuntimeSource, id string, now time.Time) (RunEvidence, []Span, string) {
	status, err := src.Status(id, now)
	if err != nil {
		return RunEvidence{}, nil, fmt.Sprintf("run %q cannot be read: %v", id, err)
	}
	run := RunEvidence{RunID: id, Repository: status.Repository, Agent: status.Worker.Agent,
		ProviderKind: status.Worker.ProviderKind, TrustMode: string(status.Worker.TrustMode), Model: status.Worker.Model,
		Phase: string(status.Phase), Disposition: string(status.Disposition), Reason: status.Reason,
		CreatedAt: status.CreatedAt.UTC(), Branch: status.Candidate.Branch, CandidateRevision: status.Candidate.Revision,
		CandidateTree: status.Candidate.Tree, Attempts: status.Attempts,
		Operations: []OperationSpan{}, Reviews: []ReviewSighting{},
		WallSeconds: Unknown[float64]("run was not terminal at collection")}
	if pr := status.PullRequest; pr != nil {
		run.PullRequest = &PullRequestRef{Number: pr.Number, HeadRevision: pr.HeadRevision, State: pr.State, Merged: pr.Merged}
	}
	events, truncated, err := readEvents(src, id)
	if err != nil {
		return RunEvidence{}, nil, fmt.Sprintf("run %q journal cannot be read: %v", id, err)
	}
	run.Events, run.EventsTruncated = len(events), truncated
	spans := foldRunEvents(&run, events)
	sort.Slice(run.Operations, func(a, b int) bool { return run.Operations[a].Start.Before(run.Operations[b].Start) })
	return run, spans, ""
}

func readEvents(src RuntimeSource, id string) (events []runtime.EngineeringEvent, truncated bool, err error) {
	var after int64
	for len(events) < maxEventsPerRun {
		page, more, err := src.EventsPage(id, after, eventsPageSize)
		if err != nil {
			return nil, false, err
		}
		events = append(events, page...)
		if !more || len(page) == 0 {
			return events, false, nil
		}
		after = page[len(page)-1].Sequence
	}
	return events, true, nil
}

func foldRunEvents(run *RunEvidence, events []runtime.EngineeringEvent) []Span {
	var spans []Span
	seen := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case runtime.EventRunCompleted, runtime.EventRunFailed, runtime.EventRunCancelled:
			if run.EndedAt == nil {
				at := e.OccurredAt.UTC()
				run.EndedAt, run.Terminal = &at, true
				run.WallSeconds = Known(at.Sub(run.CreatedAt).Seconds())
			}
		case runtime.EventOperationAfter:
			var op runtime.RunOperation
			if json.Unmarshal(e.Payload, &op) != nil || op.StartedAt == nil {
				continue
			}
			key := fmt.Sprintf("%s|%d", op.ID, op.StartedAt.UnixNano())
			if seen[key] {
				continue
			}
			seen[key] = true
			span := OperationSpan{OperationID: op.ID, Kind: op.Kind, Attempt: op.Attempt, State: string(op.State),
				Start: op.StartedAt.UTC(), End: e.OccurredAt.UTC()}
			run.Operations = append(run.Operations, span)
			spans = append(spans, Span{Label: fmt.Sprintf("%s/%s#%d", run.RunID, op.Kind, op.Attempt), Start: span.Start, End: span.End})
		case runtime.EventHandoffReported:
			run.HandoffsReported++
		case runtime.EventHandoffRefused:
			run.HandoffsRefused++
		case runtime.EventReviewContinuationGranted:
			run.ReviewContinuations++
		case runtime.EventDecisionsDelivered:
			run.DecisionDeliveries++
		case runtime.EventHumanAuthorityRecorded:
			run.HumanAuthorityRecords++
		case runtime.EventGitHubReviewObserved:
			var p runtime.GitHubReviewObservedPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				run.Reviews = append(run.Reviews, ReviewSighting{At: e.OccurredAt.UTC(), HeadRevision: p.HeadRevision,
					State: p.State, FindingCount: p.FindingCount})
			}
		}
	}
	return spans
}

// collectPlan reads the plan stream's event counts and budget cost. Cost is
// known only when at least one consumption was recorded and EVERY consumption
// reported a figure: a partial sum would be an underestimate presented as fact.
func collectPlan(src RuntimeSource, ev *RuntimeEvidence) {
	events, err := src.PlanEvents(ev.Plan.ID)
	if err != nil {
		ev.Problems = append(ev.Problems, fmt.Sprintf("plan %q stream cannot be read: %v", ev.Plan.ID, err))
		return
	}
	if len(events) > maxPlanEvents {
		ev.Problems = append(ev.Problems, fmt.Sprintf("plan %q has more than %d events; evidence is truncated", ev.Plan.ID, maxPlanEvents))
		events = events[:maxPlanEvents]
	}
	ev.Plan.EventCounts = map[string]int{}
	var total int64
	consumptions, unreported := 0, 0
	for _, e := range events {
		ev.Plan.EventCounts[e.Type]++
		if e.Type != runtime.EventPlanBudgetConsumed {
			continue
		}
		var p runtime.PlanBudgetConsumedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			unreported++
			continue
		}
		consumptions++
		if p.CostKnown && p.CostMicros != nil {
			total += *p.CostMicros
		} else {
			unreported++
		}
	}
	switch {
	case consumptions == 0:
	case unreported > 0:
		ev.CostMicros = Unknown[int64](fmt.Sprintf("%d of %d plan budget consumptions reported no cost; unknown is not zero", unreported, consumptions))
	default:
		ev.CostMicros = Known(total)
	}
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
