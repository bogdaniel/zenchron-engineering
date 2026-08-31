package runtime

// Regression proofs for the fifth #32 dogfood, run-3287cd59a3781945724dbe59bacf0466.
//
// G and H worked. The producer left a real checkpoint (README.md,
// cmd/zenchron-engineering/selfhost.go and its test, 21 insertions and 2
// deletions, no go.mod change), the continuation was exact-bound, and assurance
// was attempted three times against the same exact commit/tree/contract and
// bounded out - no dead end.
//
// What failed was underneath all of it. The configured cache held 0 bytes, and
// inside the configured image, under the runtime's own container environment,
// `go` did not exist:
//
//	go: not found
//	offline preparation exits 127
//
// L1: dockerBase pinned a PATH without the pinned image's toolchain.
// L2: doctor called Docker readiness "dependency preparation" and reported
//     FAIL=0 over an empty cache and an unusable toolchain.
// L3: every preparation failure became transient_infrastructure, so one
//     deterministic environment fault spent all three assurance attempts in
//     seconds.
//
// Every test here uses fake executors. No container starts, no module is
// downloaded, and no provider is called.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// L1: the pinned toolchain is reachable, and reachable identically everywhere
// ---------------------------------------------------------------------------

// TestSandboxPathExposesThePinnedGoToolchain is the direct L1 proof against the
// constant every dockerBase caller shares.
func TestSandboxPathExposesThePinnedGoToolchain(t *testing.T) {
	if !strings.Contains(sandboxPATH, "/usr/local/go/bin") {
		t.Fatalf("the sandbox path does not expose the pinned image's toolchain: %s", sandboxPATH)
	}
	for _, required := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		if !strings.Contains(sandboxPATH, required) {
			t.Fatalf("the sandbox path dropped %s: %s", required, sandboxPATH)
		}
	}
	// The host's PATH is never forwarded, whatever it happens to be.
	t.Setenv("PATH", "/host-only-poison:"+os.Getenv("PATH"))
	args := strings.Join(dockerBase("/candidate-probe", true), " ")
	if !strings.Contains(args, "--env PATH="+sandboxPATH) {
		t.Fatalf("dockerBase did not state the runtime-owned path: %s", args)
	}
	if strings.Contains(args, "host-only-poison") {
		t.Fatalf("dockerBase inherited the host PATH: %s", args)
	}
}

// TestBrokerAndVerifierResolveExecutablesIdentically is the consistency
// requirement: one image, one resolution rule, whoever is running.
func TestBrokerAndVerifierResolveExecutablesIdentically(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, "download"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module x\ngo 1.25\n"), 0600); err != nil {
		t.Fatal(err)
	}

	verifierFake := &fakeCommandExecutor{found: true}
	verifier := BaselineGoVerifier{
		Sandbox:            DockerSandbox{Image: "sha256:image", Executor: verifierFake},
		ArtifactStore:      ArtifactStore{Root: filepath.Join(root, "artifacts")},
		DependencyCacheDir: cache,
	}
	if err := verifier.prepare(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}

	base, _ := toolBrokerFixture(t)
	brokerFake := &fakeCommandExecutor{found: true}
	broker := ToolBroker{CandidateDir: base.CandidateDir, Sandbox: DockerSandbox{
		Image: "sha256:image", Executor: brokerFake,
		OperationID: "broker-go-probe", StateDir: filepath.Join(root, "broker-state"),
	}}
	if _, err := broker.RunCommand(context.Background(), []string{"go", "version"}); err != nil {
		t.Fatalf("a bounded Go command was refused by the broker: %v", err)
	}

	verifierText, brokerText := commandText(verifierFake.calls), commandText(brokerFake.calls)
	for _, text := range []string{verifierText, brokerText} {
		if !strings.Contains(text, "PATH="+sandboxPATH) {
			t.Fatalf("a dockerBase caller does not use the one runtime-owned path: %s", text)
		}
	}
	// candidate.run really carried the Go argv into the sandbox.
	if !strings.Contains(brokerText, "go version") {
		t.Fatalf("candidate.run did not execute the bounded Go command: %s", brokerText)
	}
	// And the boundary is unchanged for both.
	for _, text := range []string{verifierText, brokerText} {
		for _, required := range []string{"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges"} {
			if !strings.Contains(text, required) {
				t.Fatalf("the sandbox boundary lost %q: %s", required, text)
			}
		}
	}
}

