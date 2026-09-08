package domain

// The M2 planning contracts.
//
// Four separations are frozen here, and collapsing any of them is a defect:
//
//	EngineeringRole      WHAT responsibility a plan requires
//	AgentProfile         HOW an operator specialized a worker for that
//	                     responsibility
//	ExecutionAgent       WHICH installed worker actually runs
//	EngineeringPlan      the approved, immutable coordination of the above
//
// and separately:
//
//	EngineeringPlanTemplate != EngineeringPlan
//	ContextPolicy           != EngineeringPolicy
//
// A template is planning INPUT. A plan is a validated, approved artifact. A
// ContextPolicy selects which context classes an assignment receives; it can
// never grant authority, and EngineeringPolicy remains the only obligation
// system in the product.
//
// Everything in this file is DATA. Nothing here decides anything, contacts
// anything, or knows a provider's name: the compiler, the resolver and the plan
// reconciler are separate, and the kernel stays free of both.

// EngineeringRole is a semantic responsibility in a plan. It is not a provider,
// an executable, or a permanent AI persona: a plan may omit a role, combine two
// of them into one stage, or instantiate one role several times.
type EngineeringRole string

// The built-in role catalogue. It is deliberately small and closed for v0:
// every entry either appears in a #64 acceptance scenario or is required to
// express one of the small/medium/large compilation examples.
const (
	RoleProductArchitect EngineeringRole = "product_architect"
	RoleSystemArchitect  EngineeringRole = "system_architect"
	// RolePlanner is the semantic responsibility of DECOMPOSING work. It is
	// deliberately distinct from the Engineering Planner component: the
	// component compiles and validates plans, and this role is a bounded
	// responsibility a registered worker may perform through the non-mutating
	// planning invocation boundary. Its durable output is a
	// PlanRevisionProposal, never a nested plan.
	RolePlanner          EngineeringRole = "planner"
	RoleImplementer      EngineeringRole = "implementer"
	RoleTester           EngineeringRole = "tester"
	RoleSecurityReviewer EngineeringRole = "security_reviewer"
	RoleReviewer         EngineeringRole = "reviewer"
	RoleIntegrator       EngineeringRole = "integrator"
	RoleReleaseReviewer  EngineeringRole = "release_reviewer"
)

// EngineeringRoles lists the catalogue in canonical order. Order is stated
// rather than derived from a map so a schema, a document and a projection can
// all use the same sequence.
func EngineeringRoles() []EngineeringRole {
	return []EngineeringRole{
		RoleProductArchitect, RoleSystemArchitect, RolePlanner, RoleImplementer,
		RoleTester, RoleSecurityReviewer, RoleReviewer, RoleIntegrator, RoleReleaseReviewer,
	}
}

// KnownRole reports whether a role is in the catalogue. An unknown role is
// refused rather than passed through: a plan naming a responsibility nothing
// can resolve is a plan that cannot execute.
func KnownRole(role EngineeringRole) bool {
	for _, known := range EngineeringRoles() {
		if known == role {
			return true
		}
	}
	return false
}

// EngineeringCapability is a typed ability a stage requires and a profile or
// agent advertises.
//
// The v0 ontology is deliberately tiny. Today's workers are general coding
// CLIs that advertise substantially overlapping abilities, so a 13-item
// taxonomy would encode routing sophistication the product has not earned. A
// capability id is added when it creates a REAL eligibility distinction or
// carries a policy obligation, and not before.
type EngineeringCapability string

const (
	CapabilityRequirementsAnalysis  EngineeringCapability = "requirements_analysis"
	CapabilityArchitectureReasoning EngineeringCapability = "architecture_reasoning"
	CapabilityRepositoryAnalysis    EngineeringCapability = "repository_analysis"
	CapabilityCodeChange            EngineeringCapability = "code_change"
	CapabilityVerification          EngineeringCapability = "verification"
	CapabilitySecurityReview        EngineeringCapability = "security_review"
)

// EngineeringCapabilities lists the v0 ontology in canonical order.
func EngineeringCapabilities() []EngineeringCapability {
	return []EngineeringCapability{
		CapabilityRequirementsAnalysis, CapabilityArchitectureReasoning, CapabilityRepositoryAnalysis,
		CapabilityCodeChange, CapabilityVerification, CapabilitySecurityReview,
	}
}

// KnownCapability reports whether a capability is in the v0 ontology.
func KnownCapability(capability EngineeringCapability) bool {
	for _, known := range EngineeringCapabilities() {
		if known == capability {
			return true
		}
	}
	return false
}

// InvocationMode is what a stage needs a provider to be able to DO, and what a
// provider states it can prove.
//
// It is the eligibility fact a planner-role stage turns on: planning reasoning
// must run without candidate-write authority, so a provider that cannot enter
// an enforceable non-mutating mode is INELIGIBLE for it. It is never degraded
// into the permissive mode as a fallback.
type InvocationMode string

const (
	// InvocationModeMutating is ordinary bounded producer execution: the
	// worker may change the candidate workspace it was given.
	InvocationModeMutating InvocationMode = "mutating"
	// InvocationModeNonMutatingPlanning is reasoning with no write authority
	// over the workspace it is shown, in the provider's own enforceable
	// read-only or restricted mode.
	InvocationModeNonMutatingPlanning InvocationMode = "non_mutating_planning"
)

