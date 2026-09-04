package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A provider transcript used to be keyed by run alone, so attempt 2 of an
// operation overwrote attempt 1 and the history of a bounded retry could not
// be read back at all. These tests pin the identity that replaces it: which
// run, which authorizing operation, which scheduler attempt - and prove that
// evidence already written is never replaced.

func attemptRef(run, operation string, attempt int) ExecutionAttemptRef {
	return ExecutionAttemptRef{RunID: run, OperationID: operation, Attempt: attempt}
}

// executionOperationID is the shape the reconciler actually produces, punctuation
// and all. Using a sanitised stand-in would test a path the runtime never asks
// this code to encode.
func executionOperationID(run, binding string) string {
	return run + ":" + OpExecutionInvoke + ":" + operationKey(OpExecutionInvoke, binding)
}

func rawPath(t *testing.T, artifacts []Artifact) string {
	t.Helper()
	for _, a := range artifacts {
		if !a.Sanitized {
			return a.Path
		}
	}
	t.Fatalf("no raw artifact in %#v", artifacts)
	return ""
}

func sanitizedPath(t *testing.T, artifacts []Artifact) string {
	t.Helper()
	for _, a := range artifacts {
		if a.Sanitized {
			return a.Path
		}
	}
	t.Fatalf("no sanitized artifact in %#v", artifacts)
	return ""
}

// TestAttemptsOfOneOperationAddressDistinctTranscripts is acceptance A: the
// defect itself. Three attempts of ONE operation are three separate pieces of
// evidence, and writing a later one leaves the earlier ones exactly as they
// were.
func TestAttemptsOfOneOperationAddressDistinctTranscripts(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	operation := executionOperationID("run-a", "continuation|abc123")

	first, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-a", operation, 1), []byte("attempt one\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw := rawPath(t, first)
	firstBytes, err := os.ReadFile(firstRaw)
	if err != nil {
		t.Fatal(err)
	}

	second, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-a", operation, 2), []byte("attempt two\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw := rawPath(t, second)
	if secondRaw == firstRaw {
		t.Fatalf("attempt 2 addressed attempt 1's transcript: %s", secondRaw)
	}

	// Acceptance A, the part that matters: attempt 1 is byte-for-byte intact.
	after, err := os.ReadFile(firstRaw)
	if err != nil {
		t.Fatalf("attempt 1's transcript is gone after attempt 2: %v", err)
	}
	if string(after) != string(firstBytes) || string(after) != "attempt one\n" {
		t.Fatalf("attempt 2 rewrote attempt 1: %q", after)
	}

	third, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-a", operation, 3), []byte("attempt three\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	thirdRaw := rawPath(t, third)
	for name, other := range map[string]string{"attempt 1": firstRaw, "attempt 2": secondRaw} {
		if thirdRaw == other {
			t.Fatalf("attempt 3 collided with %s", name)
		}
	}
	// The sanitized halves are separated by the same identity, not just the raw.
	if sanitizedPath(t, first) == sanitizedPath(t, second) {
		t.Fatal("two attempts shared one sanitized transcript")
	}
}

// TestDistinctBindingsGetDistinctOperationNamespaces is acceptance B. A
// continuation on a new candidate revision is a different operation, so its
// attempt 1 must not land on the previous binding's attempt 1.
func TestDistinctBindingsGetDistinctOperationNamespaces(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	first := executionOperationID("run-b", "continuation|checkpoint-one")
	second := executionOperationID("run-b", "continuation|checkpoint-two")

	a, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-b", first, 1), []byte("first binding\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-b", second, 1), []byte("second binding\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rawPath(t, a) == rawPath(t, b) {
		t.Fatal("two execution bindings shared one attempt namespace")
	}
	if body, err := os.ReadFile(rawPath(t, a)); err != nil || string(body) != "first binding\n" {
		t.Fatalf("the first binding's evidence changed: %v %q", err, body)
	}
	// Different runs are separated too, even at the same binding and attempt.
	other, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-c", first, 1), []byte("other run\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rawPath(t, other) == rawPath(t, a) {
		t.Fatal("two runs shared one attempt namespace")
	}
}

