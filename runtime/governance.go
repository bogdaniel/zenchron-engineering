package runtime

// Governance observation is a different authority from publication, and this
// file is the boundary between them.
//
// The distinction is forced by the forge and is not an abstraction invented
// here. A GitHub App installation token - the runtime's publication identity,
// established so the operator stays a distinct actor whose review is
// admissible feedback - is never shown a ruleset's bypass_actors, even when it
// carries administration:read. It is shown current_user_can_bypass instead,
// which answers "can this caller bypass" rather than "who can bypass", and the
// adoption trust root depends on the second question. An identity that can see
// the field answers it; the App cannot, and no amount of permission granting
// changes that.
//
// So the runtime holds two credentials with two roles, and the invariant
// between them is:
//
//	Governance credentials may contribute read-only governance facts to
//	authority decisions, but must never be usable for forge publication or
//	other execution actions. Publication credentials may perform their
//	explicitly permitted forge actions, but lack of governance visibility must
//	never be interpreted as governance truth.
//
// Both halves are enforced by the type system rather than by discipline:
//
//   - A GovernanceCredential does not implement CredentialProvider. The method
//     is named differently and returns one value instead of two, so a
//     governance credential cannot be assigned to GitHubRESTAdapter.Credentials
//     or to RemotePolicy.Credentials. There is no conversion; there is a
//     compile error.
//   - GitHubGovernanceObserver has no publication method to call. It does not
//     implement GitHubAdapter, ForgeConversation or ForgeViewer, it issues
//     GET with no request body and nothing else, and it is the only type in
//     the runtime that holds a GovernanceCredential.
//   - GitHubRESTAdapter, which holds the publication credential, has no
//     governance method at all. Reading a ruleset through it is not a thing
//     that can be spelled, so the second half of the invariant - an absent
//     fact must never read as a favourable fact - is not something the
//     publication path can get wrong, because the publication path never
//     observes the fact.
//
// What generalises beyond GitHub is the PROPERTY, not the signature, and the
// difference is worth stating precisely because it is easy to claim too much.
// The property generalises: an observation carries whether the bypass set was
// disclosed, BypassActorsKnown is false unless a forge actually disclosed it,
// and the refusal is therefore inherited by any provider that cannot answer -
// silence is never read as a favourable answer, whoever is silent. The
// signature does not: Rulesets names GitHubRepo and TrustedMainRuleset, so a
// genuinely different forge needs those widened before it can implement this
// interface at all. That widening is deliberately not done here. There is one
// forge, and an abstraction built for a second one that does not exist would
// be shaped by guesses rather than by the second forge's actual facts.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// CredentialRoleGovernance is the only role a governance observation may be
// attributed to. It is recorded in adoption evidence so a reader can tell which
// identity CLASS disclosed the trust root, and it is checked before a build is
// called adopted, so an observer that misreports its own role is refused rather
// than believed.
const CredentialRoleGovernance = "governance-observation"

// CredentialProvenance is the non-secret account of which identity produced an
// observation. It names a role and a method - never a token, never a key path,
// never a login that would have to be kept in step with the forge.
type CredentialProvenance struct {
	Role   string `json:"role"`
	Method string `json:"method"`
	Detail string `json:"detail,omitempty"`
}

// GovernanceCredential resolves the secret authorized to READ governance facts
// about a governed remote.
//
// It is deliberately not CredentialProvider. The username half is missing
// because the only consumer of a username is the git askpass path, which is
// publication; and the method name differs so no value of this type can be
// passed where publication authority is expected.
type GovernanceCredential interface {
	// ObserveGovernance resolves the read-only secret for exactly this remote.
	// Every failure is typed exactly as a publication credential's is, so a
	// transient failure to observe is never recorded as a rejected credential.
	ObserveGovernance(identity RemoteIdentity) (secret string, err error)
	// Provenance describes the credential without disclosing it.
	Provenance() CredentialProvenance
}

