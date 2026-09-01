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
	OutputDir     string
	GOOS, GOARCH  string
	Version       string
	// DoctorConfig, when set, is the operator configuration the freshly built
	// binary is asked to run its own controller.build check against. Empty
	// skips that step and says so in the provenance rather than pretending.
	DoctorConfig string
	Policy       TrustPolicy
}

// AdoptedBuildDeps are the seams. Every field defaults, so production callers
// set none of them; they exist because the refusals below must be reachable in
// a test without a real repository, a real GitHub, or a real trusted main.
type AdoptedBuildDeps struct {
	Rulesets func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error)
	RefSHA   func(context.Context, GitHubRepo, string) (RefObservation, error)
	Git      func(dir string, args ...string) (string, error)
	Build    func(spec AdoptedBuildSpec) error
	Measure  func(path string) (string, error)
	Probe    func(binary, config string) (DoctorCheck, error)
	Now      func() time.Time
}

// AdoptedBuildSpec is the exact compilation, separated from the proof so a test
// can assert what would have been built without building it.
type AdoptedBuildSpec struct {
	SourceDir, Output     string
	GOOS, GOARCH          string
	Kind, Version         string
	Revision, Tree        string
	TrimPath, CGODisabled bool
}

// AdoptedBuildProvenance is the deterministic record. It is the artifact, and
// it is also the return value: a caller that wants to know what was built asks
// the same object the file holds.
type AdoptedBuildProvenance struct {
	SchemaVersion string          `json:"schema_version"`
	Repository    string          `json:"repository"`
	TrustRoot     TrustRootRecord `json:"trust_root"`
	TrustedMain   RevisionRecord  `json:"trusted_main"`
	Source        RevisionRecord  `json:"source"`
	Containment   string          `json:"containment_proof"`
	Kind          string          `json:"controller_kind"`
	Version       string          `json:"version"`
	GOOS          string          `json:"goos"`
	GOARCH        string          `json:"goarch"`
	BuildFlags    []string        `json:"build_flags"`
	BinarySHA256  string          `json:"binary_sha256"`
	BuiltAt       string          `json:"built_at"`
	Builder       BuilderRecord   `json:"builder"`
	OutputPath    string          `json:"output_path"`
	SelfReport    string          `json:"self_report"`
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

type BuilderRecord struct {
	Version        string `json:"version"`
	SourceRevision string `json:"source_revision"`
	Kind           string `json:"kind"`
}

const adoptedBuildSchemaVersion = "adopted-build/1"

// BuildAdoptedController performs the whole proof and then the build. Any
// refusal returns before anything is installed.
func BuildAdoptedController(ctx context.Context, request AdoptedBuildRequest, deps AdoptedBuildDeps, self ControllerBuild) (AdoptedBuildProvenance, error) {
	deps = deps.withDefaults()
	policy := request.Policy
	if policy.Ref == "" {
		policy = DefaultTrustPolicy()
	}
	var out AdoptedBuildProvenance

	// 1-2. The trust root, before anything is fetched or built. A weak gate
	// makes every later step meaningless, so it is the first question asked.
	rulesets, err := deps.Rulesets(ctx, request.Repository)
	if err != nil {
		return out, fmt.Errorf("the trust root could not be observed, so nothing may be called adopted: %w", err)
	}
	root, found := selectTrustRoot(rulesets, policy.Ref)
	if !found {
		return out, ErrNoTrustRoot
	}
	if err := VerifyTrustRoot(root, policy); err != nil {
		return out, err
	}

	// 3. Trusted main as GITHUB reports it, not as the local clone remembers.
	branch := strings.TrimPrefix(policy.Ref, "refs/heads/")
	observed, err := deps.RefSHA(ctx, request.Repository, branch)
	if err != nil {
		return out, fmt.Errorf("trusted main could not be observed: %w", err)
	}
	if !observed.Exists || observed.SHA == "" {
		return out, fmt.Errorf("trusted main %s does not exist in %s", branch, request.Repository)
	}
	trustedMain := observed.SHA

	// 4. Fetch that exact revision, then prove containment against it.
	if _, err := deps.Git(request.RepositoryDir, "fetch", "origin", trustedMain); err != nil {
		// A shallow or restricted remote may refuse a bare-SHA fetch; the
		// branch fetch is the fallback, and containment is still proven below.
		if _, branchErr := deps.Git(request.RepositoryDir, "fetch", "origin", branch); branchErr != nil {
			return out, fmt.Errorf("the trusted revision could not be fetched: %w", err)
		}
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
	if _, err := deps.Git(request.RepositoryDir, "merge-base", "--is-ancestor", source, trustedMain); err != nil {
		return out, fmt.Errorf("source %s is not contained in trusted main %s; adopted status is containment, not intent", source, trustedMain)
	}

	// 5. A clean detached checkout, and the tree RECOMPUTED from it. The
	// recomputation is the point: it catches a checkout that does not actually
	// hold the revision it claims.
	declaredTree, err := deps.Git(request.RepositoryDir, "rev-parse", source+"^{tree}")
	if err != nil {
		return out, err
	}
	tree := strings.TrimSpace(declaredTree)
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

	// 6-7. Build, then measure what was actually produced.
	version := strings.TrimSpace(request.Version)
	if version == "" {
		version = "main-" + shortSHA(source)
	}
	output := filepath.Join(request.OutputDir, "zenchron-engineering")
	spec := AdoptedBuildSpec{
		SourceDir: checkout, Output: output,
		GOOS: firstNonEmpty(request.GOOS, runtime.GOOS), GOARCH: firstNonEmpty(request.GOARCH, runtime.GOARCH),
		Kind: ControllerAdopted, Version: version, Revision: source, Tree: tree,
		TrimPath: true, CGODisabled: true,
	}
	if err := os.MkdirAll(request.OutputDir, 0700); err != nil {
		return out, err
	}
	if err := deps.Build(spec); err != nil {
		return out, fmt.Errorf("the adopted build failed, so no binary was produced: %w", err)
	}
	digest, err := deps.Measure(output)
	if err != nil {
		_ = os.Remove(output)
		return out, fmt.Errorf("the built executable could not be measured, so its provenance cannot be stated: %w", err)
	}

	// 8. The binary states its own provenance, and it must match what was
	// asked for. A build whose ldflags silently did not take effect produces a
	// working binary that lies about itself, and nothing later would catch it.
	selfReport := "not probed: no operator configuration was supplied"
	if strings.TrimSpace(request.DoctorConfig) != "" {
		if spec.GOOS != runtime.GOOS || spec.GOARCH != runtime.GOARCH {
			selfReport = fmt.Sprintf("not probed: built for %s/%s, which cannot run here", spec.GOOS, spec.GOARCH)
		} else {
			check, probeErr := deps.Probe(output, request.DoctorConfig)
			if probeErr != nil {
				_ = os.Remove(output)
				return out, fmt.Errorf("the built binary could not report its own provenance: %w", probeErr)
			}
			if check.Status != DoctorPass {
				_ = os.Remove(output)
				return out, fmt.Errorf("the built binary reports controller.build %s: %s", check.Status, check.Reason)
			}
			for _, want := range []string{ControllerAdopted, version, source, tree, digest} {
				if !strings.Contains(check.Reason, want) {
					_ = os.Remove(output)
					return out, fmt.Errorf("the built binary does not report %q in its own provenance: %s", want, check.Reason)
				}
			}
			selfReport = check.Reason
		}
	}

	out = AdoptedBuildProvenance{
		SchemaVersion: adoptedBuildSchemaVersion,
		Repository:    request.Repository.String(),
		TrustRoot: TrustRootRecord{
			RulesetID: root.ID, Name: root.Name, Digest: trustRootDigest(root),
			Policy: policy, Enforcement: root.Enforcement,
		},
		TrustedMain:  RevisionRecord{Revision: trustedMain},
		Source:       RevisionRecord{Revision: source, Tree: tree},
		Containment:  fmt.Sprintf("ancestry: %s is an ancestor of trusted main %s, re-derived from the remote-observed head rather than a local ref", source, trustedMain),
		Kind:         ControllerAdopted,
		Version:      version,
		GOOS:         spec.GOOS,
		GOARCH:       spec.GOARCH,
		BuildFlags:   []string{"-trimpath", "CGO_ENABLED=0", "-ldflags -X main.buildKind/version/sourceRevision/sourceTree"},
		BinarySHA256: digest,
		BuiltAt:      deps.Now().UTC().Format(time.RFC3339),
		Builder:      BuilderRecord{Version: self.Version, SourceRevision: self.SourceRevision, Kind: firstNonEmpty(self.Kind, ControllerUnattested)},
		OutputPath:   output,
		SelfReport:   selfReport,
	}
	return out, nil
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

func runAdoptedBuild(spec AdoptedBuildSpec) error {
	ldflags := fmt.Sprintf("-X main.buildKind=%s -X main.version=%s -X main.sourceRevision=%s -X main.sourceTree=%s",
		spec.Kind, spec.Version, spec.Revision, spec.Tree)
	args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", spec.Output, "./cmd/zenchron-engineering"}
	cmd := exec.Command("go", args...)
	cmd.Dir = spec.SourceDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+spec.GOOS, "GOARCH="+spec.GOARCH)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

// probeControllerBuild asks the freshly built binary what it thinks it is.
func probeControllerBuild(binary, config string) (DoctorCheck, error) {
	cmd := exec.Command(binary, "autonomy", "doctor", "--config", config)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return DoctorCheck{}, err
	}
	var report DoctorReport
	if err := json.Unmarshal(out, &report); err != nil {
		return DoctorCheck{}, fmt.Errorf("the built binary did not return a doctor report")
	}
	check, found := report.Check("controller.build")
	if !found {
		return DoctorCheck{}, fmt.Errorf("the built binary's doctor report has no controller.build check")
	}
	return check, nil
}
