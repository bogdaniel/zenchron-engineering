package runtime

import "time"

func (v verificationExecution) silence(since, now time.Time) (time.Duration, error) {
	store, err := v.Scheduler.permitStore()
	if err != nil {
		return 0, err
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		return 0, err
	}
	for _, p := range permits {
		if p.Parent == v.Parent && p.State == VerificationGranted {
			return 0, nil
		}
	}
	events, err := store.Events(v.Parent.RunID)
	if err != nil {
		return 0, err
	}
	for _, event := range events {
		if event.Type != EventVerificationPermitChanged {
			continue
		}
		var p VerificationPermit
		if err := strictJSON(event.Payload, &p); err != nil {
			return 0, err
		}
		// A completed actual tool is progress, a cancelled capacity wait is not.
		if p.Parent == v.Parent && p.State == VerificationReleased && p.GrantedAt != nil && p.ReleasedAt.After(since) {
			since = *p.ReleasedAt
		}
	}
	return max(0, now.Sub(since)-verificationWait(events, v.Parent.OperationID, since, now)), nil
}

// liveExternalWork credits the unfinished operation inside an external wait.
// A provider can resume while its run still waits for review; omitting its live
// work would let subtracting a nested wait erase previously consumed budget.
func liveExternalWork(events []EngineeringEvent, waitingSince, now time.Time) time.Duration {
	if waitingSince.IsZero() {
		return 0
	}
	started := map[string]time.Time{}
	var work time.Duration
	for _, event := range events {
		switch event.Type {
		case EventOperationBefore:
			started[event.OperationID] = event.OccurredAt
		case EventOperationAfter:
			delete(started, event.OperationID)
		}
	}
	for _, at := range started {
		if at.Before(waitingSince) {
			at = waitingSince
		}
		work += max(0, now.Sub(at))
	}
	return work
}

// verificationWait measures only intervals when tools are requesting capacity
// and none holds it. Tool work is never excluded. Parent settlement closes a
// pending wait even when the tool's cleanup observation arrives afterwards.
func verificationWait(events []EngineeringEvent, operationID string, since, until time.Time) time.Duration {
	tools := map[string]VerificationPermit{}
	var opened time.Time
	var excluded time.Duration
	closeWait := func(at time.Time) {
		if opened.IsZero() {
			return
		}
		start, end := opened, at
		if start.Before(since) {
			start = since
		}
		if end.After(until) {
			end = until
		}
		if end.After(start) {
			excluded += end.Sub(start)
		}
		opened = time.Time{}
	}
	for _, event := range events {
		if event.OccurredAt.After(until) {
			break
		}
		switch event.Type {
		case EventVerificationPermitChanged:
			var p VerificationPermit
			if err := decodeJSON(event.Payload, &p); err != nil {
				continue
			} // Registry validation precedes replay.
			if operationID != "" && p.Parent.OperationID != operationID {
				continue
			}
			if p.State == VerificationReleased {
				delete(tools, p.ID)
			} else {
				tools[p.ID] = p
			}
		case EventOperationAfter:
			for id, p := range tools {
				if p.Parent.OperationID == event.OperationID {
					delete(tools, id)
				}
			}
		case EventOperationBefore:
			var op RunOperation
			if decodeJSON(event.Payload, &op) != nil {
				continue
			}
			for id, p := range tools {
				if p.Parent.OperationID == event.OperationID && p.Parent.Attempt != op.AttemptIdentity {
					delete(tools, id)
				}
			}
		case EventRunCompleted, EventRunFailed, EventRunCancelled:
			clear(tools)
		default:
			continue
		}
		waiting, running := false, false
		for _, p := range tools {
			waiting = waiting || p.State == VerificationWaiting
			running = running || p.State == VerificationGranted
		}
		if !waiting || running {
			closeWait(event.OccurredAt)
			continue
		}
		if opened.IsZero() {
			opened = event.OccurredAt
		}
	}
	closeWait(until)
	return excluded
}

func (s Scheduler) operationVerificationWait(op RunOperation, until time.Time) (time.Duration, error) {
	store, ok := s.Store.(VerificationPermitStore)
	if !ok || op.ActiveSince == nil {
		return 0, nil
	}
	events, err := store.Events(op.RunID)
	if err != nil {
		return 0, err
	}
	return verificationWait(events, op.ID, *op.ActiveSince, until), nil
}
