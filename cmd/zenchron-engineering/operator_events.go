package main

// The `autonomy events` command: the append-only journal rendered as one
// bounded line per event, once or followed. Moved out of operator.go
// unchanged so that file keeps headroom under its size ceiling; it shares the
// operator commands' conventions and nothing else.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// maxEventPayloadBytes bounds ONE rendered payload. Canonical payloads are
// already bounded by the journal, so this is a display bound: it keeps a
// follower's output readable and keeps the renderer honest about the fact that
// it prints a SUMMARY, never a body.
const maxEventPayloadBytes = 1024

// followInterval is how often `--follow` re-reads the journal. It is a polite
// poll of a local SQLite file, not a subscription: no lock is taken, no
// notification channel is opened, and a controller appending concurrently is
// simply observed on the next pass.
const followInterval = time.Second

// eventView renders one journalled event. Every member is journal identity,
// journal position, or a bounded summary. Artifact CONTENT is never printed:
// what is rendered is the artifact REFERENCE the journal itself holds - a path,
// a digest, and its local-only and sanitized flags - so listing events can
// never emit raw local-only material.
type eventView struct {
	SchemaVersion string    `json:"schema_version"`
	View          string    `json:"view"`
	Sequence      int64     `json:"sequence"`
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	Type          string    `json:"type"`
	OccurredAt    time.Time `json:"occurred_at"`
	Actor         string    `json:"actor"`
	OperationID   string    `json:"operation_id,omitempty"`
	// Rendered as recorded, for diagnostics: the journal's state digests are
	// not authoritative (#453) and this view never interprets them.
	StateBefore       string             `json:"state_before,omitempty"`
	StateAfter        string             `json:"state_after,omitempty"`
	Subject           *eventSubject      `json:"subject,omitempty"`
	Payload           json.RawMessage    `json:"payload,omitempty"`
	PayloadSummary    *payloadSummary    `json:"payload_summary,omitempty"`
	Artifacts         []runtime.Artifact `json:"artifacts,omitempty"`
	PreviousEventID   string             `json:"previous_event_id,omitempty"`
	PreviousEventHash string             `json:"previous_event_hash,omitempty"`
	EventHash         string             `json:"event_hash,omitempty"`
}

// eventSubject is the exact state an event is bound to, when its payload names
// one. It is what makes a journal line answerable without opening the payload.
type eventSubject struct {
	Commit    string             `json:"commit,omitempty"`
	Tree      string             `json:"tree,omitempty"`
	Contract  *runtime.Ref       `json:"contract,omitempty"`
	Candidate *runtime.Candidate `json:"candidate,omitempty"`
	Action    string             `json:"action,omitempty"`
	Request   *runtime.Ref       `json:"request,omitempty"`
}

