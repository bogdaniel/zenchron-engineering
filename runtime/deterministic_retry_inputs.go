package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Each branch reads only inputs to its owning check. No branch stages a tree,
// repairs metadata, changes candidate bytes, or consults unrelated policy.
func candidateFailureBinding(dir, code, checkPath string, result json.RawMessage) (string, error) {
	switch code {
	case "candidate.sensitive_path":
		if checkPath == "" {
			return "", errors.New("sensitive-path failure has no check subject")
		}
		paths, err := retryCommitPaths(dir, checkPath)
		if err != nil {
			return "", err
		}
		return Digest(slices.Contains(paths, checkPath))
	case "candidate.credential_file", "candidate.symlink":
		if checkPath == "" {
			return "", errors.New("path-shape failure has no check subject")
		}
		normalized, err := normalizedCandidatePath(checkPath)
		if err != nil {
			return "", err
		}
		info, err := retryCheckPathInfo(dir, normalized)
		if errors.Is(err, os.ErrNotExist) {
			return Digest(false)
		}
		if err != nil {
			return "", err
		}
		if code == "candidate.symlink" {
			return Digest(info.Mode()&os.ModeSymlink != 0)
		}
		return Digest(info.Mode().IsRegular() && sensitiveCredentialFilename(filepath.Base(checkPath)))
	case "candidate.index_flags":
		flagged, err := flaggedIndexEntries(dir)
		if err != nil {
			return "", err
		}
		return Digest(flagged)
	case "candidate.residue":
		var failure commitFailure
		if err := decodeJSON(result, &failure); err != nil {
			return "", err
		}
		if failure.RuntimeCommit == nil {
			return "", errors.New("residue failure has no recorded runtime commit")
		}
		paths, err := dirtyPathsOutside(dir, failure.RuntimeCommit.ExcludedPaths)
		if err != nil {
			return "", err
		}
		return Digest(paths)
	case "candidate.empty", "candidate.path_shape", "candidate.scratch_only", "candidate.exclusion_record_limit":
		paths, err := changedPaths(dir)
		if err != nil {
			return "", err
		}
		if code == "candidate.empty" || code == "candidate.path_shape" {
			return Digest(paths)
		}
		debris, err := classifyRuntimeDebris(dir, paths)
		if err != nil {
			return "", err
		}
		return Digest(struct{ Eligible, Excluded []string }{withoutPaths(paths, debris.Excluded), debris.Excluded})
	case "candidate.visible_credential_value":
		if checkPath == "" {
			return "", errors.New("credential-value failure has no check subject")
		}
		return retryVisibleContent(dir, []string{checkPath})
	case "candidate.worktree_divergence", "candidate.staged_credential_value":
		if checkPath == "" {
			return "", errors.New("staged-content failure has no check subject")
		}
		return retryCandidateContentBinding(dir, code, checkPath)
	case "candidate.size_limit", "candidate.empty_tree_diff":
		return retryCandidateContentBinding(dir, code, "")
	default:
		return "", fmt.Errorf("no deterministic binding scope for check %q", code)
	}
}

