package runtime

// These tests are about ROLE CONFUSION and nothing else. The governance seam
// exists because one identity can observe the adoption trust root and a
// different identity must publish, and the whole value of the separation is
// that neither identity can be made to do the other's job. So the assertions
// below are mostly type assertions: they fail if a future edit gives a
// governance value a publication method, or gives a publication value a
// governance method, which is the only way the separation can be lost.

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func governedRepo() GitHubRepo { return GitHubRepo{Owner: "bogdaniel", Name: "zenchron-engineering"} }

func testGovernanceCredential(secret string) GovernanceCredential {
	return governanceCredential{
		method:  "test",
		detail:  "a governance credential a test controls",
		resolve: func(RemoteIdentity) (string, error) { return secret, nil },
	}
}

// TestGovernanceCredentialCannotPublish is the first half of the invariant: a
// governance credential must never be usable for forge publication. It is
// enforced by the method set, so this is the test that notices if somebody
// gives governanceCredential a Credential method "for symmetry".
func TestGovernanceCredentialCannotPublish(t *testing.T) {
	for name, value := range map[string]any{
		"the github-cli governance credential": GitHubCLIGovernanceCredential(),
		"a test governance credential":         testGovernanceCredential("s"),
		"the governance observer":              GitHubGovernanceObserver{},
	} {
		t.Run(name+" is not a credential provider", func(t *testing.T) {
			if _, ok := value.(CredentialProvider); ok {
				t.Fatal("a governance value satisfies CredentialProvider, so it can authorize a git push and every REST write the adapter makes")
			}
		})
	}
	// The observer is the only holder of a governance credential, so it is the
	// one place a publication capability could be smuggled in. It must not
	// answer any forge question except the governance one.
	var observer any = GitHubGovernanceObserver{}
	if _, ok := observer.(GitHubAdapter); ok {
		t.Fatal("the governance observer satisfies GitHubAdapter, so it can create pull requests and comment")
	}
	if _, ok := observer.(ForgeConversation); ok {
		t.Fatal("the governance observer can read conversations, which is not a governance fact")
	}
	if _, ok := observer.(ForgeViewer); ok {
		t.Fatal("the governance observer claims a publication identity")
	}
	if _, ok := observer.(ForgeAppIdentity); ok {
		t.Fatal("the governance observer claims the App's publication identity")
	}
}

// TestPublicationCredentialsCannotObserveGovernance is the second half: the
// publication identities stay unable to answer a governance question, so the
// absence of a governance fact can never be mistaken for the fact. The
// publication adapter having no Rulesets method is the enforcement; this
// records it.
func TestPublicationCredentialsCannotObserveGovernance(t *testing.T) {
	for name, value := range map[string]any{
		"the REST publication adapter": GitHubRESTAdapter{},
		"the github-cli credential":    GitHubCLICredential{},
		"the token-file credential":    GitHubTokenFileCredential{},
		"the GitHub App credential":    &GitHubAppCredential{},
	} {
		t.Run(name+" observes no governance", func(t *testing.T) {
			if _, ok := value.(ForgeGovernance); ok {
				t.Fatal("a publication value satisfies ForgeGovernance, so a governance fact could be read through the credential that publishes")
			}
			if _, ok := value.(GovernanceCredential); ok {
				t.Fatal("a publication credential satisfies GovernanceCredential, so it could be selected as the governance identity")
			}
			if _, ok := value.(interface {
				Rulesets(context.Context, GitHubRepo) ([]TrustedMainRuleset, error)
			}); ok {
				t.Fatal("a publication value can read rulesets; #219 is the proof that the credential which publishes cannot see bypass_actors, so this observation would be silently incomplete")
			}
		})
	}
}

// TestGovernanceObserverOnlyEverReads is the read-only half, asserted against
// the wire rather than against a comment: every request the observer makes is
// a GET with no body, and it carries the governance secret rather than any
// other.
func TestGovernanceObserverOnlyEverReads(t *testing.T) {
	doer := &fakeGitHubDoer{responses: map[string]string{
		"GET /repos/bogdaniel/zenchron-engineering/rulesets":          `[{"id":22043609,"name":"trusted-main-adoption"}]`,
		"GET /repos/bogdaniel/zenchron-engineering/rulesets/22043609": `{"id":22043609,"name":"trusted-main-adoption","enforcement":"active","target":"branch","bypass_actors":[]}`,
	}}
	observer := GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("governance-secret")}
	if _, err := observer.Rulesets(context.Background(), governedRepo()); err != nil {
		t.Fatal(err)
	}
	if len(doer.requests) != 2 {
		t.Fatalf("expected the listing and one ruleset read, got %d requests", len(doer.requests))
	}
	for _, request := range doer.requests {
		if request.Method != http.MethodGet {
			t.Fatalf("the governance observer issued a %s; it has no write surface", request.Method)
		}
		if request.Body != "" {
			t.Fatalf("the governance observer sent a request body: %q", request.Body)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer governance-secret" {
			t.Fatalf("the governance observer authorized with %q, not the governance credential", got)
		}
	}
}

