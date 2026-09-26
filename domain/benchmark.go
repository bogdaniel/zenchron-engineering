package domain

// The #66 benchmark record.
//
// A BenchmarkRecord is a member of Contract for the same reason the M2
// planning artifacts are: it is a canonical JSON document whose shape is
// defined by a schema, and Decode/Encode is the ONE path that validates one.
// It is NOT part of the facts -> policy -> obligations -> evidence -> authority
// flow: nothing consults a BenchmarkRecord to decide what a run may do. It is
// a measurement artifact about the product, produced after a case finishes,
// never an input to one.

// BenchmarkMode identifies which of #66's three comparable operating modes
// produced a record.
type BenchmarkMode string

const (
	BenchmarkModeDirectAgent      BenchmarkMode = "direct_agent"
	BenchmarkModeZenchronExplicit BenchmarkMode = "zenchron_explicit"
	BenchmarkModeZenchronPlanned  BenchmarkMode = "zenchron_planned"
)

// BenchmarkClass is one task-corpus class, reused/refined from #11. It is a
// closed catalogue: a record cannot claim a class the schema does not also
// accept, the same discipline the M2 planning vocabulary applies to roles.
type BenchmarkClass string

const (
	BenchmarkClassTrivialDocumentation       BenchmarkClass = "trivial/documentation"
	BenchmarkClassNormalBehavioralChange     BenchmarkClass = "normal behavioral change"
	BenchmarkClassAPIBusinessRuleWork        BenchmarkClass = "API/business-rule work"
	BenchmarkClassSecuritySensitiveChange    BenchmarkClass = "security-sensitive change"
	BenchmarkClassHiddenScopeExpansion       BenchmarkClass = "hidden/material scope expansion"
	BenchmarkClassFailingTestsRemediation    BenchmarkClass = "failing tests/remediation"
	BenchmarkClassReviewFeedbackCorrection   BenchmarkClass = "review-feedback correction"
	BenchmarkClassProviderTransientQuotaWait BenchmarkClass = "provider transient/quota wait"
	BenchmarkClassBaseDriftConflict          BenchmarkClass = "base-drift/conflict case"
	BenchmarkClassMultiTaskConcurrentCohort  BenchmarkClass = "multi-task concurrent cohort"
)

// BenchmarkClasses is the full, ordered #66 task corpus catalogue.
var BenchmarkClasses = []BenchmarkClass{
	BenchmarkClassTrivialDocumentation,
	BenchmarkClassNormalBehavioralChange,
	BenchmarkClassAPIBusinessRuleWork,
	BenchmarkClassSecuritySensitiveChange,
	BenchmarkClassHiddenScopeExpansion,
	BenchmarkClassFailingTestsRemediation,
	BenchmarkClassReviewFeedbackCorrection,
	BenchmarkClassProviderTransientQuotaWait,
	BenchmarkClassBaseDriftConflict,
	BenchmarkClassMultiTaskConcurrentCohort,
}

// BenchmarkResult is the terminal disposition a case reports.
type BenchmarkResult string

const (
	BenchmarkResultAccepted          BenchmarkResult = "accepted"
	BenchmarkResultAcceptedForReview BenchmarkResult = "accepted_for_review"
	BenchmarkResultRejected          BenchmarkResult = "rejected"
	BenchmarkResultAbandoned         BenchmarkResult = "abandoned"
	BenchmarkResultInvalidated       BenchmarkResult = "invalidated"
)

// BenchmarkRecord is one leverage observation: one case, run in one mode, with
// the human-supervision measurement #66 requires.
type BenchmarkRecord struct {
	SchemaVersion  string                  `json:"schema_version"`
	ID             string                  `json:"id"`
	Benchmark      string                  `json:"benchmark"`
	CorpusVersion  string                  `json:"corpus_version"`
	Mode           BenchmarkMode           `json:"mode"`
	RecordedAt     string                  `json:"recorded_at"`
	Case           BenchmarkCase           `json:"case"`
	Subject        BenchmarkSubject        `json:"subject"`
	Measured       BenchmarkMeasured       `json:"measured"`
	Outcome        BenchmarkOutcome        `json:"outcome"`
	Concurrency    *BenchmarkConcurrency   `json:"concurrency,omitempty"`
	Interpretation BenchmarkInterpretation `json:"interpretation"`
}

// BenchmarkCase identifies the corpus item a record measured.
type BenchmarkCase struct {
	ID         string         `json:"id"`
	Class      BenchmarkClass `json:"class"`
	Source     *string        `json:"source,omitempty"`
	Acceptance string         `json:"acceptance"`
}

