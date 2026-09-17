package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// SuccessorStatus deliberately stores identities and classifications, never
// forge response bodies, credentials, or candidate-authored diagnostics.
type SuccessorStatus struct {
	At               time.Time                `json:"at"`
	State            string                   `json:"state"`
	ObservedRevision string                   `json:"observed_revision,omitempty"`
	Active           ControllerBuild          `json:"active"`
	Successor        *AdoptedBuildProvenance  `json:"successor,omitempty"`
	Compatibility    []SuccessorCompatibility `json:"compatibility,omitempty"`
}

type SuccessorCompatibility struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Class string `json:"class"`
}

// ControllerStateFormat is intentionally stricter than a version number. A
// changed migration with an unchanged count must not reach the live database
// and make rollback to the serving controller impossible.
type ControllerStateFormat struct {
	Protocol   string `json:"protocol"`
	Schema     string `json:"schema"`
	Migrations string `json:"migrations_sha256"`
}

func CurrentControllerStateFormat() (ControllerStateFormat, error) {
	digest, err := Digest(sqliteMigrations)
	return ControllerStateFormat{Protocol: "successor/1", Schema: SchemaVersion, Migrations: digest}, err
}

func CheckSuccessorStateFormat(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, binary, "controller", "inspect-state-format").Output()
	if err != nil {
		return fmt.Errorf("successor cannot report its state format: %w", err)
	}
	var reported ControllerStateFormat
	if err := json.Unmarshal(data, &reported); err != nil {
		return fmt.Errorf("successor state format is unknown")
	}
	expected, err := CurrentControllerStateFormat()
	if err != nil {
		return err
	}
	if reported != expected {
		return fmt.Errorf("successor state format needs an explicit migration")
	}
	return nil
}

// SuccessorCompatibilityReport never rewrites genesis or approved plans.
// This runtime has no cross-controller migration protocol yet. Existing work
// therefore needs an explicit migration and prevents unattended activation.
// Attempt-only plans count too: Plans() alone would silently miss them.
func SuccessorCompatibilityReport(store *SQLiteOperationStore) ([]SuccessorCompatibility, error) {
	runs, err := store.Runs()
	if err != nil {
		return nil, err
	}
	var out []SuccessorCompatibility
	for _, run := range runs {
		class := "compatible_with_migration"
		if run.SchemaVersion != SchemaVersion {
			class = "unsupported_schema"
		} else {
			snapshot, err := store.Replay(run.ID)
			if err != nil {
				return nil, err
			}
			if terminalDisposition(snapshot.Disposition) {
				continue
			}
		}
		out = append(out, SuccessorCompatibility{ID: run.ID, Kind: "run", Class: class})
	}
	plans, err := store.PlanIdentities()
	if err != nil {
		return nil, err
	}
	states := map[string]PlanSummary{}
	for _, summary := range summarizePlans(store) {
		if summary.Error != "" {
			return nil, fmt.Errorf("plan compatibility could not be established")
		}
		states[summary.PlanID] = summary
	}
	for _, plan := range plans {
		state, found := states[plan.PlanID]
		if found && (state.State == PlanStateCompleted || state.State == PlanStateRejected) {
			continue
		}
		out = append(out, SuccessorCompatibility{ID: plan.PlanID, Kind: "plan", Class: "compatible_with_migration"})
	}
	return out, nil
}

// SuccessorBuilder is called serially by the adopted serving guardian. The
// builder remains the sole adoption path; discovery confers no authority.
type SuccessorBuilder struct {
	Request    AdoptedBuildRequest
	Deps       AdoptedBuildDeps
	Builder    ControllerBuild
	StatusPath string
	prepared   *AdoptedBuildProvenance
}

func exactRevision(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (s *SuccessorBuilder) Record(status SuccessorStatus) error {
	status.At = time.Now().UTC()
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.StatusPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.StatusPath), ".successor-status-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), s.StatusPath)
}

// RememberActive is separate from the changing observation status: a failed
// attempt at C must not erase the last successfully activated B on restart.
func (s *SuccessorBuilder) RememberActive(p AdoptedBuildProvenance) error {
	copy := *s
	copy.StatusPath = filepath.Join(s.Request.OutputRoot, "active.json")
	return copy.Record(SuccessorStatus{State: "active", Active: SuccessorIdentity(p), Successor: &p})
}

