package runtime

// held_material.go is #203: completed material work must not be silently made
// useless by a later budget boundary.
//
// A run that exhausts a budget is still ended - budget authority is finite and
// is never reset or extended here. What changes is that the terminal record
// says whether valuable material exists and names it exactly, instead of a
// bare `run_wall_budget_exhausted` that reads the same whether the run produced
// nothing or held a verified commit one push away from a pull request.
//
// The disposition is HELD, and holding grants nothing: no execution attempt,
// continuation unit, active work, provider deadline, token or cost authority,
// Git operation or publication. The material stays exactly where the lifecycle
// left it - a runtime-owned commit in the run's candidate workspace, or
// uncommitted changes in that workspace - and GC retains that workspace for as
// long as the record exists. Any further use of it goes through a new,
// separately governed run; nothing here queues one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The kinds of held material, from furthest along the lifecycle to least.
const (
	// HeldVerifiedUnpublished is a committed, execution-complete candidate
	// whose current-head assurance passed, and which no pull request carries.
	HeldVerifiedUnpublished = "verified_unpublished"
	// HeldCommittedUnverified is an execution-complete runtime commit that
	// has not (yet) passed assurance at its exact head.
	HeldCommittedUnverified = "committed_unverified"
	// HeldCheckpoint is a runtime-owned incomplete checkpoint commit (#54).
	HeldCheckpoint = "checkpoint"
	// HeldUncommitted is a producing operation's workspace change that the
	// runtime never committed: either a succeeded producer's change that the
	// budget ended the run before candidate.commit could reach, or a
	// producer's change a failure classification refused outright (#390). It
	// exists only in the candidate workspace.
	HeldUncommitted = "uncommitted"
)

// HeldDisposition is the one disposition #203 records: preserved in place,
// reported, retained from GC, and granted nothing.
const HeldDisposition = "held"

