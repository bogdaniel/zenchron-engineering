package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/analysis"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// AttemptRef is the immutable identity of ONE verification of ONE candidate.
//
// The CANDIDATE is part of the operation id, so two candidates of the same run
// never share a namespace and a later verification cannot land on an earlier
// one's evidence. The scheduler attempt separates a retry of the same
// candidate, and the confirmation pass is named rather than folded into the
// attempt, because a flake verdict is a comparison of two transcripts and
// needs both to survive.
func (r AssuranceRequest) AttemptRef() ExecutionAttemptRef {
	operation := "assurance-" + r.Commit
	if r.Confirmation {
		operation += "-confirmation"
	}
	return ExecutionAttemptRef{RunID: r.RunID, OperationID: operation, Attempt: r.Attempt}
}

type AssuranceProvider interface {
	Assure(context.Context, AssuranceRequest) (AssuranceResult, error)
}

// EvidenceProducer is a provider that DECLARES which evidence classes it can
// produce. It is the capability half of the evidence model: a policy names the
// class of evidence a claim needs, and this names the classes the configured
// system can actually obtain.
//
// The two vocabularies were never checked against each other. A run could
// therefore compile a contract, spend a real model budget implementing it, pass
// exact-tree assurance, and only then discover that the claim gating
// publication asked for a class nothing configured could ever produce.
// run-0943e257539346f8763db04505cbf322 did exactly that.
//
// Declaring a class is not a promise the evidence will PASS; it is a statement
// that this producer can answer that kind of question at all.
type EvidenceProducer interface {
	ProducedEvidenceClasses() []domain.EvidenceClass
}

// ProducibleEvidenceClasses is the set of evidence classes the configured
// providers declare, plus the classes a human records directly. A provider that
// declares nothing contributes nothing: capability is stated, never assumed
// from a type name.
func ProducibleEvidenceClasses(providers ...any) map[domain.EvidenceClass]bool {
	producible := map[domain.EvidenceClass]bool{
		// A human approval is obtained by a person through the operator
		// authority boundary, not by a provider.
		HumanEvidenceClass: true,
	}
	for _, provider := range providers {
		if declaring, ok := provider.(EvidenceProducer); ok {
			for _, class := range declaring.ProducedEvidenceClasses() {
				if class != "" {
					producible[class] = true
				}
			}
		}
	}
	return producible
}

// UnsupportedEvidenceRequirement names one required claim whose evidence class
// no configured producer can supply. It is bounded, typed identity - a claim id
// and a class - and never free text.
type UnsupportedEvidenceRequirement struct {
	ClaimID       string               `json:"claim_id"`
	EvidenceClass domain.EvidenceClass `json:"evidence_class"`
}

// UnfulfillableEvidence reports the required claims for one protected action
// that no configured producer and no human can satisfy, in deterministic order.
// An empty result means every required claim has SOME producer; it says nothing
// about whether that producer will pass.
func UnfulfillableEvidence(contract domain.EngineeringWorkContract, action domain.Action, producible map[domain.EvidenceClass]bool) []UnsupportedEvidenceRequirement {
	// The claim set is the SAME union authority evaluates: the action's own
	// condition plus the discharge claims of every material acceptance
	// obligation. Checking only the condition would let a run spend its whole
	// budget and then sit INCOMPLETE on an acceptance claim nothing can
	// produce - the original defect, one level up.
	var required []string
	for _, condition := range contract.AuthorityConditions {
		if condition.Action == action {
			required = append(required, condition.RequiredClaims...)
		}
	}
	for _, obligation := range contract.Obligations {
		if obligation.Material {
			required = append(required, obligation.RequiredClaims...)
		}
	}
	var unsupported []UnsupportedEvidenceRequirement
	{
		seen := map[string]bool{}
		for _, claimID := range required {
			if seen[claimID] {
				continue
			}
			seen[claimID] = true
			claim, defined := contract.RequiredClaims[claimID]
			if !defined {
				// An undefined claim is refused by the compiler long before
				// here; treating it as unsupported keeps this total.
				unsupported = append(unsupported, UnsupportedEvidenceRequirement{ClaimID: claimID})
				continue
			}
			if !producible[claim.EvidenceClass] {
				unsupported = append(unsupported, UnsupportedEvidenceRequirement{ClaimID: claimID, EvidenceClass: claim.EvidenceClass})
			}
		}
	}
	sort.Slice(unsupported, func(i, j int) bool { return unsupported[i].ClaimID < unsupported[j].ClaimID })
	return unsupported
}

