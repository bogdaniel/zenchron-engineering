package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImage = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// operatorConfigJSON is a complete, valid operator layer rooted at dir.
func operatorConfigJSON(dir string) string {
	return `{
	"state_dir": "` + filepath.Join(dir, "state") + `",
	"project_model_path": "` + filepath.Join(dir, "model.json") + `",
	"policy_path": "` + filepath.Join(dir, "policy.json") + `",
	"assurance": {"image": "` + testImage + `"},
	"provider": {"kind": "openai", "model": "gpt-5", "credential_path": "` + filepath.Join(dir, "key") + `"},
	"github": {"credential_mode": "github-cli"},
	"budgets": {"wall_limit_seconds": 3600, "max_execution_attempts": 3, "max_remediation_attempts": 2, "max_assurance_attempts": 2}
}`
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeOperatorConfig(t *testing.T, dir string) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, "config.json"), operatorConfigJSON(dir))
}

func requireConfigError(t *testing.T, err error) *ConfigError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a configuration error, got nil")
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("expected *ConfigError, got %T: %v", err, err)
	}
	return configErr
}

func TestOperatorConfigLoads(t *testing.T) {
	dir := t.TempDir()
	config, digest, err := LoadOperatorConfig(writeOperatorConfig(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider.Kind != ProviderOpenAI || config.Budgets.MaxExecutionAttempts != 3 {
		t.Fatalf("unexpected operator config: %+v", config)
	}
	if len(digest) != 64 {
		t.Fatalf("expected a sha256 digest, got %q", digest)
	}
}

func TestOperatorConfigRejectsUnknownMember(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(operatorConfigJSON(dir), `"github": {`, `"credential_token": "ghp_secret", "github": {`, 1)
	path := writeFile(t, filepath.Join(dir, "config.json"), body)
	err := requireConfigError(t, second(LoadOperatorConfig(path)))
	if !strings.Contains(err.Detail, "credential_token") {
		t.Fatalf("expected the unknown member to be named, got %q", err.Detail)
	}
}

func TestOperatorConfigRejectsDuplicateMember(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(operatorConfigJSON(dir), `"github": {`, `"policy_path": "/elsewhere", "github": {`, 1)
	path := writeFile(t, filepath.Join(dir, "config.json"), body)
	err := requireConfigError(t, second(LoadOperatorConfig(path)))
	if !strings.Contains(err.Detail, "duplicate") {
		t.Fatalf("expected a duplicate-member refusal, got %q", err.Detail)
	}
}

func TestOperatorConfigRejectsTrailingValue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config.json"), operatorConfigJSON(dir)+" {}")
	requireConfigError(t, second(LoadOperatorConfig(path)))
}

func TestMissingOperatorConfigIsTyped(t *testing.T) {
	err := requireConfigError(t, second(LoadOperatorConfig(filepath.Join(t.TempDir(), "absent.json"))))
	if !strings.Contains(err.Detail, "unreadable") {
		t.Fatalf("unexpected detail %q", err.Detail)
	}
}

func TestOperatorConfigRefusesUnpinnedAssuranceImage(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(operatorConfigJSON(dir), testImage, "golang:1.25", 1)
	path := writeFile(t, filepath.Join(dir, "config.json"), body)
	err := requireConfigError(t, second(LoadOperatorConfig(path)))
	if !strings.Contains(err.Detail, "assurance.image") {
		t.Fatalf("unexpected detail %q", err.Detail)
	}
}

// Authority. Everything below is the boundary between the two layers.

func TestRepositoryConfigCannotSupplyCredentials(t *testing.T) {
	for _, member := range []string{
		`"provider": {"kind": "openai", "model": "x", "credential_path": "/tmp/attacker-key"}`,
		`"github": {"credential_mode": "github-cli"}`,
		`"state_dir": "/tmp/attacker-state"`,
		`"assurance": {"image": "sha256:deadbeef"}`,
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), RepositoryConfigFile), "{"+member+"}")
		_, _, _, err := LoadRepositoryConfig(path)
		configErr := requireConfigError(t, err)
		if !strings.Contains(configErr.Detail, "operator authority") {
			t.Fatalf("member %s: expected an authority refusal, got %q", member, configErr.Detail)
		}
	}
}