// TestAssuranceRunsTheToolchainOfflineAndReadOnly proves the verification
// command itself: the trusted cache is read-only, the network is off, and the
// toolchain cannot be swapped by the tree being judged.
func TestAssuranceRunsTheToolchainOfflineAndReadOnly(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, "download"), 0700); err != nil {
		t.Fatal(err)
	}
	args := dockerBase(filepath.Join(root, "checkout"), true)
	args = append(args, goModuleCacheMount(cache), "--workdir", "/candidate")
	args = append(args, envArgs(append([]string{"GOMODCACHE=/cache"}, sandboxGoEnv...)...)...)
	text := strings.Join(args, " ")
	for _, want := range []string{
		"--network none", "dst=/cache,readonly", "GOMODCACHE=/cache",
		"GOPROXY=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "PATH=" + sandboxPATH,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the offline assurance environment is missing %q: %s", want, text)
		}
	}
}

// ---------------------------------------------------------------------------
// L2: doctor proves the toolchain and the cache, and never aliases Docker
// ---------------------------------------------------------------------------

// TestDoctorFailsWhenThePinnedImageCannotRunGo is the exact fifth-dogfood
// environment: a healthy daemon, the pinned image present, containers starting
// - and no toolchain. It used to report FAIL=0.
func TestDoctorFailsWhenThePinnedImageCannotRunGo(t *testing.T) {
	f := newDoctorFixture(t)
	broken := doctorExecutor{available: true, toolchainMissing: true}
	f.input.Sandbox = DockerSandbox{Image: doctorImage, Executor: broken}
	f.input.Codex = NativeCodexProvider{Executor: broken}
	report := f.run()

	requireCheck(t, report, "assurance.toolchain", DoctorFail, "did not resolve the Go toolchain", sandboxPATH)
	// The Docker-level checks still pass, which is exactly why the new check
	// had to exist: readiness of the daemon is not readiness of the verifier.
	requireCheck(t, report, "assurance.verifier_sandbox", DoctorPass)
	requireCheck(t, report, "assurance.dependency_preparation", DoctorFail)
	if report.Status != DoctorFail {
		t.Fatalf("doctor reported %q over an image that cannot run the verifier", report.Status)
	}
}

// TestDoctorFailsOnAnUnprovisionedDependencyCache covers every shape of the
// false readiness claim. An empty cache is the one the fifth dogfood had.
func TestDoctorFailsOnAnUnprovisionedDependencyCache(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, f *doctorFixture) string
		reason  string
	}{
		"empty": {
			prepare: func(t *testing.T, f *doctorFixture) string {
				empty := filepath.Join(f.root, "empty-cache")
				if err := os.MkdirAll(empty, 0700); err != nil {
					t.Fatal(err)
				}
				return empty
			},
			reason: "is EMPTY",
		},
		"absent": {
			prepare: func(t *testing.T, f *doctorFixture) string {
				return filepath.Join(f.root, "not-created")
			},
			reason: "cannot be inspected",
		},
		"not a directory": {
			prepare: func(t *testing.T, f *doctorFixture) string {
				file := filepath.Join(f.root, "cache-file")
				if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				return file
			},
			reason: "is not a directory",
		},
		"unconfigured": {
			prepare: func(*testing.T, *doctorFixture) string { return "" },
			reason:  "no assurance.dependency_cache_dir is configured",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDoctorFixture(t)
			f.input.DependencyCacheDir = tc.prepare(t, f)
			report := f.run()
			requireCheck(t, report, "assurance.dependency_cache", DoctorFail, tc.reason)
			if report.Status != DoctorFail {
				t.Fatalf("doctor reported %q over an unprovisioned cache", report.Status)
			}
		})
	}
}

// TestDoctorCacheAndToolchainChecksAreNotDockerAliases states the anti-alias
// requirement directly: the two new checks must be able to disagree with the
// Docker checks, and their reasons must say what was actually proven.
func TestDoctorCacheAndToolchainChecksAreNotDockerAliases(t *testing.T) {
	f := newDoctorFixture(t)
	healthy := f.run()
	toolchain := requireCheck(t, healthy, "assurance.toolchain", DoctorPass, "resolves go and gofmt", "no network")
	cache := requireCheck(t, healthy, "assurance.dependency_cache", DoctorPass, "provisioned", "cache/download module tree")
	docker := requireCheck(t, healthy, "assurance.verifier_sandbox", DoctorPass)
	if toolchain.Reason == docker.Reason || cache.Reason == docker.Reason {
		t.Fatal("a new assurance check restates the Docker readiness reason")
	}

	// Same daemon, same image, no toolchain: the Docker check holds, the
	// toolchain check does not. An alias could not do that.
	broken := newDoctorFixture(t)
	broken.input.Sandbox = DockerSandbox{Image: doctorImage, Executor: doctorExecutor{available: true, toolchainMissing: true}}
	report := broken.run()
	if got, _ := report.Check("assurance.verifier_sandbox"); got.Status != DoctorPass {
		t.Fatalf("the Docker check moved with the toolchain: %+v", got)
	}
	if got, _ := report.Check("assurance.toolchain"); got.Status != DoctorFail {
		t.Fatalf("the toolchain check did not move independently: %+v", got)
	}
}