// SemanticClaimRequest is one acceptance question, stated by the runtime. The
// verifier is told which claim and which obligations it is judging; it cannot
// choose them.
type SemanticClaimRequest struct {
	ClaimID       string
	ObligationIDs []string
	Statements    []string
}

type AssuranceRequest struct {
	RunID, Commit, Tree, CheckoutDir string
	// Attempt is the SCHEDULER's attempt number for the assurance operation.
	// It exists so a verification writes evidence under an identity a replay
	// arrives at from the journal alone, exactly as #55 requires of a provider
	// invocation.
	Attempt int
	// Confirmation marks the SECOND verification of one candidate that
	// AssuranceRerun performs to tell a flake from a real failure. Both
	// verifications are evidence - the flake verdict is derived by comparing
	// them - so each writes its own immutable transcript instead of one
	// replacing the other.
	Confirmation       bool
	Contract           Ref
	Policy             Ref
	Producer           Ref
	VerifierDefinition string
	// The fields below are used by the semantic verifier. They are bounded,
	// read-only context: identity, the exact base the diff is taken against, the
	// changed-path inventory, a summary of the automated result, and the exact
	// claims to judge. None of them is a capability.
	Repository         string
	Base               string
	Objective          string
	ChangedPaths       []string
	AutomatedAssurance string
	SemanticClaims     []SemanticClaimRequest
}
type AssuranceResult struct {
	ProviderID, VerifierDefinition string
	Passed                         bool
	FailureClass                   FailureClass
	Artifacts                      []Artifact
	Evidence                       *EvidenceBinding
	// ArtifactRef is the immutable, attempt-scoped reference to the transcript
	// this verdict was read from, and FailureSignature identifies the failure
	// itself rather than the verifier that found it. Together they are what
	// lets a remediation agent tell this failure from the previous one; see
	// runtime/assurance_evidence.go.
	ArtifactRef      string
	FailureSignature string
	// Model and Tokens are recorded only when the producer actually reports
	// them. Nothing here is invented when a provider does not expose usage.
	Model  string
	Tokens int64
	// SemanticClaims is the per-claim observation of a semantic verifier. A
	// verdict is claim-specific: one claim may be discharged while another is
	// not, and a bundle must say so rather than collapsing to one boolean.
	SemanticClaims map[string]SemanticClaimVerdict
}

// EvidenceBinding is adapter output, not an AuthorityDecision. The caller
// builds the domain EvidenceBundle only after validating this exact binding.
type EvidenceBinding struct {
	Commit, Tree                            string
	Contract, Policy, Producer, Environment Ref
}

// GuardCandidate is the PATH layer, and only the PATH layer: normalization,
// credential-file shapes, symlinked leaves and the size ceiling. It rejects
// unsafe additions but never removes otherwise safe, out-of-contract changes:
// those must be reassessed by the kernel.
//
// It deliberately no longer reads file CONTENT. A content predicate here was
// answering a different question - "is this a credential value?" - at a layer
// that cannot enforce the answer: candidate.run mounts the same workspace and
// reads whatever it likes, so refusing a path here never protected anything it
// appeared to protect. That question is asked where it can be enforced:
// ScanCandidateForCredentialValues before a producer is admitted,
// RedactCredentialValues on every model-visible tool result, and the commit
// gate in CandidateWorkspace.Commit. See credential_boundary.go.
//
// It is the COMPOSITION of the two halves below, for a caller whose observed
// paths are exactly the paths it is about to commit - a brokered tool write is
// one, because the one path it names is the one path it writes. A caller that
// observes more than it commits must ask the two halves separately, so that a
// path it has already decided not to commit cannot veto the paths it does. See
// CandidateWorkspace.Commit.
func GuardCandidate(root string, paths []string, maxBytes int64) error {
	if err := GuardCandidatePathShape(root, paths); err != nil {
		return err
	}
	return GuardCandidateCommitContent(root, paths, maxBytes)
}

