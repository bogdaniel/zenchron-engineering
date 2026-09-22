package runtime

// WHEN ONE ADOPTED CONTROLLER MAY CONTINUE ANOTHER'S RUN.
//
// A run records the controller that created it, and a controller that does not
// match it stops: the run waits on controller_changed and every authority
// request is refused. That default is correct and must stay correct - it is
// what stops a differently-configured or differently-built controller from
// silently reconciling work it may not understand - but until now it had no
// exception at all, so upgrading the controller parked every live run. An
// automated successor (#234) would therefore succeed at its own job and break
// the runs it exists to keep serving.
//
// Succession is that exception, made EXPLICIT AND PROVEN rather than by
// loosening the comparison. A transition is admitted for one run only when a
// decision recorded in that run's own journal says all five of these hold:
//
//	source lineage    both controllers are adopted, and the predecessor's
//	                  source revision is a strict ancestor of the successor's
//	trust root        the successor's revision and tree are the exact
//	                  trusted-main state it was adopted at
//	configuration     the effective configuration digest is unchanged
//	state vocabulary  the successor implements every durable event type the
//	                  run's journal contains
//	durable replay    the successor can reduce and project that journal, with
//	                  no mutation, under its own code
//
// Anything else is refused, and refusal means the predecessor keeps serving.
// There is no migration here, no "probably compatible", and no reinterpretation
// of configuration: #89 owns that larger matrix, and this primitive is
// deliberately the narrow case it will extend rather than replace.
//
// LINEAGE IS NOT UNDERSTANDING, which is why the last two checks exist. A
// descendant build proves where the code came from and says nothing about
// whether it can read what is already on disk; #122's law is that a controller
// never executes from state it cannot completely understand, and the replay is
// how this slice keeps it.
//
// HISTORY IS NOT REWRITTEN. The run row keeps naming the controller that
// created it, every event stays bound to the controller that appended it, and
// succession is an additional event rather than an edit. "Which controller
// performed operation N" is answered by the journal exactly as before; what
// changes is only which controller may append operation N+1.
//
// Stated as the invariant every later slice has to keep: AN ADMISSION
// AUTHORIZES CONTINUITY OF AN EXISTING RUN, AND DOES NOT RETROACTIVELY MAKE THE
// SUCCESSOR THE CONTROLLER OF OPERATIONS THE PREDECESSOR PERFORMED.

import (
	"fmt"
)

// ControllerBinding is the exact controller a run is bound to: the identity of
// the program, its attested build, and the operator configuration it runs
// under. ControllerSHA256 is the digest of this document, and this type is its
// ONE definition - the runtime composes it at construction and a succession
// decision recomputes it, so a stated predecessor can be checked against the
// run rather than believed.
//
// The field names and the omitempty on Build are load-bearing: they are the
// document whose digest is already on disk for every run ever created.
type ControllerBinding struct {
	Controller string           `json:"controller"`
	Build      *ControllerBuild `json:"build,omitempty"`
	Config     ConfigDigest     `json:"config"`
}

// Digest is the ControllerSHA256 a run records.
func (i ControllerBinding) Digest() (string, error) { return Digest(i) }

// adopted reports whether this identity is an adopted build. Succession is
// defined between adopted controllers only: a pre-adoption or unattested build
// has no trust root to check a lineage against, and admitting one would let a
// binary nobody adopted inherit a governed run.
func (i ControllerBinding) adopted() bool {
	return i.Build != nil && i.Build.Kind == ControllerAdopted &&
		len(i.Build.SourceRevision) == 40 && i.Build.SourceTree != ""
}

// SuccessionCheck is one dimension's verdict and the sentence a reader needs.
// The detail is populated whether the check passed or failed, because "how was
// this proven" is as much a part of the record as "was it".
type SuccessionCheck struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

func passed(format string, args ...any) SuccessionCheck {
	return SuccessionCheck{Passed: true, Detail: fmt.Sprintf(format, args...)}
}

func refusedCheck(format string, args ...any) SuccessionCheck {
	return SuccessionCheck{Detail: fmt.Sprintf(format, args...)}
}

