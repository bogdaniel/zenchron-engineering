package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// granted roots and .git, no component of the granted root or of the path
// below it may be a symlink, a regular file with more than one hard link is
// refused (unix only), every open goes through os.Root, and an opened file is
// rechecked to be the one its name still holds. A concurrent same-user process
// that swaps a parent directory for a link between the component check and the
// open is not excluded: the window is narrowed, not closed.
type Workspace struct {
	root string
}

// NewWorkspace binds file tools to root, an existing absolute directory.
func NewWorkspace(root string) (*Workspace, error) {
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
	return &Workspace{root: root}, nil
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
	if err := refuseSymlinks(ws, root); err != nil {
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

// refuseSymlinks Lstats every component of p inside r and refuses a symlink
// in any of them: a grant names directories and files, not wherever a link
// points, and os.Root alone follows links that stay inside it (a link "meta"
// to .git would otherwise alias repository metadata). A missing component
// stops the walk with an fs.ErrNotExist error once every component before it
// has passed.
func refuseSymlinks(r *os.Root, p string) error {
	if p == "." {
		return nil
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, err := r.Lstat(prefix)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q crosses symlink %q", p, prefix)
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

// readRegular reads a regular file inside gr, bounded by MaxFileBytes, with
// no symlink in its path and no other hard link to it.
func readRegular(gr *os.Root, rel string) ([]byte, error) {
	if err := refuseSymlinks(gr, rel); err != nil {
		return nil, err
	}
	f, err := gr.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkOpened(gr, rel, info); err != nil {
		return nil, err
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

// checkOpened refuses an opened file that is not a singly linked regular file
// still named by rel: a final component swapped for a link between
// refuseSymlinks and the open no longer matches its own Lstat.
func checkOpened(gr *os.Root, rel string, opened fs.FileInfo) error {
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", rel)
	}
	named, err := gr.Lstat(rel)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, named) {
		return fmt.Errorf("%q changed while it was opened", rel)
	}
	if hardLinked(opened) {
		return fmt.Errorf("%q has another hard link; it may alias a file outside the grant", rel)
	}
	return nil
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
