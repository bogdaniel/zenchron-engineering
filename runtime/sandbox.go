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
	"errors"
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
	// DependencyPreparation is NOT an alias of Docker readiness. A reachable
	// daemon holding the pinned image proves a container can start; it proves
	// nothing about whether that image resolves the verifier's toolchain under
	// the runtime's own sandbox environment. The fifth dogfood passed doctor
	// with FAIL=0 while every Go command inside that exact image exited 127.
	preparation := "unavailable"
	if assurance == "enforceable" {
		if _, err := s.ProbeToolchain(context.Background()); err == nil {
			preparation = "enforceable"
		}
	}
	return SandboxDoctor{ProviderSandbox: provider, VerifierSandbox: assurance, OfflineVerification: assurance, DependencyPreparation: preparation}
}

// ProbeToolchain answers one question about the CONFIGURED image: does the
// program the verifier depends on resolve, under exactly the environment the
// runtime builds? It is the same dockerBase boundary as a real verification -
// no network, read-only root, all capabilities dropped, the runtime's own PATH -
// so a pass means the real thing will resolve too, and it is READ-ONLY: an
// empty temporary directory is the only mount, no cache is attached, nothing is
// downloaded, and no candidate is touched.
func (s DockerSandbox) ProbeToolchain(ctx context.Context) (CommandOutput, error) {
	probe, err := os.MkdirTemp("", "zenchron-toolchain-probe-")
	if err != nil {
		return CommandOutput{}, err
	}
	defer os.RemoveAll(probe)
	// dockerBase masks /candidate/.git with a tmpfs, and Docker cannot create
	// that mountpoint inside a read-only bind. A real checkout always has one;
	// the probe makes one so it exercises the SAME boundary rather than a
	// weaker variant of it.
	if err := os.Mkdir(filepath.Join(probe, ".git"), 0700); err != nil {
		return CommandOutput{}, err
	}
	if s.OperationID == "" {
		s.OperationID = "toolchain-probe"
	}
	if s.StateDir == "" {
		state, err := os.MkdirTemp("", "zenchron-toolchain-probe-state-")
		if err != nil {
			return CommandOutput{}, err
		}
		defer os.RemoveAll(state)
		s.StateDir = state
	}
	args := append(dockerBase(probe, true), "--workdir", "/candidate")
	args = append(args, envArgs("GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly")...)
	args = append(args, s.Image, "sh", "-ec", "command -v go; command -v gofmt; go version")
	return s.run(ctx, args)
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

// sandboxPATH is the executable search path INSIDE the sandbox container. It is
// ONE runtime-owned constant, used by every caller of dockerBase, so the
// assurance verifier and a brokered candidate.run resolve programs identically
// when they run the same configured image. It is stated here rather than
// inherited: the host's PATH is never forwarded, and a candidate cannot add to
// it because the container root is read-only and only /candidate is mounted.
//
// /usr/local/go/bin is the toolchain location of the pinned Go image this M0
// sandbox is configured with. Omitting it is defect L1: the fifth dogfood's
// verifier and every brokered Go command met "go: not found" and exited 127
// inside an image that contained a working toolchain the whole time.
//
// Naming a path is not a promise that the program is there. Whether the
// CONFIGURED image actually resolves a toolchain under exactly this environment
// is a question doctor answers by running it, not one this constant asserts.
const sandboxPATH = "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// sandboxGoEnv is the deterministic Go environment shared by every runtime-owned
// Go invocation in the sandbox. GOTOOLCHAIN=local refuses a toolchain switch: a
// candidate that raised the go directive in go.mod would otherwise make Go try
// to fetch a toolchain, which with networking off is a confusing failure and
// with networking on would be a candidate choosing the program that judges it.
var sandboxGoEnv = []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=readonly " + sandboxBuildVCS}

// sandboxBuildVCS disables Go's VCS stamping. dockerBase masks /candidate/.git
// with a tmpfs - the candidate's Git metadata is runtime-owned and is
// deliberately not shown to anything running in the sandbox - so Go finds a
// directory that is not a repository and refuses with "error obtaining VCS
// status". Stamping a revision into a verification build proves nothing anyway:
// the exact commit and tree are already the assurance binding.
const sandboxBuildVCS = "-buildvcs=false"

// sandboxBuildDir is a small tmpfs the Go toolchain may EXECUTE from, and the
// only path in the container that is executable and writable at once.
//
// Docker mounts every --tmpfs noexec unless told otherwise, so /tmp, /home and
// the masked /candidate/.git are all noexec and stay that way. `go test`,
// however, compiles a test binary and then runs it; with nowhere executable it
// fails every package with "fork/exec ...: permission denied", which makes
// `go test ./...` - the whole point of the baseline verifier - impossible.
//
// What contains a test binary is the rest of the boundary, not this: the root
// filesystem is read-only, networking is off, every capability is dropped,
// no-new-privileges is set, and the candidate workspace is the only host path
// mounted. Running the candidate's own tests is the verifier's declared job.
const sandboxBuildDir = "/gobuild"

func envArgs(entries ...string) []string {
	args := make([]string, 0, len(entries)*2)
	for _, entry := range entries {
		args = append(args, "--env", entry)
	}
	return args
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
	return []string{"run", "--init", "--sig-proxy=true", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,mode=1777", "--tmpfs", "/home:rw,nosuid,nodev,noexec,mode=0700", "--tmpfs", "/candidate/.git:rw,nosuid,nodev,noexec,mode=0700", "--tmpfs", sandboxBuildDir + ":rw,nosuid,nodev,exec,mode=0700", "--env", "HOME=/home", "--env", "PATH=" + sandboxPATH, "--env", "GOTMPDIR=" + sandboxBuildDir, "--env", "GOCACHE=" + sandboxBuildDir + "/cache", "--mount", mount}
}

// PrerequisiteKind names WHAT was missing when an assurance prerequisite could
// not be met. It is a small closed set, not free text: it is what a durable
// diagnostic and an operator both need, and the fifth dogfood proved a single
// opaque string ("trusted offline cache cannot satisfy exact module graph")
// cannot tell "the cache is empty" from "go is not on the PATH".
type PrerequisiteKind string

const (
	// PrerequisiteToolchain is the configured image not resolving the program
	// the verifier needs under the runtime's own sandbox environment.
	PrerequisiteToolchain PrerequisiteKind = "toolchain_unavailable"
	// PrerequisiteCache is the operator-provisioned dependency cache being
	// absent, unreadable, or empty.
	PrerequisiteCache PrerequisiteKind = "dependency_cache_unavailable"
	// PrerequisiteModule is a module the exact tree needs that the trusted
	// offline cache does not contain.
	PrerequisiteModule PrerequisiteKind = "module_unavailable_offline"
	// PrerequisiteSandbox is the Docker daemon or image being unavailable. It
	// is the one kind that is genuinely transient.
	PrerequisiteSandbox PrerequisiteKind = "sandbox_unavailable"
	// PrerequisiteSource is the exact tree not carrying what preparation needs.
	PrerequisiteSource PrerequisiteKind = "source_incomplete"
)

// DependencyUnavailableError is an assurance PREREQUISITE failure: the verifier
// never judged the candidate, because the environment it needs was not there.
// It is deliberately not a verification failure - no verdict about the tree was
// reached - and, except for the sandbox kind, deliberately not transient:
// re-running the identical command against the identical environment produces
// the identical failure, which is exactly how the fifth dogfood spent three
// assurance attempts in a few seconds.
type DependencyUnavailableError struct {
	Kind   PrerequisiteKind
	Detail string
}

func (e *DependencyUnavailableError) Error() string {
	if e.Kind == "" {
		return "dependency preparation unavailable: " + e.Detail
	}
	return "assurance prerequisite unavailable (" + string(e.Kind) + "): " + e.Detail
}

// Transient reports whether this prerequisite may clear on its own. Only the
// sandbox kind can: a daemon comes back, an empty cache does not fill itself.
func (e *DependencyUnavailableError) Transient() bool { return e.Kind == PrerequisiteSandbox }

// classifyPreparationOutput reads the container's own output to name the
// prerequisite. It matches on shell and Go diagnostics that are stable enough
// to depend on, and falls back to the cache kind - the conservative answer for
// an offline preparation that failed for a reason we cannot name.
func classifyPreparationOutput(out CommandOutput) (PrerequisiteKind, string) {
	combined := strings.ToLower(string(out.Stdout) + "\n" + string(out.Stderr))
	switch {
	case out.ExitCode == 127, strings.Contains(combined, "not found"), strings.Contains(combined, "no such file or directory"):
		return PrerequisiteToolchain, "the configured image did not resolve the Go toolchain on the runtime sandbox path"
	case strings.Contains(combined, "missing go.sum entry"), strings.Contains(combined, "module lookup disabled"),
		strings.Contains(combined, "cannot find module"), strings.Contains(combined, "no required module provides"),
		strings.Contains(combined, "proxy.golang.org"), strings.Contains(combined, "goproxy=off"):
		return PrerequisiteModule, "the trusted offline cache does not contain a module the exact tree requires"
	case strings.Contains(combined, "permission denied"), strings.Contains(combined, "read-only file system"):
		return PrerequisiteCache, "the dependency cache is not usable by the verifier in this configuration"
	}
	return PrerequisiteCache, "offline dependency preparation failed against the trusted cache"
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
		// A prerequisite failure is not a verdict about the candidate and,
		// unless the sandbox itself was unavailable, not transient either.
		// Classifying it as transient infrastructure is what let the same
		// deterministic environment fault burn every assurance attempt.
		class := FailureAssurancePrerequisite
		var unavailable *DependencyUnavailableError
		if errors.As(err, &unavailable) && unavailable.Transient() {
			class = FailureTransientInfrastructure
		}
		return AssuranceResult{ProviderID: "baseline-go", VerifierDefinition: v.Definition(), FailureClass: class}, err
	}
	args := dockerBase(request.CheckoutDir, true)
	args = append(args, goModuleCacheMount(v.DependencyCacheDir), "--workdir", "/candidate")
	args = append(args, envArgs(append([]string{"GOMODCACHE=/cache", "GONOSUMDB=*"}, sandboxGoEnv...)...)...)
	args = append(args, v.Sandbox.Image, "sh", "-ec", "test -z \"$(gofmt -l .)\"; go vet ./...; go test ./...")
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
		return &DependencyUnavailableError{Kind: PrerequisiteCache, Detail: "no trusted pre-warmed module cache is configured"}
	}
	if info, err := os.Stat(v.DependencyCacheDir); err != nil || !info.IsDir() {
		return &DependencyUnavailableError{Kind: PrerequisiteCache, Detail: "the configured dependency cache is not a readable directory"}
	}
	if empty, err := directoryIsEmpty(v.DependencyCacheDir); err != nil || empty {
		return &DependencyUnavailableError{Kind: PrerequisiteCache, Detail: "the configured dependency cache is empty; a pre-warmed cache is operator-provisioned and this verification never downloads"}
	}
	if _, err := os.Stat(filepath.Join(checkout, "go.mod")); err != nil {
		return &DependencyUnavailableError{Kind: PrerequisiteSource, Detail: "the exact tree has no go.mod"}
	}
	if _, err := os.Stat(filepath.Join(checkout, "go.sum")); err != nil && !os.IsNotExist(err) {
		return &DependencyUnavailableError{Kind: PrerequisiteSource, Detail: "the exact tree's go.sum is unreadable"}
	}
	// The operator cache is mounted READ-ONLY here, exactly as it is during
	// verification. See goModuleCacheMount: preparation used to mount it
	// writable and run `go mod download` into it, which let the module graph of
	// one candidate write into dependency state every other run reads.
	args := dockerBase(checkout, true)
	args = append(args, goModuleCacheMount(v.DependencyCacheDir), "--workdir", "/candidate")
	args = append(args, envArgs(append([]string{"GOMODCACHE=/cache", "GOFLAGS=-mod=mod " + sandboxBuildVCS}, sandboxGoEnv[:3]...)...)...)
	// `go list -deps` resolves the exact module graph from the cache and writes
	// nothing into it. It answers the one question preparation exists to ask -
	// can this tree be built offline from the trusted material - without the
	// verification itself being what discovers the answer.
	args = append(args, v.Sandbox.Image, "go", "list", "-deps", "./...")
	sandbox := v.Sandbox
	sandbox.OperationID = "dependency-preparation-" + filepath.Base(checkout)
	if sandbox.StateDir == "" {
		sandbox.StateDir = filepath.Join(v.ArtifactStore.Root, "docker-operations")
	}
	out, err := sandbox.run(ctx, args)
	if err == nil {
		return nil
	}
	// A daemon or image that is not there is the one genuinely transient case:
	// no container ran, so there is no output to classify.
	if errors.Is(err, ErrSandboxUnavailable) {
		return &DependencyUnavailableError{Kind: PrerequisiteSandbox, Detail: "the assurance sandbox is unavailable"}
	}
	kind, detail := classifyPreparationOutput(out)
	return &DependencyUnavailableError{Kind: kind, Detail: detail}
}

// goModuleCacheMount is the ONE place the operator dependency cache is attached
// to a container, and it is always read-only.
//
// Before this repair, preparation mounted it writable and ran `go mod download`
// against the CANDIDATE's go.mod. That is shared mutable dependency state: the
// module graph a candidate declares decided what got written into a cache every
// other run of every other candidate then reads. Nothing malicious is needed for
// it to matter - it is simply not a trusted-material boundary if the thing being
// verified can extend it. Read-only makes the operator the only writer, which
// is what "operator-provisioned pre-warmed cache" always meant.
func goModuleCacheMount(dir string) string {
	return "--mount=type=bind,src=" + dir + ",dst=/cache,readonly"
}

// directoryIsEmpty reports whether a directory holds no entries. A configured
// but empty cache is the fifth dogfood's exact condition and is a prerequisite
// failure, never a candidate verdict.
func directoryIsEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
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
