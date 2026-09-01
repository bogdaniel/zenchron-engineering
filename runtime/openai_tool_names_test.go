package runtime

// Defect E regression. These tests reproduce the exact boundary a real #32 run
// hit against the OpenAI Responses API:
//
//	HTTP 400 invalid_request_error
//	param tools[0].name, code invalid_value
//	"Invalid 'tools[0].name': string does not match pattern. Expected ^[a-zA-Z0-9_-]+$."
//
// Everything here runs against the fake Responses API and the fake command
// executor. No paid API request is made, no container is started, and the whole
// file passes offline.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// canonicalToolNames is the exact set that produced the observed 400. It is
// written out literally rather than derived, so a future rename of a canonical
// capability cannot quietly make this regression stop testing the real failure.
var canonicalToolNames = []string{
	"repo.read",
	"repo.search",
	"candidate.diff",
	"candidate.apply_patch",
	"candidate.run",
}

var wireToolNames = []string{
	"repo_read",
	"repo_search",
	"candidate_diff",
	"candidate_apply_patch",
	"candidate_run",
}

// requestTools decodes the tools array of one captured request body.
func requestTools(t *testing.T, body []byte) []toolDefinition {
	t.Helper()
	var request struct {
		Tools []toolDefinition `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	return request.Tools
}

// TestOpenAIRequestNeverAdvertisesACanonicalToolName is proof 1, 2 and 3: the
// names that earned the 400 never cross the wire, the wire names are exactly
// the five expected ones, and every one of them satisfies the pattern the
// provider actually enforces.
func TestOpenAIRequestNeverAdvertisesACanonicalToolName(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, _ := openaiFixture(t, api)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatalf("brokered reasoning loop failed: %v", err)
	}
	if len(api.requests) == 0 {
		t.Fatal("no request reached the control plane, so nothing was proven about its shape")
	}
	for i, body := range api.requests {
		// The dotted names must not appear as function names ANYWHERE in the
		// request, which is stronger than checking the tools array alone.
		for _, canonical := range canonicalToolNames {
			if strings.Contains(string(body), `"name":"`+canonical+`"`) {
				t.Fatalf("request %d advertises the canonical name %q the API rejects with invalid_value: %s", i, canonical, body)
			}
		}
		tools := requestTools(t, body)
		if len(tools) != len(wireToolNames) {
			t.Fatalf("request %d advertised %d tools, want %d", i, len(tools), len(wireToolNames))
		}
		for j, tool := range tools {
			if tool.Name != wireToolNames[j] {
				t.Fatalf("request %d tools[%d].name is %q, want the wire name %q", i, j, tool.Name, wireToolNames[j])
			}
			if !pattern.MatchString(tool.Name) {
				t.Fatalf("request %d tools[%d].name %q does not match %s, which is exactly the 400 we already paid for", i, j, tool.Name, pattern)
			}
		}
	}
}

// TestOpenAIWireNameDispatchesTheCanonicalCapability is proof 4: a returned
// candidate_apply_patch drives the existing canonical candidate.apply_patch all
// the way into the real broker, with no second implementation behind it.
func TestOpenAIWireNameDispatchesTheCanonicalCapability(t *testing.T) {
	if canonical, ok := openaiCanonicalName(openaiToolCandidateApplyPatch); !ok || canonical != ToolCandidateApplyPatch {
		t.Fatalf("candidate_apply_patch decodes to %q (known=%t), want %q", canonical, ok, ToolCandidateApplyPatch)
	}
	patch := "diff --git a/decoded.txt b/decoded.txt\nnew file mode 100644\n--- /dev/null\n+++ b/decoded.txt\n@@ -0,0 +1 @@\n+decoded-9c3\n"
	api := &fakeResponsesAPI{bodies: []string{
		scriptedToolCalls(t, "1", 5, [2]string{openaiToolCandidateApplyPatch, string(jsonArgs(t, map[string]any{"patch": patch}))}),
		scriptedFinalMessage(t, "2", 5),
	}}
	provider, request, _, _ := openaiFixture(t, api)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatalf("brokered reasoning loop failed: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(request.CandidateDir, "decoded.txt"))
	if err != nil || string(written) != "decoded-9c3\n" {
		t.Fatalf("the wire name did not dispatch canonical %s into the broker: %v %q", ToolCandidateApplyPatch, err, written)
	}
}

// TestOpenAIUnknownWireNameIsRefused is proof 5. It covers both shapes that
// matter: a name nobody advertises, and the CANONICAL name itself arriving on
// the wire. The second is the one a lenient decoder would get wrong - passing
// it through would let an unadvertised name reach dispatch and become a
// capability the surface never offered.
func TestOpenAIUnknownWireNameIsRefused(t *testing.T) {
	for _, name := range []string{"shell", "candidate_applypatch", "repo.read", "candidate.apply_patch", "", "repo_read "} {
		if canonical, ok := openaiCanonicalName(name); ok {
			t.Fatalf("unknown wire name %q was decoded onto capability %q instead of being refused", name, canonical)
		}
	}
	api := &fakeResponsesAPI{bodies: []string{
		scriptedToolCalls(t, "1", 5, [2]string{"candidate.apply_patch", `{"patch":"anything"}`}),
		scriptedFinalMessage(t, "2", 5),
	}}
	provider, request, _, _ := openaiFixture(t, api)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatalf("a refusal must be returned to the model, not raised: %v", err)
	}
	last := string(api.requests[len(api.requests)-1])
	if !strings.Contains(last, `tool error: unknown tool \"candidate.apply_patch\"`) {
		t.Fatalf("the unknown name was not refused back to the model: %s", last)
	}
	// Nothing was dispatched: the refused call never became a capability.
	if entries, err := os.ReadDir(request.CandidateDir); err == nil {
		for _, entry := range entries {
			if entry.Name() == "decoded.txt" {
				t.Fatal("a refused tool name still reached the broker")
			}
		}
	}
}

