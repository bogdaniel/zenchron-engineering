package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReviewRefusalFindingsSurviveRetryAndClearOnSuccess(t *testing.T) {
	record, err := json.Marshal(executionRecord{mutationResult: mutationResult{FailureClass: FailureVerification}, ReviewRefusal: &ReviewerResultRefusedError{StageID: "review", Detail: strings.Repeat("bad candidate ", 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{snapshot: RunSnapshot{Operations: map[string]RunOperation{
		"first": {ID: "first", Kind: OpExecutionInvoke, State: OperationFailed, Result: record},
		"next":  {ID: "next", Kind: OpExecutionInvoke, CreatedAt: time.Now()},
	}}}
	findings := state.findings()
	if len(findings) != 1 || findings[0].Classification != FailureVerification || !strings.Contains(findings[0].Signature, "bad candidate") || findings[0].Signature != boundedDetail(findings[0].Signature) {
		t.Fatalf("refusal not bounded and actionable: %#v", findings)
	}
	state.snapshot.Operations["next"] = RunOperation{ID: "next", Kind: OpExecutionInvoke, State: Succeeded, Result: json.RawMessage(`{}`), CreatedAt: time.Now()}
	if got := state.findings(); len(got) != 0 {
		t.Fatalf("old refusal survived success: %#v", got)
	}
}

func TestProtectedProviderToolProbeUsesContainer(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)
	provider := OpenAIProvider{Broker: broker}
	name := "host-missing-tool; echo injection"
	if got := provider.MissingTools(context.Background(), []string{name}); len(got) != 0 {
		t.Fatalf("container success refused: %v", got)
	}
	args := brokeredContainerArgs(t, fake)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "PATH="+sandboxPATH) || !strings.Contains(joined, broker.Sandbox.Image) {
		t.Fatalf("wrong environment: %v", args)
	}
	if args[len(args)-1] != name || args[len(args)-3] != `command -v "$1"` {
		t.Fatalf("tool name became shell source: %v", args)
	}
	fake.err = context.DeadlineExceeded
	if got := provider.MissingTools(context.Background(), []string{"go"}); len(got) != 1 {
		t.Fatalf("failed container probe passed: %v", got)
	}
}
