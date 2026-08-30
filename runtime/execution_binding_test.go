package runtime

// Regression proof for issue #29 defects A and C, from dogfood run
// run-e3480a4dbef024e9a0add814f6dd2379.
//
// Defect A: invokeExecution bound ExecutionRequest.Candidate from the
// PROJECTION. On an initial implementation no candidate.committed event exists,
// so revision and tree were both empty and every real provider refused the
// request at validate ("incomplete execution request binding") before any
// provider communication happened at all.
//
// Defect C: the resulting operation recorded only
// {"failure_class":"unknown","mutated":false,"path_count":0}. The error was
// discarded, and because the provider returned before StoreTranscript there was
// no artifact either, so the run was not diagnosable after a restart.
//
// Everything here runs against the REAL OpenAIProvider validation contract with
// an injected transport. The permissive FakeExecutionProvider is deliberately
// not used: accepting an incomplete binding is exactly why the defect shipped.
// No test makes a network call, so the file passes under `docker run
// --network none`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptedTransport is the injected Doer. status 0 explodes before any exchange
// happens, which is how "the transport was REACHED" is proven: an incompletely
// bound request never gets this far.
type scriptedTransport struct {
	calls  int
	status int
	body   string
}

func (t *scriptedTransport) Do(request *http.Request) (*http.Response, error) {
	t.calls++
	if t.status == 0 {
		return nil, fmt.Errorf("exploding transport: this test performs no network call")
	}
	return &http.Response{
		StatusCode: t.status,
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Header:     http.Header{},
	}, nil
}

// bindingProvider records what the runtime handed to a REAL OpenAIProvider and
// then lets that provider validate it for real. It adds no permissiveness: the
// acceptance or refusal is entirely the real provider's.
type bindingProvider struct {
	inner    OpenAIProvider
	requests []ExecutionRequest
}

func (p *bindingProvider) Isolation() ProviderIsolation { return p.inner.Isolation() }

func (p *bindingProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	return p.inner.Execute(ctx, request)
}