// ForgeGovernance is the read-only governance-observation seam. It is the only
// way anything in the runtime learns a governance fact.
//
// It is an interface rather than a concrete adapter so that the BUILDER depends
// on the capability instead of on the credential behind it, which is what keeps
// the publication adapter out of the governance path. It is not yet a
// forge-neutral interface: the method below names GitHub types, and a second
// forge would require widening them.
type ForgeGovernance interface {
	// Rulesets reads the repository's branch rulesets. A ruleset whose
	// bypass disclosure the forge omitted is reported as undisclosed, never as
	// empty - see TrustedMainRuleset.BypassActorsKnown.
	Rulesets(ctx context.Context, repo GitHubRepo) ([]TrustedMainRuleset, error)
	// GovernanceProvenance names the identity these observations came from.
	GovernanceProvenance() CredentialProvenance
}

// governanceCredential is the only implementation of GovernanceCredential in
// this package. It holds a resolver function rather than a CredentialProvider
// so that the value does not even CARRY publication authority it could be
// talked out of: there is no field to reach through.
type governanceCredential struct {
	method  string
	detail  string
	resolve func(RemoteIdentity) (string, error)
}

func (c governanceCredential) ObserveGovernance(identity RemoteIdentity) (string, error) {
	if c.resolve == nil {
		return "", &GitHubAuthError{Detail: "no governance credential is authorized"}
	}
	return c.resolve(identity)
}

func (c governanceCredential) Provenance() CredentialProvenance {
	return CredentialProvenance{Role: CredentialRoleGovernance, Method: c.method, Detail: c.detail}
}

var _ GovernanceCredential = governanceCredential{}

// GitHubCLIGovernanceCredential demotes the operator's already-authenticated
// local `gh` session to governance observation.
//
// The operator's own identity is the one GitHub discloses bypass_actors to, and
// reading a ruleset is a control-plane observation rather than an act performed
// in anybody's name, so borrowing it to READ costs the identity separation #82
// established nothing: the runtime still publishes as the App, and the value
// returned here cannot publish at all.
func GitHubCLIGovernanceCredential() GovernanceCredential {
	return governanceCredential{
		method: GitHubCredentialCLI,
		detail: "the operator's local GitHub CLI session, used read-only for governance facts the publication identity cannot observe",
		resolve: func(identity RemoteIdentity) (string, error) {
			_, secret, err := GitHubCLICredential{}.Credential(identity)
			return secret, err
		},
	}
}

// GitHubGovernanceObserver reads governance facts from GitHub.com.
//
// It is a separate type from GitHubRESTAdapter rather than a mode of it, and it
// shares none of that adapter's request plumbing. That is the point: the
// plumbing here takes no method and no request body, so there is no argument a
// caller could supply that turns a governance observation into a write.
type GitHubGovernanceObserver struct {
	HTTP Doer
	// Endpoint is the API root. Empty uses the public GitHub.com API.
	Endpoint string
	// Credential is the operator-authorized governance credential. Nil is not
	// "anonymous", it is github_auth_required.
	Credential GovernanceCredential
}

var _ ForgeGovernance = GitHubGovernanceObserver{}

func (o GitHubGovernanceObserver) GovernanceProvenance() CredentialProvenance {
	if o.Credential == nil {
		return CredentialProvenance{}
	}
	return o.Credential.Provenance()
}

// get performs one read. It is the whole HTTP surface of this type: GET, no
// body, no caller-supplied headers, no method parameter.
func (o GitHubGovernanceObserver) get(ctx context.Context, repo GitHubRepo, path string, out any) error {
	if o.HTTP == nil {
		return fmt.Errorf("the governance observer has no HTTP transport")
	}
	identity, err := repo.identity()
	if err != nil {
		return &GitHubAuthError{Detail: "repository is not a governed GitHub remote"}
	}
	if o.Credential == nil {
		return &GitHubAuthError{Detail: "no operator-authorized governance credential is configured"}
	}
	secret, err := o.Credential.ObserveGovernance(identity)
	if err != nil {
		return err
	}
	if secret == "" {
		return &GitHubAuthError{Detail: "governance credential resolution produced an empty token"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIRoot(o.Endpoint)+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "zenchron-engineering")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := o.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("github governance request failed")
	}
	defer response.Body.Close()
	raw, err := readBoundedBody(response)
	if err != nil {
		return fmt.Errorf("github governance response unreadable")
	}
	rate, reported := observeRateLimit(response.Header, response.StatusCode)
	if err := classifyGitHubStatus(response.StatusCode, rate, reported, "governance observation of "+path); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &GitHubAPIError{Status: response.StatusCode, Detail: "governance observation of " + path + " returned an unexpected payload"}
	}
	return nil
}