// GuardCandidatePathShape is the half that protects the RUNTIME from a path,
// and it applies to every path the workspace reported.
//
// Normalization, traversal, absolute paths and symlinked leaves are questions
// about what the runtime is about to touch on this filesystem - it joins these
// names onto the workspace root and stats them - so they are asked about
// anything observed, committed or not.
func GuardCandidatePathShape(root string, paths []string) error {
	for _, p := range paths {
		normalized, err := normalizedCandidatePath(p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(filepath.Join(root, normalized))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return deterministicPathRefusal("candidate.symlink", normalized, fmt.Errorf("symlink candidate path %q", normalized))
		}
	}
	return nil
}

// GuardCandidateCommitContent is the half that protects the COMMIT, and it
// applies only to the paths that will be in it.
//
// The sensitive-name refusal and the size ceiling are both statements about the
// object the runtime is about to publish. Asking them of a path the runtime has
// already decided it cannot carry lets that path veto a commit it is not in:
// an inherited scratch repository called `.env` is not a credential in the
// candidate, and its bytes are not candidate bytes, because neither reaches the
// tree. The gates are unchanged for everything that does reach it.
func GuardCandidateCommitContent(root string, paths []string, maxBytes int64) error {
	return guardCommitNamesAndSizes(paths, maxBytes, func(normalized string) (int64, bool) {
		info, err := os.Lstat(filepath.Join(root, normalized))
		if err != nil {
			return 0, false
		}
		return info.Size(), true
	})
}

// guardCommitNamesAndSizes is the one implementation of the sensitive-name and
// size-ceiling rules. sizeOf answers for a normalized path, and false means the
// path carries no bytes (a deletion), so it adds nothing to the total. The
// worktree form above and the staged-blob form in CandidateWorkspace.Commit
// differ only in where the size comes from.
func guardCommitNamesAndSizes(paths []string, maxBytes int64, sizeOf func(normalized string) (int64, bool)) error {
	var total int64
	for _, p := range paths {
		normalized, err := normalizedCandidatePath(p)
		if err != nil {
			return err
		}
		// Credential-file SHAPES, not substrings. The predicate here used to
		// match "secret", "private" and "credential" anywhere in a base name,
		// which made secret_scanner.go, private_key_parser.go and
		// credential_policy.go permanently unopenable by the engineering
		// system that has to maintain them.
		if sensitiveCredentialFilename(filepath.Base(normalized)) {
			return deterministicPathRefusal("candidate.sensitive_path", normalized, fmt.Errorf("sensitive candidate path %q", normalized))
		}
		size, ok := sizeOf(normalized)
		if !ok {
			continue
		}
		total += size
		if maxBytes > 0 && total > maxBytes {
			return deterministicRefusal("candidate.size_limit", fmt.Errorf("candidate exceeds size ceiling"))
		}
	}
	return nil
}

func normalizedCandidatePath(p string) (string, error) {
	normalized, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: []string{p}, PathsKnown: true})
	if err != nil || filepath.IsAbs(p) || len(normalized.Paths) != 1 {
		return "", deterministicRefusal("candidate.path_shape", fmt.Errorf("unsafe candidate path %q", p))
	}
	return normalized.Paths[0], nil
}

// VerificationSurfaceChanged identifies candidate-controlled verifier inputs.
func VerificationSurfaceChanged(paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if strings.HasSuffix(p, "_test.go") || p == "go.mod" || p == "go.sum" || strings.HasPrefix(p, ".github/workflows/") {
			return true
		}
	}
	return false
}

