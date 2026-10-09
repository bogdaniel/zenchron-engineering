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

// MaxWorkGraphDocumentBytes bounds one graph's CONTENT - its name, revision and
// units - as canonical JSON.
//
// It exists because the per-field bounds below cannot prove transportability:
// 32 units of bounded purpose and bounded dependency lists multiply out well
// past any single control request. A graph this package ACCEPTS must be a graph
// an operator can actually submit, so the document itself is bounded, once, by
// the number the proposal reader uses - rather than left to arithmetic over
// field limits that silently stops fitting.
const MaxWorkGraphDocumentBytes = 6 << 10

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
	// Role is the responsibility type, from #64's catalogue. It names WHO is
	// qualified to perform this unit's work, never WHAT execution algorithm
	// runs it - domain.RoleIntegrator is an ordinary provider-executed unit
	// unless ExecutionKind says otherwise.
	Role domain.EngineeringRole `json:"role"`
	// ExecutionKind names the execution algorithm this unit's child run uses,
	// decided once by the authorized WorkGraph author/compiler and frozen the
	// moment the unit is activated (sameUnit, ValidateMutation). It is
	// independent of Role: a RoleIntegrator unit with an absent/"provider"
	// ExecutionKind still receives an ordinary execution.invoke. The empty
	// value means ExecutionKindProvider, so every graph that predates this
	// field keeps its exact prior behavior and digest.
	ExecutionKind WorkUnitExecutionKind `json:"execution_kind,omitempty"`
	// Issue is the existing issue this unit performs. Agent-backed units
	// execute as ordinary EngineeringRuns, through the same one-issue batch
	// path a direct #470 submission uses.
	Issue     int      `json:"issue"`
	DependsOn []string `json:"depends_on,omitempty"`
	// RequiresReview opts this unit into #474's review-readiness gate: its
	// admitted handoff satisfies a dependent only once an independent
	// decision for the exact bound commit is APPROVE, never merely on
	// ItemCompleted. False (the default, so every graph that predates this
	// field keeps its exact prior behavior and digest) means ordinary #472
	// satisfaction, unchanged.
	RequiresReview bool `json:"requires_review,omitempty"`
}

// WorkUnitExecutionKind is the closed, explicit discriminator for which
// execution algorithm a unit's child run uses (#475). It is never inferred
// from Role, Purpose, an issue title or anything a provider wrote.
type WorkUnitExecutionKind string

const (
	// ExecutionKindProvider is the default, legacy execution path: an
	// ordinary execution.invoke. The zero value names this kind, so an
	// absent ExecutionKind is always ExecutionKindProvider.
	ExecutionKindProvider WorkUnitExecutionKind = "provider"
	// ExecutionKindIntegrationCompose is deterministic Git composition
	// (#475, runtime.IntegrateInputs) over this unit's exact consumed
	// admitted inputs. It never invokes a provider, and it requires at
	// least two dependencies - a single dependency is #472's ordinary
	// dependency delivery, not integration.
	ExecutionKindIntegrationCompose WorkUnitExecutionKind = "integration_compose"
)

// KnownExecutionKind reports whether a kind is in the closed vocabulary,
// including the empty value. An unknown kind is refused rather than passed
// through, exactly as domain.KnownRole refuses an unknown role.
func KnownExecutionKind(kind WorkUnitExecutionKind) bool {
	switch kind {
	case "", ExecutionKindProvider, ExecutionKindIntegrationCompose:
		return true
	default:
		return false
	}
}

// effectiveExecutionKind is the unit's execution kind with its default
// applied, the one place that default is decided.
func (u WorkUnit) effectiveExecutionKind() WorkUnitExecutionKind {
	if u.ExecutionKind == "" {
		return ExecutionKindProvider
	}
	return u.ExecutionKind
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
	}{g.ID, g.Revision, g.canonicalUnits()})
}

// canonicalUnits is the units in a canonical order: by unit id, with each
// dependency list sorted. Two documents that describe the same graph therefore
// have the same content identity, however the proposer happened to order them.
//
// It copies. Sorting g.Units in place would reorder the caller's own slice as a
// side effect of asking a question about it, and the document an operator
// submitted is the document that is stored.
func (g WorkGraph) canonicalUnits() []WorkUnit {
	units := make([]WorkUnit, len(g.Units))
	for i, unit := range g.Units {
		unit.DependsOn = sortedCopy(unit.DependsOn)
		units[i] = unit
	}
	sort.Slice(units, func(i, j int) bool { return units[i].ID < units[j].ID })
	return units
}

// documentBytes is the canonical content this graph would be transported as.
func (g WorkGraph) documentBytes() (int, error) {
	document, err := domain.CanonicalJSON(struct {
		Name     string     `json:"name"`
		Revision int        `json:"revision"`
		Units    []WorkUnit `json:"units"`
	}{g.Name, g.Revision, g.Units})
	if err != nil {
		return 0, err
	}
	return len(document), nil
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
		if !KnownExecutionKind(unit.ExecutionKind) {
			return fmt.Errorf("work unit %q names execution kind %q, which is not in the execution kind vocabulary", unit.ID, unit.ExecutionKind)
		}
		if unit.effectiveExecutionKind() == ExecutionKindIntegrationCompose && len(unit.DependsOn) < 2 {
			return fmt.Errorf("work unit %q is an integration_compose unit; it names %d dependencies, needing at least 2 exact admitted inputs", unit.ID, len(unit.DependsOn))
		}
		// An integration_compose unit's whole producer stage is deterministic
		// Git composition over consumed inputs (#475); it invokes no provider
		// and publishes no PR of its own, so it has no independent review to
		// require - requiring one would simply never satisfy.
		if unit.RequiresReview && unit.effectiveExecutionKind() == ExecutionKindIntegrationCompose {
			return fmt.Errorf("work unit %q is an integration_compose unit; it has no independent review to require", unit.ID)
		}
		if unit.Issue <= 0 {
			return fmt.Errorf("work unit %q names issue %d; a unit performs one existing issue", unit.ID, unit.Issue)
		}
		// TWO UNITS ON ONE ISSUE describe the same work twice. Each would get
		// its own bound child execution, and the runtime allows one live run
		// per issue, so one of them could never start - a graph that can never
		// finish, refused when it is proposed rather than discovered later.
		if other, taken := issues[unit.Issue]; taken {
			return fmt.Errorf("work units %q and %q both perform issue %d, which can have one live run", other, unit.ID, unit.Issue)
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
	if _, err := topologicalOrder(g.Units); err != nil {
		return err
	}
	size, err := g.documentBytes()
	if err != nil {
		return err
	}
	if size > MaxWorkGraphDocumentBytes {
		return fmt.Errorf("work graph document is %d bytes, above the %d byte bound one control request carries", size, MaxWorkGraphDocumentBytes)
	}
	return nil
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
			return fmt.Errorf("work unit %q has already been activated, so revision %d may not change its issue, role, execution kind, purpose or dependencies", unit.ID, next.Revision)
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
	if a.Issue != b.Issue || a.Role != b.Role || a.effectiveExecutionKind() != b.effectiveExecutionKind() ||
		a.Purpose != b.Purpose || len(a.DependsOn) != len(b.DependsOn) {
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
