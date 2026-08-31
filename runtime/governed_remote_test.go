package runtime

// Regression proofs for Defect M, from the fifth-generation #32 dogfood
// run-32cbfb2af1134941f507c40ad7d2c74e.
//
// That run produced an assured candidate - exact-tree assurance PASS on
// 33fe93d0b73f9e0d136511494f7dd174b463f6de - and then failed three
// base.integrate attempts in about a second and a half with:
//
//	refused remote "https://github.com/bogdaniel/zenchron-engineering":
//	not the governed remote for this workspace
//
// against a repository whose base had NOT moved. The operator checkout's origin
// carried ".git", the run's candidate origin did not, and GovernedRemote
// accepted both while RemoteIdentity carried the caller's spelling forward, so
// two spellings Zenchron itself calls the same repository became two
// authorities. The refusal was then returned untyped, so the reconciler had no
// route and spent the attempt budget on an answer that could never change.
//
// Nothing here makes a network call.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two exact spellings the real run held at once.
const (
	dogfoodSourceRemote    = "https://github.com/bogdaniel/zenchron-engineering.git"
	dogfoodCandidateRemote = "https://github.com/bogdaniel/zenchron-engineering"
)

// ---------------------------------------------------------------------------
// M1: canonical identity
// ---------------------------------------------------------------------------

// TestGovernedRemoteCanonicalizesTheRealDogfoodSpellings is the direct proof.
func TestGovernedRemoteCanonicalizesTheRealDogfoodSpellings(t *testing.T) {
	source, err := GovernedRemote(dogfoodSourceRemote)
	if err != nil {
		t.Fatalf("the operator checkout's own origin was refused: %v", err)
	}
	candidate, err := GovernedRemote(dogfoodCandidateRemote)
	if err != nil {
		t.Fatalf("the run's candidate origin was refused: %v", err)
	}
	if !source.Same(candidate) || !candidate.Same(source) {
		t.Fatalf("the two spellings of one repository are different authorities: %+v vs %+v", source, candidate)
	}
	// One frozen spelling, derived from the validated identity.
	if source.URL != dogfoodCandidateRemote || candidate.URL != dogfoodCandidateRemote {
		t.Fatalf("canonical URL is %q/%q, want %q", source.URL, candidate.URL, dogfoodCandidateRemote)
	}
	if source.Owner() != "bogdaniel" || source.Name() != "zenchron-engineering" {
		t.Fatalf("canonical identity is %q/%q", source.Owner(), source.Name())
	}
	if source.Transport() != "https" {
		t.Fatalf("transport = %q", source.Transport())
	}
}

// TestGovernedRemoteSecurityMatrix is the full equal/refused matrix. Equality is
// identity equality: the transport and the validated owner/repository, never a
// host string on its own.
func TestGovernedRemoteSecurityMatrix(t *testing.T) {
	base, err := GovernedRemote("https://github.com/o/r")
	if err != nil {
		t.Fatal(err)
	}
	for _, equal := range []string{
		"https://github.com/o/r",
		"https://github.com/o/r.git",
	} {
		got, err := GovernedRemote(equal)
		if err != nil {
			t.Fatalf("%q was refused: %v", equal, err)
		}
		if !got.Same(base) {
			t.Fatalf("%q is not the same authority as https://github.com/o/r", equal)
		}
	}
	// Same host, different repository identity: accepted as a remote, and
	// deliberately NOT the same authority.
	for _, unequal := range []string{
		"https://github.com/o/other",
		"https://github.com/other/r",
	} {
		got, err := GovernedRemote(unequal)
		if err != nil {
			t.Fatalf("%q was refused outright: %v", unequal, err)
		}
		if got.Same(base) {
			t.Fatalf("%q compares equal to a different repository", unequal)
		}
	}
	for _, refused := range []string{
		"http://github.com/o/r",
		"ssh://git@github.com/o/r",
		"git@github.com:o/r.git",
		"git://github.com/o/r",
		"ext::sh -c whoami",
		"file:///tmp/o/r",
		"https://user@github.com/o/r",
		"https://github.com:443/o/r",
		"https://github.com/o/r?x=1",
		"https://github.com/o/r#frag",
		"https://evil.example/o/r",
		"https://github.com/o",
		"https://github.com/o/r/x",
		"relative/path",
		"./relative",
		"../escape",
		"-upstream",
		"",
		"   ",
	} {
		if got, err := GovernedRemote(refused); err == nil {
			t.Fatalf("%q was accepted as %+v", refused, got)
		}
	}
}