const (
	FailureFormat                  FailureClass = "format"
	FailureCompileTest             FailureClass = "compile_or_test"
	FailureVerification            FailureClass = "verification_failure"
	FailureSurface                 FailureClass = "verification_surface_changed"
	FailureWeakened                FailureClass = "verification_weakened"
	FailureTransientInfrastructure FailureClass = "transient_infrastructure"
	FailureMaterialScope           FailureClass = "material_scope_change"
	FailureAuthorityWait           FailureClass = "authority_wait"
	// FailureStateStorageExhausted is the operator's local state ceiling being
	// reached before a candidate workspace was allocated. It is detected BEFORE
	// the clone, so nothing is half-written and the run's existing state is
	// untouched.
	//
	// It waits rather than fails: the engineering work is fine, the machine is
	// full. An operator frees space or raises the bound and the same run
	// continues against the same candidate. Nothing is ever reclaimed
	// automatically to make room - trading one active run's evidence for
	// another's progress is not a decision a scheduler gets to make.
	FailureStateStorageExhausted FailureClass = "state_storage_exhausted"

	// FailureExecutionDeadlineExceeded is a provider that returned AFTER its
	// durable execution deadline, whatever the adapter reported.
	//
	// It is terminal rather than retryable on purpose: the deadline has passed,
	// so a retry has no remaining authority to spend and would be an immediate
	// second expiry. It is also not a statement about the work - the candidate
	// may be perfect - it is a statement that the authority to produce it had
	// already ended.
	FailureExecutionDeadlineExceeded FailureClass = "execution_deadline_exceeded"

	// FailureAssurancePrerequisite is the ENVIRONMENT the verifier needs not
	// being there: the configured image resolves no toolchain, the
	// operator-provisioned dependency cache is missing or empty, or the exact
	// tree needs a module the trusted offline cache does not hold.
	//
	// No verdict about the candidate was reached, so it is not a verification
	// failure. Re-running the identical command against the identical
	// environment produces the identical result, so it is not transient either -
	// classifying it as transient infrastructure is what let one deterministic
	// fault consume every assurance attempt in seconds. It waits: an operator
	// provisions what is missing and the same run re-derives assurance against
	// the same exact commit, tree and contract.
	//
	// It is deliberately NOT FailureAuthorityWait. Nothing about human authority
	// is involved.
	FailureAssurancePrerequisite FailureClass = "assurance_prerequisite_unavailable"
	// FailureToolchainUnavailable is the WORKER's environment lacking a tool its
	// contract obligates it to run.
	//
	// It is the producer-side twin of FailureAssurancePrerequisite and routes
	// the same way, for the same reason: nothing about the work is wrong, an
	// operator has to change the environment, and retrying or asking a model to
	// fix it would spend budget on a condition no reasoning can clear. It is
	// deliberately not a verification failure - no candidate was judged - and
	// not a transient one, because a missing toolchain does not come back on
	// its own.
	FailureToolchainUnavailable FailureClass = "toolchain_unavailable"
	// FailureGovernedRemoteMismatch is a deterministic trust refusal: the
	// remote a workspace is bound to is not this run's governed remote.
	//
	// It routes to a STOP, not a retry and not a wait. Retrying cannot change
	// the answer - the governed remote is configuration and nothing the runtime
	// does alters it - and waiting would be a lie about what an operator can
	// fix in place: changing the governed remote changes which repository the
	// run is about, which is a different trusted subject and therefore a
	// different run. It is not a producer failure, not a verification verdict,
	// and not an authority condition.
	FailureGovernedRemoteMismatch FailureClass = "governed_remote_mismatch"
	// FailureRequiredEvidenceUnsupported is a contract this configured system
	// can never satisfy: the claim gating a protected action names an evidence
	// class no configured producer declares and no human records.
	//
	// It routes to a STOP, and it is detected BEFORE any model budget is spent.
	// Retrying cannot conjure a producer, and waiting would imply an operator
	// could clear it in place - they cannot: the fix is a different policy or a
	// different configured producer, either of which changes the terms the run
	// is governed by and therefore needs a new run. Discovering it after the
	// work is done, as run-0943e257539346f8763db04505cbf322 did, is the defect.
	FailureRequiredEvidenceUnsupported FailureClass = "required_evidence_unsupported"
	// FailureCandidateCredentialMaterial is a high-confidence credential VALUE
	// present in candidate-visible content. It is a local prerequisite defect,
	// detected BEFORE any producer is admitted, so no reasoning iteration is
	// spent discovering it and no provider request is made.
	//
	// It is not a producer failure, not provider capacity, and not iteration
	// exhaustion - calling it any of those would tell an operator to look at
	// the model when the fact to look at is a secret in the workspace. It
	// routes to a STOP: retrying reads the same bytes, and the candidate is
	// runtime-owned, so there is nothing an operator clears in place. What
	// clears it is removing the material from what the run is about, which is
	// a different subject and therefore a different run.
	FailureCandidateCredentialMaterial FailureClass = "candidate_credential_material"
	// FailureCandidateGuardUnavailable is the controller unable to install its
	// own #241 boundary: the composition requires the brokered Git guard and
	// could not resolve the executable that enforces it.
	//
	// It is NOT a provider failure and must not be reported as one. Nothing
	// about the worker, the work, the provider's account or the network is
	// wrong; the runtime cannot enforce a law it holds itself to, so it
	// performs no execution at all. It is detected BEFORE dispatch, so no
	// invocation is spent and no candidate is touched.
	//
	// It waits rather than stopping: an operator repairs the controller
	// installation and the same run continues against the same candidate.
	// Terminalizing a run because the controller could not name its own
	// executable would destroy work over a condition that is entirely local
	// and entirely fixable.
	FailureCandidateGuardUnavailable FailureClass = "candidate_guard_unavailable"
	// FailureCandidateWriterAlive is a candidate whose writer lock is still
	// held by a process from an earlier invocation - one that outlived its
	// supervisor (#168). Refused before dispatch, it waits: an operator stops
	// the stale writer and the same run continues.
	FailureCandidateWriterAlive    FailureClass = "candidate_writer_alive"
	FailureGovernanceMismatch      FailureClass = "governance_mismatch"
	FailureWorkspaceIntegrity      FailureClass = "workspace_integrity_violation"
	FailureBaseIntegrationConflict FailureClass = "base_integration_conflict"
	FailureFlaky                   FailureClass = "flaky_verification"
	// FailureIntegrationConflict is a WorkGraph integration unit's (#475)
	// deterministic composition blocked - a Git textual conflict, or a clean
	// merge whose independent assurance then failed (semantic/uncertain).
	// Unlike FailureBaseIntegrationConflict it never routes to the provider:
	// an integrator unit never receives implementation authority, so there is
	// no producer remediation to dispatch it to.
	FailureIntegrationConflict FailureClass = "integration_conflict"
	// FailureIntegrationInvalidated is one consumed input this attempt could
	// not read at its admitted subject - superseded, unreadable, or not a
	// descendant of the verified base. A retry re-reads live state and may
	// legitimately still find it true; exhausting the attempt budget fails
	// the run, which the WorkGraph's own dependents already fail closed on.
	FailureIntegrationInvalidated FailureClass = "integration_invalidated"
	// FailureFeedbackUnresolved is an invocation delivered admitted feedback
	// that returned without discharging it: the workspace it left behind is
	// unchanged, and it did not state (or failed to bind) an explicit
	// no_change_required resolution. See FeedbackResolution.
	//
	// Provider return is not proof of semantic completion (#376): a producer
	// that defers to background work it never finishes, or that simply
	// misreads admitted feedback, returns exactly this way - indistinguishable
	// from one that legitimately needed to do nothing, right up until it is
	// asked to STATE that rather than have it inferred. So neither shape is
	// read as success; both are this class, and only a bound, admitted
	// resolution (or a mutation) escapes it.
	//
	// It routes to a bounded RETRY of the SAME execution.invoke operation,
	// under that operation's existing attempt ceiling - no budget is minted
	// or reset, and a provider that keeps returning unresolved exhausts its
	// attempts and stops truthfully, exactly like any other producer failure
	// that never lands. It is deliberately excluded from
	// PriorAttemptContextEligible: the provider was not cut short by a
	// runtime bound mid-task, so the retry gets a fresh invocation rather
	// than observations from an attempt that said nothing.
	FailureFeedbackUnresolved FailureClass = "feedback_unresolved"
	// FailureReviewRemediationUnresolved is the same shape as
	// FailureFeedbackUnresolved (#376), bound to an admitted independent-review
	// BLOCK (#474) rather than GitHub feedback: an invocation delivered
	// admitted review-remediation findings that returned without discharging
	// them, having neither mutated the candidate nor stated an admitted
	// no_change_required resolution naming them.
	//
	// A successful, non-mutating provider return is not proof the BLOCK was
	// addressed - the exact #474 B1 gap a reviewer's REQUEST_CHANGES verdict
	// would otherwise let slip past as a quietly completed operation, with
	// nothing left wanting a successor. Only a bound, admitted resolution (or
	// a mutation) escapes it.
	//
	// It routes to a bounded RETRY of the SAME execution.invoke operation, the
	// same shape FailureFeedbackUnresolved uses: no budget is minted or reset,
	// and a producer that keeps returning unresolved exhausts its attempts and
	// stops truthfully, exactly like any other producer failure that never
	// lands.
	FailureReviewRemediationUnresolved FailureClass = "review_remediation_unresolved"
	// FailureCheckpointContinuationUnresolved is a continuation invocation -
	// one that inherited a runtime-owned checkpoint, interrupted rather than
	// finished work - that returned without settling it: it did not state (or
	// failed to bind) an explicit FeedbackResolutionCheckpointComplete claim
	// bound to the exact checkpoint revision and tree it was shown. See
	// FeedbackResolution.
	//
	// Provider return is not proof of semantic completion (#379, generalizing
	// #376 from feedback discharge to checkpoint continuation): a continuation
	// that defers to background work it never finishes, or that simply
	// misreads the checkpoint, returns exactly this way - indistinguishable
	// from one that legitimately needed to do nothing further, right up until
	// it is asked to STATE that rather than have it inferred. Mutation does
	// NOT escape this class by itself: it proves work happened, not that the
	// inherited checkpoint is finished, so only a bound, admitted completion
	// claim escapes it, mutated or not.
	//
	// It routes to a bounded RETRY of the SAME execution.invoke operation,
	// under that operation's existing attempt ceiling - no budget is minted
	// or reset, and a continuation that keeps returning unresolved exhausts
	// its attempts and stops truthfully, exactly like any other producer
	// failure that never lands. It is deliberately excluded from
	// PriorAttemptContextEligible: the provider was not cut short by a
	// runtime bound mid-task, so the retry gets a fresh invocation rather
	// than observations from an attempt that said nothing.
	FailureCheckpointContinuationUnresolved FailureClass = "checkpoint_continuation_unresolved"
	// FailureReviewerProtocolIncomplete is a reviewer-role invocation that
	// completed - the process exited cleanly, within its bounds - without
	// crossing the reviewer-result protocol (reviewer_result.go): the result
	// file it wrote failed to decode as one, or it wrote none at all.
	//
	// It is deliberately NOT FailureVerification. That class means a verdict
	// WAS reached and the candidate was judged and found wanting; this class
	// means no verdict was reached at all. A reviewer that produced malformed
	// JSON, or stdout but no result file, has refused to answer the question
	// it was asked - that is a defect of THIS invocation's protocol
	// compliance, not an observation about the candidate under review, and
	// folding it into FailureVerification is what let a reviewer's own broken
	// output read as the candidate having failed review (#374).
	//
	// It routes to a bounded RETRY of the SAME execution.invoke operation, the
	// same shape FailureFeedbackUnresolved uses: no budget is minted or reset,
	// and a reviewer that keeps failing the protocol exhausts its attempts and
	// stops truthfully. The exact reason is kept (ReviewerResultRefusedError)
	// and returned to the reviewer on the retry as a finding, so correction is
	// possible instead of the reviewer guessing what was wrong the first time.
	FailureReviewerProtocolIncomplete FailureClass = "reviewer_protocol_incomplete"
	// FailureDecisionBindingStale is a decision-resumed execution.invoke
	// attempt whose own binding - the exact decision set and candidate
	// subject it was created against - no longer matches current durable
	// state: one of its decisions was superseded, a newer one now claims the
	// coalesced set, or the subject moved, all found by a fresh read taken
	// immediately before dispatch (#508 review P4b §6). Nothing was
	// attempted; it routes to a bounded retry of the SAME operation, the
	// same shape FailureFeedbackUnresolved uses, so a genuinely stale
	// binding exhausts its own attempts rather than ever being dispatched
	// with context that would contradict it.
	FailureDecisionBindingStale FailureClass = "decision_binding_stale"
	// FailureReviewRemediationStale is the same shape as
	// FailureDecisionBindingStale, for an admitted independent-review BLOCK
	// (#474 B2): a live GitHub read taken immediately before the provider is
	// launched disagreed with the head the admitted findings were assembled
	// against, or GitHub was unreachable for that read. Nothing was
	// attempted; it routes to a bounded retry of the SAME operation, so a
	// genuinely moved PR exhausts its own attempts rather than ever
	// delivering findings whose subject may already be gone.
	FailureReviewRemediationStale FailureClass = "review_remediation_stale"
)