// TrustRequirement is the execution trust a stage requires of the worker that
// performs it. It reuses the #63 trust vocabulary rather than inventing a
// second one, and it can only be REQUIRED by policy or a plan - never granted
// by a profile, a template or repository content.
type TrustRequirement string

const (
	TrustRequirementOperatorTrusted TrustRequirement = "operator_trusted"
	TrustRequirementProtected       TrustRequirement = "protected"
)

// ---------------------------------------------------------------------------
// Operator-owned customization artifacts
// ---------------------------------------------------------------------------

// ArtifactSource is where an operator-owned artifact came from. It exists so
// "who wrote this instruction" is a durable fact rather than an assumption:
// candidate or repository content may be ordinary engineering context, and may
// never become privileged agent instruction.
type ArtifactSource struct {
	// Type is the provenance class. Only operator-owned classes exist on
	// purpose; there is no `repository` class, because a repository that could
	// author instruction would be authoring its own governance.
	Type string `json:"type"`
	// Location is the operator-controlled path or configuration member the
	// artifact was read from. It is never a candidate path.
	Location string `json:"location"`
}

// Operator-owned artifact provenance classes.
const (
	// SourceOperatorConfig is an artifact stated inline in the operator
	// configuration file.
	SourceOperatorConfig = "operator_config"
	// SourceOperatorFile is an artifact read from an operator-controlled file
	// outside any candidate workspace.
	SourceOperatorFile = "operator_file"
)

// InstructionPack is a small, versioned, content-addressed set of MODEL-VISIBLE
// engineering instructions used to specialize an AgentProfile.
//
// Frozen trust law: instruction content is operator-owned authority
// configuration. Candidate or repository data may not create, replace, mutate
// or implicitly extend an active InstructionPack. A file inside a candidate
// that resembles AGENTS.md, CLAUDE.md or a prompt is ordinary untrusted
// context, and the adapters already suppress it where the provider supports it.
type InstructionPack struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Revision      string `json:"revision"`
	// Digest is the content digest of Instructions. It is what an assignment
	// freezes, so editing a pack cannot silently change work already approved.
	Digest       string         `json:"digest"`
	Source       ArtifactSource `json:"source"`
	Instructions []string       `json:"instructions"`
}

// ContextClass is one class of engineering context an assignment may receive.
// The vocabulary is closed so a ContextPolicy is a SELECTION over stated
// classes rather than an open-ended query language.
type ContextClass string

const (
	ContextObjective          ContextClass = "objective"
	ContextAcceptanceCriteria ContextClass = "acceptance_criteria"
	ContextObligations        ContextClass = "obligations"
	ContextPermissions        ContextClass = "permissions"
	ContextProhibitions       ContextClass = "prohibitions"
	ContextProjectFacts       ContextClass = "project_model_facts"
	ContextArchitectureNotes  ContextClass = "architecture_notes"
	ContextPolicyExcerpts     ContextClass = "policy_excerpts"
	ContextRepositoryPaths    ContextClass = "repository_paths"
	ContextUpstreamOutputs    ContextClass = "upstream_stage_outputs"
	ContextCandidateDiff      ContextClass = "candidate_diff"
	ContextPriorFindings      ContextClass = "prior_findings"
	// ContextProducerReasoning is the implementation worker's own reasoning
	// transcript. It is a class so it can be NAMED and refused: an independent
	// reviewer never inherits it, and no ContextPolicy can request it.
	ContextProducerReasoning ContextClass = "producer_reasoning"
)

// ContextClasses lists the closed vocabulary in canonical order.
func ContextClasses() []ContextClass {
	return []ContextClass{
		ContextObjective, ContextAcceptanceCriteria, ContextObligations, ContextPermissions,
		ContextProhibitions, ContextProjectFacts, ContextArchitectureNotes, ContextPolicyExcerpts,
		ContextRepositoryPaths, ContextUpstreamOutputs, ContextCandidateDiff, ContextPriorFindings,
		ContextProducerReasoning,
	}
}

// RequiredContextClasses are the classes an assignment ALWAYS receives. A
// ContextPolicy may narrow everything else and may never erase these: they
// carry the governance envelope, and a worker that cannot see its obligations
// is a worker that cannot be held to them.
func RequiredContextClasses() []ContextClass {
	return []ContextClass{
		ContextObjective, ContextAcceptanceCriteria, ContextObligations,
		ContextPermissions, ContextProhibitions,
	}
}

// ContextPolicy is the operator-owned selection rule for assignment context. It
// is NOT EngineeringPolicy: it grants no authority, discharges no obligation,
// and cannot remove context the work contract requires.
type ContextPolicy struct {
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	Revision      string         `json:"revision"`
	Digest        string         `json:"digest"`
	Source        ArtifactSource `json:"source"`
	// Include narrows the optional classes to exactly this set. Empty means
	// every class the compiler would otherwise supply.
	Include []ContextClass `json:"include,omitempty"`
	// Exclude removes optional classes. Excluding a required class is refused
	// at load time rather than ignored at compile time.
	Exclude []ContextClass `json:"exclude,omitempty"`
}

