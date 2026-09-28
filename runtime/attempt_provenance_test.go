package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// #327: every provider-executed run attempt durably records the invocation
// provenance that governed and explained it. These tests pin the journal event,
// its attempt binding, its survival across a restart, its explicit absence, and
// its size against the canonical payload ceiling.

// provenanceStep is one scripted physical attempt.
type provenanceStep struct {
	invocation *InvocationProvenance
	failure    *ProviderFailure
	err        error
	mutate     bool
}

// provenanceProvider returns whatever provenance a step scripts, exactly as the
// CLI adapter hands its InvocationProvenance to the runtime.
type provenanceProvider struct {
	steps []provenanceStep
	calls int
}

func (p *provenanceProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *provenanceProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	step := p.steps[min(p.calls, len(p.steps)-1)]
	p.calls++
	if step.mutate {
		body := fmt.Sprintf("package candidate\n\n// attempt %d\n", request.Attempt)
		if err := os.WriteFile(filepath.Join(request.CandidateDir, "candidate.go"), []byte(body), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	result := ExecutionResult{ProviderID: "claude", Attempt: request.Attempt, Outcome: Succeeded, Failure: step.failure}
	if step.failure != nil || step.err != nil {
		result.Outcome = OperationFailed
	}
	if step.invocation != nil {
		invocation := *step.invocation
		result.Invocation = &invocation
	}
	return result, step.err
}

// deadlineProvenance is the #324 attempt's shape: killed at its wall deadline
// under the structured Claude oracle, with a tool still open and NO final
// result - so its permission denials are unknown, and absent.
func deadlineProvenance() *InvocationProvenance {
	started := time.Date(2026, 9, 26, 17, 50, 8, 0, time.UTC)
	deadline := started.Add(30 * time.Minute)
	return &InvocationProvenance{
		AgentID: "claude", ProviderKind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted, Model: "sonnet",
		InvocationObservation: domain.InvocationObservation{
			Executable: "claude", Version: "2.1.0 (Claude Code)",
			PermissionMode: "acceptEdits", AuthMode: AuthModeLocalCLISession, AuthModeSource: AuthSourceProviderState,
			WorkspaceInstructionsSuppressed: true,
			Argv:                            []string{"--print", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits", "[prompt]"},
			PromptSHA256:                    strings.Repeat("ab", 32),
			Deadline:                        &deadline, StartedAt: &started, CompletedAt: &deadline,
			Elapsed: 30 * time.Minute, OverranDeadline: false,
			TerminationCause: "deadline_reached",
			InactivityLimit:  10 * time.Minute,
			ProgressMode:     progressStructuredClaudeEvents,
			StructuredEvents: 202,
			OpenToolsAtExit:  1,
			ProcessID:        48213,
			GitGuarded:       true,
		},
		GitRefusals: []GitRefusal{{Operation: "git worktree --porcelain -z <1 operand(s)>", Reason: "refused", Origin: "provider_runtime", DirtyCount: 4}},
	}
}

// successProvenance is an attempt that returned with a final result, which is
// what makes its denial count and denied tools KNOWN.
func successProvenance() *InvocationProvenance {
	invocation := deadlineProvenance()
	invocation.TerminationCause, invocation.OpenToolsAtExit = "provider_returned", 0
	invocation.FinalResultObserved = true
	invocation.PermissionDenials, invocation.PermissionDeniedTools = 6, []string{"Bash", "WebFetch"}
	return invocation
}

func provenanceEvents(t *testing.T, rt *EngineeringRuntime, runID string) ([]EngineeringEvent, []ExecutionAttemptProvenance) {
	t.Helper()
	events, err := rt.Journal(runID)
	if err != nil {
		t.Fatal(err)
	}
	var matched []EngineeringEvent
	var payloads []ExecutionAttemptProvenance
	for _, event := range events {
		if event.Type != EventExecutionAttemptProvenance {
			continue
		}
		payload, err := decodePayload[ExecutionAttemptProvenance](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		matched, payloads = append(matched, event), append(payloads, payload)
	}
	return matched, payloads
}

// assertPinned checks every field #327 pins, against what the provider
// reported, minus the refusals that are counted on the operation result.
func assertPinned(t *testing.T, got ExecutionAttemptProvenance, want *InvocationProvenance) {
	t.Helper()
	expected := *want
	expected.GitRefusals = nil
	// deadline_bound is the RUNTIME's decision at attempt start (#328), not
	// the adapter's, so it is added here; attempt_wall_budget_test pins it.
	if got.Invocation.DeadlineBound == "" {
		t.Fatal("journalled provenance names no deadline bound for a run with a frozen attempt limit")
	}
	expected.DeadlineBound = got.Invocation.DeadlineBound
	if !reflect.DeepEqual(got.Invocation, expected) {
		gotJSON, _ := json.Marshal(got.Invocation)
		wantJSON, _ := json.Marshal(expected)
		t.Fatalf("journalled provenance differs from the attempt's:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if got.Invocation.GitRefusals != nil {
		t.Fatal("the attempt event carried git refusals, a second source for what the operation result counts")
	}
}

func runWithProvenance(t *testing.T, steps ...provenanceStep) (*phase8Fixture, string) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	fixture.deps.Provider = &provenanceProvider{steps: steps}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)
	return fixture, runID
}

// Tests 1 and 2 of #327: a producer attempt that failed at its deadline, and
// one that succeeded, each journal every pinned field - bound to the operation
// and physical attempt that produced it, and before that operation settles.
func TestEveryProviderAttemptJournalsItsInvocationProvenance(t *testing.T) {
	for name, step := range map[string]provenanceStep{
		"deadline": {invocation: deadlineProvenance(), mutate: true, err: errors.New("exit status 143"),
			failure: &ProviderFailure{Classification: FailureUnknown}},
		"success": {invocation: successProvenance(), mutate: true},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, runID := runWithProvenance(t, step)
			events, payloads := provenanceEvents(t, fixture.runtime, runID)
			if len(payloads) == 0 {
				t.Fatal("a provider-executed attempt journalled no invocation provenance")
			}
			assertPinned(t, payloads[0], step.invocation)
			// UNKNOWN IS NOT ZERO: the deadline attempt had no final result, so
			// it records no denial count or tools at all; the success does.
			if inv := payloads[0].Invocation; inv.FinalResultObserved != (name == "success") ||
				(name == "deadline" && (inv.PermissionDenials != 0 || inv.PermissionDeniedTools != nil)) ||
				(name == "success" && inv.PermissionDenials != 6) {
				t.Fatalf("%s: final_result_observed=%v denials=%d tools=%v", name, inv.FinalResultObserved, inv.PermissionDenials, inv.PermissionDeniedTools)
			}
			if raw := string(events[0].Payload); name == "deadline" && strings.Contains(raw, "permission_denials") {
				t.Fatalf("an unknown denial count was persisted: %s", raw)
			}
			event := events[0]
			if payloads[0].OperationID != event.OperationID || payloads[0].AttemptIdentity != 1 {
				t.Fatalf("provenance is bound to %q attempt %d, the event to %q",
					payloads[0].OperationID, payloads[0].AttemptIdentity, event.OperationID)
			}
			operation, ok := operationByID(fixture.state(runID), event.OperationID)
			if !ok || operation.Kind != OpExecutionInvoke {
				t.Fatalf("provenance is bound to %q, not an execution operation", event.OperationID)
			}
			// ONE SOURCE: the operation result still carries only the counted
			// refusal fields, never a copy of the provenance.
			journal, err := fixture.runtime.Journal(runID)
			if err != nil {
				t.Fatal(err)
			}
			settled := false
			for _, later := range journal[event.Sequence:] {
				if later.Type == EventOperationAfter && later.OperationID == event.OperationID {
					settled = true
					if strings.Contains(string(later.Payload), "termination_cause") {
						t.Fatal("operation.after carries provenance as well: two sources for one fact")
					}
					break
				}
			}
			if !settled {
				t.Fatal("the provenance event is not followed by its operation's settlement")
			}
		})
	}
}

// Test 3 of #327: two physical attempts on one operation keep two records,
// keyed by attempt identity - never one overwritten or merged.
func TestTwoPhysicalAttemptsKeepTwoProvenanceRecords(t *testing.T) {
	first := deadlineProvenance()
	first.TerminationCause, first.StructuredEvents = "provider_inactivity_limit_reached", 11
	second := deadlineProvenance()
	second.TerminationCause, second.StructuredEvents, second.OpenToolsAtExit = "provider_returned", 93, 0
	fixture := newPhase8Fixture(t)
	fixture.deps.Provider = &provenanceProvider{steps: []provenanceStep{
		{invocation: first, err: &ProviderStopError{Reason: StopIterationBudget, Detail: "stopped"}},
		{invocation: second, mutate: true},
	}}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	for pass := 0; pass < 8; pass++ {
		fixture.reconcile(runID)
		if _, payloads := provenanceEvents(t, fixture.runtime, runID); len(payloads) >= 2 {
			break
		}
	}
	_, payloads := provenanceEvents(t, fixture.runtime, runID)
	if len(payloads) < 2 {
		t.Fatalf("expected two attempt records, got %d", len(payloads))
	}
	if payloads[0].OperationID != payloads[1].OperationID {
		t.Fatalf("the two attempts were not of one operation: %q %q", payloads[0].OperationID, payloads[1].OperationID)
	}
	if payloads[0].AttemptIdentity != 1 || payloads[1].AttemptIdentity != 2 {
		t.Fatalf("attempt identities = %d, %d; want 1, 2", payloads[0].AttemptIdentity, payloads[1].AttemptIdentity)
	}
	assertPinned(t, payloads[0], first)
	assertPinned(t, payloads[1], second)
}

// Test 4 of #327: after a restart, status and the events view return the
// provenance from the journal alone. The reopened runtime's artifact store
// fails every read, so nothing can be answered from a transcript.
func TestAttemptProvenanceSurvivesARestartWithoutTheArtifactStore(t *testing.T) {
	want := deadlineProvenance()
	fixture, runID := runWithProvenance(t, provenanceStep{invocation: want, mutate: true,
		err: errors.New("exit status 143"), failure: &ProviderFailure{Classification: FailureUnknown}})

	_, restarted := reopen(t, fixture)
	unreadable := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unreadable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deps := fixture.deps
	deps.Artifacts = ArtifactStore{Root: unreadable}
	restarted = fixture.newRuntime(deps)
	if _, err := os.ReadDir(unreadable); err == nil {
		t.Fatal("the artifact store root is readable; the restart would prove nothing")
	}

	status, err := restarted.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExecutionAttemptProvenance == nil {
		t.Fatal("status lost the attempt's provenance across a restart")
	}
	got := status.ExecutionAttemptProvenance.Invocation
	if got.TerminationCause != "deadline_reached" || got.ProgressMode != progressStructuredClaudeEvents ||
		got.InactivityLimit != 10*time.Minute || got.StructuredEvents != 202 || got.OpenToolsAtExit != 1 ||
		got.FinalResultObserved || got.PermissionDenials != 0 || got.PermissionDeniedTools != nil {
		t.Fatalf("status reports %+v", got)
	}
	if _, payloads := provenanceEvents(t, restarted, runID); len(payloads) == 0 {
		t.Fatal("the events view lost the attempt's provenance across a restart")
	} else {
		assertPinned(t, payloads[0], want)
	}
}

// Test 5 of #327: an attempt that never reached a provider records NO
// provenance, and status says none was recorded rather than showing zeros.
func TestAPreDispatchRefusalRecordsNoProvenance(t *testing.T) {
	fixture, runID := runWithProvenance(t, provenanceStep{err: errors.New("the capability probe failed")})
	if _, payloads := provenanceEvents(t, fixture.runtime, runID); len(payloads) != 0 {
		t.Fatalf("a refusal before dispatch journalled provenance: %+v", payloads)
	}
	status, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Attempts[OpExecutionInvoke] == 0 {
		t.Fatal("the refused attempt was not an attempt; the absence below would prove nothing")
	}
	if status.ExecutionAttemptProvenance != nil {
		t.Fatalf("status invented provenance for an attempt that reached no provider: %+v", status.ExecutionAttemptProvenance)
	}
}

// The projection is a view of the LATEST attempt: a later attempt that reached
// no provider must not keep showing the earlier attempt's record.
func TestALaterAttemptWithoutProvenanceClearsTheEarlierRecord(t *testing.T) {
	before := func(seq int64) EngineeringEvent {
		payload, err := marshalPayloadJSON(RunOperation{SchemaVersion: SchemaVersion, ID: "op-invoke", RunID: "run-1",
			Kind: OpExecutionInvoke, IdempotencyKey: "invoke-1", State: Running, Attempt: int(seq), MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		return EngineeringEvent{Type: EventOperationBefore, Sequence: seq, OperationID: "op-invoke", Payload: payload}
	}
	record, err := marshalPayloadJSON(newExecutionAttemptProvenance("op-invoke", 1, *deadlineProvenance()))
	if err != nil {
		t.Fatal(err)
	}
	provenance := EngineeringEvent{Type: EventExecutionAttemptProvenance, OperationID: "op-invoke", Payload: record}
	projection, err := Project([]EngineeringEvent{before(1), provenance})
	if err != nil {
		t.Fatal(err)
	}
	if projection.ExecutionAttemptProvenance == nil || projection.ExecutionAttemptProvenance.AttemptIdentity != 1 {
		t.Fatal("the attempt's provenance was not projected")
	}
	projection, err = Project([]EngineeringEvent{before(1), provenance, before(2)})
	if err != nil {
		t.Fatal(err)
	}
	if projection.ExecutionAttemptProvenance != nil {
		t.Fatal("a later attempt with no provenance kept showing the earlier attempt's record")
	}
}

// seq20OperationAfter is the #324 run's journal sequence 20 operation.after
// payload, verbatim except for the operator's home directory (renamed to a
// same-length path, so the measured size is the real one).
const seq20OperationAfter = `{"active_since": "2026-09-26T17:50:08.437533Z", "attempt": 1, "attempt_identity": 1, "created_at": "2026-09-26T17:50:08.426897Z", "deadline": "2026-09-26T18:20:08.437533Z", "id": "run-00a8aecb360c53896934e5b051b0ccc5:execution.invoke:execution.invoke#initial|1|b1202216a0cd9cc534ffcc17fe5dc4605f0bf352", "idempotency_key": "execution.invoke#initial|1|b1202216a0cd9cc534ffcc17fe5dc4605f0bf352", "input_state_sha256": "ec345275bc1471cc8719ef29fc012c2a1fcea4c606dc07d430fcb4224618c060", "kind": "execution.invoke", "last_progress_at": "2026-09-26T17:50:08.437533Z", "max_attempts": 2, "result": {"diagnostic": {"artifact_ref": "/Users/operator1/.zenchron/state/artifacts/provider/claude/run-00a8aecb360c53896934e5b051b0ccc5/run-00a8aecb360c53896934e5b051b0ccc5%3Aexecution.invoke%3Aexecution.invoke%23initial%7C1%7Cb1202216a0cd9cc534ffcc17fe5dc4605f0bf352/attempt-1.raw.log", "failure_class": "execution_incomplete", "message": "exit status 143", "model": "sonnet", "provider_kind": "runtime.CLIAgentProvider", "route": "retry", "stage": "provider_result"}, "discard_refusals": 8, "discard_refused": "git worktree --porcelain -z <1 operand(s)> [origin=provider_runtime] (4 dirty candidate path(s) preserved)", "failure_class": "execution_incomplete", "mutated": true, "path_count": 7, "provider_executed": true, "provider_id": "claude"}, "run_id": "run-00a8aecb360c53896934e5b051b0ccc5", "schema_version": "0.1", "started_at": "2026-09-26T17:50:08.437533Z", "state": "succeeded", "wall_budget": 1800000000000}`

// worstInvocation is THE LARGEST LEGAL SHAPE: every string at its field bound
// (long absolute paths, with separators that need escaping), the maximum argv
// cardinality at the element bound, eight maximal denied-tool identifiers,
// maximal counters and timestamps, and a refusal list that must not be
// carried. Maximal integers are the I-JSON ceiling the canonicalizer enforces.
func worstInvocation() (InvocationProvenance, []string, []string) {
	winPath := long(`C:\Users\operator\AppData\Local\zenchron\state\artifacts\provider\claude\`)
	stamp := time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC)
	argv := make([]string, 0, maxProvenanceArgs+1)
	for i := 0; i < maxProvenanceArgs; i++ {
		argv = append(argv, long(fmt.Sprintf("--add-dir=/Users/operator/.zenchron/state/scratch/run-%02d/", i)))
	}
	argv = append(argv, argvTruncated)
	tools := make([]string, domain.MaxPermissionDeniedTools)
	for i := range tools {
		tools[i] = fmt.Sprintf("mcp__%d__", i) + strings.Repeat("t", domain.MaxPermissionDeniedToolBytes-len(fmt.Sprintf("mcp__%d__", i)))
	}
	refusals := make([]GitRefusal, 64)
	for i := range refusals {
		refusals[i] = GitRefusal{Operation: long("git reset --hard "), Reason: long("refused "), DirtyPaths: []string{long("a/")}}
	}
	return InvocationProvenance{
		AgentID: long("agent-"), ProviderKind: long("kind-"), TrustMode: TrustMode(long("trust-")), Model: long("model-"),
		InvocationObservation: domain.InvocationObservation{
			Executable: winPath, Version: long("version "), SandboxMode: long("sandbox-"), PermissionMode: long("mode-"),
			PermissionBypass: true, AuthMode: long("auth-"), AuthModeSource: long("source-"),
			WorkspaceBound: true, WorkspaceInstructionsSuppressed: true,
			Argv: argv, PromptSHA256: strings.Repeat("f", 64),
			Deadline: &stamp, StartedAt: &stamp, CompletedAt: &stamp,
			Elapsed: maxJSONInt, OverranDeadline: true,
			TerminationCause: long("cause-"), InactivityLimit: maxJSONInt, ProgressMode: long("progress-"),
			StructuredEvents: maxJSONInt, OpenToolsAtExit: maxJSONInt, FinalResultObserved: true, PermissionDenials: maxJSONInt,
			PermissionDeniedTools: tools, ProtocolAnomalies: maxJSONInt, ProcessID: maxJSONInt, GitGuarded: true,
		},
		GitRefusals: refusals,
	}, argv, tools
}

const maxJSONInt = 1<<53 - 1

func long(prefix string) string {
	return prefix + strings.Repeat("x", maxPayloadFieldBytes-len(prefix))
}

// hostileInvocation defeats every per-field bound at once: each string is
// control characters, which canonical JSON escapes six-fold, so even an empty
// argv leaves the record far above the ceiling - and a duration past the
// I-JSON range makes it uncanonicalizable outright.
func hostileInvocation() InvocationProvenance {
	control := strings.Repeat("\x01", maxPayloadFieldBytes)
	invocation, _, _ := worstInvocation()
	invocation.AgentID, invocation.ProviderKind, invocation.TrustMode, invocation.Model = control, control, TrustMode(control), control
	o := &invocation.InvocationObservation
	o.Executable, o.Version, o.SandboxMode, o.PermissionMode, o.AuthMode, o.AuthModeSource = control, control, control, control, control, control
	o.TerminationCause, o.ProgressMode = "deadline_reached", control
	o.Argv = make([]string, maxProvenanceArgs)
	for i := range o.Argv {
		o.Argv[i] = control
	}
	o.Elapsed = math.MaxInt64
	return invocation
}

// Test 6 of #327: the sizing that chose the dedicated event, kept as a
// regression so a future provenance field cannot consume the headroom
// invisibly.
func TestAttemptProvenanceFitsTheCanonicalPayloadCeiling(t *testing.T) {
	// THE BASELINE. operation.after is unchanged by #327: the #324 payload
	// still validates, at its measured size.
	baseline, err := CanonicalJSON(json.RawMessage(seq20OperationAfter))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload(EngineeringEvent{Type: EventOperationAfter, Payload: json.RawMessage(seq20OperationAfter)}); err != nil {
		t.Fatalf("the #324 operation.after no longer validates: %v", err)
	}
	t.Logf("#324 sequence-20 operation.after: %d canonical bytes, %d headroom", len(baseline), maxCanonicalPayloadBytes-len(baseline))
	if len(baseline) != 1445 {
		t.Fatalf("the #324 baseline measured %d canonical bytes, want 1445", len(baseline))
	}

	// THE REAL SHAPE fits untouched: nothing is trimmed from an ordinary attempt.
	real := newExecutionAttemptProvenance(strings.Repeat("o", 140), 1, *deadlineProvenance())
	if !reflect.DeepEqual(real.Invocation.Argv, deadlineProvenance().Argv) {
		t.Fatalf("an ordinary argv was trimmed: %v", real.Invocation.Argv)
	}
	realBytes, err := CanonicalJSON(real)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("#324-shaped attempt provenance event: %d canonical bytes, %d headroom", len(realBytes), maxCanonicalPayloadBytes-len(realBytes))

	worst, argv, tools := worstInvocation()
	withoutRefusals := worst
	withoutRefusals.GitRefusals = nil
	untrimmed, err := CanonicalJSON(ExecutionAttemptProvenance{OperationID: long("op-"), AttemptIdentity: maxJSONInt, Invocation: withoutRefusals})
	if err != nil {
		t.Fatal(err)
	}
	record := newExecutionAttemptProvenance(long("op-"), maxJSONInt, worst)
	canonical, err := CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worst-case attempt provenance: %d canonical bytes before the argv tail is bounded, %d recorded (%d of %d argv elements), %d headroom",
		len(untrimmed), len(canonical), len(record.Invocation.Argv), len(argv), maxCanonicalPayloadBytes-len(canonical))
	if len(canonical) > maxCanonicalPayloadBytes {
		t.Fatalf("the worst-case event is %d canonical bytes, above the %d ceiling", len(canonical), maxCanonicalPayloadBytes)
	}
	payload, err := marshalPayloadJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload(EngineeringEvent{Type: EventExecutionAttemptProvenance, Payload: payload}); err != nil {
		t.Fatalf("the worst-case event would be refused at append, which drops the record: %v", err)
	}
	// Bounded, never silently: a trimmed argv says so, and keeps its head.
	got := record.Invocation.Argv
	if len(got) == 0 || got[len(got)-1] != argvTruncated || got[0] != argv[0] || len(got) >= len(argv) {
		t.Fatalf("the argv was not visibly bounded from its tail: %d elements, last %q", len(got), got[len(got)-1])
	}
	if record.Invocation.GitRefusals != nil || !reflect.DeepEqual(record.Invocation.PermissionDeniedTools, tools) {
		t.Fatal("bounding changed something other than the argv tail")
	}

	// THE HOSTILE SHAPE: six-fold escapes everywhere and an uncanonicalizable
	// duration. The record falls back to its fixed-size core, visibly, and is
	// still appendable.
	hostile := hostileInvocation()
	if _, err := CanonicalJSON(ExecutionAttemptProvenance{OperationID: "op", AttemptIdentity: 1, Invocation: hostile}); err == nil {
		t.Fatal("the hostile shape canonicalizes; it would not exercise the fallback")
	}
	fallback := newExecutionAttemptProvenance(long("op-"), maxJSONInt, hostile)
	if !fallback.Invocation.Truncated || fallback.Invocation.TerminationCause != "deadline_reached" || fallback.Invocation.Argv != nil {
		t.Fatalf("the hostile record did not fall back to its marked core: %+v", fallback.Invocation)
	}
	if !appendable(EventExecutionAttemptProvenance, fallback) {
		t.Fatal("the fallback record would be refused at append")
	}
	fallbackBytes, err := CanonicalJSON(fallback)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("hostile attempt provenance fallback: %d canonical bytes, %d headroom", len(fallbackBytes), maxCanonicalPayloadBytes-len(fallbackBytes))
	// Escapes alone, with every value canonicalizable, fall back the same way.
	escaped := hostileInvocation()
	escaped.Elapsed = time.Hour
	if record := newExecutionAttemptProvenance("op", 1, escaped); !record.Invocation.Truncated || !appendable(EventExecutionAttemptProvenance, record) {
		t.Fatalf("a six-fold-escaped record did not fall back to an appendable core: %+v", record.Invocation)
	}
}

// The fallback is not a unit-test artefact: a hostile record reaches the
// journal through the real reconcile path, before its operation settles.
func TestAHostileProvenanceRecordStillAppendsBeforeOperationAfter(t *testing.T) {
	hostile := hostileInvocation()
	fixture, runID := runWithProvenance(t, provenanceStep{invocation: &hostile, mutate: true})
	events, payloads := provenanceEvents(t, fixture.runtime, runID)
	if len(payloads) == 0 || !payloads[0].Invocation.Truncated {
		t.Fatalf("the hostile attempt left no marked record: %+v", payloads)
	}
	journal, err := fixture.runtime.Journal(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, later := range journal[events[0].Sequence:] {
		if later.Type == EventOperationAfter && later.OperationID == events[0].OperationID {
			return
		}
	}
	t.Fatal("the pass failed before the operation settled")
}

// Planning half of the fix: a planner that is REFUSED - here, killed at its
// deadline - has no revision, so its attempt record is where its invocation
// provenance lives. It must survive, fitted beside everything else the attempt
// records.
func TestAPlannerDeadlineRefusalKeepsItsInvocationProvenance(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	input.Reasoned = nil
	observed := deadlineProvenance().InvocationObservation
	input.Reasoning.Invocation = &observed
	if _, err := f.service.RecordPlanningRefusal(input, &PlannerRefusedError{AgentID: "codex", Detail: "the provider reported execution_incomplete"}); err != nil {
		t.Fatalf("the refusal could not be recorded: %v", err)
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].Reasoning == nil || view.Attempts[0].Reasoning.Invocation == nil {
		t.Fatalf("the refused planner's invocation provenance was lost: %#v", view.Attempts)
	}
	if got := *view.Attempts[0].Reasoning.Invocation; !reflect.DeepEqual(got, observed) {
		t.Fatalf("the refused planner recorded %+v, want %+v", got, observed)
	}
}

// A heavy refusal record - every stage, reason and reference slot used, with
// ordinary lengths the record fits by itself - still takes the worst and the
// hostile observation without becoming unappendable. (A refusal whose own
// lists are all at their element bounds already exceeds the ceiling without
// any provenance; that predates #327 and is not changed here.)
func TestAWorstCasePlannerRefusalStaysUnderTheCeiling(t *testing.T) {
	for name, invocation := range map[string]func() InvocationProvenance{
		"worst":   func() InvocationProvenance { inv, _, _ := worstInvocation(); return inv },
		"hostile": hostileInvocation,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAttemptFixture(t)
			input := refusedProposal(f)
			input.Reasoned = nil
			for i := 0; i < maxPayloadListItems+4; i++ {
				input.Reasoned = append(input.Reasoned, domain.PlanStage{
					ID: fmt.Sprintf("stage-%02d-%s", i, strings.Repeat("s", 40)), Kind: domain.StageAgent, Role: domain.RoleImplementer,
					DependsOn: []string{fmt.Sprintf("stage-%02d", i)},
				})
			}
			input.References = nil
			for i := 0; i < maxPayloadListItems; i++ {
				input.References = append(input.References, PlanSourceReferencePayload{
					Repository: "acme/repo", Issue: 1000 + i, Available: false, Detail: "issue could not be read: " + strings.Repeat("d", 60),
				})
			}
			observed := invocation().InvocationObservation
			input.Reasoning.Invocation = &observed
			reasons := make([]string, 0, maxPayloadListItems+4)
			for i := 0; i < cap(reasons); i++ {
				reasons = append(reasons, fmt.Sprintf("reason %02d: %s", i, strings.Repeat("r", 80)))
			}
			if _, err := f.service.RecordPlanningRefusal(input, &PlannerRefusedError{AgentID: "codex", Detail: strings.Join(reasons, "; ")}); err != nil {
				t.Fatalf("the worst refusal could not be recorded: %v", err)
			}
			events, err := f.store.PlanEvents("plan-attempt")
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type != EventPlanAttemptRefused {
					continue
				}
				canonical, err := CanonicalJSON(event.Payload)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s planner refusal event: %d canonical bytes, %d headroom", name, len(canonical), maxCanonicalPayloadBytes-len(canonical))
				payload, err := decodePayload[PlanAttemptRefusedPayload](event.Payload)
				if err != nil {
					t.Fatal(err)
				}
				if payload.Reasoning == nil || payload.Reasoning.Invocation == nil {
					t.Fatal("the refusal dropped the invocation it had room to carry a bounded form of")
				}
				return
			}
			t.Fatal("no refusal event was journalled")
		})
	}
}

// The event's schema refuses what the producer never writes.
func TestAttemptProvenanceRefusesUnboundedMaterial(t *testing.T) {
	valid := newExecutionAttemptProvenance("op-invoke", 1, *deadlineProvenance())
	for name, mutate := range map[string]func(*ExecutionAttemptProvenance){
		"unbound attempt":   func(p *ExecutionAttemptProvenance) { p.AttemptIdentity = 0 },
		"no operation":      func(p *ExecutionAttemptProvenance) { p.OperationID = "" },
		"carried refusals":  func(p *ExecutionAttemptProvenance) { p.Invocation.GitRefusals = []GitRefusal{{Operation: "git clean"}} },
		"nine denied tools": func(p *ExecutionAttemptProvenance) { p.Invocation.PermissionDeniedTools = make([]string, 9) },
		"denied tool input": func(p *ExecutionAttemptProvenance) { p.Invocation.PermissionDeniedTools = []string{"Bash(rm -rf .)"} },
		"unbounded argv":    func(p *ExecutionAttemptProvenance) { p.Invocation.Argv = make([]string, maxProvenanceArgs+2) },
		"unbounded string":  func(p *ExecutionAttemptProvenance) { p.Invocation.Version = strings.Repeat("v", 201) },
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			record.Invocation.Argv = append([]string(nil), valid.Invocation.Argv...)
			mutate(&record)
			payload, err := marshalPayloadJSON(record)
			if err != nil {
				t.Fatal(err)
			}
			if validateEventPayload(EngineeringEvent{Type: EventExecutionAttemptProvenance, Payload: payload}) == nil {
				t.Fatal("the event schema accepted it")
			}
		})
	}
	payload, err := marshalPayloadJSON(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventPayload(EngineeringEvent{Type: EventExecutionAttemptProvenance, Payload: payload}); err != nil {
		t.Fatalf("the ordinary event was refused: %v", err)
	}
}

// Denied tools are read from the TYPED tool_name only, as a bounded set.
func TestDeniedToolNamesAreTypedBoundedIdentifiers(t *testing.T) {
	var denials []json.RawMessage
	for _, raw := range []string{
		`{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"rm -rf ."}}`,
		`{"tool_name":"Bash","tool_use_id":"t2"}`,
		`{"tool_name":"Write the file anyway please"}`,
		`{"tool_name":"` + strings.Repeat("A", domain.MaxPermissionDeniedToolBytes+1) + `"}`,
		`{"tool_name":7}`,
		`{"reason":"no tool name"}`,
		`{"tool_name":"mcp__github__create_issue"}`,
		`{"tool_name":"WebFetch"}`,
	} {
		denials = append(denials, json.RawMessage(raw))
	}
	got := deniedToolNames(denials)
	if want := []string{"Bash", "WebFetch", "mcp__github__create_issue"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("denied tools = %v, want %v", got, want)
	}
	denials = nil
	for i := 0; i < 20; i++ {
		denials = append(denials, json.RawMessage(fmt.Sprintf(`{"tool_name":"Tool%02d"}`, i)))
	}
	if got := deniedToolNames(denials); len(got) != domain.MaxPermissionDeniedTools {
		t.Fatalf("recorded %d denied tools, want the bound of %d", len(got), domain.MaxPermissionDeniedTools)
	}
}

// Test 7 of #327, runtime half: the planner persists the whole explanatory
// core with its reasoning provenance, not one field of it.
func TestPlanningReasoningCarriesTheInvocationObservation(t *testing.T) {
	input, provider := plannerFixture(t, goodAnswer)
	provider.invocation = deadlineProvenance()
	provider.invocation.PermissionMode = "plan"
	output, err := InvokePlanner(context.Background(), input)
	if err != nil {
		t.Fatalf("planning invocation: %v", err)
	}
	if output.Reasoning.Invocation == nil {
		t.Fatal("planning dropped its invocation provenance")
	}
	if !reflect.DeepEqual(*output.Reasoning.Invocation, provider.invocation.InvocationObservation) {
		t.Fatalf("planning recorded %+v", *output.Reasoning.Invocation)
	}
	if output.Reasoning.ProviderMode != "plan" {
		t.Fatalf("provider mode = %q", output.Reasoning.ProviderMode)
	}
}

func refusalReasoning() *PlanReasoningPayload {
	return reasoningPayload(domain.PlanReasoningProvenance{
		AgentID: "claude", ProviderKind: AgentKindClaudeCode, TrustMode: domain.TrustRequirementOperatorTrusted,
		InvocationMode:        domain.InvocationModeNonMutatingPlanning,
		WorkspaceDigestBefore: strings.Repeat("a", 64), WorkspaceDigestAfter: strings.Repeat("a", 64),
		WorkspaceUnchanged: true,
	})
}

// A refusal already at the ceiling cannot take even the fixed-size core of its
// invocation. It still appends, and it SAYS the provenance was dropped for
// size, so "dropped" never reads as "the provider reported none".
func TestARefusalAtTheCeilingMarksItsInvocationAsDropped(t *testing.T) {
	base := PlanAttemptRefusedPayload{
		AttemptID: PendingAttemptID, Revision: 1, Origin: domain.ProposalOriginInitial,
		Errors: []string{"the provider reported execution_incomplete"},
	}
	// Room for the flag, not for the minimal observation.
	flagged := func(p PlanAttemptRefusedPayload) bool {
		r := *refusalReasoning()
		r.InvocationDroppedForSize = true
		p.Reasoning, p.AttemptID = &r, strings.Repeat("a", maxPayloadFieldBytes)
		return appendable(EventPlanAttemptRefused, p)
	}
	// Add depends_on elements across stages until one no longer fits, then
	// size that last element to the byte.
padding:
	for s := 0; s < maxPayloadListItems; s++ {
		base.Stages = append(base.Stages, PlanAttemptStagePayload{ID: fmt.Sprintf("pad-%02d", s), Kind: string(domain.StageAgent)})
		stage := &base.Stages[len(base.Stages)-1]
		for d := 0; d < maxPayloadListItems; d++ {
			stage.DependsOn = append(stage.DependsOn, long(fmt.Sprintf("dep-%02d-%02d-", s, d)))
			if flagged(base) {
				continue
			}
			lo, hi := 0, maxPayloadFieldBytes
			for lo < hi {
				mid := (lo + hi + 1) / 2
				stage.DependsOn[d] = strings.Repeat("d", mid)
				if flagged(base) {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			if lo == 0 {
				stage.DependsOn = stage.DependsOn[:d]
			} else {
				stage.DependsOn[d] = strings.Repeat("d", lo)
			}
			break padding
		}
	}
	if !flagged(base) {
		t.Fatal("could not pad the refusal to the ceiling")
	}
	payload := base
	payload.Reasoning = refusalReasoning()
	fitRefusalInvocation(&payload, deadlineProvenance().InvocationObservation)
	if payload.Reasoning.Invocation != nil || !payload.Reasoning.InvocationDroppedForSize {
		t.Fatalf("a dropped invocation was not marked: invocation=%v dropped=%v", payload.Reasoning.Invocation, payload.Reasoning.InvocationDroppedForSize)
	}
	payload.AttemptID = strings.Repeat("a", maxPayloadFieldBytes)
	if !appendable(EventPlanAttemptRefused, payload) {
		t.Fatal("marking the drop made the refusal unappendable")
	}
	// A refusal with room keeps the invocation, unmarked.
	roomy := PlanAttemptRefusedPayload{AttemptID: PendingAttemptID, Revision: 1, Origin: domain.ProposalOriginInitial,
		Errors: []string{"refused"}, Reasoning: refusalReasoning()}
	fitRefusalInvocation(&roomy, deadlineProvenance().InvocationObservation)
	if roomy.Reasoning.Invocation == nil || roomy.Reasoning.InvocationDroppedForSize {
		t.Fatal("a refusal with room lost or mis-marked its invocation")
	}
}