// useRealProvider rebuilds the fixture's runtime around a real OpenAIProvider
// bound to the run's own candidate workspace, with an operator key file that
// lives outside that workspace.
func useRealProvider(t *testing.T, fixture *phase8Fixture, runID string, api *scriptedTransport, configure func(*OpenAIProvider)) *bindingProvider {
	t.Helper()
	control := filepath.Join(fixture.root, "control")
	if err := os.MkdirAll(control, 0700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(control, "openai.key")
	if err := os.WriteFile(keyFile, []byte(fixtureAPIKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	candidate := candidateDir(fixture.stateDir, runID)
	inner := OpenAIProvider{
		ArtifactStore: ArtifactStore{Root: filepath.Join(control, "artifacts")},
		Model:         "gpt-fixture",
		AuthMode:      "operator_key_file",
		APIKeyFile:    keyFile,
		Endpoint:      "https://api.invalid/v1/responses",
		HTTP:          api,
		Broker: ToolBroker{CandidateDir: candidate, Sandbox: DockerSandbox{
			Image: "sha256:image", Executor: &fakeCommandExecutor{found: true},
			OperationID: "binding-operation", StateDir: filepath.Join(control, "sandbox"),
		}},
		MaxIterations: 1,
	}
	if configure != nil {
		configure(&inner)
	}
	provider := &bindingProvider{inner: inner}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return provider
}

// executionDiagnostics reads back every execution.invoke operation result the
// journal holds, strictly, exactly as a restarted process would.
func executionDiagnostics(t *testing.T, events []EngineeringEvent) []executionRecord {
	t.Helper()
	var out []executionRecord
	for _, e := range events {
		if e.Type != EventOperationAfter {
			continue
		}
		operation, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			t.Fatalf("decode persisted operation.after: %v", err)
		}
		if operation.Kind != OpExecutionInvoke || len(operation.Result) == 0 {
			continue
		}
		var record executionRecord
		if err := json.Unmarshal(operation.Result, &record); err != nil {
			t.Fatalf("decode persisted execution result %s: %v", operation.Result, err)
		}
		out = append(out, record)
	}
	return out
}

// ---------------------------------------------------------------------------
// Defect A: initial execution subject binding
// ---------------------------------------------------------------------------

// TestInitialExecutionBindsTheObservedWorkspaceSubject is the defect-A proof.
// The initial request carries the EXACT observed workspace head and tree, that
// subject is the trusted base, the real provider's validate ACCEPTS it, the
// transport is reached, and no candidate.committed event is fabricated to get
// there.
func TestInitialExecutionBindsTheObservedWorkspaceSubject(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := &scriptedTransport{}
	provider := useRealProvider(t, fixture, runID, api, nil)
	fixture.reconcile(runID)

	if len(provider.requests) == 0 {
		t.Fatal("the provider was never invoked")
	}
	request := provider.requests[0]
	if request.Purpose != InvocationInitial {
		t.Fatalf("first invocation purpose = %q", request.Purpose)
	}

	// The binding is exact and complete.
	if request.Candidate.Revision == "" || request.Candidate.Tree == "" {
		t.Fatalf("the initial execution request carries an incomplete subject binding: %#v", request.Candidate)
	}
	baseTree := mustGit(t, fixture.origin, "rev-parse", fixture.base+"^{tree}")
	if request.Candidate.Revision != fixture.base || request.Candidate.Tree != baseTree {
		t.Fatalf("the pristine initial subject %#v is not the trusted base %s/%s", request.Candidate, fixture.base, baseTree)
	}
	workspace := candidateDir(fixture.stateDir, runID)
	if got := mustGit(t, workspace, "rev-parse", "HEAD"); got != request.Candidate.Revision {
		t.Fatalf("the bound subject %s is not the workspace head %s", request.Candidate.Revision, got)
	}

	// The REAL provider validation contract accepts it. This is the check the
	// failed run tripped, run against the same request the runtime built.
	if err := provider.inner.validate(request); err != nil {
		t.Fatalf("real provider validation refused the request: %v", err)
	}
	// And it was reached in the run itself, not only in this assertion: an
	// exploding transport can only be reached past validate.
	if api.calls == 0 {
		t.Fatal("no HTTP exchange was attempted: execution never got past provider validation")
	}

	// The workspace execution subject is NOT a runtime-owned candidate commit.
	events := journalOf(t, fixture.runtime, runID)
	for _, e := range events {
		if e.Type == EventCandidateCommitted {
			t.Fatal("an initial invocation fabricated a candidate.committed event")
		}
	}
	state := fixture.state(runID)
	if state.projection.CandidateRevision != "" || state.projection.CandidateTree != "" {
		t.Fatalf("the projection adopted the workspace subject as a produced candidate: %#v", state.projection)
	}
}

// TestExecutionSubjectMustBeTheSubjectTheInvocationClaims is the invariant
// proof: a remediation observing a head that disagrees with the projected
// candidate is refused as a workspace_integrity_violation, never proceeded with
// silently. The observation is a real one, read through the runtime Git
// boundary, not a synthesized string.
func TestExecutionSubjectMustBeTheSubjectTheInvocationClaims(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)

	workspace := &CandidateWorkspace{Dir: candidateDir(fixture.stateDir, runID), BaseRevision: fixture.base}
	observed, err := workspace.head()
	if err != nil {
		t.Fatal(err)
	}
	if observed.Commit == "" || observed.Tree == "" {
		t.Fatalf("the governed Git boundary observed no subject: %#v", observed)
	}
	other := strings.Repeat("a", 40)

	for name, tc := range map[string]struct {
		purpose    InvocationPurpose
		projection RunProjection
		base       string
		refused    bool
	}{
		"remediation at the projected candidate": {
			purpose:    InvocationRemediation,
			projection: RunProjection{CandidateRevision: observed.Commit, CandidateTree: observed.Tree},
		},
		"remediation against a different head": {
			purpose:    InvocationRemediation,
			projection: RunProjection{CandidateRevision: other, CandidateTree: observed.Tree},
			refused:    true,
		},
		"remediation against a different tree": {
			purpose:    InvocationRemediation,
			projection: RunProjection{CandidateRevision: observed.Commit, CandidateTree: other},
			refused:    true,
		},
		"initial on a pristine workspace": {
			purpose: InvocationInitial,
			base:    observed.Commit,
		},
		"initial on a workspace that left the trusted base": {
			purpose: InvocationInitial,
			base:    other,
			refused: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			bound := &CandidateWorkspace{Dir: workspace.Dir, BaseRevision: tc.base}
			err := assertExecutionSubject(&runState{projection: tc.projection}, bound, tc.purpose, observed)
			if tc.refused {
				var violation *WorkspaceIntegrityError
				if !errors.As(err, &violation) {
					t.Fatalf("a disagreeing subject was accepted: %v", err)
				}
				if RouteFailure(FailureWorkspaceIntegrity) != RouteRestore {
					t.Fatalf("workspace integrity no longer routes to refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("the claimed subject was refused: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Defect C: durable sanitized diagnostics
// ---------------------------------------------------------------------------

// TestEarlyProviderRefusalIsDiagnosableAfterRestart is the defect-C proof. It
// reproduces the same shape of failure the dogfood run hit - a real provider
// refusing at validate, before any transport and therefore before any
// transcript artifact - then CLOSES the store, reopens it, and reconstructs the
// root cause from durable state alone.
func TestEarlyProviderRefusalIsDiagnosableAfterRestart(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := &scriptedTransport{}
	// A real validate-stage refusal: complete request binding, misconfigured
	// provider. Nothing reaches the transport.
	useRealProvider(t, fixture, runID, api, func(p *OpenAIProvider) { p.Model = "" })
	fixture.reconcile(runID)
	if api.calls != 0 {
		t.Fatalf("a validate-stage refusal reached the transport %d times", api.calls)
	}

	// Restart: the process-local error values are gone. Only the store remains.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	events, err := reopened.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(EngineeringRun{ID: runID}, events)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Disposition != Failed {
		t.Fatalf("durable disposition = %q (%q), want a failed run", snapshot.Disposition, snapshot.Reason)
	}

	records := executionDiagnostics(t, events)
	if len(records) == 0 {
		t.Fatalf("no durable execution.invoke result survived the restart: %v", journalTypes(events))
	}
	record := records[0]

	// The retry-routing contract is unchanged.
	if record.FailureClass != FailureUnknown || record.Mutated || record.PathCount != 0 {
		t.Fatalf("the mutation facts changed shape: %#v", record.mutationResult)
	}
	// And the root cause is now nameable without source archaeology.
	diagnostic := record.Diagnostic
	if diagnostic == nil {
		t.Fatal("the execution error was discarded again: the durable result carries no diagnostic")
	}
	if diagnostic.Stage != execStageProviderRequest {
		t.Fatalf("diagnostic stage = %q, want the pre-transport request stage", diagnostic.Stage)
	}
	if !strings.Contains(diagnostic.Message, "provider model required") {
		t.Fatalf("the sanitized message does not identify the refusal: %q", diagnostic.Message)
	}
	if diagnostic.ProviderKind != fmt.Sprintf("%T", fixture.deps.Provider) {
		t.Fatalf("diagnostic provider kind = %q, want the configured provider's type", diagnostic.ProviderKind)
	}
	if diagnostic.Route != RouteFailure(FailureUnknown) {
		t.Fatalf("diagnostic route = %q, want the route the failure class carries", diagnostic.Route)
	}
	// No provider interaction happened, so no transcript is required and no
	// HTTP fact may be claimed.
	if diagnostic.ArtifactRef != "" || diagnostic.HTTPStatus != 0 || diagnostic.ProviderErrorCode != "" {
		t.Fatalf("a pre-transport refusal claimed provider interaction: %#v", diagnostic)
	}
	assertNoSecretsInJournal(t, events)
}

// TestHTTPExchangeFailuresRecordSafeProviderFacts proves the other half of the
// diagnostic contract: when an exchange DOES happen, the status and the
// provider's own error code become durable, the transcript artifact is
// referenced, and the response body stays out of the journal.
func TestHTTPExchangeFailuresRecordSafeProviderFacts(t *testing.T) {
	const marker = "SECRET-BODY-MARKER-9c3"

	for name, tc := range map[string]struct {
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		"rate limited": {
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"slow down ` + marker + `","type":"rate_limit","code":"rate_limit_exceeded"}}`,
			// A non-200 is refused before the envelope is decoded, so the
			// status is the only safe fact there is.
			wantStatus: http.StatusTooManyRequests,
		},
		"provider error envelope": {
			status:     http.StatusOK,
			body:       `{"id":"resp_1","model":"gpt-fixture","error":{"message":"` + marker + `","type":"server_error","code":"internal_failure"}}`,
			wantStatus: http.StatusOK,
			wantCode:   "internal_failure",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPhase8Fixture(t)
			runID := fixture.start()
			api := &scriptedTransport{status: tc.status, body: tc.body}
			useRealProvider(t, fixture, runID, api, nil)
			fixture.reconcile(runID)
			if api.calls == 0 {
				t.Fatal("execution never got past provider validation")
			}

			events := journalOf(t, fixture.runtime, runID)
			records := executionDiagnostics(t, events)
			if len(records) == 0 {
				t.Fatalf("no durable execution result: %v", journalTypes(events))
			}
			diagnostic := records[0].Diagnostic
			if diagnostic == nil {
				t.Fatal("the durable result carries no diagnostic")
			}
			if diagnostic.Stage != execStageProviderLoop || diagnostic.Code != string(StopProviderError) {
				t.Fatalf("diagnostic stage/code = %q/%q, want the bounded loop's own stop reason", diagnostic.Stage, diagnostic.Code)
			}
			if diagnostic.HTTPStatus != tc.wantStatus || diagnostic.ProviderErrorCode != tc.wantCode {
				t.Fatalf("http status/code = %d/%q, want %d/%q", diagnostic.HTTPStatus, diagnostic.ProviderErrorCode, tc.wantStatus, tc.wantCode)
			}
			if diagnostic.ArtifactRef == "" {
				t.Fatal("an HTTP exchange produced no referenced transcript artifact")
			}
			if diagnostic.Model != "gpt-fixture" {
				t.Fatalf("diagnostic model = %q, want the model the exchange used", diagnostic.Model)
			}
			if strings.Contains(diagnostic.Message, marker) {
				t.Fatalf("the provider response body reached the durable diagnostic: %q", diagnostic.Message)
			}
			if journalMentions(events, marker) {
				t.Fatal("the provider response body reached a durable event row")
			}
			assertNoSecretsInJournal(t, events)
		})
	}
}

// TestSanitizedDetailRedactsAndBounds is the field-level guarantee every
// durable diagnostic string goes through.
func TestSanitizedDetailRedactsAndBounds(t *testing.T) {
	got := sanitizedDetail("failed for Authorization: Bearer sk-live-should-never-persist and ghp_abcdefghijklmnop")
	for _, secret := range []string{"sk-live-should-never-persist", "ghp_abcdefghijklmnop", "Bearer"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized detail still carries %q: %q", secret, got)
		}
	}
	if len(sanitizedDetail(strings.Repeat("x", 100_000))) > maxPayloadFieldBytes {
		t.Fatal("sanitized detail is unbounded")
	}
}

// assertNoSecretsInJournal is the leak check every diagnostic test shares.
func assertNoSecretsInJournal(t *testing.T, events []EngineeringEvent) {
	t.Helper()
	for _, secret := range []string{fixtureAPIKey, "Authorization", "Bearer "} {
		if journalMentions(events, secret) {
			t.Fatalf("a durable event row carries %q", secret)
		}
	}
}
