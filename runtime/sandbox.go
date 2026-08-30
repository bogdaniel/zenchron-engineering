package runtime

// This file contains the deliberately narrow M0 local adapters.  Assurance only
// runs inside Docker with a pinned image and denies execution when Docker cannot
// provide that boundary.  A local host-process fallback would invalidate the
// runtime's credential, controller, and cross-run isolation guarantees.
// Execution providers are not forced to be Docker-shaped: see
// native_codex.go, whose control plane must reach a remote AI provider.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var ErrSandboxUnavailable = fmt.Errorf("required sandbox capability is unavailable or unenforceable")

type CommandOutput struct {
	Stdout, Stderr []byte
	ExitCode       int
}
type CommandExecutor interface {
	LookPath(string) error
	Run(context.Context, string, []string, string, []string, time.Duration) (CommandOutput, error)
	Output(context.Context, string, []string, string, []string, time.Duration) (CommandOutput, error)
}
type OSCommandExecutor struct{}

func (OSCommandExecutor) LookPath(name string) error { _, err := exec.LookPath(name); return err }
func (OSCommandExecutor) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := runBoundedProcess(ctx, cmd, grace)
	result := CommandOutput{Stdout: []byte(out.String()), Stderr: []byte(errOut.String())}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
	}
	return result, err
}
func (OSCommandExecutor) Output(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	return OSCommandExecutor{}.Run(ctx, name, args, dir, env, grace)
}

type DockerSandbox struct {
	Image, OperationID, StateDir string
	// Endpoint is trusted runtime/controller configuration. It is intentionally
	// translated into Docker CLI control-plane arguments, never container env.
	Endpoint DockerEndpoint
	Executor CommandExecutor
	Grace    time.Duration
}

// DockerEndpoint identifies the daemon the trusted runtime is allowed to
// control. An empty Host retains Docker's local default, but ambient
// DOCKER_HOST and Docker configuration are never inherited by runtime calls.
// M0 deliberately supports only direct local sockets and unauthenticated TCP
// endpoints supplied by the operator; credential-bearing endpoint URLs and
// Docker's SSH transport are not an implicit authority channel.
type DockerEndpoint struct{ Host string }

func (e DockerEndpoint) identity() (string, error) {
	if e.Host == "" {
		return "local-default", nil
	}
	u, err := url.Parse(e.Host)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid trusted Docker endpoint")
	}
	switch u.Scheme {
	case "tcp":
		if u.Host == "" || u.Path != "" {
			return "", fmt.Errorf("invalid trusted Docker endpoint")
		}
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return "", fmt.Errorf("invalid trusted Docker endpoint")
		}
	default:
		return "", fmt.Errorf("unsupported trusted Docker endpoint scheme %q", u.Scheme)
	}
	return e.Host, nil
}

func (e DockerEndpoint) args(args []string) ([]string, error) {
	if _, err := e.identity(); err != nil {
		return nil, err
	}
	if e.Host == "" {
		return append([]string(nil), args...), nil
	}
	return append([]string{"--host", e.Host}, args...), nil
}

func (s DockerSandbox) executor() CommandExecutor {
	if s.Executor == nil {
		return OSCommandExecutor{}
	}
	return s.Executor
}
func (s DockerSandbox) ready() error {
	if s.Image == "" || !strings.HasPrefix(s.Image, "sha256:") {
		return ErrSandboxUnavailable
	}
	if s.executor().LookPath("docker") != nil {
		return ErrSandboxUnavailable
	}
	if _, err := s.Endpoint.identity(); err != nil {
		return ErrSandboxUnavailable
	}
	_, err := s.dockerRun(context.Background(), []string{"info", "--format", "{{.ServerVersion}}"})
	if err != nil {
		return ErrSandboxUnavailable
	}
	image, err := s.dockerOutput(context.Background(), []string{"image", "inspect", "--format", "{{.Id}}", s.Image})
	if err != nil || strings.TrimSpace(string(image.Stdout)) != s.Image {
		return ErrSandboxUnavailable
	}
	return nil
}
func (s DockerSandbox) dockerRun(ctx context.Context, args []string) (CommandOutput, error) {
	bound, err := s.Endpoint.args(args)
	if err != nil {
		return CommandOutput{}, err
	}
	return s.executor().Run(ctx, "docker", bound, "", []string{}, s.Grace)
}
func (s DockerSandbox) dockerOutput(ctx context.Context, args []string) (CommandOutput, error) {
	bound, err := s.Endpoint.args(args)
	if err != nil {
		return CommandOutput{}, err
	}
	return s.executor().Output(ctx, "docker", bound, "", []string{}, s.Grace)
}
func (s DockerSandbox) daemonIdentity() (string, error) {
	out, err := s.dockerOutput(context.Background(), []string{"info", "--format", "{{.ID}}"})
	if err != nil || strings.TrimSpace(string(out.Stdout)) == "" {
		return "", fmt.Errorf("trusted Docker daemon identity unavailable")
	}
	return strings.TrimSpace(string(out.Stdout)), nil
}
func (s DockerSandbox) run(ctx context.Context, args []string) (CommandOutput, error) {
	if err := s.ready(); err != nil {
		return CommandOutput{}, err
	}
	if s.Grace <= 0 {
		s.Grace = 5 * time.Second
	}
	return s.runContainer(ctx, args)
}