// Names that the next add -A would carry, observed without updating the index.
// A staged addition since deleted is absent; names of deletions from HEAD are
// present, because the name gate also judges deletion paths. Nested repositories
// are classified by the same structural owner as the commit's exclusion gate.
func retryCommitPaths(dir, checkPath string) ([]string, error) {
	paths := []string{checkPath}
	if checkPath == "" {
		var err error
		paths, err = candidateChangedPaths(dir)
		if err != nil {
			return nil, err
		}
	}
	debris, err := classifyRuntimeDebris(dir, paths)
	if err != nil {
		return nil, err
	}
	paths = withoutPaths(paths, debris.Excluded)
	head, err := readHead(dir)
	if err != nil {
		return nil, err
	}
	store, err := subjectStore(dir, head.Commit)
	if err != nil {
		return nil, err
	}
	fileMode, err := gitOutput(dir, "config", "--type=bool", "--default=true", "--get", "core.filemode")
	if err != nil {
		return nil, err
	}
	var carried []string
	for _, path := range paths {
		entry, err := gitOutput(store, "ls-tree", "-z", head.Commit, "--", path)
		if err != nil {
			return nil, err
		}
		info, err := retryCheckPathInfo(dir, path)
		if errors.Is(err, os.ErrNotExist) {
			if entry != "" {
				carried = append(carried, path) // A deletion still carries its refused name.
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if entry == "" || !info.Mode().IsRegular() {
			carried = append(carried, path)
			continue
		}
		// A staged change reverted in the worktree disappears after add -A.
		// Compare with verified baseline content, never candidate objects.
		blob, err := gitOutput(dir, "hash-object", "--", path)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(strings.SplitN(entry, "\t", 2)[0])
		if len(fields) != 3 {
			return nil, errors.New("unreadable retry baseline entry")
		}
		mode := "100644"
		if info.Mode().Perm()&0111 != 0 {
			mode = "100755"
		}
		if strings.TrimSpace(fileMode) == "false" {
			staged, err := gitOutput(dir, "ls-files", "--stage", "-z", "--", path)
			if err != nil {
				return nil, err
			}
			mode = fields[0]
			if staged != "" {
				mode, _, _ = strings.Cut(staged, " ")
			}
		}
		if fields[2] != strings.TrimSpace(blob) || fields[0] != mode {
			carried = append(carried, path)
		}
	}
	return carried, nil
}

// The staged checks bind their own subject, not every readable workspace file.
// Resolved attributes and only the selected filter's configuration can change
// the next staged bytes. Unrelated Git config, ignored files and scratch cannot.
func retryCandidateContentBinding(dir, code, checkPath string) (string, error) {
	paths := []string{checkPath}
	if checkPath == "" {
		var err error
		paths, err = retryCommitPaths(dir, "")
		if err != nil {
			return "", err
		}
	}
	content, err := retryVisibleContent(dir, paths)
	if err != nil {
		return "", err
	}
	indexArgs := []string{"ls-files", "--stage", "-z"}
	if checkPath != "" {
		indexArgs = append(indexArgs, "--", checkPath)
	}
	index, err := gitOutput(dir, indexArgs...)
	if err != nil {
		return "", err
	}
	if code != "candidate.empty_tree_diff" {
		// The content predicates judge blob bytes, never index mode bits.
		var entries []string
		for _, rec := range strings.Split(index, "\x00") {
			if at := strings.IndexByte(rec, ' '); at >= 0 {
				entries = append(entries, rec[at+1:])
			}
		}
		index = strings.Join(entries, "\x00")
	}
	var modes map[string]string
	if code == "candidate.empty_tree_diff" {
		modes = make(map[string]string, len(paths))
		for _, path := range paths {
			info, err := os.Lstat(filepath.Join(dir, path))
			if errors.Is(err, os.ErrNotExist) {
				modes[path] = "deleted"
				continue
			}
			if err != nil {
				return "", err
			}
			modes[path] = fmt.Sprint(info.Mode().Type(), info.Mode().Perm()&0111 != 0)
		}
	}
	var attributes []string
	filters := map[string]bool{}
	for _, path := range paths {
		attrs, err := gitOutput(dir, "check-attr", "-z", "filter", "text", "eol", "working-tree-encoding", "ident", "crlf", "--", path)
		if err != nil {
			return "", err
		}
		attributes = append(attributes, attrs)
		parts := strings.Split(attrs, "\x00")
		for i := 0; i+2 < len(parts); i += 3 {
			if parts[i+1] == "filter" {
				filters["filter."+parts[i+2]+"."] = true
			}
		}
	}
	config, err := gitOutput(dir, "config", "--null", "--list", "--local")
	if err != nil {
		return "", err
	}
	var conversion []string
	for _, rec := range strings.Split(config, "\x00") {
		key, _, _ := strings.Cut(rec, "\n")
		switch key {
		case "core.autocrlf", "core.eol", "core.safecrlf":
			conversion = append(conversion, rec)
		case "core.filemode":
			if code == "candidate.empty_tree_diff" {
				conversion = append(conversion, rec)
			}
		default:
			for prefix := range filters {
				if strings.HasPrefix(key, prefix) {
					conversion = append(conversion, rec)
				}
			}
		}
	}
	head, err := readHead(dir)
	if err != nil {
		return "", err
	}
	store, err := subjectStore(dir, head.Commit)
	if err != nil {
		return "", err
	}
	base, err := gitOutput(store, "rev-parse", head.Commit+"^{tree}")
	if err != nil {
		return "", err
	}
	base = strings.TrimSpace(base)
	if checkPath != "" {
		if base, err = gitOutput(store, "ls-tree", "-z", head.Commit, "--", checkPath); err != nil {
			return "", err
		}
	}
	ceiling := int64(0)
	if code == "candidate.size_limit" {
		ceiling = maxCandidateBytes
	}
	return Digest(struct {
		Content, Index, Base   string
		Attributes, Conversion []string
		Modes                  map[string]string
		SizeCeiling            int64
	}{content, index, base, attributes, conversion, modes, ceiling})
}

func retryVisibleContent(dir string, paths []string) (string, error) {
	content := make(map[string]string, len(paths))
	for _, path := range paths {
		normalized, err := normalizedCandidatePath(path)
		if err != nil {
			return "", err
		}
		info, err := retryCheckPathInfo(dir, normalized)
		if errors.Is(err, os.ErrNotExist) {
			content[path] = "deleted"
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			content[path] = "nonregular"
			continue
		}
		digest, err := fileDigest(filepath.Join(dir, path))
		if err != nil {
			return "", err
		}
		content[path] = digest
	}
	return Digest(content)
}

// Match the scanner's visibility: it never descends through a directory link.
// A former subject behind such a link is absent, not permission to hash a file
// outside the candidate. The final leaf is observed with Lstat as well.
func retryCheckPathInfo(dir, normalized string) (os.FileInfo, error) {
	parts := strings.Split(normalized, "/")
	var info os.FileInfo
	for i, part := range parts {
		dir = filepath.Join(dir, part)
		var err error
		info, err = os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fs.ErrNotExist
		}
	}
	return info, nil
}
