package runtime

// A PLAN BELONGS TO THE CONFIGURATION IT WAS PROPOSED UNDER (#307, #89).
//
// A run records its controller and parks as controller_changed. A plan whose
// next agent stage has not started has no run at all, so a re-adoption to a
// new controller-effective configuration used to pass it by, and the supervisor
// then dispatched its remaining stages as new runs under the new one. The hold
// is decided by configuration, never by when anything happened: plans cannot be
// cancelled, so a false hold is permanent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var configAfterTheChange = ConfigDigest{Global: "config-after-the-change"}

// planTickAfterReadoption records the fixture plan under planConfig, performs a
// real cold re-adoption to readoptTo (with no previous authority, as the
// fixture has none), and runs one supervisor tick whose plan service governs
// under readoptTo. It returns the plan's report and how many runs the tick
// created.
func planTickAfterReadoption(t *testing.T, planConfig, readoptTo func(*phase8Fixture) ConfigDigest, legacy bool) (PlanTickReport, int) {
	t.Helper()
	c, request := readoptionFixture(t)
	plan := newPlanRunFixtureOn(t, c.fixture, parallelStages(), planConfig(c.fixture))
	plan.approve(t)
	if legacy {
		if _, err := c.store.db.Exec(`UPDATE plans SET config_digest = '' WHERE id = ?`, plan.plan.ID); err != nil {
			t.Fatal(err)
		}
	}
	settleLiveRuns(t, c, request.Now)
	request.Now = c.fixture.clock.Now()
	request.Binding.Config = readoptTo(c.fixture)
	if _, err := ReadoptController(c.store, readoptionLease(t, c), request); err != nil {
		t.Fatalf("the re-adoption was refused: %v", err)
	}
	plan.service.Config = readoptTo(c.fixture)
	runsBefore, err := c.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: c.store, Clock: c.fixture.clock, Owner: "owner-1",
		Liveness:          OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:      []GitHubRepo{repo},
		MaxConcurrentRuns: 2,
		PollInterval:      time.Minute,
		Agents:            supervisorRegistry(t),
		Plans:             plan.service,
		Runtime:           func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) { return c.fixture.runtime, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plans) != 1 {
		t.Fatalf("the tick reconciled %d plans", len(report.Plans))
	}
	runsAfter, err := c.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	return report.Plans[0], len(runsAfter) - len(runsBefore)
}

func configX(f *phase8Fixture) ConfigDigest { return f.deps.ConfigDigest }
func configY(*phase8Fixture) ConfigDigest   { return configAfterTheChange }

func requireHeld(t *testing.T, report PlanTickReport, created int) {
	t.Helper()
	if len(report.Started) != 0 || created != 0 {
		t.Fatalf("a plan from another configuration dispatched across the re-adoption: started=%#v created=%d", report.Started, created)
	}
	if !strings.Contains(report.Waiting, "configuration_changed") {
		t.Fatalf("the plan does not say why it is held: %q", report.Waiting)
	}
}

func requireDispatched(t *testing.T, report PlanTickReport) {
	t.Helper()
	if len(report.Started) != 2 || report.Waiting != "" {
		t.Fatalf("a plan proposed under the governing configuration was held: %#v", report)
	}
}

// (b) Proposed under X, re-adopted to Y: held.
func TestAPlanFromAnotherConfigurationIsHeldAcrossAReadoption(t *testing.T) {
	report, created := planTickAfterReadoption(t, configX, configY, false)
	requireHeld(t, report, created)
}

// (a) The operator edits the configuration to Y and proposes BEFORE running the
// re-adoption to Y. The plan is Y's, whenever it was claimed.
func TestAPlanProposedUnderTheNewConfigurationBeforeReadoptionDispatches(t *testing.T) {
	report, _ := planTickAfterReadoption(t, configY, configY, false)
	requireDispatched(t, report)
}

// (c) A first re-adoption - no previous authority - under the configuration the
// plan was proposed under changes nothing about the plan.
func TestAFirstReadoptionUnderTheSameConfigurationHoldsNothing(t *testing.T) {
	report, _ := planTickAfterReadoption(t, configX, configX, false)
	requireDispatched(t, report)
}

// (d) A legacy plan records no configuration. Its identity is judged instead: an
// id this configuration does not derive for its source is held.
func TestALegacyPlanIsJudgedByItsDerivedIdentity(t *testing.T) {
	report, created := planTickAfterReadoption(t, configX, configX, true)
	requireHeld(t, report, created)

	// And a legacy plan whose id IS the one the governing configuration
	// derives for its source is that configuration's plan.
	f := newPhase8Fixture(t)
	derived, err := f.runtime.PlanID(f.issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimPlanAttempt(derived, f.deps.Repository.Identity, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BindPlanSource(derived, f.issue); err != nil {
		t.Fatal(err)
	}
	if held, err := planConfigurationHold(f.store, derived, f.deps.ConfigDigest); err != nil || held != "" {
		t.Fatalf("a legacy plan with this configuration's derived id was held: %q %v", held, err)
	}
	if held, err := planConfigurationHold(f.store, derived, configAfterTheChange); err != nil || held == "" {
		t.Fatalf("a legacy plan derived under another configuration was not held: %q %v", held, err)
	}
}

// The column is additive: a plan written before it migrates as a legacy plan
// (recorded, with no configuration) rather than being lost or invented one.
func TestAPrePlanConfigurationRowMigratesAsLegacy(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(dir, "runtime.db"))+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	before := sqliteMigrations[:len(sqliteMigrations)-1]
	for _, migration := range before {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(before))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO plans (` + sqlitePlanColumns + `) VALUES ('plan-old', 'acme/repo', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recorded, found, err := store.planConfig("plan-old")
	if err != nil || !found || recorded != "" {
		t.Fatalf("the pre-existing plan migrated as %q found=%v err=%v, want a legacy plan", recorded, found, err)
	}
}

// (e) Approving or revising a held plan is refused up front: an approval could
// never dispatch, and a revision would spend a planning invocation on it.
func TestAHeldPlanCannotBeApprovedOrRevised(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	service := fixture.service
	service.Config = configAfterTheChange

	_, err := service.Approve(fixture.plan.ID, fixture.plan.Revision, fixture.plan.Digest,
		shownAssignments(t, fixture.service, fixture.plan.ID, fixture.plan.Revision), "operator", "looks right")
	var refused *PlanRefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "configuration_changed") {
		t.Fatalf("approving a held plan was not refused: %v", err)
	}

	_, err = service.Propose(context.Background(), ProposeInput{PlanID: fixture.plan.ID, Issue: fixture.issue, Repository: "acme/repo"})
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "configuration_changed") {
		t.Fatalf("revising a held plan was not refused: %v", err)
	}
	identities, err := fixture.store.PlanIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].Revision != 1 {
		t.Fatalf("the refused revision wrote something: %#v", identities)
	}
}