// dockerOperationRecord is runtime-owned state, deliberately outside candidate
// workspaces.  It is written before create so a crashed controller retains the
// exact name it alone is permitted to reconcile; recovery never searches by a
// broad name prefix or label.
type dockerOperationRecord struct {
	OperationID, ContainerName, Image, Endpoint, DaemonID, Phase string
}

func (s DockerSandbox) operationRecordPath() (string, error) {
	if s.OperationID == "" || s.StateDir == "" {
		return "", fmt.Errorf("runtime-owned Docker operation identity and state directory are required")
	}
	endpoint, err := s.Endpoint.identity()
	if err != nil {
		return "", err
	}
	name := sha256.Sum256([]byte(s.OperationID + "\x00" + s.Image + "\x00" + endpoint))
	return filepath.Join(s.StateDir, "docker-operation-"+hex.EncodeToString(name[:])+".json"), nil
}

func (s DockerSandbox) operationRecord() (dockerOperationRecord, string, error) {
	path, err := s.operationRecordPath()
	if err != nil {
		return dockerOperationRecord{}, "", err
	}
	endpoint, err := s.Endpoint.identity()
	if err != nil {
		return dockerOperationRecord{}, "", err
	}
	daemonID, err := s.daemonIdentity()
	if err != nil {
		return dockerOperationRecord{}, "", err
	}
	nameDigest := sha256.Sum256([]byte("zenchron-container\x00" + s.OperationID + "\x00" + s.Image + "\x00" + endpoint + "\x00" + daemonID))
	return dockerOperationRecord{OperationID: s.OperationID, ContainerName: "zenchron-" + hex.EncodeToString(nameDigest[:12]), Image: s.Image, Endpoint: endpoint, DaemonID: daemonID, Phase: "planned"}, path, nil
}

func writeDockerOperation(path string, record dockerOperationRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".docker-operation-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func dockerCreateArgs(runArgs []string, name, operation string) ([]string, error) {
	if len(runArgs) == 0 || runArgs[0] != "run" {
		return nil, fmt.Errorf("Docker sandbox requires a docker run specification")
	}
	args := []string{"create", "--name", name, "--label", "zenchron.runtime.operation=" + operation}
	for _, arg := range runArgs[1:] {
		// Explicit removal is performed only after inspect/wait proves the exact
		// container stopped. sig-proxy belongs to `run`, not `create`.
		if arg == "--rm" || arg == "--sig-proxy=true" {
			continue
		}
		args = append(args, arg)
	}
	return args, nil
}

func (s DockerSandbox) inspectRunning(name string) (bool, bool, error) {
	out, err := s.dockerOutput(context.Background(), []string{"inspect", "--format", "{{.State.Running}}", name})
	if err != nil {
		if strings.Contains(strings.ToLower(string(out.Stderr)), "no such") {
			return false, false, nil
		}
		return false, false, err
	}
	return strings.TrimSpace(string(out.Stdout)) == "true", true, nil
}

func (s DockerSandbox) removeExact(name string) error {
	_, err := s.dockerRun(context.Background(), []string{"rm", name})
	return err
}

