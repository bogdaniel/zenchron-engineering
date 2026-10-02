package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// The run-detail page is the one surface every nil-able console field (held
// material, assurance, authority, the live operation overlay, budgets) can
// reach at once. A fully populated RunDetail exercises every {{with}}/{{if}}
// guard in run_detail.html; an empty one (TestControlPlaneOverviewAndRuns...
// in control_plane_server_test.go) only exercises their "absent" branches.
// Both have to render without the template engine panicking on a nil pointer
// dereference.
func TestRunDetailTemplateRendersFullyPopulatedState(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lastProgress := started.Add(90 * time.Second)
	deadline := started.Add(time.Hour)

	detail := runtime.RunDetail{
		Summary: runtime.RunSummary{
			RunID: "run-rich", Repository: "acme/widgets", Issue: 42,
			Agent: "codex", ProviderKind: "codex-cli", Model: "gpt-5",
			Phase: runtime.Execute, Disposition: runtime.Waiting, Reason: "awaiting_review",
			Operation: "execute", Attempt: 2, Executing: true,
			Elapsed: 45 * time.Minute,
			Branch:  "zenchron/run-rich", CandidateRevision: "0123456789abcdef", CandidateTree: "fedcba9876543210",
			Workspace:   "/var/zenchron/run-rich",
			PullRequest: 99, PRState: "open", CI: "pending", Review: "requested",
			FeedbackAdmitted: 3, FeedbackPending: 1,
			Attempts: map[string]int{"execute": 2, "assure": 1},
		},
		HeldMaterial: &runtime.HeldMaterial{
			Kind: runtime.HeldUncommitted, Revision: "0123456789abcdef", Operation: "op-5",
			PathCount: 4, NextStep: "candidate.commit", BlockedBy: "run_wall_budget_exhausted",
			Successor: "continuation", SuccessorUnavailable: "run_active_work_exhausted",
		},
		Assurance: &runtime.AssuranceObservation{
			AssuranceObservedPayload: runtime.AssuranceObservedPayload{Passed: false, FailureClass: "test_failure"},
		},
		SemanticAssurance: &runtime.AssuranceObservation{
			AssuranceObservedPayload: runtime.AssuranceObservedPayload{Passed: true},
		},
		PublicationAuthority: &runtime.PublicationAuthority{Status: domain.AuthorityAuthorized},
		AuthorityDecisions: map[string]runtime.AuthorityEvaluation{
			"git.pull_request.create\x00main": {
				AuthorityEvaluatedPayload: runtime.AuthorityEvaluatedPayload{Status: domain.AuthorityAuthorized},
			},
		},
		ExecutionDiagnostic: &runtime.ExecutionDiagnostic{
			Stage: "execute", FailureClass: "provider_error", Code: "rate_limited",
			Message: "the provider refused the request", Successor: "retry",
		},
		CandidateComplete: false,
		Checkpoints:       2,
		Budgets: &runtime.RunBudgets{
			WallLimit: 2 * time.Hour, MaxExecutionAttempts: 5, MaxExecutionContinuations: 3,
		},
		Operation: &runtime.ConsoleOperation{
			ID: "op-5", Kind: "execute", State: runtime.Running,
			Attempt: 2, MaxAttempts: 5,
			StartedAt: &started, LastProgressAt: &lastProgress, SilentFor: 30 * time.Second,
			ProgressSource: "row", Deadline: &deadline, DeadlineBound: "provider_inactivity",
		},
	}

	events := []eventView{
		{
			Sequence: 1, ID: "evt-1", RunID: "run-rich", Type: "run.created",
			OccurredAt: started, Actor: "runtime", StateAfter: "contract",
			Payload: json.RawMessage(`{"goal":"fix the thing"}`),
		},
		{
			Sequence: 2, ID: "evt-2", RunID: "run-rich", Type: "candidate.committed",
			OccurredAt: lastProgress, Actor: "runtime", StateBefore: "execute", StateAfter: "observe",
			Subject:   &eventSubject{Commit: "0123456789abcdef", Tree: "fedcba9876543210", Action: "git.pull_request.create:main"},
			Artifacts: []runtime.Artifact{{Path: "transcript.log", SHA256: "deadbeef", LocalOnly: true, Sanitized: true}},
		},
		{
			Sequence: 3, ID: "evt-3", RunID: "run-rich", Type: "provider.invoked",
			OccurredAt: deadline, Actor: "runtime",
			PayloadSummary: &payloadSummary{Bytes: 4096, SHA256: "abc123"},
		},
	}

	data := runDetailData{ObservedAt: deadline, Detail: detail, Events: events}

	rec := httptest.NewRecorder()
	if err := renderControlPlaneTemplate(rec, runDetailTemplate, data); err != nil {
		t.Fatalf("render run detail: %v", err)
	}
	rendered := rec.Body.String()

	for _, want := range []string{
		"run-rich", "acme/widgets", "awaiting_review",
		"candidate incomplete", "2 checkpoint(s)",
		"uncommitted", "run_wall_budget_exhausted", "continuation",
		"failed", "(test_failure)", "passed",
		"authorized",
		"provider_error", "rate_limited", "the provider refused the request",
		"wall limit 2h0m0s", "max continuations 3",
		"row", "provider_inactivity",
		"run.created", "candidate.committed", "provider.invoked",
		"transcript.log", "sha256:deadbeef",
		"payload withheld: 4096 bytes",
		"fix the thing",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("run detail page does not render %q:\n%s", want, rendered)
		}
	}
}

// The overview page's controller block has to survive every observation
// being absent (an unreachable controller, no generation recorded) without
// dereferencing a nil pointer: ConsoleOperation/ControllerStatus's pointer
// fields are exactly the "not observed" case the rest of this codebase
// insists on rendering as UNKNOWN rather than inferring.
func TestOverviewTemplateRendersUnreachableController(t *testing.T) {
	data := overviewData{
		ObservedAt: time.Now().UTC(),
		Controller: runtime.ControllerStatus{
			DurableConsistency: runtime.DurableUnrecorded,
			Serving:            runtime.ServingUnknown,
			Live:               runtime.LiveObservation{Reachable: false, Detail: "no control endpoint is available"},
			Projection:         runtime.ProjectionObservation{State: runtime.ProjectionMissing},
			Findings:           []string{"no activation has ever been recorded"},
		},
		Fleet: runtime.ConsoleFleetView{Capacity: 2},
	}
	var buf bytes.Buffer
	rec := httptest.NewRecorder()
	if err := renderControlPlaneTemplate(rec, overviewTemplate, data); err != nil {
		t.Fatalf("render overview: %v", err)
	}
	buf.Write(rec.Body.Bytes())
	rendered := buf.String()
	for _, want := range []string{"no_activation_recorded", "unknown", "not reachable", "no control endpoint is available", "no activation has ever been recorded"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("overview page does not render %q:\n%s", want, rendered)
		}
	}
}
