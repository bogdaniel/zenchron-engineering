package runtime

// The GitHub App publication identity.
//
// GitHubTokenFileCredential already let the runtime publish as somebody other
// than the operator, and the documentation already named a GitHub App as the
// way to get such a token. That claim was not true. An App does not hand out a
// long-lived token an operator can write into a file: it hands out an
// INSTALLATION token that is minted from the App's private key and expires in
// an hour, and an installation token cannot answer GET /user - GitHub refuses
// with 403 "Resource not accessible by integration", a status that carries no
// rate-limit headers and therefore classifies as a permanently rejected
// credential. Feedback admission fails closed on an unresolved publication
// identity, so an operator who followed the documentation got a runtime that
// published fine and never admitted a single review, permanently.
//
// This file is the identity the documentation promised: mint the installation
// token from the key, re-mint it before it expires, and resolve the identity
// from GET /app - which is also the only endpoint that can answer it, because
// the App's own comments are authored by "<slug>[bot]" and nothing in the
// installation token says what the slug is.
//
// The boundary is GitHubCLICredential's, with one addition. The private key is
// a CREDENTIAL, not a path to one once it is read, so:
//   - the file must be owner-only, exactly as the publication token file must;
//   - the parsed key, the minted token and the signed JWT live in locals and in
//     unexported fields of this struct, and String() is defined so that a %v of
//     the whole value cannot print them;
//   - no failure here quotes a key byte, a token, or the underlying I/O error,
//     which on some systems carries content fragments.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// appTokenSafetyMargin is how long before expiry the installation token is
// re-minted. An hour-long token used until its last second is a token that
// expires between the runtime deciding to publish and GitHub reading the
// header.
const appTokenSafetyMargin = 5 * time.Minute

// appAssertionLifetime is the life of the App JWT. GitHub refuses anything over
// ten minutes; the backdated issue time absorbs clock skew between this host
// and GitHub, which is the documented reason GitHub gives for backdating it.
const (
	appAssertionLifetime = 9 * time.Minute
	appAssertionBackdate = time.Minute
)

// GitHubAppCredential is the PUBLICATION credential of a GitHub App
// installation. It satisfies CredentialProvider, so the REST calls and the
// `git push` askpass path are authorized by the same value as every other mode,
// and ForgeAppIdentity, so the adapter can resolve the publication identity
// without GET /user.
//
// It is a pointer receiver because it caches: minting costs two HTTP round
// trips and a signature, and the token is valid for an hour.
type GitHubAppCredential struct {
	// AppID and InstallationID are GitHub's numeric identifiers. Neither is a
	// secret: the App id is visible on the App's public page, and the
	// installation id is visible in the installation URL.
	AppID          int64
	InstallationID int64
	// PrivateKeyPath is the PEM file downloaded when the App key was
	// generated. It is a PATH in configuration and a CREDENTIAL once read.
	PrivateKeyPath string
	// HTTP is the transport. Nil is not "no network", it is a configuration
	// fault, reported as such rather than panicking inside a credential call.
	HTTP Doer
	// Endpoint is the API root. Empty uses the public GitHub.com API.
	Endpoint string
	// Now is the clock seam. Nil uses time.Now; a test drives expiry with it
	// rather than sleeping through an hour.
	Now func() time.Time

	// Everything below is cache, guarded by mu. The token is a secret and the
	// field is unexported for that reason; see String().
	mu       sync.Mutex
	token    string
	expires  time.Time
	identity GitHubActor
}

var (
	_ CredentialProvider = (*GitHubAppCredential)(nil)
	_ ForgeAppIdentity   = (*GitHubAppCredential)(nil)
)

// String keeps a %v of this value from printing the cached installation token.
// fmt prints unexported fields, and a credential that leaks the moment somebody
// logs the struct that holds it is not held.
func (c *GitHubAppCredential) String() string {
	return fmt.Sprintf("GitHubAppCredential(app %d, installation %d)", c.AppID, c.InstallationID)
}

