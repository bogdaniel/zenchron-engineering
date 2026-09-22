package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func successorServeArgs(args []string) (plain []string, source string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--follow-main" {
			plain = append(plain, args[i])
			continue
		}
		if source != "" || i+1 == len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
			return nil, "", fmt.Errorf("serve accepts --follow-main <controller-source-clone> once")
		}
		i++
		source, err = filepath.Abs(args[i])
		if err != nil {
			return nil, "", err
		}
	}
	return
}

// A persistent adopted guardian retains the old executable and stays alive
// throughout handoff. It never drives runs itself. Only its one serve child
// owns the control endpoint and scheduler. This also preserves service-manager
// PID and signal semantics across controller generations.
func serveFollowingMain(args []string, source string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	c, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	config, active, forge := c.config, c.build, c.forge
	c.release()
	if active.Kind != runtime.ControllerAdopted {
		return runtime.ExitInvalid, fmt.Errorf("--follow-main requires an adopted guardian")
	}
	// The controller source repository is fixed. --repo continues to select
	// work, never the executable that will receive the operator's authority.
	target, err := repositoryTarget(source, "")
	if err != nil {
		return runtime.ExitInvalid, err
	}
	if target.Identity != "bogdaniel/zenchron-engineering" {
		return runtime.ExitInvalid, fmt.Errorf("successor source must be the controller repository")
	}
	governance := overrides.Governance
	if governance == nil {
		governance, err = governanceObserver(config.GitHub)
		if err != nil {
			return runtime.ExitInvalid, err
		}
	}
	// A stable lock prevents competing guardians from alternating children.
	lock, err := runtime.AcquireOwnershipLock(config.StateDir, "successor-guardian")
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer lock.Release()
	if running, present := runtime.SupervisorPresence(config.StateDir); running || present {
		return runtime.ExitInvalid, fmt.Errorf("start --follow-main after stopping the existing serve process and reclaiming any stale endpoint")
	}
	binary, err := os.Executable()
	if err != nil {
		return runtime.ExitFailed, err
	}
	builder := &runtime.SuccessorBuilder{
		Builder:    active,
		StatusPath: filepath.Join(config.StateDir, "controllers", "successor-status.json"),
		Request: runtime.AdoptedBuildRequest{
			Repository:    runtime.GitHubRepo{Owner: "bogdaniel", Name: "zenchron-engineering"},
			RepositoryDir: source, OutputRoot: filepath.Join(config.StateDir, "controllers"),
			Sandbox:            runtime.DockerSandbox{Image: config.Assurance.Image, Endpoint: runtime.DockerEndpoint{Host: config.Assurance.DockerHost}, StateDir: filepath.Join(config.StateDir, "artifacts", "docker-operations")},
			DependencyCacheDir: config.Assurance.DependencyCacheDir,
		},
		Deps: runtime.AdoptedBuildDeps{Governance: governance, RefSHA: forge.RefSHA},
	}
	last, err := builder.LastActive()
	if err != nil {
		return runtime.ExitFailed, err
	}
	if last != nil {
		binary, active = last.OutputPath, runtime.SuccessorIdentity(*last)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	child, err := startServeChild(binary, args, stdout)
	if err != nil {
		return runtime.ExitFailed, err
	}
	defer func() {
		if child != nil {
			_ = child.stop()
		}
	}()
	if err := child.ready(ctx, config.StateDir, active); err != nil {
		return runtime.ExitFailed, err
	}
	if err := child.activate(config.StateDir, active); err != nil {
		return runtime.ExitFailed, err
	}
	// Polling is deliberately bounded, including failures. No failed build
	// or governance read tears down the serving child.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	activationPending := false
	for {
		select {
		case <-ctx.Done():
			return runtime.ExitCompleted, nil
		case <-child.done:
			return runtime.ExitCompleted, child.err
		case <-ticker.C:
		}
		if activationPending {
			if err := child.activate(config.StateDir, active); err != nil {
				continue
			}
			activationPending = false
			_ = builder.Record(runtime.SuccessorStatus{Active: active, Successor: last, State: "active"})
		}
		buildCtx, stopBuild := context.WithTimeout(ctx, 20*time.Minute)
		p, prepareErr := builder.Prepare(buildCtx, active)
		stopBuild()
		if prepareErr != nil {
			fmt.Fprintln(stdout, "successor preparation refused; current controller retained (see controllers/successor-status.json)")
			continue
		}
		if p == nil {
			continue
		}
		status := runtime.SuccessorStatus{Active: active, Successor: p, ObservedRevision: p.Source.Revision}
		cwd, cwdErr := os.Getwd()
		freshConfig, configErr := runtime.LoadConfig(flags.Config, cwd)
		if cwdErr != nil || configErr != nil || freshConfig.Digest != config.Digest {
			status.State = "waiting_configuration_drift"
			_ = builder.Record(status)
			continue
		}
		response, checkErr := runtime.SendControl(config.StateDir, runtime.ControlRequest{Command: "successor-compatibility"})
		if checkErr != nil || !response.OK || json.Unmarshal(response.Payload, &status.Compatibility) != nil {
			status.State = "unsupported_schema"
			if err := builder.Record(status); err != nil {
				fmt.Fprintln(stdout, "successor status could not be persisted; current controller retained")
			}
			continue
		}
		if len(status.Compatibility) != 0 {
			status.State = "waiting_compatibility"
			if err := builder.Record(status); err != nil {
				fmt.Fprintln(stdout, "successor status could not be persisted; current controller retained")
			}
			continue
		}
		if err := runtime.CheckSuccessorStateFormat(ctx, p.OutputPath); err != nil {
			status.State = "unsupported_schema"
			_ = builder.Record(status)
			continue
		}
		status.State = "activating"
		if err := builder.Record(status); err != nil {
			fmt.Fprintln(stdout, "successor status could not be persisted; current controller retained")
			continue
		}
		// Recheck the artifact immediately before relinquishing the old serve.
		if err := runtime.VerifySuccessorArtifact(*p); err != nil {
			continue
		}
		if err := child.stop(); err != nil {
			return runtime.ExitFailed, err
		}
		child = nil
		// A submission could have raced the first compatibility read. With the
		// old child fully joined, enumerate again before starting new code.
		check, checkErr := newComposition(flags, overrides)
		if checkErr == nil {
			status.Compatibility, checkErr = runtime.SuccessorCompatibilityReport(check.store)
			if check.config.Digest != config.Digest {
				checkErr = fmt.Errorf("configuration changed during handoff")
			}
			check.release()
		}
		next := runtime.SuccessorIdentity(*p)
		if checkErr == nil && len(status.Compatibility) == 0 {
			// Re-observe after the old process has unwound, not just before
			// waiting for it. A changed gate rolls back to the known binary.
			verifyCtx, cancelVerify := context.WithTimeout(ctx, time.Minute)
			verified, verifyErr := builder.Prepare(verifyCtx, active)
			cancelVerify()
			if verifyErr != nil || verified == nil || runtime.SuccessorIdentity(*verified) != next {
				checkErr = fmt.Errorf("successor trust changed during handoff")
			}
		}
		if checkErr == nil && len(status.Compatibility) == 0 && ctx.Err() == nil {
			child, err = startServeChild(p.OutputPath, append(append([]string{}, args...), "--successor-require-idle"), stdout)
			if err == nil {
				err = child.ready(ctx, config.StateDir, next)
			}
		} else {
			err = fmt.Errorf("durable state changed during handoff")
		}
		if err == nil {
			// Failure to persist the restart identity is an activation failure;
			// keep the preceding generation recoverable rather than claiming
			// an update a guardian restart would silently undo.
			err = builder.RememberActive(*p)
		}
		if err == nil {
			binary, active = p.OutputPath, next
			last = p
			status.Active, status.State = active, "active"
			// Once activation is sent its effect can outlive a lost reply.
			// Never roll back to A after B may have created work. Retry the
			// idempotent activation on B while keeping its durable identity.
			if activateErr := child.activate(config.StateDir, next); activateErr != nil {
				activationPending, status.State = true, "activation_unconfirmed"
			}
		} else {
			if child != nil {
				if stopErr := child.stop(); stopErr != nil {
					return runtime.ExitFailed, stopErr
				}
				child = nil
			}
			if ctx.Err() != nil {
				return runtime.ExitCompleted, nil
			}
			child, err = startServeChild(binary, args, stdout)
			if err == nil {
				err = child.ready(ctx, config.StateDir, active)
			}
			if err == nil {
				if last != nil {
					err = builder.RememberActive(*last)
				} else {
					err = builder.ForgetActive()
				}
			}
			if err == nil {
				err = child.activate(config.StateDir, active)
			}
			status.State = "activation_failed_rolled_back"
			if err != nil {
				status.State = "rollback_failed"
				_ = builder.Record(status)
				return runtime.ExitFailed, err
			}
		}
		if err := builder.Record(status); err != nil {
			fmt.Fprintln(stdout, "successor status could not be persisted; active controller retained")
		}
	}
}

type serveChild struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // read only after done closes
}