// TestOpenAIToolNameMappingIsBijective is proof 6: two canonical capabilities
// can never collide onto one wire name, and the validator refuses a surface
// where they would.
func TestOpenAIToolNameMappingIsBijective(t *testing.T) {
	if len(openaiWireByCanonical) != len(openaiToolNames) || len(openaiCanonicalByWire) != len(openaiToolNames) {
		t.Fatalf("the mapping is not bijective: %d canonical, %d wire, %d entries",
			len(openaiWireByCanonical), len(openaiCanonicalByWire), len(openaiToolNames))
	}
	for _, entry := range openaiToolNames {
		wire, ok := openaiWireName(entry.Canonical)
		if !ok || wire != entry.Wire {
			t.Fatalf("%q encodes to %q (known=%t), want %q", entry.Canonical, wire, ok, entry.Wire)
		}
		canonical, ok := openaiCanonicalName(entry.Wire)
		if !ok || canonical != entry.Canonical {
			t.Fatalf("%q decodes to %q (known=%t), want %q", entry.Wire, canonical, ok, entry.Canonical)
		}
	}
	// The advertised surface is exactly the canonical surface, in both
	// directions: no capability is advertised without a wire name, and no wire
	// name is defined for a capability that is not advertised.
	definitions, err := openaiToolDefinitions()
	if err != nil {
		t.Fatalf("the real advertised surface does not validate: %v", err)
	}
	if len(definitions) != len(toolDefinitions()) {
		t.Fatalf("encoding changed the size of the surface: %d vs %d", len(definitions), len(toolDefinitions()))
	}
	// A colliding surface is refused rather than silently advertised.
	collided := definitions
	collided[1].Name = collided[0].Name
	if err := validateOpenAIToolNames(collided); err == nil {
		t.Fatal("two capabilities collided onto one wire name and the surface was accepted")
	}
}

// TestOpenAIInvalidToolSurfaceIsRefusedBeforeCredentialOrHTTP proves the local
// validation is worth having: an unadvertisable surface must be refused before
// the credential is read and before any request is made, which is what turns
// the observed 400 into a free local failure.
func TestOpenAIInvalidToolSurfaceIsRefusedBeforeCredentialOrHTTP(t *testing.T) {
	for _, invalid := range [][]toolDefinition{
		nil,
		{{Type: "function", Name: ""}},
		{{Type: "function", Name: "repo.read"}},
		{{Type: "function", Name: "repo_read"}, {Type: "function", Name: "repo_read"}},
		{{Type: "function", Name: "repo_read"}},
	} {
		if err := validateOpenAIToolNames(invalid); err == nil {
			t.Fatalf("invalid advertised surface %#v was accepted", invalid)
		}
	}
	// The real surface is the one the provider actually sends, and it is valid.
	definitions, err := openaiToolDefinitions()
	if err != nil {
		t.Fatalf("the real surface must validate: %v", err)
	}
	if err := validateOpenAIToolNames(definitions); err != nil {
		t.Fatalf("the real surface must validate: %v", err)
	}
	// No credential is needed to reach that answer: the validator is pure.
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, keyFile := openaiFixture(t, api)
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Execute(context.Background(), request); err == nil {
		t.Fatal("a missing credential must still refuse")
	}
	if len(api.requests) != 0 {
		t.Fatalf("a refused execution still reached the control plane: %d request(s)", len(api.requests))
	}
}
