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

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// controlPlaneTokenBytes is the raw entropy of a generated token, before hex
// encoding doubles its length.
const controlPlaneTokenBytes = 32

// controlPlaneDir is the owner-only directory the control plane keeps its own
// state in, inside the operator's Zenchron state directory.
func controlPlaneDir(stateDir string) string {
	return filepath.Join(stateDir, "control-plane")
}

// loadOrCreateControlPlaneToken returns the bearer token every request must
// present. A token already on disk is reused, so restarting the control plane
// does not invalidate a frontend's existing session; an absent one is
// generated and persisted.
//
// An existing directory or file that is not owner-only is REFUSED, not
// repaired: silently tightening permissions on a path would hide a
// configuration mistake instead of reporting it.
func loadOrCreateControlPlaneToken(stateDir string) (string, error) {
	dir := controlPlaneDir(stateDir)
	if err := ensureOwnerOnlyDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "token")
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
		// silently overwrite the other's token file after a frontend has
		// already read it.
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

// controlPlaneAuthorized reports whether an Authorization header carries
// exactly this token as a bearer credential. The comparison is constant-time
// so a timing difference cannot be used to guess the token one byte at a time.
func controlPlaneAuthorized(token, header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}
