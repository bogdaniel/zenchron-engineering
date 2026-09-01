package main

import (
	"context"
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

	config, configErr := runtime.LoadConfig(flags.Config, cwd)
	doctorConfig := ""
	if configErr == nil {
		doctorConfig = config.OperatorPath
	}
	forge := overrides.GitHub
	if forge == nil {
		endpoint := ""
		mode := runtime.GitHubCredentialCLI
		if configErr == nil {
			endpoint, mode = config.GitHub.Endpoint, config.GitHub.CredentialMode
		}
		forge = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    endpoint,
			Credentials: githubCredentials(mode),
		}
	}
	rulesetReader, ok := forge.(interface {
		Rulesets(context.Context, runtime.GitHubRepo) ([]runtime.TrustedMainRuleset, error)
	})
	if !ok {
		return runtime.ExitFailed, fmt.Errorf("the configured forge adapter cannot observe repository rulesets, so no trust root can be established")
	}

	output := strings.TrimSpace(flags.Output)
	if output == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return runtime.ExitFailed, homeErr
		}
		output = filepath.Join(home, ".zenchron-adopted-controller")
	}

	deps := runtime.AdoptedBuildDeps{
		Rulesets: rulesetReader.Rulesets,
		RefSHA:   forge.RefSHA,
	}
	self, _ := controllerBuild()
	request := runtime.AdoptedBuildRequest{
		Repository:    runtime.GitHubRepo{Owner: owner, Name: name},
		Revision:      strings.TrimSpace(flags.Revision),
		RepositoryDir: cwd,
		DoctorConfig:  doctorConfig,
		Policy:        runtime.DefaultTrustPolicy(),
	}

	// The output directory is derived from the revision only AFTER the proof
	// resolves it, so a caller cannot pick a directory that implies a revision
	// the builder did not verify. The two-phase call keeps that honest: the
	// first pass proves and builds into a staging directory, and installation
	// is the last step.
	staging, err := os.MkdirTemp("", "zenchron-adopted-stage-")
	if err != nil {
		return runtime.ExitFailed, err
	}
	defer os.RemoveAll(staging)
	request.OutputDir = staging

	provenance, err := runtime.BuildAdoptedController(context.Background(), request, deps, self)
	if err != nil {
		return runtime.ExitFailed, err
	}

	installDir := filepath.Join(output, provenance.Version)
	if err := os.MkdirAll(installDir, 0700); err != nil {
		return runtime.ExitFailed, err
	}
	installed := filepath.Join(installDir, "zenchron-engineering")
	if err := moveFile(provenance.OutputPath, installed); err != nil {
		return runtime.ExitFailed, err
	}
	if err := os.Chmod(installed, 0500); err != nil {
		return runtime.ExitFailed, err
	}
	provenance.OutputPath = installed
	digest, err := runtime.WriteAdoptedBuildProvenance(filepath.Join(installDir, "provenance.json"), provenance)
	if err != nil {
		return runtime.ExitFailed, err
	}

	fmt.Fprintf(stdout, "kind:              %s\n", provenance.Kind)
	fmt.Fprintf(stdout, "version:           %s\n", provenance.Version)
	fmt.Fprintf(stdout, "source:            %s\n", provenance.Source.Revision)
	fmt.Fprintf(stdout, "tree:              %s\n", provenance.Source.Tree)
	fmt.Fprintf(stdout, "trusted main:      %s\n", provenance.TrustedMain.Revision)
	fmt.Fprintf(stdout, "trust root:        ruleset %d %q %s\n", provenance.TrustRoot.RulesetID, provenance.TrustRoot.Name, provenance.TrustRoot.Digest)
	fmt.Fprintf(stdout, "binary sha256:     %s\n", provenance.BinarySHA256)
	fmt.Fprintf(stdout, "binary:            %s\n", provenance.OutputPath)
	fmt.Fprintf(stdout, "provenance sha256: %s\n", digest)
	fmt.Fprintf(stdout, "self report:       %s\n", provenance.SelfReport)
	return runtime.ExitCompleted, nil
}

// moveFile prefers a rename and falls back to a copy, because the staging
// directory and the install directory may be on different filesystems.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0600)
}
