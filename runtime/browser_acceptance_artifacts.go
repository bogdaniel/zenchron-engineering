package runtime

// What the browser tests write is bounded where it is written and again where
// it is read.
//
// Candidate-influenced code runs in the browser container (the application
// under test is the candidate's), so a writable host directory would let it
// fill the controller's disk. /artifacts is therefore a size- and
// inode-limited tmpfs INSIDE the container: the kernel refuses the write that
// would exceed it, and the test that wrote it fails. No host path is writable.
//
// A real run writes more screenshots than the output channel can carry, so the
// export has two tiers, both streamed on stdout by the runtime-owned end of the
// workload:
//
//   - a MANIFEST of every regular file the tests wrote (path and SHA-256), with
//     a count of everything that is not a directory so that an unlistable file
//     is visible. It is the digest of this manifest that the record carries, and
//     the per-test "wrote something" check reads it;
//   - a bounded set of the files themselves, as a tar, for review. Each retained
//     file must match the manifest's hash for it.
//
// Everything is turned into host state here, not trusted: bounded entry counts,
// per-file and aggregate size, regular files only, every path inside the
// intended tree, exclusive creation under directories checked to be real, and
// hashing as the bytes are copied. A set that breaks a bound or a rule is
// refused as a whole; one that cannot be read at all concludes nothing.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	browserManifestBegin  = "ZENCHRON-MANIFEST-BEGIN"
	browserManifestEnd    = "ZENCHRON-MANIFEST-END"
	browserManifestCount  = "ZENCHRON-COUNT"
	browserArtifactsBegin = "ZENCHRON-ARTIFACTS-BEGIN"
	browserArtifactsEnd   = "ZENCHRON-ARTIFACTS-END"

	// What the tests may write at all (the tmpfs the kernel enforces) and how
	// many of those files the manifest may name.
	browserWrittenMaxFiles = 512
	browserArtifactsTmpfs  = "/artifacts:rw,nosuid,nodev,noexec,size=32m,nr_inodes=1024,mode=0777"

	// What is retained on the host for review.
	browserRetainedMaxFiles     = 64
	browserRetainedMaxFileBytes = 1 << 20
	browserRetainedMaxBytes     = 3 << 20
	browserArtifactMaxDepth     = 5
)

var (
	// errBrowserArtifactsMissing: the export could not be read at all (no
	// section, a cut-off stream, undecodable bytes). Nothing is concluded.
	errBrowserArtifactsMissing = errors.New("the browser artifacts were not exported intact")
	// errBrowserArtifactsRefused: the export was read and broke a bound or a
	// rule. That is something the exercised code did, so it is judged.
	errBrowserArtifactsRefused = errors.New("the browser artifacts broke the artifact bounds")

	browserManifestLine = regexp.MustCompile(`^([0-9a-f]{64})  \./(Test[A-Za-z0-9_]+)/([A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)*)$`)
)

type browserArtifactSet struct {
	// digest covers every file the tests wrote, as the manifest names them.
	digest string
	// written is the number of files the tests wrote; retained and bytes are
	// what was kept on the host.
	written, retained int
	bytes             int64
	// silent names the selected tests that wrote no file.
	silent []string
}

// section returns the text between two marker lines, using the LAST begin
// marker so output printed earlier cannot stand in for the runtime's own.
func section(stdout []byte, begin, end string) (before, body []byte, ok bool) {
	start := bytes.LastIndex(stdout, []byte("\n"+begin+"\n"))
	if start < 0 {
		return stdout, nil, false
	}
	rest := stdout[start+len(begin)+2:]
	stop := bytes.Index(rest, []byte("\n"+end))
	if stop < 0 {
		return stdout, nil, false
	}
	return stdout[:start], rest[:stop], true
}

// collectBrowserArtifacts reads the manifest and the retained files out of the
// container's stdout, keeps the retained files under dest, and returns stdout
// without the (large) sections, so the stored transcript stays readable.
func collectBrowserArtifacts(stdout []byte, dest string, selection []string) ([]byte, browserArtifactSet, error) {
	before, manifestBody, ok := section(stdout, browserManifestBegin, browserManifestEnd)
	if !ok {
		return stdout, browserArtifactSet{}, fmt.Errorf("%w: no manifest", errBrowserArtifactsMissing)
	}
	_, archiveBody, ok := section(stdout, browserArtifactsBegin, browserArtifactsEnd)
	if !ok {
		return before, browserArtifactSet{}, fmt.Errorf("%w: no retained files section", errBrowserArtifactsMissing)
	}
	manifest, err := parseBrowserManifest(manifestBody)
	if err != nil {
		return before, browserArtifactSet{}, err
	}
	encoded := bytes.Join(bytes.Fields(archiveBody), nil)
	if ceiling := (browserRetainedMaxBytes+browserRetainedMaxFiles*1536+8192)*4/3 + 16; len(encoded) > ceiling {
		return before, browserArtifactSet{}, fmt.Errorf("%w: the retained export is %d encoded bytes", errBrowserArtifactsRefused, len(encoded))
	}
	archive := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, err := base64.StdEncoding.Decode(archive, encoded)
	if err != nil {
		return before, browserArtifactSet{}, fmt.Errorf("%w: %v", errBrowserArtifactsMissing, err)
	}
	set, err := writeRetainedArtifacts(archive[:n], dest, manifest)
	set.written = len(manifest)
	set.digest, set.silent = manifestDigest(manifest, selection)
	if err == nil {
		err = keepManifest(dest, manifest)
	}
	note := fmt.Sprintf("\n[artifacts: %d written by the tests, %d retained (%d bytes), manifest sha256 %s]\n", set.written, set.retained, set.bytes, set.digest)
	return append(append([]byte{}, before...), note...), set, err
}

