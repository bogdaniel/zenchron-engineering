package orchestration

// The WorkGraph (#472): the durable operational dependency graph.
//
// It answers five questions and no others: what work units exist, what depends
// on what, which dependencies are satisfied, which units are runnable now, and
// which child EngineeringRun belongs to a unit. "Which eligible operation
// executes now" remains the existing scheduler's question, and the graph has no
// pool, semaphore, lease, attempt or retry of its own to answer it with.
//
// A unit is satisfied by an ADMITTED #470 EngineeringHandoff bound to the exact
// candidate its child committed - never by a provider exiting. This file
// consumes that primitive and does not redesign it.
//
// It is also not a second planner. #64's EngineeringPlan answers which stages
// one objective requires, resolves them to profiles and carries the approval
// boundary. A WorkGraph advances existing issue-backed work units through
// handoffs, and borrows #64's role catalogue rather than inventing a second
// vocabulary for the same fact.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// WorkGraphSchemaVersion versions the durable graph revision document.
const WorkGraphSchemaVersion = "0.1"

// MaxWorkGraphUnits bounds one graph. A graph is a coordination structure an
// operator or a planner proposes, so it is bounded like any other untrusted
// payload, and the bound is #470's cohort bound for the same reason: a proposal
// has to fit inside one bounded control request. It is not a concurrency
// number - the scheduler owns the only one.
const MaxWorkGraphUnits = MaxBatchItems

const (
	maxWorkGraphNameBytes = 200
	maxUnitIDBytes        = 64
	maxUnitPurposeBytes   = 1 << 10
)

// WorkGraph is one revision of a graph. Every revision is written once and kept:
// the document is immutable, and progression lives in the activation records and
// the child runs, never in a mutated field of this document.
type WorkGraph struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Repository    string `json:"repository"`
	// AgentID is the execution agent every unit's child run is created under.
	// It is explicit for the same reason a batch's is: a graph spends one
	// agent's capacity, and defaulting it would hide who does the work.
	AgentID string `json:"agent_id"`
	// Name is the operator's stable name for this graph. Identity is derived
	// from it, so resubmitting the same graph finds the one already adopted
	// rather than creating a second.
	Name        string     `json:"name"`
	Revision    int        `json:"revision"`
	RequestedBy string     `json:"requested_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Units       []WorkUnit `json:"units"`
}

// WorkUnit is one bounded engineering responsibility and the dependencies whose
// admitted output it consumes.
//
// Its required INPUT references are DependsOn: each named unit's admitted
// handoff subject. Its OUTPUT reference is its own admitted handoff, which the
// graph deliberately does not restate - #470 owns that record, and copying the
// subject here would be a second answer to what the unit produced.
type WorkUnit struct {
	ID      string `json:"id"`
	Purpose string `json:"purpose"`
	// Role is the responsibility type, from #64's catalogue.
	Role domain.EngineeringRole `json:"role"`
	// Issue is the existing issue this unit performs. Agent-backed units
	// execute as ordinary EngineeringRuns, through the same one-issue batch
	// path a direct #470 submission uses.
	Issue     int      `json:"issue"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// WorkGraphID is the deterministic identity of a graph: the same repository,
// agent and name always name the same graph across revisions, which is what
// makes a lost reply harmless and a mutation an append rather than a new graph.
func WorkGraphID(repository, agentID, name string) (string, error) {
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(agentID) == "" || strings.TrimSpace(name) == "" {
		return "", errors.New("a work graph needs a repository, an execution agent and a name")
	}
	if len(name) > maxWorkGraphNameBytes {
		return "", fmt.Errorf("work graph name is %d bytes, above the %d byte bound", len(name), maxWorkGraphNameBytes)
	}
	digest, err := domain.Digest(struct {
		Repository string `json:"repository"`
		Agent      string `json:"agent"`
		Name       string `json:"name"`
	}{strings.ToLower(repository), agentID, name})
	if err != nil {
		return "", err
	}
	return "graph-" + digest[:32], nil
}

