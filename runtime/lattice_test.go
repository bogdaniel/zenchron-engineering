package runtime

// lattice_test.go is the Phase 10 audit of the complete configuration lattice
// (§16) and of the unsafe-override surface (§15).
//
// The invariant under audit is one sentence:
//
//	hard safety ceiling  ∩  operator/global config  ∩  repository tightening
//	  ∩  CLI tightening  =  effective configuration
//
// Every test below is an intersection proof for one settable dimension. The
// three questions asked of each are always the same, which is why they are
// tables and not prose: a repository proposing a TIGHTER value wins, a
// repository proposing a LOOSER value is REFUSED (never silently clamped, so
// the file and the effective configuration can never disagree), and a member
// the repository has no authority to name at all is refused as an authority
// violation BEFORE the file is decoded.
//
// The §15 half is an audit result, not a feature: M0 has NO unsafe runtime
// bypass that configuration can enable. There is no member in either layer
// that names one, so the frozen both-conditions rule (operator policy AND an
// explicit CLI invocation, with durable provenance) has nothing to govern yet.
// The tests below keep it that way by pinning both layers to an exact member
// set, so a future member that would be an unsafe override cannot arrive
// silently - it has to fail this file first.

import (
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// The lattice, stated structurally
// ---------------------------------------------------------------------------

// configMembers is the complete set of dotted JSON member names a
// configuration struct can carry. It is how "what is settable at all" becomes
// an assertion rather than a reading of the struct.
func configMembers(typ reflect.Type, prefix string) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var members []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		inner := field.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct {
			members = append(members, configMembers(inner, prefix+name+".")...)
			continue
		}
		members = append(members, prefix+name)
	}
	sort.Strings(members)
	return members
}

func requireMembers(t *testing.T, label string, typ reflect.Type, want []string) {
	t.Helper()
	got := configMembers(typ, "")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s members = %v, want exactly %v (a new member is a new authority: state which layer owns it, and if it is an unsafe override wire it to the both-conditions rule before adding it here)", label, got, want)
	}
}

// TestOperatorLayerCarriesNoUnsafeOverrideMember is the §15 audit assertion.
// M0 has no configurable unsafe runtime bypass, so the operator layer must
// stay exactly this set. Adding a member that permits an unsafe capability
// fails here first, which is the point: the frozen rule requires operator
// policy AND an explicit CLI invocation AND durable provenance, and none of
// that exists to be wired to yet.
func TestOperatorLayerCarriesNoUnsafeOverrideMember(t *testing.T) {
	requireMembers(t, "OperatorConfig", reflect.TypeOf(OperatorConfig{}), []string{
		"state_dir",
		"project_model_path",
		"policy_path",
		"assurance.image",
		"assurance.docker_host",
		"assurance.dependency_cache_dir",
		"provider.kind",
		"provider.model",
		"provider.auth_mode",
		"provider.credential_path",
		"provider.endpoint",
		"github.credential_mode",
		"github.endpoint",
		"budgets.wall_limit_seconds",
		"budgets.max_execution_attempts",
		"budgets.max_execution_continuations",
		"budgets.max_remediation_attempts",
		"budgets.max_assurance_attempts",
		"watch.repositories",
		"watch.label",
		"watch.poll_interval_seconds",
		"watch.max_concurrent_runs",
		"gc.retention_hours",
		"operator.id",
		"operator.require_configured_id",
	})
}