// TestDoctorToolchainProbeIsReadOnly proves the probe cannot be the thing that
// breaks the guarantees it is checking: no network, no cache, no candidate, no
// download.
func TestDoctorToolchainProbeIsReadOnly(t *testing.T) {
	f := newDoctorFixture(t)
	fake := &fakeCommandExecutor{found: true, outputs: []CommandOutput{
		{Stdout: []byte("27.1.1\n")}, {Stdout: []byte(doctorImage + "\n")},
		{}, {Stdout: []byte("/usr/local/go/bin/go\n/usr/local/go/bin/gofmt\ngo version go1.25.14 linux/arm64\n")},
	}}
	sandbox := DockerSandbox{Image: doctorImage, Executor: fake}
	if _, err := sandbox.ProbeToolchain(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	text := commandText(fake.calls)
	if !strings.Contains(text, "--network none") {
		t.Fatalf("the probe ran with networking: %s", text)
	}
	if strings.Contains(text, f.cacheDir) || strings.Contains(text, "dst=/cache") {
		t.Fatalf("the probe mounted the dependency cache: %s", text)
	}
	for _, forbidden := range []string{"go mod download", "go get", "go install"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("the probe ran %q, which is not read-only: %s", forbidden, text)
		}
	}
}

// ---------------------------------------------------------------------------
// L3: a deterministic prerequisite waits; only a real transient retries
// ---------------------------------------------------------------------------

// TestAssurancePrerequisiteWaitsInsteadOfBurningRetries is the L3 routing proof.
func TestAssurancePrerequisiteWaitsInsteadOfBurningRetries(t *testing.T) {
	if got := RouteFailure(FailureAssurancePrerequisite); got != RouteWait {
		t.Fatalf("route = %q, want %q", got, RouteWait)
	}
	for _, forbidden := range []FailureRoute{RouteRetry, RouteStop, RouteReassess, RouteProviderRemediation, RouteGofmt, RouteRestore} {
		if RouteFailure(FailureAssurancePrerequisite) == forbidden {
			t.Fatalf("a deterministic prerequisite must not route to %q", forbidden)
		}
	}
	if FailureAssurancePrerequisite == FailureAuthorityWait {
		t.Fatal("an environment prerequisite must not reuse the human-authority wait")
	}
	if FailureAssurancePrerequisite == FailureVerification {
		t.Fatal("a prerequisite failure must not be a candidate verdict")
	}
	if waitReason(FailureAssurancePrerequisite) != "assurance_dependency_unavailable" {
		t.Fatalf("wait reason = %q", waitReason(FailureAssurancePrerequisite))
	}
	// Genuinely transient infrastructure keeps its retry.
	if RouteFailure(FailureTransientInfrastructure) != RouteRetry {
		t.Fatal("transient infrastructure lost its retry")
	}
}

// TestPreparationNamesWhatWasMissing is the diagnostics requirement: "cache
// miss" and "go not found" must be distinguishable, which they were not.
func TestPreparationNamesWhatWasMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		out  CommandOutput
		want PrerequisiteKind
	}{
		"executable unavailable": {
			out:  CommandOutput{ExitCode: 127, Stderr: []byte("sh: 1: go: not found")},
			want: PrerequisiteToolchain,
		},
		"module unavailable offline": {
			out:  CommandOutput{ExitCode: 1, Stderr: []byte("go: missing go.sum entry for module providing package example.com/x")},
			want: PrerequisiteModule,
		},
		"module lookup disabled": {
			out:  CommandOutput{ExitCode: 1, Stderr: []byte("module lookup disabled by GOPROXY=off")},
			want: PrerequisiteModule,
		},
		"cache unusable": {
			out:  CommandOutput{ExitCode: 1, Stderr: []byte("go: permission denied writing /cache")},
			want: PrerequisiteCache,
		},
	} {
		t.Run(name, func(t *testing.T) {
			kind, detail := classifyPreparationOutput(tc.out)
			if kind != tc.want {
				t.Fatalf("classified %q as %q, want %q", tc.out.Stderr, kind, tc.want)
			}
			if detail == "" {
				t.Fatal("the classification carries no operator-usable detail")
			}
		})
	}
}

