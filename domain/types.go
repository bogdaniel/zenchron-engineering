package domain

// SchemaVersion is the only schema version implemented by this package.
const SchemaVersion = "0.1"

// Stage identifies when an engineering fact or scope was established.
type Stage string

const (
	StagePredicted Stage = "predicted"
	StageObserved  Stage = "observed"
	StageVerified  Stage = "verified"
)

// Confidence describes certainty attached to an engineering fact.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Subject binds a contract to an exact repository revision.
type Subject struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// ObjectRevision references an exact revision of another durable object.
type ObjectRevision struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// RevisionReference references a revision where the object ID is its map key.
type RevisionReference struct {
	Revision string `json:"revision"`
}

// Action identifies one protected operation and target.
type Action struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// ProjectModel describes project state relevant to engineering governance.
type ProjectModel struct {
	SchemaVersion      string                       `json:"schema_version"`
	ID                 string                       `json:"id"`
	Revision           string                       `json:"revision"`
	Subject            Subject                      `json:"subject"`
	CriticalBoundaries *map[string]CriticalBoundary `json:"critical_boundaries,omitempty"`
	TestCapabilities   *map[string]TestCapability   `json:"test_capabilities,omitempty"`
	PolicyProfiles     *[]string                    `json:"policy_profiles,omitempty"`
}

// CriticalBoundary identifies paths belonging to a sensitive boundary.
type CriticalBoundary struct {
	Type  string   `json:"type"`
	Paths []string `json:"paths"`
}

// TestCapability describes one project testing capability.
type TestCapability struct {
	Type string `json:"type"`
}

// EngineeringFact is one attributable, uncertainty-preserving engineering fact.
type EngineeringFact struct {
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	Key           string         `json:"key"`
	Value         FactValue      `json:"value"`
	Stage         Stage          `json:"stage"`
	Confidence    Confidence     `json:"confidence"`
	Subject       Subject        `json:"subject"`
	Provenance    FactProvenance `json:"provenance"`
}

// FactProvenance identifies how a fact was produced.
type FactProvenance struct {
	Type     string `json:"type"`
	Producer string `json:"producer"`
}

// EngineeringPolicy is a revisioned set of fact-driven policy rules.
type EngineeringPolicy struct {
	SchemaVersion string                `json:"schema_version"`
	ID            string                `json:"id"`
	Revision      string                `json:"revision"`
	Rules         map[string]PolicyRule `json:"rules"`
}

// PolicyRule maps one condition to one or more policy effects.
type PolicyRule struct {
	When   PolicyCondition `json:"when"`
	Effect PolicyEffect    `json:"effect"`
}

// PolicyCondition matches one engineering fact and optional qualifiers.
type PolicyCondition struct {
	Fact       string           `json:"fact"`
	Equals     FactValue        `json:"equals"`
	Stage      *Stage           `json:"stage,omitempty"`
	Confidence *Confidence      `json:"confidence,omitempty"`
	Provenance *ProvenanceMatch `json:"provenance,omitempty"`
}

// ProvenanceMatch optionally constrains fact provenance.
type ProvenanceMatch struct {
	Type     *string `json:"type,omitempty"`
	Producer *string `json:"producer,omitempty"`
}

// PolicyRequirement describes an invariant or obligation emitted by policy.
type PolicyRequirement struct {
	Statement      string    `json:"statement"`
	RequiredClaims *[]string `json:"required_claims,omitempty"`
}

// PolicyEffect contains outcomes emitted when a rule matches.
type PolicyEffect struct {
	Obligations         *map[string]PolicyRequirement `json:"obligations,omitempty"`
	Invariants          *map[string]PolicyRequirement `json:"invariants,omitempty"`
	RequiredClaims      *map[string]RequiredClaim     `json:"required_claims,omitempty"`
	Permissions         *[]Action                     `json:"permissions,omitempty"`
	Prohibitions        *[]Action                     `json:"prohibitions,omitempty"`
	AuthorityConditions *[]AuthorityCondition         `json:"authority_conditions,omitempty"`
}

