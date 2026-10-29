package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type unfinishedVerificationProvider struct {
	*isolatedProvider
	started bool
}

func (p *unfinishedVerificationProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	result, err := p.isolatedProvider.Execute(ctx, request)
	if err != nil || p.started {
		return result, err
	}
	p.started = true
	v, ok := verificationExecutionFrom(ctx)
	if !ok {
		return ExecutionResult{}, os.ErrInvalid
	}
	permit, err := v.Scheduler.RequestVerification(v.Parent, "unfinished-tool", "unfinished-tool-owner")
	if err != nil {
		return ExecutionResult{}, err
	}
	if acquired, err := v.Scheduler.AcquireVerification(permit); !acquired || err != nil {
		return ExecutionResult{}, os.ErrInvalid
	}
	return result, nil
}

func TestNestedVerificationPreservesWorkUntilToolCleanupIsProven(t *testing.T) {
	f := newPhase8Fixture(t)
	provider := &unfinishedVerificationProvider{isolatedProvider: f.provider}
	deps := f.deps
	deps.Provider = provider
	deps.Liveness = alwaysAlive()
	f.runtime = f.newRuntime(deps)
	id := f.start()
	outcome := f.reconcile(id)
	if outcome.Reason != ReasonVerificationCleanup || len(provider.requests) != 1 {
		t.Fatalf("unverified tool restarted its parent: %+v, calls=%d", outcome, len(provider.requests))
	}
	path := filepath.Join(candidateDir(f.stateDir, id), "candidate.go")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "package candidate\n" {
		t.Fatalf("produced work was moved while its tool still owned it: %q/%v", data, err)
	}
	for _, event := range eventsOf(t, f.store, id) {
		if event.Type == EventCandidateCommitted || event.Type == EventCandidateQuarantined {
			t.Fatalf("unverified tool allowed material settlement: %s", event.Type)
		}
	}
	f.clock.advance(2 * time.Minute)
	deps.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner != "unfinished-tool-owner" })
	f.runtime = f.newRuntime(deps)
	outcome = f.reconcile(id)
	// Cleanup does not guess that an otherwise unknown provider failure is
	// retryable. The original invocation and its work remain preserved.
	if len(provider.requests) != 1 || outcome.Disposition != Failed || outcome.Reason != "execution.invoke_failure_not_retryable" {
		t.Fatalf("cleanup changed unknown failure semantics: %d / %+v", len(provider.requests), outcome)
	}
	if pending, err := f.runtime.scheduler.VerificationCleanupPending(id); pending || err != nil {
		t.Fatalf("proven cleanup left capacity held: %v/%v", pending, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "package candidate\n" {
		t.Fatal("cleanup lost the preserved candidate work")
	}
}