// TestGovernanceObserverWithoutACredentialFailsClosed: no credential is
// github_auth_required, never an anonymous read, and never a request at all.
func TestGovernanceObserverWithoutACredentialFailsClosed(t *testing.T) {
	doer := &fakeGitHubDoer{}
	observer := GitHubGovernanceObserver{HTTP: doer}
	_, err := observer.Rulesets(context.Background(), governedRepo())
	var authErr *GitHubAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected a typed auth refusal, got %v", err)
	}
	if len(doer.requests) != 0 {
		t.Fatal("an unauthorized governance observer still reached the network")
	}
	if provenance := observer.GovernanceProvenance(); provenance.Role != "" {
		t.Fatalf("an unauthorized observer claimed provenance %+v", provenance)
	}
}

// TestGovernanceObserverKeepsAnUndisclosedBypassUndisclosed replays the exact
// two payloads #219 recorded: what a GitHub App installation token is served,
// and what an identity that can see the field is served. The two must not
// normalize to the same observation.
func TestGovernanceObserverKeepsAnUndisclosedBypassUndisclosed(t *testing.T) {
	const appShaped = `{"id":22043609,"name":"trusted-main-adoption","enforcement":"active","target":"branch","current_user_can_bypass":"never"}`
	const userShaped = `{"id":22043609,"name":"trusted-main-adoption","enforcement":"active","target":"branch","bypass_actors":[]}`
	const withActor = `{"id":22043609,"name":"trusted-main-adoption","enforcement":"active","target":"branch","bypass_actors":[{"actor_id":1,"actor_type":"OrganizationAdmin"}]}`

	for name, testCase := range map[string]struct {
		payload string
		known   bool
		count   int
	}{
		"an App token is shown current_user_can_bypass and no bypass_actors": {appShaped, false, 0},
		"an identity that can see the field is shown an empty list":          {userShaped, true, 0},
		"a disclosed bypass actor is counted":                                {withActor, true, 1},
	} {
		t.Run(name, func(t *testing.T) {
			doer := &fakeGitHubDoer{responses: map[string]string{
				"GET /repos/bogdaniel/zenchron-engineering/rulesets":          `[{"id":22043609,"name":"trusted-main-adoption"}]`,
				"GET /repos/bogdaniel/zenchron-engineering/rulesets/22043609": testCase.payload,
			}}
			observer := GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("s")}
			observed, err := observer.Rulesets(context.Background(), governedRepo())
			if err != nil {
				t.Fatal(err)
			}
			if len(observed) != 1 {
				t.Fatalf("expected one ruleset, got %d", len(observed))
			}
			if observed[0].BypassActorsKnown != testCase.known || observed[0].BypassActors != testCase.count {
				t.Fatalf("bypass disclosure normalized to known=%t count=%d, want known=%t count=%d",
					observed[0].BypassActorsKnown, observed[0].BypassActors, testCase.known, testCase.count)
			}
		})
	}
}

// TestAdoptedBuildRefusesAnUnattributedGovernanceObservation is the fail-closed
// rule at the builder: an observer that will not say which role it observed
// under has produced an observation of unknown standing, and the build refuses
// rather than recording a trust root nobody can audit the provenance of.
func TestAdoptedBuildRefusesAnUnattributedGovernanceObservation(t *testing.T) {
	for name, provenance := range map[string]CredentialProvenance{
		"no provenance at all":  {},
		"no method":             {Role: CredentialRoleGovernance},
		"a publication role":    {Role: "publication", Method: GitHubCredentialApp},
		"an invented role":      {Role: "trusted", Method: "github-cli"},
		"a blank-padded method": {Role: CredentialRoleGovernance, Method: "   "},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			f := newAdoptedFixture(t)
			f.governance.provenance = provenance
			request := f.request(t)
			_, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
			if err == nil {
				t.Fatal("an unattributed governance observation produced an adopted artifact")
			}
			if !strings.Contains(err.Error(), "unknown standing") {
				t.Fatalf("the refusal does not name the reason: %v", err)
			}
			assertNothingInstalled(t, request.OutputRoot)
		})
	}
}