// BenchmarkSubject binds a record to the exact repository state and the
// agents that acted on it, in whichever mode produced the record.
type BenchmarkSubject struct {
	Repository        string               `json:"repository"`
	BaseRevision      string               `json:"base_revision"`
	CandidateRevision *string              `json:"candidate_revision,omitempty"`
	Controller        *BenchmarkController `json:"controller,omitempty"`
	Agents            []BenchmarkAgent     `json:"agents"`
}

// BenchmarkController describes the runtime build that produced a
// zenchron_explicit or zenchron_planned record. direct_agent records omit it.
type BenchmarkController struct {
	Kind string  `json:"kind"`
	Note *string `json:"note,omitempty"`
}

// BenchmarkAgent is one provider invocation the case attributes to a record:
// the sole implementer for direct_agent and zenchron_explicit, or one entry
// per plan stage for zenchron_planned.
type BenchmarkAgent struct {
	StageID      *string `json:"stage_id,omitempty"`
	Role         *string `json:"role,omitempty"`
	AgentID      string  `json:"agent_id"`
	ProviderKind string  `json:"provider_kind"`
	VendorFamily string  `json:"vendor_family"`
	Model        *string `json:"model,omitempty"`
	TrustMode    *string `json:"trust_mode,omitempty"`
}

// BenchmarkMeasured is the human-supervision measurement #66 requires,
// recorded separately rather than folded into one number. Time waiting on a
// provider or CI that does not consume human attention is deliberately not a
// field here: it is out of scope for supervision time by definition.
type BenchmarkMeasured struct {
	ActiveOperatorMinutes   *float64      `json:"active_operator_minutes"`
	RelayActions            *int          `json:"relay_actions,omitempty"`
	WorkaroundInterventions *int          `json:"workaround_interventions,omitempty"`
	AuthorityInterventions  *int          `json:"authority_interventions,omitempty"`
	TaskSelectionMinutes    *float64      `json:"task_selection_minutes,omitempty"`
	ReviewMinutes           *float64      `json:"review_minutes,omitempty"`
	ProviderWaitMinutes     *float64      `json:"provider_wait_minutes,omitempty"`
	WallSecondsTotal        *float64      `json:"wall_seconds_total,omitempty"`
	Cost                    BenchmarkCost `json:"cost"`
}

// BenchmarkCost is unknown when a provider reports none. Unknown is never
// rendered as zero: Amount/Currency are populated only when Known is true.
type BenchmarkCost struct {
	Known    bool     `json:"known"`
	Currency *string  `json:"currency,omitempty"`
	Amount   *float64 `json:"amount,omitempty"`
	Reason   *string  `json:"reason,omitempty"`
}

// BenchmarkOutcome is what the case actually produced, including the
// secondary metrics #66 asks to report alongside the primary leverage ratio.
type BenchmarkOutcome struct {
	Result               BenchmarkResult `json:"result"`
	Artifact             *string         `json:"artifact,omitempty"`
	ReviewCycles         *int            `json:"review_cycles,omitempty"`
	FirstPassAccepted    *bool           `json:"first_pass_accepted,omitempty"`
	RemediationCount     *int            `json:"remediation_count,omitempty"`
	EscapedDefects       *int            `json:"escaped_defects,omitempty"`
	FalseBlocks          *int            `json:"false_blocks,omitempty"`
	AuthorityPrecision   *float64        `json:"authority_precision,omitempty"`
	AuthorityRecall      *float64        `json:"authority_recall,omitempty"`
	GroundTruthAvailable bool            `json:"ground_truth_available"`
}

// BenchmarkConcurrency is present only for cases run as part of a concurrent
// cohort; it carries the parallelism-proof measurements #66 asks for.
type BenchmarkConcurrency struct {
	ConcurrentWith                     []string `json:"concurrent_with"`
	OverlapWallSeconds                 *float64 `json:"overlap_wall_seconds,omitempty"`
	HumanAttentionDuringOverlapMinutes *float64 `json:"human_attention_during_overlap_minutes,omitempty"`
	CrossTaskBlocking                  *bool    `json:"cross_task_blocking,omitempty"`
	ManualMessageShuttling             *bool    `json:"manual_message_shuttling,omitempty"`
}

// BenchmarkInterpretation states in words what the record does and does not
// show, so a single observation is never silently read as a benchmark result.
type BenchmarkInterpretation struct {
	LeverageClaim string `json:"leverage_claim"`
	Note          string `json:"note"`
}
