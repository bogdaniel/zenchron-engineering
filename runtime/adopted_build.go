package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Adopted controller builder
// ---------------------------------------------------------------------------

// A controller may claim kind=adopted only when every one of these is true:
//
//  1. the source revision is an exact commit;
//  2. the source tree is recomputed from that revision, not taken on trust;
//  3. the revision is contained in the currently OBSERVED externally governed
//     trusted main;
//  4. the trust root governing that branch is active and satisfies the policy;
//  5. the build checkout is clean and detached at that exact revision;
//  6. the binary is built under controlled settings;
//  7. the output binary is measured;
//  8. provenance records all of it.
//
// A branch name is never evidence. A local main ref is never evidence - it is
// whatever the last fetch left behind, and an attacker who can write the local
// repository can write that. A caller passing --kind adopted is never
// evidence, which is why this command takes no such flag.
//
// The builder owns the whole proof. There is deliberately no path that emits a
// binary first and records provenance afterwards: a binary claiming adoption
// that has not yet been proven adopted is the exact artifact this exists to
// prevent, and it would be indistinguishable from a real one the moment it
// leaves this function.

// AdoptedBuildRequest is what the operator asks for. Note what is absent: no
// kind, no "trust me", no way to name the tree.
type AdoptedBuildRequest struct {
	Repository GitHubRepo
	// Revision is optional. Empty means "whatever trusted main is right now",
	// which is the normal case. A named revision is still checked for
	// containment, so pinning an older adopted commit is allowed and lying
	// about one is not.
	Revision string
	// RepositoryDir is a local clone used only as a Git transport for fetching
	// the trusted revision. Nothing about it is trusted: the revision, the
	// tree and containment are all re-derived.
	RepositoryDir string
	// OutputRoot is the controller root. Staging happens INSIDE it so the final
	// version directory can be published by a same-filesystem atomic rename.
	OutputRoot   string
	GOOS, GOARCH string
	Version      string
	// Sandbox and DependencyCacheDir are the controlled build environment: the
	// same pinned image and read-only operator cache exact-tree assurance uses.
	Sandbox            DockerSandbox
	DependencyCacheDir string
	Policy             TrustPolicy
}

// AdoptedBuildDeps are the seams. Every field defaults, so production callers
// set none of them; they exist because the refusals below must be reachable in
// a test without a real repository, a real GitHub, or a real trusted main.
type AdoptedBuildDeps struct {
	Rulesets func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error)
	RefSHA   func(context.Context, GitHubRepo, string) (RefObservation, error)
	Git      func(dir string, args ...string) (string, error)
	// Fetch is separate from Git because it is the one step that reaches the
	// network, and it must be bound to the governed remote exactly like every
	// other remote operation the runtime performs.
	Fetch   func(dir, revision, branch string) error
	Build   func(context.Context, AdoptedBuildSpec) (BuildEnvironment, error)
	Measure func(path string) (string, error)
	// Probe asks the freshly built binary what it thinks it is. It is not
	// optional: an adopted artifact whose own provenance was never checked is
	// exactly the artifact this command exists to refuse.
	Probe func(binary string) (ControllerBuild, error)
	Now   func() time.Time
}

// AdoptedBuildSpec is the exact compilation, separated from the proof so a test
// can assert what would have been built without building it.
type AdoptedBuildSpec struct {
	SourceDir, Output string
	GOOS, GOARCH      string
	Kind, Version     string
	Revision, Tree    string
	// Sandbox and CacheDir are the controlled environment. An empty image is
	// refused: an arbitrary `go` from an ambient PATH is not a reproducible
	// trust boundary, and pretending otherwise would make every other proof in
	// this file decorative.
	Sandbox  DockerSandbox
	CacheDir string
}

// BuildEnvironment is what actually compiled the binary, recorded because
// "which source" is only half of reproducibility. Nothing here is inherited
// from the host: the container receives an explicit environment allowlist, so
// ambient GOFLAGS, GOWORK, GOENV, GOPROXY and PATH cannot reach the compiler.
type BuildEnvironment struct {
	Kind        string   `json:"kind"`
	Image       string   `json:"image"`
	Toolchain   string   `json:"toolchain"`
	Network     string   `json:"network"`
	SourceMount string   `json:"source_mount"`
	CacheMount  string   `json:"cache_mount"`
	Environment []string `json:"environment"`
	Digest      string   `json:"digest"`
}