func (s DockerSandbox) terminateExact(record dockerOperationRecord, path string) error {
	if s.Grace <= 0 {
		s.Grace = 5 * time.Second
	}
	record.Phase = "graceful_stop_requested"
	if err := writeDockerOperation(path, record); err != nil {
		return err
	}
	_, _ = s.dockerRun(context.Background(), []string{"kill", "--signal", "TERM", record.ContainerName})
	deadline := time.NewTimer(s.Grace)
	poll := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer poll.Stop()
	for {
		running, exists, err := s.inspectRunning(record.ContainerName)
		if err != nil {
			return fmt.Errorf("inspect runtime-owned Docker container: %w", err)
		}
		if !exists || !running {
			break
		}
		select {
		case <-deadline.C:
			record.Phase = "force_kill_requested"
			if err := writeDockerOperation(path, record); err != nil {
				return err
			}
			if _, err := s.dockerRun(context.Background(), []string{"kill", "--signal", "KILL", record.ContainerName}); err != nil {
				return err
			}
		case <-poll.C:
			continue
		}
		break
	}
	// docker wait reaps Docker's execution result; rm is deliberately separate.
	if _, err := s.dockerRun(context.Background(), []string{"wait", record.ContainerName}); err != nil {
		return err
	}
	if err := s.removeExact(record.ContainerName); err != nil {
		return err
	}
	running, exists, err := s.inspectRunning(record.ContainerName)
	if err != nil {
		return err
	}
	if exists || running {
		return fmt.Errorf("runtime-owned Docker container %q remains after cleanup", record.ContainerName)
	}
	record.Phase = "removed"
	return writeDockerOperation(path, record)
}

func (s DockerSandbox) runContainer(ctx context.Context, args []string) (CommandOutput, error) {
	record, path, err := s.operationRecord()
	if err != nil {
		return CommandOutput{}, err
	}
	if err := writeDockerOperation(path, record); err != nil {
		return CommandOutput{}, err
	}
	createArgs, err := dockerCreateArgs(args, record.ContainerName, record.OperationID)
	if err != nil {
		return CommandOutput{}, err
	}
	record.Phase = "create_requested"
	if err := writeDockerOperation(path, record); err != nil {
		return CommandOutput{}, err
	}
	if _, err := s.dockerRun(context.Background(), createArgs); err != nil {
		return CommandOutput{}, err
	}
	record.Phase = "created"
	if err := writeDockerOperation(path, record); err != nil {
		return CommandOutput{}, err
	}
	var once sync.Once
	cleanupDone := make(chan error, 1)
	cleanup := func() {
		once.Do(func() { cleanupDone <- s.terminateExact(record, path) })
	}
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cleanup()
		case <-stopWatch:
		}
	}()
	// The CLI is still a runtime-owned host process and receives normal bounded
	// cleanup. The concurrent exact-container cleanup below is the proof that
	// the Docker workload ended; CLI termination alone is never treated as such.
	out, runErr := s.dockerRun(ctx, []string{"start", "--attach", record.ContainerName})
	close(stopWatch)
	if ctx.Err() != nil {
		cleanup()
		if cleanupErr := <-cleanupDone; cleanupErr != nil {
			return out, cleanupErr
		}
		return out, runErr
	}
	// A natural exit is not delayed: it is inspected and removed immediately.
	running, exists, inspectErr := s.inspectRunning(record.ContainerName)
	if inspectErr != nil {
		return out, inspectErr
	}
	if exists && running {
		return out, fmt.Errorf("Docker start returned while runtime-owned container remains running")
	}
	if exists {
		if _, err := s.dockerRun(context.Background(), []string{"wait", record.ContainerName}); err != nil {
			return out, err
		}
		if err := s.removeExact(record.ContainerName); err != nil {
			return out, err
		}
	}
	record.Phase = "removed"
	if err := writeDockerOperation(path, record); err != nil {
		return out, err
	}
	return out, runErr
}

// DockerReconciliation is intentionally conservative. A record is the sole
// authority to target a container; missing state never authorizes discovery by
// name pattern. Docker daemon/inspect uncertainty remains ambiguous for the
// scheduler to surface rather than being retried as if nothing happened.
type DockerReconciliation string

const (
	DockerNoContainer DockerReconciliation = "no_container_recorded"
	DockerRemoved     DockerReconciliation = "removed"
	DockerAmbiguous   DockerReconciliation = "ambiguous"
)