type FailureRoute string

const (
	RouteGofmt               FailureRoute = "remediation.gofmt"
	RouteProviderRemediation FailureRoute = "execution.remediation"
	RouteRetry               FailureRoute = "retry"
	RouteReassess            FailureRoute = "reassess"
	RouteWait                FailureRoute = "wait"
	RouteRestore             FailureRoute = "restore_or_refuse"
	RouteStop                FailureRoute = "stop"
)

func RouteFailure(c FailureClass) FailureRoute {
	switch c {
	case FailureFormat:
		return RouteGofmt
	// A VERDICT ABOUT THE CANDIDATE routes to the producer that made it.
	//
	// FailureVerification is the class the Go verifier, the semantic verifier,
	// an internal reviewer's blocking verdict and admitted forge feedback all
	// produce: the candidate was judged and not accepted. It was absent from
	// every arm here and fell to RouteStop, which meant the primary
	// verification failure class in the system planned no operation at all -
	// not remediation, and not a terminal failure either. The run then took the
	// `!wanted` branch in Reconcile and settled waiting/goal_state_reached, a
	// stage built on it settled "completed", and its dependents were released
	// against work that had failed. That is the #126 chain, and it starts here.
	//
	// It sits beside FailureCompileTest deliberately: both are statements that
	// the work is wrong, both are answerable by the worker that produced it,
	// and both are bounded by the same remediation budget. Exhausting that
	// budget is what makes it terminal; being unrouted never should have.
	case FailureCompileTest, FailureBaseIntegrationConflict, FailureVerification:
		return RouteProviderRemediation
	case FailureTransientProvider, FailureTransientInfrastructure, FailureExecutionIncomplete,
		FailureProviderNoProgress, FailureFeedbackUnresolved, FailureReviewRemediationUnresolved,
		FailureCheckpointContinuationUnresolved, FailureReviewerProtocolIncomplete,
		FailureProviderBackgroundWorkUnresolved, FailureConnectivity, FailureDecisionBindingStale,
		FailureReviewRemediationStale:
		return RouteRetry
	// An integration unit never gets a producer to remediate it (#475): its
	// whole producer stage is deterministic composition, not a free-form
	// invocation. A bounded retry re-reads live state through the same
	// unconditional live-currency check every attempt makes; exhausting the
	// attempt budget is what makes either class terminal, same as any other
	// bounded retry here.
	case FailureIntegrationConflict, FailureIntegrationInvalidated:
		return RouteRetry
	case FailureMaterialScope, FailureSurface, FailureWeakened, FailureGovernanceMismatch:
		return RouteReassess
	case FailureWorkspaceIntegrity:
		return RouteRestore
	// Stopping is the point: there is no authority left to route anywhere. The
	// deadline has passed or the operator has stopped the run, and in both
	// cases a retry would inherit exactly the revocation that ended this one.
	case FailureExecutionDeadlineExceeded, FailureRunCancelled:
		return RouteStop
	case FailureAuthorityWait, FailureProviderAccountUnavailable, FailureAssurancePrerequisite,
		FailureToolchainUnavailable, FailureProviderQuota, FailureProviderRateLimited,
		FailureStateStorageExhausted, FailureControllerShutdown, FailureProviderUnavailable,
		FailureCandidateGuardUnavailable, FailureCandidateWriterAlive:
		return RouteWait
	default:
		return RouteStop
	}
}