// ControllerSuccessionResult is the settled answer. There are two values on
// purpose: a third, softer one is where "probably compatible" would enter, and
// an upgrade that proceeds on a maybe is the failure this whole decision
// exists to prevent.
type ControllerSuccessionResult string

const (
	SuccessionCompatible ControllerSuccessionResult = "compatible"
	SuccessionRefused    ControllerSuccessionResult = "refused"
)

// ControllerSuccessionDecision is the durable evidence that one controller may
// continue one run. It names both parties exactly, records each dimension
// separately, and is the payload of the journalled admission.
type ControllerSuccessionDecision struct {
	RunID string `json:"run_id"`
	// HandoffID is the transition this decision was made for, and it is what
	// stops an admission from outliving its occasion.
	//
	// Without it an admission is a standing capability: a transition that
	// wrote admissions and then FAILED left every one of those runs saying the
	// successor may append, forever, under any later circumstance. That was
	// inert only because work admission is computed elsewhere, which makes the
	// safety of the journal depend on a rule the journal does not state - and
	// a latent capability that is safe by coincidence is not safe.
	//
	// Bound this way, the evidence means "the successor may continue this run
	// IF transition H activates", and a transition that never activates leaves
	// evidence that never means anything.
	HandoffID   string            `json:"handoff_id"`
	Predecessor ControllerBinding `json:"predecessor"`
	Successor   ControllerBinding `json:"successor"`

	SourceLineage   SuccessionCheck `json:"source_lineage"`
	TrustRoot       SuccessionCheck `json:"trust_root"`
	Configuration   SuccessionCheck `json:"configuration"`
	StateVocabulary SuccessionCheck `json:"state_vocabulary"`
	DurableReplay   SuccessionCheck `json:"durable_replay"`

	Result ControllerSuccessionResult `json:"result"`
}

func (d ControllerSuccessionDecision) checks() []SuccessionCheck {
	return []SuccessionCheck{d.SourceLineage, d.TrustRoot, d.Configuration, d.StateVocabulary, d.DurableReplay}
}

// settle computes the result from the dimensions. It is a fold rather than an
// assignment at each site so a dimension added later cannot be forgotten in
// one branch and silently stop counting.
func (d *ControllerSuccessionDecision) settle() {
	for _, check := range d.checks() {
		if !check.Passed {
			d.Result = SuccessionRefused
			return
		}
	}
	d.Result = SuccessionCompatible
}

// Refusals names the dimensions that did not pass, for an operator reading a
// blocked upgrade.
func (d ControllerSuccessionDecision) Refusals() []string {
	named := []struct {
		name  string
		check SuccessionCheck
	}{
		{"source_lineage", d.SourceLineage}, {"trust_root", d.TrustRoot},
		{"configuration", d.Configuration}, {"state_vocabulary", d.StateVocabulary},
		{"durable_replay", d.DurableReplay},
	}
	var refusals []string
	for _, entry := range named {
		if !entry.check.Passed {
			refusals = append(refusals, entry.name+": "+entry.check.Detail)
		}
	}
	return refusals
}

// ControllerSuccessionInput is everything the decision reads. It takes the
// journal rather than a store handle because the evaluation must be READ-ONLY
// by construction: a compatibility check that could write is a check that can
// damage the state it was deciding about.
type ControllerSuccessionInput struct {
	Run         EngineeringRun
	Events      []EngineeringEvent
	Predecessor ControllerBinding
	Successor   ControllerBinding
	// TrustedMain is the trusted-main state the successor was adopted at,
	// taken from its own adopted-build provenance.
	TrustedMain RevisionRecord
	// IsAncestor answers the lineage question with Git, the same
	// `merge-base --is-ancestor` the adopted build proves containment with. It
	// is a seam so the refusals below are reachable without a repository.
	IsAncestor func(ancestor, descendant string) (bool, error)
}

