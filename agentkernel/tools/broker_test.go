package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestNewBrokerRefusesInvalidRegistrations(t *testing.T) {
	read := func(name string) Tool { return fakeTool(name, api.CapabilityFileRead, api.ToolResult{}, nil) }
	schema := func(s string) Tool {
		t := fakeTool("s", api.CapabilityFileRead, api.ToolResult{}, nil).(*tool)
		t.spec.InputSchema = json.RawMessage(s)
		return t
	}
	cases := map[string][]Tool{
		"duplicate":    {read("a"), read("a")},
		"invalid name": {read("bad name")},
		"dotted name":  {read("a.b")},
		"nil":          {nil},
		"unknown kind": {fakeTool("x", "net.fetch", api.ToolResult{}, nil)},
		"open schema":  {schema(`{"type":"object","additionalProperties":true,"properties":{}}`)},
		"unenforced keyword": {schema(
			`{"type":"object","additionalProperties":false,"properties":{"p":{"type":"string","pattern":"x"}}}`)},
		"number type":  {schema(`{"type":"object","additionalProperties":false,"properties":{"p":{"type":"number"}}}`)},
		"bad required": {schema(`{"type":"object","additionalProperties":false,"properties":{},"required":["p"]}`)},
	}
	for name, tools := range cases {
		if _, err := NewBroker(tools...); err == nil {
			t.Errorf("%s: NewBroker accepted", name)
		}
	}
}

func TestSpecsOfferOnlyGrantedPermittedTools(t *testing.T) {
	f := newFixture(t, nil)
	names := func(specs []api.ToolSpec) string {
		var out []string
		for _, s := range specs {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	write := grant("w", api.CapabilityFileWrite, ".")
	read := grant("r", api.CapabilityFileRead, ".")
	if got := names(f.broker.Specs([]api.Capability{read}, api.ModeReadOnly)); got != "read_file,read_files" {
		t.Fatalf("read grant offers %q", got)
	}
	// read_only is structural: a write grant that somehow reached the broker
	// still offers no write tool.
	if got := names(f.broker.Specs([]api.Capability{read, write}, api.ModeReadOnly)); got != "read_file,read_files" {
		t.Fatalf("read_only offers %q", got)
	}
	if got := names(f.broker.Specs([]api.Capability{write}, api.ModeReadWrite)); got != "write_file,apply_patch" {
		t.Fatalf("write grant offers %q", got)
	}
	if got := names(f.broker.Specs(nil, api.ModeReadWrite)); got != "" {
		t.Fatalf("no grants offers %q", got)
	}
}

func TestDispatchRefusesBeforeReachingTheTool(t *testing.T) {
	f := newFixture(t, map[string]string{"src/a.txt": "original\n"})
	before := digests(t, f.root, f.outside)
	write := grant("w", api.CapabilityFileWrite, "src")
	digest := api.Digest([]byte("original\n"))
	good := `{"path":"src/a.txt","content":"x","expected_sha256":"` + digest + `"}`
	cases := map[string]struct {
		call api.ToolCall
		env  Env
	}{
		"unknown tool":        {call("rm_rf", `{"path":"src"}`), f.env(api.ModeReadWrite, write)},
		"read_only mode":      {call("write_file", good), f.env(api.ModeReadOnly, write)},
		"unknown mode":        {call("write_file", good), f.env("admin", write)},
		"missing capability":  {call("write_file", good), f.env(api.ModeReadWrite, grant("r", api.CapabilityFileRead, "."))},
		"grant outside path":  {call("write_file", good), f.env(api.ModeReadWrite, grant("w2", api.CapabilityFileWrite, "docs"))},
		"names a grant":       {call("write_file", strings.Replace(good, "{", `{"grant":"w",`, 1)), f.env(api.ModeReadWrite)},
		"extra field":         {call("write_file", strings.Replace(good, "{", `{"mode":"read_write",`, 1)), f.env(api.ModeReadWrite, write)},
		"duplicate key":       {call("write_file", strings.Replace(good, "{", `{"path":"docs/x",`, 1)), f.env(api.ModeReadWrite, write)},
		"trailing data":       {call("write_file", good+`{}`), f.env(api.ModeReadWrite, write)},
		"wrong type":          {call("write_file", strings.Replace(good, `"x"`, `7`, 1)), f.env(api.ModeReadWrite, write)},
		"null field":          {call("write_file", strings.Replace(good, `"x"`, `null`, 1)), f.env(api.ModeReadWrite, write)},
		"missing field":       {call("write_file", `{"path":"src/a.txt","content":"x"}`), f.env(api.ModeReadWrite, write)},
		"not an object":       {call("write_file", `["src/a.txt"]`), f.env(api.ModeReadWrite, write)},
		"null arguments":      {call("write_file", `null`), f.env(api.ModeReadWrite, write)},
		"empty arguments":     {call("write_file", ``), f.env(api.ModeReadWrite, write)},
		"zero output limit":   {call("write_file", good), Env{Grants: []api.Capability{write}, Mode: api.ModeReadWrite}},
		"integer as fraction": {call("read_file", `{"path":"src/a.txt","start_line":1.5}`), f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))},
	}
	for name, c := range cases {
		res, err := f.broker.Dispatch(context.Background(), c.call, c.env)
		if err != nil || res.Status != api.ToolRefused || res.Mutated || res.Grant != "" || res.CallID != "call-1" {
			t.Errorf("%s: got %+v, %v; want refused without grant or mutation", name, res, err)
		}
	}
	sameDigests(t, before, digests(t, f.root, f.outside))
}

