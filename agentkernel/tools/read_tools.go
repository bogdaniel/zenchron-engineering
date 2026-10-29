package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

const (
	maxSearchHits     = 1000
	maxPatternBytes   = 1024
	maxSearchLineEcho = 300
)

// ReadFile returns the read_file tool (file.read). Its output starts with the
// file's content digest, which is also the precondition write tools take.
// Output over the call's limit is cut by the broker with the full bytes kept
// as an artifact, and the header's line count lets the model ask for a range.
func (w *Workspace) ReadFile() Tool {
	return &tool{
		spec: api.ToolSpec{
			Name:        "read_file",
			Description: "Read a workspace file, optionally a 1-based inclusive line range. Output begins with the file's sha256 digest.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],` +
				`"properties":{"path":{"type":"string","description":"workspace-relative path"},` +
				`"start_line":{"type":"integer"},"end_line":{"type":"integer"}}}`),
		},
		kind: api.CapabilityFileRead, scope: pathScope, invoke: w.readFile,
	}
}

func (w *Workspace) readFile(ctx context.Context, inv Invocation) (api.ToolResult, error) {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(inv.Arguments, &a); err != nil {
		return failed("arguments: %v", err), nil
	}
	if err := ctx.Err(); err != nil {
		return failed("%v", err), nil
	}
	gr, _, rel, err := w.open(inv.Grant, a.Path)
	if err != nil {
		return failed("%v", err), nil
	}
	defer gr.Close()
	data, err := readRegular(gr, rel)
	if err != nil {
		return failed("%v", err), nil
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := max(a.StartLine, 1), a.EndLine
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	if a.StartLine < 0 || a.EndLine < 0 || (len(lines) > 0 && (start > len(lines) || end < start)) {
		return failed("line range %d-%d is outside 1-%d", a.StartLine, a.EndLine, len(lines)), nil
	}
	if len(lines) == 0 {
		start = 0
	}
	header := fmt.Sprintf("path: %s\ndigest: %s\nlines: %d-%d of %d\n\n",
		a.Path, api.Digest(data), start, end, len(lines))
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines[start-1:end], "")
	}
	return api.ToolResult{Status: api.ToolOK, Output: header + body}, nil
}

// Search returns the search tool (file.search): a literal or RE2 pattern over
// regular files under one granted path. A path crossing a symlink is refused,
// links and hard-linked files under it are skipped, binary files are skipped, and hits stop at a fixed bound that the output states.
func (w *Workspace) Search() Tool {
	return &tool{
		spec: api.ToolSpec{
			Name:        "search",
			Description: "Search regular files under a workspace path for a literal string or RE2 regular expression.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","pattern"],` +
				`"properties":{"path":{"type":"string","description":"workspace-relative file or directory"},` +
				`"pattern":{"type":"string"},"literal":{"type":"boolean"}}}`),
		},
		kind: api.CapabilityFileSearch, scope: pathScope, invoke: w.search,
	}
}

var errHitLimit = errors.New("hit limit")

func (w *Workspace) search(ctx context.Context, inv Invocation) (api.ToolResult, error) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Literal bool   `json:"literal"`
	}
	if err := json.Unmarshal(inv.Arguments, &a); err != nil {
		return failed("arguments: %v", err), nil
	}
	if a.Pattern == "" || len(a.Pattern) > maxPatternBytes {
		return failed("pattern must be 1-%d bytes", maxPatternBytes), nil
	}
	expr := a.Pattern
	if a.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return failed("pattern: %v", err), nil
	}
	gr, root, rel, err := w.open(inv.Grant, a.Path)
	if err != nil {
		return failed("%v", err), nil
	}
	defer gr.Close()
	if err := refuseSymlinks(gr, rel); err != nil {
		return failed("%v", err), nil
	}
	var hits []string
	err = fs.WalkDir(gr.FS(), rel, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A submodule or worktree .git is a file; skip any .git entry.
		if strings.EqualFold(d.Name(), ".git") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := readRegular(gr, name)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil // unreadable, oversized and binary files hold no text hits
		}
		return matchLines(re, path.Join(root, name), data, &hits)
	})
	if err != nil && !errors.Is(err, errHitLimit) {
		return failed("search: %v", err), nil
	}
	out := strings.Join(hits, "")
	if errors.Is(err, errHitLimit) {
		out += fmt.Sprintf("[stopped after %d hits; results are incomplete, narrow the path or pattern]\n", maxSearchHits)
	}
	if len(hits) == 0 {
		out += "no matches\n"
	}
	return api.ToolResult{Status: api.ToolOK, Output: out}, nil
}

func matchLines(re *regexp.Regexp, display string, data []byte, hits *[]string) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), MaxFileBytes)
	for n := 1; sc.Scan(); n++ {
		if !re.Match(sc.Bytes()) {
			continue
		}
		if len(*hits) == maxSearchHits {
			return errHitLimit
		}
		*hits = append(*hits, fmt.Sprintf("%s:%d: %s\n", display, n, cut(sc.Text(), maxSearchLineEcho)))
	}
	return sc.Err()
}
