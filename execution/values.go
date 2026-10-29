package execution

import (
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// UpstreamContext is one completed upstream producer's output as the downstream
// work that consumes it sees it: a plan stage's output to a later stage, or a
// WorkGraph unit's admitted handoff to the unit that depends on it (#472).
type UpstreamContext struct {
	// StageID is the producer's identity in whichever graph names it: a plan
	// stage id, or a WorkGraph unit id.
	StageID string
	RunID   string
	Commit  string
	Tree    string
	// Handoff is the admitted #470 handoff this output was transferred by, for
	// a WorkGraph unit's upstream. It is what makes a dependency edge DELIVER
	// rather than only gate: the consuming invocation is given the exact
	// handoff its activation was bound to, not merely a commit.
	//
	// Nil for a plan stage, which has no handoff. Every field inside it is
	// worker-authored, and it is framed as untrusted data like the diff.
	Handoff *UpstreamHandoff
	// Diff is the change that stage produced, bounded by the runtime. Empty
	// means the runtime could not read it, which is stated rather than hidden.
	Diff string
	// Truncated reports that the diff was cut to the runtime's bound, so a
	// reviewer knows it is reading part of a change rather than all of it.
	Truncated bool
	// Assurance is the runtime's latest exact-head verifier observation, not
	// producer-authored evidence or a substitute for independent review.
	Assurance []AssuranceContext
}

// AssuranceContext is one runtime-owned verification observation for an
// upstream candidate.
type AssuranceContext struct {
	ProviderID         string
	VerifierDefinition string
	Passed             bool
	Commit             string
	Tree               string
	BundleID           string
	BundleRevision     string
}

// UpstreamHandoff is the producer's own admitted report, as the consumer sees
// it. The runtime owns the identity; the worker wrote everything else.
type UpstreamHandoff struct {
	ID              string
	Outcome         string
	Summary         string
	Unresolved      []string
	RecommendedNext []string
}

type Finding struct {
	Classification FailureClass
	// Verifier is the identity of what OBSERVED the failure, kept separate
	// from what the failure IS. Collapsing the two into one signature made
	// every failure a verifier could ever report indistinguishable from every
	// other, which is what left five consecutive remediation attempts reading
	// byte-identical input and editing blindly.
	Verifier string
	// Signature identifies THE FAILURE. It is a digest of normalized failure
	// evidence, not the evidence itself: a candidate's own tests write that
	// evidence, and a digest is the one form of it that cannot carry a
	// sentence. Two different failures differ here; the same failure twice
	// does not.
	Signature string
	// ArtifactRef is the immutable, attempt-scoped transcript this finding was
	// derived from. It is composed from runtime identity, never from a path a
	// provider or candidate supplied.
	ArtifactRef string
	// Diagnostic is a bounded, sanitized excerpt of that transcript, and it is
	// UNTRUSTED DATA. A candidate's tests print into the verifier's own stdout,
	// so this text is quoted to a model inside untrusted markers and is never
	// rendered into the trusted half of an envelope. Holding it in the same
	// struct as the trusted fields is safe only because no formatter prints a
	// Finding whole; see findingSummary and String.
	Diagnostic string `json:"-"`
}

// String renders a finding WITHOUT its diagnostic, and exists so that the
// safety of this type does not depend on every future caller remembering to.
//
// The trusted half of a worker envelope is built with a format verb. A later
// %v or %s on a Finding - in a log line, an error, a debug print, a rendering
// of a slice of them - would otherwise splice attacker-writable verifier bytes
// into whatever it was building. With this method that is structurally
// impossible: the excerpt has exactly one way out, verifierEvidenceEnvelope,
// which quotes it inside untrusted markers. The json tag above closes the
// serialization route for the same reason.
func (f Finding) String() string {
	fields := []string{"class=" + string(f.Classification)}
	if f.Verifier != "" {
		fields = append(fields, "verifier="+f.Verifier)
	}
	if f.Signature != "" {
		fields = append(fields, "signature="+f.Signature)
	}
	if f.ArtifactRef != "" {
		fields = append(fields, "evidence="+f.ArtifactRef)
	}
	return "[" + strings.Join(fields, " ") + "]"
}

// AttemptBound names the bound that ended, or would end, one physical provider
// attempt. The three are different resources and are reported apart (#328):
// inactivity is the provider not moving, the attempt wall is this ATTEMPT
// being long enough, and run active work is the whole RUN's cumulative budget.
type AttemptBound string

type Artifact struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	MediaType   string `json:"media_type"`
	LocalOnly   bool   `json:"local_only"`
	Sanitized   bool   `json:"sanitized"`
	Publishable bool   `json:"publishable"`
}

