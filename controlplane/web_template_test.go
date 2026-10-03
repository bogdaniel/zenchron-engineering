package controlplane

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// renderTemplate executes a page template exactly as Web.render does, so a
// broken field reference (a DTO field renamed or reshaped without updating
// its template) fails here instead of being masked by
// httptest.ResponseRecorder's sticky-first-WriteHeader behavior (the
// recorder keeps its 200 from a partial write even after render() reports a
// 500, so a status-code assertion alone cannot catch this).
func renderTemplate(t *testing.T, tmpl *template.Template, data any) string {
	t.Helper()
	var buf bytes.Buffer
	page := webPage{ObservedAt: time.Now(), Data: data}
	if err := tmpl.ExecuteTemplate(&buf, "layout", page); err != nil {
		t.Fatalf("template execution failed: %v", err)
	}
	return buf.String()
}

// TestOverviewTemplateRendersEveryOptionalField exercises every branch
// overview.html takes when the controller has a durable generation, a
// reachable live endpoint, findings, and the fleet has a plan - the fields
// most likely to be reshaped silently by a DTO change (#397 review
// 5397796709 is exactly that: a DTO field renamed to flatten a struct while
// the template still addressed the old shape).
func TestOverviewTemplateRendersEveryOptionalField(t *testing.T) {
	data := overviewData{
		ObservedAt: time.Now(),
		Controller: Controller{
			DurableConsistency: rt.DurableConsistent, Serving: rt.Serving, Projection: rt.ProjectionCurrent,
			GenerationMatch: "match", Role: rt.RoleHeld, WorkAdmission: rt.AdmissionOpen,
			DurableGeneration: &Build{Kind: "release", Version: "v1", SourceRevision: "abc"},
			LiveReachable:     true,
			LiveBuild:         &Build{Kind: "release", Version: "v1", SourceRevision: "abc"},
			Findings:          []string{"a controller that is not the durably active generation is admitting work"},
		},
		Fleet: Fleet{
			Capacity: 2, Executing: 1, Active: 1, SupervisorRunning: true,
			Counts: FleetCounts{Active: 1},
			Plans:  []Plan{{ID: "p", Revision: 1, ApprovedRevision: 1, State: "executing", Stages: map[string]int{"running": 1}, Unavailable: true}},
		},
	}
	out := renderTemplate(t, overviewTemplate, data)
	for _, want := range []string{"overview", "release", "abc", "durably active generation", "could not be replayed"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("expected %q in rendered overview, got: %s", want, out)
		}
	}
}

// TestOverviewTemplateRendersControllerUnreachable exercises the "not
// reachable" / "unknown" branches, the complement of the fully-populated
// case above.
func TestOverviewTemplateRendersControllerUnreachable(t *testing.T) {
	data := overviewData{ObservedAt: time.Now(), Controller: Controller{GenerationMatch: "unknown"}, Fleet: Fleet{}}
	renderTemplate(t, overviewTemplate, data)
}

// TestRunsTemplateRendersEveryColumn exercises every runs.html column,
// including the Unavailable banner row.
func TestRunsTemplateRendersEveryColumn(t *testing.T) {
	data := runsData{
		ObservedAt: time.Now(),
		Fleet:      Fleet{Capacity: 2, Executing: 1, Active: 1},
		Runs: []Run{{
			ID: "r", Repository: "example/repo", Issue: 7, Agent: "claude", ProviderKind: "anthropic", Model: "sonnet",
			Phase: rt.Execute, Disposition: rt.Active, Operation: "provider", Attempt: 1, Executing: true,
			Elapsed: time.Minute, Branch: "zenchron/run-r", CandidateRevision: "deadbeef",
			Attempts: map[string]int{"provider": 1}, Held: true,
			PullRequest: 9, PRState: "open", CI: "passing", Review: "approved",
		}, {ID: "unavailable-run", Unavailable: true}},
		Filter: runsFilter{},
	}
	out := renderTemplate(t, runsTemplate, data)
	for _, want := range []string{"example/repo", "claude", "anthropic", "sonnet", "deadbeef", "approved", "could not be replayed"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("expected %q in rendered runs list, got: %s", want, out)
		}
	}
}