func (s DockerSandbox) ReconcileDockerOperation() (DockerReconciliation, error) {
	path, err := s.operationRecordPath()
	if err != nil {
		return DockerNoContainer, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DockerNoContainer, nil
	}
	if err != nil {
		return DockerAmbiguous, err
	}
	var record dockerOperationRecord
	endpoint, endpointErr := s.Endpoint.identity()
	daemonID, daemonErr := s.daemonIdentity()
	if err := json.Unmarshal(data, &record); err != nil || endpointErr != nil || daemonErr != nil || record.ContainerName == "" || record.OperationID != s.OperationID || record.Endpoint != endpoint || record.DaemonID != daemonID {
		return DockerAmbiguous, fmt.Errorf("invalid runtime-owned Docker operation record")
	}
	running, exists, err := s.inspectRunning(record.ContainerName)
	if err != nil {
		return DockerAmbiguous, err
	}
	if !exists {
		record.Phase = "removed"
		return DockerRemoved, writeDockerOperation(path, record)
	}
	if running {
		if err := s.terminateExact(record, path); err != nil {
			return DockerAmbiguous, err
		}
		return DockerRemoved, nil
	}
	if _, err := s.dockerRun(context.Background(), []string{"wait", record.ContainerName}); err != nil {
		return DockerAmbiguous, err
	}
	if err := s.removeExact(record.ContainerName); err != nil {
		return DockerAmbiguous, err
	}
	record.Phase = "removed"
	if err := writeDockerOperation(path, record); err != nil {
		return DockerAmbiguous, err
	}
	return DockerRemoved, nil
}

// SandboxDoctor is capability evidence, not a promise based on a configuration
// string. ProviderSandbox reports the native Codex CLI's proven sandbox
// capability; the remaining fields report Docker, which isolates assurance.
// They are separate values because they are separate boundaries.
type SandboxDoctor struct{ ProviderSandbox, VerifierSandbox, OfflineVerification, DependencyPreparation string }

func DiagnoseSandbox(p NativeCodexProvider, s DockerSandbox) SandboxDoctor {
	provider, assurance := "unavailable", "unavailable"
	if p.probe(context.Background()) == nil {
		provider = "enforceable"
	}
	if s.ready() == nil {
		assurance = "enforceable"
	}
	return SandboxDoctor{ProviderSandbox: provider, VerifierSandbox: assurance, OfflineVerification: assurance, DependencyPreparation: assurance}
}

// ArtifactStore keeps raw output local and records a separately sanitized
// candidate. Sanitization never makes output publishable by itself.
type ArtifactStore struct{ Root string }

func (s ArtifactStore) StoreTranscript(prefix string, stdout, stderr []byte) ([]Artifact, error) {
	if s.Root == "" {
		return nil, fmt.Errorf("artifact root required")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return nil, err
	}
	raw := append(append([]byte{}, stdout...), stderr...)
	rawPath := filepath.Join(s.Root, prefix+".raw.log")
	if err := os.WriteFile(rawPath, raw, 0600); err != nil {
		return nil, err
	}
	sanitizedPath := filepath.Join(s.Root, prefix+".sanitized-candidate.log")
	// Generic replacement is intentionally conservative: a later explicit
	// publication review must set Publishable after inspecting this candidate.
	sanitized := redactTranscript(raw)
	if err := os.WriteFile(sanitizedPath, sanitized, 0600); err != nil {
		return nil, err
	}
	rawArtifact, err := artifact(rawPath, false, false)
	if err != nil {
		return nil, err
	}
	sanitizedArtifact, err := artifact(sanitizedPath, true, false)
	if err != nil {
		return nil, err
	}
	return []Artifact{rawArtifact, sanitizedArtifact}, nil
}

var transcriptSecrets = regexp.MustCompile(`(?i)(github_pat_[a-z0-9_]+|ghp_[a-z0-9]+|aws_secret_access_key\s*=\s*[^\s]+|authorization:\s*bearer\s+[^\s]+)`)

func redactTranscript(raw []byte) []byte {
	return []byte(transcriptSecrets.ReplaceAllString(strings.ReplaceAll(string(raw), "\x00", "[NUL]"), "[REDACTED]"))
}
func artifact(path string, sanitized, publishable bool) (Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256(data)
	return Artifact{Path: path, SHA256: hex.EncodeToString(sum[:]), MediaType: "text/plain", LocalOnly: !sanitized, Sanitized: sanitized, Publishable: publishable}, nil
}