type Candidate struct {
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

type Ref struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// FeedbackContext is one admitted item as a worker sees it: framed, attributed
// and delimited. It is DATA. The trusted instruction text tells the worker so,
// and nothing in this struct is ever treated as an instruction to the system.
type FeedbackContext struct {
	Key    string
	Class  FeedbackClass
	Actor  string
	Path   string
	Commit string
	Body   string
}

// FeedbackClass is where an item came from. It is recorded because the operator
// question "why did the worker see this" is answered differently for a review
// of the exact head and for a comment on the source issue.
type FeedbackClass string

// PriorAttemptObservations is the runtime's bounded, deterministic account of
// what an eligible retry inherited from earlier attempts of the SAME execution
// binding - and, just as importantly, of what it did not.
//
// It is PROVENANCE, not content. Text is the model-visible material and is
// deliberately not persisted: the observations themselves already exist as the
// immutable per-attempt artifacts #55 established, and a durable row that
// duplicated them would grow without bound. What is persisted answers the
// operator's question - which earlier attempts were supplied, which were
// dropped by the aggregate bound, which were individually truncated, how many
// bytes crossed, and a digest proving the assembly was deterministic.
type PriorAttemptObservations struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	// Supplied, Omitted and Truncated are ascending attempt numbers. Omitted
	// names an attempt that HAD observations and did not fit the aggregate
	// bound; Truncated names one whose own observations were cut to the
	// per-attempt bound. An attempt that observed nothing appears in neither,
	// because nothing about it was dropped.
	Supplied  []int  `json:"supplied_attempts,omitempty"`
	Omitted   []int  `json:"omitted_attempts,omitempty"`
	Truncated []int  `json:"truncated_attempts,omitempty"`
	Bytes     int    `json:"bytes"`
	Digest    string `json:"digest,omitempty"`

	// Text is the assembled model-visible context. It is excluded from the
	// durable record on purpose; see the type comment.
	Text string `json:"-"`
}

// InvocationProvenance is the durable, non-secret record of HOW one attempt was
// invoked. It is what makes a constrained native run and an explicitly
// authorized bypass run distinguishable forever.
//
// The explanatory core - command, modes, bounds, termination and the
// structured-progress counters - is domain.InvocationObservation, embedded so
// the wire shape stays flat and unchanged, and so a run attempt's journal event
// and a planning revision carry ONE definition of it (#327). What is added here
// is who ran, and the #241 refusals only a run attempt has.
type InvocationProvenance struct {
	AgentID      string    `json:"agent_id"`
	ProviderKind string    `json:"provider_kind"`
	TrustMode    TrustMode `json:"trust_mode"`
	Model        string    `json:"model,omitempty"`
	// DeadlineBound is WHICH bound this attempt's execution_deadline is
	// (#328): the physical-attempt wall, or the run's remaining active work
	// when that was smaller. The runtime decided it when the attempt started
	// and records it here, beside the deadline it explains; no adapter sets it.
	// Empty for an attempt of a run that predates the attempt limit.
	DeadlineBound AttemptBound `json:"deadline_bound,omitempty"`
	domain.InvocationObservation
	// GitRefusals are the destructive Git operations the runtime refused during
	// this invocation, bounded and carrying no provider-chosen operand.
	//
	// They are OBSERVATION, not failure. A provider that reached for a
	// destructive recovery, was refused, and then did the work properly
	// succeeded - and the refusal is still the most interesting thing that
	// happened, because it is where expensive reasoning was nearly lost.
	//
	// They are folded into the operation result as a count and the latest
	// shape, which is why the attempt-provenance event carries none of them.
	GitRefusals []GitRefusal `json:"git_refusals,omitempty"`
}

// TrustMode is what the runtime may honestly claim about the boundary around
// one execution worker. It is not a capability and never an authority: an
// operator_trusted agent may author a change and still cannot authorize it.
type TrustMode string

// GitRefusal is the durable record of one refused provider Git operation.
//
// It is written by the broker, inside the provider's process tree, into a
// runtime-owned file the provider is not told the purpose of - the same
// unspoofable-slot pattern the reviewer verdict uses. The adapter reads it
// after the invocation so the refusal becomes attempt evidence rather than a
// line of stderr the provider could have paraphrased.
type GitRefusal struct {
	// Operation is the bounded, redacted argv the provider asked for.
	Operation string `json:"operation"`
	// Reason is the runtime's own words for what it refused.
	Reason string `json:"reason"`
	// Target is the RESOURCE the invocation resolved to. It is recorded because
	// a refusal is not interpretable without it: the same argv is a protected
	// operation against the candidate and an ordinary one against a test's own
	// fixture, and #257 exists because the boundary could not tell them apart.
	Target GitTargetClass `json:"target,omitempty"`
	// Origin is the actor that DIRECTLY originated the invocation, established
	// from execution topology in runtime/git_origin.go. It is recorded on every
	// refusal, including as "unknown", because a record that omits the actor
	// is the record #259 exists to replace: the same refusal means opposite
	// things depending on whether a model, the provider's own machinery or a
	// test binary issued it, and 63 of them once meant the opposite of what
	// was concluded. Nothing in this file reads it.
	Origin GitActorOrigin `json:"origin"`
	// OriginBasis is HOW that was established, so the derivation is auditable
	// rather than asserted.
	OriginBasis string `json:"origin_basis,omitempty"`
	// DirtyPaths is the bounded set of candidate paths that would have been
	// discarded, or empty when the workspace held no delta. It is OBSERVATION
	// for the operator and for the provider's next decision; it is not what
	// the refusal was based on. See BrokerGitCommand.
	DirtyPaths []string `json:"dirty_paths,omitempty"`
	// DirtyCount is the true total, which DirtyPaths may have been bounded
	// below. A record that showed three paths without saying there were forty
	// would understate what was at stake.
	DirtyCount int `json:"dirty_count,omitempty"`
}

// GitActorOrigin is the actor that directly originated a governed Git
// operation. The vocabulary is deliberately generic and provider-neutral: a
// provider adapter may later prove a finer subsystem identity, and the
// evidence schema must not have to learn one vendor's internals to record it.
type GitActorOrigin string

// GitTargetClass is the resource a Git invocation resolves to.
type GitTargetClass string