// TestPreparationRefusesAnEmptyCacheDeterministically covers the exact observed
// condition, end to end through Assure: an empty cache is a prerequisite wait,
// not a transient retry and not a verdict.
func TestPreparationRefusesAnEmptyCacheDeterministically(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module x\ngo 1.25\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommandExecutor{found: true}
	v := BaselineGoVerifier{
		Sandbox:            DockerSandbox{Image: "sha256:image", Executor: fake},
		ArtifactStore:      ArtifactStore{Root: filepath.Join(root, "artifacts")},
		DependencyCacheDir: cache,
	}
	err := v.prepare(context.Background(), checkout)
	if err == nil {
		t.Fatal("an empty pre-warmed cache was accepted")
	}
	var unavailable *DependencyUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("preparation returned an untyped failure: %v", err)
	}
	if unavailable.Kind != PrerequisiteCache {
		t.Fatalf("kind = %q, want %q", unavailable.Kind, PrerequisiteCache)
	}
	if unavailable.Transient() {
		t.Fatal("an empty cache was reported as transient; it does not fill itself")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("the diagnostic does not name the condition: %v", err)
	}
	// Nothing was run at all: the refusal is local and free.
	if len(fake.calls) != 0 {
		t.Fatalf("a deterministic refusal still started a container: %v", fake.calls)
	}
}

// TestOnlyAnUnavailableSandboxIsTransient keeps the narrowness honest.
func TestOnlyAnUnavailableSandboxIsTransient(t *testing.T) {
	transient := &DependencyUnavailableError{Kind: PrerequisiteSandbox}
	if !transient.Transient() {
		t.Fatal("an unavailable sandbox is genuinely transient and must retry")
	}
	for _, kind := range []PrerequisiteKind{PrerequisiteToolchain, PrerequisiteCache, PrerequisiteModule, PrerequisiteSource} {
		if (&DependencyUnavailableError{Kind: kind}).Transient() {
			t.Fatalf("%q was reported transient; re-running it changes nothing", kind)
		}
	}
}

// TestPrerequisiteWaitDoesNotSatisfyAssurance is the defect-G guard restated for
// the new route: a wait-routed observation reached no verdict either, so it must
// not satisfy the assurance operation.
func TestPrerequisiteWaitDoesNotSatisfyAssurance(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.useAssurance(&alwaysFailingVerifier{class: FailureAssurancePrerequisite})
	runID := fixture.start()

	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Waiting || outcome.Reason != "assurance_dependency_unavailable" {
		t.Fatalf("disposition = %q (%q), want the prerequisite wait", outcome.Disposition, outcome.Reason)
	}
	state := fixture.state(runID)
	key, wanted := bindAssuranceGo(state)
	if !wanted {
		t.Fatal("assurance is not bound for a completed candidate")
	}
	if state.satisfied(OpAssuranceGo, key) {
		t.Fatal("a wait-routed unjudged assurance satisfied its operation: defect G, again")
	}
	if state.snapshot.Reason == "goal_state_reached" {
		t.Fatal("the run concluded its goal was reached with an unjudged candidate")
	}

	// Observing the same unresolved prerequisite does not spend the budget, and
	// the exact binding never moves.
	op, _ := assuranceOperation(t, fixture.store, runID)
	first := op.IdempotencyKey
	for tick := 0; tick < 6; tick++ {
		if got := fixture.reconcile(runID); got.Reason != "assurance_dependency_unavailable" {
			t.Fatalf("tick %d left the run %q (%q)", tick, got.Disposition, got.Reason)
		}
		op, _ = assuranceOperation(t, fixture.store, runID)
		if op.Attempt >= op.MaxAttempts {
			t.Fatalf("tick %d burned the assurance budget: attempt %d of %d", tick, op.Attempt, op.MaxAttempts)
		}
		if op.IdempotencyKey != first {
			t.Fatalf("the exact assurance binding moved: %q -> %q", first, op.IdempotencyKey)
		}
	}
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventRunFailed) != 0 {
		t.Fatalf("an operator-fixable prerequisite failed the run: %v", journalTypes(events))
	}
}