// parseBrowserManifest reads "<sha256>  ./<test>/<path>" lines and the final
// count of non-directory entries. A name outside the plain character set is not
// listed, which makes the count disagree, which refuses the set.
func parseBrowserManifest(body []byte) (map[string]string, error) {
	manifest := map[string]string{}
	counted := -1
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if value, found := strings.CutPrefix(line, browserManifestCount+" "); found {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: unreadable count", errBrowserArtifactsMissing)
			}
			counted = n
			continue
		}
		match := browserManifestLine.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("%w: a written file has a name outside the plain set", errBrowserArtifactsRefused)
		}
		name := match[2] + "/" + match[3]
		for _, part := range strings.Split(name, "/") {
			if part == "." || part == ".." {
				return nil, fmt.Errorf("%w: %q is not a plain path", errBrowserArtifactsRefused, name)
			}
		}
		if len(strings.Split(name, "/")) > browserArtifactMaxDepth {
			return nil, fmt.Errorf("%w: %q is nested too deeply", errBrowserArtifactsRefused, name)
		}
		if _, duplicate := manifest[name]; duplicate {
			return nil, fmt.Errorf("%w: %q is listed twice", errBrowserArtifactsRefused, name)
		}
		if len(manifest) >= browserWrittenMaxFiles {
			return nil, fmt.Errorf("%w: more than %d files written", errBrowserArtifactsRefused, browserWrittenMaxFiles)
		}
		manifest[name] = match[1]
	}
	switch {
	case counted < 0:
		return nil, fmt.Errorf("%w: no count", errBrowserArtifactsMissing)
	case counted != len(manifest):
		return nil, fmt.Errorf("%w: %d entries that are not directories, %d listed regular files", errBrowserArtifactsRefused, counted, len(manifest))
	}
	return manifest, nil
}

// manifestDigest is the digest of every file the tests wrote, and the selected
// tests that wrote none.
func manifestDigest(manifest map[string]string, selection []string) (string, []string) {
	lines := make([]string, 0, len(manifest))
	perTest := map[string]int{}
	for name, hash := range manifest {
		lines = append(lines, name+" "+hash)
		perTest[strings.SplitN(name, "/", 2)[0]]++
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	var silent []string
	for _, name := range selection {
		if perTest[name] == 0 {
			silent = append(silent, name)
		}
	}
	return hex.EncodeToString(sum[:]), silent
}

// browserManifestKept is where the full manifest is kept beside the retained
// files, so the record's digest can be recomputed from what is on the host.
const browserManifestKept = "MANIFEST.sha256"

func keepManifest(dest string, manifest map[string]string) error {
	lines := make([]string, 0, len(manifest))
	for name, hash := range manifest {
		lines = append(lines, hash+"  ./"+name)
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(dest, browserManifestKept), []byte(strings.Join(lines, "\n")+"\n"), 0600)
}

// writeRetainedArtifacts keeps the exported files under dest. Each must be a
// regular file the manifest lists, within the retention bounds, and must hash to
// the manifest's value for it.
func writeRetainedArtifacts(archive []byte, dest string, manifest map[string]string) (browserArtifactSet, error) {
	var set browserArtifactSet
	refuse := func(format string, args ...any) (browserArtifactSet, error) {
		return set, fmt.Errorf("%w: %s", errBrowserArtifactsRefused, fmt.Sprintf(format, args...))
	}
	seen := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return set, nil
		}
		if err != nil {
			return set, fmt.Errorf("%w: %v", errBrowserArtifactsMissing, err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return refuse("%q is not a regular file (type %q)", header.Name, string(header.Typeflag))
		}
		expected, listed := manifest[name]
		switch {
		case !listed:
			return refuse("%q is not a file the manifest lists", header.Name)
		case seen[name]:
			return refuse("%q appears twice", name)
		case set.retained+1 > browserRetainedMaxFiles:
			return refuse("more than %d files retained", browserRetainedMaxFiles)
		case header.Size < 0 || header.Size > browserRetainedMaxFileBytes:
			return refuse("%q is %d bytes, above the %d byte file bound", name, header.Size, browserRetainedMaxFileBytes)
		case set.bytes+header.Size > browserRetainedMaxBytes:
			return refuse("the retained files exceed the %d byte aggregate bound", browserRetainedMaxBytes)
		}
		parent, err := realDirectory(dest, path.Dir(name))
		if err != nil {
			return refuse("%q: %v", name, err)
		}
		file, err := os.OpenFile(filepath.Join(parent, path.Base(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return set, err
		}
		hash := sha256.New()
		copied, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, header.Size))
		if closeErr := file.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil || copied != header.Size {
			return set, fmt.Errorf("%w: %q was cut short", errBrowserArtifactsMissing, name)
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return refuse("%q does not match the manifest's hash for it", name)
		}
		seen[name] = true
		set.retained++
		set.bytes += copied
	}
}