// Rulesets reads the repository's branch rulesets. It is the only way the
// adopted-controller builder learns whether a trust root exists: the builder
// never assumes protection from a branch name, and never takes an operator's
// word for it.
//
// The listing gives ids and names only, so each ruleset is then fetched
// individually for the rules themselves. That is one request per ruleset, and
// a repository has a handful, not thousands.
func (o GitHubGovernanceObserver) Rulesets(ctx context.Context, repo GitHubRepo) ([]TrustedMainRuleset, error) {
	var listing []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := o.get(ctx, repo, repoPath(repo)+"/rulesets", &listing); err != nil {
		return nil, err
	}
	rulesets := make([]TrustedMainRuleset, 0, len(listing))
	for _, entry := range listing {
		var wire struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Enforcement string `json:"enforcement"`
			Target      string `json:"target"`
			Conditions  struct {
				RefName struct {
					Include []string `json:"include"`
					Exclude []string `json:"exclude"`
				} `json:"ref_name"`
			} `json:"conditions"`
			// A POINTER, so an omitted or null bypass_actors stays
			// distinguishable from a disclosed empty list. This is the exact
			// difference #219 turns on: an App token is served
			// current_user_can_bypass and no bypass_actors at all, and reading
			// that silence as "there are none" would be the belief in a gate
			// rather than a gate.
			BypassActors *[]json.RawMessage `json:"bypass_actors"`
			Rules        []struct {
				Type       string `json:"type"`
				Parameters struct {
					AllowedMergeMethods []string `json:"allowed_merge_methods"`
					RequiredApprovals   int      `json:"required_approving_review_count"`
					Strict              bool     `json:"strict_required_status_checks_policy"`
					Checks              []struct {
						Context       string `json:"context"`
						IntegrationID int64  `json:"integration_id"`
					} `json:"required_status_checks"`
				} `json:"parameters"`
			} `json:"rules"`
		}
		if err := o.get(ctx, repo, repoPath(repo)+"/rulesets/"+strconv.FormatInt(entry.ID, 10), &wire); err != nil {
			return nil, err
		}
		observed := TrustedMainRuleset{
			ID: wire.ID, Name: wire.Name, Enforcement: wire.Enforcement,
			Targets: wire.Conditions.RefName.Include, Excluded: wire.Conditions.RefName.Exclude,
			TargetType: wire.Target,
		}
		if wire.BypassActors != nil {
			observed.BypassActors, observed.BypassActorsKnown = len(*wire.BypassActors), true
		}
		for _, rule := range wire.Rules {
			switch rule.Type {
			case "deletion":
				observed.Deletion = true
			case "non_fast_forward":
				observed.NonFastForward = true
			case "pull_request":
				observed.PullRequest = &PullRequestRule{
					AllowedMergeMethods: rule.Parameters.AllowedMergeMethods,
					RequiredApprovals:   rule.Parameters.RequiredApprovals,
				}
			case "required_status_checks":
				checks := make([]RequiredCheck, 0, len(rule.Parameters.Checks))
				for _, c := range rule.Parameters.Checks {
					checks = append(checks, RequiredCheck{Context: c.Context, IntegrationID: c.IntegrationID})
				}
				observed.RequiredChecks = &RequiredChecksRule{Strict: rule.Parameters.Strict, Checks: checks}
			}
		}
		rulesets = append(rulesets, observed)
	}
	return rulesets, nil
}
