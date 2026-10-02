package main

// The control plane's own authority boundary.
//
// The existing control socket's authority is filesystem permission on a Unix
// domain socket (runtime/control_endpoint.go): nothing authenticating is
// stored beside it, because owning the socket path IS the credential. A TCP
// loopback listener has no equivalent - any local user can dial 127.0.0.1 -
// so a bearer token is the minimum boundary that makes the two comparable.
// The token file itself inherits the same discipline: owner-only (0600)
// inside an owner-only (0700) directory under the operator's Zenchron state
// directory, so reading it requires the same filesystem access a Unix socket
// would have required.
//
// ADR-0004 is explicit that the token is never exposed in a URL, a log line,
// or an API payload. That rules out the common "printed URL carries the
// token as a query parameter" bootstrap: a query parameter ends up in proxy
// and access logs, browser history, and the Referer header of whatever the
// page links to next. Instead, `control-plane` prints only the TOKEN FILE's
// path at startup (controlPlaneTokenPath) - the operator already has the
// filesystem access needed to read it, the same access a Unix-socket-based
// credential would have required - and the console's own "/login" page
// accepts the token pasted into a form POST, never a URL. The one request
// that presents it that way is handed back a short-lived, HttpOnly,
// same-site cookie, so the credential does not linger anywhere the token
// itself was not already written.
import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// controlPlaneTokenBytes is the raw entropy of a generated token, before hex
// encoding doubles its length.
const controlPlaneTokenBytes = 32

// controlPlaneCookie is the name of the session cookie a browser is handed
// once it presents the token. Its value is never anything other than the
// token itself: there is no server-side session store to look it up in, so
// the cookie's value is exactly the credential clients using the
// Authorization header present instead.
const controlPlaneCookie = "zenchron_control_plane_token"

// controlPlaneCookieMaxAge is deliberately short relative to a typical
// operator session: the console is meant to be reopened, not left signed in
// indefinitely on a shared machine.
const controlPlaneCookieMaxAge = 24 * time.Hour

// controlPlaneDir is the owner-only directory the control plane keeps its own
// state in, inside the operator's Zenchron state directory.
func controlPlaneDir(stateDir string) string {
	return filepath.Join(stateDir, "control-plane")
}

// controlPlaneTokenPath is where the bearer token lives on disk. It is the
// only form in which `control-plane` ever reports the token at startup: the
// path, never the value, so the value is never written to this process's own
// stdout/log.
func controlPlaneTokenPath(stateDir string) string {
	return filepath.Join(controlPlaneDir(stateDir), "token")
}

// loadOrCreateControlPlaneToken returns the bearer token every request must
// present. A token already on disk is reused, so restarting the console does
// not invalidate a browser's existing session; an absent one is generated and
// persisted.
//
// An existing directory or file that is not owner-only is REFUSED, not
// repaired: silently tightening permissions on a path would hide a
// configuration mistake instead of reporting it.
func loadOrCreateControlPlaneToken(stateDir string) (string, error) {
	dir := controlPlaneDir(stateDir)
	if err := ensureOwnerOnlyDir(dir); err != nil {
		return "", err
	}
	path := controlPlaneTokenPath(stateDir)
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("control plane token %s is not owner-only (mode %o); refusing to use it", path, info.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("control plane token %s is empty", path)
		}
		return token, nil
	case os.IsNotExist(err):
		token, genErr := generateControlPlaneToken()
		if genErr != nil {
			return "", genErr
		}
		// O_EXCL: two processes racing to create the token must not have one
		// silently overwrite the other's token file after a browser has
		// already been handed it.
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return "", openErr
		}
		defer file.Close()
		if _, writeErr := file.WriteString(token); writeErr != nil {
			return "", writeErr
		}
		return token, nil
	default:
		return "", err
	}
}

func generateControlPlaneToken() (string, error) {
	raw := make([]byte, controlPlaneTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control plane token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// ensureOwnerOnlyDir creates dir at 0700 if absent, and refuses an existing
// path that is not owner-only or not a directory.
func ensureOwnerOnlyDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s is not owner-only (mode %o); refusing to use it", dir, info.Mode().Perm())
		}
		return nil
	case os.IsNotExist(err):
		return os.MkdirAll(dir, 0700)
	default:
		return err
	}
}

// controlPlaneTokenPresented extracts whatever credential this request
// offered: the Authorization header a script or curl would set, or the
// session cookie a browser that already signed in through "/login" carries
// automatically. It deliberately does NOT look at a query parameter or at
// the login form's own POST body - the only ways the raw token travels on
// the wire are the header and the cookie it is exchanged for.
func controlPlaneTokenPresented(r *http.Request) string {
	const prefix = "Bearer "
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, prefix) {
		return strings.TrimPrefix(header, prefix)
	}
	if cookie, err := r.Cookie(controlPlaneCookie); err == nil {
		return cookie.Value
	}
	return ""
}

// controlPlaneTokenEqual is a constant-time comparison so a timing
// difference cannot be used to guess the token one byte at a time.
func controlPlaneTokenEqual(token, presented string) bool {
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// setControlPlaneCookie hands a browser that just proved it holds the token
// (by presenting it in the "/login" form POST) a cookie that stands in for
// it on every subsequent request. HttpOnly keeps it out of reach of any
// script this page runs; SameSite=Lax keeps it from being sent on a
// cross-site request while still surviving the top-level redirect that sets
// it. There is no Secure flag because this listener is plain HTTP on
// loopback by design (ADR-0004) and marking the cookie Secure would make the
// browser discard it outright.
func setControlPlaneCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     controlPlaneCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(controlPlaneCookieMaxAge.Seconds()),
	})
}

// safeLoginRedirect validates "next" so a login POST never redirects off
// this console: it must be a path on this origin (starting with exactly one
// "/", never "//..." which a browser resolves as protocol-relative to
// another host). Anything else falls back to the console's own home.
func safeLoginRedirect(next string) string {
	if next == "" || next == "/login" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/overview"
	}
	return next
}