func (c *GitHubAppCredential) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Credential issues the installation token for the governed remote.
func (c *GitHubAppCredential) Credential(identity RemoteIdentity) (string, string, error) {
	if identity.URL == "" || identity.Transport() != "https" {
		return "", "", &GitHubAuthError{Detail: "credential is only issued to the governed https remote"}
	}
	token, _, err := c.current(context.Background())
	if err != nil {
		return "", "", err
	}
	return gitHubCredentialUser, token, nil
}

// AppIdentity names the actor GitHub stamps on this App's own comments. It is
// the App's slug with the "[bot]" suffix GitHub appends, which is what the
// self-loop guard has to recognize; the id is the APP's id rather than the bot
// account's, and is provenance only - admission compares logins.
func (c *GitHubAppCredential) AppIdentity(ctx context.Context) (GitHubActor, error) {
	_, actor, err := c.current(ctx)
	return actor, err
}

// current returns the cached installation token and identity, minting a new
// pair when none is held or the held one is close enough to expiry to be unsafe
// to use.
func (c *GitHubAppCredential) current(ctx context.Context) (string, GitHubActor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Add(appTokenSafetyMargin).Before(c.expires) {
		return c.token, c.identity, nil
	}
	key, err := c.privateKey()
	if err != nil {
		return "", GitHubActor{}, err
	}
	assertion, err := appAssertion(key, c.AppID, c.now())
	if err != nil {
		return "", GitHubActor{}, err
	}
	token, expires, err := c.exchange(ctx, assertion)
	if err != nil {
		return "", GitHubActor{}, err
	}
	identity, err := c.resolveApp(ctx, assertion)
	if err != nil {
		return "", GitHubActor{}, err
	}
	// Nothing is cached until BOTH halves succeeded: a token whose identity is
	// unknown would publish under an actor the self-loop guard cannot name.
	c.token, c.expires, c.identity = token, expires, identity
	return token, identity, nil
}

// privateKey reads and parses the App key. The owner-only rule is the token
// file's, for the stronger reason: a key another local account can read is an
// App that account can act as, for every installation, until the key is
// revoked.
func (c *GitHubAppCredential) privateKey() (*rsa.PrivateKey, error) {
	if strings.TrimSpace(c.PrivateKeyPath) == "" {
		return nil, &GitHubAuthError{Detail: "no GitHub App private key path is configured"}
	}
	info, err := os.Stat(c.PrivateKeyPath)
	switch {
	case err != nil:
		return nil, &GitHubAuthError{Detail: "the GitHub App private key file cannot be inspected"}
	case !info.Mode().IsRegular():
		return nil, &GitHubAuthError{Detail: "the GitHub App private key path is not a regular file"}
	case info.Mode().Perm()&0o077 != 0:
		return nil, &GitHubAuthError{Detail: "the GitHub App private key file is readable by other users; run chmod 600 on it"}
	}
	raw, err := os.ReadFile(c.PrivateKeyPath)
	if err != nil {
		return nil, &GitHubAuthError{Detail: "the GitHub App private key file cannot be read"}
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, &GitHubAuthError{Detail: "the GitHub App private key file is not PEM; download the key again from the App's settings page"}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, &GitHubAuthError{Detail: "the GitHub App private key file does not hold a readable private key"}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, &GitHubAuthError{Detail: "the GitHub App private key is not an RSA key, and GitHub App assertions are RS256"}
	}
	return key, nil
}

// appAssertion is the RS256 JWT that authenticates as the APP rather than as an
// installation. It is stdlib only: a JWT is two base64url JSON objects and one
// PKCS#1 v1.5 signature, and a dependency for that is a supply-chain surface
// bought with nothing.
func appAssertion(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	if appID <= 0 {
		return "", &GitHubAuthError{Detail: "no GitHub App id is configured"}
	}
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Issued  int64 `json:"iat"`
		Expires int64 `json:"exp"`
		Issuer  int64 `json:"iss"`
	}{
		Issued:  now.Add(-appAssertionBackdate).Unix(),
		Expires: now.Add(appAssertionLifetime).Unix(),
		Issuer:  appID,
	})
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	signing := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		// The signing error is not quoted: it is produced from key material.
		return "", &GitHubAuthError{Detail: "the GitHub App assertion could not be signed with the configured private key"}
	}
	return signing + "." + encode(signature), nil
}