// TestRepositoryLayerReachesOnlyTightenableBounds is the other half: the
// repository layer's ENTIRE reachable surface is six numeric bounds. There is
// no capability, credential, provider, endpoint, transport, image, state
// directory, operator identity, retention window, enrolment, or publication
// member for a repository to name, so widening any of those is not a rule that
// has to hold at runtime - it is unreachable.
func TestRepositoryLayerReachesOnlyTightenableBounds(t *testing.T) {
	requireMembers(t, "RepositoryConfig", reflect.TypeOf(RepositoryConfig{}), []string{
		"budgets.wall_limit_seconds",
		"budgets.max_execution_attempts",
		"budgets.max_execution_continuations",
		"budgets.max_remediation_attempts",
		"budgets.max_assurance_attempts",
		"watch.poll_interval_seconds",
		"watch.max_concurrent_runs",
	})
	// The pre-decode allowlists must describe that same surface. They are the
	// enforcement; the struct is only the shape.
	if !reflect.DeepEqual(sortedKeys(repositoryScope), []string{"budgets", "watch"}) {
		t.Fatalf("repositoryScope = %v, want exactly [budgets watch]", sortedKeys(repositoryScope))
	}
	if !reflect.DeepEqual(sortedKeys(repositoryWatchScope), []string{"max_concurrent_runs", "poll_interval_seconds"}) {
		t.Fatalf("repositoryWatchScope = %v, want exactly [max_concurrent_runs poll_interval_seconds]", sortedKeys(repositoryWatchScope))
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Dimensions a repository may not name at all
// ---------------------------------------------------------------------------

// TestRepositoryCannotNameAnOperatorDimension walks every operator-authority
// dimension and proves the refusal is an AUTHORITY violation raised before the
// file is decoded.
//
// Each proposal carries a value of the wrong JSON type on purpose. A decoder
// reaching it would complain about the type; refuseOutOfScope reaching it
// first complains about authority. Asserting the message is about authority
// and NOT about unmarshalling is therefore a proof of ordering, not of
// wording.
func TestRepositoryCannotNameAnOperatorDimension(t *testing.T) {
	for _, c := range []struct{ dimension, member, wants string }{
		{"capabilities / assurance image", `"assurance": 7`, `"assurance"`},
		{"state dir", `"state_dir": []`, `"state_dir"`},
		{"project model", `"project_model_path": 0`, `"project_model_path"`},
		{"policy", `"policy_path": false`, `"policy_path"`},
		{"provider / credential authority", `"provider": "openai"`, `"provider"`},
		{"forge credential authority", `"github": 1`, `"github"`},
		{"gc retention", `"gc": 999999`, `"gc"`},
		{"operator identity", `"operator": ["attacker"]`, `"operator"`},
		{"watch enrolment", `"watch": {"repositories": 5}`, `"watch.repositories"`},
		{"watch opt-in label", `"watch": {"label": 5}`, `"watch.label"`},
		// Alongside members it legitimately may name, so the refusal is not an
		// artefact of the file containing nothing else.
		{"gc beside a legal bound", `"budgets": {"wall_limit_seconds": 60}, "gc": {"retention_hours": 1}`, `"gc"`},
		{"enrolment beside a legal bound", `"watch": {"max_concurrent_runs": 1, "repositories": ["attacker/self"]}`, `"watch.repositories"`},
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), RepositoryConfigFile), "{"+c.member+"}")
		config, digest, present, err := LoadRepositoryConfig(path)
		configErr := requireConfigError(t, err)
		if !strings.Contains(configErr.Detail, c.wants) || !strings.Contains(configErr.Detail, "operator authority") {
			t.Fatalf("%s: expected an authority refusal naming %s, got %q", c.dimension, c.wants, configErr.Detail)
		}
		if strings.Contains(configErr.Detail, "cannot unmarshal") {
			t.Fatalf("%s: the member was decoded before its authority was checked: %q", c.dimension, configErr.Detail)
		}
		if present || digest != "" || config.Budgets != nil || config.Watch != nil {
			t.Fatalf("%s: a refused layer was still returned: present=%v digest=%q %+v", c.dimension, present, digest, config)
		}
	}
}

// ---------------------------------------------------------------------------
// Dimensions a repository may tighten
// ---------------------------------------------------------------------------

// latticeOperator is one operator layer that pins every tightenable dimension
// to an explicit value, so all six cases below meet the same ceiling.
func latticeOperator(t *testing.T, dir string) string {
	t.Helper()
	return operatorConfigWithWatch(t, dir, `{"repositories": ["owner/name"], "poll_interval_seconds": 120, "max_concurrent_runs": 2}`)
}

func latticeWatchSeconds(t *testing.T, config Config) int {
	t.Helper()
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	return int(settings.PollInterval / time.Second)
}

func latticeWatchRuns(t *testing.T, config Config) int {
	t.Helper()
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	return settings.MaxConcurrentRuns
}

