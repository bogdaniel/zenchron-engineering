package runtime

import (
	"errors"
	"fmt"
)

// RunPolicy is the category R policy one EngineeringRun is frozen under at
// creation (ADR-0003 §1, Phase B1). It is not a second store: Budgets IS the
// run's persisted run.Budgets and Agent IS its recorded assignment. What this
// type adds is one stable digest over them, together with the repository
// layer whose tightening was already resolved into those budgets (§2).
//
// Only the resulting values are digested, not how a plan stage narrowed them
// (ADR-0003 open question 2).
type RunPolicy struct {
	Budgets RunBudgets `json:"budgets"`
	// Agent is the execution agent the run was assigned at creation - the
	// frozen form of default_agent.
	Agent string `json:"agent,omitempty"`
	// RepositoryConfig is the repository-layer configuration digest whose
	// tighten-only budgets were resolved before the run froze its policy.
	RepositoryConfig string `json:"repository_config,omitempty"`
}

// Digest is the RunPolicyDigest.
func (p RunPolicy) Digest() (string, error) { return Digest(p) }

// policy is the RunPolicy this row records. A nil Budgets has none.
func (run EngineeringRun) policy(repositoryConfig string) RunPolicy {
	var budgets RunBudgets
	if run.Budgets != nil {
		budgets = *run.Budgets
	}
	return RunPolicy{Budgets: budgets, Agent: run.AgentID, RepositoryConfig: repositoryConfig}
}

// RunPolicyRecord is what the genesis event records about the frozen policy:
// its digest, and the one digest input that is not on the run row.
type RunPolicyRecord struct {
	SHA256           string `json:"sha256"`
	RepositoryConfig string `json:"repository_config,omitempty"`
}

// RunCreatedPayload is the run.created payload. Both members are optional and
// additive: a genesis written before B1 is a bare ControllerBuild (the
// embedded, flattened form decodes it unchanged) or nothing at all.
type RunCreatedPayload struct {
	*ControllerBuild
	RunPolicy *RunPolicyRecord `json:"run_policy,omitempty"`
}

func (p RunCreatedPayload) validate() error {
	var errs []error
	if p.ControllerBuild != nil {
		errs = append(errs, p.validateAttested())
	}
	if p.RunPolicy != nil {
		if !isSHA256Hex(p.RunPolicy.SHA256) {
			errs = append(errs, fmt.Errorf("run_policy.sha256 %q is not a sha256 digest", p.RunPolicy.SHA256))
		}
		errs = append(errs, bounded("run_policy.repository_config", p.RunPolicy.RepositoryConfig))
	}
	if p.ControllerBuild == nil && p.RunPolicy == nil {
		errs = append(errs, errors.New("a run.created payload records a controller build, a run policy, or is absent"))
	}
	return errors.Join(errs...)
}

// Where a run's RunPolicy comes from (ADR-0003 §5). Nothing is backfilled.
const (
	// RunPolicyRecorded: the digest was recorded in the run's genesis event.
	RunPolicyRecorded = "run_created"
	// RunPolicyLegacyRunBudgets: a run created before B1 with a persisted
	// run.Budgets. Its policy is DERIVED from that durable record and is never
	// claimed as one made at creation.
	RunPolicyLegacyRunBudgets = "legacy_run_budgets"
	// RunPolicyLegacyControllerBinding: a run with no run.Budgets at all. The
	// policy that governed it is identified only by the configuration digest
	// inside its controller binding, so no RunPolicyDigest is claimed.
	RunPolicyLegacyControllerBinding = "legacy_controller_binding"
)

// RunPolicyStatus is status's answer to "which policy governs this run".
type RunPolicyStatus struct {
	SHA256           string `json:"sha256,omitempty"`
	Source           string `json:"source"`
	RepositoryConfig string `json:"repository_config,omitempty"`
	// Unverifiable names the budget members status could NOT report. They are
	// members the run never recorded, whose value is the one its controller
	// binding identifies, and the reading controller is not that binding.
	// They are reported as zero and listed here, never filled in from this
	// process's configuration. UNKNOWN stays UNKNOWN.
	Unverifiable []string `json:"unverifiable,omitempty"`
}

