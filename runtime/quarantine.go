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
	// The manifest is the completeness marker restart adoption trusts, so it
	// appears atomically: a crash mid-write leaves no manifest, never a torn
	// one.
	temporary := filepath.Join(dest, ".manifest.json.tmp")
	if err := os.WriteFile(temporary, document, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dest, "manifest.json"))
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
// checkpoint and is committed by candidate.commit, and an attempt an operator
// stop ended (interrupted, or classified run_cancelled) is #203's to hold in
// place.
//
// ORDER IS THE SAFETY ARGUMENT.
//
//  1. Copy. Nothing in the workspace is touched; a failed copy leaves every
//     refused byte where it was and the attempt becomes a stop.
//  2. Journal. A complete copy gets its durable identity BEFORE the workspace
//     is modified, so a restore that fails half way can never orphan it: the
//     candidate.quarantined event travels with this operation's outcome
//     whether or not the restore below succeeds.
//  3. Restore. On failure the attempt becomes a stop - the record says the
//     copy is complete and the restore is not, and no retry may run on a
//     workspace that may still hold part of the refused material.
func (r *EngineeringRuntime) settleRefusedMaterial(out *effect, state *runState, operation RunOperation, attempt int, workspace, subject string, paths []string) {
	if out.state != OperationFailed || out.interrupted || len(paths) == 0 {
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
	if class == FailureRunCancelled {
		return
	}
	location := quarantineDir(state.run.ID, operation.ID, attempt)
	manifest := QuarantineManifest{
		OperationID: operation.ID, Attempt: attempt, Subject: subject,
		FailureClass: class, ContentDigest: digest,
	}
	if err := quarantineRefusedMaterial(workspace, filepath.Join(r.deps.StateDir, location), manifest, paths); err != nil {
		stopRefusedAttempt(out, fmt.Errorf("refused material could not be copied to quarantine %s; it is still in the workspace: %w", location, err))
		return
	}
	restoreErr := restoreRefusedPaths(workspace, paths)
	out.events = append(out.events, journalEntry{Type: EventCandidateQuarantined, Payload: CandidateQuarantinedPayload{
		OperationID: operation.ID, Attempt: attempt, Subject: subject, FailureClass: class,
		PathCount: len(paths), PathsDigest: pathsDigest(paths), ContentDigest: digest, Location: location,
		Restored: restoreErr == nil,
	}})
	if restoreErr != nil {
		stopRefusedAttempt(out, fmt.Errorf("refused material is preserved in quarantine %s but the workspace could not be restored to the attempt's subject: %w", location, restoreErr))
	}
}

// stopRefusedAttempt makes a failed settlement whose refused material could
// not be fully moved aside TERMINAL, whatever result shape the failing path
// produced. It does not depend on recognising the result type: any result is
// replaced by an executionRecord carrying FailureUnknown (which routes to
// stop), with the original result kept, bounded, in the diagnostic message,
// so no retry can be admitted onto a workspace that may still hold it.
func stopRefusedAttempt(out *effect, cause error) {
	record := executionRecord{}
	original := ""
	switch rec := out.result.(type) {
	case executionRecord:
		record = rec
		if rec.Diagnostic != nil {
			original = rec.Diagnostic.Message
		}
	case mutationResult:
		record.mutationResult = rec
	default:
		if encoded, err := json.Marshal(rec); err == nil {
			original = string(encoded)
		}
	}
	record.FailureClass = FailureUnknown
	record.Checkpoint = false
	message := cause.Error()
	if original != "" {
		message += "; original result: " + original
	}
	diagnostic := ExecutionDiagnostic{Stage: execStageCandidateAdmission}
	if record.Diagnostic != nil {
		diagnostic = *record.Diagnostic
	}
	diagnostic.FailureClass = FailureUnknown
	diagnostic.Route = RouteStop
	diagnostic.Message = boundedDetail(message)
	record.Diagnostic = &diagnostic
	out.state = OperationFailed
	out.result = record
}

// adoptOrphanQuarantines finds every COMPLETE quarantine copy under this run
// that no candidate.quarantined event names, and gives it its durable
// identity. Completeness is the manifest: quarantineRefusedMaterial writes it
// last, so a directory without one is a copy the crash interrupted, and the
// workspace was never touched for it. For each orphan the refused paths are
// restored again - restoreRefusedPaths is idempotent, so paths already
// restored are simply confirmed - and an event is returned recording whether
// that succeeded. Any restore failure is returned as an error alongside the
// events already produced, so the caller can journal them and stop.
//
// ADOPTION IS BOUND TO THIS DISPATCH. Only a quarantine of THIS operation,
// from an EARLIER physical attempt, at exactly the location that operation
// and attempt derive, is adopted. A manifest anywhere else - another
// operation's, a later or equal attempt's, or a directory whose name does not
// match what it claims - is not this dispatch's crashed predecessor and is
// left alone: a manifest alone grants nothing.
func (r *EngineeringRuntime) adoptOrphanQuarantines(state *runState, operation RunOperation, attempt int, workspace string) ([]journalEntry, error) {
	root := filepath.Join(r.deps.StateDir, "runs", state.run.ID, "quarantine")
	// No quarantine directory means no copy was ever completed here; a root
	// that is not a directory cannot hold one either.
	if info, err := os.Stat(root); errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, e := range state.events {
		if e.Type != EventCandidateQuarantined {
			continue
		}
		var p CandidateQuarantinedPayload
		if json.Unmarshal(e.Payload, &p) == nil {
			known[p.Location] = true
		}
	}
	var events []journalEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		location := filepath.Join("runs", state.run.ID, "quarantine", entry.Name())
		if known[location] {
			continue
		}
		document, err := os.ReadFile(filepath.Join(r.deps.StateDir, location, "manifest.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return events, err
		}
		var manifest QuarantineManifest
		if err := json.Unmarshal(document, &manifest); err != nil {
			return events, fmt.Errorf("quarantine %s has an unreadable manifest: %w", location, err)
		}
		if manifest.OperationID != operation.ID || manifest.Attempt >= attempt ||
			quarantineDir(state.run.ID, manifest.OperationID, manifest.Attempt) != location {
			continue
		}
		paths := make([]string, 0, len(manifest.Paths))
		for path := range manifest.Paths {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		restoreErr := restoreRefusedPaths(workspace, paths)
		events = append(events, journalEntry{Type: EventCandidateQuarantined, Payload: CandidateQuarantinedPayload{
			OperationID: manifest.OperationID, Attempt: manifest.Attempt, Subject: manifest.Subject,
			FailureClass: manifest.FailureClass, PathCount: len(paths), PathsDigest: pathsDigest(paths),
			ContentDigest: manifest.ContentDigest, Location: location,
			Restored: restoreErr == nil, Adopted: true,
		}})
		if restoreErr != nil {
			return events, fmt.Errorf("orphaned quarantine %s could not be restored out of the workspace: %w", location, restoreErr)
		}
	}
	return events, nil
}