// TestGovernedRemoteKeepsLocalRepositoriesLocal proves canonicalization did not
// swallow the filesystem transport the fixtures depend on, and did not make a
// local path GitHub-shaped.
func TestGovernedRemoteKeepsLocalRepositoriesLocal(t *testing.T) {
	dir := t.TempDir()
	identity, err := GovernedRemote(dir)
	if err != nil {
		t.Fatalf("an absolute local repository was refused: %v", err)
	}
	if identity.Transport() != "file" || identity.URL != filepath.Clean(dir) {
		t.Fatalf("local identity is %+v", identity)
	}
	if identity.Owner() != "" || identity.Name() != "" {
		t.Fatalf("a local path was given a GitHub owner/name: %+v", identity)
	}
	// A trailing separator is the same directory and the same authority.
	trailing, err := GovernedRemote(dir + string(filepath.Separator))
	if err != nil || !trailing.Same(identity) {
		t.Fatalf("two spellings of one local repository disagree: %+v vs %+v (%v)", trailing, identity, err)
	}
	// A different directory is a different authority, and a GitHub remote is
	// never the same authority as a local path.
	other := t.TempDir()
	otherIdentity, err := GovernedRemote(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherIdentity.Same(identity) {
		t.Fatal("two different local repositories compare equal")
	}
	github, err := GovernedRemote("https://github.com/o/r")
	if err != nil {
		t.Fatal(err)
	}
	if github.Same(identity) {
		t.Fatal("a GitHub remote compares equal to a local path")
	}
	if _, err := GovernedRemote(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("a non-existent local repository was accepted")
	}
}

// TestWorkspaceAcceptsTheRealDogfoodSpellingPair is the exact condition the run
// hit: a workspace governed by one spelling whose own origin carries the other.
func TestWorkspaceAcceptsTheRealDogfoodSpellingPair(t *testing.T) {
	workspace := CandidateWorkspace{Dir: t.TempDir(), Remote: dogfoodSourceRemote}
	identity, err := workspace.boundRemote(dogfoodCandidateRemote)
	if err != nil {
		t.Fatalf("the real dogfood spelling pair is still refused: %v", err)
	}
	if identity.URL != dogfoodCandidateRemote {
		t.Fatalf("bound remote is %q", identity.URL)
	}
	// And the reverse pairing, because neither spelling is privileged.
	reversed := CandidateWorkspace{Dir: t.TempDir(), Remote: dogfoodCandidateRemote}
	if _, err := reversed.boundRemote(dogfoodSourceRemote); err != nil {
		t.Fatalf("the reversed spelling pair is refused: %v", err)
	}
}

// TestWorkspaceStillRefusesADifferentRepository is the security check the repair
// must not have weakened.
func TestWorkspaceStillRefusesADifferentRepository(t *testing.T) {
	workspace := CandidateWorkspace{Dir: t.TempDir(), Remote: dogfoodSourceRemote}
	for _, foreign := range []string{
		"https://github.com/someone/else",
		"https://github.com/someone/else.git",
		"https://github.com/bogdaniel/other-repository",
		"https://github.com/other-owner/zenchron-engineering",
	} {
		err := workspace.boundRemote2(t, foreign)
		if err == nil {
			t.Fatalf("%q was accepted as the governed remote", foreign)
		}
		var mismatch *GovernedRemoteMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("%q was refused untyped: %v", foreign, err)
		}
		if mismatch.Governed != dogfoodCandidateRemote {
			t.Fatalf("the refusal names governed remote %q", mismatch.Governed)
		}
	}
	// A remote that is not governed at all is still refused by classification,
	// before any identity comparison happens.
	for _, refused := range []string{"ssh://git@github.com/bogdaniel/zenchron-engineering", "https://evil.example/bogdaniel/zenchron-engineering"} {
		if err := workspace.boundRemote2(t, refused); err == nil {
			t.Fatalf("%q was accepted", refused)
		}
	}
}

