package runtime

// Operator identity in M0 is PROVENANCE, not authentication.
//
// This file records which identity the operator layer configured and which
// local OS account and host the runtime process actually ran as. That is
// evidence about where a local action came from. It is NOT proof of who a
// person is: a local account name is not a credential, nothing here is signed,
// and no challenge was issued to anybody. The types are named so the two can
// never be conflated in a durable record - every RecordedOperator carries an
// OperatorProvenance, and M0 emits exactly one value, ProvenanceLocalUnverified.
//
// Repository configuration cannot choose the operator identity. The identity is
// an OperatorConfig member, and repositoryScope in config.go is an allowlist of
// the top-level members an in-repo file may name, so a repository naming
// "operator" is refused as an authority violation before it is decoded.
//
// There is deliberately no role, no permission matrix, and no auth provider: a
// local runtime that cannot verify a person has nothing to attach them to.

import (
	"os/user"
	"strings"
)

// OperatorProvenance states HOW a recorded operator identity was established.
// It is a required member of every RecordedOperator so that a later milestone
// which can actually verify a person adds a NEW value rather than silently
// reinterpreting the records M0 already wrote.
type OperatorProvenance string

// ProvenanceLocalUnverified is the only value M0 records: a configured identity
// string, plus the local account and host the runtime observed. None of it was
// authenticated, and no reader may treat it as a verified person identity.
const ProvenanceLocalUnverified OperatorProvenance = "local_unverified"

// RecordedOperator is recorded operator provenance for one local action. It is
// deliberately not called an authenticated user: ID is whatever the operator
// layer configured or, failing that, the local account name, and neither was
// proven to belong to a person.
type RecordedOperator struct {
	ID          string             `json:"id"`
	AccountName string             `json:"account_name,omitempty"`
	Host        string             `json:"host,omitempty"`
	Provenance  OperatorProvenance `json:"provenance"`
}

// OperatorIdentityConfig is the operator layer's identity member. It names an
// identity, never a secret, and it lives here rather than in RepositoryConfig
// because a repository the runtime is changing may not choose who is recorded
// as having authorized the change.
type OperatorIdentityConfig struct {
	// ID is the operator-configured identity. It wins over every observed
	// value, because it is the only one an operator actually stated.
	ID string `json:"id,omitempty"`
	// RequireConfiguredID is the fail-closed switch for a deployment whose
	// policy needs a meaningful human identity: with it set, the local account
	// name is refused as a substitute rather than quietly standing in for one.
	RequireConfiguredID bool `json:"require_configured_id,omitempty"`
}

// OperatorIdentityError is the typed fail-closed result of an operator identity
// that is required but neither configured nor resolvable. It exists so callers
// branch on a type instead of testing an identity string for emptiness: an
// empty identity must never flow onward as if it were an identity.
type OperatorIdentityError struct{ Detail string }

func (e *OperatorIdentityError) Error() string { return "operator identity: " + e.Detail }

// ResolveOperator resolves this process's recorded operator provenance from the
// operator layer plus the local account and host. It fails closed: there is no
// path that returns an empty identity and no error.
func (c OperatorConfig) ResolveOperator() (RecordedOperator, error) {
	return resolveOperator(c.Operator, localAccountName())
}

// resolveOperator takes the observed account name as an argument so both
// fail-closed branches - nothing configured and nothing observed, and a policy
// that refuses the observed value - are reachable in a test without touching
// the host's account database.
func resolveOperator(configured OperatorIdentityConfig, account string) (RecordedOperator, error) {
	operator := RecordedOperator{
		ID:          strings.TrimSpace(configured.ID),
		AccountName: strings.TrimSpace(account),
		Host:        ownerHost(),
		Provenance:  ProvenanceLocalUnverified,
	}
	if operator.ID == "" && configured.RequireConfiguredID {
		return RecordedOperator{}, &OperatorIdentityError{Detail: "policy requires a configured operator identity; the local account name is not one. Set operator.id in the operator configuration"}
	}
	if operator.ID == "" {
		// Provenance of last resort. It is recorded as the identity because it
		// is the best evidence available, and it is still marked unverified.
		operator.ID = operator.AccountName
	}
	if operator.ID == "" {
		return RecordedOperator{}, &OperatorIdentityError{Detail: "no operator identity is configured and no local account name is resolvable; set operator.id in the operator configuration"}
	}
	return operator, nil
}

// localAccountName is the local OS account the process runs as. An account that
// cannot be resolved yields "", which resolveOperator turns into a typed
// failure rather than an anonymous identity.
func localAccountName() string {
	account, err := user.Current()
	if err != nil {
		return ""
	}
	return account.Username
}
