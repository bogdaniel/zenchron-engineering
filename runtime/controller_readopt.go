package runtime

// THE COLD RE-ADOPTION, and everything it refuses.
//
// This is the recovery boundary for a controller-effective configuration
// change, and it is the only operation in this system that establishes present
// authority without a transition. Everything about it is therefore narrowed
// until it can only do the one thing it exists for.
//
// COLD. It takes the controller role itself and refuses while anything else
// holds it. An authority root that could move under a running controller is
// the hot re-anchor this protocol spent itself refusing: the live process would
// go on serving under an authority it never observed.
//
// THIS EXECUTABLE. It re-measures itself, requires its own adopted provenance,
// and requires that provenance to be exact trusted main right now. An operator
// naming an arbitrary generation would be choosing which code governs from a
// command line, which is the decision adoption exists to take away from
// anybody's typing.
//
// THE SAME CONTROLLER. The program identity does not change. This sanctions a
// changed effective configuration or establishes a new adopted root; it is not
// an identity override, and a binding whose Controller differs is refused.
//
// NOTHING IN FLIGHT, AND NOTHING STRANDED. An unresolved transition is somebody
// else's half-finished business and is resolved first. A nonterminal run whose
// binding this generation cannot continue blocks the whole operation: carrying
// old-configuration work across a boundary the operator created is exactly the
// silent continuation the succession law forbids, and doing it here because the
// operator asked would make the law a suggestion.
//
// BOUND TO WHAT IT SAW. The commit is conditional on the authority observed
// during preflight, so an activation completing in between loses rather than
// being overwritten.
//
// AND IT DOES NOT LEAK. After it, the next ordinary transition succeeds from
// the re-adopted binding exactly as it would from an activated one. There is no
// re-adoption path in normal upgrades and no flag that remembers this happened.

import (
	"fmt"
	"os"
	"time"
)

// ReadoptionRequest is what an operator supplies.
type ReadoptionRequest struct {
	// Reason is required. An authority event nobody explained is one nobody
	// can review.
	Reason string
	// Operator is the resolved operator provenance, recorded beside what they
	// sanctioned.
	Operator RecordedOperator
	// Self is this executable, measured.
	Self ControllerSelfRecord
	// Provenance is this generation's own adopted build record, read from the
	// published version directory.
	Provenance AdoptedBuildProvenance
	// Binding is what this controller binds as under the configuration now in
	// effect: identity, build and effective configuration digest.
	Binding ControllerBinding
	// ObserveTrustedMain is asked for trusted main ONCE, immediately before
	// the authority commit. It is a port rather than a snapshot because the
	// preflight below replays every live run's journal, which takes as long as
	// it takes: a revision observed before that is a fact about a branch as it
	// was, used to authorize a governing root established now.
	//
	// The recovery path never calls it. See ReadoptController.
	ObserveTrustedMain func() (RevisionRecord, error)
	Now                time.Time
}

// readoptionStore is everything this operation reads and writes.
type readoptionStore interface {
	runReader
	ControllerHandoffs() ([]ControllerHandoff, error)
	CurrentControllerAuthority() (ControllerAuthority, bool, error)
	ControllerReadoptions() ([]ControllerReadoption, error)
	ReadoptController(readoption ControllerReadoption, expected *ControllerAuthority) (bool, error)
}

// ReadoptionRefusedError is a re-adoption that was not performed, and why.
type ReadoptionRefusedError struct{ Detail string }

func (e *ReadoptionRefusedError) Error() string {
	return "the controller was not re-adopted: " + e.Detail
}

func refuseReadoption(format string, args ...any) error {
	return &ReadoptionRefusedError{Detail: fmt.Sprintf(format, args...)}
}