// TestRepositoryConfigCannotChooseTheOperatorIdentity is the other half of the
// authority boundary: a repository may not name who authorized the change being
// made to it. "operator" is absent from repositoryScope, which is checked
// BEFORE the file is decoded, so the refusal is an authority violation and not
// an incidental unknown field - the value that is not even shaped like an
// OperatorIdentityConfig proves the check ran first, because a decoder would
// have complained about its type instead.
func TestRepositoryConfigCannotChooseTheOperatorIdentity(t *testing.T) {
	for _, member := range []string{
		`"operator": {"id": "attacker"}`,
		`"operator": {"id": "attacker", "require_configured_id": false}`,
		`"operator": {"require_configured_id": false}`,
		`"operator": {}`,
		`"operator": ["not-an-identity-config"]`,
		// Alongside a member this layer legitimately may name, so the refusal
		// is not an artefact of the file containing nothing else.
		`"budgets": {"wall_limit_seconds": 60}, "operator": {"id": "attacker"}`,
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), RepositoryConfigFile), "{"+member+"}")
		config, digest, present, err := LoadRepositoryConfig(path)
		configErr := requireConfigError(t, err)
		if !strings.Contains(configErr.Detail, `"operator"`) || !strings.Contains(configErr.Detail, "operator authority") {
			t.Fatalf("member %s: expected an authority refusal naming operator, got %q", member, configErr.Detail)
		}
		// A refused layer yields nothing a caller could go on to apply.
		if present || digest != "" || config.Budgets != nil || config.Watch != nil {
			t.Fatalf("member %s: a refused layer was still returned: present=%v digest=%q %+v", member, present, digest, config)
		}
	}
}

func TestRepositoryConfigRejectsUnknownMember(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), RepositoryConfigFile), `{"budgets": {"wall_limit_seconds": 10, "credential_token": "ghp_secret"}}`)
	_, _, _, err := LoadRepositoryConfig(path)
	configErr := requireConfigError(t, err)
	if !strings.Contains(configErr.Detail, "credential_token") {
		t.Fatalf("expected the unknown member to be named, got %q", configErr.Detail)
	}
}

func TestAbsentRepositoryConfigIsNotAnError(t *testing.T) {
	_, digest, present, err := LoadRepositoryConfig(filepath.Join(t.TempDir(), RepositoryConfigFile))
	if err != nil || present || digest != "" {
		t.Fatalf("expected an absent layer, got present=%v digest=%q err=%v", present, digest, err)
	}
}

func TestRepositoryConfigTightens(t *testing.T) {
	dir := t.TempDir()
	writeOperatorConfig(t, dir)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"budgets": {"wall_limit_seconds": 60, "max_execution_attempts": 1}}`)
	config, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if config.Budgets.WallLimitSeconds != 60 || config.Budgets.MaxExecutionAttempts != 1 {
		t.Fatalf("repository layer did not tighten: %+v", config.Budgets)
	}
	// Untouched bounds keep the operator value.
	if config.Budgets.MaxAssuranceAttempts != 2 {
		t.Fatalf("unnamed bound changed: %+v", config.Budgets)
	}
	if config.Digest.Repository == "" || config.Digest.Global == "" {
		t.Fatalf("both layers must be recorded: %+v", config.Digest)
	}
	if config.RunBudgets().WallLimit.Seconds() != 60 {
		t.Fatalf("unexpected run budgets: %+v", config.RunBudgets())
	}
}

func TestRepositoryConfigCannotRaiseACeiling(t *testing.T) {
	dir := t.TempDir()
	writeOperatorConfig(t, dir)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"budgets": {"wall_limit_seconds": 86400}}`)
	_, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	configErr := requireConfigError(t, err)
	if !strings.Contains(configErr.Detail, "only tighten") {
		t.Fatalf("expected a tighten-only refusal, got %q", configErr.Detail)
	}
	if configErr.Path == "" {
		t.Fatal("the refusal must name the repository file")
	}
}

