package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// findClause is a host-written tool over documents the host supplies in
// memory. It has no repository, change or candidate concept (#446 A02): it
// returns the numbered paragraphs of one document that mention a term.
type findClause struct {
	documents map[string]string
}

type clauseArgs struct {
	Document string `json:"document"`
	Term     string `json:"term"`
}

func (findClause) Spec() api.ToolSpec {
	return api.ToolSpec{
		Name:        "find_clause",
		Description: "Return the numbered paragraphs of a supplied document that mention a term (case-insensitive).",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["document","term"],` +
			`"properties":{"document":{"type":"string"},"term":{"type":"string"}}}`),
	}
}

// Kind is file.search: the host grants it per document root like any search.
func (findClause) Kind() api.CapabilityKind { return api.CapabilityFileSearch }

func (findClause) Scope(arguments json.RawMessage) (tools.Scope, error) {
	var a clauseArgs
	if err := json.Unmarshal(arguments, &a); err != nil {
		return tools.Scope{}, err
	}
	return tools.Scope{Paths: []string{a.Document}}, nil
}

func (f findClause) Invoke(_ context.Context, inv tools.Invocation) (api.ToolResult, error) {
	var a clauseArgs
	if err := json.Unmarshal(inv.Arguments, &a); err != nil {
		return api.ToolResult{Status: api.ToolError, Error: "arguments: " + err.Error()}, nil
	}
	// Recheck the grant at use time rather than trust the broker's selection.
	if !underRoots(a.Document, inv.Grant.Roots) {
		return api.ToolResult{Status: api.ToolRefused, Error: a.Document + " is outside the granted roots"}, nil
	}
	text, ok := f.documents[a.Document]
	if !ok {
		return api.ToolResult{Status: api.ToolError, Error: "no such document: " + a.Document}, nil
	}
	var b strings.Builder
	term := strings.ToLower(a.Term)
	for i, para := range strings.Split(text, "\n\n") {
		if strings.Contains(strings.ToLower(para), term) {
			fmt.Fprintf(&b, "paragraph %d: %s\n", i+1, para)
		}
	}
	if b.Len() == 0 {
		return api.ToolResult{Status: api.ToolOK, Output: "no paragraph mentions " + a.Term}, nil
	}
	return api.ToolResult{Status: api.ToolOK, Output: b.String()}, nil
}

func underRoots(p string, roots []string) bool {
	for _, r := range roots {
		if r == "." || p == r || strings.HasPrefix(p, r+"/") {
			return api.ValidRelativePath(p) && path.Clean(p) == p
		}
	}
	return false
}