// boundRemote2 is a readability shim: boundRemote returns two values and every
// assertion above is about the error.
func (w CandidateWorkspace) boundRemote2(t *testing.T, remote string) error {
	t.Helper()
	_, err := w.boundRemote(remote)
	return err
}

// ---------------------------------------------------------------------------
// M2: a deterministic identity refusal is not a retry
// ---------------------------------------------------------------------------

// TestGovernedRemoteMismatchStopsInsteadOfBurningAttempts is the M2 proof
// against the real handler: one attempt, a stop, and no producer remediation.
func TestGovernedRemoteMismatchStopsInsteadOfBurningAttempts(t *testing.T) {
	if got := RouteFailure(FailureGovernedRemoteMismatch); got != RouteStop {
		t.Fatalf("route = %q, want %q", got, RouteStop)
	}
	for _, forbidden := range []FailureRoute{RouteRetry, RouteWait, RouteReassess, RouteProviderRemediation, RouteGofmt, RouteRestore} {
		if RouteFailure(FailureGovernedRemoteMismatch) == forbidden {
			t.Fatalf("a deterministic identity refusal must not route to %q", forbidden)
		}
	}
	for _, wrong := range []FailureClass{FailureTransientInfrastructure, FailureAuthorityWait, FailureCompileTest, FailureVerification} {
		if FailureGovernedRemoteMismatch == wrong {
			t.Fatalf("the identity refusal reused %q", wrong)
		}
	}

	// The refusal is proven where the repair lives: the handler. The run is
	// driven normally until a candidate workspace exists, then the run's
	// governed remote is repointed at a DIFFERENT repository - which is exactly
	// what a spelling mismatch looked like from inside the run: the workspace's
	// own origin is no longer the remote this run is governed by.
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)

	foreign := filepath.Join(fixture.root, "foreign-origin")
	initFixtureRepo(t, foreign, "README.md", "foreign\n")
	identity, err := GovernedRemote(foreign)
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.Remote = identity
	fixture.runtime = fixture.newRuntime(fixture.deps)

	produced := fixture.runtime.integrateBase(context.Background(), fixture.state(runID), RunOperation{Kind: OpBaseIntegrate})
	if produced.state != OperationFailed {
		t.Fatalf("a governed-remote mismatch did not fail the operation: %+v", produced)
	}
	result, ok := produced.result.(baseIntegrateResult)
	if !ok {
		t.Fatalf("the handler returned an untyped result: %#v", produced.result)
	}
	if result.FailureClass != FailureGovernedRemoteMismatch {
		t.Fatalf("recorded class = %q, want %q", result.FailureClass, FailureGovernedRemoteMismatch)
	}
	if result.Moved || result.Strategy != "" {
		t.Fatalf("a refused integration claimed work: %+v", result)
	}
	if len(produced.events) != 0 {
		t.Fatalf("a refused integration journalled events: %+v", produced.events)
	}

	// And that recorded class is what stops attempts 2 and 3: the reconciler
	// re-attempts an operation only when its last recorded failure routes to a
	// retry or a wait, and this one routes to neither.
	if reattemptable(RouteFailure(result.FailureClass)) {
		t.Fatal("a governed-remote mismatch is reattemptable, so it would spend the budget again")
	}
	// The refusal itself is typed all the way down at the boundary.
	workspace, err := fixture.runtime.workspace(fixture.state(runID))
	if err != nil {
		t.Fatal(err)
	}
	var mismatch *GovernedRemoteMismatchError
	if _, err := workspace.boundRemote("origin"); !errors.As(err, &mismatch) {
		t.Fatalf("the boundary refusal is not typed: %v", err)
	}
}

