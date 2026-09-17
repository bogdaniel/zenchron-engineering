package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestSuccessorStateFormatProbeFailsClosed(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	format, err := CurrentControllerStateFormat()
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"none", "schema", "migration", "protocol", "unknown"} {
		t.Run(change, func(t *testing.T) {
			value := format
			switch change {
			case "schema":
				value.Schema = "new"
			case "migration":
				value.Migrations = "changed-with-same-count"
			case "protocol":
				value.Protocol = "unknown"
			case "unknown":
				value = ControllerStateFormat{}
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(t.TempDir(), "probe")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat <<'FORMAT'\n"+string(data)+"\nFORMAT\n"), 0700); err != nil {
				t.Fatal(err)
			}
			err = CheckSuccessorStateFormat(context.Background(), binary)
			if (err == nil) != (change == "none") {
				t.Fatalf("state format %s: %v", change, err)
			}
		})
	}
}

func successorFixture(t *testing.T) (*SuccessorBuilder, *adoptedFixture, ControllerBuild) {
	t.Helper()
	f := newAdoptedFixture(t)
	active := ControllerBuild{Kind: ControllerAdopted, Version: "old", SourceRevision: adoptedGit(t, f.dir, "rev-parse", "HEAD^"), SourceTree: adoptedGit(t, f.dir, "rev-parse", "HEAD^^{tree}"), BinarySHA256: strings.Repeat("3", 64)}
	s := &SuccessorBuilder{Request: f.request(t), Deps: f.deps, Builder: active, StatusPath: filepath.Join(t.TempDir(), "status.json")}
	return s, f, active
}

func successorStatus(t *testing.T, s *SuccessorBuilder) SuccessorStatus {
	t.Helper()
	data, err := os.ReadFile(s.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	var status SuccessorStatus
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestSuccessorBuildReusesGovernedBuilderAndDeduplicates(t *testing.T) {
	s, f, active := successorFixture(t)
	p, err := s.Prepare(context.Background(), active)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source.Revision != f.head || p.Source.Tree != f.headTree || len(f.built) != 1 {
		t.Fatalf("wrong source or build count: %+v", p)
	}
	if successorStatus(t, s).State != "eligible" {
		t.Fatal("not eligible")
	}
	// Restart the observer; the provenance file, not an in-memory flag,
	// prevents a second build of the immutable version directory.
	s.prepared = nil
	if _, err := s.Prepare(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	if len(f.built) != 1 {
		t.Fatal("same revision rebuilt")
	}
	if p, err := s.Prepare(context.Background(), SuccessorIdentity(*p)); p != nil || err != nil {
		t.Fatal(p, err)
	}
	if successorStatus(t, s).State != "current" {
		t.Fatal("active revision not deduplicated")
	}
}

func TestSuccessorActiveRecordSurvivesLaterFailure(t *testing.T) {
	s, f, active := successorFixture(t)
	p, err := s.Prepare(context.Background(), active)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RememberActive(*p); err != nil {
		t.Fatal(err)
	}
	f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) { return nil, errors.New("unavailable") }
	if _, err := s.Prepare(context.Background(), active); err == nil {
		t.Fatal("unknown governance accepted")
	}
	restored, err := s.LastActive()
	if err != nil || restored == nil || SuccessorIdentity(*restored) != SuccessorIdentity(*p) {
		t.Fatal(restored, err)
	}
	if err := s.ForgetActive(); err != nil {
		t.Fatal(err)
	}
	if restored, err := s.LastActive(); restored != nil || err != nil {
		t.Fatal(restored, err)
	}
}

func TestSuccessorRefusesUnrelatedMain(t *testing.T) {
	s, f, active := successorFixture(t)
	s.Deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
		return RefObservation{Exists: true, SHA: f.orphan}, nil
	}
	if p, err := s.Prepare(context.Background(), active); p != nil || err == nil {
		t.Fatal("unrelated main was adopted")
	}
	if len(f.built) != 0 {
		t.Fatal("unrelated main was built")
	}
}

func TestSuccessorCandidateAndUnknownGovernanceNeverBuild(t *testing.T) {
	for _, refusal := range []string{"candidate", "unknown", "short_revision", "missing_governance"} {
		t.Run(refusal, func(t *testing.T) {
			s, f, active := successorFixture(t)
			switch refusal {
			case "candidate":
				s.Builder.Kind = ControllerPreAdoptionBuild
			case "unknown":
				f.governance.provenance.Role = "publication"
			case "short_revision":
				s.Deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
					return RefObservation{Exists: true, SHA: "abc123"}, nil
				}
			case "missing_governance":
				s.Deps.Governance = nil
			}
			if p, err := s.Prepare(context.Background(), active); err == nil || p != nil {
				t.Fatal("unsafe observation accepted")
			}
			if len(f.built) != 0 {
				t.Fatal("refused observer built an artifact")
			}
			if successorStatus(t, s).State != "blocked_governance_change" {
				t.Fatal("refusal not durable")
			}
		})
	}
}

