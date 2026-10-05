package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configWith loads a complete operator layer with extra top-level members
// spliced in, e.g. `"supervisor": {"max_concurrent_runs": 2}`.
func configWith(t *testing.T, dir, extra string) Config {
	t.Helper()
	body := operatorConfigJSON(dir)
	if extra != "" {
		body = strings.TrimSuffix(strings.TrimSpace(body), "}") + ",\n" + extra + "\n}"
	}
	path := writeFile(t, filepath.Join(dir, "config.json"), body)
	config, err := LoadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// governedBy seeds the governing controller authority with this configuration
// identity, as an adopted controller's activation would have.
func governingAuthority(t *testing.T, store *SQLiteOperationStore, config ConfigDigest) {
	t.Helper()
	document, err := CanonicalJSON(ControllerAuthority{
		Kind: AuthorityHandoffActivation, Ref: "handoff-legacy",
		Binding: ControllerBinding{Controller: "controller-a", Config: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO controller_current_authority (id, kind, ref, updated_unix_nano, document)
		VALUES ('current', ?, ?, ?, ?)`, string(AuthorityHandoffActivation), "handoff-legacy", int64(1), string(document)); err != nil {
		t.Fatal(err)
	}
}

func bindingDigest(t *testing.T, config ConfigDigest) string {
	t.Helper()
	digest, err := ControllerBinding{Controller: "controller-a", Config: config}.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

const (
	ceilingTwo = `"supervisor": {"max_concurrent_runs": 2}`
	ceilingTen = `"supervisor": {"max_concurrent_runs": 10}`
)

// TestSupervisorPolicyLeavesTheControllerEffectiveDigest: an S member moves
// the legacy whole-file digest and the policy digest, never the C-only one; a
// C member moves the C-only one.
func TestSupervisorPolicyLeavesTheControllerEffectiveDigest(t *testing.T) {
	dir := t.TempDir()
	two, ten := configWith(t, dir, ceilingTwo), configWith(t, dir, ceilingTen)
	if two.Digest == ten.Digest {
		t.Fatal("the legacy digest no longer sees the supervisor ceiling, so the bridge below proves nothing")
	}
	if two.Effective != ten.Effective {
		t.Fatal("a supervisor ceiling still decides the controller-effective configuration")
	}
	p2, err := two.SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	p10, err := ten.SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p2 == p10 || p10.MaxConcurrentRuns != 10 {
		t.Fatalf("the supervisor policy does not carry the ceiling: %+v %+v", p2, p10)
	}
	base := configWith(t, dir, "")
	watch := configWith(t, dir, `"watch": {"poll_interval_seconds": 30, "max_concurrent_runs": 3, "max_concurrent_observations": 4, "max_concurrent_verifications": 1}`)
	if watch.Effective != base.Effective {
		t.Fatal("watch cadence and concurrency still decide the controller-effective configuration")
	}
	// The verification ceiling (#490) is S: it is in the recorded policy and
	// never in the controller-effective identity.
	verify := configWith(t, dir, `"supervisor": {"max_concurrent_verifications": 1}`)
	if verify.Effective != base.Effective {
		t.Fatal("the verification ceiling decides the controller-effective configuration")
	}
	pv, err := verify.SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	pb, err := base.SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if pv.MaxConcurrentVerifications != 1 || pb.MaxConcurrentVerifications != DefaultMaxConcurrentVerifications {
		t.Fatalf("the supervisor policy does not carry the effective verification ceiling: %+v %+v", pv, pb)
	}
	if labelled := configWith(t, dir, `"watch": {"label": "other-label"}`); labelled.Effective == base.Effective {
		t.Fatal("the watch label is authority (C) and must still decide the controller-effective configuration")
	}
	if agent := configWith(t, dir, `"planning_dir": "/elsewhere"`); agent.Effective == base.Effective {
		t.Fatal("a C member no longer decides the controller-effective configuration")
	}
}

// TestB4CrossesFromTheLegacyIdentityWithoutReAdoption is #346's acceptance:
// a legacy controller governs under its whole-file digest with live work
// bound to it; the first B4 start keeps that identity on proof of identical
// values; an S-only edit then keeps it on proof of identical C content, while
// recording a new SupervisorPolicyDigest and resolving the new ceiling; a
// genuine C change still does not.
func TestB4CrossesFromTheLegacyIdentityWithoutReAdoption(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	legacy := configWith(t, dir, ceilingTwo)
	governingAuthority(t, store, legacy.Digest)
	runsBoundTo := bindingDigest(t, legacy.Digest)

	// 1-4. The first B4 start, with the configuration unchanged.
	first, err := ResolveConfigIdentity(store, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != legacy.Digest || bindingDigest(t, first.Digest) != runsBoundTo {
		t.Fatal("the first B4 start does not bind as the governing legacy identity, so its predecessor would refuse it and every live run would park")
	}
	recorded, err := RecordSupervisorStart(store, first, time.Unix(1_800_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}

	// 5-8. Only the ceiling changes; ordinary restart.
	edited, err := ResolveConfigIdentity(store, configWith(t, dir, ceilingTen))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Digest != legacy.Digest || bindingDigest(t, edited.Digest) != runsBoundTo {
		t.Fatalf("an S-only edit changed the controller identity: %+v", edited.Digest)
	}
	restarted, err := RecordSupervisorStart(store, edited, time.Unix(1_800_000_100, 0).UTC())
	if err != nil {
		t.Fatalf("the restarted supervisor could not record its start: %v", err)
	}
	if restarted.PolicyDigest == recorded.PolicyDigest {
		t.Fatal("the new supervisor generation records the old policy")
	}
	settings, err := edited.WatchSettings()
	if err != nil || settings.MaxConcurrentRuns != 10 {
		t.Fatalf("the restarted supervisor resolves ceiling %d (%v), want 10", settings.MaxConcurrentRuns, err)
	}
	latest, found, err := store.LatestSupervisorStart()
	if err != nil || !found || latest.PolicyDigest != restarted.PolicyDigest {
		t.Fatalf("status would name policy %q (%v), want %q", latest.PolicyDigest, err, restarted.PolicyDigest)
	}

	// 9. A genuine C change still does not cross.
	changed := configWith(t, dir, ceilingTen+`, "planning_dir": "/elsewhere"`)
	changed, err = ResolveConfigIdentity(store, changed)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == legacy.Digest {
		t.Fatal("a controller-effective change kept the governing identity")
	}
	if _, err := RecordSupervisorStart(store, changed, time.Unix(1_800_000_200, 0).UTC()); err == nil {
		t.Fatal("a configuration the authority does not govern recorded a supervisor start")
	}
}

// TestCompatibilityIsNeverInferredFromAbsence (#346 acceptance 10): an S edit
// made before any B4 start recorded what the governing identity's C content
// is cannot be proven S-only, so it is treated as the change it might be.
func TestCompatibilityIsNeverInferredFromAbsence(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	governingAuthority(t, store, configWith(t, dir, ceilingTwo).Digest)
	edited, err := ResolveConfigIdentity(store, configWith(t, dir, ceilingTen))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Digest == configWith(t, dir, ceilingTwo).Digest {
		t.Fatal("an unproven S edit kept the legacy identity")
	}
}

// TestIdentityResolutionRules covers the remaining two rules: no governing
// authority leaves the legacy digest exactly as it was, and an authority that
// already names C-only content keeps that canonical form.
func TestIdentityResolutionRules(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	config := configWith(t, dir, ceilingTwo)
	ungoverned, err := ResolveConfigIdentity(store, config)
	if err != nil || ungoverned.Digest != config.Digest {
		t.Fatalf("without an authority the identity is %+v (%v), want the legacy digest", ungoverned.Digest, err)
	}
	governingAuthority(t, store, config.Effective)
	canonical, err := ResolveConfigIdentity(store, configWith(t, dir, ceilingTen))
	if err != nil || canonical.Digest != config.Effective {
		t.Fatalf("an authority naming C-only content resolved %+v (%v)", canonical.Digest, err)
	}
}

// TestTheRecordedPolicyIsTheEnforcedPolicy: the policy a driver records is the
// one it enforces - defaults applied and the supervisor/watch members combined
// stricter-wins - never merely what the file states. Two files that state
// different things but resolve to the same enforcement are the same policy,
// and an unstated member records the shipped default as an explicit value.
func TestTheRecordedPolicyIsTheEnforcedPolicy(t *testing.T) {
	dir := t.TempDir()
	unstated, err := configWith(t, dir, "").SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if unstated != (SupervisorPolicy{MaxConcurrentRuns: 1, MaxConcurrentObservations: DefaultMaxConcurrentObservations, PollIntervalSeconds: DefaultWatchPollSeconds}) {
		t.Fatalf("an unstated policy records %+v, not the defaults it enforces", unstated)
	}
	explicit, err := configWith(t, dir, `"supervisor": {"max_concurrent_runs": 1, "max_concurrent_observations": 2, "poll_interval_seconds": 60}`).SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if explicit != unstated {
		t.Fatalf("stating the defaults recorded %+v and leaving them unstated %+v: the same enforcement is two policies", explicit, unstated)
	}
	combined, err := configWith(t, dir, `"supervisor": {"max_concurrent_runs": 10, "poll_interval_seconds": 30}, "watch": {"max_concurrent_runs": 3, "poll_interval_seconds": 90}`).SupervisorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if combined.MaxConcurrentRuns != 3 || combined.PollIntervalSeconds != 90 {
		t.Fatalf("recorded %+v, want the stricter ceiling 3 and the longer interval 90 the supervisor enforces", combined)
	}
}
