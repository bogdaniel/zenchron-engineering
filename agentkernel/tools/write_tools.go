package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Absent is the expected_sha256 precondition for creating a file that must
// not exist yet.
const Absent = "absent"

// WriteFile returns the write_file tool (file.write). The caller must state
// the digest of the content it last read (or Absent); a mismatch fails and
// never overwrites newer content.
func (w *Workspace) WriteFile() Tool {
	return &tool{
		spec: api.ToolSpec{
			Name:        "write_file",
			Description: `Create or replace a workspace file. expected_sha256 is the current content digest from read_file, or "absent" to create.`,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,` +
				`"required":["path","content","expected_sha256"],"properties":{"path":{"type":"string"},` +
				`"content":{"type":"string"},"expected_sha256":{"type":"string"}}}`),
		},
		kind: api.CapabilityFileWrite, scope: pathScope,
		invoke: func(ctx context.Context, inv Invocation) (api.ToolResult, error) {
			var a struct {
				Path     string `json:"path"`
				Content  string `json:"content"`
				Expected string `json:"expected_sha256"`
			}
			if err := json.Unmarshal(inv.Arguments, &a); err != nil {
				return failed("arguments: %v", err), nil
			}
			return w.writeChecked(ctx, inv.Grant, a.Path, a.Expected, func([]byte, bool) ([]byte, error) {
				return []byte(a.Content), nil
			}), nil
		},
	}
}

// ApplyPatch returns the apply_patch tool (file.write): replace exactly one
// occurrence of old_text, under the same digest precondition as write_file.
func (w *Workspace) ApplyPatch() Tool {
	return &tool{
		spec: api.ToolSpec{
			Name:        "apply_patch",
			Description: "Replace the single occurrence of old_text with new_text in an existing file whose current digest is expected_sha256.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,` +
				`"required":["path","expected_sha256","old_text","new_text"],"properties":{"path":{"type":"string"},` +
				`"expected_sha256":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}}}`),
		},
		kind: api.CapabilityFileWrite, scope: pathScope,
		invoke: func(ctx context.Context, inv Invocation) (api.ToolResult, error) {
			var a struct {
				Path     string `json:"path"`
				Expected string `json:"expected_sha256"`
				Old      string `json:"old_text"`
				New      string `json:"new_text"`
			}
			if err := json.Unmarshal(inv.Arguments, &a); err != nil {
				return failed("arguments: %v", err), nil
			}
			return w.writeChecked(ctx, inv.Grant, a.Path, a.Expected, func(cur []byte, exists bool) ([]byte, error) {
				if !exists {
					return nil, errors.New("file does not exist")
				}
				if n := bytes.Count(cur, []byte(a.Old)); a.Old == "" || n != 1 {
					return nil, fmt.Errorf("old_text must occur exactly once, found %d", n)
				}
				return bytes.Replace(cur, []byte(a.Old), []byte(a.New), 1), nil
			}), nil
		},
	}
}

// writeChecked replaces p atomically if its content still matches expected:
// checked once before the new bytes are staged and again immediately before
// the rename, so a writer that raced in between wins and this call fails.
func (w *Workspace) writeChecked(ctx context.Context, grant api.Capability, p, expected string,
	change func(current []byte, exists bool) ([]byte, error)) api.ToolResult {
	if expected != Absent && !api.ValidDigest(expected) {
		return failed(`expected_sha256 must be "absent" or a sha256:<hex> digest`)
	}
	if err := ctx.Err(); err != nil {
		return failed("%v", err)
	}
	gr, _, rel, err := w.open(grant, p)
	if err != nil {
		return failed("%v", err)
	}
	defer gr.Close()
	current, mode, err := precondition(gr, rel, expected)
	if err != nil {
		return failed("%v", err)
	}
	next, err := change(current, current != nil)
	if err != nil {
		return failed("%v", err)
	}
	if len(next) > MaxFileBytes {
		return failed("new content exceeds %d bytes", MaxFileBytes)
	}
	madeDirs, err := ensureDir(gr, path.Dir(rel))
	if err == nil {
		err = refuseSymlinks(gr, path.Dir(rel)) // the temp file and rename land in this directory
	}
	if err != nil {
		res := failed("%v", err)
		res.Mutated = madeDirs
		return res
	}
	if err := replace(gr, rel, expected, next, mode); err != nil {
		res := failed("%v", err)
		res.Mutated = madeDirs
		return res
	}
	return api.ToolResult{
		Status:  api.ToolOK,
		Output:  fmt.Sprintf("wrote %s (%d bytes)\ndigest: %s\n", p, len(next), api.Digest(next)),
		Mutated: true,
	}
}

// precondition returns the current content (nil when absent) and the mode a
// replacement keeps, or an error when the content is not what was expected.
// No component of rel may be a symlink, so neither the target nor a parent
// directory can alias a file outside the grant.
func precondition(gr *os.Root, rel, expected string) ([]byte, fs.FileMode, error) {
	err := refuseSymlinks(gr, rel)
	if errors.Is(err, fs.ErrNotExist) {
		if expected != Absent {
			return nil, 0, fmt.Errorf("precondition failed: %q does not exist", rel)
		}
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := gr.Lstat(rel)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("refusing to write %q: not a regular file", rel)
	}
	current, err := readRegular(gr, rel)
	if err != nil {
		return nil, 0, err
	}
	if got := api.Digest(current); got != expected {
		return nil, 0, fmt.Errorf("precondition failed: current content is %s, not %s; re-read before writing", got, expected)
	}
	return current, info.Mode().Perm(), nil
}

func ensureDir(gr *os.Root, dir string) (bool, error) {
	if dir == "." {
		return false, nil
	}
	if _, err := gr.Stat(dir); err == nil {
		return false, nil
	}
	return true, gr.MkdirAll(dir, 0o755)
}

func replace(gr *os.Root, rel, expected string, data []byte, mode fs.FileMode) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(rel), ".zk-"+hex.EncodeToString(suffix)+".tmp")
	f, err := gr.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		_, _, err = precondition(gr, rel, expected)
	}
	if err == nil {
		err = gr.Rename(tmp, rel)
	}
	if err != nil {
		return errors.Join(err, removeTemp(gr, tmp))
	}
	return nil
}

func removeTemp(gr *os.Root, tmp string) error {
	if err := gr.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
