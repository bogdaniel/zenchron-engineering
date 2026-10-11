package runtime

import (
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// PlanView is the operator's whole answer about one plan: the exact revision,
// its replayed state, the assignments resolution would produce, and every stage
// that cannot be assigned with the reason.
type PlanView struct {
	WorkGraphID string                   `json:"work_graph_id,omitempty"`
	Plan        domain.EngineeringPlan   `json:"plan"`
	Snapshot    PlanSnapshot             `json:"state"`
	Assigned    []domain.AgentAssignment `json:"assignments,omitempty"`
	Blocked     []planning.Blocked       `json:"blocked,omitempty"`
	// Consumed and Envelope are shown together, and unknown cost stays unknown:
	// an approval view that rendered "not reported" as zero would be telling an
	// operator something nobody measured.
	Envelope domain.PlanBudgetEnvelope `json:"budget_envelope"`
	Consumed domain.PlanConsumption    `json:"consumed"`
	// Preview is present when the revision shown is NOT the one governing the
	// work. The state beside it is then prospective - what approving this
	// revision would leave - rather than a report of what is happening.
	Preview *PlanPreview `json:"preview,omitempty"`
	// BaseChange is present when this revision binds a different exact base than
	// the revision it replaces. Approving it authorizes work against a different
	// tree, and everything performed against the old base is redone.
	BaseChange *PlanBaseChange `json:"base_change,omitempty"`
	// AssignmentsDigest is the digest of the assignments approving THIS view
	// would bind: who performs each stage that has not started, under which
	// profile, packs, context and worker.
	//
	// It exists so a decision can name it. The plan digest binds the document,
	// and registry and workforce edits do not change that document - so an edit
	// landing between reading a proposal and deciding on it would be bound as
	// "what the operator saw". Naming this closes that the same way naming the
	// revision digest closed deciding an unread document.
	AssignmentsDigest string `json:"assignments_digest,omitempty"`
	// Unbound is the stages this revision's approval bound NOTHING for, in
	// stage order: they showed a blocker rather than an assignment, so there
	// was no identity to hold execution to and they resolve live when they
	// become performable.
	Unbound []string `json:"unbound,omitempty"`
}
