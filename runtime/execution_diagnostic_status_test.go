package runtime

// Observability regression for the second #32 dogfood
// (run-277c3aacc89c72385f569f1f00dca40a).
//
// The repair from 1bf30b32 already PERSISTED a sanitized diagnostic. What was
// missing is that normal operator output never showed it: `autonomy status
// --text` said only execution.invoke_failure_not_retryable, and an operator had
// to know the sanitized artifact path and open it by hand to discover the
// stage, the HTTP status, and that tools[0].name was the rejected field.
//
// These tests drive the REAL provider against the REAL observed 400, then close
// the store, reopen it, and rebuild the status projection from durable state
// alone - no process-local error value, no manual runtime.db inspection.

import (
	"encoding/json"
	"strings"
	"testing"
)

// observedToolNameRejection is the exact response body the real run received.
// The message is deliberately part of the fixture: what must NOT happen is it
// reaching a durable row or an operator-visible field.
const observedToolNameRejection = `{"error":{"message":"Invalid 'tools[0].name': string does not match pattern. Expected ^[a-zA-Z0-9_-]+$.","type":"invalid_request_error","param":"tools[0].name","code":"invalid_value"}}`

// TestExecutionDiagnosticSurvivesRestartAndReachesStatus is the observability
// proof. The diagnostic must be reachable from Status() after a restart, which
// means it is projected from the journal and not carried in memory.
func TestExecutionDiagnosticSurvivesRestartAndReachesStatus(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := &scriptedTransport{status: 400, body: observedToolNameRejection}
	useRealProvider(t, fixture, runID, api, nil)
	fixture.reconcile(runID)
	if api.calls == 0 {
		t.Fatal("execution never reached the transport, so no HTTP fact was produced")
	}

	// Restart: close the store, reopen it, and build a runtime whose only
	// knowledge of the failure is the durable journal.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	deps := fixture.deps
	deps.Store = reopened
	restarted := fixture.newRuntime(deps)

	report, err := restarted.Status(runID)
	if err != nil {
		t.Fatalf("status after restart failed: %v", err)
	}
	diagnostic := report.ExecutionDiagnostic
	if diagnostic == nil {
		t.Fatal("status exposes no execution diagnostic: the operator is back to opening artifacts by hand")
	}
	if diagnostic.Stage != execStageProviderLoop {
		t.Fatalf("stage = %q, want %q", diagnostic.Stage, execStageProviderLoop)
	}
	if diagnostic.Code != string(StopProviderError) {
		t.Fatalf("code = %q, want the bounded loop's own stop reason", diagnostic.Code)
	}
	if diagnostic.HTTPStatus != 400 {
		t.Fatalf("http status = %d, want the observed 400", diagnostic.HTTPStatus)
	}
	if diagnostic.ProviderErrorCode != "invalid_value" {
		t.Fatalf("provider error code = %q, want invalid_value", diagnostic.ProviderErrorCode)
	}
	if diagnostic.ProviderErrorParam != "tools[0].name" {
		t.Fatalf("provider error param = %q, want tools[0].name: this is the field the operator had to open the artifact to find", diagnostic.ProviderErrorParam)
	}
	if diagnostic.FailureClass == "" || diagnostic.Route == "" {
		t.Fatalf("the failure class and route are not surfaced: %#v", diagnostic)
	}
	if diagnostic.Model != "gpt-fixture" {
		t.Fatalf("model = %q, want the model the exchange used", diagnostic.Model)
	}
	if diagnostic.ProviderKind == "" {
		t.Fatal("the provider kind is not surfaced")
	}
	if diagnostic.ArtifactRef == "" {
		t.Fatal("the sanitized diagnostic artifact is not referenced")
	}

	// The bounded fields are identity only. The provider's free-form message
	// and the raw body never become an operator-visible field.
	rendered, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"does not match pattern", "Invalid 'tools[0].name'", fixtureAPIKey, "Bearer "} {
		if strings.Contains(string(rendered), forbidden) {
			t.Fatalf("the status report leaked unbounded provider material %q: %s", forbidden, rendered)
		}
	}
	assertNoSecretsInJournal(t, journalOf(t, restarted, runID))
}

// TestExecutionDiagnosticIsProjectedFromTheJournalAlone proves the projection
// itself is the mechanism, independently of any runtime wiring: folding the
// persisted events reconstructs the same diagnostic.
func TestExecutionDiagnosticIsProjectedFromTheJournalAlone(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := &scriptedTransport{status: 400, body: observedToolNameRejection}
	useRealProvider(t, fixture, runID, api, nil)
	fixture.reconcile(runID)

	events := journalOf(t, fixture.runtime, runID)
	projection, err := Project(events)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if projection.ExecutionDiagnostic == nil {
		t.Fatalf("the journal alone does not reconstruct the diagnostic: %v", journalTypes(events))
	}
	if projection.ExecutionDiagnostic.HTTPStatus != 400 || projection.ExecutionDiagnostic.ProviderErrorParam != "tools[0].name" {
		t.Fatalf("the projected diagnostic lost the observed facts: %#v", projection.ExecutionDiagnostic)
	}
	// A run with no execution failure carries no diagnostic: the field is an
	// answer, never a decoration.
	empty, err := Project(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ExecutionDiagnostic != nil {
		t.Fatalf("an empty journal projected a diagnostic: %#v", empty.ExecutionDiagnostic)
	}
}