// ReadoptController performs the whole operation: preflight, commit, and the
// record of what was sanctioned.
//
// The caller supplies the role it already took, because taking it is how this
// operation proves it is cold and the composition root is where a role is
// acquired. It is exercised rather than trusted: every durable write happens
// inside the authority section.
func ReadoptController(store readoptionStore, lease *ControllerRoleLease, request ReadoptionRequest) (ControllerReadoption, error) {
	// WHAT THIS EXECUTABLE IS. Proven first because every path needs it, and
	// because it asks nothing of anybody: the running bytes, the published
	// record beside them, and the file they are.
	if err := validateReadoption(request); err != nil {
		return ControllerReadoption{}, err
	}
	previous, hadPrevious, err := store.CurrentControllerAuthority()
	if err != nil {
		return ControllerReadoption{}, err
	}

	// RECOVERY BEFORE DECISION, and it asks the forge nothing.
	//
	// If a re-adoption of this exact binding already governs, there is no
	// adoption decision left to make: it was made and committed, and what
	// remains is a local projection. Requiring trusted main to be unchanged
	// here - or reachable at all - would make the crash guarantee conditional
	// on a branch that moves and a forge that can be down, which is precisely
	// the window this recovery exists for.
	if hadPrevious && previous.Kind == AuthorityOperatorReadoption {
		same, err := sameBinding(previous.Binding, request.Binding)
		if err != nil {
			return ControllerReadoption{}, err
		}
		if same {
			return existingReadoption(store, previous.Ref)
		}
	}

	// A NEW BOUNDARY FROM HERE. Everything below decides whether one may be
	// established, and nothing below is reached by a retry.
	if err := refuseUnresolvedTransitions(store); err != nil {
		return ControllerReadoption{}, err
	}
	if err := refuseStrandedRuns(store, request.Binding, hadPrevious, previous); err != nil {
		return ControllerReadoption{}, err
	}

	// CURRENCY IS PROVEN LAST, immediately before the commit. Scanning and
	// replaying every live run takes real time, and a governing root is the
	// code the forge attests NOW rather than when this command started.
	if request.ObserveTrustedMain == nil {
		return ControllerReadoption{}, refuseReadoption("no trusted-main observation was supplied, and currency is never assumed")
	}
	trusted, err := request.ObserveTrustedMain()
	if err != nil {
		return ControllerReadoption{}, refuseReadoption("trusted main could not be observed, so this generation cannot be shown to be it: %v", err)
	}
	if trusted.Revision != request.Provenance.Source.Revision || trusted.Tree != request.Provenance.Source.Tree {
		return ControllerReadoption{}, refuseReadoption(
			"trusted main is %s and this generation was built from %s; re-adopt the generation trusted main names",
			shortSHA(trusted.Revision), shortSHA(request.Provenance.Source.Revision))
	}

	readoption := ControllerReadoption{
		ID:         readoptionID(request.Binding, request.Now),
		Binding:    request.Binding,
		Self:       request.Self,
		Provenance: request.Provenance,
		Config:     request.Binding.Config,
		Reason:     request.Reason,
		Operator:   request.Operator,
		RecordedAt: request.Now,
	}
	var expected *ControllerAuthority
	if hadPrevious {
		expected = &previous
		readoption.Previous = &previous
		readoption.PreviousConfig = previous.Binding.Config
	}

	var committed bool
	if err := lease.WithAuthority(func() error {
		var commitErr error
		committed, commitErr = store.ReadoptController(readoption, expected)
		return commitErr
	}); err != nil {
		return ControllerReadoption{}, err
	}
	if !committed {
		return ControllerReadoption{}, refuseReadoption(
			"the governing controller authority changed while this re-adoption was being prepared; nothing was written")
	}
	return readoption, nil
}

// validateReadoption is everything about the REQUEST that can be judged before
// the state directory is read.
func validateReadoption(request ReadoptionRequest) error {
	switch {
	case request.Reason == "":
		return refuseReadoption("an operator reason is required")
	case request.Operator.ID == "":
		return refuseReadoption("the operator identity could not be resolved, and an authority event records who performed it")
	case request.Self.Unattested:
		return refuseReadoption("this controller is an unattested build, and only an adopted generation can be a governing root")
	case request.Binding.Build == nil:
		return refuseReadoption("this controller has no attested build to re-adopt")
	}
	// THE RUNNING BINARY IS THE ONE BEING SANCTIONED. Everything below compares
	// what this process measured against what the published record claims, so a
	// directory that was edited after publication cannot be re-adopted on the
	// strength of its own provenance file.
	if err := request.Self.ProvesGeneration(request.Binding); err != nil {
		return refuseReadoption("this process is not the generation it would re-adopt: %v", err)
	}
	if request.Provenance.Kind != ControllerAdopted {
		return refuseReadoption("the published provenance for this generation is %q, not an adopted build", request.Provenance.Kind)
	}
	if request.Provenance.BinarySHA256 != request.Self.Measured {
		return refuseReadoption("this executable measures %s and its published provenance records %s",
			shortSHA(request.Self.Measured), shortSHA(request.Provenance.BinarySHA256))
	}
	// AND IT MUST BE THE PUBLISHED ARTIFACT ITSELF, not a binary that merely
	// measures the same.
	//
	// Every check above is satisfied by a copy: same bytes, same build, same
	// provenance file read from the immutable directory. A copy in /tmp would
	// therefore have re-adopted itself and pointed the stable entrypoint
	// outside the adopted-controller root, at a file nothing governs and
	// anything can replace. The identity that matters is the FILE, so it is
	// compared as one - which lets current/zenchron-engineering resolve to the
	// canonical binary and refuses a duplicate of it.
	running, err := os.Stat(request.Self.ExecutablePath)
	if err != nil {
		return refuseReadoption("the running executable %s could not be examined: %v", request.Self.ExecutablePath, err)
	}
	published, err := os.Stat(request.Provenance.OutputPath)
	if err != nil {
		return refuseReadoption("the published artifact %s could not be examined: %v", request.Provenance.OutputPath, err)
	}
	if !os.SameFile(running, published) {
		return refuseReadoption(
			"this process is running %s, which is not the published artifact %s; re-adopt the immutable generation itself",
			request.Self.ExecutablePath, request.Provenance.OutputPath)
	}
	return nil
}