// CandidateVerdict reports whether an assurance result is a valid verdict
// about the CANDIDATE: a pass, or a failure the verifier judged (format,
// compile/test, verification, or an unpassed result naming no class, which is
// read as verification). This is the one definition. Everything else - an
// infrastructure fault, a cancellation, a missing prerequisite - says nothing
// about the candidate, and only two verdicts can disagree as a flake.
func CandidateVerdict(r AssuranceResult) bool {
	if r.Passed {
		return true
	}
	switch r.FailureClass {
	case "", FailureFormat, FailureCompileTest, FailureVerification:
		return true
	}
	return false
}

// PriorAttemptContextEligible reports whether a retry of the same execution
// binding may inherit the previous attempt's observations.
//
// Only a runtime-bounded incomplete execution qualifies. That class means the
// runtime itself cut a reasoning loop short with work still to do, so the next
// attempt continues the same engineering task and re-reading the same files is
// pure waste - which is the finding this rule exists for.
//
// The other reattemptable classes are excluded deliberately. A transient
// provider or infrastructure failure says nothing about the work: the attempt
// it ended may have reached no capability at all, and policy is that such a
// retry starts fresh. Every remaining class either routes to a DIFFERENT
// operation or stops the run. An empty classification is not eligible either,
// so a retry can never acquire history by having no recorded reason - unknown
// failures stay fail-closed here exactly as they do in RouteFailure.
func PriorAttemptContextEligible(prior FailureClass) bool {
	return prior == FailureExecutionIncomplete
}