// TestRestartCannotOverwriteDurableAttemptEvidence is acceptance D. A
// controller that died between writing a transcript and journalling it must be
// able to finish reporting, but must never be able to replace what a previous
// attempt actually said.
func TestRestartCannotOverwriteDurableAttemptEvidence(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	ref := attemptRef("run-d", executionOperationID("run-d", "initial|1|base"), 1)
	body := []byte("the invocation that happened\n")

	first, err := store.StoreExecutionAttemptTranscript("openai-responses", ref, body, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Re-persisting the SAME bytes is idempotent: a restarted controller
	// re-reporting work it already did is not new evidence.
	repeat, err := store.StoreExecutionAttemptTranscript("openai-responses", ref, body, nil)
	if err != nil {
		t.Fatalf("an identical re-publication was refused: %v", err)
	}
	if rawPath(t, repeat) != rawPath(t, first) {
		t.Fatal("an idempotent re-publication moved the artifact")
	}

	// Different bytes under the same identity are refused, not merged and not
	// overwritten. Exactly one invocation produced this identity.
	_, err = store.StoreExecutionAttemptTranscript("openai-responses", ref, []byte("a different story\n"), nil)
	if err == nil {
		t.Fatal("durable attempt evidence was silently replaced")
	}
	var conflict *TranscriptConflictError
	if !asTranscriptConflict(err, &conflict) {
		t.Fatalf("overwrite refusal was not typed: %#v", err)
	}
	surviving, readErr := os.ReadFile(rawPath(t, first))
	if readErr != nil || string(surviving) != string(body) {
		t.Fatalf("the refused write still damaged the transcript: %v %q", readErr, surviving)
	}
}

// TestAttemptIdentityIsRefusedWhenIncomplete is the plumbing guard. Reaching
// the store without a scheduler attempt is a wiring defect in this runtime, and
// it is named as one rather than written under a guessed identity.
func TestAttemptIdentityIsRefusedWhenIncomplete(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	operation := executionOperationID("run-e", "initial|1|base")
	for name, ref := range map[string]ExecutionAttemptRef{
		"no run":       attemptRef("", operation, 1),
		"no operation": attemptRef("run-e", "", 1),
		"zero attempt": attemptRef("run-e", operation, 0),
		"negative":     attemptRef("run-e", operation, -1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.StoreExecutionAttemptTranscript("openai-responses", ref, []byte("x"), nil); err == nil {
				t.Fatal("an incomplete attempt identity was accepted")
			}
		})
	}
	if _, err := store.StoreExecutionAttemptTranscript("", attemptRef("run-e", operation, 1), []byte("x"), nil); err == nil {
		t.Fatal("a transcript with no provider identity was accepted")
	}
}

// TestAttemptTranscriptPathsAreSafeAndLegible proves the encoding: an operation
// id carries ':', '#' and '|', and a hostile-looking component must not be able
// to leave the artifact root.
func TestAttemptTranscriptPathsAreSafeAndLegible(t *testing.T) {
	root := t.TempDir()
	store := ArtifactStore{Root: root}
	operation := executionOperationID("run-f", "continuation|../../escape")

	artifacts, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-f", operation, 2), []byte("bounded\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := rawPath(t, artifacts)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
		t.Fatalf("an operation id escaped the artifact root: %s", path)
	}
	// The property is that each identity becomes ONE path component: no
	// separator, and never "." or "..". Dots inside a longer name are ordinary
	// characters and are left legible on purpose.
	for _, component := range []string{
		encodePathComponent(operation),
		encodePathComponent("../.."),
		encodePathComponent(".."),
		encodePathComponent("."),
		encodePathComponent("a/b"),
		encodePathComponent("a\\b"),
		encodePathComponent("nul\x00byte"),
	} {
		if component == "." || component == ".." || component != filepath.Base(component) ||
			strings.ContainsAny(component, "/\\\x00") {
			t.Fatalf("encoding produced an unsafe path component: %q", component)
		}
	}
	// Legible enough to find by hand: the run, the attempt and the provider are
	// all still readable in the path.
	for _, want := range []string{"provider", "openai-responses", "run-f", "attempt-2"} {
		if !strings.Contains(path, want) {
			t.Fatalf("path %q lost %q", path, want)
		}
	}
}

// TestEveryExecutionRequestProducerSuppliesTheSchedulerAttempt is acceptance H.
//
// Provider-side validation alone would only discover a missing constructor
// after a run had already started - which is exactly how a previous attempt at
// this change turned a typed provider-account WAIT into a terminal failure. So
// the guard is here, in the source, where a newly added producer fails the
// suite instead of a run.
func TestEveryExecutionRequestProducerSuppliesTheSchedulerAttempt(t *testing.T) {
	root := repositoryRootForTest(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "fixtures" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		// This test names the pattern it looks for; finding itself is noise.
		if entry.Name() == "execution_attempt_transcript_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, literal := range executionRequestLiterals(string(data)) {
			// A zero value states nothing and cannot omit a field.
			if strings.TrimSpace(literal) == "" {
				continue
			}
			if !strings.Contains(literal, "Attempt:") {
				offenders = append(offenders, rel+": "+firstLine(literal))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these ExecutionRequest producers do not supply the scheduler attempt, so their transcripts would have no durable identity:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// executionRequestLiterals returns the body of every ExecutionRequest composite
// literal in a file, matching braces so a nested literal does not end it early.
func executionRequestLiterals(source string) []string {
	var bodies []string
	for _, marker := range []string{"ExecutionRequest{", "runtime.ExecutionRequest{"} {
		from := 0
		for {
			at := strings.Index(source[from:], marker)
			if at < 0 {
				break
			}
			start := from + at + len(marker)
			depth := 1
			end := start
			for end < len(source) && depth > 0 {
				switch source[end] {
				case '{':
					depth++
				case '}':
					depth--
				}
				end++
			}
			// A map of requests keyed by name has its literals inside; those
			// are found by the same scan on the next iteration.
			bodies = append(bodies, source[start:min(end, len(source))])
			from = start
		}
	}
	return bodies
}

func firstLine(body string) string {
	line := strings.TrimSpace(body)
	if at := strings.IndexByte(line, '\n'); at >= 0 {
		line = line[:at]
	}
	if len(line) > 90 {
		line = line[:90]
	}
	return line
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// repositoryRootForTest walks up to the module root so the source guard reads
// the whole repository rather than one package.
func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}

func asTranscriptConflict(err error, target **TranscriptConflictError) bool {
	for err != nil {
		if conflict, ok := err.(*TranscriptConflictError); ok {
			*target = conflict
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