// EvaluateControllerSuccession decides whether Successor may continue this run.
//
// It is the successor that must run this: the vocabulary and replay checks are
// only meaningful when performed by the code that would be doing the reading.
// A predecessor asking "can B read this?" on B's behalf would be answering from
// its own decoders, which is the question nobody needs answered.
func EvaluateControllerSuccession(in ControllerSuccessionInput) ControllerSuccessionDecision {
	decision := ControllerSuccessionDecision{
		RunID: in.Run.ID, Predecessor: in.Predecessor, Successor: in.Successor,
	}
	decision.SourceLineage = evaluateLineage(in)
	decision.TrustRoot = evaluateTrustRoot(in)
	decision.Configuration = evaluateConfiguration(in)
	decision.StateVocabulary = evaluateVocabulary(in)
	decision.DurableReplay = evaluateReplay(in)
	decision.settle()
	return decision
}

// evaluateLineage proves the predecessor is who the run says it is, that both
// parties are adopted, and that the successor descends strictly from the
// predecessor.
//
// THE IDENTITY BINDING IS PART OF LINEAGE, and it is the part that makes the
// rest non-forgeable: a decision may state any predecessor it likes, and only
// one of them recomputes to the digest the run actually carries.
func evaluateLineage(in ControllerSuccessionInput) SuccessionCheck {
	stated, err := in.Predecessor.Digest()
	if err != nil {
		return refusedCheck("the stated predecessor identity could not be digested: %v", err)
	}
	if stated != in.Run.ControllerSHA256 {
		return refusedCheck("the stated predecessor is not the controller run %s records", in.Run.ID)
	}
	if !in.Predecessor.adopted() {
		return refusedCheck("the predecessor is not an adopted build, so it has no lineage to succeed from")
	}
	if !in.Successor.adopted() {
		return refusedCheck("the successor is not an adopted build")
	}
	from, to := in.Predecessor.Build.SourceRevision, in.Successor.Build.SourceRevision
	if from == to {
		return refusedCheck("the successor is built from the same source revision %s, so it succeeds nothing", shortSHA(to))
	}
	if in.IsAncestor == nil {
		return refusedCheck("no lineage prover was supplied, and lineage is never assumed")
	}
	ancestor, err := in.IsAncestor(from, to)
	if err != nil {
		return refusedCheck("the lineage of %s to %s could not be established: %v", shortSHA(from), shortSHA(to), err)
	}
	if !ancestor {
		return refusedCheck("%s is not an ancestor of %s", shortSHA(from), shortSHA(to))
	}
	return passed("%s is a strict adopted ancestor of %s", shortSHA(from), shortSHA(to))
}

// evaluateTrustRoot requires the successor to BE trusted main rather than
// merely to descend from the predecessor. A descendant of an adopted commit is
// not automatically trusted: it is whatever was built, and only the exact
// trusted-main state carries the adoption governance this transition inherits.
func evaluateTrustRoot(in ControllerSuccessionInput) SuccessionCheck {
	if !in.Successor.adopted() {
		return refusedCheck("the successor is not an adopted build")
	}
	if len(in.TrustedMain.Revision) != 40 || in.TrustedMain.Tree == "" {
		return refusedCheck("no trusted-main state was observed for the successor")
	}
	if in.Successor.Build.SourceRevision != in.TrustedMain.Revision {
		return refusedCheck("the successor is built from %s, which is not trusted main %s",
			shortSHA(in.Successor.Build.SourceRevision), shortSHA(in.TrustedMain.Revision))
	}
	if in.Successor.Build.SourceTree != in.TrustedMain.Tree {
		return refusedCheck("the successor's tree %s is not the tree of trusted main %s",
			shortSHA(in.Successor.Build.SourceTree), shortSHA(in.TrustedMain.Tree))
	}
	return passed("the successor is the exact trusted-main state %s", shortSHA(in.TrustedMain.Revision))
}