// Requirement states a contract invariant or obligation.
type Requirement struct {
	Statement string `json:"statement"`
}

// EvidenceClass identifies an extensible, policy-defined class of supporting
// evidence. Its values are non-empty strings validated by the JSON Schema.
type EvidenceClass string

// RequiredClaim describes evidence needed by a work contract.
type RequiredClaim struct {
	EvidenceClass                 EvidenceClass `json:"evidence_class"`
	IndependentFromChangeProducer bool          `json:"independent_from_change_producer"`
}

// AuthorityCondition identifies claims required for a protected action.
type AuthorityCondition struct {
	Action         Action   `json:"action"`
	RequiredClaims []string `json:"required_claims"`
}

// ContractScope describes the bounded paths and observation stage of work.
type ContractScope struct {
	Stage           Stage    `json:"stage"`
	AllowedPaths    []string `json:"allowed_paths"`
	ProhibitedPaths []string `json:"prohibited_paths"`
}

// ContractProvenance binds a work contract to its compiler inputs.
type ContractProvenance struct {
	ProjectModel             ObjectRevision `json:"project_model"`
	Policy                   ObjectRevision `json:"policy"`
	CompilerVersion          string         `json:"compiler_version"`
	PreviousContractRevision *string        `json:"previous_contract_revision,omitempty"`
}