// payloadSummary replaces a payload too large to render. It states the size and
// the digest of what was withheld rather than truncating JSON into something
// that no longer parses.
type payloadSummary struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// autonomyEvents renders the persisted journal in persisted order. It opens the
// durable store DIRECTLY: it takes no ownership of the state directory and no
// run-driving lease, which is what makes it safe - and correct - to read a run
// another controller is currently driving.
func autonomyEvents(parent context.Context, flags autonomyFlags, runID string, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer store.Close()

	if _, found, err := store.Run(runID); err != nil {
		return runtime.ExitFailed, err
	} else if !found {
		err := &runNotFoundError{RunID: runID}
		return exitRunNotFound, err
	}
	if !flags.Follow {
		events, err := store.Events(runID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		if err := writeJSON(stdout, eventViews(events)); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	return followEvents(parent, store, runID, stdout)
}

// followEvents tails the journal. It mutates nothing: the only call it makes is
// a cursor-filtered journal read, so a concurrent append by an
// independent handle is simply the next thing it observes. It exits cleanly on
// SIGINT or SIGTERM, which is a shutdown of THIS reader and never a
// cancellation of the run.
func followEvents(parent context.Context, store *runtime.SQLiteOperationStore, runID string, stdout io.Writer) (int, error) {
	ctx, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var last int64
	ticker := time.NewTicker(followInterval)
	defer ticker.Stop()
	for {
		events, err := store.EventsAfter(runID, last)
		if err != nil {
			return runtime.ExitFailed, err
		}
		for _, event := range events {
			if event.Sequence <= last {
				continue
			}
			last = event.Sequence
			if err := writeJSON(stdout, renderEvent(event)); err != nil {
				return runtime.ExitFailed, err
			}
		}
		select {
		case <-ctx.Done():
			return runtime.ExitCompleted, nil
		case <-ticker.C:
		}
	}
}

func eventViews(events []runtime.EngineeringEvent) []eventView {
	out := make([]eventView, 0, len(events))
	for _, event := range events {
		out = append(out, renderEvent(event))
	}
	return out
}

// renderEvent renders one journalled row. Nothing is re-ordered, nothing is
// filtered, and no artifact body is opened.
func renderEvent(event runtime.EngineeringEvent) eventView {
	view := eventView{
		SchemaVersion:     event.SchemaVersion,
		View:              "autonomy.events/1",
		Sequence:          event.Sequence,
		ID:                event.ID,
		RunID:             event.RunID,
		Type:              event.Type,
		OccurredAt:        event.OccurredAt,
		Actor:             eventActor(event),
		OperationID:       event.OperationID,
		StateBefore:       event.StateBefore,
		StateAfter:        event.StateAfter,
		Subject:           eventSubjectOf(event.Payload),
		Artifacts:         event.Artifacts,
		PreviousEventID:   event.PreviousEventID,
		PreviousEventHash: event.PreviousEventHash,
		EventHash:         event.EventHash,
	}
	if len(event.Payload) > maxEventPayloadBytes {
		sum := sha256.Sum256(event.Payload)
		view.PayloadSummary = &payloadSummary{Bytes: len(event.Payload), SHA256: hex.EncodeToString(sum[:])}
		return view
	}
	view.Payload = event.Payload
	return view
}

// eventActor is where the event came from. An operator-appended event carries
// no operation id, because recording operator intent is not an engineering side
// effect and never takes an operation lease; that is what distinguishes it.
func eventActor(event runtime.EngineeringEvent) string {
	switch event.Type {
	case runtime.EventHumanAuthorityRecorded:
		return "operator"
	case runtime.EventRunCancelled:
		if event.OperationID == "" {
			return "operator"
		}
	case runtime.EventGitHubPRObserved, runtime.EventGitHubCIObserved, runtime.EventGitHubReviewObserved,
		runtime.EventSourceIntentChanged, runtime.EventSourceOptInRemoved, runtime.EventSourceOptInRestored:
		return "forge"
	}
	return "runtime"
}

// eventSubjectOf reads the exact state binding a payload names, if it names
// one. It decodes the shared member names rather than every payload type, so a
// payload the renderer has never heard of still reports its binding.
func eventSubjectOf(payload json.RawMessage) *eventSubject {
	if len(payload) == 0 {
		return nil
	}
	var bound struct {
		Commit    string             `json:"commit"`
		Tree      string             `json:"tree"`
		Contract  *runtime.Ref       `json:"contract"`
		Candidate *runtime.Candidate `json:"candidate"`
		Action    *domain.Action     `json:"action"`
		Request   *runtime.Ref       `json:"request"`
	}
	if json.Unmarshal(payload, &bound) != nil {
		return nil
	}
	subject := eventSubject{
		Commit: bound.Commit, Tree: bound.Tree,
		Contract: bound.Contract, Candidate: bound.Candidate, Request: bound.Request,
	}
	if bound.Action != nil {
		subject.Action = bound.Action.Type + ":" + bound.Action.Target
	}
	if subject == (eventSubject{}) {
		return nil
	}
	return &subject
}