// evaluateConfiguration requires the effective configuration to be IDENTICAL.
// Deciding that a changed configuration is still compatible is exactly the
// judgement #89 exists to make; this slice does not make it, and an unchanged
// digest is the one case that needs nobody's judgement.
func evaluateConfiguration(in ControllerSuccessionInput) SuccessionCheck {
	if in.Predecessor.Config != in.Successor.Config {
		return refusedCheck("the effective configuration changed between controllers")
	}
	if in.Predecessor.Controller != in.Successor.Controller {
		return refusedCheck("the controller identity changed from %q to %q",
			in.Predecessor.Controller, in.Successor.Controller)
	}
	return passed("the effective configuration is unchanged")
}

// evaluateVocabulary requires the successor to implement every durable event
// type this run already holds. It is checked separately from the replay
// because the two fail for different reasons and a reader needs to know which:
// an unimplemented type is a controller too old for the state, and a replay
// failure is state the controller cannot reconstruct.
func evaluateVocabulary(in ControllerSuccessionInput) SuccessionCheck {
	for _, event := range in.Events {
		if !eventTypes[event.Type] {
			return refusedCheck("event type %q is not in this controller's vocabulary", event.Type)
		}
		if _, implemented := eventPayloads[event.Type]; !implemented {
			return refusedCheck("event type %q has no implemented payload schema in this controller", event.Type)
		}
	}
	return passed("all %d durable event(s) are within this controller's vocabulary", len(in.Events))
}

// evaluateReplay requires the successor to reconstruct the run from its journal
// under its own code. Reduce already fails closed on an unknown type, a broken
// chain and an invalid hash, so this is the whole of "can B read what A wrote"
// - performed against values, writing nothing.
func evaluateReplay(in ControllerSuccessionInput) SuccessionCheck {
	snapshot, err := Reduce(in.Run, in.Events)
	if err != nil {
		return refusedCheck("the journal does not replay under this controller: %v", err)
	}
	if _, err := Project(in.Events); err != nil {
		return refusedCheck("the journal does not project under this controller: %v", err)
	}
	return passed("the journal replays to state %s under this controller", shortSHA(snapshot.StateSHA256))
}

// ControllerSuccessionContinues reports whether controller may drive this run:
// either it created it, or the run's journal admits an unbroken chain of
// successions from its creator to it.
//
// THE CHAIN IS FOLLOWED, not searched. Each admission moves the run's effective
// controller exactly one step, and an admission whose predecessor is not where
// the run currently stands is ignored rather than applied out of order - so a
// stale decision recorded against an earlier generation cannot reattach a
// controller the run has already succeeded past.
// activatedHandoffs answers whether one transition reached activation. It is a
// predicate rather than a store so the chain rule stays a pure function over
// (run, journal, authority), and so a caller that has already read the handoff
// table once does not read it again per run.
type activatedHandoffs func(handoffID string) bool

func ControllerSuccessionContinues(run EngineeringRun, events []EngineeringEvent, controller string, activated activatedHandoffs) bool {
	current := run.ControllerSHA256
	if current == controller {
		return true
	}
	if activated == nil {
		// NO AUTHORITY ORACLE, NO SUCCESSION. A caller that cannot say which
		// transitions activated cannot be told that one did.
		return false
	}
	for _, event := range events {
		if event.Type != EventControllerSuccessionAdmitted {
			continue
		}
		var decision ControllerSuccessionDecision
		if len(event.Payload) == 0 || decodeJSON(event.Payload, &decision) != nil {
			// An admission that cannot be read admits nothing. Failing closed
			// here costs an upgrade and keeps a controller from inheriting a
			// run on the strength of a record it could not parse.
			continue
		}
		if decision.Result != SuccessionCompatible {
			continue
		}
		// THE OCCASION MUST HAVE HAPPENED. An admission whose transition failed,
		// or has not activated yet, grants nothing: it was evidence for a
		// succession that the control model never adopted.
		if decision.HandoffID == "" || !activated(decision.HandoffID) {
			continue
		}
		from, fromErr := decision.Predecessor.Digest()
		to, toErr := decision.Successor.Digest()
		if fromErr != nil || toErr != nil || from != current {
			continue
		}
		current = to
		if current == controller {
			return true
		}
	}
	return false
}