// TestTightenLatticePerDimension is the §16 table. Every dimension a
// repository can address is asked the same three questions.
//
// The looser column states the direction that WIDENS authority for that
// dimension, which for polling is inverted: asking to be polled more often is
// asking for more of the operator's runtime, so a SHORTER interval is the
// widening proposal.
func TestTightenLatticePerDimension(t *testing.T) {
	for _, c := range []struct {
		dimension   string
		tighter     string
		wantTighter int
		looser      string
		zero        string
		get         func(*testing.T, Config) int
	}{
		{
			"budgets.wall_limit_seconds", `{"budgets": {"wall_limit_seconds": 60}}`, 60,
			`{"budgets": {"wall_limit_seconds": 3601}}`,
			`{"budgets": {"wall_limit_seconds": 0}}`,
			func(_ *testing.T, c Config) int { return c.Budgets.WallLimitSeconds },
		},
		{
			"budgets.max_execution_attempts", `{"budgets": {"max_execution_attempts": 1}}`, 1,
			`{"budgets": {"max_execution_attempts": 4}}`,
			`{"budgets": {"max_execution_attempts": 0}}`,
			func(_ *testing.T, c Config) int { return c.Budgets.MaxExecutionAttempts },
		},
		{
			"budgets.max_remediation_attempts", `{"budgets": {"max_remediation_attempts": 1}}`, 1,
			`{"budgets": {"max_remediation_attempts": 3}}`,
			`{"budgets": {"max_remediation_attempts": 0}}`,
			func(_ *testing.T, c Config) int { return c.Budgets.MaxRemediationAttempts },
		},
		{
			"budgets.max_assurance_attempts", `{"budgets": {"max_assurance_attempts": 1}}`, 1,
			`{"budgets": {"max_assurance_attempts": 3}}`,
			`{"budgets": {"max_assurance_attempts": 0}}`,
			func(_ *testing.T, c Config) int { return c.Budgets.MaxAssuranceAttempts },
		},
		{
			// Inverted: tighter is a LONGER interval.
			"watch.poll_interval_seconds", `{"watch": {"poll_interval_seconds": 900}}`, 900,
			`{"watch": {"poll_interval_seconds": 119}}`,
			`{"watch": {"poll_interval_seconds": 0}}`,
			latticeWatchSeconds,
		},
		{
			"watch.max_concurrent_runs", `{"watch": {"max_concurrent_runs": 1}}`, 1,
			`{"watch": {"max_concurrent_runs": 3}}`,
			`{"watch": {"max_concurrent_runs": 0}}`,
			latticeWatchRuns,
		},
	} {
		// Tighter wins, and is the value the effective configuration reports.
		dir := t.TempDir()
		path := latticeOperator(t, dir)
		writeFile(t, filepath.Join(dir, RepositoryConfigFile), c.tighter)
		config, err := LoadConfig(path, dir)
		if err != nil {
			t.Fatalf("%s: tightening was refused: %v", c.dimension, err)
		}
		if got := c.get(t, config); got != c.wantTighter {
			t.Fatalf("%s: effective value = %d, want the repository's tighter %d", c.dimension, got, c.wantTighter)
		}
		if config.Digest.Repository == "" || config.Digest.Global == "" {
			t.Fatalf("%s: both layers must be recorded in the digest: %+v", c.dimension, config.Digest)
		}

		// Looser is REFUSED, not clamped. The refusal names the dimension and
		// the file, so an author learns the bound instead of silently getting
		// a different effective configuration than the file states.
		loose := t.TempDir()
		loosePath := latticeOperator(t, loose)
		writeFile(t, filepath.Join(loose, RepositoryConfigFile), c.looser)
		configErr := requireConfigError(t, second2(LoadConfig(loosePath, loose)))
		if !strings.Contains(configErr.Detail, "only tighten") || !strings.Contains(configErr.Detail, c.dimension) {
			t.Fatalf("%s: expected a tighten-only refusal naming the dimension, got %q", c.dimension, configErr.Detail)
		}
		if configErr.Path == "" {
			t.Fatalf("%s: the refusal must name the repository file", c.dimension)
		}

		// Zero is refused too, in every dimension. Absent means "propose
		// nothing"; zero would otherwise read as tightening to nothing, and
		// for polling it is the widest proposal there is.
		zero := t.TempDir()
		zeroPath := latticeOperator(t, zero)
		writeFile(t, filepath.Join(zero, RepositoryConfigFile), c.zero)
		requireConfigError(t, second2(LoadConfig(zeroPath, zero)))
	}
}