// HeldMaterial is the bounded, non-secret identity of what a budget-ended run
// is holding. It is computed ONCE, from replayed state, when the terminal
// disposition is journalled, and afterwards only ever read back - so a restart
// or a changed configuration reproduces the same record rather than a
// re-derivation of it.
type HeldMaterial struct {
	Kind string `json:"kind"`
	// Revision is the commit the material is: the candidate commit for a
	// committed kind, and for uncommitted material the commit the workspace
	// changes sit on. Tree is that commit's tree when the journal recorded
	// one. Either empty means UNKNOWN; neither is ever guessed.
	Revision string `json:"revision,omitempty"`
	Tree     string `json:"tree,omitempty"`
	// Operation, PathCount and ContentDigest identify uncommitted material:
	// the producing operation, how many paths it changed, and a digest over
	// those paths and their contents taken when the producer returned.
	// ContentDigest absent means UNKNOWN, never "no content".
	Operation     string `json:"operation,omitempty"`
	PathCount     int    `json:"path_count,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
	// NextStep is the lifecycle operation the planner selects next, and
	// BlockedBy is the terminal reason that step is not admissible.
	NextStep  string `json:"next_step,omitempty"`
	BlockedBy string `json:"blocked_by"`
	// Successor and SuccessorUnavailable are the latest execution attempt's
	// selected successor and why it cannot be admitted (#328), when the
	// attempt recorded one.
	Successor            string `json:"successor,omitempty"`
	SuccessorUnavailable string `json:"successor_unavailable,omitempty"`
	Disposition          string `json:"disposition"`
}

var heldKinds = map[string]bool{HeldVerifiedUnpublished: true, HeldCommittedUnverified: true, HeldCheckpoint: true, HeldUncommitted: true}

func (h HeldMaterial) validate() error {
	var closed error
	if !heldKinds[h.Kind] {
		closed = fmt.Errorf("held_material.kind %q is not a held-material kind", h.Kind)
	}
	if h.Disposition != HeldDisposition {
		closed = errors.Join(closed, fmt.Errorf("held_material.disposition must be %q", HeldDisposition))
	}
	return errors.Join(closed,
		required("held_material.kind", h.Kind),
		bounded("held_material.revision", h.Revision),
		bounded("held_material.tree", h.Tree),
		bounded("held_material.operation", h.Operation),
		nonNegative("held_material.path_count", h.PathCount),
		bounded("held_material.content_digest", h.ContentDigest),
		bounded("held_material.next_step", h.NextStep),
		required("held_material.blocked_by", h.BlockedBy),
		bounded("held_material.successor", h.Successor),
		bounded("held_material.successor_unavailable", h.SuccessorUnavailable),
		required("held_material.disposition", h.Disposition))
}

// The run-level budget reasons conditions() settles a run on.
const (
	ReasonRunWallBudgetExhausted       = "run_wall_budget_exhausted"
	ReasonLifecycleDeadlineExhausted   = "run_lifecycle_deadline_exhausted"
	ReasonContinuationsExhausted       = "execution_continuations_exhausted"
	ReasonProviderInvocationsExhausted = "run_provider_invocations_exhausted"
	attemptsExhaustedSuffix            = "_attempts_exhausted"
)

var budgetReasons = map[string]bool{
	ReasonRunWallBudgetExhausted: true, ReasonLifecycleDeadlineExhausted: true,
	ReasonContinuationsExhausted: true, ReasonProviderInvocationsExhausted: true,
}

// BudgetBoundary reports whether a terminal failure is a budget boundary: one
// of the closed set of run-level budget reasons, or an operation's attempt
// budget (`<kind>_attempts_exhausted`).
func BudgetBoundary(disposition Disposition, reason string) bool {
	return disposition == Failed && (budgetReasons[reason] || strings.HasSuffix(reason, attemptsExhaustedSuffix))
}

// heldMaterial names the valuable material the run holds when a budget ends
// it, or nil when it holds none. It is pure: replayed state only.
func (s *runState) heldMaterial(reason string) *HeldMaterial {
	held := HeldMaterial{BlockedBy: reason, Disposition: HeldDisposition, NextStep: s.nextLifecycleStep()}
	if d := s.projection.ExecutionDiagnostic; d != nil && d.SuccessorUnavailable != "" {
		held.Successor, held.SuccessorUnavailable = d.Successor, d.SuccessorUnavailable
	}
	head := s.projection.CandidateRevision
	if producing, pending := bindCandidateCommit(s); pending {
		return held.uncommitted(s, producing, head).bounded()
	}
	// A failure classification refused a producing operation's change before
	// candidate.commit ever ran for it (#390): the SAME disposition as a
	// succeeded producer's not-yet-committed change above, because mutation
	// proves material exists whether or not the invocation that produced it
	// was admitted. The refusal, not a budget, is what stopped this specific
	// change from reaching a commit, but the material is identified and held
	// exactly the same way, rather than silently going away with the run.
	if producing, refused := s.refusedMutation(); refused {
		return held.uncommitted(s, producing.ID, head).bounded()
	}
	if head == "" {
		return nil
	}
	if pr := s.projection.PullRequest; pr != nil && pr.HeadRevision == head {
		return nil
	}
	held.Revision, held.Tree = head, s.projection.CandidateTree
	switch {
	case !s.projection.CandidateComplete:
		held.Kind = HeldCheckpoint
	case s.verifiedAt(head):
		held.Kind = HeldVerifiedUnpublished
	default:
		held.Kind = HeldCommittedUnverified
	}
	return held.bounded()
}

// uncommitted fills in the HeldUncommitted shape shared by a succeeded
// producer whose change a budget boundary caught before candidate.commit ran,
// and a producer a failure classification refused outright (#390): the
// identity of the material - its producing operation, path count and content
// digest - and the commit it sits on top of, falling back to the trusted base
// when no candidate commit exists yet.
func (h HeldMaterial) uncommitted(s *runState, operation, head string) HeldMaterial {
	var record mutationResult
	// A result that does not decode leaves the identity unknown, never
	// invented: the kind and the producing operation still stand.
	_ = json.Unmarshal(s.snapshot.Operations[operation].Result, &record)
	h.Kind, h.Operation = HeldUncommitted, operation
	h.PathCount, h.ContentDigest = record.PathCount, record.ContentDigest
	h.Revision, h.Tree = head, s.projection.CandidateTree
	if head == "" {
		h.Revision, h.Tree = s.baseRevision(), ""
	}
	return h
}

// refusedMutation reports the most recent FAILED producing operation whose
// workspace change a failure classification refused to admit (#390): mutation
// proves material exists, not that the invocation which produced it
// succeeded, so material a refusal leaves behind is named and held exactly as
// a succeeded producer's own not-yet-committed change already is, rather than
// disappearing when the run stops. It is read only after bindCandidateCommit
// finds nothing pending, so a succeeded mutation awaiting its commit is always
// reported as that, never as a refusal.
func (s *runState) refusedMutation() (RunOperation, bool) {
	var found RunOperation
	ok := false
	for _, kind := range []string{OpExecutionInvoke, OpRemediationGofmt} {
		for _, op := range s.snapshot.Operations {
			if op.Kind != kind || op.State != OperationFailed {
				continue
			}
			var result mutationResult
			if len(op.Result) == 0 || json.Unmarshal(op.Result, &result) != nil || !result.Mutated {
				continue
			}
			if !ok || op.CreatedAt.After(found.CreatedAt) {
				found, ok = op, true
			}
		}
	}
	return found, ok
}

// bounded never lets the terminal event become unappendable. Descriptive
// fields go through the package's one boundedField. An IDENTITY field past the
// bound is recorded as UNKNOWN (empty) instead: a truncated commit or
// operation id would name a different object, which is worse than naming none.
func (h HeldMaterial) bounded() *HeldMaterial {
	for _, field := range []*string{&h.NextStep, &h.BlockedBy, &h.Successor, &h.SuccessorUnavailable} {
		*field = boundedField(*field)
	}
	for _, field := range []*string{&h.Revision, &h.Tree, &h.Operation, &h.ContentDigest} {
		if boundedField(*field) != *field {
			*field = ""
		}
	}
	return &h
}

// verifiedAt reports whether head carries every assurance the lifecycle wants
// before publication: automated assurance passed at that exact commit, no
// independent semantic finding at it failed, and neither assurance operation
// is still wanted and unsatisfied. It asks the assurance bindings directly
// rather than the planner's first pick, which an earlier op could mask.
// Anything less is committed_unverified.
func (s *runState) verifiedAt(head string) bool {
	automated, semantic := s.projection.Assurance, s.projection.SemanticAssurance
	if automated == nil || automated.Commit != head || !automated.Passed {
		return false
	}
	if semantic != nil && semantic.Commit == head && !semantic.Passed {
		return false
	}
	for _, spec := range []operationSpec{{OpAssuranceGo, bindAssuranceGo}, {OpAssuranceSemantic, bindAssuranceSemantic}} {
		if key, wanted := spec.bind(s); wanted && key != "" && !s.satisfied(spec.kind, key) {
			return false
		}
	}
	return true
}

// nextLifecycleStep is the first non-observation operation the planner still
// wants: the step the budget is refusing. Observation is skipped because it
// is always re-wanted at a fresh epoch and says nothing about the material.
func (s *runState) nextLifecycleStep() string {
	for _, spec := range operationSpecs {
		if observationKinds[spec.kind] {
			continue
		}
		if key, wanted := spec.bind(s); wanted && key != "" && !s.satisfied(spec.kind, key) {
			return spec.kind
		}
	}
	return ""
}

// workspaceContentDigest identifies uncommitted workspace material by path and
// content WITHOUT any Git operation: it reads files, it writes no object, index
// or ref. A deleted path, a symlink and a special file are identified as such.
// Only a REGULAR file is ever opened: opening a FIFO a producer left in place
// of a tracked file blocks forever, and a device is not the producer's content.
// An unreadable path makes the whole digest unknown (""), and so does an empty
// change: there is nothing to identify.
//
// ponytail: Lstat-then-open races a process swapping a regular file for a FIFO
// in between. The digest runs after the producer returned, so no runtime-
// launched writer is live; an O_NONBLOCK open is the upgrade if that changes.
func workspaceContentDigest(dir string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, path := range sorted {
		full := filepath.Join(dir, path)
		info, err := os.Lstat(full)
		var identity string
		switch {
		case errors.Is(err, os.ErrNotExist):
			identity = "deleted"
		case err != nil:
			return ""
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return ""
			}
			identity = "symlink:" + target
		case !info.Mode().IsRegular():
			identity = "special:" + info.Mode().Type().String()
		default:
			digest, err := fileDigest(full)
			if err != nil {
				return ""
			}
			identity = "file:" + info.Mode().Perm().String() + ":" + digest
		}
		b.WriteString(path + "\x00" + identity + "\n")
	}
	return textDigest(b.String())
}