// alreadyAdmitted reports whether this exact evidence - this transition, this
// successor - is already in the journal.
func alreadyAdmitted(events []EngineeringEvent, handoffID, successor string) bool {
	for _, event := range events {
		if event.Type != EventControllerSuccessionAdmitted || len(event.Payload) == 0 {
			continue
		}
		var recorded ControllerSuccessionDecision
		if decodeJSON(event.Payload, &recorded) != nil || recorded.HandoffID != handoffID {
			continue
		}
		if digest, err := recorded.Successor.Digest(); err == nil && digest == successor {
			return true
		}
	}
	return false
}

// activatedHandoffs reads which transitions this state directory records as
// activated. It is the runtime's authority oracle for the chain rule, and it
// reads the handoff table rather than anything live.
func (r *EngineeringRuntime) activatedHandoffs() activatedHandoffs {
	records, err := r.deps.Store.ControllerHandoffs()
	if err != nil {
		// FAIL CLOSED. "I could not read which transitions activated" is not
		// "none did" for authority purposes, but it is the only safe answer
		// here: an unreadable table must not promote a successor.
		return func(string) bool { return false }
	}
	activated := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Phase == HandoffActivated {
			activated[record.ID] = true
		}
	}
	return func(id string) bool { return activated[id] }
}

// AdmitControllerSuccession records that this run may be continued by the
// decision's successor.
//
// WHEN IT MAY BE CALLED: as part of the ownership transition, by the successor,
// after it has acquired the scheduler and revalidated the run. Never during a
// preflight. A preflight that admitted would leave durable state saying the
// successor was admitted for a handoff that then failed, and the predecessor -
// still the active controller - would be reading a journal that says otherwise.
// Preflight produces decisions; only the transition records them.
//
// It is idempotent: a run that already continues under the successor is left
// exactly as it is rather than accumulating a second admission, because a
// repeated upgrade request is an ordinary condition and duplicate evidence of
// one transition would make the chain ambiguous.
func (r *EngineeringRuntime) AdmitControllerSuccession(runID string, decision ControllerSuccessionDecision) error {
	if decision.Result != SuccessionCompatible {
		return fmt.Errorf("a refused succession is not admitted: %v", decision.Refusals())
	}
	if decision.RunID != runID {
		return fmt.Errorf("the decision names run %q and was offered for run %q", decision.RunID, runID)
	}
	if decision.HandoffID == "" {
		return fmt.Errorf("a succession admission names the transition it was decided for")
	}
	run, ok, err := r.deps.Store.Run(runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown run %q", runID)
	}
	events, err := r.deps.Store.Events(runID)
	if err != nil {
		return err
	}
	successor, err := decision.Successor.Digest()
	if err != nil {
		return err
	}
	// IDEMPOTENCE IS ABOUT THE RECORD, NOT ABOUT AUTHORITY. Asking "does the
	// successor already continue this run" would answer no for an admission
	// whose transition has not activated yet - which is every admission at the
	// moment it is written - and a retry would then try to append the same
	// evidence twice.
	if alreadyAdmitted(events, decision.HandoffID, successor) {
		return nil
	}
	activated := r.activatedHandoffs()
	predecessor, err := decision.Predecessor.Digest()
	if err != nil {
		return err
	}
	// THE TRANSITION MUST START WHERE THE RUN STANDS. Admitting one whose
	// predecessor the run has already succeeded past would record a branch in
	// a line that has to stay a line.
	if !ControllerSuccessionContinues(run, events, predecessor, activated) {
		return fmt.Errorf("run %s is not currently continued by the decision's predecessor", runID)
	}
	payload, err := marshalPayloadJSON(decision)
	if err != nil {
		return err
	}
	_, err = r.deps.Store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		// Deterministic in the transition, so the same succession cannot be
		// journalled twice even if two callers race past the check above.
		ID:         fmt.Sprintf("%s-controller-succession-%s-%s", runID, shortSHA(successor), decision.HandoffID),
		RunID:      runID,
		Type:       EventControllerSuccessionAdmitted,
		OccurredAt: r.deps.Clock.Now(),
		Payload:    payload,
	})
	return err
}