func TestRepositoryConfigCannotTightenToNothing(t *testing.T) {
	dir := t.TempDir()
	writeOperatorConfig(t, dir)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"budgets": {"max_execution_attempts": 0}}`)
	requireConfigError(t, second2(LoadConfig(filepath.Join(dir, "config.json"), dir)))
}

// Digests.

func TestDigestIsStableAcrossFormatting(t *testing.T) {
	first := t.TempDir()
	writeOperatorConfig(t, first)
	original, err := LoadConfig(filepath.Join(first, "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	// Same values, different member order and whitespace, same directory.
	reordered := `{"budgets": {"max_assurance_attempts": 2, "max_execution_attempts": 3, "max_remediation_attempts": 2, "wall_limit_seconds": 3600},` +
		`"github": {"credential_mode": "github-cli"},` +
		`"provider": {"credential_path": "` + filepath.Join(first, "key") + `", "kind": "openai", "model": "gpt-5"},` +
		`"assurance": {"image": "` + testImage + `"},` +
		`"policy_path": "` + filepath.Join(first, "policy.json") + `",` +
		`"project_model_path": "` + filepath.Join(first, "model.json") + `",` +
		`"state_dir": "` + filepath.Join(first, "state") + `"}`
	path := writeFile(t, filepath.Join(first, "reordered.json"), reordered)
	again, err := LoadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if original.Digest.Global != again.Digest.Global {
		t.Fatalf("digest is not stable: %q vs %q", original.Digest.Global, again.Digest.Global)
	}
	if original.Digest.Repository != "" {
		t.Fatalf("no repository layer was read, digest must be empty: %q", original.Digest.Repository)
	}
}

func TestDigestDiffersWhenConfigDiffers(t *testing.T) {
	dir := t.TempDir()
	writeOperatorConfig(t, dir)
	base, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"budgets": {"wall_limit_seconds": 60}}`)
	tightened, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if base.Digest.Global != tightened.Digest.Global {
		t.Fatal("the operator digest must record the operator layer as written")
	}
	if base.Digest.Repository == tightened.Digest.Repository {
		t.Fatal("adding a repository layer must change the repository digest")
	}

	other := t.TempDir()
	writeFile(t, filepath.Join(other, "config.json"), strings.Replace(operatorConfigJSON(other), `"max_execution_attempts": 3`, `"max_execution_attempts": 4`, 1))
	changed, err := LoadConfig(filepath.Join(other, "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest.Global == base.Digest.Global {
		t.Fatal("a different operator config must digest differently")
	}
}

// TestDigestCoversTheOperatorIdentity proves the recorded configuration digest
// covers WHO a run is recorded as authorized by, not only the bounds it runs
// under. A digest that did not move when the operator identity moved would let
// two runs claim the same governing configuration while having been authorized
// against different identities.
func TestDigestCoversTheOperatorIdentity(t *testing.T) {
	dir := t.TempDir()
	// Every layer below is written into the same directory, so the paths inside
	// them are identical and the operator member is the only thing that varies.
	digestOf := func(name, operator string) string {
		t.Helper()
		body := operatorConfigJSON(dir)
		if operator != "" {
			body = strings.Replace(body, `"github": {`, operator+`, "github": {`, 1)
		}
		config, err := LoadConfig(writeFile(t, filepath.Join(dir, name), body), "")
		if err != nil {
			t.Fatal(err)
		}
		return config.Digest.Global
	}
	seen := map[string]string{}
	for _, layer := range []struct{ name, operator string }{
		{"absent.json", ""},
		{"identity-a.json", `"operator": {"id": "operator-a"}`},
		{"identity-b.json", `"operator": {"id": "operator-b"}`},
		{"identity-a-required.json", `"operator": {"id": "operator-a", "require_configured_id": true}`},
	} {
		digest := digestOf(layer.name, layer.operator)
		if len(digest) != 64 {
			t.Fatalf("%s: expected a sha256 digest, got %q", layer.name, digest)
		}
		if previous, collided := seen[digest]; collided {
			t.Fatalf("%s digests identically to %s: the operator identity is not covered", layer.name, previous)
		}
		seen[digest] = layer.name
		// Stability: the same identity, reached through a different file with
		// different formatting, digests identically.
		restated := strings.Replace(layer.operator, `"operator": {`, `"operator": {  `, 1)
		if again := digestOf("restated-"+layer.name, restated); again != digest {
			t.Fatalf("%s: digest is not stable across formatting: %q vs %q", layer.name, digest, again)
		}
	}
}

func TestOperatorConfigPathPrefersExplicitThenEnv(t *testing.T) {
	t.Setenv(OperatorConfigEnv, "/from/env.json")
	explicit, err := OperatorConfigPath("/explicit.json")
	if err != nil || explicit != "/explicit.json" {
		t.Fatalf("explicit path must win: %q %v", explicit, err)
	}
	fromEnv, err := OperatorConfigPath("")
	if err != nil || fromEnv != "/from/env.json" {
		t.Fatalf("environment must be used next: %q %v", fromEnv, err)
	}
	t.Setenv(OperatorConfigEnv, "")
	fallback, err := OperatorConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(fallback) != "config.json" || !strings.Contains(fallback, "zenchron") {
		t.Fatalf("unexpected default path %q", fallback)
	}
}