// TestTighteningIsIdempotentAcrossTheWholeLattice proves the intersection is a
// meet and not a sequence of independent writes: a repository that tightens
// every dimension it may name at once gets exactly those values, every
// dimension it does NOT name keeps the operator value, and the operator-only
// dimensions are untouched by any of it.
func TestTighteningIsIdempotentAcrossTheWholeLattice(t *testing.T) {
	dir := t.TempDir()
	path := latticeOperator(t, dir)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{
		"budgets": {"wall_limit_seconds": 60, "max_execution_attempts": 1},
		"watch": {"poll_interval_seconds": 300, "max_concurrent_runs": 1}
	}`)
	config, err := LoadConfig(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if config.Budgets.WallLimitSeconds != 60 || config.Budgets.MaxExecutionAttempts != 1 {
		t.Fatalf("named bounds did not tighten: %+v", config.Budgets)
	}
	if config.Budgets.MaxRemediationAttempts != 2 || config.Budgets.MaxAssuranceAttempts != 2 {
		t.Fatalf("unnamed bounds moved: %+v", config.Budgets)
	}
	if got := latticeWatchSeconds(t, config); got != 300 {
		t.Fatalf("watch interval = %ds, want 300", got)
	}
	if got := latticeWatchRuns(t, config); got != 1 {
		t.Fatalf("watch concurrency = %d, want 1", got)
	}
	// Operator-only dimensions survive intact: enrolment, opt-in label,
	// retention, assurance image, provider, forge credential mode, state dir.
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Repositories) != 1 || settings.Repositories[0].String() != "owner/name" || settings.Label != DefaultWatchLabel {
		t.Fatalf("enrolment or opt-in label moved: %+v %q", settings.Repositories, settings.Label)
	}
	if config.GCRetention() != DefaultGCRetention {
		t.Fatalf("gc retention moved: %s", config.GCRetention())
	}
	if config.Assurance.Image != testImage || config.Provider.Kind != ProviderOpenAI || config.GitHub.CredentialMode != GitHubCredentialCLI {
		t.Fatalf("an operator-only dimension moved: %+v", config.OperatorConfig)
	}
	if config.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("state dir moved: %q", config.StateDir)
	}
}

// TestHardSafetyCeilingIsBelowEveryConfiguredLayer proves the outermost term
// of the intersection. MinWatchPollSeconds is a global floor no layer may pass
// - the operator layer is refused outright below it, and the repository layer
// cannot reach it because its own proposal must already be no more frequent
// than an operator interval that has been validated against the floor.
func TestHardSafetyCeilingIsBelowEveryConfiguredLayer(t *testing.T) {
	for _, seconds := range []int{-MinWatchPollSeconds, -1, 1, MinWatchPollSeconds - 1} {
		dir := t.TempDir()
		path := operatorConfigWithWatch(t, dir, `{"repositories": ["owner/name"], "poll_interval_seconds": `+strconv.Itoa(seconds)+`}`)
		configErr := requireConfigError(t, second(LoadOperatorConfig(path)))
		if !strings.Contains(configErr.Detail, "poll_interval_seconds") {
			t.Fatalf("%ds: expected the floor to be named, got %q", seconds, configErr.Detail)
		}
	}
	// resolveMaxConcurrentRuns is the same shape for concurrency and is
	// covered by TestOperatorCeilingCannotBeRaisedByConfiguration; what is
	// asserted here is that the effective watch settings never exceed the
	// operator's authorization no matter what the repository proposed.
	dir := t.TempDir()
	path := latticeOperator(t, dir)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"watch": {"max_concurrent_runs": 2}}`)
	config, err := LoadConfig(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := latticeWatchRuns(t, config); got > 2 {
		t.Fatalf("effective concurrency %d exceeds the operator authorization of 2", got)
	}
}

// ---------------------------------------------------------------------------
// §15 - the four things no override may ever do
// ---------------------------------------------------------------------------

// TestNoConfigurationAuthorizesSelfAdoption is the first frozen prohibition.
// Human authority over PUBLICATION is legitimate; adoption is not, and the
// boundary is an allowlist, so an adoption-shaped action is unsupported rather
// than merely absent from a deny-list. Nothing in either configuration layer
// contributes to this map, which is why the assertion is on the map itself.
func TestNoConfigurationAuthorizesSelfAdoption(t *testing.T) {
	if !reflect.DeepEqual(sortedKeys(authorizableActions), []string{"candidate.push", "git.pull_request.create", "git.pull_request.update"}) {
		t.Fatalf("authorizableActions = %v, want exactly the three publication actions: adoption is not human-authorizable", sortedKeys(authorizableActions))
	}
	for _, action := range []string{
		"git.pull_request.merge",
		"git.pull_request.adopt",
		"candidate.adopt",
		"candidate.merge",
		"repository.merge",
		"main.push",
	} {
		if authorizableActions[action] {
			t.Fatalf("%q became human-authorizable: a candidate must never be able to authorize its own adoption", action)
		}
	}
}

