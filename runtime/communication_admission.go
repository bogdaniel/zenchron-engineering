package runtime

import (
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// batchMessageScope builds one batch's message scope from durable state: its
// units, the subjects of every admitted handoff (owner and exact candidate as
// the runtime bound them), and the messages already admitted. current maps
// each unit to its latest admitted handoff subject.
//
// This is the #470 batch adapter of MessageScope. A WorkGraph (#472) supplies
// the same three facts for its own units.
func batchMessageScope(store *SQLiteOperationStore, batch orchestration.Batch) (orchestration.MessageScope, map[string]*orchestration.HandoffSubject, error) {
	return queryBatchMessageScope(store.db, batch)
}

// queryBatchMessageScope is the sqlExecutor-generic read ResolveDecisionRequest
// (decision_store.go) pins inside its own linearized transaction, exactly as
// the standalone batchMessageScope does outside one: the request's liveness
// and its owner's CURRENT subject must come from the SAME held snapshot the
// final insert commits against, not a read taken moments before it under no
// lock at all.
func queryBatchMessageScope(q sqlExecutor, batch orchestration.Batch) (orchestration.MessageScope, map[string]*orchestration.HandoffSubject, error) {
	scope := orchestration.MessageScope{ID: batch.ID, Subjects: map[string]orchestration.MessageSubject{}}
	current := map[string]*orchestration.HandoffSubject{}
	for _, item := range batch.Items {
		unit := orchestration.BatchItemUnit(item.Issue)
		scope.Units = append(scope.Units, unit)
		handoffs, err := queryRunHandoffs(q, item.RunID)
		if err != nil {
			return orchestration.MessageScope{}, nil, err
		}
		for _, handoff := range handoffs {
			scope.Subjects[handoff.ID] = orchestration.MessageSubject{Handoff: handoff.ID, Owner: unit, Revision: handoff.Subject}
			subject := handoff.Subject
			current[unit] = &subject
		}
	}
	admitted, err := queryScopeMessages(q, batch.ID)
	if err != nil {
		return orchestration.MessageScope{}, nil, err
	}
	scope.Admitted = admitted
	return scope, current, nil
}

// communicationView is what an orchestrated invocation is shown: its own
// unit, every unit's latest admitted handoff (the subjects a Finding may
// name), its inbox, the scope's STILL OPEN decisions, and the minimal fact
// behind each one that has since been resolved (#508).
type communicationView struct {
	Unit     string              `json:"unit"`
	Handoffs map[string]string   `json:"latest_handoffs,omitempty"`
	Inbox    orchestration.Inbox `json:"inbox"`
	// Decisions are the scope's decision_request messages that are live AND
	// still unresolved. A resolved one is never shown here - it is shown in
	// ResolvedDecisions instead, so a unit can never read an answered
	// question as one still waiting on it.
	Decisions []orchestration.EngineeringMessage `json:"open_decisions,omitempty"`
	// ResolvedDecisions is the minimal, untrusted fact for each decision this
	// scope asked and an authorized operator has since answered: the request
	// id, the bounded outcome, optional rationale and who authorized it. It
	// is DATA an invocation may read, never a new instruction or permission -
	// framed exactly like an upstream producer's own report already is.
	ResolvedDecisions []orchestration.DecisionResolution `json:"resolved_decisions,omitempty"`
}

// communicationContext renders the view for one orchestrated run.
func communicationContext(store *SQLiteOperationStore, run EngineeringRun) (string, error) {
	batch, found, err := store.OrchestrationBatch(run.Orchestration.BatchID)
	if err != nil || !found {
		return "", fmt.Errorf("orchestration batch %s of run %s is unreadable (found=%t): %v", run.Orchestration.BatchID, run.ID, found, err)
	}
	scope, current, err := batchMessageScope(store, batch)
	if err != nil {
		return "", err
	}
	unit, ok := batchUnitOf(batch, run.ID)
	if !ok {
		return "", fmt.Errorf("run %s is not an item of batch %s", run.ID, batch.ID)
	}
	open, resolved, err := splitDecisionsByResolution(store, scope.Admitted)
	if err != nil {
		return "", err
	}
	view := communicationView{
		Unit: unit, Handoffs: map[string]string{},
		Inbox:             orchestration.InboxFor(unit, scope.Admitted, current[unit]),
		Decisions:         open,
		ResolvedDecisions: resolved,
	}
	for id, subject := range scope.Subjects {
		if latest := current[subject.Owner]; latest != nil && *latest == subject.Revision {
			view.Handoffs[subject.Owner] = id
		}
	}
	rendered, err := CanonicalJSON(view)
	return string(rendered), err
}

// splitDecisionsByResolution joins the scope's decision requests against the
// durable decision_resolutions store (#508), which #473's OpenDecisions
// cannot do on its own - it has no resolution store to consult. A request
// with a durable resolution moves out of "open" and into "resolved" the
// moment that resolution lands, never a tick later.
func splitDecisionsByResolution(store *SQLiteOperationStore, admitted []orchestration.EngineeringMessage) (
	open []orchestration.EngineeringMessage, resolved []orchestration.DecisionResolution, err error) {
	for _, message := range orchestration.OpenDecisions(admitted) {
		resolution, found, err := store.DecisionResolutionByRequestID(message.ID)
		if err != nil {
			return nil, nil, err
		}
		if found {
			resolved = append(resolved, resolution)
			continue
		}
		open = append(open, message)
	}
	return open, resolved, nil
}

func batchUnitOf(batch orchestration.Batch, runID string) (string, bool) {
	for _, item := range batch.Items {
		if item.RunID == runID {
			return orchestration.BatchItemUnit(item.Issue), true
		}
	}
	return "", false
}

// admitOrchestratedMessages admits every journalled, not yet decided message
// report of one batch, in batch item order and then journal order, so a
// replay after restart decides exactly what the first pass decided.
//
// Every binding comes from the runtime: the source is the run, agent,
// operation and attempt the journal names; the document must still be the one
// whose digest was journalled when the invocation completed; route and
// subject are derived by orchestration.AdmitMessages. A report that can never
// be admitted is refused once, durably, rather than retried every pass.
//
// ponytail: replays each item's journal every pass, like the handoff step;
// index observations by scope if batches grow past tens of items.
func admitOrchestratedMessages(store *SQLiteOperationStore, stateDir string, batch orchestration.Batch, now time.Time) error {
	scope, _, err := batchMessageScope(store, batch)
	if err != nil {
		return err
	}
	decided, err := store.ScopeMessageRefusals(batch.ID)
	if err != nil {
		return err
	}
	for _, message := range scope.Admitted {
		decided[messageInvocationKey(message.Source.RunID, message.Source.OperationID, message.Source.Attempt)] = "admitted"
	}
	for _, item := range batch.Items {
		run, found, err := store.Run(item.RunID)
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
				Unit: orchestration.BatchItemUnit(item.Issue), RunID: run.ID, AgentID: run.AgentID,
				OperationID: observed.OperationID, Attempt: observed.Attempt,
			}
			admitted, reason, err := admitObservedMessages(stateDir, scope, source, observed.DocumentSHA256, now)
			if err != nil {
				return err
			}
			if reason != "" {
				if err := store.RefuseMessages(key, batch.ID, run.ID, reason, now); err != nil {
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

// admitObservedMessages decides one observed report. A non-empty reason is a
// durable refusal; an error is a failure to decide, retried next pass.
func admitObservedMessages(stateDir string, scope orchestration.MessageScope, source orchestration.MessageSource, journalled string, now time.Time) ([]orchestration.EngineeringMessage, string, error) {
	path, err := MessageReportPath(stateDir, ExecutionAttemptRef{RunID: source.RunID, OperationID: source.OperationID, Attempt: source.Attempt})
	if err != nil {
		return nil, "", err
	}
	report, digest, present, err := readMessageReport(path)
	if !present || err != nil || digest != journalled {
		return nil, fmt.Sprintf("the message report is no longer the document journalled when the invocation completed (present=%t, error=%v)", present, err), nil
	}
	admitted, err := orchestration.AdmitMessages(report, digest, scope, source, now)
	if err != nil {
		return nil, err.Error(), nil
	}
	return admitted, "", nil
}

// messageInvocationKey identifies one invocation's report, admitted or refused.
func messageInvocationKey(runID, operationID string, attempt int) string {
	return fmt.Sprintf("%s|%s|%d", runID, operationID, attempt)
}
