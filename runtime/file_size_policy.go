package runtime

// The repository's file-size policy (AGENTS.md section 4), checked against an
// EXACT git tree. This is a port of scripts/check_file_sizes.py's counting and
// classification, kept in step with it by TestFileSizePolicyMatchesThePythonChecker.
//
// It exists because the Python checker needs a working tree and `git ls-files`,
// and the assurance sandbox masks .git. Run in the controller, on the host-side
// assurance checkout, it needs neither, and it reads the exact tree rather than
// a directory a producer could have touched.
//
// THE RULES COME FROM THE BASE REVISION, never from the candidate. The baseline
// ceilings and the approved exceptions are human governance; a candidate that
// edits file-size-baseline.tsv or file-size-exceptions.tsv to make itself pass
// is not honoured, so it fails on the file it grew. Nothing here writes
// either manifest.

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	fileSizeHardCap       = 1000
	fileSizeBaselineFile  = "file-size-baseline.tsv"
	fileSizeExceptionFile = "file-size-exceptions.tsv"
	// FileSizePolicyClaimID is the one claim id the runtime recognizes. A policy
	// that lists it in required_claims (evidence class automated_test) is what
	// turns this check on for a contract; a contract compiled without it is not
	// checked, so a run keeps exactly the policy it was compiled under.
	FileSizePolicyClaimID = "claim-file-size-policy"
)

// fileSizeSuffixes, and the directories excluded from counting, are the
// checker's own lists.
var fileSizeSuffixes = map[string]bool{
	".go": true, ".rs": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".cs": true, ".php": true, ".rb": true, ".swift": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".hpp": true, ".vue": true, ".svelte": true, ".css": true, ".scss": true, ".html": true, ".sql": true, ".sh": true,
}

var fileSizeExcludedDirs = map[string]bool{"vendor": true, "node_modules": true, ".venv": true}

// generatedHeader is the checker's regular expression, applied to the first
// 2048 bytes exactly as it is there.
var generatedHeader = regexp.MustCompile(
	`(?im)^[ \t]*(?://|#|/\*+|\*|<!--|--)[ \t]*(?:code[ \t]+)?generated\b[^\r\n]*\bdo not edit\b`)

// FileSizeViolation is one file the policy refuses.
type FileSizeViolation struct {
	Path  string
	Lines int
	// Limit is the ceiling or exception limit that applies, or the hard cap.
	Limit  int
	Source string // "hard cap", "baseline ceiling" or "exception limit"
}

func (v FileSizeViolation) String() string {
	return fmt.Sprintf("FAIL %d %q: exceeds %s %d", v.Lines, v.Path, v.Source, v.Limit)
}

// fileSizeSuffix is Python's PurePosixPath.suffix: the last dot-suffix of the
// final component, empty for a name that only starts with a dot.
func fileSizeSuffix(name string) string {
	base := path.Base(name)
	dot := strings.LastIndex(base, ".")
	if dot <= 0 || dot == len(base)-1 {
		return ""
	}
	return base[dot:]
}

func isFileSizeCounted(name string) bool {
	if !fileSizeSuffixes[fileSizeSuffix(name)] {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if fileSizeExcludedDirs[part] {
			return false
		}
	}
	return true
}

// countFileLines is the physical line count, and false for generated content.
func countFileLines(data []byte) (int, bool) {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	if generatedHeader.Match(head) {
		return 0, false
	}
	lines := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines, true
}

// manifestAtBase says whether the base revision has the manifest, and only
// answers "absent" when git CONFIRMS it. `git cat-file -e rev:path` exits 128
// for a missing path and for a corrupt or unreadable object alike, so its exit
// status cannot tell them apart; listing the tree entry can: success with no
// entry is confirmed absence, and any failure is a failed lookup. A regular
// file is the only thing a manifest may be (the checker refuses a symlink).
func manifestAtBase(dir, base, name string) (present bool, err error) {
	out, err := gitOutput(dir, "ls-tree", "-z", "--full-tree", base, "--", name)
	if err != nil {
		return false, fmt.Errorf("the file-size policy could not be read: looking up %s at base revision %s failed (%w); refusing to treat it as absent", name, base, err)
	}
	if out == "" {
		return false, nil
	}
	meta, _, _ := strings.Cut(out, "\t")
	if fields := strings.Fields(meta); len(fields) != 3 || fields[1] != "blob" || fields[0] == "120000" {
		return false, fmt.Errorf("%s at base revision %s is not a regular file", name, base)
	}
	return true, nil
}

