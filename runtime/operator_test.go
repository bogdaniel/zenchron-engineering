package runtime

import (
	"errors"
	"strings"
	"testing"
)

// TestResolveOperatorPrefersTheStatedIdentityOverTheObservedOne proves the
// ordering operator.go states: an identity an operator actually stated wins
// over one the runtime merely observed, and the observed account is still
// recorded BESIDE it rather than replaced by it, because the two are different
// evidence and a durable record that merged them could not be taken apart
// later. Every accepted resolution carries ProvenanceLocalUnverified, including
// the ones where an operator stated the identity: stating an identity is not
// proving one.
func TestResolveOperatorPrefersTheStatedIdentityOverTheObservedOne(t *testing.T) {
	for _, resolved := range []struct {
		name        string
		configured  OperatorIdentityConfig
		account     string
		wantID      string
		wantAccount string
	}{
		{"a configured identity wins over the observed account",
			OperatorIdentityConfig{ID: "operator-a"}, "local-account", "operator-a", "local-account"},
		{"a configured identity satisfies the policy that requires one",
			OperatorIdentityConfig{ID: "operator-a", RequireConfiguredID: true}, "local-account", "operator-a", "local-account"},
		{"a configured identity stands alone when no account is observed",
			OperatorIdentityConfig{ID: "operator-a"}, "", "operator-a", ""},
		// The fallback. It is recorded as the identity because it is the best
		// evidence available, and the assertion below is that being the best
		// available evidence does not make it verified evidence.
		{"the observed account is the identity of last resort",
			OperatorIdentityConfig{}, "local-account", "local-account", "local-account"},
		{"surrounding whitespace is not part of an identity",
			OperatorIdentityConfig{ID: "  operator-a  "}, "  local-account  ", "operator-a", "local-account"},
	} {
		t.Run(resolved.name, func(t *testing.T) {
			operator, err := resolveOperator(resolved.configured, resolved.account)
			if err != nil {
				t.Fatalf("a resolvable identity was refused: %v", err)
			}
			if operator.ID != resolved.wantID {
				t.Fatalf("recorded identity %q, want %q", operator.ID, resolved.wantID)
			}
			if operator.AccountName != resolved.wantAccount {
				t.Fatalf("recorded account %q, want %q", operator.AccountName, resolved.wantAccount)
			}
			// The point of the fallback case: an identity taken from the local
			// account database is recorded, never promoted. There is exactly one
			// provenance this milestone can write, and a fallback gets the same
			// one a configured identity gets - unverified.
			if operator.Provenance != ProvenanceLocalUnverified {
				t.Fatalf("recorded provenance %q, want %q", operator.Provenance, ProvenanceLocalUnverified)
			}
			if operator.Host == "" {
				t.Fatal("a recorded operator must name the host the action came from")
			}
		})
	}
}

// TestResolveOperatorFailsClosedInsteadOfRecordingAnAnonymousIdentity proves
// the two no-identity paths are refusals and not empty successes. The assertion
// that no identity comes back is the load-bearing half: an error a caller
// ignores must not leave a RecordedOperator that could still be journalled as
// if somebody had authorized something.
func TestResolveOperatorFailsClosedInsteadOfRecordingAnAnonymousIdentity(t *testing.T) {
	for _, refused := range []struct {
		name       string
		configured OperatorIdentityConfig
		account    string
	}{
		{"policy requires a configured identity and none is set",
			OperatorIdentityConfig{RequireConfiguredID: true}, "local-account"},
		// Whitespace is not an identity, so it cannot be used to satisfy the
		// policy that refuses the local account name as a substitute.
		{"policy requires a configured identity and only whitespace is set",
			OperatorIdentityConfig{ID: "   ", RequireConfiguredID: true}, "local-account"},
		{"policy requires a configured identity and no account is resolvable either",
			OperatorIdentityConfig{RequireConfiguredID: true}, ""},
		{"nothing is configured and no local account is resolvable",
			OperatorIdentityConfig{}, ""},
		{"nothing is configured and the observed account is whitespace",
			OperatorIdentityConfig{ID: "  "}, "  "},
	} {
		t.Run(refused.name, func(t *testing.T) {
			operator, err := resolveOperator(refused.configured, refused.account)
			var identityErr *OperatorIdentityError
			if !errors.As(err, &identityErr) {
				t.Fatalf("expected *OperatorIdentityError, got %T: %v", err, err)
			}
			if operator != (RecordedOperator{}) {
				t.Fatalf("a refused resolution still handed back an identity: %+v", operator)
			}
			// The refusal names the member that fixes it, because an operator
			// who cannot act on it will reach for whatever does not fail.
			if !strings.Contains(identityErr.Error(), "operator.id") {
				t.Fatalf("the refusal must name operator.id, got %q", identityErr.Error())
			}
		})
	}
}

// TestResolveOperatorNeverReturnsAnIdentityWithoutProvenance sweeps every
// combination of the resolver's inputs and asserts the single invariant that
// has to hold across all of them: an identity comes back WITH a provenance, or
// it does not come back at all. A zero-valued provenance is therefore not
// constructible through the resolver, which is what stops a later milestone
// that can verify a person from finding M0 records that never said how sure
// they were.
func TestResolveOperatorNeverReturnsAnIdentityWithoutProvenance(t *testing.T) {
	for _, id := range []string{"", "   ", "operator-a"} {
		for _, require := range []bool{false, true} {
			for _, account := range []string{"", "   ", "local-account"} {
				configured := OperatorIdentityConfig{ID: id, RequireConfiguredID: require}
				operator, err := resolveOperator(configured, account)
				if err != nil {
					if operator != (RecordedOperator{}) {
						t.Fatalf("%+v account %q: a refusal returned %+v", configured, account, operator)
					}
					continue
				}
				if operator.ID == "" {
					t.Fatalf("%+v account %q: an anonymous identity was resolved: %+v", configured, account, operator)
				}
				if operator.Provenance != ProvenanceLocalUnverified {
					t.Fatalf("%+v account %q: provenance %q, want %q", configured, account, operator.Provenance, ProvenanceLocalUnverified)
				}
			}
		}
	}
}

// TestOperatorConfigResolvesFromTheOperatorLayer proves the exported entry
// point reads the identity from the operator layer - the layer a repository
// cannot write, see TestRepositoryConfigCannotChooseTheOperatorIdentity - and
// records the observed account and host alongside it as separate evidence.
func TestOperatorConfigResolvesFromTheOperatorLayer(t *testing.T) {
	config := OperatorConfig{Operator: OperatorIdentityConfig{ID: "operator-a", RequireConfiguredID: true}}
	operator, err := config.ResolveOperator()
	if err != nil {
		t.Fatalf("a configured identity was refused: %v", err)
	}
	if operator.ID != "operator-a" {
		t.Fatalf("recorded identity %q, want the operator layer's", operator.ID)
	}
	if operator.Provenance != ProvenanceLocalUnverified {
		t.Fatalf("recorded provenance %q, want %q", operator.Provenance, ProvenanceLocalUnverified)
	}
	// The observed account is evidence about where the action ran, so it is
	// recorded even when it is not the identity.
	if operator.AccountName != strings.TrimSpace(localAccountName()) {
		t.Fatalf("recorded account %q, want the observed local account %q", operator.AccountName, localAccountName())
	}
	if operator.Host != ownerHost() {
		t.Fatalf("recorded host %q, want %q", operator.Host, ownerHost())
	}
}