// exchange trades the App assertion for an installation access token.
func (c *GitHubAppCredential) exchange(ctx context.Context, assertion string) (string, time.Time, error) {
	if c.InstallationID <= 0 {
		return "", time.Time{}, &GitHubAuthError{Detail: "no GitHub App installation id is configured"}
	}
	status, header, body, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", c.InstallationID), assertion)
	if err != nil {
		return "", time.Time{}, err
	}
	rate, reported := observeRateLimit(header, status)
	if err := classifyGitHubStatus(status, rate, reported, "installation token"); err != nil {
		return "", time.Time{}, err
	}
	var payload struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", time.Time{}, &GitHubAPIError{Status: status, Detail: "unreadable installation token response"}
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", time.Time{}, &GitHubAuthError{Detail: "the installation token response carried no token"}
	}
	if payload.Expires.IsZero() {
		// An expiry the runtime did not read is not assumed to be an hour: a
		// token used past its life is an outage in the middle of a publish.
		return "", time.Time{}, &GitHubAPIError{Status: status, Detail: "the installation token response carried no expiry"}
	}
	return payload.Token, payload.Expires, nil
}

// resolveApp reads the App's own record. It is authenticated with the ASSERTION
// and not with the installation token, because GET /app is an App endpoint: an
// installation token is refused there exactly as it is on GET /user.
func (c *GitHubAppCredential) resolveApp(ctx context.Context, assertion string) (GitHubActor, error) {
	status, header, body, err := c.do(ctx, http.MethodGet, "/app", assertion)
	if err != nil {
		return GitHubActor{}, err
	}
	rate, reported := observeRateLimit(header, status)
	if err := classifyGitHubStatus(status, rate, reported, "publication identity"); err != nil {
		return GitHubActor{}, err
	}
	var payload struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return GitHubActor{}, &GitHubAPIError{Status: status, Detail: "unreadable publication identity response"}
	}
	slug := strings.TrimSpace(payload.Slug)
	if slug == "" {
		return GitHubActor{}, &GitHubAPIError{Status: status, Detail: "the forge named no App slug, so the account its comments are authored by cannot be derived"}
	}
	return GitHubActor{Login: slug + "[bot]", ID: payload.ID, Bot: true}, nil
}

// do performs one App-authenticated request. It is separate from
// GitHubRESTAdapter.doRaw because that path resolves a credential through this
// one, and because these two endpoints are not repository-scoped.
func (c *GitHubAppCredential) do(ctx context.Context, method, path, assertion string) (int, http.Header, []byte, error) {
	if c.HTTP == nil {
		return 0, nil, nil, &GitHubAuthError{Detail: "the GitHub App credential has no HTTP transport"}
	}
	// The endpoint is checked before the assertion is ever placed in a request,
	// for the same reason the REST adapter and the governance observer check
	// theirs first: a refused endpoint must never see the credential.
	root, err := githubAPIRoot(c.Endpoint)
	if err != nil {
		return 0, nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, root+path, nil)
	if err != nil {
		return 0, nil, nil, &GitHubAPIError{Detail: "the GitHub App request could not be built"}
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "zenchron-engineering")
	request.Header.Set("Authorization", "Bearer "+assertion)
	response, err := c.HTTP.Do(request)
	if err != nil {
		// A transport failure is transient by nature, and the error is not
		// quoted: it is built from a request that carries the assertion.
		return 0, nil, nil, &GitHubTransientError{Detail: "the GitHub App endpoint could not be reached"}
	}
	defer response.Body.Close()
	raw, err := readBoundedBody(response)
	if err != nil {
		return 0, nil, nil, &GitHubAPIError{Status: response.StatusCode, Detail: "unreadable GitHub App response"}
	}
	return response.StatusCode, response.Header, raw, nil
}
