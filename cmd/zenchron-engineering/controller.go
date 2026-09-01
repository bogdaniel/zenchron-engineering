package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// controllerBuildAdopted is the only supported way to produce a controller that
// claims kind=adopted.
//
// It takes no --kind flag, and that absence is the design. Adoption is a fact
// about where source lives - contained in a branch some external authority
// governs - and a flag is just a caller asserting it. The command therefore
// proves the fact itself or refuses, and an operator who wants a binary without
// the proof already has one: `go build` produces an unattested controller,
// which is legal, honest, and says so when asked.
func controllerBuildAdopted(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitFailed, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	owner, name, ok := strings.Cut(target.Identity, "/")
	if !ok {
		return runtime.ExitInvalid, fmt.Errorf("repository %q is not owner/name", target.Identity)
	}

	// The controlled build environment is not optional, so a configuration
	// that will not load is a refusal rather than a quiet downgrade to
	// whatever `go` happens to be on the PATH.
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return runtime.ExitInvalid, fmt.Errorf("an adopted build needs the operator configuration for its pinned build environment: %w", err)
	}
	forge := overrides.GitHub
	if forge == nil {
		forge = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    config.GitHub.Endpoint,
			Credentials: githubCredentials(config.GitHub.CredentialMode),
		}
	}
	rulesetReader, ok := forge.(interface {
		Rulesets(context.Context, runtime.GitHubRepo) ([]runtime.TrustedMainRuleset, error)
	})
	if !ok {
		return runtime.ExitFailed, fmt.Errorf("the configured forge adapter cannot observe repository rulesets, so no trust root can be established")
	}
	deps := runtime.AdoptedBuildDeps{Rulesets: rulesetReader.Rulesets, RefSHA: forge.RefSHA}

	output := strings.TrimSpace(flags.Output)
	if output == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return runtime.ExitFailed, homeErr
		}
		output = filepath.Join(home, ".zenchron-adopted-controller")
	}

	// The builder's own identity is recorded truthfully. It need not be
	// adopted - it usually is not - but a failed measurement is recorded as a
	// failed measurement, never laundered into "unattested", which would claim
	// a deliberate absence of provenance where there is a broken one.
	builder := runtime.BuilderRecord{Kind: runtime.ControllerUnattested}
	if self, selfErr := controllerBuild(); selfErr != nil {
		builder.ResolutionError = selfErr.Error()
	} else {
		builder = runtime.BuilderRecord{Kind: self.Kind, Version: self.Version, SourceRevision: self.SourceRevision}
		if builder.Kind == "" {
			builder.Kind = runtime.ControllerUnattested
		}
	}

	request := runtime.AdoptedBuildRequest{
		Repository:         runtime.GitHubRepo{Owner: owner, Name: name},
		Revision:           strings.TrimSpace(flags.Revision),
		RepositoryDir:      cwd,
		OutputRoot:         output,
		Sandbox:            runtime.DockerSandbox{Image: config.Assurance.Image, Endpoint: runtime.DockerEndpoint{Host: config.Assurance.DockerHost}, StateDir: filepath.Join(config.StateDir, "artifacts", "docker-operations")},
		DependencyCacheDir: config.Assurance.DependencyCacheDir,
		Policy:             runtime.DefaultTrustPolicy(),
	}

	provenance, err := runtime.BuildAdoptedController(context.Background(), request, deps, builder)
	if err != nil {
		return runtime.ExitFailed, err
	}

	fmt.Fprintf(stdout, "kind:              %s\n", provenance.Kind)
	fmt.Fprintf(stdout, "version:           %s\n", provenance.Version)
	fmt.Fprintf(stdout, "source:            %s\n", provenance.Source.Revision)
	fmt.Fprintf(stdout, "tree:              %s\n", provenance.Source.Tree)
	fmt.Fprintf(stdout, "trusted main:      %s (tree %s)\n", provenance.TrustedMain.Revision, provenance.TrustedMain.Tree)
	fmt.Fprintf(stdout, "trust root:        ruleset %d %q %s\n", provenance.TrustRoot.RulesetID, provenance.TrustRoot.Name, provenance.TrustRoot.Digest)
	fmt.Fprintf(stdout, "build environment: %s %s (%s), network %s, source %s, cache %s\n",
		provenance.BuildEnv.Kind, provenance.BuildEnv.Image, provenance.BuildEnv.Toolchain,
		provenance.BuildEnv.Network, provenance.BuildEnv.SourceMount, provenance.BuildEnv.CacheMount)
	fmt.Fprintf(stdout, "build env digest:  %s\n", provenance.BuildEnv.Digest)
	fmt.Fprintf(stdout, "binary sha256:     %s\n", provenance.BinarySHA256)
	fmt.Fprintf(stdout, "binary:            %s\n", provenance.OutputPath)
	fmt.Fprintf(stdout, "self probe:        %s %s matched=%t\n", provenance.SelfProbe.Kind, provenance.SelfProbe.Version, provenance.SelfProbe.Matched)
	fmt.Fprintf(stdout, "builder:           kind=%s version=%s\n", provenance.Builder.Kind, provenance.Builder.Version)
	return runtime.ExitCompleted, nil
}

// controllerInspectSelf reports this binary's own build provenance and nothing
// else.
//
// It exists so the adopted builder can always verify what it just produced.
// The full doctor needs an operator configuration, a Docker daemon and two
// credentials - none of which bear on whether a binary's embedded metadata
// matches the executable it is embedded in. Requiring them turned a missing
// config into a skipped probe, and a skipped probe into an adopted artifact
// nobody had checked.
func controllerInspectSelf(args []string, stdout io.Writer) (int, error) {
	build, err := controllerBuild()
	if err != nil {
		return runtime.ExitFailed, fmt.Errorf("this binary cannot establish its own provenance: %w", err)
	}
	encoded, err := json.MarshalIndent(build, "", "  ")
	if err != nil {
		return runtime.ExitFailed, err
	}
	fmt.Fprintln(stdout, string(encoded))
	return runtime.ExitCompleted, nil
}
