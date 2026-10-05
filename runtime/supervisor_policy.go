package runtime

// Supervisor operating policy and the controller's configuration identity
// (ADR-0003 B4, #346).
//
// The supervisor's own operating bounds - how many runs it drives at once, how
// many it lets observe, how often it looks again - are category S: they govern
// the long-running process and never the content or authority of a run. They
// therefore stop deciding the controller's configuration identity. They are
// frozen per supervisor start into a SupervisorPolicy, recorded durably with
// its digest, and changing one takes effect at the next start without parking,
// re-identifying or rewriting any run.
//
// THE IDENTITY IS A STABLE TOKEN, NOT A RECOMPUTED DIGEST. A controller's
// configuration identity (ControllerBinding.Config, and with it every run id,
// PlanID and ControllerSHA256) used to be the digest of the whole configuration
// file. Re-deriving it from only the C fields would change it - and the
// controller that is running today hands off to its successor by naming the
// successor's binding with ITS OWN digest of the configuration and refusing a
// successor that announces anything else. That predecessor cannot be changed
// after the fact, so the first controller that knows about S must bind with
// exactly the token the governing authority already holds.
//
// So the token stays what the governing authority names, and is kept only on
// proof that the configuration's controller-effective (C-only) content is the
// content that token was established for:
//
//  1. the authority names this configuration's C-only digest;
//  2. the authority names this configuration's legacy whole-file digest - the
//     same values, before S left the digest (the first B4 start);
//  3. a supervisor-start record maps the authority's token to a C-only digest
//     equal to this configuration's - an S-only edit since then.
//
// Anything else is a genuine controller-effective change: the token becomes
// the C-only digest, it does not match, and the existing #307 boundary refuses
// exactly as it always has. With no governing authority the token is the
// legacy digest - exactly what it was before this change.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// controllerEffectiveOperator is the operator layer with every member B4
// moved to category S cleared, so it no longer contributes to identity.
func controllerEffectiveOperator(o OperatorConfig) OperatorConfig {
	o.Supervisor = SupervisorConfig{}
	o.Watch.PollIntervalSeconds, o.Watch.MaxConcurrentRuns, o.Watch.MaxConcurrentObservations = 0, 0, 0
	return o
}

// controllerEffectiveRepository is the repository layer without its watch
// members, all of which are category S.
func controllerEffectiveRepository(r RepositoryConfig) RepositoryConfig {
	r.Watch = nil
	return r
}

// SupervisorPolicy is the S configuration one supervisor start runs under, as
// stated in the operator and repository layers. Its digest is what says later
// which policy governed that supervisor generation.
type SupervisorPolicy struct {
	Supervisor      SupervisorConfig `json:"supervisor"`
	Watch           WatchPolicy      `json:"watch"`
	RepositoryWatch *RepositoryWatch `json:"repository_watch,omitempty"`
}

// WatchPolicy is the S part of the operator's watch section.
type WatchPolicy struct {
	PollIntervalSeconds       int `json:"poll_interval_seconds,omitempty"`
	MaxConcurrentRuns         int `json:"max_concurrent_runs,omitempty"`
	MaxConcurrentObservations int `json:"max_concurrent_observations,omitempty"`
}

func supervisorPolicyOf(o OperatorConfig, r *RepositoryConfig) SupervisorPolicy {
	policy := SupervisorPolicy{Supervisor: o.Supervisor, Watch: WatchPolicy{
		PollIntervalSeconds: o.Watch.PollIntervalSeconds, MaxConcurrentRuns: o.Watch.MaxConcurrentRuns,
		MaxConcurrentObservations: o.Watch.MaxConcurrentObservations,
	}}
	if r != nil {
		policy.RepositoryWatch = r.Watch
	}
	return policy
}

