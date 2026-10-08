package runtime

import "fmt"

// AdoptedBuildProvenance is the deterministic record. It is the artifact, and
// it is also the return value: a caller that wants to know what was built asks
// the same object the file holds.
type AdoptedBuildProvenance struct {
	SchemaVersion string          `json:"schema_version"`
	Repository    string          `json:"repository"`
	TrustRoot     TrustRootRecord `json:"trust_root"`
	// TrustedMain is the revision adoption stood on. In adopted-build/1 it was
	// main_head as well; in adopted-build/2 it is RESOLVED (ADR-0007), and
	// the fields below say from where and on what evidence.
	TrustedMain RevisionRecord `json:"trusted_main"`
	// MainHead, TrustedRevisionPolicy, TrustEvidence and Skipped are
	// adopted-build/2. They are omitted, not zero, in a v1 record so a v1
	// record re-encodes byte for byte; read one through Projected.
	MainHead              *RevisionRecord        `json:"main_head,omitempty"`
	TrustedRevisionPolicy *TrustedRevisionPolicy `json:"trusted_revision_policy,omitempty"`
	TrustEvidence         *TrustEvidenceRecord   `json:"trust_evidence,omitempty"`
	Skipped               []SkippedRevision      `json:"skipped,omitempty"`
	Source                RevisionRecord         `json:"source"`
	Containment           string                 `json:"containment_proof"`
	Kind                  string                 `json:"controller_kind"`
	Version               string                 `json:"version"`
	GOOS                  string                 `json:"goos"`
	GOARCH                string                 `json:"goarch"`
	BuildFlags            []string               `json:"build_flags"`
	BuildEnv              BuildEnvironment       `json:"build_environment"`
	BinarySHA256          string                 `json:"binary_sha256"`
	BuiltAt               string                 `json:"built_at"`
	Builder               BuilderRecord          `json:"builder"`
	OutputPath            string                 `json:"output_path"`
	SelfProbe             SelfProbeRecord        `json:"self_probe"`
}

// SelfProbeRecord is the binary's own account of itself, checked against what
// was asked for. There is no "not probed" state: an unprobed build is refused,
// never recorded.
type SelfProbeRecord struct {
	Kind         string `json:"kind"`
	Version      string `json:"version"`
	Revision     string `json:"source_revision"`
	Tree         string `json:"source_tree"`
	BinarySHA256 string `json:"binary_sha256"`
	Matched      bool   `json:"matched"`
}

type TrustRootRecord struct {
	RulesetID   int64                 `json:"ruleset_id"`
	Name        string                `json:"name"`
	Digest      string                `json:"digest"`
	Policy      TrustRootPolicyRecord `json:"policy"`
	Enforcement string                `json:"enforcement"`
	// ObservedBy is the provenance of the identity that disclosed this gate,
	// with no secret in it. It is recorded because "the ruleset discloses no
	// bypass actor" and "the identity that asked was not shown any" are
	// different statements, and a reader of the evidence must be able to tell
	// which one was made.
	ObservedBy CredentialProvenance `json:"observed_by"`
	// Bypass is that same distinction written down rather than left to be
	// inferred from a zero. An auditor reading this record months later must
	// be able to see which of the two facts the build actually established.
	Bypass BypassDisclosure `json:"bypass"`
}

// BypassDisclosure is what the governance observation ESTABLISHED about who can
// bypass the trust root.
//
// The zero value is "nothing was established", which is the honest reading of a
// record that carries no disclosure - an absent fact is not a favourable fact,
// and that is true of a record as much as of a decision. Observed is therefore
// a separate member from Count rather than Count being allowed to speak for
// both: {observed:false, count:0} and {observed:true, count:0} are opposite
// statements that a bare zero would have collapsed into one.
//
// A successful adopted build can only ever write {observed:true, count:0}: any
// other combination is refused before a record exists, so every combination the
// type can express is either that one or evidence that something was refused.
type BypassDisclosure struct {
	// Observed reports that the forge actually disclosed the bypass actor set
	// to the governance identity that asked.
	Observed bool `json:"observed"`
	// Count is the size of that set, meaningful only when Observed is true.
	Count int `json:"count"`
	// Detail states which of the two facts this is in one sentence, so the
	// record cannot be misread by someone skimming for a number.
	Detail string `json:"detail"`
}

// describeBypass turns an observed ruleset into the recorded disclosure. It
// reads the SAME two fields VerifyTrustRoot decides on, so the evidence and the
// decision can never disagree about what was seen.
func describeBypass(root TrustedMainRuleset) BypassDisclosure {
	if !root.BypassActorsKnown {
		return BypassDisclosure{
			Detail: "the forge disclosed no bypass actor set to the governance identity, so nothing about bypasses was established by this observation",
		}
	}
	return BypassDisclosure{
		Observed: true, Count: root.BypassActors,
		Detail: fmt.Sprintf("the governance identity was shown the bypass actor set and it contained %d actor(s)", root.BypassActors),
	}
}

