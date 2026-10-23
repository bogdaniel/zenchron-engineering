package runtime

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parseFileSizeManifest is scripts/check_file_sizes.py's read_manifest and
// parse_limit, ported without relaxing any of them, over the text of one
// manifest as the BASE revision holds it. It is a pure function: a manifest the
// canonical checker refuses is refused here, with the reason and the manifest
// named, and never read as a partial or a widened policy.
//
// Two entries for one path are refused, not merged. Taking the last one would
// let a later entry silently widen a limit a human approved earlier in the file.
//
// The checker's STALE-entry rules (an entry for a path that is not a tracked
// source file over the cap) are repository maintenance, not a property of a
// candidate, and are not part of this gate; see docs/file-size-policy.md.
func parseFileSizeManifest(name, data string, columns int) (map[string]int, error) {
	if !utf8.ValidString(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", name)
	}
	limits := map[string]int{}
	// Cut, strip and judge the text exactly as Python does (file_size_pytext.go).
	for number, line := range splitPythonLines(data) {
		if trimPythonSpace(line) == "" || strings.HasPrefix(trimLeftPythonSpace(line), "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != columns {
			return nil, fmt.Errorf("malformed %s entry at line %d", name, number+1)
		}
		for _, field := range fields {
			if trimPythonSpace(field) == "" {
				return nil, fmt.Errorf("malformed %s entry at line %d", name, number+1)
			}
		}
		if !isCanonicalRelativePath(fields[0]) {
			return nil, fmt.Errorf("%s path is not repository-relative: %q", name, fields[0])
		}
		if _, duplicate := limits[fields[0]]; duplicate {
			return nil, fmt.Errorf("duplicate %s entry: %q", name, fields[0])
		}
		limit, err := parseFileSizeLimit(fields[1], name, number+1)
		if err != nil {
			return nil, err
		}
		limits[fields[0]] = limit
	}
	return limits, nil
}

// isCanonicalRelativePath is the checker's test on an entry's path:
// PurePosixPath(name) is not absolute, has no ".." component, and
// as_posix() == name, which is to say the name is already normalised - no empty
// or "." component, no doubled or trailing slash.
func isCanonicalRelativePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

var fileSizeLimitPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseFileSizeLimit is parse_limit: a canonical positive integer above the hard cap.
func parseFileSizeLimit(text, name string, number int) (int, error) {
	limit, err := strconv.Atoi(text)
	if !fileSizeLimitPattern.MatchString(text) || err != nil || limit <= fileSizeHardCap {
		return 0, fmt.Errorf("%s line %d: limit must be an integer above %d", name, number, fileSizeHardCap)
	}
	return limit, nil
}

// refuseSymlinkedEntries is read_manifest's `Path(name).is_symlink()` test,
// applied to each entry of one manifest against the PINNED tree's own symlink
// listing (git mode 120000) rather than a filesystem path a producer could have
// shaped. Not counting a symlink as a source file is a separate rule; this one
// refuses the manifest, as the script does, for both manifests.
func refuseSymlinkedEntries(manifest string, entries map[string]int, symlinks map[string]bool) error {
	var linked []string
	for path := range entries {
		if symlinks[path] {
			linked = append(linked, path)
		}
	}
	if len(linked) == 0 {
		return nil
	}
	sort.Strings(linked)
	return fmt.Errorf("%s entry is a symlink: %q", manifest, linked[0])
}

// validateFileSizeLimits is load_exceptions' cross-check: an exception for a file
// that is also in the baseline must state a limit ABOVE its ceiling, so the two
// manifests can never disagree about which is the wider.
func validateFileSizeLimits(ceilings, exceptions map[string]int) error {
	for path, limit := range exceptions {
		if ceiling, inBaseline := ceilings[path]; inBaseline && limit <= ceiling {
			return fmt.Errorf("%s: exception for a baseline file must state a limit above its ceiling in %s: %q", fileSizeExceptionFile, fileSizeBaselineFile, path)
		}
	}
	return nil
}
