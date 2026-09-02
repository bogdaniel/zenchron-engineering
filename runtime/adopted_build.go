package runtime

import (
	"context"
	"crypto/sha1"
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
	// The adoption policy is FROZEN, not a parameter. A caller that could
	// weaken the trusted ref, the required check, the allowed merge methods or
	// the strict-check rule and still receive an artifact labelled adopted
	// would make the label mean whatever the caller wanted. There is exactly
	// one adoption policy, and this is where it comes from.
	policy := DefaultTrustPolicy()
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
	startingDigest, err := trustRootDigest(root)
	if err != nil {
		return out, err
	}

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
	defer func() { _ = os.RemoveAll(checkout) }() // best-effort: a leftover temp dir is not a proof failure
	if err := materializeTree(deps, request.RepositoryDir, tree, checkout); err != nil {
		return out, err
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
			_ = os.RemoveAll(staging) // best-effort: the refusal already stands
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
		// One expected cause is bootstrapping: a trusted revision that predates
		// `controller inspect-self` cannot answer, and the builder refuses
		// rather than recording an unverified artifact. That is the corrected
		// law working, and it clears as soon as a revision carrying the command
		// is the one being built from.
		return out, fmt.Errorf("the built binary could not report its own provenance, so it may not claim adoption "+
			"(a source revision predating `controller inspect-self` cannot answer): %w", err)
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
	finalDigest, err := trustRootDigest(finalRoot)
	if err != nil {
		return out, err
	}
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
	if err := publishControllerVersion(staging, final); err != nil {
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
	root, err := selectTrustRoot(rulesets, policy.Ref)
	if err != nil {
		return TrustedMainRuleset{}, err
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
func publishControllerVersion(staging, final string) error {
	if _, err := os.Lstat(final); err == nil {
		return errImmutableVersion(final)
	}
	if err := os.Rename(staging, final); err != nil {
		// A concurrent publisher may have created it between the check and the
		// rename. That is the same immutability answer, not a race to win.
		if _, statErr := os.Lstat(final); statErr == nil {
			return errImmutableVersion(final)
		}
		return fmt.Errorf("the controller version directory could not be published atomically, so nothing was installed: %w", err)
	}
	return nil
}

// errImmutableVersion is the whole existing-version policy.
//
// It deliberately does not compare the installed artifact and declare a match
// idempotent. Two builds of the same source differ in their timestamps and in
// the trust observations they were published under, so "equivalent" would have
// to be a semantic judgement about two historical records - and a wrong
// judgement silently replaces the provenance of every run the installed
// controller has already governed. Refusing is both safer and simpler, and an
// operator who wants another proof run can name a different output root.
func errImmutableVersion(final string) error {
	return fmt.Errorf("%s already exists; controller version directories are immutable and are never inspected, replaced or reconciled. Use a different --output root for another proof run", final)
}

// selectTrustRoot picks the ruleset governing the branch. More than one may
// exist; the one that targets the ref is the one that matters.
// It requires EXACTLY ONE. Zero means there is no gate. More than one means
// GitHub is composing rules whose combined effect this builder would have to
// guess, and guessing the composition of overlapping rulesets is how a gate
// gets reported that is not the gate being enforced. M1-B refuses instead of
// implementing a rules engine it cannot prove.
func selectTrustRoot(rulesets []TrustedMainRuleset, ref string) (TrustedMainRuleset, error) {
	var applicable []TrustedMainRuleset
	for _, r := range rulesets {
		if trustRootContains(r.Targets, ref) && !trustRootContains(r.Excluded, ref) {
			applicable = append(applicable, r)
		}
	}
	switch len(applicable) {
	case 0:
		return TrustedMainRuleset{}, ErrNoTrustRoot
	case 1:
		return applicable[0], nil
	default:
		return TrustedMainRuleset{}, fmt.Errorf("%d rulesets govern %s; their combined effect is not something this builder can prove, so no source under them is called adopted", len(applicable), ref)
	}
}

// trustRootDigest is a canonical digest of the trust root as observed, so the
// provenance names WHICH gate was in force and a later reader can tell whether
// it has since changed.
func trustRootDigest(root TrustedMainRuleset) (string, error) {
	return canonicalDigest(root)
}

// materializeTree writes the EXACT bytes the commit tree names.
//
// This replaces `git archive | tar`, which was wrong twice over. It ran two
// programs found on the ambient PATH, and archive is a PRESENTATION of a tree
// rather than the tree itself: export-ignore drops paths, export-subst
// rewrites content, and working-tree encoding transforms bytes. A build
// approved against tree T could therefore be compiled from something that is
// not T while every later check still named T.
//
// Instead the tree is read through the controlled Git seam - the trusted
// binary, an environment built from scratch, no system or global
// configuration, no external diff or filters - and each blob is written here
// by Go. Nothing consults .gitattributes because nothing checks anything out:
// there is no working tree for a smudge filter to act on.
//
// Each blob is then verified against the object id the tree named, so the
// materialization is not trusted either.
func materializeTree(deps AdoptedBuildDeps, repoDir, tree, into string) error {
	// Object ids are verified by recomputing them, so the hash must be the one
	// this code knows how to compute. Anything else fails closed.
	format, err := deps.Git(repoDir, "rev-parse", "--show-object-format")
	if err != nil {
		return fmt.Errorf("the repository object format could not be established: %w", err)
	}
	if strings.TrimSpace(format) != "sha1" {
		return fmt.Errorf("this builder verifies sha1 object ids; the repository uses %q", strings.TrimSpace(format))
	}
	listing, err := deps.Git(repoDir, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return fmt.Errorf("the tree %s could not be listed: %w", tree, err)
	}
	entries := strings.Split(strings.TrimRight(listing, "\x00"), "\x00")
	written := 0
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		meta, path, found := strings.Cut(entry, "\t")
		if !found {
			return fmt.Errorf("the tree listing is malformed")
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return fmt.Errorf("the tree listing is malformed")
		}
		mode, object := fields[0], fields[2]
		if err := safeTreePath(path); err != nil {
			return err
		}
		target := filepath.Join(into, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		content, err := deps.Git(repoDir, "cat-file", "blob", object)
		if err != nil {
			return fmt.Errorf("object %s could not be read: %w", object, err)
		}
		if got := blobID(content); got != object {
			return fmt.Errorf("object %s materialized as %s; the checkout is not the tree it claims", object, got)
		}
		switch mode {
		case "100644", "100755":
			perm := os.FileMode(0600)
			if mode == "100755" {
				perm = 0700
			}
			if err := os.WriteFile(target, []byte(content), perm); err != nil {
				return err
			}
		case "120000":
			if err := os.Symlink(content, target); err != nil {
				return err
			}
		case "160000":
			// A submodule is a pointer to another repository. Its content is
			// not in this tree, so a build from it is not a build of this tree.
			return fmt.Errorf("tree %s contains submodule %q, whose content this tree does not name", tree, path)
		default:
			return fmt.Errorf("tree %s contains %q with unsupported mode %s", tree, path, mode)
		}
		written++
	}
	if written == 0 {
		return fmt.Errorf("tree %s materialized no files", tree)
	}
	return nil
}

// safeTreePath refuses any path that would escape the build checkout or write
// into Git's own metadata.
func safeTreePath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || filepath.IsAbs(path) {
		return fmt.Errorf("refused tree path %q", path)
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." || element == ".git" {
			return fmt.Errorf("refused tree path %q", path)
		}
	}
	return nil
}

// blobID recomputes Git's object id for content, so a materialized file is
// checked against the id the tree named rather than assumed correct.
func blobID(content string) string {
	sum := sha1.New()
	fmt.Fprintf(sum, "blob %d\x00", len(content))
	_, _ = io.WriteString(sum, content)
	return hex.EncodeToString(sum.Sum(nil))
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// WriteAdoptedBuildProvenance stores the record owner-only beside the binary.
//
// The returned digest is an EXACT-FILE digest of the bytes written, not a
// canonical digest of the document's meaning. The file is indented for a human
// reader, so re-serializing it differently would produce different bytes and
// the same meaning. The semantic sub-object digests inside it - the trust root
// and the build environment - go through CanonicalJSON and are comparable
// across observations; this one answers "is this file the file that was
// written", which is a different and equally useful question. Calling it
// canonical would be calling it something it is not.
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
	// dockerBase masks /candidate/.git with a tmpfs, which needs the mount
	// point to exist inside a read-only bind. A git-archive export has no
	// .git, so the empty directory is created here purely as that mount point.
	// It is created AFTER the tree was recomputed, so it cannot affect the
	// content the build was approved for, and the tmpfs hides it from the
	// compiler anyway.
	if err := os.MkdirAll(filepath.Join(spec.SourceDir, ".git"), 0700); err != nil {
		return BuildEnvironment{}, fmt.Errorf("the build checkout could not be prepared for the sandbox boundary: %w", err)
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
	digest, err := canonicalDigest(built)
	if err != nil {
		return BuildEnvironment{}, err
	}
	built.Digest = digest
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
// JSON convention. A canonicalization failure is an ERROR, never a fallback to
// encoding/json: a digest that silently stopped being canonical still looks
// like one, and two records that should compare equal would not.
func canonicalDigest(value any) (string, error) {
	encoded, err := CanonicalJSON(value)
	if err != nil {
		return "", fmt.Errorf("a trust-relevant digest could not be canonicalized: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func measureExecutable(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
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