// RevisionDigest is the CONTENT identity of one revision: its graph, its
// revision number and its units.
//
// Composition time and requester are deliberately excluded. A resubmission of
// the same proposal is the same revision even though it was composed a moment
// later, and that is exactly what lets a lost reply be answered by finding the
// revision already adopted instead of refusing it as a conflict.
func (g WorkGraph) RevisionDigest() (string, error) {
	return domain.Digest(struct {
		ID       string     `json:"id"`
		Revision int        `json:"revision"`
		Units    []WorkUnit `json:"units"`
	}{g.ID, g.Revision, g.Units})
}

// WorkGraphProposal is a proposed revision as an operator - or, later, a planner
// or a model - states it. Nothing here is trusted: it becomes a WorkGraph only
// through Compose, and is adopted only after Validate and, for a mutation,
// ValidateMutation.
type WorkGraphProposal struct {
	Name string `json:"name"`
	// Revision is the revision being proposed. 1 adopts a new graph; N+1
	// mutates the graph currently at N. It is explicit so two proposers cannot
	// both believe they are appending to the revision they read.
	Revision int        `json:"revision"`
	Units    []WorkUnit `json:"units"`
}

// Compose turns a proposal into a candidate revision under the repository,
// agent and clock the RUNTIME owns. A proposer states units; it never states
// which repository or agent they run under, or when it happened.
func (p WorkGraphProposal) Compose(repository, agentID, requestedBy string, at time.Time) (WorkGraph, error) {
	id, err := WorkGraphID(repository, agentID, p.Name)
	if err != nil {
		return WorkGraph{}, err
	}
	graph := WorkGraph{
		SchemaVersion: WorkGraphSchemaVersion, ID: id, Repository: repository,
		AgentID: agentID, Name: p.Name, Revision: p.Revision,
		RequestedBy: requestedBy, CreatedAt: at, Units: p.Units,
	}
	if err := graph.Validate(); err != nil {
		return WorkGraph{}, err
	}
	return graph, nil
}

// Validate refuses every graph this build could not advance, and every graph
// that would quietly mean something other than what it says. It is the complete
// deterministic admission check: a mutation proposed by a model is admitted only
// by passing this and ValidateMutation, never by being plausible.
func (g WorkGraph) Validate() error {
	if g.SchemaVersion != WorkGraphSchemaVersion {
		return fmt.Errorf("work graph schema version %q is not %q", g.SchemaVersion, WorkGraphSchemaVersion)
	}
	if err := boundedField("work graph name", g.Name, maxWorkGraphNameBytes); err != nil {
		return err
	}
	id, err := WorkGraphID(g.Repository, g.AgentID, g.Name)
	if err != nil {
		return err
	}
	if id != g.ID {
		return fmt.Errorf("work graph %s does not match the identity %s of its own repository, agent and name", g.ID, id)
	}
	if g.Revision < 1 {
		return fmt.Errorf("work graph revision %d is not a revision", g.Revision)
	}
	if g.CreatedAt.IsZero() {
		return errors.New("work graph creation time is required")
	}
	if len(g.Units) == 0 {
		return errors.New("a work graph names at least one work unit")
	}
	if len(g.Units) > MaxWorkGraphUnits {
		return fmt.Errorf("work graph names %d units, above the %d unit bound", len(g.Units), MaxWorkGraphUnits)
	}
	byID := make(map[string]WorkUnit, len(g.Units))
	issues := make(map[int]string, len(g.Units))
	for _, unit := range g.Units {
		if err := boundedField("work unit id", unit.ID, maxUnitIDBytes); err != nil {
			return err
		}
		if _, exists := byID[unit.ID]; exists {
			return fmt.Errorf("work unit %q is named more than once", unit.ID)
		}
		byID[unit.ID] = unit
		if err := boundedField(fmt.Sprintf("work unit %q purpose", unit.ID), unit.Purpose, maxUnitPurposeBytes); err != nil {
			return err
		}
		if !domain.KnownRole(unit.Role) {
			return fmt.Errorf("work unit %q names role %q, which is not in the role catalogue", unit.ID, unit.Role)
		}
		if unit.Issue <= 0 {
			return fmt.Errorf("work unit %q names issue %d; a unit performs one existing issue", unit.ID, unit.Issue)
		}
		// TWO UNITS ON ONE ISSUE would resolve to one child run identity, so
		// the graph would show two units advancing on one run's single output.
		if other, taken := issues[unit.Issue]; taken {
			return fmt.Errorf("work units %q and %q both perform issue %d, which has one child run", other, unit.ID, unit.Issue)
		}
		issues[unit.Issue] = unit.ID
	}
	for _, unit := range g.Units {
		seen := map[string]bool{}
		for _, dependency := range unit.DependsOn {
			if dependency == unit.ID {
				return fmt.Errorf("work unit %q depends on itself", unit.ID)
			}
			if seen[dependency] {
				return fmt.Errorf("work unit %q lists dependency %q twice", unit.ID, dependency)
			}
			seen[dependency] = true
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("work unit %q depends on %q, which is not a unit of this graph", unit.ID, dependency)
			}
		}
	}
	_, err = topologicalOrder(g.Units)
	return err
}