// refuseUnresolvedTransitions keeps a re-adoption from stepping over somebody
// else's half-finished business.
func refuseUnresolvedTransitions(store readoptionStore) error {
	records, err := store.ControllerHandoffs()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.InFlight() {
			return refuseReadoption(
				"transition %s is in flight at %q; resolve or settle it before re-adopting", record.ID, record.Phase)
		}
	}
	return nil
}

// refuseStrandedRuns blocks the operation when live work would be left behind.
//
// The rule is the succession law, applied at the one boundary succession cannot
// cross: a run this generation cannot prove it may continue is not carried, and
// because a re-adoption carries EVERYTHING or nothing, one such run refuses the
// whole operation. An operator settles those runs deliberately, exactly as they
// would for a blocked upgrade.
func refuseStrandedRuns(store readoptionStore, binding ControllerBinding, hadPrevious bool, previous ControllerAuthority) error {
	runs, err := store.Runs()
	if err != nil {
		return err
	}
	governing, err := binding.Digest()
	if err != nil {
		return err
	}
	// THE ACTIVATION ORACLE, OR EVERY INHERITED RUN LOOKS STRANDED.
	//
	// A run reaches its current controller through succession admissions in
	// its own journal, and each of those means anything only if the transition
	// it was admitted for activated. Passing nil says "no authority oracle, no
	// succession" - the safe answer where none can be supplied, and the wrong
	// one here, because it makes every legitimately inherited run incompatible
	// and refuses a re-adoption that should proceed. It is #282's lesson in a
	// new place.
	activated, err := activatedTransitions(store)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			continue
		}
		events, err := store.Events(run.ID)
		if err != nil {
			return err
		}
		// The same question every other caller asks: may THIS controller
		// append to THIS run. A run created under the new binding answers yes
		// and is untouched by the boundary.
		if ControllerSuccessionContinues(run, events, governing, activated) {
			continue
		}
		detail := "no controller authority governs yet"
		if hadPrevious {
			detail = fmt.Sprintf("%s %s governs", previous.Kind, previous.Ref)
		}
		return refuseReadoption(
			"run %s is not terminal and this generation cannot continue it (%s); settle it or restore the previous configuration",
			run.ID, detail)
	}
	return nil
}

// existingReadoption returns the record a converging retry is recognising.
func existingReadoption(store readoptionStore, id string) (ControllerReadoption, error) {
	readoptions, err := store.ControllerReadoptions()
	if err != nil {
		return ControllerReadoption{}, err
	}
	for _, readoption := range readoptions {
		if readoption.ID == id {
			return readoption, nil
		}
	}
	return ControllerReadoption{}, refuseReadoption(
		"authority names re-adoption %s and no such record exists", id)
}

// readoptionID names one re-adoption of one binding at one instant.
//
// It carries the WHOLE binding digest. Eight hex characters are enough to read
// and not enough to be an identity: two different bindings sharing a prefix
// would address one record, and the store would then be asked to reconcile two
// authority events that are not the same event. The full digest costs nothing
// and removes the question.
func readoptionID(binding ControllerBinding, now time.Time) string {
	digest, err := binding.Digest()
	if err != nil {
		return fmt.Sprintf("readopt-unbindable-%d", now.UTC().UnixNano())
	}
	return fmt.Sprintf("readopt-%s-%d", digest, now.UTC().UnixNano())
}

// sameBinding compares two bindings by the digest every other caller compares.
func sameBinding(left, right ControllerBinding) (bool, error) {
	leftDigest, err := left.Digest()
	if err != nil {
		return false, err
	}
	rightDigest, err := right.Digest()
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

// activatedTransitions answers which transitions activated, from the durable
// records.
//
// It is the same question storeTransitionActivated answers for a concrete
// SQLite store, asked through the narrow interface this operation reads
// everything else through - and it is history rather than present authority:
// an admission is evidence for a succession the control model adopted, and a
// transition that activated and was later superseded still adopted it.
func activatedTransitions(store readoptionStore) (transitionActivated, error) {
	records, err := store.ControllerHandoffs()
	if err != nil {
		return nil, err
	}
	activated := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Phase == HandoffActivated {
			activated[record.ID] = true
		}
	}
	return func(handoffID string) bool { return activated[handoffID] }, nil
}