// ClassifyProviderFailure preserves the existing narrow capacity semantics:
// only recognizable transient-capacity diagnostics may consume retry/fallback
// budget. Every other provider failure is diagnosable and bounded as unknown.
func ClassifyProviderFailure(stdout, stderr []byte) FailureClass {
	diagnostic := strings.ToLower(string(stdout) + "\n" + string(stderr))
	for _, signal := range []string{"model is at capacity", "selected model is at capacity", "capacity. please try", "temporarily unavailable"} {
		if strings.Contains(diagnostic, signal) {
			return FailureTransientProvider
		}
	}
	return FailureUnknown
}
func providerPrompt(r ExecutionRequest) string {
	return fmt.Sprintf("Modify only %s. Run=%s source=%s controller=%s base=%s candidate=%s/%s contract=%s/%s purpose=%s. Objective: %s. Acceptance obligations: %s. Constraints: %s. Prohibitions: %s. Permissions: %s. Findings: %v. Do not access paths outside that workspace.", r.CandidateDir, r.RunID, r.SourceSnapshot.ID, r.ControllerID, r.Base.Revision, r.Candidate.Revision, r.Candidate.Tree, r.Contract.ID, r.Contract.Revision, r.Purpose, r.Objective, strings.Join(r.AcceptanceObligations, "; "), strings.Join(r.Constraints, "; "), strings.Join(r.Prohibitions, "; "), strings.Join(r.Permissions, "; "), r.Findings)
}

// dockerBase creates the complete host boundary: only candidate is writable;
// Docker's root is read-only, capabilities are dropped, networking is absent,
// and the environment is an explicit empty allowlist. Callers state their own
// --workdir; dockerBase deliberately asserts no working directory of its own.
func dockerBase(candidate string, candidateReadOnly bool) []string {
	mount := "type=bind,src=" + candidate + ",dst=/candidate"
	if candidateReadOnly {
		mount += ",readonly"
	}
	return []string{"run", "--init", "--sig-proxy=true", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,mode=1777", "--tmpfs", "/home:rw,nosuid,nodev,noexec,mode=0700", "--tmpfs", "/candidate/.git:rw,nosuid,nodev,noexec,mode=0700", "--env", "HOME=/home", "--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "--mount", mount}
}

type DependencyUnavailableError struct{ Detail string }

func (e *DependencyUnavailableError) Error() string {
	return "dependency preparation unavailable: " + e.Detail
}

type BaselineGoVerifier struct {
	Sandbox            DockerSandbox
	ArtifactStore      ArtifactStore
	DependencyCacheDir string
}

