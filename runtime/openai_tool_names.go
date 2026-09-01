package runtime

// OpenAI function tool names are PROVIDER-SPECIFIC WIRE NAMES, not Zenchron
// capability identities.
//
// Zenchron's canonical ToolSurface capabilities are dotted - repo.read,
// candidate.apply_patch - and that is the contract ToolSurface.dispatch and
// every refusal message speak. The OpenAI Responses API imposes its own lexical
// restriction on tools[].name (^[a-zA-Z0-9_-]+$), which a "." violates; a real
// run against the API answered HTTP 400 invalid_request_error, param
// tools[0].name, code invalid_value, "string does not match pattern".
//
// The fix is a codec at the provider boundary, not a rename. The canonical
// names stay exactly what they are; only what crosses the OpenAI wire is
// translated, and it is translated back before anything is dispatched.
//
// The mapping is deliberately an EXPLICIT TABLE rather than punctuation
// replacement. A generic "." -> "_" rewrite is not a codec: it is a guess that
// silently acquires new behaviour whenever a capability name changes, it is not
// provably injective (repo.read and repo_read would collide onto one wire
// name), and its inverse is ambiguous. The table below is checked at provider
// start for exactly the properties the wire depends on, so a capability added
// without a wire name refuses the surface instead of shipping a broken request.
//
// Fail-closed is the rule in both directions. A canonical capability with no
// wire name is never advertised, and a wire name that is not in the table is
// never dispatched: it is refused by name, never normalized onto a neighbour.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// OpenAI wire names. One per canonical capability, and nothing else.
const (
	openaiToolRepoRead            = "repo_read"
	openaiToolRepoSearch          = "repo_search"
	openaiToolCandidateDiff       = "candidate_diff"
	openaiToolCandidateApplyPatch = "candidate_apply_patch"
	openaiToolCandidateRun        = "candidate_run"
)

// openaiToolNamePattern is the provider's documented restriction, encoded from
// the observed 400 rather than from a guess about the whole server validator.
var openaiToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// openaiToolNames is the complete, ordered, explicit mapping. Adding a
// capability to ToolSurface without adding it here makes the OpenAI surface
// refuse to advertise, which is the intended failure.
var openaiToolNames = []struct{ Canonical, Wire string }{
	{ToolRepoRead, openaiToolRepoRead},
	{ToolRepoSearch, openaiToolRepoSearch},
	{ToolCandidateDiff, openaiToolCandidateDiff},
	{ToolCandidateApplyPatch, openaiToolCandidateApplyPatch},
	{ToolCandidateRun, openaiToolCandidateRun},
}

// The two directions are built once from the one table, so they cannot drift
// apart, and a duplicate on either side would make the two maps disagree in
// size - which validateOpenAIToolNames refuses.
var openaiWireByCanonical, openaiCanonicalByWire = func() (map[string]string, map[string]string) {
	wire := make(map[string]string, len(openaiToolNames))
	canonical := make(map[string]string, len(openaiToolNames))
	for _, entry := range openaiToolNames {
		wire[entry.Canonical] = entry.Wire
		canonical[entry.Wire] = entry.Canonical
	}
	return wire, canonical
}()

// openaiWireName encodes one canonical capability for the OpenAI wire.
func openaiWireName(canonical string) (string, bool) {
	name, ok := openaiWireByCanonical[canonical]
	return name, ok
}

// openaiCanonicalName decodes one OpenAI wire name back to the canonical
// capability. An unknown name reports false and MUST be refused: returning the
// input unchanged would make an unadvertised name dispatchable.
func openaiCanonicalName(wire string) (string, bool) {
	name, ok := openaiCanonicalByWire[wire]
	return name, ok
}

// refusedOpenAIToolName is the fail-closed answer to a name the codec does not
// know. It is returned to the model as an ordinary tool result, exactly like
// ToolSurface's own unknown-tool refusal, so the model can correct itself and
// the repetition still counts toward no-progress.
func refusedOpenAIToolName(wire string) string {
	advertised := make([]string, 0, len(openaiToolNames))
	for _, entry := range openaiToolNames {
		advertised = append(advertised, entry.Wire)
	}
	return "tool error: unknown tool " + strconv.Quote(wire) + "; the available tools are " + strings.Join(advertised, ", ")
}

// validateOpenAIToolNames proves the invariants the wire actually depends on,
// for the EXACT surface about to be advertised. It is deliberately not a
// re-implementation of the provider's evolving server-side validator: it
// encodes only what a real 400 gave us evidence for, plus the codec properties
// that make decoding a returned name unambiguous.
func validateOpenAIToolNames(definitions []toolDefinition) error {
	if len(definitions) == 0 {
		return fmt.Errorf("openai tool surface is empty")
	}
	seen := make(map[string]bool, len(definitions))
	for i, definition := range definitions {
		name := definition.Name
		if name == "" {
			return fmt.Errorf("openai tool surface: tools[%d].name is empty", i)
		}
		if !openaiToolNamePattern.MatchString(name) {
			return fmt.Errorf("openai tool surface: tools[%d].name %q does not match %s", i, name, openaiToolNamePattern.String())
		}
		if seen[name] {
			return fmt.Errorf("openai tool surface: tools[%d].name %q is advertised more than once", i, name)
		}
		seen[name] = true
		canonical, known := openaiCanonicalName(name)
		if !known {
			return fmt.Errorf("openai tool surface: tools[%d].name %q maps to no canonical capability", i, name)
		}
		// The round trip is what makes the mapping bijective in practice and
		// not merely a lookup that happens to answer.
		if back, ok := openaiWireName(canonical); !ok || back != name {
			return fmt.Errorf("openai tool surface: tools[%d].name %q does not round-trip through canonical %q", i, name, canonical)
		}
	}
	// Closure over the advertised surface: the table maps exactly the
	// capabilities being advertised, in both directions, with no duplicate on
	// either side collapsing two capabilities onto one wire name.
	if len(openaiWireByCanonical) != len(openaiToolNames) || len(openaiCanonicalByWire) != len(openaiToolNames) {
		return fmt.Errorf("openai tool surface: the wire name table is not bijective")
	}
	if len(seen) != len(openaiToolNames) {
		return fmt.Errorf("openai tool surface: %d tools advertised but %d wire names are defined", len(seen), len(openaiToolNames))
	}
	return nil
}

// openaiToolDefinitions is the exact surface sent to the Responses API: the
// canonical ToolSurface declarations with their names encoded for the wire,
// validated before it can be marshalled into a request.
func openaiToolDefinitions() ([]toolDefinition, error) {
	definitions := toolDefinitions()
	for i := range definitions {
		wire, ok := openaiWireName(definitions[i].Name)
		if !ok {
			return nil, fmt.Errorf("openai tool surface: canonical capability %q has no OpenAI wire name", definitions[i].Name)
		}
		definitions[i].Name = wire
	}
	if err := validateOpenAIToolNames(definitions); err != nil {
		return nil, err
	}
	return definitions, nil
}
