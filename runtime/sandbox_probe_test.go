//go:build unix

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #447: Docker readiness/identity probes against a daemon that never answers.
// The fake docker on PATH blocks forever, as the live one did for 4h18m.

func hungDocker(t *testing.T, ceiling time.Duration) DockerSandbox {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec /bin/sleep 1000\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := dockerProbeCeiling
	dockerProbeCeiling = ceiling
	t.Cleanup(func() { dockerProbeCeiling = old })
	return DockerSandbox{Image: "sha256:" + strings.Repeat("0", 64), Grace: 100 * time.Millisecond}
}

// probeWithin10s fails the test, rather than hanging it, when probe outlives 10s.
func probeWithin10s(t *testing.T, probe func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- probe() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the Docker probe did not end within 10s: it is not bounded by the caller and the ceiling")
		return nil
	}
}

func TestDockerProbeCallerCancellationIsNotUnavailability(t *testing.T) {
	s := hungDocker(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	err := probeWithin10s(t, func() error { return s.ready(ctx) })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("caller cancellation must surface as cancellation, got %v", err)
	}
}

func TestDockerProbeCeilingIsUnavailable(t *testing.T) {
	s := hungDocker(t, 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	err := probeWithin10s(t, func() error { return s.ready(ctx) })
	if !errors.Is(err, ErrSandboxUnavailable) || !strings.Contains(err.Error(), "did not respond within 300ms") {
		t.Fatalf("an unresponsive daemon must be unavailable within the ceiling, got %v", err)
	}
}

func TestDockerProbeKeepsAShorterCallerDeadline(t *testing.T) {
	s := hungDocker(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := probeWithin10s(t, func() error { return s.ready(ctx) })
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("the caller's own shorter deadline must end the probe as the caller's, got %v", err)
	}
}

func TestDockerDaemonIdentityIsBounded(t *testing.T) {
	s := hungDocker(t, 300*time.Millisecond)
	if err := probeWithin10s(t, func() error { _, err := s.daemonIdentity(context.Background()); return err }); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("daemon identity against a hung daemon must be unavailable, got %v", err)
	}
	dockerProbeCeiling = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := probeWithin10s(t, func() error { _, err := s.daemonIdentity(ctx); return err }); !errors.Is(err, context.Canceled) || errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("daemon identity must surface caller cancellation, got %v", err)
	}
}

func TestDoctorReportsAnUnresponsiveDaemonWithinTheCeiling(t *testing.T) {
	s := hungDocker(t, 300*time.Millisecond)
	var report DoctorReport
	probeWithin10s(t, func() error { report = Doctor(context.Background(), DoctorInput{Sandbox: s}); return nil })
	check, ok := report.Check("assurance.verifier_sandbox")
	if !ok || check.Status != DoctorFail || !strings.Contains(check.Reason, "Docker daemon did not respond within 300ms") {
		t.Fatalf("doctor must report the bounded unresponsive-daemon diagnostic, got %+v", check)
	}
}

// A cancelled readiness probe reaches assurance preparation as the caller's
// cancellation, not as a classified dependency prerequisite.
func TestPreparationKeepsProbeCancellation(t *testing.T) {
	s := hungDocker(t, time.Hour)
	cache, checkout := t.TempDir(), t.TempDir()
	for _, f := range []string{filepath.Join(cache, "mod"), filepath.Join(checkout, "go.mod")} {
		if err := os.WriteFile(f, []byte("module x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v := BaselineGoVerifier{Sandbox: s, ArtifactStore: ArtifactStore{Root: t.TempDir()}, DependencyCacheDir: cache}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	err := probeWithin10s(t, func() error { return v.prepare(ctx, checkout) })
	var prerequisite *DependencyUnavailableError
	if !errors.Is(err, context.Canceled) || errors.As(err, &prerequisite) {
		t.Fatalf("preparation must surface the caller's cancellation, got %v", err)
	}
}