// TestMostPermissiveConfigurationCannotAuthorizeProducerSelfEvidence is the
// second. The authority evaluator takes a contract, evidence bundles, and the
// change producer - and no configuration at all. This asserts that directly:
// the loosest operator layer the validator will accept, applied to a run whose
// only passing evidence came from the change producer itself, still does not
// reach AUTHORIZED.
func TestMostPermissiveConfigurationCannotAuthorizeProducerSelfEvidence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), strings.Replace(
		operatorConfigJSON(dir),
		`"budgets": {"wall_limit_seconds": 3600, "max_execution_attempts": 3, "max_remediation_attempts": 2, "max_assurance_attempts": 2}`,
		`"budgets": {"wall_limit_seconds": 2147483647, "max_execution_attempts": 2147483647, "max_remediation_attempts": 2147483647, "max_assurance_attempts": 2147483647}, "gc": {"retention_hours": 2147483647}, "operator": {"id": "operator-who-approves-everything"}`,
		1))
	if _, err := LoadConfig(filepath.Join(dir, "config.json"), dir); err != nil {
		t.Fatalf("the most permissive operator layer must still load, or this proves nothing: %v", err)
	}

	contract := providerAuthorityContract(t)
	bundle := providerAuthorityEvidence(t)
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
		Action: action, RequiredClaims: []string{"claim-auth-regression-tests"},
	})
	changeProducer := domain.EvidenceProducer{ID: "codex-provider-1", Type: domain.ProducerExecutionProvider}
	item := bundle.Evidence["evidence-auth-tests-passed"]
	item.Producer = changeProducer
	bundle.Evidence["evidence-auth-tests-passed"] = item

	state, err := KernelFlow{}.Decide(KernelState{Contract: contract, Evidence: map[string]domain.EvidenceBundle{bundle.ID: bundle}}, action, changeProducer)
	if err != nil {
		t.Fatal(err)
	}
	if state.Decision.Status == domain.AuthorityAuthorized {
		t.Fatalf("producer output counted as independent evidence: %#v", state.Decision)
	}
}