func (v BaselineGoVerifier) Definition() string {
	d, _ := Digest(struct{ Name, Version string }{"baseline-go-offline", "gofmt-vet-test-v1"})
	return d
}
func (v BaselineGoVerifier) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	if request.Commit == "" || request.Tree == "" || request.CheckoutDir == "" || request.Contract.ID == "" {
		return AssuranceResult{}, fmt.Errorf("incomplete assurance binding")
	}
	if v.ArtifactStore.Root == "" {
		return AssuranceResult{}, fmt.Errorf("local artifact store required")
	}
	if err := os.MkdirAll(v.ArtifactStore.Root, 0700); err != nil {
		return AssuranceResult{}, err
	}
	if request.VerifierDefinition != "" && request.VerifierDefinition != v.Definition() {
		return AssuranceResult{}, fmt.Errorf("verifier definition mismatch")
	}
	if tree, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD^{tree}"); err != nil || strings.TrimSpace(tree) != request.Tree {
		return AssuranceResult{}, fmt.Errorf("exact candidate tree unavailable")
	}
	if commit, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(commit) != request.Commit {
		return AssuranceResult{}, fmt.Errorf("exact candidate commit unavailable")
	}
	if err := v.prepare(ctx, request.CheckoutDir); err != nil {
		return AssuranceResult{ProviderID: "baseline-go", VerifierDefinition: v.Definition(), FailureClass: FailureTransientInfrastructure}, err
	}
	args := dockerBase(request.CheckoutDir, true)
	args = append(args, "--mount", "type=bind,src="+v.DependencyCacheDir+",dst=/cache,readonly", "--workdir", "/candidate", "--env", "GOMODCACHE=/cache", "--env", "GOPROXY=off", "--env", "GOSUMDB=off", "--env", "GONOSUMDB=*", "--env", "GOFLAGS=-mod=readonly", v.Sandbox.Image, "sh", "-ec", "test -z \"$(gofmt -l .)\"; go vet ./...; go test ./...")
	sandbox := v.Sandbox
	sandbox.OperationID = "assurance-" + request.RunID + "-" + request.Commit
	if sandbox.StateDir == "" {
		sandbox.StateDir = filepath.Join(v.ArtifactStore.Root, "docker-operations")
	}
	out, runErr := sandbox.run(ctx, args)
	artifacts, artifactErr := v.ArtifactStore.StoreTranscript("assurance-"+request.RunID, out.Stdout, out.Stderr)
	if artifactErr != nil {
		return AssuranceResult{}, artifactErr
	}
	result := AssuranceResult{ProviderID: "baseline-go", VerifierDefinition: v.Definition(), Passed: runErr == nil && ctx.Err() == nil, Artifacts: artifacts, Evidence: &EvidenceBinding{Commit: request.Commit, Tree: request.Tree, Contract: request.Contract, Policy: request.Policy, Producer: Ref{ID: "baseline-go", Revision: v.Definition()}, Environment: Ref{ID: "docker-network-none", Revision: v.Sandbox.Image}}}
	if runErr != nil || ctx.Err() != nil {
		result.FailureClass = FailureVerification
		if ctx.Err() != nil {
			result.FailureClass = FailureUnknown
		}
	}
	if tree, err := gitOutput(request.CheckoutDir, "rev-parse", "HEAD^{tree}"); err != nil || strings.TrimSpace(tree) != request.Tree {
		return AssuranceResult{}, fmt.Errorf("verifier input changed during assurance")
	}
	return result, runErr
}
func (v BaselineGoVerifier) prepare(ctx context.Context, checkout string) error {
	if v.DependencyCacheDir == "" {
		return &DependencyUnavailableError{Detail: "trusted pre-warmed module cache is required"}
	}
	if _, err := os.Stat(filepath.Join(checkout, "go.mod")); err != nil {
		return &DependencyUnavailableError{Detail: "go.mod unavailable"}
	}
	if _, err := os.Stat(filepath.Join(checkout, "go.sum")); err != nil && !os.IsNotExist(err) {
		return &DependencyUnavailableError{Detail: "go.sum unavailable"}
	}
	args := dockerBase(checkout, true)
	args = append(args, "--mount", "type=bind,src="+v.DependencyCacheDir+",dst=/cache", "--workdir", "/candidate", "--env", "GOMODCACHE=/cache", "--env", "GOPROXY=off", "--env", "GOSUMDB=off", "--env", "GOFLAGS=-mod=readonly", v.Sandbox.Image, "go", "mod", "download")
	sandbox := v.Sandbox
	sandbox.OperationID = "dependency-preparation-" + filepath.Base(checkout)
	if sandbox.StateDir == "" {
		sandbox.StateDir = filepath.Join(v.ArtifactStore.Root, "docker-operations")
	}
	_, err := sandbox.run(ctx, args)
	if err != nil {
		return &DependencyUnavailableError{Detail: "trusted offline cache cannot satisfy exact module graph"}
	}
	return nil
}

// CreateAssuranceCheckout proves the checked out input before the verifier is
// invoked. The verifier only receives this detached disposable clone, never a
// producer's writable workspace.
func CreateAssuranceCheckout(source, destination, commit, tree string) error {
	if source == "" || destination == "" || commit == "" || tree == "" {
		return fmt.Errorf("source, destination, commit, and tree are required")
	}
	if _, err := runGit("", "clone", "--no-checkout", source, destination); err != nil {
		return err
	}
	if _, err := runGit(destination, "checkout", "--detach", commit); err != nil {
		return err
	}
	gotCommit, err := gitOutput(destination, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(gotCommit) != commit {
		return fmt.Errorf("checkout commit mismatch")
	}
	gotTree, err := gitOutput(destination, "rev-parse", "HEAD^{tree}")
	if err != nil || strings.TrimSpace(gotTree) != tree {
		return fmt.Errorf("checkout tree mismatch")
	}
	return nil
}