// ProfileConstraints are the tighter operating bounds a profile may state. Every
// member can only NARROW: there is deliberately no member that raises a ceiling,
// grants a permission, or enables a permission bypass.
type ProfileConstraints struct {
	// MaxWallSeconds tightens the per-invocation wall bound.
	MaxWallSeconds *int `json:"max_wall_seconds,omitempty"`
	// MaxExecutionAttempts tightens the retry ceiling for stages this profile
	// performs.
	MaxExecutionAttempts *int `json:"max_execution_attempts,omitempty"`
	// DenyPermissionBypass refuses the provider's unsafe permission mode for
	// this profile even where the agent has standing operator permission for
	// it. There is no member that ALLOWS a bypass: that statement belongs to
	// the agent configuration, which is operator authority.
	DenyPermissionBypass bool `json:"deny_permission_bypass,omitempty"`
}

// AgentProfile is the operator-defined custom agent: a reusable specialization
// of an already-registered ExecutionAgent, built without changing Zenchron core
// code.
//
// Frozen customization law:
//
//	Customization may specialize or narrow. It may not escalate.
//
// A profile cannot raise provider trust, widen filesystem/network/secret
// access, raise permission or budget ceilings, grant publication or acceptance
// authority, suppress an EngineeringPolicy obligation, reset consumed budget,
// or claim isolation the underlying provider cannot prove.
type AgentProfile struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Version       int    `json:"version"`
	// Digest is the content digest of the profile, excluding this member. An
	// assignment freezes it, so editing profile v3 into v4 cannot rewrite work
	// already approved or in flight.
	Digest string `json:"digest"`
	// ExecutionAgent is the id of a #63 registered agent. A profile never
	// introduces an executable, a credential or a trust mode.
	ExecutionAgent string `json:"execution_agent"`
	// Model is a supported model preference. It is a preference over the
	// agent's configured default, never a new provider.
	Model string `json:"model,omitempty"`
	// Capabilities is the effective capability set this profile advertises. It
	// is validated against what the underlying agent can support, so a profile
	// cannot advertise an ability its worker does not have.
	Capabilities []EngineeringCapability `json:"capabilities"`
	// Instructions names InstructionPacks the operator has already installed.
	Instructions []string `json:"instructions,omitempty"`
	// ContextPolicy names an installed ContextPolicy.
	ContextPolicy string `json:"context_policy,omitempty"`
	// TrustRequirement is the trust mode this profile REQUIRES of its agent.
	// Stating a stronger requirement than the agent holds makes the profile
	// invalid; it never promotes the agent.
	TrustRequirement TrustRequirement   `json:"trust_requirement"`
	Constraints      ProfileConstraints `json:"constraints,omitzero"`
	Source           ArtifactSource     `json:"source"`
}

// ---------------------------------------------------------------------------
// Execution agent descriptors
// ---------------------------------------------------------------------------

// ExecutionAgentDescriptor is what a planner may reason over about one
// registered #63 worker. It is a PROJECTION built by the runtime from the agent
// registry and the adapter catalogue; nothing in the kernel or in this package
// learns a provider's name from it.
//
// VendorFamily is a separate fact from ProviderKind on purpose. Two adapter ids
// backed by the same vendor are not vendor-family independent, and a plan that
// required vendor independence must not be satisfied by renaming an adapter.
type ExecutionAgentDescriptor struct {
	ID           string           `json:"id"`
	ProviderKind string           `json:"provider_kind"`
	VendorFamily string           `json:"vendor_family"`
	TrustMode    TrustRequirement `json:"trust_mode"`
	Model        string           `json:"model,omitempty"`
	// Capabilities is what this worker can be asked to do at all. Today's
	// coding CLIs advertise a broad, overlapping set, and that is stated
	// honestly rather than differentiated for appearance.
	Capabilities []EngineeringCapability `json:"capabilities"`
	// InvocationModes are the modes this adapter can actually enter and prove.
	// A mode absent here makes the agent ineligible for a stage that requires
	// it - there is no permissive fallback.
	InvocationModes []InvocationMode `json:"invocation_modes"`
	// Available and Detail are the non-billable readiness observation.
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
	// Unattended reports whether the operator allows this worker to be started
	// without an explicit command.
	Unattended bool `json:"unattended"`
}

