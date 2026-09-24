package main

// `controller re-adopt`: the operator's cold authority boundary.
//
// It exists for one situation, and the message an operator gets when they are
// in it names this command. The controller-effective configuration is part of
// what a controller is authorized to continue under, so changing it means the
// running binding and every later one disagree - ordinary succession correctly
// refuses to cross that, and without this there was no way across at all.
//
// The composition here is deliberately thin: take the role, measure this
// executable, read its published provenance, observe trusted main, resolve the
// operator, and hand all of it to the runtime, which decides. Everything that
// can refuse is a refusal in runtime/controller_readopt.go, where it can be
// tested without a machine.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const readoptUsage = `usage: zenchron-engineering controller re-adopt --reason <text> [--repo owner/name] [--config <path>]

Sanction this adopted generation as the controller that governs now, after a
deliberate controller-effective configuration change. It is COLD: stop serve
and every other controller process first.`

func controllerReadopt(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, reason, err := parseReadoptFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	// COLD, AND PROVEN SO. Taking the role is how this process establishes
	// that no controller is running; a flag or a socket probe would be a
	// guess about somebody else's process.
	lease, err := runtime.AcquireControllerRole(built.config.StateDir)
	if err != nil {
		return runtime.ExitInvalid, fmt.Errorf(
			"re-adoption is a cold operation and the controller role is held; stop serve and any other controller process first: %w", err)
	}
	defer func() { _ = lease.Release() }()

	self, err := controllerSelf()
	if err != nil {
		return runtime.ExitFailed, fmt.Errorf("this controller cannot establish its own identity: %w", err)
	}
	binding, err := built.controllerBinding()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	operator, err := built.config.ResolveOperator()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	provenance, found, err := runtime.PublishedAdoptedController(controllerRoot(),
		runtime.RevisionRecord{Revision: self.Build.SourceRevision, Tree: self.Build.SourceTree})
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !found {
		return runtime.ExitInvalid, fmt.Errorf(
			"generation %s is not published under %s, so its adopted provenance cannot be read",
			self.Build.Version, controllerRoot())
	}
	repository, err := runtime.ParseGitHubRepo(provenance.Repository)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	trustedMain, err := built.observeTrustedMain(context.Background(), repository)
	if err != nil {
		return runtime.ExitFailed, err
	}

	readoption, err := runtime.ReadoptController(built.store, lease, runtime.ReadoptionRequest{
		Reason: reason, Operator: operator, Self: self, Provenance: provenance,
		Binding: binding, TrustedMain: trustedMain, Now: time.Now().UTC(),
	})
	if err != nil {
		return runtime.ExitInvalid, err
	}

	// THE PROJECTION IS REPAIRED AFTER, AND IS NOT THE AUTHORITY. Authority is
	// committed above; the stable entrypoint is a pointer that follows it, and
	// a failure here leaves an operator aimed at the previous artifact with the
	// control model entirely correct. Re-running the command converges.
	drift := ""
	if _, err := runtime.ActivateReadoptedGeneration(built.store, self, controllerRoot()); err != nil {
		drift = err.Error()
	}

	fmt.Fprintf(stdout, "re-adopted:        %s\n", readoption.ID)
	fmt.Fprintf(stdout, "generation:        %s (%s)\n", binding.Build.Version, shortVersion(binding.Build.SourceRevision))
	fmt.Fprintf(stdout, "controller:        %s\n", binding.Controller)
	fmt.Fprintf(stdout, "configuration:     %s\n", shortVersion(binding.Config.Global))
	if readoption.Previous != nil {
		fmt.Fprintf(stdout, "superseded:        %s %s (configuration %s)\n",
			readoption.Previous.Kind, readoption.Previous.Ref, shortVersion(readoption.PreviousConfig.Global))
	}
	fmt.Fprintf(stdout, "operator:          %s (%s)\n", readoption.Operator.ID, readoption.Operator.Provenance)
	fmt.Fprintf(stdout, "reason:            %s\n", readoption.Reason)
	if drift != "" {
		fmt.Fprintf(stdout, "stable entrypoint: NOT REPAIRED: %s\n", drift)
		fmt.Fprintf(stdout, "\nAuthority is committed. Re-run this command to repair the stable entrypoint.\n")
		return runtime.ExitCompleted, nil
	}
	fmt.Fprintf(stdout, "stable entrypoint: %s\n", controllerRoot()+"/"+runtime.StableEntrypointName)
	fmt.Fprintf(stdout, "\nStart serve to resume. Ordinary automatic succession continues from this generation.\n")
	return runtime.ExitCompleted, nil
}

// parseReadoptFlags accepts the ordinary configuration flags plus the required
// reason. The reason is a flag rather than a positional argument because an
// operator who forgets it should get a refusal naming it, not an argument
// silently read as something else.
func parseReadoptFlags(args []string) (autonomyFlags, string, error) {
	var reason string
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--reason" {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return autonomyFlags{}, "", fmt.Errorf("--reason requires text\n\n%s", readoptUsage)
			}
			reason, i = strings.TrimSpace(args[i+1]), i+1
			continue
		}
		rest = append(rest, args[i])
	}
	if reason == "" {
		return autonomyFlags{}, "", fmt.Errorf("--reason is required: a controller authority event records why it was performed\n\n%s", readoptUsage)
	}
	flags, err := parseAutonomyFlags(rest)
	if err != nil {
		return autonomyFlags{}, "", fmt.Errorf("%w\n\n%s", err, readoptUsage)
	}
	return flags, reason, nil
}

// observeTrustedMain is the same observation the updater makes, through the
// same governance-credentialled path.
func (c *composition) observeTrustedMain(ctx context.Context, repository runtime.GitHubRepo) (runtime.RevisionRecord, error) {
	governance, err := governanceObserver(c.config.GitHub)
	if err != nil {
		return runtime.RevisionRecord{}, fmt.Errorf(
			"the governance credential that observes the trust root is not configured, and a governing root is the code trusted main names: %w", err)
	}
	forge := c.forge
	if forge == nil {
		forge = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    c.config.GitHub.Endpoint,
			Credentials: githubCredentials(c.config.GitHub),
		}
	}
	remote, err := runtime.GovernedRemote(repository.CloneURL())
	if err != nil {
		return runtime.RevisionRecord{}, err
	}
	source, err := runtime.EnsureControllerSource(c.config.StateDir, remote, c.credentials)
	if err != nil {
		return runtime.RevisionRecord{}, err
	}
	return runtime.ObserveTrustedMainRevision(ctx,
		runtime.AdoptedBuildDeps{Governance: governance, RefSHA: forge.RefSHA}, repository, source)
}

var _ = os.Getenv