type RevisionRecord struct {
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

// BuilderRecord is the identity of the tool that produced the artifact. It may
// truthfully be unattested - the builder need not itself be adopted - but it
// must not LIE: a builder whose own attestation could not be resolved records
// that fact rather than laundering it into "unattested", which would claim a
// deliberate absence of provenance where there is a failed measurement.
type BuilderRecord struct {
	Version         string `json:"version"`
	SourceRevision  string `json:"source_revision"`
	Kind            string `json:"kind"`
	ResolutionError string `json:"resolution_error,omitempty"`
}

const adoptedBuildSchemaVersion = "adopted-build/2"

// TrustEvidenceKind says what made TrustedMain trusted.
type TrustEvidenceKind string

const (
	// TrustEvidenceT2 is an exact-revision T2 observation (adopted-build/2).
	TrustEvidenceT2 TrustEvidenceKind = "t2"
	// TrustEvidenceLegacy is an adopted-build/1 record. M1-B's guarantee was
	// that the branch moved under a strict required-"go" ruleset, not that the
	// resulting commit itself produced T2; no observation was made, so none is
	// stated.
	TrustEvidenceLegacy TrustEvidenceKind = "legacy"
)

type TrustEvidenceRecord struct {
	Kind        TrustEvidenceKind      `json:"kind"`
	Observation *T2EvidenceObservation `json:"observation,omitempty"`
}

const legacyAdoptedBuildSchemaVersion = "adopted-build/1"

// Projected reads any supported record as an adopted-build/2 one, and refuses
// a schema it does not know. A v1 record projects faithfully to the M1-B
// model and nothing more: main_head == trusted_main, trust evidence legacy,
// nothing skipped. No T2 observation is synthesized for it.
func (p AdoptedBuildProvenance) Projected() (AdoptedBuildProvenance, error) {
	switch p.SchemaVersion {
	case adoptedBuildSchemaVersion:
		return p, p.validateV2()
	case legacyAdoptedBuildSchemaVersion:
		head := p.TrustedMain
		p.MainHead = &head
		p.TrustEvidence = &TrustEvidenceRecord{Kind: TrustEvidenceLegacy}
		p.Skipped = []SkippedRevision{}
		return p, nil
	default:
		return p, fmt.Errorf("adopted-build schema %q is not one this controller can read", p.SchemaVersion)
	}
}

// validateV2 refuses an adopted-build/2 record whose trust evidence does not
// prove its own trusted_main. The monotonic floor is read from persisted
// provenance, so a record that merely has the right shape must not pass.
func (p AdoptedBuildProvenance) validateV2() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("adopted-build/2 record for %s is invalid: %s", shortSHA(p.TrustedMain.Revision), fmt.Sprintf(format, args...))
	}
	if p.MainHead == nil {
		return invalid("no main_head")
	}
	if p.TrustedRevisionPolicy == nil {
		return invalid("no trusted_revision_policy")
	}
	if p.TrustEvidence == nil || p.TrustEvidence.Kind != TrustEvidenceT2 || p.TrustEvidence.Observation == nil {
		return invalid("no T2 trust evidence")
	}
	evidence, policy := p.TrustEvidence.Observation, p.TrustedRevisionPolicy
	// THE POLICY IS FROZEN, so a record cannot carry its own. A record that
	// changed its policy and its deciding attempt together would otherwise be
	// internally consistent while proving nothing about the real producer.
	if *policy != DefaultTrustedRevisionPolicy() {
		return invalid("its trusted_revision_policy is not the frozen policy")
	}
	// AUTHORITY IS RECOMPUTED FROM THE RAW ATTEMPTS. Eligible, Inconsistent
	// and Deciding are cached conclusions; a record whose cache says success
	// while its attempts say otherwise is refused, not believed.
	recomputed := EvaluateT2Evidence(*policy, evidence.Subject, evidence.Attempts)
	switch {
	case len(recomputed.Attempts) != len(evidence.Attempts):
		return invalid("its T2 evidence carries attempts the policy does not accept")
	case !recomputed.Eligible:
		return invalid("its raw T2 attempts do not make %s eligible", shortSHA(evidence.Subject))
	case recomputed.Eligible != evidence.Eligible || recomputed.Inconsistent != evidence.Inconsistent ||
		evidence.Deciding == nil || !sameAttempt(*recomputed.Deciding, *evidence.Deciding):
		return invalid("its recorded T2 conclusion disagrees with its raw attempts")
	case evidence.Subject != p.TrustedMain.Revision:
		return invalid("its T2 evidence is about %s", shortSHA(evidence.Subject))
	case evidence.Tier != policy.Tier:
		return invalid("evidence tier %q is not the policy's %q", evidence.Tier, policy.Tier)
	}
	return nil
}

// sameAttempt compares the identity and outcome of two attempts. Times are not
// compared: they are reported values, not part of the decision.
func sameAttempt(a, b T2Attempt) bool {
	return a.RunID == b.RunID && a.Attempt == b.Attempt && a.IntegrationID == b.IntegrationID &&
		a.HeadSHA == b.HeadSHA && a.Job == b.Job && a.Status == b.Status && a.Conclusion == b.Conclusion
}