func (s *SuccessorBuilder) ForgetActive() error {
	err := os.Remove(filepath.Join(s.Request.OutputRoot, "active.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *SuccessorBuilder) LastActive() (*AdoptedBuildProvenance, error) {
	data, err := os.ReadFile(filepath.Join(s.Request.OutputRoot, "active.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var status SuccessorStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, err
	}
	p := status.Successor
	if p == nil || status.State != "active" || status.Active != SuccessorIdentity(*p) || !exactRevision(p.Source.Revision) || !exactRevision(p.Source.Tree) || p.Kind != ControllerAdopted || p.Repository != s.Request.Repository.String() || p.OutputPath != filepath.Join(s.Request.OutputRoot, "successor-"+p.Source.Revision, "zenchron-engineering") {
		return nil, fmt.Errorf("last active successor record is invalid")
	}
	if err := verifySuccessorArtifact(*p, s.Deps.withDefaults()); err != nil {
		return nil, err
	}
	return p, nil
}

// Prepare deduplicates successful builds, including across guardian restarts.
// Cached artifacts never bypass a fresh governance observation or measurement.
func (s *SuccessorBuilder) Prepare(ctx context.Context, active ControllerBuild) (p *AdoptedBuildProvenance, err error) {
	status := SuccessorStatus{Active: active, State: "blocked_governance_change"}
	defer func() {
		if recordErr := s.Record(status); recordErr != nil {
			p, err = nil, recordErr
		}
	}()
	if s.Builder.Kind != ControllerAdopted || s.Builder.validateAttested() != nil || !exactRevision(s.Builder.SourceRevision) || !exactRevision(s.Builder.SourceTree) {
		return nil, fmt.Errorf("only an adopted controller may supervise successor adoption")
	}
	deps := s.Deps.withDefaults()
	if deps.Governance == nil || deps.RefSHA == nil {
		return nil, fmt.Errorf("successor observation requires forge and governance identities")
	}
	identity := deps.Governance.GovernanceProvenance()
	if identity.Role != CredentialRoleGovernance || identity.Method == "" {
		return nil, fmt.Errorf("unknown governance observation identity")
	}
	policy := DefaultTrustPolicy()
	root, err := observeTrustRoot(ctx, deps, s.Request.Repository, policy)
	if err != nil {
		return nil, err
	}
	digest, err := trustRootDigest(root)
	if err != nil {
		return nil, err
	}
	revision, err := observeTrustedMain(ctx, deps, s.Request.Repository, "main")
	if err != nil {
		return nil, err
	}
	if !exactRevision(revision) {
		return nil, fmt.Errorf("trusted main is not an exact revision")
	}
	status.ObservedRevision = revision
	if revision == active.SourceRevision {
		status.State = "current"
		return nil, nil
	}
	status.State = "main_changed"
	if err := s.Record(status); err != nil {
		return nil, err
	}
	if err := deps.Fetch(s.Request.RepositoryDir, revision, "main"); err != nil {
		return nil, err
	}
	if !exactRevision(active.SourceRevision) {
		return nil, fmt.Errorf("active source revision is not exact")
	}
	if _, err := deps.Git(s.Request.RepositoryDir, "merge-base", "--is-ancestor", active.SourceRevision, revision); err != nil {
		status.State = "blocked_governance_change"
		return nil, fmt.Errorf("trusted main is not a successor of the active controller")
	}
	tree, err := revisionTree(deps, s.Request.RepositoryDir, revision)
	if err != nil {
		return nil, err
	}
	request := s.Request
	request.Revision, request.Version = revision, "successor-"+revision
	if s.prepared == nil {
		// Only reuse a record in the runtime-owned output root. OutputPath in
		// that record is checked below before any executable is invoked.
		data, readErr := os.ReadFile(filepath.Join(request.OutputRoot, request.Version, "provenance.json"))
		if readErr == nil {
			var cached AdoptedBuildProvenance
			if json.Unmarshal(data, &cached) == nil {
				s.prepared = &cached
			}
		}
	}
	if s.prepared == nil || s.prepared.Source.Revision != revision || s.prepared.TrustRoot.Digest != digest {
		status.State = "build_failed"
		built, buildErr := BuildAdoptedController(ctx, request, deps, BuilderRecord{Kind: s.Builder.Kind, Version: s.Builder.Version, SourceRevision: s.Builder.SourceRevision})
		if buildErr != nil {
			return nil, buildErr
		}
		s.prepared = &built
	}
	p = s.prepared
	status.State = "artifact_verification_failed"
	expectedPath := filepath.Join(request.OutputRoot, request.Version, "zenchron-engineering")
	if p.OutputPath != expectedPath || p.Repository != request.Repository.String() || p.SchemaVersion != adoptedBuildSchemaVersion || p.Source.Revision != revision || p.Source.Tree != tree || !exactRevision(p.Source.Tree) || p.Kind != ControllerAdopted || !p.SelfProbe.Matched || !p.TrustRoot.Bypass.Observed || p.TrustRoot.Bypass.Count != 0 {
		return nil, fmt.Errorf("successor provenance does not match the governed request")
	}
	if err := verifySuccessorArtifact(*p, deps); err != nil {
		return nil, err
	}
	// Main and the gate must still be the observed ones, including when the
	// adopted builder would allow an ancestor after a concurrent merge.
	finalRoot, err := observeTrustRoot(ctx, deps, request.Repository, policy)
	if err != nil {
		return nil, err
	}
	finalDigest, err := trustRootDigest(finalRoot)
	if err != nil {
		return nil, err
	}
	finalMain, err := observeTrustedMain(ctx, deps, request.Repository, "main")
	if err != nil {
		return nil, err
	}
	if finalDigest != digest || finalMain != revision {
		status.State = "blocked_governance_change"
		return nil, fmt.Errorf("trusted main or governance changed before activation")
	}
	status.State, status.Successor = "eligible", p
	return p, nil
}

func SuccessorIdentity(p AdoptedBuildProvenance) ControllerBuild {
	return ControllerBuild{Kind: p.Kind, Version: p.Version, SourceRevision: p.Source.Revision, SourceTree: p.Source.Tree, BinarySHA256: p.BinarySHA256}
}

func VerifySuccessorArtifact(p AdoptedBuildProvenance) error {
	return verifySuccessorArtifact(p, AdoptedBuildDeps{}.withDefaults())
}

func verifySuccessorArtifact(p AdoptedBuildProvenance, deps AdoptedBuildDeps) error {
	digest, err := deps.Measure(p.OutputPath)
	if err != nil {
		return err
	}
	if digest != p.BinarySHA256 {
		return fmt.Errorf("successor binary digest changed")
	}
	reported, err := deps.Probe(p.OutputPath)
	if err != nil {
		return err
	}
	if reported != SuccessorIdentity(p) {
		return fmt.Errorf("successor self-probe disagrees with provenance")
	}
	return nil
}