// baseIntegrateAttempts counts started attempts per base.integrate OPERATION.
// The binding deliberately changes around a publication, so a run legitimately
// has several base.integrate operations; what must never happen is one of them
// being attempted more than once for a deterministic refusal.
func baseIntegrateAttempts(t *testing.T, events []EngineeringEvent) map[string]int {
	t.Helper()
	attempts := map[string]int{}
	for _, e := range events {
		if e.Type != EventOperationBefore {
			continue
		}
		var op RunOperation
		if json.Unmarshal(e.Payload, &op) == nil && op.Kind == OpBaseIntegrate {
			attempts[op.IdempotencyKey]++
		}
	}
	return attempts
}

// TestBaseIntegrateSucceedsWithoutDriftOrRebase is the no-drift acceptance: the
// exact condition the dogfood run was in when it was refused. The base has not
// moved, so the fetch happens, the observation matches, and nothing is rebased,
// merged or moved.
func TestBaseIntegrateSucceedsWithoutDriftOrRebase(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)

	state := fixture.state(runID)
	base := state.baseRevision()
	candidateBefore, treeBefore := state.projection.CandidateRevision, state.projection.CandidateTree
	if base == "" || candidateBefore == "" {
		t.Fatalf("the fixture produced no candidate to integrate: %+v", state.projection)
	}
	originMain, err := gitOutput(fixture.origin, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(originMain) != base {
		t.Fatalf("this proof needs no drift: origin/main %s vs base %s", strings.TrimSpace(originMain), base)
	}
	assurance := state.projection.Assurance
	if assurance == nil || !assurance.Passed {
		t.Fatalf("this proof needs passing assurance: %#v", assurance)
	}

	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventCandidateBaseIntegrated) != 0 {
		t.Fatalf("a no-drift integration rebased or merged: %v", journalTypes(events))
	}
	after := fixture.state(runID)
	if after.projection.CandidateRevision != candidateBefore || after.projection.CandidateTree != treeBefore {
		t.Fatalf("a no-drift integration moved the candidate: %s/%s -> %s/%s",
			candidateBefore, treeBefore, after.projection.CandidateRevision, after.projection.CandidateTree)
	}
	// It succeeded, exactly once, and refreshed the trusted metadata baseline.
	key, wanted := bindBaseIntegrate(after)
	if !wanted {
		t.Fatal("base integration is not bound for an assured candidate")
	}
	if !after.satisfied(OpBaseIntegrate, key) {
		t.Fatal("a no-drift base integration did not succeed")
	}
	for key, attempts := range baseIntegrateAttempts(t, events) {
		if attempts != 1 {
			t.Fatalf("a no-drift base integration used %d attempts for %q", attempts, key)
		}
	}
	if after.projection.CandidateMetadata == "" {
		t.Fatal("the trusted metadata baseline was not refreshed after the governed fetch")
	}
}

// TestCandidateCloneRecordsTheCanonicalRemote closes the loop: a workspace
// created from either spelling records the canonical one, so the mismatch
// cannot be reintroduced by whichever spelling a caller happened to pass.
func TestCandidateCloneRecordsTheCanonicalRemote(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	base := initFixtureRepo(t, origin, "README.md", "base\n")
	state := t.TempDir()
	workspace, err := CreateCandidateClone(state, "run-canonical", origin+string(filepath.Separator), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Remote != filepath.Clean(origin) {
		t.Fatalf("the workspace recorded %q, want the canonical %q", workspace.Remote, filepath.Clean(origin))
	}
	if _, err := os.Stat(workspace.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.boundRemote("origin"); err != nil {
		t.Fatalf("a workspace cannot bind its own origin: %v", err)
	}
}