// MergePrecedence is deliberately observation-only: merged always wins over
// issue closure, including when a new controller would otherwise block work.
func MergePrecedence(merged, issueClosed bool) (Disposition, string) {
	if merged {
		return Completed, "merged"
	}
	if issueClosed {
		return Waiting, "source_closed"
	}
	return Active, ""
}

// HumanAuthorityBinding prevents an approval from being carried onto a moved
// candidate or contract. It intentionally does not perform a merge.
type HumanAuthorityBinding struct {
	RunID, CandidateRevision, CandidateTree string
	Contract                                Ref
	Action                                  string
	Decision                                string
	HumanID                                 string
}

func (b HumanAuthorityBinding) Validate(s RunSnapshot) error {
	if b.RunID != s.ID || b.CandidateRevision != s.Candidate.Revision || b.CandidateTree != s.Candidate.Tree || b.Contract != s.Contract {
		return fmt.Errorf("stale human authority binding")
	}
	if b.Decision != "approve" && b.Decision != "reject" {
		return fmt.Errorf("invalid human decision")
	}
	return nil
}

// InvocationModeUnsupportedError is the typed refusal for an invocation an
// adapter cannot perform in the mode the work requires.
//
// It exists so a planner-role stage whose provider has no provable read-only
// mode produces an EXPLAINABLE ineligibility - which the resolver turns into
// another eligible selection or a typed blocked state - rather than a silent
// downgrade into a mode that may write.
type InvocationModeUnsupportedError struct {
	AgentID string
	Kind    string
	Mode    domain.InvocationMode
	Detail  string
}

func (e *InvocationModeUnsupportedError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = "this provider exposes no mode whose non-mutating boundary the runtime can prove, so it is ineligible rather than run permissively"
	}
	return fmt.Sprintf("agent %q (%s) cannot perform a %q invocation: %s", e.AgentID, e.Kind, e.Mode, detail)
}
