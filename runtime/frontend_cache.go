package runtime

// Offline frontend dependency cache.
//
// A worker runs with network access denied, so `npm ci` can only succeed from a
// cache it already has. The operator prepares one cache per lockfile, keyed by
// the SHA-256 of frontend/package-lock.json:
//
//	<frontend_cache_dir>/<lockfile sha256>/
//
// For each invocation whose candidate has that lockfile, the runtime copies the
// matching cache into the invocation's own scratch and points npm at the copy
// with offline mode forced on. The operator's cache is only read, concurrent
// invocations never share a writable cache, and nothing is committed into the
// candidate. A cache for a different lockfile is simply a different directory,
// so there is no way to receive dependencies that do not match the lockfile
// being installed; npm ci then checks every tarball against the lockfile's own
// integrity hashes.
//
// This broker grants no permission and does not enable a browser: the sandbox,
// network denial and writable roots are untouched, and a worker still cannot
// run Chrome or Playwright.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	frontendLockfile      = "frontend/package-lock.json"
	frontendLockMaxBytes  = 8 << 20
	frontendCacheScratch  = "npm-cache"
	frontendUserConfig    = "npmrc"
	frontendGlobalConfig  = "npmrc-global"
	frontendVersionBudget = 5 * time.Second
)

var frontendToolVersion = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)

// provisionFrontendCache prepares the offline npm environment for one
// invocation. It returns nothing when the agent has no cache source or the
// candidate has no frontend lockfile. A missing cache for the lockfile is
// reported in the observation and leaves npm offline against an empty cache; an
// unreadable lockfile, an unreadable cache or a cache holding anything but
// plain files and directories refuses the invocation, because the cache is
// operator-owned and a fault in it should not be papered over.
func provisionFrontendCache(source, candidateDir, scratchDir string) ([]string, *domain.FrontendCacheObservation, error) {
	if source == "" {
		return nil, nil, nil
	}
	lock, err := readBounded(filepath.Join(candidateDir, frontendLockfile), frontendLockMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("frontend cache: read %s: %w", frontendLockfile, err)
	}
	sum := sha256.Sum256(lock)
	observation := &domain.FrontendCacheObservation{LockSHA256: hex.EncodeToString(sum[:])}
	if scratchDir == "" {
		observation.Status = domain.FrontendCacheScratchRequired
		return nil, observation, nil
	}
	cacheDir := filepath.Join(scratchDir, frontendCacheScratch)
	if err := os.RemoveAll(cacheDir); err != nil {
		return nil, nil, fmt.Errorf("frontend cache: reset scratch: %w", err)
	}
	copied, err := copyCacheTree(filepath.Join(source, observation.LockSHA256), cacheDir)
	if err != nil {
		return nil, nil, fmt.Errorf("frontend cache: %w", err)
	}
	observation.Status = domain.FrontendCacheMissing
	if copied {
		observation.Status = domain.FrontendCacheProvisioned
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("frontend cache: %w", err)
	}
	user, global := filepath.Join(scratchDir, frontendUserConfig), filepath.Join(scratchDir, frontendGlobalConfig)
	settings := "cache=" + cacheDir + "\noffline=true\nignore-scripts=true\naudit=false\nfund=false\nupdate-notifier=false\n"
	for path, content := range map[string]string{user: settings, global: ""} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return nil, nil, fmt.Errorf("frontend cache: %w", err)
		}
	}
	return []string{"NPM_CONFIG_USERCONFIG=" + user, "NPM_CONFIG_GLOBALCONFIG=" + global}, observation, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

// copyCacheTree copies a cache directory without following links. It reports
// false when the source does not exist.
func copyCacheTree(source, target string) (bool, error) {
	info, err := os.Lstat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("cache %s is not a plain directory", filepath.Base(source))
	}
	return true, filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(destination, 0700)
		case entry.Type().IsRegular():
			return copyCacheFile(path, destination)
		}
		return fmt.Errorf("cache entry %s is neither a file nor a directory", relative)
	})
}

func copyCacheFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// withToolVersions adds the node and npm versions the worker would resolve on
// its own search path. A version that cannot be read stays absent.
func (p CLIAgentProvider) withToolVersions(ctx context.Context, env []string) *domain.FrontendCacheObservation {
	if p.frontendCache == nil {
		return nil
	}
	observed := *p.frontendCache
	search := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			search = value
		}
	}
	read := func(tool string) string {
		path := resolveOnSearchPath(search, tool)
		if path == "" {
			return ""
		}
		out, err := p.executor().Output(ctx, path, []string{"--version"}, "", env, frontendVersionBudget)
		version := strings.TrimPrefix(strings.TrimSpace(string(out.Stdout)), "v")
		if err != nil || !frontendToolVersion.MatchString(version) {
			return ""
		}
		return version
	}
	observed.NodeVersion, observed.NpmVersion = read("node"), read("npm")
	return &observed
}

func resolveOnSearchPath(search, tool string) string {
	for _, dir := range filepath.SplitList(search) {
		candidate := filepath.Join(dir, tool)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return candidate
		}
	}
	return ""
}