// readFileSizeManifest reads one manifest at the base revision. A manifest the
// base CONFIRMABLY does not have is empty, as in the checker; one it has but
// that cannot be looked up, read or parsed is an error, never a silently empty
// policy.
func readFileSizeManifest(dir, base, name string, columns int) (map[string]int, error) {
	present, err := manifestAtBase(dir, base, name)
	if err != nil {
		return nil, err
	}
	if !present {
		return map[string]int{}, nil
	}
	data, err := gitOutput(dir, "show", base+":"+name)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", name, base, err)
	}
	limits := map[string]int{}
	for number, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != columns {
			return nil, fmt.Errorf("malformed %s entry at line %d", name, number+1)
		}
		for _, field := range fields {
			if strings.TrimSpace(field) == "" {
				return nil, fmt.Errorf("malformed %s entry at line %d", name, number+1)
			}
		}
		limit, err := strconv.Atoi(fields[1])
		if err != nil || limit <= fileSizeHardCap || strconv.Itoa(limit) != fields[1] {
			return nil, fmt.Errorf("%s line %d: limit must be an integer above %d", name, number+1, fileSizeHardCap)
		}
		limits[fields[0]] = limit
	}
	return limits, nil
}

// CheckFileSizePolicy reports every file of tree that the policy refuses, using
// the ceilings and exceptions of the base revision. dir is any repository that
// holds both objects.
func CheckFileSizePolicy(dir, base, tree string) ([]FileSizeViolation, error) {
	if base == "" || tree == "" {
		return nil, errors.New("a base revision and an exact tree are required")
	}
	// A base that cannot be read is an error. Without this, the "manifest not
	// present at base" reading above would turn a missing base into an empty,
	// permissive policy.
	if _, err := runGit(dir, "cat-file", "-e", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("base revision %s is not available to read the size policy from", base)
	}
	ceilings, err := readFileSizeManifest(dir, base, fileSizeBaselineFile, 2)
	if err != nil {
		return nil, err
	}
	exceptions, err := readFileSizeManifest(dir, base, fileSizeExceptionFile, 3)
	if err != nil {
		return nil, err
	}
	blobs, err := countedBlobs(dir, tree)
	if err != nil {
		return nil, err
	}
	contents, err := readBlobs(dir, blobs)
	if err != nil {
		return nil, err
	}
	var violations []FileSizeViolation
	for _, blob := range blobs {
		lines, counted := countFileLines(contents[blob.id])
		if !counted || lines <= fileSizeHardCap {
			continue
		}
		// The checker's order: an exception first, then the baseline ceiling.
		if limit, ok := exceptions[blob.path]; ok {
			if lines > limit {
				violations = append(violations, FileSizeViolation{blob.path, lines, limit, "exception limit"})
			}
			continue
		}
		if limit, ok := ceilings[blob.path]; ok {
			if lines > limit {
				violations = append(violations, FileSizeViolation{blob.path, lines, limit, "baseline ceiling"})
			}
			continue
		}
		violations = append(violations, FileSizeViolation{blob.path, lines, fileSizeHardCap, "hard cap"})
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].Path < violations[j].Path })
	return violations, nil
}

// countedBlobs lists the regular source files of tree with their sizes.
// Symlinks, submodules and anything that is not a blob are skipped, as the
// checker skips them.
func countedBlobs(dir, tree string) ([]stagedBlob, error) {
	out, err := gitOutput(dir, "ls-tree", "-r", "-l", "-z", "--full-tree", tree)
	if err != nil {
		return nil, fmt.Errorf("list tree %s: %w", tree, err)
	}
	var blobs []stagedBlob
	for _, entry := range strings.Split(out, "\x00") {
		meta, name, found := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 4 || fields[1] != "blob" || fields[0] == "120000" || !isFileSizeCounted(name) {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unreadable size for %q: %w", name, err)
		}
		blobs = append(blobs, stagedBlob{path: name, id: fields[2], size: size})
	}
	return blobs, nil
}
