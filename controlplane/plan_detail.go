package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// PlanDetail is the one sanitized view of a plan revision (#425). It carries
// structure, state and bindings only: no objective, rationale, stage reason or
// reasoning payload - the boundary treats plan text as private, exactly as
// /v1/plans/{id} does - and no assignment RESOLUTION, which needs the
// governing controller's configuration (runtime.PlanService.DurableView).
type PlanDetail struct {
	ID       string `json:"plan_id"`
	Revision int    `json:"revision"`
	// GoverningRevision is the approved revision the work runs under; 0 means
	// no revision has been approved.
	GoverningRevision int `json:"governing_revision"`
	// Shown says what Revision is relative to the governing one: governing,
	// proposed, superseded, rejected or unapproved. A proposal is never
	// presented as the plan that governs the work.
	Shown        string            `json:"shown"`
	Digest       string            `json:"digest"`
	Repository   string            `json:"repository"`
	BaseRevision string            `json:"base_revision"`
	Template     string            `json:"template,omitempty"`
	Stages       []PlanStageDetail `json:"stages"`
	Budget       PlanBudget        `json:"budget"`
	// Invalidated lists the stages approving this revision would invalidate;
	// present only when Revision is previewed against a different governing one.
	Invalidated []string        `json:"invalidated,omitempty"`
	BaseChange  *PlanBaseChange `json:"base_change,omitempty"`
}

type PlanStageDetail struct {
	ID        string   `json:"stage_id"`
	Kind      string   `json:"kind"`
	DependsOn []string `json:"depends_on"`
	// Role, Profile and ContextPolicy are what the plan REQUIRES;
	// BoundProfile, Agent, ProviderKind and TrustMode are what a started stage
	// was BOUND to. EngineeringRole, AgentProfile and ExecutionAgent are three
	// different facts and stay three different fields.
	Role          string `json:"role,omitempty"`
	Profile       string `json:"profile,omitempty"`
	ContextPolicy string `json:"context_policy,omitempty"`
	BoundProfile  string `json:"bound_profile,omitempty"`
	// State is empty when the replayed plan has no record of the stage:
	// unknown, not "pending".
	State        string `json:"state,omitempty"`
	AssignmentID string `json:"assignment_id,omitempty"`
	Agent        string `json:"agent,omitempty"`
	ProviderKind string `json:"provider_kind,omitempty"`
	TrustMode    string `json:"trust_mode,omitempty"`
	// RunID is set only for agent stages: a gate never creates a run.
	RunID string `json:"run_id,omitempty"`
	// Gate is what durably satisfied a gate stage: the existing evidence
	// bundle or authority decision and human evidence it references, never a
	// run of its own.
	Gate *PlanGateRefs `json:"gate,omitempty"`
	// Review is this stage's latest recorded verdict, present only when the
	// stage is a reviewer. It names the exact candidate/tree the verdict is
	// about - staleness against a newer upstream head is not implied.
	Review *PlanStageReviewDTO `json:"review,omitempty"`
}

type PlanStageReviewDTO struct {
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason,omitempty"`
	Findings  []string `json:"findings,omitempty"`
	Candidate string   `json:"candidate"`
	Tree      string   `json:"tree"`
}

type PlanGateRefs struct {
	Claims          []string `json:"claims,omitempty"`
	Evidence        *RefDTO  `json:"evidence,omitempty"`
	Decision        *RefDTO  `json:"decision,omitempty"`
	HumanEvidenceID string   `json:"human_evidence_id,omitempty"`
	// ProvenHeads is each proving run at the head it carried, "run@commit".
	ProvenHeads []string `json:"proven_heads,omitempty"`
}

type RefDTO struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

func refDTO(r rt.Ref) *RefDTO {
	if r.ID == "" {
		return nil
	}
	return &RefDTO{ID: r.ID, Revision: r.Revision}
}

type PlanBudget struct {
	MaxChildRuns           int  `json:"max_child_runs"`
	MaxConcurrency         int  `json:"max_concurrency"`
	MaxProviderInvocations int  `json:"max_provider_invocations"`
	MaxWallSeconds         int  `json:"max_wall_seconds,omitempty"`
	ChildRuns              int  `json:"child_runs"`
	ProviderInvocations    int  `json:"provider_invocations"`
	WallSeconds            int  `json:"wall_seconds"`
	CostKnown              bool `json:"cost_known"`
	// CostMicros is reported only when CostKnown: an unknown cost is not zero.
	CostMicros *int64 `json:"cost_micros,omitempty"`
}

