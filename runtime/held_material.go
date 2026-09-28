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
	// HeldUncommitted is a succeeded producing operation's workspace change
	// that the runtime never committed because the budget ended the run
	// first. It exists only in the candidate workspace.
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

func (h HeldMaterial) validate() error {
	return errors.Join(
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

// BudgetBoundary reports whether a terminal failure reason is a budget
// boundary. Every budget reason the runtime settles on ends in _exhausted: the
// run wall, the lifecycle deadline, the continuation and provider-invocation
// ceilings, and an operation's attempt budget.
func BudgetBoundary(disposition Disposition, reason string) bool {
	return disposition == Failed && strings.HasSuffix(reason, "_exhausted")
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
		var record mutationResult
		// A result that does not decode leaves the identity unknown, never
		// invented: the kind and the producing operation still stand.
		_ = json.Unmarshal(s.snapshot.Operations[producing].Result, &record)
		held.Kind, held.Operation = HeldUncommitted, producing
		if len(producing) > maxPayloadFieldBytes {
			// Never make the terminal event unappendable: an id past the field
			// bound is recorded as unknown rather than truncated into a lie.
			held.Operation = ""
		}
		held.PathCount, held.ContentDigest = record.PathCount, record.ContentDigest
		held.Revision, held.Tree = head, s.projection.CandidateTree
		if head == "" {
			held.Revision, held.Tree = s.baseRevision(), ""
		}
		return &held
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
	case s.verifiedAt(head, held.NextStep):
		held.Kind = HeldVerifiedUnpublished
	default:
		held.Kind = HeldCommittedUnverified
	}
	return &held
}

// verifiedAt reports whether head carries every assurance the lifecycle wants
// before publication: automated assurance passed at that exact commit, no
// independent semantic finding at it failed, and no assurance step is still
// the next one wanted. Anything less is committed_unverified.
func (s *runState) verifiedAt(head, next string) bool {
	automated, semantic := s.projection.Assurance, s.projection.SemanticAssurance
	if automated == nil || automated.Commit != head || !automated.Passed {
		return false
	}
	if semantic != nil && semantic.Commit == head && !semantic.Passed {
		return false
	}
	return next != OpAssuranceGo && next != OpAssuranceSemantic
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
// or ref. A deleted path and a symlink are identified as such. An unreadable
// path makes the whole digest unknown (""), and so does an empty change: there
// is nothing to identify.
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