// ValidateMutation is the deterministic gate a proposed next revision passes
// before anything adopts it. A graph mutation may be PROPOSED by a planner or a
// model; nothing here trusts the proposer.
//
// activated names every unit whose child run this graph has already claimed.
// Such a unit is FROZEN: a revision that removed it, re-pointed it at another
// issue, or changed what it consumes would retroactively describe work that has
// already been performed against the inputs the old revision named.
//
// Nothing here can reset a consumed budget: a revision is an append, the
// activation records and their child runs are untouched by it, and a run's
// budget is the run's own.
func ValidateMutation(current, next WorkGraph, activated map[string]bool) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.ID != current.ID || next.Repository != current.Repository ||
		next.AgentID != current.AgentID || next.Name != current.Name {
		return fmt.Errorf("work graph revision %d describes a different graph than %s", next.Revision, current.ID)
	}
	if next.Revision != current.Revision+1 {
		return fmt.Errorf("work graph %s is at revision %d; the next revision is %d, not %d",
			current.ID, current.Revision, current.Revision+1, next.Revision)
	}
	byID := make(map[string]WorkUnit, len(next.Units))
	for _, unit := range next.Units {
		byID[unit.ID] = unit
	}
	for _, unit := range current.Units {
		if !activated[unit.ID] {
			continue
		}
		proposed, kept := byID[unit.ID]
		if !kept {
			return fmt.Errorf("work unit %q has already been activated, so revision %d may not remove it", unit.ID, next.Revision)
		}
		if !sameUnit(unit, proposed) {
			return fmt.Errorf("work unit %q has already been activated, so revision %d may not change its issue, role, purpose or dependencies", unit.ID, next.Revision)
		}
	}
	return nil
}

// boundedField refuses operator- or proposer-authored text that is absent, over
// bound, or not text. It is deliberately separate from the handoff report's own
// bound checks: those belong to the worker protocol #492 owns, and sharing one
// helper would couple a graph's refusals to that protocol's wording.
func boundedField(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes, above the %d byte bound", name, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

func sameUnit(a, b WorkUnit) bool {
	if a.Issue != b.Issue || a.Role != b.Role || a.Purpose != b.Purpose || len(a.DependsOn) != len(b.DependsOn) {
		return false
	}
	left, right := sortedCopy(a.DependsOn), sortedCopy(b.DependsOn)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// topologicalOrder is a deterministic dependency-first order over the units, and
// the cycle refusal. The order makes projection decide an upstream unit before
// anything that consumes it, so no pass can read a dependency's state before it
// has one. A cycle is reported with the units still unresolved, so an operator
// sees the loop rather than a bare "invalid graph".
func topologicalOrder(units []WorkUnit) ([]string, error) {
	remaining := make(map[string]int, len(units))
	dependents := make(map[string][]string, len(units))
	for _, unit := range units {
		remaining[unit.ID] = len(unit.DependsOn)
		for _, dependency := range unit.DependsOn {
			dependents[dependency] = append(dependents[dependency], unit.ID)
		}
	}
	ready := make([]string, 0, len(units))
	for id, count := range remaining {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(units))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := sortedCopy(dependents[id])
		for _, dependent := range next {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) == len(units) {
		return order, nil
	}
	unresolved := make([]string, 0, len(units)-len(order))
	for id, count := range remaining {
		if count > 0 {
			unresolved = append(unresolved, id)
		}
	}
	sort.Strings(unresolved)
	return nil, fmt.Errorf("work unit dependencies form a cycle among %s", strings.Join(unresolved, ", "))
}