// AdoptedBuildProvenance is the deterministic record. It is the artifact, and
// it is also the return value: a caller that wants to know what was built asks
// the same object the file holds.
type AdoptedBuildProvenance struct {
	SchemaVersion string           `json:"schema_version"`
	Repository    string           `json:"repository"`
	TrustRoot     TrustRootRecord  `json:"trust_root"`
	TrustedMain   RevisionRecord   `json:"trusted_main"`
	Source        RevisionRecord   `json:"source"`
	Containment   string           `json:"containment_proof"`
	Kind          string           `json:"controller_kind"`
	Version       string           `json:"version"`
	GOOS          string           `json:"goos"`
	GOARCH        string           `json:"goarch"`
	BuildFlags    []string         `json:"build_flags"`
	BuildEnv      BuildEnvironment `json:"build_environment"`
	BinarySHA256  string           `json:"binary_sha256"`
	BuiltAt       string           `json:"built_at"`
	Builder       BuilderRecord    `json:"builder"`
	OutputPath    string           `json:"output_path"`
	SelfProbe     SelfProbeRecord  `json:"self_probe"`
}

// SelfProbeRecord is the binary's own account of itself, checked against what
// was asked for. There is no "not probed" state: an unprobed build is refused,
// never recorded.
type SelfProbeRecord struct {
	Kind         string `json:"kind"`
	Version      string `json:"version"`
	Revision     string `json:"source_revision"`
	Tree         string `json:"source_tree"`
	BinarySHA256 string `json:"binary_sha256"`
	Matched      bool   `json:"matched"`
}

type TrustRootRecord struct {
	RulesetID   int64       `json:"ruleset_id"`
	Name        string      `json:"name"`
	Digest      string      `json:"digest"`
	Policy      TrustPolicy `json:"policy"`
	Enforcement string      `json:"enforcement"`
}

