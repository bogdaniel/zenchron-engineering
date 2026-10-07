package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// MaxFileBytes bounds any single file a file tool reads or writes.
const MaxFileBytes = 8 << 20

// Workspace is the host-supplied directory the file tools operate in.
//
// Its path guards are development-grade, not protected isolation, and a host
// must report them as such: every path is checked lexically against the
// granted roots, a granted root may not cross a symlink, and every open goes
// through os.Root, which refuses at open time any path (symlinks included)
// that would resolve outside the granted root. That narrows, but does not
// close, races with a concurrent same-user process.
type Workspace struct {
	root     string
	snapshot *SnapshotGuard
}

// SnapshotGuard lets read tools report that the workspace no longer matches
// the manifest an execution was bound to, so stale context can be refreshed.
type SnapshotGuard struct {
	// Bound is the manifest digest the execution was compiled against.
	Bound string
	// Current computes the workspace's manifest digest now.
	Current func() (string, error)
}

// NewWorkspace binds file tools to root, an existing absolute directory.
// snapshot is optional.
func NewWorkspace(root string, snapshot *SnapshotGuard) (*Workspace, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("workspace root %q must be a clean absolute path", root)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", root)
	}
	if snapshot != nil && (!api.ValidDigest(snapshot.Bound) || snapshot.Current == nil) {
		return nil, errors.New("snapshot guard needs a bound sha256 digest and a Current function")
	}
	return &Workspace{root: root, snapshot: snapshot}, nil
}

// open returns the granted root containing p as an os.Root, the root's
// workspace-relative name, and p relative to it. The caller closes the root.
func (w *Workspace) open(grant api.Capability, p string) (*os.Root, string, string, error) {
	root, ok := rootFor(grant, p)
	if !ok {
		return nil, "", "", fmt.Errorf("path %q is not a clean path inside a granted root", p)
	}
	ws, err := os.OpenRoot(w.root)
	if err != nil {
		return nil, "", "", err
	}
	defer ws.Close()
	if err := refuseSymlinkedRoot(ws, root); err != nil {
		return nil, "", "", err
	}
	gr, err := ws.OpenRoot(root)
	if err != nil {
		return nil, "", "", err
	}
	if err := sameDir(ws, gr, root); err != nil {
		return nil, "", "", errors.Join(err, gr.Close())
	}
	rel := "."
	if root == "." {
		rel = p
	} else if p != root {
		rel = strings.TrimPrefix(p, root+"/")
	}
	return gr, root, rel, nil
}

// refuseSymlinkedRoot refuses a granted root reached through a symlink: the
// grant names a directory, not wherever a link currently points.
func refuseSymlinkedRoot(ws *os.Root, root string) error {
	if root == "." {
		return nil
	}
	parts := strings.Split(root, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, err := ws.Lstat(prefix)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("granted root %q crosses symlink %q", root, prefix)
		}
	}
	return nil
}

// sameDir rechecks, after opening, that the directory held is still the one
// the granted root names, so a swap between check and open is refused.
func sameDir(ws, gr *os.Root, root string) error {
	held, err := gr.Stat(".")
	if err != nil {
		return err
	}
	named, err := ws.Lstat(root)
	if err != nil {
		return err
	}
	if !os.SameFile(held, named) {
		return fmt.Errorf("granted root %q changed while it was opened", root)
	}
	return nil
}

// readRegular reads a regular file inside gr, bounded by MaxFileBytes.
func readRegular(gr *os.Root, rel string) ([]byte, error) {
	f, err := gr.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%q exceeds %d bytes", rel, MaxFileBytes)
	}
	return data, nil
}

// snapshotNote is empty when the workspace still matches the bound manifest.
func (w *Workspace) snapshotNote() string {
	if w.snapshot == nil {
		return ""
	}
	current, err := w.snapshot.Current()
	if err != nil {
		return fmt.Sprintf("note: workspace manifest unknown (%v); content may differ from bound snapshot %s\n",
			err, w.snapshot.Bound)
	}
	if current != w.snapshot.Bound {
		return fmt.Sprintf("note: workspace manifest is %s, not bound snapshot %s; content below is current\n",
			current, w.snapshot.Bound)
	}
	return ""
}

// tool is the one Tool implementation every built-in tool is assembled from.
type tool struct {
	spec   api.ToolSpec
	kind   api.CapabilityKind
	scope  func(json.RawMessage) (Scope, error)
	invoke func(context.Context, Invocation) (api.ToolResult, error)
}

func (t *tool) Spec() api.ToolSpec                                             { return t.spec }
func (t *tool) Kind() api.CapabilityKind                                       { return t.kind }
func (t *tool) Scope(args json.RawMessage) (Scope, error)                      { return t.scope(args) }
func (t *tool) Invoke(c context.Context, i Invocation) (api.ToolResult, error) { return t.invoke(c, i) }

func pathScope(args json.RawMessage) (Scope, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Scope{}, err
	}
	return Scope{Paths: []string{a.Path}}, nil
}

func failed(format string, args ...any) api.ToolResult {
	return api.ToolResult{Status: api.ToolError, Error: fmt.Sprintf(format, args...)}
}
