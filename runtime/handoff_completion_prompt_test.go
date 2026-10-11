package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestProducerHandoffSeparatesImplementationFromAcceptance(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	request.HandoffPath = filepath.Join(t.TempDir(), "handoff.json")
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	prompt := strings.Join(fake.execution(t).args, " ")
	for _, want := range []string{
		"assigned producer implementation is complete",
		"real unfinished implementation",
		"independent review, runtime-owned assurance and publication authority remain separate required gates",
		"sandbox-denied runtime assurance is not unfinished assigned engineering work",
		"never claim those checks passed",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("producer prompt dropped completion boundary %q", want)
		}
	}
}