type PlanBaseChange struct {
	FromRevision int    `json:"from_revision"`
	From         string `json:"from"`
	To           string `json:"to"`
}

func planDetailProjection(v rt.PlanView) PlanDetail {
	plan, snapshot := v.Plan, v.Snapshot
	out := PlanDetail{
		ID: plan.ID, Revision: plan.Revision, Digest: plan.Digest,
		Repository: plan.Subject.Repository, BaseRevision: plan.Subject.Revision,
		Stages: make([]PlanStageDetail, 0, len(plan.Stages)),
		Budget: PlanBudget{
			MaxChildRuns: v.Envelope.MaxChildRuns, MaxConcurrency: v.Envelope.MaxConcurrency,
			MaxProviderInvocations: v.Envelope.MaxProviderInvocations, MaxWallSeconds: v.Envelope.MaxWallSeconds,
			ChildRuns: v.Consumed.ChildRuns, ProviderInvocations: v.Consumed.ProviderInvocations,
			WallSeconds: v.Consumed.WallSeconds, CostKnown: v.Consumed.CostKnown,
		},
	}
	if v.Consumed.CostKnown {
		out.Budget.CostMicros = v.Consumed.CostMicros
	}
	governing, approved := snapshot.ApprovedRevision()
	if approved {
		out.GoverningRevision = governing
	}
	switch {
	case snapshot.Rejected[plan.Revision]:
		out.Shown = "rejected"
	case !approved:
		out.Shown = "unapproved"
	case plan.Revision == governing:
		out.Shown = "governing"
	case plan.Revision > governing:
		out.Shown = "proposed"
	default:
		out.Shown = "superseded"
	}
	if t := plan.Provenance.Template; t != nil {
		out.Template = t.ID
	}
	for _, stage := range plan.Stages {
		s := PlanStageDetail{
			ID: stage.ID, Kind: string(stage.Kind), DependsOn: append([]string{}, stage.DependsOn...),
			Role: string(stage.Role), Profile: stage.Profile, ContextPolicy: stage.ContextPolicy,
		}
		if p, ok := snapshot.Stages[stage.ID]; ok {
			s.State, s.AssignmentID = string(p.State), p.AssignmentID
			s.Agent, s.ProviderKind, s.TrustMode = p.AgentID, p.ProviderKind, p.TrustMode
			if p.ProfileID != "" {
				s.BoundProfile = fmt.Sprintf("%s@%d", p.ProfileID, p.ProfileVersion)
			}
			if stage.Kind == domain.StageAgent {
				s.RunID = p.RunID
			} else if g := p.Gate; g != nil {
				s.Gate = &PlanGateRefs{
					Claims: slices.Clone(g.Claims), Evidence: refDTO(g.Evidence), Decision: refDTO(g.Decision),
					HumanEvidenceID: g.HumanEvidenceID, ProvenHeads: slices.Clone(g.ProvenHeads),
				}
			}
			if r := p.Review; r != nil {
				s.Review = &PlanStageReviewDTO{
					Verdict: r.Verdict, Reason: r.Reason, Findings: slices.Clone(r.Findings),
					Candidate: r.Candidate, Tree: r.Tree,
				}
			}
		}
		out.Stages = append(out.Stages, s)
	}
	if v.Preview != nil {
		out.Invalidated = slices.Clone(v.Preview.Invalidated)
	}
	if c := v.BaseChange; c != nil {
		out.BaseChange = &PlanBaseChange{FromRevision: c.FromRevision, From: c.From, To: c.To}
	}
	return out
}

// readPlanDetail is shared by the JSON route and the page so both present the
// same read. A missing plan or revision is not_found; anything else is a read
// failure, never an empty plan.
func readPlanDetail(store *rt.ReadStore, r *http.Request) (PlanDetail, int, string) {
	// ?revision= selects one point in the plan's history, bounded like a page.
	revision, ok := pageNumber(r, "revision", 0, 1<<31-1)
	if !ok {
		return PlanDetail{}, 400, "invalid_page"
	}
	view, err := store.PlanView(r.PathValue("id"), int(revision))
	var refused *rt.PlanRefusedError
	if errors.As(err, &refused) {
		return PlanDetail{}, 404, "not_found"
	}
	if err != nil {
		return PlanDetail{}, 500, "read_failed"
	}
	return planDetailProjection(view), 200, ""
}

func (a *API) planDetail(w http.ResponseWriter, r *http.Request) {
	detail, status, code := readPlanDetail(a.Store, r)
	if status != 200 {
		fail(w, status, code)
		return
	}
	send(w, 200, detail)
}