func TestSuccessorFailureDoesNotLeakDiagnostics(t *testing.T) {
	s, _, active := successorFixture(t)
	s.Deps.Build = func(context.Context, AdoptedBuildSpec) (BuildEnvironment, error) {
		return BuildEnvironment{}, errors.New("secret response body")
	}
	if p, err := s.Prepare(context.Background(), active); err == nil || p != nil {
		t.Fatal("failed build accepted")
	}
	status := successorStatus(t, s)
	if status.Active != active || status.State != "build_failed" {
		t.Fatalf("%+v", status)
	}
	data, _ := os.ReadFile(s.StatusPath)
	if strings.Contains(string(data), "secret") {
		t.Fatal("raw error persisted")
	}
}

func TestSuccessorRechecksGovernanceAndArtifactOnReuse(t *testing.T) {
	for _, change := range []string{"governance", "binary", "probe", "path"} {
		t.Run(change, func(t *testing.T) {
			s, f, active := successorFixture(t)
			p, err := s.Prepare(context.Background(), active)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "governance":
				f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) { return nil, errors.New("unknown") }
			case "binary":
				if err := os.Chmod(p.OutputPath, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.OutputPath, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "probe":
				s.Deps.Probe = func(string) (ControllerBuild, error) { return active, nil }
			case "path":
				s.prepared.OutputPath = filepath.Join(t.TempDir(), "untrusted")
			}
			if p, err := s.Prepare(context.Background(), active); err == nil || p != nil {
				t.Fatal("changed evidence accepted")
			}
			if len(f.built) != 1 {
				t.Fatal("refused cached artifact rebuilt")
			}
		})
	}
}

func TestSuccessorGateIncludesAttemptOnlyPlansAndPreservesRuns(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if report, err := SuccessorCompatibilityReport(store); err != nil || len(report) != 0 {
		t.Fatal(report, err)
	}
	if _, err := store.ClaimPlanAttempt("attempt-only", "acme/widgets", time.Now()); err != nil {
		t.Fatal(err)
	}
	report, err := SuccessorCompatibilityReport(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || report[0].ID != "attempt-only" || report[0].Class != "compatible_with_migration" {
		t.Fatalf("%+v", report)
	}
	live := EngineeringRun{ID: "live", SchemaVersion: SchemaVersion, Disposition: Active, ControllerSHA256: strings.Repeat("a", 64)}
	if err := store.PutRun(live); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRun(EngineeringRun{ID: "done", SchemaVersion: SchemaVersion, Disposition: Completed}); err != nil {
		t.Fatal(err)
	}
	report, err = SuccessorCompatibilityReport(store)
	if err != nil || len(report) != 2 {
		t.Fatal(report, err)
	}
	after, found, err := store.Run(live.ID)
	if err != nil || !found || after.ControllerSHA256 != live.ControllerSHA256 {
		t.Fatal("compatibility check changed historical controller", err)
	}
}
