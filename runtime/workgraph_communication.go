package runtime

// The WorkGraph-wide message scope (#474): the cross-unit seam #472 and #473
// each deliberately left unowned (docs/workgraph.md, "Not in this ticket").
//
// #473's MessageScope is supplied, today, only by one #470 batch - and a
// WorkGraph unit's child run is its OWN one-issue batch (runtime/workgraph.go),
// so that batch's scope names only itself. A reviewer unit could never even
// name a sibling producer's handoff as a Finding's subject under that scope.
//
// This file supplies the OTHER scope #473's own doc comment names as pending:
// "A WorkGraph (#472) supplies the same three facts for its own units"
// (communication_admission.go). It reuses #473's types and admission rule
// unchanged - no second Finding format, no new scheduler, no new store.
//
// A message admitted here is admitted exactly once: reconcileOrchestration
// (#470) skips its own per-batch admission for a WorkGraph-activated batch
// (batch.Origin != nil), because a message's identity (MessageID) is a pure
// function of the invocation alone, not of which scope admits it - admitting
// the same invocation under two scopes would collide on that identity.

import (
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// workGraphMessageScope builds one graph's message scope from durable state,
// read fresh: its activated units, the subjects of their admitted handoffs,
// and the messages already admitted under the graph's own scope (its stable
// GraphID, not any one unit's batch).
func workGraphMessageScope(store *SQLiteOperationStore, stateDir, graphID string, now time.Time) (orchestration.MessageScope, map[string]*orchestration.HandoffSubject, error) {
	view, err := WorkGraphStatus(store, stateDir, graphID, now, nil)
	if err != nil {
		return orchestration.MessageScope{}, nil, err
	}
	return graphViewMessageScope(store, view)
}

// graphViewMessageScope is workGraphMessageScope over an already-projected
// view, so a caller that projected one for another reason (the reconciler)
// does not pay for a second read of the same graph.
func graphViewMessageScope(store *SQLiteOperationStore, view WorkGraphView) (orchestration.MessageScope, map[string]*orchestration.HandoffSubject, error) {
	scope := orchestration.MessageScope{ID: view.GraphID, Subjects: map[string]orchestration.MessageSubject{}}
	current := map[string]*orchestration.HandoffSubject{}
	for _, unit := range view.Units {
		if unit.RunID == "" {
			// Not yet activated: it is not a unit of this scope until it has a
			// child run whose invocations could ever write into it.
			continue
		}
		owner := orchestration.BatchItemUnit(unit.Issue)
		scope.Units = append(scope.Units, owner)
		handoffs, err := store.RunHandoffs(unit.RunID)
		if err != nil {
			return orchestration.MessageScope{}, nil, err
		}
		for _, handoff := range handoffs {
			scope.Subjects[handoff.ID] = orchestration.MessageSubject{Handoff: handoff.ID, Owner: owner, Revision: handoff.Subject}
			subject := handoff.Subject
			current[owner] = &subject
		}
	}
	admitted, err := store.ScopeMessages(view.GraphID)
	if err != nil {
		return orchestration.MessageScope{}, nil, err
	}
	scope.Admitted = admitted
	return scope, current, nil
}

// reconcileWorkGraphMessages is the supervisor pass's graph-message step: it
// admits every journalled, not yet decided message report of every graph's
// activated units, against that graph's own scope. One graph's problem never
// stops another.
func (s *Supervisor) reconcileWorkGraphMessages() []string {
	graphs, err := s.deps.Store.WorkGraphs()
	if err != nil {
		return []string{boundedDetail(err.Error())}
	}
	var problems []string
	now := s.deps.Clock.Now()
	for _, graph := range graphs {
		view, err := WorkGraphStatus(s.deps.Store, s.deps.StateDir, graph.ID, now, nil)
		if err != nil {
			problems = append(problems, boundedDetail(graph.ID+": "+err.Error()))
			continue
		}
		if err := admitWorkGraphMessages(s.deps.Store, s.deps.StateDir, view, now); err != nil {
			problems = append(problems, boundedDetail(graph.ID+" messages: "+err.Error()))
		}
	}
	return problems
}

// admitWorkGraphMessages admits one graph's outstanding message reports, in
// unit order and then journal order, so a replay after restart decides
// exactly what the first pass decided - the same determinism
// admitOrchestratedMessages gives one batch.
func admitWorkGraphMessages(store *SQLiteOperationStore, stateDir string, view WorkGraphView, now time.Time) error {
	scope, _, err := graphViewMessageScope(store, view)
	if err != nil {
		return err
	}
	decided, err := store.ScopeMessageRefusals(view.GraphID)
	if err != nil {
		return err
	}
	for _, message := range scope.Admitted {
		decided[messageInvocationKey(message.Source.RunID, message.Source.OperationID, message.Source.Attempt)] = "admitted"
	}
	for _, unit := range view.Units {
		if unit.RunID == "" {
			continue
		}
		run, found, err := store.Run(unit.RunID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		events, err := store.Events(run.ID)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Type != EventMessagesObserved {
				continue
			}
			observed, err := decodePayload[MessagesObservedPayload](event.Payload)
			if err != nil {
				return err
			}
			key := messageInvocationKey(run.ID, observed.OperationID, observed.Attempt)
			if observed.Refusal != "" || decided[key] != "" {
				continue
			}
			source := orchestration.MessageSource{
				Unit: orchestration.BatchItemUnit(unit.Issue), RunID: run.ID, AgentID: run.AgentID,
				OperationID: observed.OperationID, Attempt: observed.Attempt,
			}
			admitted, reason, err := admitObservedMessages(stateDir, scope, source, observed.DocumentSHA256, now)
			if err != nil {
				return err
			}
			if reason != "" {
				if err := store.RefuseMessages(key, view.GraphID, run.ID, reason, now); err != nil {
					return err
				}
				decided[key] = reason
				continue
			}
			if err := store.AdmitMessages(admitted); err != nil {
				return err
			}
			scope.Admitted = append(scope.Admitted, admitted...)
			decided[key] = "admitted"
		}
	}
	return nil
}
