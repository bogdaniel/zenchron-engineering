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
	// TrustedMain is trusted main as observed right now.
	TrustedMain RevisionRecord
	Now         time.Time
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
	if err := validateReadoption(request); err != nil {
		return ControllerReadoption{}, err
	}
	previous, hadPrevious, err := store.CurrentControllerAuthority()
	if err != nil {
		return ControllerReadoption{}, err
	}
	if err := refuseUnresolvedTransitions(store); err != nil {
		return ControllerReadoption{}, err
	}
	if err := refuseStrandedRuns(store, request.Binding, hadPrevious, previous); err != nil {
		return ControllerReadoption{}, err
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

	// ALREADY DONE IS DONE. A crash between the authority commit and the
	// projection repair leaves an authority that already names this binding,
	// and re-running must converge on it rather than manufacture a second
	// authority event for the same boundary.
	if hadPrevious && previous.Kind == AuthorityOperatorReadoption {
		same, err := sameBinding(previous.Binding, request.Binding)
		if err != nil {
			return ControllerReadoption{}, err
		}
		if same {
			return existingReadoption(store, previous.Ref)
		}
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
	// AND IT MUST BE TRUSTED MAIN NOW. A governing root is the code the forge
	// currently attests, not the code that was trusted when it was built.
	if request.TrustedMain.Revision != request.Provenance.Source.Revision ||
		request.TrustedMain.Tree != request.Provenance.Source.Tree {
		return refuseReadoption("trusted main is %s and this generation was built from %s; re-adopt the generation trusted main names",
			shortSHA(request.TrustedMain.Revision), shortSHA(request.Provenance.Source.Revision))
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
		if ControllerSuccessionContinues(run, events, governing, nil) {
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

// readoptionID is deterministic in the binding and the instant, so a retry that
// re-reads the same authority addresses the same record.
func readoptionID(binding ControllerBinding, now time.Time) string {
	digest, err := binding.Digest()
	if err != nil || len(digest) < 8 {
		return fmt.Sprintf("readopt-%d", now.UnixNano())
	}
	return fmt.Sprintf("readopt-%s-%d", digest[:8], now.UTC().Unix())
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