// EngineeringWorkContract is the bounded governance envelope for one unit of work.
type EngineeringWorkContract struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"id"`
	Revision            string                   `json:"revision"`
	Objective           string                   `json:"objective"`
	AcceptanceIntent    []string                 `json:"acceptance_intent"`
	Subject             Subject                  `json:"subject"`
	Scope               ContractScope            `json:"scope"`
	Facts               []string                 `json:"facts"`
	Invariants          map[string]Requirement   `json:"invariants"`
	Obligations         map[string]Requirement   `json:"obligations"`
	RequiredClaims      map[string]RequiredClaim `json:"required_claims"`
	Permissions         []Action                 `json:"permissions"`
	Prohibitions        []Action                 `json:"prohibitions"`
	AuthorityConditions []AuthorityCondition     `json:"authority_conditions"`
	Provenance          ContractProvenance       `json:"provenance"`
}

// ProducerType identifies the class of an evidence producer.
type ProducerType string

const (
	ProducerExecutionProvider ProducerType = "execution_provider"
	ProducerAssuranceProvider ProducerType = "assurance_provider"
	ProducerDeterministicTool ProducerType = "deterministic_tool"
	ProducerHuman             ProducerType = "human"
)

// EvidenceResultStatus is the outcome established by evidence.
type EvidenceResultStatus string

const (
	EvidencePassed       EvidenceResultStatus = "pass"
	EvidenceFailed       EvidenceResultStatus = "fail"
	EvidenceInconclusive EvidenceResultStatus = "inconclusive"
)

// EvidenceLifecycleStatus describes whether evidence currently applies.
type EvidenceLifecycleStatus string

const (
	EvidenceValid      EvidenceLifecycleStatus = "valid"
	EvidenceStale      EvidenceLifecycleStatus = "stale"
	EvidenceInvalid    EvidenceLifecycleStatus = "invalid"
	EvidenceIncomplete EvidenceLifecycleStatus = "incomplete"
)

// EvidenceProducer identifies who or what produced evidence.
type EvidenceProducer struct {
	ID   string       `json:"id"`
	Type ProducerType `json:"type"`
}

// EvidenceEnvironment binds evidence to its execution or review context.
type EvidenceEnvironment struct {
	Type       string    `json:"type"`
	Identifier string    `json:"identifier"`
	Toolchain  *[]string `json:"toolchain,omitempty"`
}

// EvidenceResult records an evidence outcome independently from its lifecycle.
type EvidenceResult struct {
	Status  EvidenceResultStatus `json:"status"`
	Summary *string              `json:"summary,omitempty"`
}

// EvidenceLifecycle records current evidence applicability.
type EvidenceLifecycle struct {
	Status EvidenceLifecycleStatus `json:"status"`
	Reason *string                 `json:"reason,omitempty"`
}

// EvidenceIntegrity records optional evidence integrity metadata.
type EvidenceIntegrity struct {
	Method string `json:"method"`
	Value  string `json:"value"`
}

// EvidenceProvenance identifies the source and recording time of evidence.
type EvidenceProvenance struct {
	Source     string             `json:"source"`
	RecordedAt string             `json:"recorded_at"`
	Integrity  *EvidenceIntegrity `json:"integrity,omitempty"`
}

// EvidenceItem supports one required claim; its identity is the containing map key.
type EvidenceItem struct {
	ClaimID       string              `json:"claim_id"`
	EvidenceClass EvidenceClass       `json:"evidence_class"`
	Producer      EvidenceProducer    `json:"producer"`
	Environment   EvidenceEnvironment `json:"environment"`
	Result        EvidenceResult      `json:"result"`
	Lifecycle     EvidenceLifecycle   `json:"lifecycle"`
	Provenance    EvidenceProvenance  `json:"provenance"`
}

// EvidenceBundle binds evidence to exact subject, contract, and policy revisions.
type EvidenceBundle struct {
	SchemaVersion string                  `json:"schema_version"`
	ID            string                  `json:"id"`
	Revision      string                  `json:"revision"`
	Subject       Subject                 `json:"subject"`
	Contract      ObjectRevision          `json:"contract"`
	Policy        ObjectRevision          `json:"policy"`
	Evidence      map[string]EvidenceItem `json:"evidence"`
}

// CapabilityStatus records whether the runtime technically has a capability.
type CapabilityStatus string

const (
	CapabilityAvailable   CapabilityStatus = "available"
	CapabilityUnavailable CapabilityStatus = "unavailable"
	CapabilityUnknown     CapabilityStatus = "unknown"
)

// PermissionStatus records whether a contract permits an action.
type PermissionStatus string

const (
	PermissionGranted PermissionStatus = "granted"
	PermissionDenied  PermissionStatus = "denied"
)

// AuthorityStatus is the current action-scoped authority outcome.
type AuthorityStatus string

const (
	AuthorityIncomplete        AuthorityStatus = "incomplete"
	AuthorityBlocked           AuthorityStatus = "blocked"
	AuthorityStale             AuthorityStatus = "stale"
	AuthorityAwaitingAuthority AuthorityStatus = "awaiting_authority"
	AuthorityAuthorized        AuthorityStatus = "authorized"
)

// Capability is the runtime capability snapshot used by a decision.
type Capability struct {
	Status CapabilityStatus `json:"status"`
}

// Permission is the contract permission snapshot used by a decision.
type Permission struct {
	Status PermissionStatus `json:"status"`
}

// DecisionBasis records the exact evidence bundle revisions and change producer
// identity considered by an authority evaluation.
type DecisionBasis struct {
	EvidenceBundles map[string]RevisionReference `json:"evidence_bundles"`
	ChangeProducer  EvidenceProducer             `json:"change_producer"`
}

// AuthorityDecision records whether one protected action may happen now.
type AuthorityDecision struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	Revision      string          `json:"revision"`
	Subject       Subject         `json:"subject"`
	Contract      ObjectRevision  `json:"contract"`
	Basis         DecisionBasis   `json:"basis"`
	Action        Action          `json:"action"`
	Capability    Capability      `json:"capability"`
	Permission    Permission      `json:"permission"`
	Status        AuthorityStatus `json:"status"`
	Satisfied     []string        `json:"satisfied"`
	Missing       []string        `json:"missing"`
	Stale         []string        `json:"stale"`
	Blocking      []string        `json:"blocking"`
}