// SupportsInvocationMode reports whether this worker can enter one mode.
func (d ExecutionAgentDescriptor) SupportsInvocationMode(mode InvocationMode) bool {
	for _, supported := range d.InvocationModes {
		if supported == mode {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Independence
// ---------------------------------------------------------------------------

// IndependenceDimension is the typed equivalence class two stages must differ
// in. It replaces the ambiguous "different_material_producer" string: an
// independence requirement nobody can evaluate is an independence requirement
// nobody enforces.
type IndependenceDimension string

const (
	// IndependenceAgentProfile requires a different AgentProfile identity.
	IndependenceAgentProfile IndependenceDimension = "agent_profile"
	// IndependenceExecutionAgent requires a different registered agent id.
	IndependenceExecutionAgent IndependenceDimension = "execution_agent"
	// IndependenceProviderKind requires a different adapter kind.
	IndependenceProviderKind IndependenceDimension = "provider_kind"
	// IndependenceVendorFamily requires a different vendor/model family. Two
	// Claude Code profiles satisfy profile independence and never satisfy
	// this one.
	IndependenceVendorFamily IndependenceDimension = "vendor_family"
	// IndependenceHuman requires a human leg. It is only ever satisfied
	// through the human decision boundary, never by any worker.
	IndependenceHuman IndependenceDimension = "human"
)

// IndependenceDimensions lists the dimensions in increasing strength order.
// Order is meaningful: satisfying a stronger dimension satisfies the weaker
// ones it implies, and a required dimension is never silently degraded.
func IndependenceDimensions() []IndependenceDimension {
	return []IndependenceDimension{
		IndependenceAgentProfile, IndependenceExecutionAgent,
		IndependenceProviderKind, IndependenceVendorFamily, IndependenceHuman,
	}
}

// IndependenceRequirement states that a stage's producer must differ from the
// producers of other named stages in one dimension.
type IndependenceRequirement struct {
	Dimension IndependenceDimension `json:"dimension"`
	// DifferentFrom names the stages whose producer this stage must not share
	// the dimension with. It is omitempty because POLICY states a relationship
	// - "independent of the material producer" - without knowing stage ids;
	// the plan compiler binds it to exact stages, and the plan schema requires
	// it there.
	DifferentFrom []string `json:"different_from,omitempty"`
	// HumanSubstitutionPermitted records that POLICY explicitly permits an
	// independent human review to stand in when no eligible independent worker
	// exists. It is a policy statement carried into the plan; a template, a
	// profile or an operator approval cannot introduce it.
	HumanSubstitutionPermitted bool `json:"human_substitution_permitted,omitempty"`
}

// ---------------------------------------------------------------------------
// Plan requirements compiled by the existing policy compiler
// ---------------------------------------------------------------------------

// RoleRequirement is a policy-required role, optionally constrained. It is
// carried in PolicyEffect and compiled by the existing policy compiler: there
// is no PlannerPolicy and no second obligation engine.
type RoleRequirement struct {
	Role EngineeringRole `json:"role"`
	// Capabilities are additionally required of whoever performs the role.
	Capabilities []EngineeringCapability `json:"capabilities,omitempty"`
	// Independence is the typed independence this role must hold from the
	// material producer. `different_from` is empty here because policy names a
	// relationship rather than a stage id; the compiler binds it to the
	// producing stages when the plan is compiled.
	Independence *IndependenceRequirement `json:"independence,omitempty"`
	// TrustRequirement is the execution trust the role requires.
	TrustRequirement TrustRequirement `json:"trust_requirement,omitempty"`
	// Statement is the operator-readable reason, so an added obligation can be
	// explained in the approval view rather than appearing as a bare id.
	Statement string `json:"statement"`
}

// GateRequirement is a policy-required non-agent gate. Gates never create
// EngineeringRuns: an assurance gate is satisfied from existing
// evidence/authority state, and a human decision gate from the existing human
// authority boundary.
type GateRequirement struct {
	Kind StageKind `json:"kind"`
	// RequiredClaims are the claim ids whose satisfaction proves this gate. For
	// a human decision gate they are the claims a person answers.
	RequiredClaims []string `json:"required_claims,omitempty"`
	// Action is the protected action a human decision gate decides, when it
	// decides one.
	Action    *Action `json:"action,omitempty"`
	Statement string  `json:"statement"`
}

// PlanRequirements are the plan-shaped obligations the existing policy compiler
// emits into a work contract. They are OBLIGATIONS, not a plan: the planner may
// add conservative work beyond them and may never weaken or drop them.
type PlanRequirements struct {
	Roles []RoleRequirement `json:"roles,omitempty"`
	// Capabilities are required of the plan as a whole - some stage must hold
	// each of them.
	Capabilities []EngineeringCapability `json:"capabilities,omitempty"`
	Gates        []GateRequirement       `json:"gates,omitempty"`
}

// Empty reports whether policy stated no plan-shaped obligation at all.
func (r PlanRequirements) Empty() bool {
	return len(r.Roles) == 0 && len(r.Capabilities) == 0 && len(r.Gates) == 0
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// StageKind distinguishes what a stage IS. Only StageAgent may ever become an
// EngineeringRun; the two gate kinds reference existing durable state and
// deliberately create no worker run at all.
type StageKind string

const (
	StageAgent             StageKind = "agent"
	StageAssuranceGate     StageKind = "assurance_gate"
	StageHumanDecisionGate StageKind = "human_decision_gate"
)

// StageKinds lists the closed set in canonical order.
func StageKinds() []StageKind {
	return []StageKind{StageAgent, StageAssuranceGate, StageHumanDecisionGate}
}

// TemplateStage is one stage a reusable template asks for. It is a planning
// HINT and a constraint, never governance: policy may add obligations a
// template omitted, and a template may not remove one.
type TemplateStage struct {
	ID                   string                   `json:"id"`
	Kind                 StageKind                `json:"kind"`
	Role                 EngineeringRole          `json:"role,omitempty"`
	Objective            string                   `json:"objective,omitempty"`
	DependsOn            []string                 `json:"depends_on,omitempty"`
	RequiresCapabilities []EngineeringCapability  `json:"requires_capabilities,omitempty"`
	Independence         *IndependenceRequirement `json:"independence,omitempty"`
	// Profile is an operator preference for which installed profile performs
	// this stage. It is a preference: policy obligations and eligibility still
	// decide, and an ineligible preference is explained rather than obeyed.
	Profile string `json:"profile,omitempty"`
	// ContextPolicy narrows this stage's context. It can never widen it.
	ContextPolicy string `json:"context_policy,omitempty"`
	// When is a deterministic condition over the same engineering facts the
	// policy compiler matches. It reuses PolicyCondition rather than inventing
	// a second predicate language, and it is the ONLY conditionality a
	// template has: there are no loops, expressions or scripts.
	When *PolicyCondition `json:"when,omitempty"`
	// RequiredClaims and Action mirror GateRequirement for template-declared
	// gates.
	RequiredClaims []string `json:"required_claims,omitempty"`
	Action         *Action  `json:"action,omitempty"`
}

// EngineeringPlanTemplate is a reusable, versioned engineering process. It is
// planning input: template + intent + ProjectModel + EngineeringPolicy +
// available workforce compile into a proposed plan.
//
// A template may not remove a policy obligation, widen trust or permission,
// declare evidence sufficient, reset a budget or authorize adoption.
type EngineeringPlanTemplate struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	Version       int             `json:"version"`
	Digest        string          `json:"digest"`
	Description   string          `json:"description,omitempty"`
	Stages        []TemplateStage `json:"stages"`
	// BudgetEnvelope may only TIGHTEN the operator's configured maxima. The
	// compiler intersects it with the operator ceiling rather than trusting it.
	BudgetEnvelope *PlanBudgetEnvelope `json:"budget_envelope,omitempty"`
	Source         ArtifactSource      `json:"source"`
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

// PlanBudgetEnvelope is the AGGREGATE ceiling for one plan, on top of the
// per-run and per-assignment budgets that remain authoritative.
//
// Frozen budget law: a revision, reassignment, restart or decomposition step
// may not reset already-consumed plan or run budget. A revision may tighten the
// remaining envelope; widening it requires the same explicit operator authority
// that could have granted the ceiling originally.
type PlanBudgetEnvelope struct {
	// MaxChildRuns bounds how many EngineeringRuns this plan may create in
	// total, across every revision.
	MaxChildRuns int `json:"max_child_runs"`
	// MaxConcurrency bounds how many of them may be active at once, WITHIN the
	// operator's global ceiling. It never raises that ceiling.
	MaxConcurrency int `json:"max_concurrency"`
	// MaxProviderInvocations bounds total provider invocations attributable to
	// this plan.
	MaxProviderInvocations int `json:"max_provider_invocations"`
	// MaxWallSeconds bounds total ACTIVE execution wall time across the plan.
	MaxWallSeconds int `json:"max_wall_seconds,omitempty"`
	// MaxCostMicros is a monetary ceiling and is a POINTER because most
	// configurations cannot report cost at all. Absent means unknown, which is
	// carried as unknown everywhere; it is never rendered as zero.
	MaxCostMicros *int64 `json:"max_cost_micros,omitempty"`
}

// StageBudget is one stage's share, resolved into the assignment. It is bounded
// by the envelope and by the operator's per-run budgets.
type StageBudget struct {
	MaxExecutionAttempts int `json:"max_execution_attempts,omitempty"`
	MaxWallSeconds       int `json:"max_wall_seconds,omitempty"`
}

// PlanStage is one node of the plan graph.
type PlanStage struct {
	ID   string    `json:"id"`
	Kind StageKind `json:"kind"`
	// Role is required for an agent stage and absent for a gate: a gate is not
	// performed by anybody, which is exactly why it creates no run.
	Role                 EngineeringRole          `json:"role,omitempty"`
	Objective            string                   `json:"objective,omitempty"`
	DependsOn            []string                 `json:"depends_on,omitempty"`
	RequiresCapabilities []EngineeringCapability  `json:"requires_capabilities,omitempty"`
	Independence         *IndependenceRequirement `json:"independence,omitempty"`
	TrustRequirement     TrustRequirement         `json:"trust_requirement,omitempty"`
	// InvocationMode is what the stage requires of its provider. A planner-role
	// stage requires non-mutating planning; an implementer stage is mutating.
	InvocationMode InvocationMode `json:"invocation_mode,omitempty"`
	// Profile pins or prefers an installed profile.
	Profile       string `json:"profile,omitempty"`
	ContextPolicy string `json:"context_policy,omitempty"`
	// RequiredClaims are the claim ids an assurance or human decision gate is
	// satisfied by. They reference the SAME claim ids the work contract and the
	// evidence bundles already use; the plan defines no second evidence model.
	RequiredClaims []string `json:"required_claims,omitempty"`
	// Action is the protected action a human decision gate decides.
	Action *Action     `json:"action,omitempty"`
	Budget StageBudget `json:"budget,omitzero"`
	// Rationale is the planner's operator-readable reason for this stage. It is
	// explanation, never authority.
	Rationale string `json:"rationale,omitempty"`
}

// PlanReasoningProvenance records the reasoning invocation that contributed to
// a plan, including the proof that it could not write.
//
// Every member is an OBSERVATION about the invocation. A plan compiled without
// a reasoning agent carries no provenance at all rather than an empty one.
type PlanReasoningProvenance struct {
	AgentID      string           `json:"agent_id"`
	ProviderKind string           `json:"provider_kind"`
	VendorFamily string           `json:"vendor_family,omitempty"`
	TrustMode    TrustRequirement `json:"trust_mode"`
	Model        string           `json:"model,omitempty"`
	ProfileID    string           `json:"profile_id,omitempty"`
	// InvocationMode is the mode the provider actually entered, and
	// ProviderMode names the provider's own enforceable mode where it has one.
	InvocationMode InvocationMode `json:"invocation_mode"`
	ProviderMode   string         `json:"provider_mode,omitempty"`
	// WorkspaceDigestBefore/After are the runtime's OWN verification that the
	// planning workspace did not change, independent of any provider claim.
	WorkspaceDigestBefore string `json:"workspace_digest_before"`
	WorkspaceDigestAfter  string `json:"workspace_digest_after"`
	// WorkspaceUnchanged is the conclusion. It is recorded rather than implied
	// so a reader never has to compare two digests to learn the answer.
	WorkspaceUnchanged bool `json:"workspace_unchanged"`
}

// PlanProvenance binds a plan revision to everything that produced it.
type PlanProvenance struct {
	CompilerVersion string         `json:"compiler_version"`
	ProjectModel    ObjectRevision `json:"project_model"`
	Policy          ObjectRevision `json:"policy"`
	Contract        ObjectRevision `json:"contract"`
	Template        *TemplateRef   `json:"template,omitempty"`
	// Reasoning is present when a registered execution agent contributed
	// semantic reasoning to this revision.
	Reasoning *PlanReasoningProvenance `json:"reasoning,omitempty"`
	// PreviousRevision is the revision this one replaces.
	PreviousRevision *int `json:"previous_revision,omitempty"`
	// ProposalID is the PlanRevisionProposal that produced this revision.
	ProposalID string `json:"proposal_id,omitempty"`
}

// TemplateRef pins the exact template a plan was compiled from. A later
// template edit affects new proposals, never a historical approved plan.
type TemplateRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// EngineeringPlan coordinates one or more EngineeringRuns plus typed non-agent
// gates. It REFERENCES the work contract, evidence bundles and authority
// decisions; it duplicates none of them.
//
// A plan revision is immutable. Changing anything approval-visible produces a
// new revision through a PlanRevisionProposal, never an in-place edit.
type EngineeringPlan struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Revision      int    `json:"revision"`
	// Digest is the content digest of this revision, excluding the member
	// itself. It is what an assignment, a proposal and the journal all pin.
	Digest         string             `json:"digest"`
	Objective      string             `json:"objective"`
	Subject        Subject            `json:"subject"`
	BudgetEnvelope PlanBudgetEnvelope `json:"budget_envelope"`
	Stages         []PlanStage        `json:"stages"`
	Provenance     PlanProvenance     `json:"provenance"`
}

// Stage returns one stage by id.
func (p EngineeringPlan) Stage(id string) (PlanStage, bool) {
	for _, stage := range p.Stages {
		if stage.ID == id {
			return stage, true
		}
	}
	return PlanStage{}, false
}

// ---------------------------------------------------------------------------
// Assignments
// ---------------------------------------------------------------------------

// PlanRef pins an exact plan revision.
type PlanRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
}

// PackRef pins an exact InstructionPack revision by digest.
type PackRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}

// ProfileBinding is the frozen identity of the profile that performed work.
// Editing the profile afterwards cannot change what this says.
type ProfileBinding struct {
	ID               string                  `json:"id"`
	Version          int                     `json:"version"`
	Digest           string                  `json:"digest"`
	Instructions     []PackRef               `json:"instructions,omitempty"`
	ContextPolicy    *PackRef                `json:"context_policy,omitempty"`
	Capabilities     []EngineeringCapability `json:"capabilities"`
	TrustRequirement TrustRequirement        `json:"trust_requirement"`
}

// AgentBinding is the frozen identity of the underlying worker.
type AgentBinding struct {
	ID           string           `json:"id"`
	ProviderKind string           `json:"provider_kind"`
	VendorFamily string           `json:"vendor_family"`
	TrustMode    TrustRequirement `json:"trust_mode"`
	Model        string           `json:"model,omitempty"`
}

// UpstreamOutput is one accepted upstream stage result an assignment may
// consume. It is EXPLICIT: an integrator consumes stated outputs rather than
// inheriting a conversation.
type UpstreamOutput struct {
	StageID string `json:"stage_id"`
	RunID   string `json:"run_id,omitempty"`
	// Candidate is the exact commit/tree the upstream stage produced.
	Candidate string `json:"candidate,omitempty"`
	Tree      string `json:"tree,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// ContextPack is the minimum useful context one assignment receives. Different
// roles receive different packs, and no pack ever carries another worker's
// hidden reasoning transcript.
type ContextPack struct {
	Objective          string           `json:"objective"`
	AcceptanceCriteria []string         `json:"acceptance_criteria"`
	Obligations        []string         `json:"obligations,omitempty"`
	Permissions        []string         `json:"permissions,omitempty"`
	Prohibitions       []string         `json:"prohibitions,omitempty"`
	Facts              []string         `json:"facts,omitempty"`
	ArchitectureNotes  []string         `json:"architecture_notes,omitempty"`
	PolicyExcerpts     []string         `json:"policy_excerpts,omitempty"`
	RepositoryPaths    []string         `json:"repository_paths,omitempty"`
	UpstreamOutputs    []UpstreamOutput `json:"upstream_outputs,omitempty"`
	// Included and Excluded are the audit trail of the ContextPolicy decision,
	// so a reviewer can see what a worker was not shown.
	Included []ContextClass `json:"included"`
	Excluded []ContextClass `json:"excluded,omitempty"`
}

// CandidateEvaluation is one considered profile and why it was or was not
// eligible. Rejections are recorded, not summarized away: "why did my security
// reviewer not get this stage" is an operator question with an exact answer.
type CandidateEvaluation struct {
	Profile  string   `json:"profile"`
	Agent    string   `json:"agent"`
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

// ResolutionExplanation is the deterministic account of the selection.
type ResolutionExplanation struct {
	Considered []CandidateEvaluation `json:"considered"`
	Selected   string                `json:"selected,omitempty"`
	// Reason states why the selected candidate won, or why none did.
	Reason string `json:"reason"`
}

// IndependenceBinding is a resolved independence obligation: the dimension, the
// stages it separates, and the equivalence class values that prove separation.
type IndependenceBinding struct {
	Dimension     IndependenceDimension `json:"dimension"`
	DifferentFrom []string              `json:"different_from"`
	// Class is this assignment's value in the dimension, and OtherClasses are
	// the values it must differ from. Recording both is what makes a later
	// relabelling detectable: identity is compared, never trusted.
	Class        string   `json:"class"`
	OtherClasses []string `json:"other_classes,omitempty"`
	// SatisfiedBy names how the requirement is met: a distinct worker, or an
	// explicitly policy-permitted human leg.
	SatisfiedBy string `json:"satisfied_by"`
}

// Independence satisfaction mechanisms.
const (
	IndependenceSatisfiedByWorker = "independent_worker"
	IndependenceSatisfiedByHuman  = "independent_human_review"
)

// AgentAssignment is one executable stage resolved to an exact worker, with the
// reason it was eligible and the exact configuration that performed the work.
//
// Provider-specific semantics never leak into kernel authority types: this is a
// runtime/planning artifact, and the kernel continues to see contracts,
// evidence and authority decisions only.
type AgentAssignment struct {
	SchemaVersion        string                  `json:"schema_version"`
	ID                   string                  `json:"id"`
	Plan                 PlanRef                 `json:"plan"`
	StageID              string                  `json:"stage_id"`
	Role                 EngineeringRole         `json:"role"`
	RequiredCapabilities []EngineeringCapability `json:"required_capabilities,omitempty"`
	Profile              ProfileBinding          `json:"profile"`
	Agent                AgentBinding            `json:"agent"`
	InvocationMode       InvocationMode          `json:"invocation_mode"`
	TrustRequirement     TrustRequirement        `json:"trust_requirement"`
	Contract             ObjectRevision          `json:"contract"`
	Context              ContextPack             `json:"context"`
	Budget               StageBudget             `json:"budget,omitzero"`
	Independence         []IndependenceBinding   `json:"independence,omitempty"`
	Selection            ResolutionExplanation   `json:"selection"`
	// RunID is the ordinary #63 EngineeringRun this assignment became, once the
	// plan reconciler created it. Absent until the stage is dependency-ready.
	RunID string `json:"run_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Revision proposals
// ---------------------------------------------------------------------------

// Proposal origins. A proposal states where it came from because the approval
// boundary is the same for all of them and the accountability is not.
const (
	ProposalOriginInitial       = "initial_plan"
	ProposalOriginDecomposition = "planner_role_decomposition"
	ProposalOriginOperatorEdit  = "operator_edit"
	ProposalOriginRemediation   = "remediation"
)

// ProposalProvenance identifies who produced a proposal.
type ProposalProvenance struct {
	Origin string `json:"origin"`
	// StageID is the planner-role stage that emitted a decomposition proposal.
	StageID   string                   `json:"stage_id,omitempty"`
	Operator  string                   `json:"operator,omitempty"`
	Reasoning *PlanReasoningProvenance `json:"reasoning,omitempty"`
}

// BudgetDelta is the proposal's effect on the aggregate envelope, stated
// separately from the plan content so an approver sees it without diffing.
//
// Consumed is carried here because a proposal is evaluated against what has
// ALREADY been spent: a revision may tighten the remaining envelope and may
// never reset consumption.
type BudgetDelta struct {
	Current  PlanBudgetEnvelope `json:"current"`
	Proposed PlanBudgetEnvelope `json:"proposed"`
	Consumed PlanConsumption    `json:"consumed"`
	// Widened reports that the proposal asks for a LARGER envelope, which
	// requires explicit operator authority within configured maxima.
	Widened bool `json:"widened"`
}

// PlanConsumption is what a plan has already spent. It is a projection of the
// durable journal and never a stored counter that a restart could reset.
type PlanConsumption struct {
	ChildRuns           int `json:"child_runs"`
	ProviderInvocations int `json:"provider_invocations"`
	WallSeconds         int `json:"wall_seconds"`
	// CostMicros is a POINTER for the same reason the ceiling is: a
	// subscription CLI reports no cost, and unknown is not zero.
	CostMicros *int64 `json:"cost_micros,omitempty"`
	// CostKnown states whether ANY invocation reported cost. It is separate
	// from the value so "nothing reported" is distinguishable from "reported
	// zero".
	CostKnown bool `json:"cost_known"`
}

// PlanChangeSummary is the approval-visible difference between the current
// revision and the proposed one.
type PlanChangeSummary struct {
	AddedStages   []string `json:"added_stages,omitempty"`
	RemovedStages []string `json:"removed_stages,omitempty"`
	ChangedStages []string `json:"changed_stages,omitempty"`
	// InvalidatedStages are downstream stages whose assumptions this change
	// invalidates. Only affected stages appear: an unrelated stage is never
	// invalidated to be safe, because that would discard valid work.
	InvalidatedStages []string `json:"invalidated_stages,omitempty"`
	// Material reports that the change alters approval-visible semantics -
	// stages, dependencies, assignments, budgets, trust or independence - and
	// therefore requires a fresh operator approval before affected execution
	// continues.
	Material bool `json:"material"`
}

// ProposalValidationStatus is the deterministic validator's verdict.
type ProposalValidationStatus string

const (
	ProposalValid   ProposalValidationStatus = "valid"
	ProposalRefused ProposalValidationStatus = "refused"
)

// ProposalValidation is what deterministic validation concluded. A refused
// proposal is DURABLE: refusing quietly would lose the reason a reasoning agent
// asked for something it may not have.
type ProposalValidation struct {
	Status ProposalValidationStatus `json:"status"`
	Errors []string                 `json:"errors,omitempty"`
}

// ApprovalStatus is the operator decision state of a proposal.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

// ProposalApproval is the operator's answer. It is authority to EXECUTE within
// existing policy and permission ceilings, and it is never merge, release or
// acceptance authority.
type ProposalApproval struct {
	Status     ApprovalStatus `json:"status"`
	Operator   string         `json:"operator,omitempty"`
	RecordedAt string         `json:"recorded_at,omitempty"`
	Note       string         `json:"note,omitempty"`
}

// PlanRevisionProposal is the durable artifact that crosses the deterministic
// validation and operator approval boundary.
//
// A planner-role decomposition stage emits one of these. It never creates a
// nested child plan: there is no sub-plan scheduler, and a plan cannot contain
// a plan.
type PlanRevisionProposal struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	// Source is the exact plan revision this proposal was made against. A
	// proposal against a superseded revision is refused rather than rebased.
	Source PlanRef `json:"source"`
	// Proposed is the replacement revision content. It is validated in full
	// against the EngineeringPlan schema by the same code path that validates
	// any plan; the proposal schema constrains its shape structurally.
	Proposed EngineeringPlan `json:"proposed_revision"`
	Reason   string          `json:"reason"`

	Provenance ProposalProvenance `json:"provenance"`
	Budget     BudgetDelta        `json:"budget"`
	Changes    PlanChangeSummary  `json:"changes"`
	Validation ProposalValidation `json:"validation"`
	Approval   ProposalApproval   `json:"approval"`
}

// ---------------------------------------------------------------------------
// Content identity
// ---------------------------------------------------------------------------

// Content digests. Each one is the SHA-256 of the artifact's own canonical
// document with the digest member cleared, so an artifact can carry its own
// identity without the identity depending on itself.
//
// They exist as methods rather than as one reflective helper because the set is
// small, closed, and each one is a one-line statement of what identity means for
// that artifact. An assignment freezes these values; editing an artifact
// afterwards therefore cannot rewrite work already approved or in flight.

// ContentDigest is this pack's identity.
func (p InstructionPack) ContentDigest() (string, error) { p.Digest = ""; return Digest(p) }

// ContentDigest is this context policy's identity.
func (p ContextPolicy) ContentDigest() (string, error) { p.Digest = ""; return Digest(p) }

// ContentDigest is this profile's identity.
func (p AgentProfile) ContentDigest() (string, error) { p.Digest = ""; return Digest(p) }

// ContentDigest is this template's identity.
func (t EngineeringPlanTemplate) ContentDigest() (string, error) { t.Digest = ""; return Digest(t) }

// ContentDigest is this plan revision's identity.
func (p EngineeringPlan) ContentDigest() (string, error) { p.Digest = ""; return Digest(p) }
