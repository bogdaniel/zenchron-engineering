package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const fixtureLock = `{"lockfileVersion":3}`

// frontendCacheFixture is an operator cache root holding one cache keyed by
// the fixture lockfile, plus a candidate that carries that lockfile.
type frontendCacheFixture struct {
	source, candidate, lockHash string
}

func newFrontendCacheFixture(t *testing.T) frontendCacheFixture {
	t.Helper()
	sum := sha256.Sum256([]byte(fixtureLock))
	f := frontendCacheFixture{source: t.TempDir(), candidate: t.TempDir(), lockHash: hex.EncodeToString(sum[:])}
	cache := filepath.Join(f.source, f.lockHash)
	if err := os.MkdirAll(filepath.Join(cache, "content-v2"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "content-v2", "tarball"), []byte("operator bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	f.writeLock(t, fixtureLock)
	return f
}

func (f frontendCacheFixture) writeLock(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(f.candidate, "frontend")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendCacheIsProvisionedIntoScratchAndNeverWrittenBack(t *testing.T) {
	f := newFrontendCacheFixture(t)
	scratch := t.TempDir()
	env, observation, err := provisionFrontendCache(f.source, f.candidate, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Status != domain.FrontendCacheProvisioned || observation.LockSHA256 != f.lockHash {
		t.Fatalf("observation = %#v", observation)
	}
	copied, err := os.ReadFile(filepath.Join(scratch, "npm-cache", "content-v2", "tarball"))
	if err != nil || string(copied) != "operator bytes" {
		t.Fatalf("matching cache was not copied: %q %v", copied, err)
	}
	settings, err := os.ReadFile(filepath.Join(scratch, "npmrc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cache=" + filepath.Join(scratch, "npm-cache"), "offline=true", "ignore-scripts=true"} {
		if !strings.Contains(string(settings), want) {
			t.Fatalf("npm settings lack %q:\n%s", want, settings)
		}
	}
	for _, entry := range env {
		if strings.Contains(entry, f.source) {
			t.Fatalf("the worker was handed the operator's cache path: %s", entry)
		}
	}
	// The worker may scribble in its copy; the operator's cache is untouched.
	if err := os.WriteFile(filepath.Join(scratch, "npm-cache", "content-v2", "tarball"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(f.source, f.lockHash, "content-v2", "tarball"))
	if string(original) != "operator bytes" {
		t.Fatalf("the operator's cache changed: %q", original)
	}
}

func TestFrontendCacheForAnotherLockfileIsNeverUsed(t *testing.T) {
	f := newFrontendCacheFixture(t)
	f.writeLock(t, `{"lockfileVersion":3,"packages":{"":{"name":"changed"}}}`)
	scratch := t.TempDir()
	env, observation, err := provisionFrontendCache(f.source, f.candidate, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Status != domain.FrontendCacheMissing || observation.LockSHA256 == f.lockHash {
		t.Fatalf("a mismatched lockfile was served the cache for another one: %#v", observation)
	}
	if entries, _ := os.ReadDir(filepath.Join(scratch, "npm-cache")); len(entries) != 0 {
		t.Fatalf("a mismatched cache was copied: %v", entries)
	}
	// npm still runs offline against the empty cache, so it fails with
	// ENOTCACHED rather than reaching for a registry.
	settings, _ := os.ReadFile(filepath.Join(scratch, "npmrc"))
	if !strings.Contains(string(settings), "offline=true") || len(env) != 2 {
		t.Fatalf("a missing cache left npm able to go online: env=%v\n%s", env, settings)
	}
}

func TestFrontendCacheDoesNotApplyWithoutASourceOrALockfile(t *testing.T) {
	f := newFrontendCacheFixture(t)
	if env, observation, err := provisionFrontendCache("", f.candidate, t.TempDir()); env != nil || observation != nil || err != nil {
		t.Fatalf("no cache source must be a no-op: %v %v %v", env, observation, err)
	}
	goOnly := t.TempDir()
	if env, observation, err := provisionFrontendCache(f.source, goOnly, t.TempDir()); env != nil || observation != nil || err != nil {
		t.Fatalf("a workspace without a frontend lockfile must be a no-op: %v %v %v", env, observation, err)
	}
	if _, observation, err := provisionFrontendCache(f.source, f.candidate, ""); err != nil || observation.Status != domain.FrontendCacheScratchRequired {
		t.Fatalf("no scratch must be reported, not hidden: %#v %v", observation, err)
	}
}

func TestFrontendCacheRefusesWhatItCannotCopyFaithfully(t *testing.T) {
	t.Run("a link inside the cache", func(t *testing.T) {
		f := newFrontendCacheFixture(t)
		if err := os.Symlink("/etc/hosts", filepath.Join(f.source, f.lockHash, "link")); err != nil {
			t.Skip(err)
		}
		if _, _, err := provisionFrontendCache(f.source, f.candidate, t.TempDir()); err == nil {
			t.Fatal("a cache holding a link was copied")
		}
	})
	t.Run("a cache that is itself a link", func(t *testing.T) {
		f := newFrontendCacheFixture(t)
		broader := t.TempDir()
		real := filepath.Join(f.source, f.lockHash)
		if err := os.Rename(real, filepath.Join(broader, "cache")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(broader, "cache"), real); err != nil {
			t.Skip(err)
		}
		if _, _, err := provisionFrontendCache(f.source, f.candidate, t.TempDir()); err == nil {
			t.Fatal("a linked cache directory was followed")
		}
	})
	t.Run("an unreadable lockfile", func(t *testing.T) {
		f := newFrontendCacheFixture(t)
		path := filepath.Join(f.candidate, "frontend", "package-lock.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := provisionFrontendCache(f.source, f.candidate, t.TempDir()); err == nil {
			t.Fatal("an unreadable lockfile was treated as absent")
		}
	})
}

func TestConcurrentInvocationsNeverShareAWritableCache(t *testing.T) {
	f := newFrontendCacheFixture(t)
	first, second := t.TempDir(), t.TempDir()
	for _, scratch := range []string{first, second} {
		if _, _, err := provisionFrontendCache(f.source, f.candidate, scratch); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(first, "npm-cache", "content-v2", "tarball"), []byte("first only"), 0600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(second, "npm-cache", "content-v2", "tarball"))
	if string(got) != "operator bytes" {
		t.Fatalf("one invocation's write reached another: %q", got)
	}
}

// TestCodexKeepsItsSandboxWhileReceivingTheOfflineCache runs the real
// CLIAgentProvider.Execute path, which is the one place the process argv and
// environment are assembled, and asserts what the process was actually handed.
func TestCodexKeepsItsSandboxWhileReceivingTheOfflineCache(t *testing.T) {
	f := newFrontendCacheFixture(t)
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	provider.Agent.FrontendCacheDir = f.source
	request.CandidateDir = f.candidate
	request.ScratchDir = filepath.Join(t.TempDir(), "scratch")

	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	call := fake.execution(t)
	args := strings.Join(call.args, "\x00")
	for _, want := range []string{"--sandbox\x00workspace-write", "sandbox_workspace_write.network_access=false", "--ask-for-approval\x00never"} {
		if !strings.Contains(args, want) {
			t.Fatalf("the sandbox changed: %q missing from %q", want, call.args)
		}
	}
	if strings.Contains(args, f.source) {
		t.Fatalf("the operator cache is a writable root or argument: %q", call.args)
	}
	for _, name := range []string{"NPM_CONFIG_USERCONFIG", "NPM_CONFIG_GLOBALCONFIG"} {
		if !slices.ContainsFunc(call.env, func(e string) bool { return strings.HasPrefix(e, name+"=") }) {
			t.Fatalf("%s was not passed: %v", name, call.env)
		}
	}
	for _, entry := range call.env {
		if strings.Contains(entry, f.source) || strings.HasPrefix(entry, "NPM_TOKEN") {
			t.Fatalf("host cache or credential leaked into the worker environment: %s", entry)
		}
	}
	cache := result.Invocation.FrontendCache
	if cache == nil || cache.Status != domain.FrontendCacheProvisioned || cache.LockSHA256 != f.lockHash {
		t.Fatalf("provenance = %#v", cache)
	}
	if result.Invocation.PermissionBypass || result.Invocation.SandboxMode != "workspace-write" {
		t.Fatalf("the permission boundary moved: %#v", result.Invocation)
	}
	if err := validateInvocationObservation(result.Invocation.InvocationObservation); err != nil {
		t.Fatal(err)
	}
}

func TestInvocationWithoutAFrontendLockfileCarriesNoCacheFacts(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	provider.Agent.FrontendCacheDir = t.TempDir()
	request.ScratchDir = filepath.Join(t.TempDir(), "scratch")
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Invocation.FrontendCache != nil {
		t.Fatalf("a Go-only workspace recorded cache facts: %#v", result.Invocation.FrontendCache)
	}
	for _, entry := range fake.execution(t).env {
		if strings.HasPrefix(entry, "NPM_CONFIG_") {
			t.Fatalf("a Go-only workspace received npm configuration: %s", entry)
		}
	}
}

func TestFrontendCacheProvenanceIsBounded(t *testing.T) {
	ok := domain.FrontendCacheObservation{LockSHA256: strings.Repeat("a", 64), Status: domain.FrontendCacheProvisioned, NodeVersion: "24.15.0", NpmVersion: "11.12.1"}
	if err := validateInvocationObservation(domain.InvocationObservation{FrontendCache: &ok}); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]domain.FrontendCacheObservation{
		"lock identity": {LockSHA256: "nothex", Status: domain.FrontendCacheProvisioned},
		"status":        {LockSHA256: ok.LockSHA256, Status: "online_fallback"},
	} {
		if err := validateInvocationObservation(domain.InvocationObservation{FrontendCache: &bad}); err == nil {
			t.Fatalf("an invalid %s was accepted", name)
		}
	}
}

func TestFrontendCacheDirIsOnlyForNativeCLIAgents(t *testing.T) {
	native := AgentConfig{Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted), FrontendCacheDir: "relative/cache"}
	if _, err := resolveAgent("codex", native); err == nil {
		t.Fatal("a relative cache directory was accepted")
	}
	native.FrontendCacheDir = "/srv/npm-cache"
	if agent, err := resolveAgent("codex", native); err != nil || agent.FrontendCacheDir != "/srv/npm-cache" {
		t.Fatalf("%#v %v", agent, err)
	}
	for _, kind := range []string{AgentKindOpenAIResponses} {
		trust, known := agentKinds[kind]
		if !known {
			continue
		}
		brokered := AgentConfig{Kind: kind, TrustMode: string(trust), Model: "m", CredentialPath: "/srv/credential", FrontendCacheDir: "/srv/npm-cache"}
		if _, err := resolveAgent("api", brokered); err == nil {
			t.Fatalf("a brokered %s agent accepted a frontend cache", kind)
		}
	}
}

// TestFrontendBuildIsReproducibleFromTheOperatorCache is the opt-in end-to-end
// proof against a real npm and a real cache root:
//
//	ZENCHRON_FRONTEND_CACHE_ROOT=$HOME/.zenchron-npm-cache go test ./runtime -run TestFrontendBuildIsReproducible
//
// It installs offline from the copy the runtime provisions, runs the type
// check, the frontend unit tests, and builds the bundle twice.
func TestFrontendBuildIsReproducibleFromTheOperatorCache(t *testing.T) {
	root := os.Getenv("ZENCHRON_FRONTEND_CACHE_ROOT")
	if root == "" {
		t.Skip("ZENCHRON_FRONTEND_CACHE_ROOT names an operator cache root; unset")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	repository, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if out, err := exec.Command("cp", "-R", filepath.Join(repository, "frontend"), workspace).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(workspace, "frontend", "node_modules")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "controlplane", "assets", "design"), 0700); err != nil {
		t.Fatal(err)
	}
	env, observation, err := provisionFrontendCache(root, workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Status != domain.FrontendCacheProvisioned {
		t.Fatalf("no cache for this lockfile under %s: %#v", root, observation)
	}
	npm := func(args ...string) {
		t.Helper()
		cmd := exec.Command("npm", args...)
		cmd.Dir = filepath.Join(workspace, "frontend")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("npm %v: %v\n%s", args, err, out)
		}
	}
	npm("ci", "--ignore-scripts")
	npm("test")
	digest := func() string {
		sum := sha256.New()
		for _, name := range []string{"react.js", "tokens.css", "layout.css", "components.css", "runtime.css"} {
			data, err := os.ReadFile(filepath.Join(workspace, "controlplane", "assets", "design", name))
			if err != nil {
				t.Fatal(err)
			}
			sum.Write(data)
		}
		return hex.EncodeToString(sum.Sum(nil))
	}
	npm("run", "build")
	first := digest()
	npm("run", "build")
	if second := digest(); first != second {
		t.Fatalf("two builds from the same cache differ: %s %s", first, second)
	}
}
