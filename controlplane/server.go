package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// TokenFile is private local state, never an HTTP response or a log field.
const TokenFile = "control-plane.token"

func LoadToken(stateDir string) (string, error) {
	info, err := os.Lstat(stateDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("control plane requires an owner-only state directory")
	}
	path := filepath.Join(stateDir, TokenFile)
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, writeErr := f.WriteString(token)
		closeErr := f.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return token, nil
	}
	if !os.IsExist(err) {
		return "", err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return "", fmt.Errorf("token must be a regular 0600 file")
	}
	f, err = os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) || after.Size() != 64 || after.Mode().Perm() != 0600 {
		return "", fmt.Errorf("token file changed")
	}
	b = make([]byte, 64)
	n, err := io.ReadFull(f, b)
	if err != nil || n != 64 {
		return "", fmt.Errorf("invalid token file")
	}
	if _, err = hex.DecodeString(string(b[:n])); err != nil {
		return "", fmt.Errorf("invalid token file")
	}
	return string(b[:n]), nil
}

// Listen accepts literal loopback addresses only: configuration cannot expose
// this local authority boundary on a wildcard or externally routed interface.
func Listen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("control plane requires a loopback IP address")
	}
	return net.Listen("tcp", address)
}