// TestAdoptedProvenanceRecordsWhoObservedTheTrustRoot: the evidence names the
// identity class, and carries no secret.
func TestAdoptedProvenanceRecordsWhoObservedTheTrustRoot(t *testing.T) {
	f := newAdoptedFixture(t)
	f.governance.provenance = GitHubCLIGovernanceCredential().Provenance()
	request := f.request(t)
	provenance, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
	if err != nil {
		t.Fatal(err)
	}
	observed := provenance.TrustRoot.ObservedBy
	if observed.Role != CredentialRoleGovernance || observed.Method != GitHubCredentialCLI {
		t.Fatalf("the evidence does not name the governance identity: %+v", observed)
	}
	if transcriptSecrets.MatchString(observed.Role + observed.Method + observed.Detail) {
		t.Fatal("the recorded credential provenance carries credential-shaped material")
	}
}

// ---------------------------------------------------------------------------
// A failed governance observation is never an empty governance state
// ---------------------------------------------------------------------------

// The rule these tests enforce is one sentence: nothing that goes wrong may
// produce the observation "the bypass actor set is empty". That statement is
// reachable from exactly one place - a forge response that actually contained
// the field - and every other outcome has to be distinguishable from it, both
// in the decision and in the record that is written down.
//
// The enumeration is deliberate rather than illustrative. Each named failure
// class gets its own case, because a rule that holds on the path its author
// thought about is not a rule.

// TestGovernanceObservationFailuresAreNeverEmptyGovernance covers the failures
// that happen at the wire: every one yields a typed error and no ruleset at
// all, so there is no observation for the builder to misread.
func TestGovernanceObservationFailuresAreNeverEmptyGovernance(t *testing.T) {
	const listing = `[{"id":22043609,"name":"trusted-main-adoption"}]`
	const disclosed = `{"id":22043609,"name":"trusted-main-adoption","enforcement":"active","target":"branch","bypass_actors":[]}`
	listingPath := "GET /repos/bogdaniel/zenchron-engineering/rulesets"
	rulesetPath := "GET /repos/bogdaniel/zenchron-engineering/rulesets/22043609"

	for name, build := range map[string]func() GitHubGovernanceObserver{
		"no governance credential is configured": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{HTTP: &fakeGitHubDoer{}}
		},
		"the credential is present but authentication fails": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP:       &fakeGitHubDoer{status: http.StatusUnauthorized},
				Credential: testGovernanceCredential("stale"),
			}
		},
		"the credential authenticates but is not authorized": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP:       &fakeGitHubDoer{status: http.StatusForbidden},
				Credential: testGovernanceCredential("s"),
			}
		},
		"the forge answers with an unparseable listing": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP:       &fakeGitHubDoer{responses: map[string]string{listingPath: "not json at all"}},
				Credential: testGovernanceCredential("s"),
			}
		},
		"the forge answers with an unparseable ruleset": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP: &fakeGitHubDoer{responses: map[string]string{
					listingPath: listing, rulesetPath: `{"bypass_actors": "not a list"}`,
				}},
				Credential: testGovernanceCredential("s"),
			}
		},
		"the forge is unreachable": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP:       &fakeGitHubDoer{err: errors.New("dial tcp: i/o timeout")},
				Credential: testGovernanceCredential("s"),
			}
		},
		"the forge is rate limiting": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP:       &fakeGitHubDoer{status: http.StatusTooManyRequests},
				Credential: testGovernanceCredential("s"),
			}
		},
		"a future credential implementation declines to answer": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP: &fakeGitHubDoer{responses: map[string]string{listingPath: listing, rulesetPath: disclosed}},
				Credential: governanceCredential{method: "future", resolve: func(RemoteIdentity) (string, error) {
					return "", errors.New("this credential cannot observe governance")
				}},
			}
		},
		"a future credential implementation answers with nothing": func() GitHubGovernanceObserver {
			return GitHubGovernanceObserver{
				HTTP: &fakeGitHubDoer{responses: map[string]string{listingPath: listing, rulesetPath: disclosed}},
				Credential: governanceCredential{method: "future", resolve: func(RemoteIdentity) (string, error) {
					return "", nil
				}},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			observed, err := build().Rulesets(context.Background(), governedRepo())
			if err == nil {
				t.Fatalf("a failed governance observation succeeded and reported %+v", observed)
			}
			if observed != nil {
				t.Fatalf("a failed governance observation still produced rulesets: %+v", observed)
			}
		})
	}
}