func TestLoadEngineeringPolicyIsStrict(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadEngineeringPolicy(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("expected a read failure")
	}
	path := writeFile(t, filepath.Join(dir, "policy.json"), `{"schema_version": "0.1", "id": "p", "revision": "1", "rules": {}, "unknown": 1}`)
	if _, err := LoadEngineeringPolicy(path); err == nil {
		t.Fatal("expected a strict decode failure")
	}
	valid, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", "security-sensitive.engineering-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := LoadEngineeringPolicy(writeFile(t, filepath.Join(dir, "valid.json"), string(valid)))
	if err != nil {
		t.Fatal(err)
	}
	if policy.ID == "" {
		t.Fatal("expected a decoded policy")
	}
}

// Watch. Enrolment is operator-global: the whole point of the tests below is
// that a repository cannot put itself under automation, nor speed it up.

// operatorConfigWithWatch writes an operator layer carrying a watch member.
func operatorConfigWithWatch(t *testing.T, dir, watch string) string {
	t.Helper()
	body := strings.Replace(operatorConfigJSON(dir), `"github": {`, `"watch": `+watch+`, "github": {`, 1)
	return writeFile(t, filepath.Join(dir, "config.json"), body)
}

func TestWatchDefaultsAreLabelAndOneRun(t *testing.T) {
	dir := t.TempDir()
	path := operatorConfigWithWatch(t, dir, `{"repositories": ["bogdaniel/zenchron-engineering", "zenchron-dynamics/zenchron-foundry"]}`)
	config, _, err := LoadOperatorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Label != "zenchron:auto" {
		t.Fatalf("unexpected default opt-in label %q", settings.Label)
	}
	if settings.MaxConcurrentRuns != 1 {
		t.Fatalf("watch must default to one concurrent run, got %d", settings.MaxConcurrentRuns)
	}
	if settings.PollInterval != DefaultWatchPollSeconds*time.Second {
		t.Fatalf("unexpected default poll interval %s", settings.PollInterval)
	}
	if len(settings.Repositories) != 2 || settings.Repositories[0].Owner != "bogdaniel" || settings.Repositories[1].Name != "zenchron-foundry" {
		t.Fatalf("unexpected enrolled repositories: %+v", settings.Repositories)
	}
	// An empty enrolment is legal and watches nothing; it is not a crawler.
	bare := t.TempDir()
	writeOperatorConfig(t, bare)
	none, _, err := LoadOperatorConfig(filepath.Join(bare, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if settings, err := none.WatchSettings(); err != nil || len(settings.Repositories) != 0 {
		t.Fatalf("an unconfigured watch must observe nothing: %+v %v", settings.Repositories, err)
	}
}

func TestRepositoryConfigCannotEnrolItselfIntoWatch(t *testing.T) {
	for _, member := range []string{
		`{"watch": {"repositories": ["attacker/self"]}}`,
		`{"watch": {"repositories": []}}`,
		`{"watch": {"label": "zenchron:auto"}}`,
		`{"watch": {"poll_interval_seconds": 600, "repositories": ["attacker/self"]}}`,
	} {
		path := writeFile(t, filepath.Join(t.TempDir(), RepositoryConfigFile), member)
		_, _, _, err := LoadRepositoryConfig(path)
		configErr := requireConfigError(t, err)
		if !strings.Contains(configErr.Detail, "operator authority") || !strings.Contains(configErr.Detail, "watch.") {
			t.Fatalf("%s: expected a watch authority refusal, got %q", member, configErr.Detail)
		}
	}
}

func TestRepositoryConfigCannotRaiseWatchFrequencyOrConcurrency(t *testing.T) {
	operatorWatch := `{"repositories": ["owner/name"], "poll_interval_seconds": 120, "max_concurrent_runs": 2}`
	for _, c := range []struct{ repository, wants string }{
		{`{"watch": {"poll_interval_seconds": 60}}`, "watch.poll_interval_seconds"},
		{`{"watch": {"max_concurrent_runs": 3}}`, "watch.max_concurrent_runs"},
	} {
		dir := t.TempDir()
		path := operatorConfigWithWatch(t, dir, operatorWatch)
		writeFile(t, filepath.Join(dir, RepositoryConfigFile), c.repository)
		configErr := requireConfigError(t, second2(LoadConfig(path, dir)))
		if !strings.Contains(configErr.Detail, "only tighten") || !strings.Contains(configErr.Detail, c.wants) {
			t.Fatalf("%s: expected a tighten-only refusal naming %s, got %q", c.repository, c.wants, configErr.Detail)
		}
		if configErr.Path == "" {
			t.Fatal("the refusal must name the repository file")
		}
	}
	// Loosening for itself - polled less often, fewer runs - is allowed.
	dir := t.TempDir()
	path := operatorConfigWithWatch(t, dir, operatorWatch)
	writeFile(t, filepath.Join(dir, RepositoryConfigFile), `{"watch": {"poll_interval_seconds": 900, "max_concurrent_runs": 1}}`)
	config, err := LoadConfig(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.PollInterval != 900*time.Second || settings.MaxConcurrentRuns != 1 {
		t.Fatalf("repository layer did not tighten watch: %+v", settings)
	}
	// Enrolment is untouched by the repository layer either way.
	if len(settings.Repositories) != 1 || settings.Repositories[0].String() != "owner/name" {
		t.Fatalf("enrolment changed: %+v", settings.Repositories)
	}
}

func TestWatchPollingBelowTheSafeLowerBoundIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := operatorConfigWithWatch(t, dir, `{"repositories": ["owner/name"], "poll_interval_seconds": 5}`)
	configErr := requireConfigError(t, second(LoadOperatorConfig(path)))
	if !strings.Contains(configErr.Detail, "poll_interval_seconds") || !strings.Contains(configErr.Detail, "30") {
		t.Fatalf("expected the lower bound to be named, got %q", configErr.Detail)
	}
	// The same floor is unreachable from the repository layer, whose proposal
	// must already be no more frequent than the operator's interval.
	tight := t.TempDir()
	tightPath := operatorConfigWithWatch(t, tight, `{"repositories": ["owner/name"], "poll_interval_seconds": 30}`)
	writeFile(t, filepath.Join(tight, RepositoryConfigFile), `{"watch": {"poll_interval_seconds": 5}}`)
	requireConfigError(t, second2(LoadConfig(tightPath, tight)))
}

func TestWatchRepositoriesMustBeValidAndUnique(t *testing.T) {
	for _, repositories := range []string{
		`["not-a-repo"]`,
		`["owner/"]`,
		`["/name"]`,
		`["owner/name/extra"]`,
		`["https://github.com/owner/name"]`,
		`["git@github.com:owner/name.git"]`,
		`["owner/name", "owner/name"]`,
		`["owner/name", "Owner/Name"]`,
	} {
		dir := t.TempDir()
		path := operatorConfigWithWatch(t, dir, `{"repositories": `+repositories+`}`)
		configErr := requireConfigError(t, second(LoadOperatorConfig(path)))
		if !strings.Contains(configErr.Detail, "watch.repositories") {
			t.Fatalf("%s: expected a watch.repositories refusal, got %q", repositories, configErr.Detail)
		}
	}
}

func TestWatchConfigIsPartOfTheOperatorDigest(t *testing.T) {
	dir := t.TempDir()
	watched, _, err := LoadOperatorConfig(operatorConfigWithWatch(t, dir, `{"repositories": ["owner/name"], "poll_interval_seconds": 60}`))
	if err != nil {
		t.Fatal(err)
	}
	// The same watch configuration written differently digests identically:
	// only the values decide, never the formatting.
	reordered := strings.Replace(operatorConfigJSON(dir), `"github": {`, `"watch": {"poll_interval_seconds":60,"repositories":["owner/name"]}, "github": {`, 1)
	stable, _, err := LoadOperatorConfig(writeFile(t, filepath.Join(dir, "reordered.json"), reordered))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := Digest(watched)
	if err != nil {
		t.Fatal(err)
	}
	stableDigest, err := Digest(stable)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != stableDigest {
		t.Fatalf("watch digest is not stable: %q vs %q", firstDigest, stableDigest)
	}
	// A changed watch configuration must digest differently: a run records
	// exactly which enrolment governed it.
	other, _, err := LoadOperatorConfig(operatorConfigWithWatch(t, dir, `{"repositories": ["owner/name"], "poll_interval_seconds": 60, "label": "zenchron:manual"}`))
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := Digest(other)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("changing watch configuration must change the operator digest")
	}
}

// second and second2 drop the leading return values so a multi-value call can be
// asserted on its error alone.
func second[A any](_ A, _ string, err error) error { return err }
func second2[A any](_ A, err error) error          { return err }