// TestRunDetailTemplateRendersEveryOptionalField fully populates every
// pointer member of RunDetail, so every {{with}}/{{if}} branch the template
// takes on a complete run is exercised, not just the sparse fixture run
// api_test.go's own fixture produces.
func TestRunDetailTemplateRendersEveryOptionalField(t *testing.T) {
	now := time.Now()
	deadline := now.Add(time.Hour)
	status := RunDetail{
		ID: "r", Repository: "example/repo", Phase: rt.Execute, Disposition: rt.Active,
		Elapsed: time.Hour, ActiveElapsed: 30 * time.Minute, ExternalWaitElapsed: 30 * time.Minute,
		Source:     &Source{Repository: "example/repo", Issue: 7, URL: "https://example.test/issues/7", IntentChanged: true},
		Worker:     &Worker{Agent: "claude", ProviderKind: "anthropic", Model: "sonnet", TrustMode: rt.TrustOperatorTrusted, WorkspaceBound: true},
		Candidate:  &Candidate{Branch: "zenchron/run-r", Revision: "deadbeef", Tree: "treehash"},
		Base:       &Ref{ID: "main", Revision: "basecommit"},
		Contract:   &Ref{ID: "contract", Revision: "1"},
		Controller: &ControllerRef{BuildKind: "release", BuildVersion: "v1", Changed: true},
		Operation: &Operation{
			Kind: "provider", State: rt.Running, Attempt: 1, MaxAttempts: 2, ProgressSource: "row",
			StartedAt: &now, HeartbeatAt: &now, LastProgressAt: &now, SilentFor: time.Second,
			InactivityLimit: time.Minute, InactivitySuspension: "active", Deadline: &deadline, DeadlineBound: rt.BoundAttemptWall,
		},
		PullRequest: &PullRequest{Number: 9, State: "open", Merged: false, Stale: true},
		Attempts:    map[string]int{"provider": 1},
		Budgets: &Budgets{
			WallLimit: time.Hour, LifecycleDeadline: 2 * time.Hour, MaxExecutionAttempts: 3,
			MaxExecutionContinuations: 2, MaxRemediationAttempts: 1, MaxAssuranceAttempts: 1,
		},
		HeldMaterial: &HeldMaterial{Kind: rt.HeldUncommitted, Revision: "deadbeef", BlockedBy: "run_wall_budget_exhausted", NextStep: "assurance", Successor: "retry", SuccessorUnavailable: "execution_attempts_exhausted"},
		Assurance:    &Assurance{Passed: false, FailureClass: rt.FailureCompileTest, Stale: true},
		PublicationAuthority: &PublicationAuthority{
			Status: domain.AuthorityAwaitingAuthority, ActionType: "pull_request", ActionTarget: "open",
		},
		AuthorityRequest:         &AuthorityRequest{Status: domain.AuthorityIncomplete, Requires: []string{"human_approval"}, Missing: []string{"human_approval"}, Stale: []string{"ci"}},
		ExecutionDiagnostic:      &ExecutionDiagnostic{Stage: "provider_request", FailureClass: rt.FailureCompileTest, Bound: rt.BoundAttemptWall, Successor: "retry", SuccessorUnavailable: "execution_attempts_exhausted"},
		CandidateDiscardRefusals: 1, CandidateDiscardRefused: "git reset --hard",
	}
	data := runDetailData{
		ObservedAt: now, Status: status, After: 0, NextAfter: 1, HasMore: true,
		Events: []Event{{Sequence: 1, Type: "run.created", OccurredAt: now}},
	}
	out := renderTemplate(t, runDetailTemplate, data)
	for _, want := range []string{
		"example/repo", "claude", "anthropic", "sonnet", "operator_trusted", "zenchron/run-r", "deadbeef", "treehash",
		"main", "basecommit", "contract", "release", "different controller", "open", "stale",
		"provider", "run_wall_budget_exhausted", "assurance", "retry", "execution_attempts_exhausted",
		"compile_or_test", "awaiting_authority", "human_approval", "provider_request", "attempt_wall",
		"git reset --hard", "run.created",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("expected %q in rendered run detail, got: %s", want, out)
		}
	}
}

// TestRunDetailTemplateRendersWithoutOptionalFields exercises the "none" /
// "unknown" / "not observed" branches, the complement of the fully-populated
// case above - this is close to what api_test.go's sparse fixture produces.
func TestRunDetailTemplateRendersWithoutOptionalFields(t *testing.T) {
	data := runDetailData{ObservedAt: time.Now(), Status: RunDetail{ID: "r", Phase: rt.Execute, Disposition: rt.Active}}
	renderTemplate(t, runDetailTemplate, data)
}

// TestPagesDeclareLiveRegionsAndNeverRenderLive pins the contract console.js
// refreshes against (#420): every data page carries the [data-live] region
// ids the script swaps, and the server render itself only ever claims a
// timestamped snapshot - "live" is earned by a successful client refresh,
// never printed by the template.
func TestPagesDeclareLiveRegionsAndNeverRenderLive(t *testing.T) {
	pages := []struct {
		name string
		tmpl *template.Template
		data any
		ids  []string
	}{
		{"overview", overviewTemplate, overviewData{}, []string{"overview-live"}},
		{"runs", runsTemplate, runsData{}, []string{"runs-meta", "runs-table"}},
		{"run detail", runDetailTemplate, runDetailData{Status: RunDetail{ID: "r"}}, []string{"run-live"}},
	}
	for _, p := range pages {
		out := renderTemplate(t, p.tmpl, p.data)
		for _, id := range p.ids {
			if !strings.Contains(out, `id="`+id+`" data-live`) {
				t.Errorf("%s: missing live region %q", p.name, id)
			}
		}
		if !strings.Contains(out, `>snapshot</span>`) || strings.Contains(out, `>live</span>`) {
			t.Errorf("%s: server render must claim a snapshot, not live", p.name)
		}
	}
}
