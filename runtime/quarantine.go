package runtime

// quarantine.go is #390's attempt boundary: material produced by an invocation
// the runtime REFUSED must neither be committed under that invocation nor be
// inherited by the next one.
//
// A failed execution.invoke used to leave its mutation dirty in the candidate
// workspace. The same binding's next physical attempt then ran on top of it,
// and the workspace delta it was judged by - candidateChangedPaths - could not
// tell the refused bytes from its own, so a later success admitted them. The
// refused bytes are therefore moved OUT of the workspace at the moment of
// refusal: copied into a runtime-owned quarantine directory beside the
// candidate (never deleted), journalled as candidate.quarantined, and the
// refused paths restored to the exact governed subject the attempt started
// from. The retry runs against that subject and nothing else.
//
// The quarantine lives OUTSIDE the candidate's .git on purpose: the workspace's
// trusted metadata baseline is adopted only from a SUCCEEDED operation, so a
// ref written by a failing one would trip the next integrity check.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// quarantineDir is where one refused physical attempt's bytes are kept.
// Relative to the state directory, so the journalled location survives a
// moved state directory the same way every other run path does.
func quarantineDir(runID, operationID string, attempt int) string {
	return filepath.Join("runs", runID, "quarantine", quarantineSlug(operationID)+fmt.Sprintf("-attempt-%d", attempt))
}

// quarantineSlug keeps an operation id usable as one path element: ids carry
// '|', ':' and '#', and none of that may introduce a separator.
func quarantineSlug(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if len(slug) > 120 {
		slug = slug[len(slug)-120:]
	}
	return slug
}

// QuarantineManifest is the self-describing record written beside the copied
// bytes, so the quarantine is intelligible without the journal.
type QuarantineManifest struct {
	OperationID   string            `json:"operation_id"`
	Attempt       int               `json:"attempt"`
	Subject       string            `json:"subject"`
	FailureClass  FailureClass      `json:"failure_class,omitempty"`
	ContentDigest string            `json:"content_digest,omitempty"`
	Paths         map[string]string `json:"paths"`
}

// quarantineRefusedMaterial copies every refused path into dest and writes the
// manifest. It only READS the workspace; nothing is restored here, so a copy
// that fails leaves the material exactly where the producer put it.
func quarantineRefusedMaterial(workspace, dest string, manifest QuarantineManifest, paths []string) error {
	files := filepath.Join(dest, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return err
	}
	manifest.Paths = make(map[string]string, len(paths))
	for _, rel := range paths {
		src := filepath.Join(workspace, rel)
		dst := filepath.Join(files, rel)
		info, err := os.Lstat(src)
		switch {
		case errors.Is(err, os.ErrNotExist):
			manifest.Paths[rel] = "deleted"
			continue
		case err != nil:
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
			manifest.Paths[rel] = "symlink"
		case info.Mode().IsRegular():
			if err := copyRegular(src, dst, info.Mode().Perm()); err != nil {
				return err
			}
			manifest.Paths[rel] = "file"
		default:
			// A directory (an untracked tree git reports as one path) is
			// copied whole; anything else is named, not copied.
			if info.IsDir() {
				if err := copyTree(src, dst); err != nil {
					return err
				}
				manifest.Paths[rel] = "directory"
				continue
			}
			manifest.Paths[rel] = "special:" + info.Mode().Type().String()
		}
	}
	document, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "manifest.json"), document, 0o600)
}

func copyRegular(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyRegular(path, target, info.Mode().Perm())
		}
		return nil
	})
}

// restoreRefusedPaths returns exactly the refused paths to the subject the
// attempt started from (HEAD - an invocation never moves it). Tracked paths
// are checked out from HEAD; paths HEAD does not know are removed. Nothing
// outside the refused set is touched, so runtime-owned scratch the commit
// would exclude anyway stays where it is.
func restoreRefusedPaths(workspace string, paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var tracked []string
	for _, rel := range sorted {
		if _, err := gitOutput(workspace, "cat-file", "-e", "HEAD:"+rel); err == nil {
			tracked = append(tracked, rel)
			continue
		}
		if err := os.RemoveAll(filepath.Join(workspace, rel)); err != nil {
			return err
		}
	}
	if len(tracked) > 0 {
		args := append([]string{"checkout", "HEAD", "--"}, tracked...)
		if _, err := runGit(workspace, args...); err != nil {
			return err
		}
	}
	// The index may still stage what the producer added; it is returned to
	// HEAD for the refused paths so the next attempt starts from the subject.
	args := append([]string{"reset", "-q", "HEAD", "--"}, sorted...)
	if _, err := runGit(workspace, args...); err != nil {
		return err
	}
	remaining, err := candidateChangedPaths(workspace)
	if err != nil {
		return err
	}
	for _, rel := range remaining {
		for _, refused := range sorted {
			if rel == refused || strings.HasPrefix(rel, refused+"/") {
				return fmt.Errorf("refused path %q is still changed after restore", rel)
			}
		}
	}
	return nil
}

// settleRefusedMaterial is the single attempt-boundary settlement every exit
// of invokeExecution passes through after the provider returned. It acts only
// on a FAILED settlement that left candidate material behind: a Succeeded
// operation's material is either an admitted execution or a governed
// checkpoint and is committed by candidate.commit, and a cancelled one is a
// stop whose material #203 holds in place.
//
// The material is copied out first and restored second. If either step
// fails, the attempt is re-classified as a stop: the refused bytes are still
// in the workspace, and no retry may be admitted to run on top of them.
func (r *EngineeringRuntime) settleRefusedMaterial(out *effect, state *runState, operation RunOperation, attempt int, workspace, subject string, paths []string) {
	if out.state != OperationFailed || len(paths) == 0 {
		return
	}
	var class FailureClass
	var digest string
	switch rec := out.result.(type) {
	case executionRecord:
		if rec.Checkpoint {
			return
		}
		class, digest = rec.FailureClass, rec.ContentDigest
	case mutationResult:
		class, digest = rec.FailureClass, rec.ContentDigest
	}
	location := quarantineDir(state.run.ID, operation.ID, attempt)
	manifest := QuarantineManifest{
		OperationID: operation.ID, Attempt: attempt, Subject: subject,
		FailureClass: class, ContentDigest: digest,
	}
	err := quarantineRefusedMaterial(workspace, filepath.Join(r.deps.StateDir, location), manifest, paths)
	if err == nil {
		err = restoreRefusedPaths(workspace, paths)
	}
	if err != nil {
		stopRefusedAttempt(out, fmt.Errorf("refused material could not be quarantined at %s: %w", location, err))
		return
	}
	out.events = append(out.events, journalEntry{Type: EventCandidateQuarantined, Payload: CandidateQuarantinedPayload{
		OperationID: operation.ID, Attempt: attempt, Subject: subject, FailureClass: class,
		PathCount: len(paths), PathsDigest: pathsDigest(paths), ContentDigest: digest, Location: location,
	}})
}

// stopRefusedAttempt turns a failed settlement whose refused material could
// not be moved aside into a terminal one, keeping everything else it records.
func stopRefusedAttempt(out *effect, cause error) {
	switch rec := out.result.(type) {
	case executionRecord:
		rec.FailureClass = FailureUnknown
		if rec.Diagnostic != nil {
			rec.Diagnostic.FailureClass = FailureUnknown
			rec.Diagnostic.Route = RouteStop
			rec.Diagnostic.Message = boundedDetail(cause.Error())
		}
		out.result = rec
	case mutationResult:
		rec.FailureClass = FailureUnknown
		out.result = rec
	}
}