type RevisionRecord struct {
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

// BuilderRecord is the identity of the tool that produced the artifact. It may
// truthfully be unattested - the builder need not itself be adopted - but it
// must not LIE: a builder whose own attestation could not be resolved records
// that fact rather than laundering it into "unattested", which would claim a
// deliberate absence of provenance where there is a failed measurement.
type BuilderRecord struct {
	Version         string `json:"version"`
	SourceRevision  string `json:"source_revision"`
	Kind            string `json:"kind"`
	ResolutionError string `json:"resolution_error,omitempty"`
}

const adoptedBuildSchemaVersion = "adopted-build/1"

// BuildAdoptedController performs the whole proof, then the build, then
// publishes. Any refusal returns before anything is installed.
func BuildAdoptedController(ctx context.Context, request AdoptedBuildRequest, deps AdoptedBuildDeps, self BuilderRecord) (AdoptedBuildProvenance, error) {
	deps = deps.withDefaults()
	var out AdoptedBuildProvenance
	// The production dependencies have no honest default: guessing a forge or
	// a ref observer would be inventing the trust root. Missing ones are a
	// typed refusal, never a panic.
	if deps.Rulesets == nil || deps.RefSHA == nil {
		return out, fmt.Errorf("the builder has no way to observe the trust root or trusted main, so nothing may be called adopted")
	}
	policy := request.Policy
	if policy.Ref == "" {
		policy = DefaultTrustPolicy()
	}
	// A cross-target build cannot be asked what it thinks it is, and an
	// unprobed adopted artifact is refused. Cross-compiling stays available
	// for unattested builds, which is where it belongs.
	targetOS := firstNonEmpty(request.GOOS, runtime.GOOS)
	targetArch := firstNonEmpty(request.GOARCH, runtime.GOARCH)
	if targetOS != runtime.GOOS || targetArch != runtime.GOARCH {
		return out, fmt.Errorf("an adopted build for %s/%s cannot run its own provenance check here (%s/%s); cross-target adoption is refused rather than left unverified",
			targetOS, targetArch, runtime.GOOS, runtime.GOARCH)
	}

	// 1. The trust root, before anything is fetched or built. A weak gate
	// makes every later step meaningless, so it is the first question asked.
	root, err := observeTrustRoot(ctx, deps, request.Repository, policy)
	if err != nil {
		return out, err
	}
	startingDigest := trustRootDigest(root)

	// 2. Trusted main as GITHUB reports it, not as the local clone remembers.
	branch := strings.TrimPrefix(policy.Ref, "refs/heads/")
	trustedMain, err := observeTrustedMain(ctx, deps, request.Repository, branch)
	if err != nil {
		return out, err
	}

	// 3. Fetch that exact revision, then prove containment against it.
	if err := deps.Fetch(request.RepositoryDir, trustedMain, branch); err != nil {
		return out, fmt.Errorf("the trusted revision could not be fetched: %w", err)
	}
	source := strings.TrimSpace(request.Revision)
	if source == "" {
		source = trustedMain
	}
	resolved, err := deps.Git(request.RepositoryDir, "rev-parse", source+"^{commit}")
	if err != nil {
		return out, fmt.Errorf("source revision %s is not an exact commit here: %w", source, err)
	}
	source = strings.TrimSpace(resolved)
	if err := proveContained(deps, request.RepositoryDir, source, trustedMain); err != nil {
		return out, err
	}

	// 4. A clean detached checkout, and the tree RECOMPUTED from it. The
	// recomputation is the point: it catches a checkout that does not actually
	// hold the revision it claims.
	tree, err := revisionTree(deps, request.RepositoryDir, source)
	if err != nil {
		return out, err
	}
	checkout, err := os.MkdirTemp("", "zenchron-adopted-build-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(checkout)
	if err := exportRevision(deps, request.RepositoryDir, source, checkout); err != nil {
		return out, err
	}
	recomputed, err := recomputeTree(deps, checkout)
	if err != nil {
		return out, err
	}
	if recomputed != tree {
		return out, fmt.Errorf("the build checkout holds tree %s, not the %s that revision %s names", recomputed, tree, source)
	}

	// 5. Staging INSIDE the controller root, so publication is a
	// same-filesystem atomic rename rather than a copy that can half-finish.
	version := strings.TrimSpace(request.Version)
	if version == "" {
		version = "main-" + shortSHA(source)
	}
	final := filepath.Join(request.OutputRoot, version)
	if err := os.MkdirAll(request.OutputRoot, 0700); err != nil {
		return out, err
	}
	staging, err := os.MkdirTemp(request.OutputRoot, ".staging-"+version+"-")
	if err != nil {
		return out, err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(staging)
		}
	}()
	output := filepath.Join(staging, "zenchron-engineering")

	// 6. Build in the controlled environment, then measure what was produced.
	spec := AdoptedBuildSpec{
		SourceDir: checkout, Output: output, GOOS: targetOS, GOARCH: targetArch,
		Kind: ControllerAdopted, Version: version, Revision: source, Tree: tree,
		Sandbox: request.Sandbox, CacheDir: request.DependencyCacheDir,
	}
	environment, err := deps.Build(ctx, spec)
	if err != nil {
		return out, fmt.Errorf("the adopted build failed, so no binary was produced: %w", err)
	}
	digest, err := deps.Measure(output)
	if err != nil {
		return out, fmt.Errorf("the built executable could not be measured, so its provenance cannot be stated: %w", err)
	}

	// 7. The binary states its own provenance, and it must match exactly. A
	// build whose ldflags silently did not take effect produces a working
	// binary that lies about itself, and nothing later would catch it.
	if err := os.Chmod(output, 0500); err != nil {
		return out, err
	}
	reported, err := deps.Probe(output)
	if err != nil {
		return out, fmt.Errorf("the built binary could not report its own provenance, so it may not claim adoption: %w", err)
	}
	expected := ControllerBuild{Kind: ControllerAdopted, Version: version, SourceRevision: source, SourceTree: tree, BinarySHA256: digest}
	if reported != expected {
		return out, fmt.Errorf("the built binary reports %+v, not the %+v it was built as", reported, expected)
	}

	// 8. Revalidate the trust state immediately before publication. The gate
	// could have been weakened, or main could have moved, while the build ran.
	// A concurrent legitimate merge is not a failure - containment is what
	// matters - but the provenance must state the trust state at PUBLICATION,
	// not the one the build happened to start under.
	finalRoot, err := observeTrustRoot(ctx, deps, request.Repository, policy)
	if err != nil {
		return out, fmt.Errorf("the trust root could not be revalidated before publication: %w", err)
	}
	finalDigest := trustRootDigest(finalRoot)
	if finalDigest != startingDigest {
		return out, fmt.Errorf("the trust root changed while the build ran (%s -> %s); refusing to publish under a gate that is not the one proven", startingDigest, finalDigest)
	}
	finalMain, err := observeTrustedMain(ctx, deps, request.Repository, branch)
	if err != nil {
		return out, fmt.Errorf("trusted main could not be revalidated before publication: %w", err)
	}
	if finalMain != trustedMain {
		if err := deps.Fetch(request.RepositoryDir, finalMain, branch); err != nil {
			return out, fmt.Errorf("trusted main moved to %s and could not be re-fetched: %w", finalMain, err)
		}
	}
	if err := proveContained(deps, request.RepositoryDir, source, finalMain); err != nil {
		return out, err
	}
	mainTree, err := revisionTree(deps, request.RepositoryDir, finalMain)
	if err != nil {
		return out, fmt.Errorf("the trusted main tree could not be established: %w", err)
	}

	out = AdoptedBuildProvenance{
		SchemaVersion: adoptedBuildSchemaVersion,
		Repository:    request.Repository.String(),
		TrustRoot: TrustRootRecord{
			RulesetID: finalRoot.ID, Name: finalRoot.Name, Digest: finalDigest,
			Policy: policy, Enforcement: finalRoot.Enforcement,
		},
		TrustedMain:  RevisionRecord{Revision: finalMain, Tree: mainTree},
		Source:       RevisionRecord{Revision: source, Tree: tree},
		Containment:  fmt.Sprintf("ancestry: %s is an ancestor of trusted main %s, re-derived from the remote-observed head at publication rather than a local ref", source, finalMain),
		Kind:         ControllerAdopted,
		Version:      version,
		GOOS:         spec.GOOS,
		GOARCH:       spec.GOARCH,
		BuildFlags:   []string{"-trimpath", "-mod=readonly", "-buildvcs=false", "CGO_ENABLED=0"},
		BuildEnv:     environment,
		BinarySHA256: digest,
		BuiltAt:      deps.Now().UTC().Format(time.RFC3339),
		Builder:      self,
		OutputPath:   filepath.Join(final, "zenchron-engineering"),
		SelfProbe: SelfProbeRecord{
			Kind: reported.Kind, Version: reported.Version, Revision: reported.SourceRevision,
			Tree: reported.SourceTree, BinarySHA256: reported.BinarySHA256, Matched: true,
		},
	}

	// 9. COMPLETE provenance is written in staging, before anything is
	// published. A failure after the binary is in place would otherwise leave
	// an installed controller claiming adoption with no record of why.
	if _, err := WriteAdoptedBuildProvenance(filepath.Join(staging, "provenance.json"), out); err != nil {
		return AdoptedBuildProvenance{}, fmt.Errorf("the provenance could not be written, so nothing was installed: %w", err)
	}
	if err := os.Chmod(staging, 0700); err != nil {
		return AdoptedBuildProvenance{}, err
	}

	// 10. Publish the whole version directory atomically. Version directories
	// are immutable: an existing one is verified, never replaced, because it
	// is the provenance of every run it has already governed.
	if err := publishControllerVersion(staging, final, out); err != nil {
		return AdoptedBuildProvenance{}, err
	}
	published = true
	return out, nil
}

// observeTrustRoot reads the gate and refuses anything that is not one.
func observeTrustRoot(ctx context.Context, deps AdoptedBuildDeps, repo GitHubRepo, policy TrustPolicy) (TrustedMainRuleset, error) {
	rulesets, err := deps.Rulesets(ctx, repo)
	if err != nil {
		return TrustedMainRuleset{}, fmt.Errorf("the trust root could not be observed, so nothing may be called adopted: %w", err)
	}
	root, found := selectTrustRoot(rulesets, policy.Ref)
	if !found {
		return TrustedMainRuleset{}, ErrNoTrustRoot
	}
	if err := VerifyTrustRoot(root, policy); err != nil {
		return TrustedMainRuleset{}, err
	}
	return root, nil
}

func observeTrustedMain(ctx context.Context, deps AdoptedBuildDeps, repo GitHubRepo, branch string) (string, error) {
	observed, err := deps.RefSHA(ctx, repo, branch)
	if err != nil {
		return "", fmt.Errorf("trusted main could not be observed: %w", err)
	}
	if !observed.Exists || observed.SHA == "" {
		return "", fmt.Errorf("trusted main %s does not exist in %s", branch, repo)
	}
	return observed.SHA, nil
}

func proveContained(deps AdoptedBuildDeps, dir, source, trustedMain string) error {
	if _, err := deps.Git(dir, "merge-base", "--is-ancestor", source, trustedMain); err != nil {
		return fmt.Errorf("source %s is not contained in trusted main %s; adopted status is containment, not intent", source, trustedMain)
	}
	return nil
}

func revisionTree(deps AdoptedBuildDeps, dir, revision string) (string, error) {
	out, err := deps.Git(dir, "rev-parse", revision+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// publishControllerVersion makes the staged directory the version directory in
// one atomic rename, and treats an existing version as immutable.
//
// An existing directory is NOT replaced. It is the provenance of every run the
// controller it holds has already governed, and replacing its binary would
// retroactively change what those runs were driven by. If it is byte-identical
// to what was just built, this is idempotent; if it differs, it is a refusal.
func publishControllerVersion(staging, final string, built AdoptedBuildProvenance) error {
	if existing, err := os.Stat(final); err == nil && existing.IsDir() {
		return reconcileExistingVersion(final, built)
	}
	if err := os.Rename(staging, final); err != nil {
		// A concurrent publisher may have created it between the check and
		// the rename; that is the same immutability question, not a race to
		// win.
		if existing, statErr := os.Stat(final); statErr == nil && existing.IsDir() {
			return reconcileExistingVersion(final, built)
		}
		return fmt.Errorf("the controller version directory could not be published atomically, so nothing was installed: %w", err)
	}
	return nil
}

func reconcileExistingVersion(final string, built AdoptedBuildProvenance) error {
	installed, err := measureExecutable(filepath.Join(final, "zenchron-engineering"))
	if err != nil {
		return fmt.Errorf("%s already exists and cannot be verified; refusing to touch an installed controller: %w", final, err)
	}
	if installed != built.BinarySHA256 {
		return fmt.Errorf("%s already holds a different controller (installed %s, built %s); version directories are immutable and this one is not replaced",
			final, installed, built.BinarySHA256)
	}
	return nil
}

// selectTrustRoot picks the ruleset governing the branch. More than one may
// exist; the one that targets the ref is the one that matters.
func selectTrustRoot(rulesets []TrustedMainRuleset, ref string) (TrustedMainRuleset, bool) {
	for _, r := range rulesets {
		if trustRootContains(r.Targets, ref) {
			return r, true
		}
	}
	return TrustedMainRuleset{}, false
}

// trustRootDigest is a canonical digest of the trust root as observed, so the
// provenance names WHICH gate was in force and a later reader can tell whether
// it has since changed.
func trustRootDigest(root TrustedMainRuleset) string {
	encoded, err := json.Marshal(root)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func exportRevision(deps AdoptedBuildDeps, repoDir, revision, into string) error {
	// git archive is the export: it writes exactly the revision's content and
	// leaves no .git behind, which is what makes the checkout clean by
	// construction rather than by inspection afterwards.
	archive := exec.Command("git", "-C", repoDir, "archive", revision)
	untar := exec.Command("tar", "-x", "-C", into)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	var stderr strings.Builder
	archive.Stderr = &stderr
	if err := archive.Start(); err != nil {
		return err
	}
	if err := untar.Run(); err != nil {
		_ = archive.Wait()
		return fmt.Errorf("the build checkout could not be exported: %w", err)
	}
	if err := archive.Wait(); err != nil {
		return fmt.Errorf("the build checkout could not be exported: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// recomputeTree hashes the exported checkout with Git's own object rules, in a
// throwaway repository that shares nothing with the source. The comparison is
// therefore against the same tree identity the commit names, derived from the
// bytes on disk rather than from what the source repository claims.
func recomputeTree(deps AdoptedBuildDeps, dir string) (string, error) {
	gitDir, err := os.MkdirTemp("", "zenchron-tree-git-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(gitDir)
	index := filepath.Join(gitDir, "index")
	env := append(os.Environ(),
		"GIT_DIR="+gitDir, "GIT_WORK_TREE="+dir, "GIT_INDEX_FILE="+index,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	// init runs WITHOUT the work-tree variables: Git refuses to create a
	// repository while they are set.
	if err := runIn(dir, os.Environ(), "git", "init", "-q", "--bare", gitDir); err != nil {
		return "", fmt.Errorf("the tree could not be recomputed: %w", err)
	}
	if err := runIn(dir, env, "git", "add", "-A", "."); err != nil {
		return "", fmt.Errorf("the tree could not be recomputed: %w", err)
	}
	out, err := outputIn(dir, env, "git", "write-tree")
	if err != nil {
		return "", fmt.Errorf("the tree could not be recomputed: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func runIn(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	return cmd.Run()
}

func outputIn(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	return string(out), err
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// WriteAdoptedBuildProvenance stores the record owner-only beside the binary.
func WriteAdoptedBuildProvenance(path string, provenance AdoptedBuildProvenance) (string, error) {
	encoded, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return "", err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (d AdoptedBuildDeps) withDefaults() AdoptedBuildDeps {
	if d.Git == nil {
		d.Git = gitOutput
	}
	if d.Fetch == nil {
		d.Fetch = fetchTrustedRevision
	}
	if d.Build == nil {
		d.Build = runAdoptedBuild
	}
	if d.Measure == nil {
		d.Measure = measureExecutable
	}
	if d.Probe == nil {
		d.Probe = probeControllerBuild
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// fetchTrustedRevision brings the exact trusted revision into the local
// repository through the GOVERNED remote, so the object containment is later
// proven against came from the remote GitHub named, not from whatever the
// local clone happened to be holding.
func fetchTrustedRevision(dir, revision, branch string) error {
	origin, err := gitOutput(dir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("the repository has no origin remote to fetch the trusted revision from: %w", err)
	}
	remote, err := GovernedRemote(strings.TrimSpace(origin))
	if err != nil {
		return err
	}
	runner := RepositoryGitRunner{Dir: dir, Local: controlPolicy(), Remote: &RemotePolicy{Identity: remote}}
	if _, err := runner.run("fetch", remote.URL, revision); err == nil {
		return nil
	}
	// A remote may refuse a bare-SHA fetch. The branch fetch is the fallback,
	// and containment is still proven against the remote-observed head below.
	_, err = runner.run("fetch", remote.URL, branch)
	return err
}

// runAdoptedBuild compiles inside the pinned assurance sandbox.
//
// This is the whole of defect W. The previous implementation ran `go build`
// with the host's environment appended, which meant ambient GOFLAGS, GOWORK,
// GOENV, GOPROXY, GOTOOLCHAIN and PATH could all change what was compiled
// AFTER the source tree had already been approved. GOFLAGS=-overlay=... alone
// is enough: the binary ends up containing code from outside the recomputed
// tree while its embedded metadata still names that tree, and every later
// check - including the self-probe - passes.
//
// The fix is not to scrub a list of variable names, which is a race against
// whatever the toolchain learns to read next. It is to stop inheriting an
// environment at all. Docker's --env is an explicit allowlist, so the
// container starts with exactly what dockerBase and this function name and
// nothing else: no host PATH, no host GOFLAGS, no host GOWORK, no host GOENV.
//
// The source is mounted read-only, the operator module cache is mounted
// read-only, networking is off, and the staging output directory is the only
// writable thing the compiler can reach.
func runAdoptedBuild(ctx context.Context, spec AdoptedBuildSpec) (BuildEnvironment, error) {
	if strings.TrimSpace(spec.Sandbox.Image) == "" {
		return BuildEnvironment{}, fmt.Errorf("no pinned build image is configured; an arbitrary go from the ambient PATH is not a reproducible trust boundary for an adopted build")
	}
	if strings.TrimSpace(spec.CacheDir) == "" {
		return BuildEnvironment{}, fmt.Errorf("no trusted module cache is configured; an adopted build never downloads, so it needs one provisioned in advance")
	}
	if empty, err := directoryIsEmpty(spec.CacheDir); err != nil || empty {
		return BuildEnvironment{}, fmt.Errorf("the trusted module cache %s is empty or unreadable; an adopted build compiles offline and cannot provision it", spec.CacheDir)
	}
	sandbox := spec.Sandbox
	sandbox.OperationID = "adopted-build-" + spec.Revision

	toolchain, err := adoptedBuildToolchain(ctx, sandbox, spec)
	if err != nil {
		return BuildEnvironment{}, err
	}

	if _, err := sandbox.run(ctx, adoptedBuildArgs(spec)); err != nil {
		return BuildEnvironment{}, err
	}

	built := BuildEnvironment{
		Kind:        "pinned-container",
		Image:       sandbox.Image,
		Toolchain:   toolchain,
		Network:     "none",
		SourceMount: "read-only",
		CacheMount:  "read-only",
		Environment: adoptedBuildEnvironment(spec),
	}
	built.Digest = canonicalDigest(built)
	return built, nil
}

// adoptedBuildEnvironment is the compiler's ENTIRE environment. It is stated
// here and nowhere else; nothing is read from the host.
func adoptedBuildEnvironment(spec AdoptedBuildSpec) []string {
	return append([]string{
		"GOMODCACHE=/cache",
		"GOWORK=off",
		"GOENV=off",
		"CGO_ENABLED=0",
		"GOOS=" + spec.GOOS,
		"GOARCH=" + spec.GOARCH,
	}, sandboxGoEnv...)
}

// adoptedBuildArgs is the complete container invocation, separated so a test
// can assert what the compiler will and will not see without running Docker.
func adoptedBuildArgs(spec AdoptedBuildSpec) []string {
	ldflags := fmt.Sprintf("-X main.buildKind=%s -X main.version=%s -X main.sourceRevision=%s -X main.sourceTree=%s",
		spec.Kind, spec.Version, spec.Revision, spec.Tree)
	args := dockerBase(spec.SourceDir, true)
	args = append(args, goModuleCacheMount(spec.CacheDir),
		"--mount=type=bind,src="+filepath.Dir(spec.Output)+",dst=/out",
		"--workdir", "/candidate")
	args = append(args, envArgs(adoptedBuildEnvironment(spec)...)...)
	return append(args, spec.Sandbox.Image, "go", "build", "-trimpath",
		"-ldflags", ldflags, "-o", "/out/"+filepath.Base(spec.Output), "./cmd/zenchron-engineering")
}

// adoptedBuildToolchain records WHICH compiler ran, measured from the pinned
// image rather than assumed from its tag.
func adoptedBuildToolchain(ctx context.Context, sandbox DockerSandbox, spec AdoptedBuildSpec) (string, error) {
	args := dockerBase(spec.SourceDir, true)
	args = append(args, "--workdir", "/candidate")
	args = append(args, envArgs(sandboxGoEnv...)...)
	args = append(args, sandbox.Image, "go", "version")
	probe := sandbox
	probe.OperationID = "adopted-build-toolchain-" + spec.Revision
	out, err := probe.run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("the pinned build toolchain could not be identified: %w", err)
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}

// canonicalDigest hashes a value through the repository's existing canonical
// JSON convention rather than inventing a second hashing dialect.
func canonicalDigest(value any) string {
	encoded, err := CanonicalJSON(value)
	if err != nil {
		encoded, err = json.Marshal(value)
		if err != nil {
			return ""
		}
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func measureExecutable(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// probeControllerBuild asks the freshly built binary what it thinks it is,
// through a read-only self-inspection that needs no operator environment.
//
// The previous version ran the full doctor, so it needed a loadable operator
// configuration, a reachable Docker daemon, a forge credential and a provider
// credential - none of which have anything to do with whether a binary's own
// build metadata matches what was asked for. When the configuration failed to
// load, the probe was silently skipped and the artifact recorded "not probed",
// which is the one state an adopted artifact may not be in.
func probeControllerBuild(binary string) (ControllerBuild, error) {
	cmd := exec.Command(binary, "controller", "inspect-self", "--json")
	out, err := cmd.Output()
	if err != nil {
		return ControllerBuild{}, fmt.Errorf("the binary could not report its own provenance: %w", err)
	}
	var reported ControllerBuild
	if err := json.Unmarshal(out, &reported); err != nil {
		return ControllerBuild{}, fmt.Errorf("the binary did not return a controller provenance document")
	}
	return reported, nil
}