func TestDispatchSelectsGrantCoveringPath(t *testing.T) {
	f := newFixture(t, map[string]string{"a/x.txt": "ax", "b/x.txt": "bx"})
	env := f.env(api.ModeReadOnly, grant("ga", api.CapabilityFileRead, "a"), grant("gb", api.CapabilityFileRead, "b"))
	res, err := f.broker.Dispatch(context.Background(), call("read_file", map[string]string{"path": "b/x.txt"}), env)
	if err != nil || res.Status != api.ToolOK || res.Grant != "gb" || !strings.HasSuffix(res.Output, "bx") {
		t.Fatalf("got %+v, %v", res, err)
	}
	res, _ = f.broker.Dispatch(context.Background(), call("read_files", map[string][]string{"paths": {"a/x.txt", "b/x.txt"}}), env)
	if res.Status != api.ToolRefused {
		t.Fatalf("a macro spanning two grants must be refused, got %+v", res)
	}
}

func TestOutputBoundKeepsErrorExitCodeAndExactArtifact(t *testing.T) {
	f := newFixture(t, nil)
	code := 3
	full := strings.Repeat("é-line\n", 100)
	tool := fakeTool("noisy", api.CapabilityFileRead, api.ToolResult{
		Status: api.ToolError, Output: full, Error: "boom", ExitCode: &code,
	}, nil)
	b, err := NewBroker(tool)
	if err != nil {
		t.Fatal(err)
	}
	env := f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))
	env.OutputLimit = 50
	res, err := b.Dispatch(context.Background(), call("noisy", `{"path":"x"}`), env)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Output) > 50 || res.Error != "boom" || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("bounded result lost metadata: %+v", res)
	}
	if !strings.HasPrefix(full, res.Output) || !utf8.ValidString(res.Output) {
		t.Fatalf("excerpt %q is not a clean UTF-8 prefix", res.Output)
	}
	if res.FullOutput == nil || res.FullOutput.Digest != api.Digest([]byte(full)) || res.FullOutput.Size != int64(len(full)) {
		t.Fatalf("FullOutput = %+v, want exact digest of full output", res.FullOutput)
	}
	got, err := f.store.Get(context.Background(), *res.FullOutput)
	if err != nil || string(got) != full {
		t.Fatalf("artifact = %d bytes, %v", len(got), err)
	}
}

func TestRecordingFailureIsExplicit(t *testing.T) {
	big := strings.Repeat("x", 100)
	env := Env{Grants: []api.Capability{grant("w", api.CapabilityFileWrite, ".")}, Mode: api.ModeReadWrite,
		Artifacts: failingStore{}, Producer: "p", OutputLimit: 10}
	mutator := fakeTool("mutator", api.CapabilityFileWrite, api.ToolResult{Status: api.ToolOK, Output: big, Mutated: true}, nil)
	reader := fakeTool("reader", api.CapabilityFileRead, api.ToolResult{Status: api.ToolOK, Output: big}, nil)
	b, err := NewBroker(mutator, reader)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Dispatch(context.Background(), call("mutator", `{"path":"a"}`), env)
	if err == nil || !res.Mutated || res.Status != api.ToolError || !strings.Contains(res.Error, "disk full") {
		t.Fatalf("mutation with unrecordable output: %+v, %v; want unknown-outcome error", res, err)
	}
	env.Grants = append(env.Grants, grant("r", api.CapabilityFileRead, "."))
	res, err = b.Dispatch(context.Background(), call("reader", `{"path":"a"}`), env)
	if err != nil || res.Status != api.ToolError || res.Output != "" || !res.Truncated {
		t.Fatalf("read with unrecordable output: %+v, %v; want a plain tool error", res, err)
	}
}

func TestToolErrorOnMutatingKindIsUnknownOutcome(t *testing.T) {
	tool := fakeTool("w", api.CapabilityFileWrite, api.ToolResult{}, context.DeadlineExceeded)
	b, err := NewBroker(tool)
	if err != nil {
		t.Fatal(err)
	}
	env := Env{Grants: []api.Capability{grant("g", api.CapabilityFileWrite, ".")}, Mode: api.ModeReadWrite, OutputLimit: 10}
	res, err := b.Dispatch(context.Background(), call("w", `{"path":"a"}`), env)
	if err == nil || res.Status != api.ToolError || res.Grant != "g" {
		t.Fatalf("got %+v, %v", res, err)
	}
}