// reportedBudgets is what status may truthfully present as this run's budgets.
// Under the creating binding it is exactly budgets(). Under a changed
// controller, a member budgets() would fill from the binding (because the run
// never recorded it) is unverifiable: filling it from this process's
// configuration would present another controller's value as the run's. Then
// NO budgets are reported (nil, JSON null) and the unverifiable members are
// named instead. A zero would read as a real bound and an omitted lifecycle or
// inactivity value as "none", so neither encoding is used for UNKNOWN.
func (s *runState) reportedBudgets() (*RunBudgets, []string) {
	budgets := s.budgets()
	if !s.controllerChanged {
		return &budgets, nil
	}
	var recorded RunBudgets
	if s.run.Budgets != nil {
		recorded = *s.run.Budgets
	}
	var unverifiable []string
	for _, member := range []struct {
		name    string
		missing bool
	}{
		{"wall_limit", recorded.WallLimit <= 0},
		// Only a nil record reads the lifecycle deadline from the binding. A
		// recorded zero means "none".
		{"lifecycle_deadline", s.run.Budgets == nil},
		{"max_execution_attempts", recorded.MaxExecutionAttempts <= 0},
		{"max_execution_continuations", recorded.MaxExecutionContinuations <= 0 && recorded.MaxExecutionAttempts <= 0},
		{"max_remediation_attempts", recorded.MaxRemediationAttempts <= 0},
		{"max_assurance_attempts", recorded.MaxAssuranceAttempts <= 0},
		{"provider_inactivity_limit", recorded.ProviderInactivityLimit <= 0},
	} {
		if member.missing {
			unverifiable = append(unverifiable, member.name)
		}
	}
	if len(unverifiable) > 0 {
		return nil, unverifiable
	}
	return &budgets, nil
}

// runPolicy reads the recorded RunPolicyDigest, or states the legacy source.
func (s *runState) runPolicy() RunPolicyStatus {
	if recorded := s.genesis().RunPolicy; recorded != nil {
		return RunPolicyStatus{SHA256: recorded.SHA256, Source: RunPolicyRecorded, RepositoryConfig: recorded.RepositoryConfig}
	}
	if s.run.Budgets == nil {
		return RunPolicyStatus{Source: RunPolicyLegacyControllerBinding}
	}
	// Derived from durable run facts only, and labelled so. The repository
	// layer it was created under was never recorded, so none is claimed.
	digest, err := s.run.policy("").Digest()
	if err != nil {
		return RunPolicyStatus{Source: RunPolicyLegacyRunBudgets}
	}
	return RunPolicyStatus{SHA256: digest, Source: RunPolicyLegacyRunBudgets}
}

// budgets is the bound THIS run is judged by: its frozen RunPolicy budgets,
// read back EXACTLY. No member is read live and none is min(live, frozen)
// (ADR-0003 §2 condition 6), so a configuration edit - wider OR narrower - and a
// restart leave a live run's budgets untouched.
//
// THE LEGACY RULE (ADR-0003 §5). A member the run has no frozen value for -
// a run with nil run.Budgets, or a member that predates its field - is the
// value identified by the run's controller binding. That is the configured
// value, and ONLY because it cannot be anything else: such a run is reconciled
// solely by a controller whose binding (and so whose configuration digest) is
// identical to the one that created it - any other controller parks it
// controller_changed before it plans anything, and #307 re-adopt refuses
// while it is live. So the fallback reproduces exactly the value the run
// already had; it can never hand it a wider one. A member whose zero IS a
// frozen meaning keeps that meaning: lifecycle deadline (none), provider
// invocations (unbounded), attempt wall limit (the pre-#328 operation rule).
func (s *runState) budgets() RunBudgets {
	configured := s.rt.deps.Budgets.defaults()
	if s.run.Budgets == nil {
		configured.AttemptWallLimit = 0
		configured.MaxProviderInvocations = 0
		configured.MaxExecutionContinuations = s.continuationLimit()
		return configured
	}
	frozen := *s.run.Budgets
	for _, member := range []struct {
		frozen     *int
		configured int
	}{
		{&frozen.MaxExecutionAttempts, configured.MaxExecutionAttempts},
		{&frozen.MaxRemediationAttempts, configured.MaxRemediationAttempts},
		{&frozen.MaxAssuranceAttempts, configured.MaxAssuranceAttempts},
	} {
		if *member.frozen <= 0 {
			*member.frozen = member.configured
		}
	}
	if frozen.WallLimit <= 0 {
		frozen.WallLimit = configured.WallLimit
	}
	// A run persisted before #238 has no window. Its absence is NOT read as
	// "no bound", which would hand exactly those runs the unbounded stall the
	// window exists to remove; it is the bound identified by its binding.
	if frozen.ProviderInactivityLimit <= 0 {
		frozen.ProviderInactivityLimit = configured.ProviderInactivityLimit
	}
	frozen.MaxExecutionContinuations = s.continuationLimit()
	return frozen
}