// TestNoConfigurationGrantsCandidateGitHubCredentials is the third. The forge
// credential is a Go-level injection selected by github.credential_mode, a
// member no repository may name; and the two environments a candidate's work
// actually runs in - the provider control plane and the assurance sandbox -
// are allowlists built from scratch, so an ambient forge token on the host
// does not reach either even when one is present.
func TestNoConfigurationGrantsCandidateGitHubCredentials(t *testing.T) {
	for _, ambient := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_API_TOKEN", "SSH_AUTH_SOCK"} {
		t.Setenv(ambient, "leaked-secret-value")
	}
	provider := NativeCodexProvider{CodexHome: t.TempDir()}
	for _, entry := range provider.env() {
		name, value, _ := strings.Cut(entry, "=")
		if name != "PATH" && name != "HOME" && name != "CODEX_HOME" {
			t.Fatalf("provider environment carries %q, which is not on the allowlist", name)
		}
		if strings.Contains(value, "leaked-secret-value") {
			t.Fatalf("an ambient credential reached the provider environment through %s", name)
		}
	}
	// The container environment is a stated allowlist. Every Go variable on it
	// is there because the toolchain has to put writable state SOMEWHERE the
	// runtime chose: leaving one unnamed is how the module cache and then the
	// build cache ended up inside a candidate workspace. All of them point
	// INSIDE the container, and none can carry a host value.
	args := dockerBase(t.TempDir(), true)
	allowed := map[string]bool{
		"HOME": true, "PATH": true, "GOTMPDIR": true, "GOCACHE": true,
		"GOPATH": true, "GOMODCACHE": true, "GOENV": true,
	}
	for i, arg := range args {
		if arg != "--env" {
			continue
		}
		name, value, _ := strings.Cut(args[i+1], "=")
		if !allowed[name] {
			t.Fatalf("assurance sandbox forwards %q, which is not on the allowlist", args[i+1])
		}
		if !strings.HasPrefix(value, "/") {
			t.Fatalf("assurance sandbox environment %q is not a container-absolute value", args[i+1])
		}
	}
	if !containsPair(args, "--network", "none") {
		t.Fatal("assurance sandbox must run with no network at all")
	}
	// And the credential seam itself is not addressable from configuration:
	// RepositoryConfig has no member for it, and the operator layer names a
	// MODE, never a secret.
	if strings.Contains(strings.Join(configMembers(reflect.TypeOf(RepositoryConfig{}), ""), " "), "credential") {
		t.Fatal("the repository layer gained a credential member")
	}
	for _, mode := range []string{GitHubCredentialCLI, GitHubCredentialNone, "token", "env", "inline"} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.json"), strings.Replace(operatorConfigJSON(dir), `"credential_mode": "github-cli"`, `"credential_mode": "`+mode+`"`, 1))
		_, _, err := LoadOperatorConfig(filepath.Join(dir, "config.json"))
		if wantOK := mode == GitHubCredentialCLI || mode == GitHubCredentialNone; wantOK != (err == nil) {
			t.Fatalf("github.credential_mode %q: err = %v, want accepted=%v", mode, err, wantOK)
		}
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestSubjectBindingCannotBeLoosenedWhileStillClaimingAuthorized is the
// fourth. Evidence is bound to an exact subject, contract revision, and
// policy; loosening any leg of that binding must never leave a normal
// AUTHORIZED state behind. Two refusals are both correct outcomes and both
// accepted here: the loosened bundle is rejected outright as invalid, or it
// decodes and the decision comes back as something other than AUTHORIZED. What
// is never acceptable is AUTHORIZED with a binding that no longer names the
// exact subject. The control case runs first, so a failure here is the binding
// rule and not a broken fixture.
func TestSubjectBindingCannotBeLoosenedWhileStillClaimingAuthorized(t *testing.T) {
	action := domain.Action{Type: "git.pull_request.create", Target: "main"}
	decide := func(t *testing.T, mutate func(*domain.EvidenceBundle)) (domain.AuthorityDecision, error) {
		t.Helper()
		contract := providerAuthorityContract(t)
		bundle := providerAuthorityEvidence(t)
		contract.AuthorityConditions = append(contract.AuthorityConditions, domain.AuthorityCondition{
			Action: action, RequiredClaims: []string{"claim-auth-regression-tests"},
		})
		mutate(&bundle)
		producer := domain.EvidenceProducer{ID: "codex-provider-1", Type: domain.ProducerExecutionProvider}
		state, err := KernelFlow{}.Decide(KernelState{Contract: contract, Evidence: map[string]domain.EvidenceBundle{bundle.ID: bundle}}, action, producer)
		return state.Decision, err
	}
	control, err := decide(t, func(*domain.EvidenceBundle) {})
	if err != nil || control.Status != domain.AuthorityAuthorized {
		t.Fatalf("the exactly-bound control must authorize, or the cases below prove nothing: %#v %v", control, err)
	}
	evaluated := 0
	for _, c := range []struct {
		leg    string
		loosen func(*domain.EvidenceBundle)
	}{
		{"subject revision", func(b *domain.EvidenceBundle) { b.Subject.Revision = strings.Repeat("0", 40) }},
		{"subject repository", func(b *domain.EvidenceBundle) { b.Subject.Repository = "attacker/other" }},
		{"subject cleared", func(b *domain.EvidenceBundle) { b.Subject = domain.Subject{} }},
		{"contract revision", func(b *domain.EvidenceBundle) { b.Contract.Revision = "not-the-contract-revision" }},
		{"contract id cleared", func(b *domain.EvidenceBundle) { b.Contract.ID = "" }},
		{"policy revision", func(b *domain.EvidenceBundle) { b.Policy.Revision = "not-the-policy-revision" }},
	} {
		decision, err := decide(t, c.loosen)
		if err != nil {
			continue // refused outright: an even stronger answer than "not authorized".
		}
		evaluated++
		if decision.Status == domain.AuthorityAuthorized {
			t.Fatalf("loosening the %s binding still reported AUTHORIZED: %#v", c.leg, decision)
		}
	}
	// At least one loosened binding must have reached the evaluator, or this
	// would pass merely because every case was rejected before it got there.
	if evaluated == 0 {
		t.Fatal("no loosened binding reached the authority evaluator; the binding rule itself was never exercised")
	}
}
