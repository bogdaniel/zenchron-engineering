package domain

type TemplateStage struct {
	ID                   string                   `json:"id"`
	Kind                 StageKind                `json:"kind"`
	Role                 EngineeringRole          `json:"role,omitempty"`
	ExecutionKind        string                   `json:"execution_kind,omitempty"`
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