// TestAdoptedBuildRefusesEveryGovernanceFailure is the same enumeration one
// layer up. The assertion is not only that the build refuses: it is that NO
// provenance record exists afterwards, so there is no artifact in which an
// absent observation could later be read as an empty one.
func TestAdoptedBuildRefusesEveryGovernanceFailure(t *testing.T) {
	undisclosed := func() TrustedMainRuleset {
		r := goodRuleset()
		r.BypassActors, r.BypassActorsKnown = 0, false
		return r
	}

	for name, arrange := range map[string]func(*adoptedFixture){
		"no governance observer is wired at all": func(f *adoptedFixture) {
			f.deps.Governance = nil
		},
		"the observer will not name its role": func(f *adoptedFixture) {
			f.governance.provenance = CredentialProvenance{}
		},
		"authentication fails": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return nil, &GitHubAuthError{Detail: "github rejected the credential with status 401"}
			}
		},
		"the credential is not authorized": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return nil, &GitHubAuthError{Detail: "github rejected the credential with status 403"}
			}
		},
		"the response is unparseable": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return nil, &GitHubAPIError{Status: 200, Detail: "unexpected payload"}
			}
		},
		"the observation is transiently unavailable": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return nil, &GitHubTransientError{Status: 503, Detail: "governance observation failed"}
			}
		},
		"the forge omits the bypass actor set": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return []TrustedMainRuleset{undisclosed()}, nil
			}
		},
		"the identity changes mid-build and stops being shown the field": func(f *adoptedFixture) {
			// The configuration-reload shape: the first observation is made by
			// an identity that can see bypass actors and the revalidation is
			// not. The trust root digest covers the disclosure, so the gate
			// proven is not the gate at publication and the build refuses.
			calls := 0
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				calls++
				if calls > 1 {
					return []TrustedMainRuleset{undisclosed()}, nil
				}
				return []TrustedMainRuleset{goodRuleset()}, nil
			}
		},
		"the observation stops being possible mid-build": func(f *adoptedFixture) {
			calls := 0
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				calls++
				if calls > 1 {
					return nil, &GitHubAuthError{Detail: "the governance credential expired"}
				}
				return []TrustedMainRuleset{goodRuleset()}, nil
			}
		},
		"a future observer answers with no rulesets at all": func(f *adoptedFixture) {
			f.governance.rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
				return nil, nil
			}
		},
	} {
		t.Run("refuse when "+name, func(t *testing.T) {
			f := newAdoptedFixture(t)
			arrange(f)
			request := f.request(t)
			provenance, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
			if err == nil {
				t.Fatal("a failed governance observation produced an adopted artifact")
			}
			if provenance.TrustRoot.Bypass.Observed {
				t.Fatalf("a refused build still claims the bypass set was observed: %+v", provenance.TrustRoot.Bypass)
			}
			assertNothingInstalled(t, request.OutputRoot)
			if matches, _ := filepath.Glob(filepath.Join(request.OutputRoot, "*", "provenance.json")); len(matches) != 0 {
				t.Fatalf("a refused build left a provenance record behind: %v", matches)
			}
		})
	}
}

// TestAdoptedProvenanceStatesWhichBypassFactWasEstablished is the positive
// half: the record says "observed, and the set was empty" in so many words,
// rather than leaving a reader to infer it from a zero. The unobserved shape is
// asserted too, because a type that cannot express the difference would pass
// the first assertion by accident.
func TestAdoptedProvenanceStatesWhichBypassFactWasEstablished(t *testing.T) {
	f := newAdoptedFixture(t)
	request := f.request(t)
	provenance, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if !provenance.TrustRoot.Bypass.Observed || provenance.TrustRoot.Bypass.Count != 0 {
		t.Fatalf("an adopted record does not state that the empty bypass set was observed: %+v", provenance.TrustRoot.Bypass)
	}
	if !strings.Contains(provenance.TrustRoot.Bypass.Detail, "was shown") {
		t.Fatalf("the record does not say what was established: %q", provenance.TrustRoot.Bypass.Detail)
	}

	// The same record shape, written from an undisclosed observation, must be
	// a different record. It cannot arise from a successful build, so it is
	// asserted at the function that produces it.
	undisclosed := goodRuleset()
	undisclosed.BypassActors, undisclosed.BypassActorsKnown = 0, false
	unobserved := describeBypass(undisclosed)
	empty := describeBypass(goodRuleset())
	if unobserved.Observed || unobserved.Count != empty.Count {
		t.Fatalf("the undisclosed case is not distinguishable by its flag: %+v", unobserved)
	}
	if unobserved == empty || unobserved.Detail == empty.Detail {
		t.Fatal("an undisclosed bypass set and an observed empty one produce the same record")
	}
}