// SupervisorStart is the durable record of one supervisor start: the
// configuration identity it served under, that identity's C-only content, and
// the S policy it applied.
type SupervisorStart struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	StartedAt     time.Time        `json:"started_at"`
	Config        ConfigDigest     `json:"config"`
	Effective     ConfigDigest     `json:"effective"`
	PolicyDigest  string           `json:"policy_digest"`
	Policy        SupervisorPolicy `json:"policy"`
}

// SupervisorStartSchemaVersion versions the record.
const SupervisorStartSchemaVersion = "0.1"

// ResolveConfigIdentity sets config.Digest to the configuration identity this
// process binds as, under the rules at the top of this file. It reads and
// never writes.
func ResolveConfigIdentity(store *SQLiteOperationStore, config Config) (Config, error) {
	authority, found, err := store.CurrentControllerAuthority()
	if err != nil || !found {
		return config, err
	}
	governing := authority.Binding.Config
	switch {
	case governing == config.Effective, governing == config.Digest:
		config.Digest = governing
		return config, nil
	}
	projected, err := store.ConfigProjected(governing, config.Effective)
	if err != nil {
		return config, err
	}
	if projected {
		config.Digest = governing
		return config, nil
	}
	config.Digest = config.Effective
	return config, nil
}

// RecordSupervisorStart durably records this start. It refuses a config
// identity that is not the governing authority's: a projection record is
// evidence that a token is THIS content, and only a process the authority
// governs may write it.
func RecordSupervisorStart(store *SQLiteOperationStore, config Config, now time.Time) (SupervisorStart, error) {
	authority, found, err := store.CurrentControllerAuthority()
	if err != nil {
		return SupervisorStart{}, err
	}
	if found && authority.Binding.Config != config.Digest {
		return SupervisorStart{}, fmt.Errorf("this process binds as configuration %s and the governing authority as %s; a supervisor start is recorded only under the governing configuration",
			short12(config.Digest.Global), short12(authority.Binding.Config.Global))
	}
	policyDigest, err := Digest(config.Policy)
	if err != nil {
		return SupervisorStart{}, err
	}
	start := SupervisorStart{
		SchemaVersion: SupervisorStartSchemaVersion, StartedAt: now,
		Config: config.Digest, Effective: config.Effective, PolicyDigest: policyDigest, Policy: config.Policy,
	}
	if start.ID, err = Digest(struct {
		Config    ConfigDigest `json:"config"`
		Effective ConfigDigest `json:"effective"`
		Policy    string       `json:"policy"`
		At        time.Time    `json:"at"`
	}{start.Config, start.Effective, policyDigest, now}); err != nil {
		return SupervisorStart{}, err
	}
	document, err := CanonicalJSON(start)
	if err != nil {
		return SupervisorStart{}, err
	}
	_, err = store.db.Exec(`INSERT INTO supervisor_starts (id, started_unix_nano, config_global, config_repository,
		effective_global, effective_repository, policy_digest, document) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		start.ID, now.UnixNano(), start.Config.Global, start.Config.Repository,
		start.Effective.Global, start.Effective.Repository, policyDigest, string(document))
	return start, err
}

// ConfigProjected reports whether a supervisor start recorded that the
// identity token names exactly this C-only content.
func (s *SQLiteOperationStore) ConfigProjected(token, effective ConfigDigest) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM supervisor_starts
		WHERE config_global = ? AND config_repository = ? AND effective_global = ? AND effective_repository = ? LIMIT 1`,
		token.Global, token.Repository, effective.Global, effective.Repository).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LatestSupervisorStart is the most recent supervisor start, for status.
func (s *SQLiteOperationStore) LatestSupervisorStart() (SupervisorStart, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM supervisor_starts ORDER BY started_unix_nano DESC, id DESC LIMIT 1`).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return SupervisorStart{}, false, nil
	}
	if err != nil {
		return SupervisorStart{}, false, err
	}
	var start SupervisorStart
	if err := strictJSON([]byte(document), &start); err != nil {
		return SupervisorStart{}, false, fmt.Errorf("stored supervisor start is unreadable: %w", err)
	}
	return start, true, nil
}