// TestAssuranceResumesOnceThePrerequisiteIsProvisioned is the resume half: the
// operator provisions what was missing and the SAME run verifies the SAME exact
// tree, with no manual repair of the candidate.
func TestAssuranceResumesOnceThePrerequisiteIsProvisioned(t *testing.T) {
	fixture := newPhase8Fixture(t)
	verifier := &switchableVerifier{class: FailureAssurancePrerequisite}
	fixture.useAssurance(verifier)
	runID := fixture.start()

	if outcome := fixture.reconcile(runID); outcome.Reason != "assurance_dependency_unavailable" {
		t.Fatalf("outcome = %+v", outcome)
	}
	before := fixture.state(runID)
	commit, tree := before.projection.CandidateRevision, before.projection.CandidateTree
	if commit == "" {
		t.Fatal("the fixture produced no candidate to verify")
	}

	// The operator provisions the missing material. Nothing about the run changed.
	verifier.provision()
	fixture.reconcile(runID)

	after := fixture.state(runID)
	if after.projection.CandidateRevision != commit || after.projection.CandidateTree != tree {
		t.Fatalf("the candidate moved while waiting: %s/%s -> %s/%s",
			commit, tree, after.projection.CandidateRevision, after.projection.CandidateTree)
	}
	assurance := after.projection.Assurance
	if assurance == nil || !assurance.Passed {
		t.Fatalf("assurance did not re-derive after the prerequisite was provisioned: %#v", assurance)
	}
	if assurance.Commit != commit || assurance.Tree != tree {
		t.Fatalf("assurance ran on %s/%s, not the preserved exact tree %s/%s", assurance.Commit, assurance.Tree, commit, tree)
	}
}

// switchableVerifier reports a prerequisite failure until an operator provisions
// what was missing, then verifies normally. It is the external world.
type switchableVerifier struct {
	class       FailureClass
	provisioned bool
}

func (v *switchableVerifier) provision() { v.provisioned = true }

func (v *switchableVerifier) Assure(context.Context, AssuranceRequest) (AssuranceResult, error) {
	if v.provisioned {
		return AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: true}, nil
	}
	return AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: false, FailureClass: v.class}, nil
}

// TestOnlyTheGoBuildAreaIsExecutable pins the one boundary this repair moved.
// `go test` compiles a binary and runs it, so the toolchain needs somewhere
// executable; everything else stays noexec, and the rest of the boundary is
// untouched.
func TestOnlyTheGoBuildAreaIsExecutable(t *testing.T) {
	args := dockerBase(t.TempDir(), true)
	var execable, noexec []string
	for i, arg := range args {
		if arg != "--tmpfs" {
			continue
		}
		path, options, _ := strings.Cut(args[i+1], ":")
		if strings.Contains(","+options+",", ",exec,") {
			execable = append(execable, path)
			continue
		}
		if !strings.Contains(options, "noexec") {
			t.Fatalf("tmpfs %q states neither exec nor noexec: %q", path, options)
		}
		noexec = append(noexec, path)
	}
	if len(execable) != 1 || execable[0] != sandboxBuildDir {
		t.Fatalf("executable tmpfs set is %v, want exactly [%s]", execable, sandboxBuildDir)
	}
	for _, required := range []string{"/tmp", "/home", "/candidate/.git"} {
		if !slices.Contains(noexec, required) {
			t.Fatalf("%s is no longer mounted noexec: %v", required, noexec)
		}
	}
	// Everything that contains a test binary is unchanged.
	text := strings.Join(args, " ")
	for _, required := range []string{"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--pids-limit 256"} {
		if !strings.Contains(text, required) {
			t.Fatalf("the sandbox boundary lost %q: %s", required, text)
		}
	}
	if strings.Contains(text, "docker.sock") || strings.Contains(text, "/var/run/docker") {
		t.Fatalf("the sandbox mounts the Docker socket: %s", text)
	}
	// The build area is a container tmpfs, never a host path.
	if strings.Contains(text, "src="+sandboxBuildDir) {
		t.Fatalf("the Go build area is bound to a host path: %s", text)
	}
}

// TestSandboxGoEnvironmentDisablesVCSStamping records why -buildvcs=false is
// required rather than incidental: the candidate's Git metadata is masked, so
// Go finds a directory that is not a repository.
func TestSandboxGoEnvironmentDisablesVCSStamping(t *testing.T) {
	if !strings.Contains(strings.Join(sandboxGoEnv, " "), sandboxBuildVCS) {
		t.Fatalf("the shared Go environment does not disable VCS stamping: %v", sandboxGoEnv)
	}
	args := strings.Join(dockerBase(t.TempDir(), true), " ")
	if !strings.Contains(args, "--tmpfs /candidate/.git:") {
		t.Fatalf("the candidate's Git metadata is no longer masked, so the stamping rationale changed: %s", args)
	}
}