func startServeChild(binary string, args []string, stdout io.Writer) (*serveChild, error) {
	cmd := exec.Command(binary, append([]string{"serve", "--successor-standby"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := &serveChild{cmd: cmd, done: make(chan struct{})}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	return child, nil
}

func (c *serveChild) activate(stateDir string, expected runtime.ControllerBuild) error {
	response, err := runtime.SendControl(stateDir, runtime.ControlRequest{Command: "successor-activate"})
	if err != nil {
		return err
	}
	var active runtime.ControllerBuild
	if !response.OK || json.Unmarshal(response.Payload, &active) != nil || active != expected {
		return fmt.Errorf("serve activation was not acknowledged by the expected controller")
	}
	return nil
}

func (c *serveChild) ready(ctx context.Context, stateDir string, expected runtime.ControllerBuild) error {
	return c.readyWithProbe(ctx, expected, func() (runtime.ControlResponse, error) {
		return runtime.SendControl(stateDir, runtime.ControlRequest{Command: runtime.ControlPing})
	})
}

func (c *serveChild) readyWithProbe(ctx context.Context, expected runtime.ControllerBuild, probe func() (runtime.ControlResponse, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("successor did not prove active identity: %w", ctx.Err())
		case <-c.done:
			return fmt.Errorf("serve exited before proving active identity")
		case <-ticker.C:
			response, err := probe()
			if err != nil || !response.OK {
				continue
			}
			var identity struct {
				Build runtime.ControllerBuild `json:"build"`
				PID   int                     `json:"pid"`
			}
			if json.Unmarshal(response.Payload, &identity) != nil {
				continue
			}
			if identity.PID != c.cmd.Process.Pid || identity.Build != expected {
				return fmt.Errorf("active serve identity disagrees with the adopted artifact")
			}
			return nil
		}
	}
}

func (c *serveChild) stop() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		select {
		case <-c.done:
			return nil
		default:
			return err
		}
	}
	// Do not kill a controller still unwinding its workers or launch beside
	// it. A stalled shutdown needs operator intervention, not dual ownership.
	select {
	case <-c.done:
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("old serve has not stopped; successor activation refused")
	}
}
