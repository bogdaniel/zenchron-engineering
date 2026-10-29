package runtime

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func tarOf(t *testing.T, headers []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for index := range headers {
		header := headers[index]
		if header.Typeflag == tar.TypeReg || header.Typeflag == 0 {
			header.Typeflag, header.Size = tar.TypeReg, int64(len(bodies[index]))
		}
		header.Mode = 0o644
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			_, _ = writer.Write(bodies[index])
		}
	}
	_ = writer.Close()
	return archive.Bytes()
}
func manifestFor(files map[string][]byte) map[string]string {
	manifest := map[string]string{}
	for name, body := range files {
		sum := sha256.Sum256(body)
		manifest[name] = hex.EncodeToString(sum[:])
	}
	return manifest
}

// What the tests write is bounded and shaped before any of it is kept.
func TestTheRetainedArtifactsAreBoundedPlainAndMatchTheManifest(t *testing.T) {
	test := browserSelection[0]
	reg := func(name string, size int) tar.Header {
		return tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(size)}
	}
	filler := func(n int) []byte { return bytes.Repeat([]byte("x"), n) }
	many := make([]tar.Header, browserRetainedMaxFiles+1)
	manyBodies := make([][]byte, len(many))
	manyFiles := map[string][]byte{}
	for i := range many {
		name := test + "/f" + strconv.Itoa(i) + ".png"
		many[i], manyBodies[i], manyFiles[name] = reg(name, 1), filler(1), filler(1)
	}
	big := browserRetainedMaxFileBytes + 1
	var aggregate []tar.Header
	var aggregateBodies [][]byte
	aggregateFiles := map[string][]byte{}
	for i := 0; i < 4; i++ {
		name := test + "/a" + strconv.Itoa(i) + ".png"
		aggregate = append(aggregate, reg(name, 900<<10))
		aggregateBodies = append(aggregateBodies, filler(900<<10))
		aggregateFiles[name] = filler(900 << 10)
	}
	one := map[string][]byte{test + "/a.png": filler(1)}
	for name, tc := range map[string]struct {
		archive  []byte
		manifest map[string]string
	}{
		"too many files retained":          {tarOf(t, many, manyBodies), manifestFor(manyFiles)},
		"a file above the bound":           {tarOf(t, []tar.Header{reg(test+"/big.png", big)}, [][]byte{filler(big)}), manifestFor(map[string][]byte{test + "/big.png": filler(big)})},
		"above the aggregate":              {tarOf(t, aggregate, aggregateBodies), manifestFor(aggregateFiles)},
		"a symbolic link":                  {tarOf(t, []tar.Header{{Name: test + "/a.png", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}, [][]byte{nil}), manifestFor(one)},
		"a hard link":                      {tarOf(t, []tar.Header{{Name: test + "/a.png", Typeflag: tar.TypeLink, Linkname: test + "/other"}}, [][]byte{nil}), manifestFor(one)},
		"a device":                         {tarOf(t, []tar.Header{{Name: test + "/a.png", Typeflag: tar.TypeChar}}, [][]byte{nil}), manifestFor(one)},
		"a fifo":                           {tarOf(t, []tar.Header{{Name: test + "/a.png", Typeflag: tar.TypeFifo}}, [][]byte{nil}), manifestFor(one)},
		"a parent traversal":               {tarOf(t, []tar.Header{reg(test+"/../../escape.png", 1)}, [][]byte{filler(1)}), manifestFor(one)},
		"an absolute path":                 {tarOf(t, []tar.Header{reg("/etc/escape.png", 1)}, [][]byte{filler(1)}), manifestFor(one)},
		"a file the manifest never listed": {tarOf(t, []tar.Header{reg(test+"/unlisted.png", 1)}, [][]byte{filler(1)}), manifestFor(one)},
		"a duplicate":                      {tarOf(t, []tar.Header{reg(test+"/a.png", 1), reg(test+"/a.png", 1)}, [][]byte{filler(1), filler(1)}), manifestFor(one)},
		"bytes the manifest does not hash": {tarOf(t, []tar.Header{reg(test+"/a.png", 1)}, [][]byte{[]byte("y")}), manifestFor(one)},
	} {
		scratch := t.TempDir()
		dest := filepath.Join(scratch, "artifacts")
		if err := os.Mkdir(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := writeRetainedArtifacts(tc.archive, dest, tc.manifest)
		if !errors.Is(err, errBrowserArtifactsRefused) {
			t.Errorf("%s was not refused: %v", name, err)
		}
		// Nothing escaped the artifact directory, whatever was or was not kept.
		if entries, _ := os.ReadDir(scratch); len(entries) != 1 {
			t.Errorf("%s wrote outside the artifact directory: %v", name, entries)
		}
	}

	// A plain, in-bound set that matches its manifest is kept, hashed as copied.
	dest := t.TempDir()
	files := map[string][]byte{test + "/shot.png": []byte("12345"), browserSelection[1] + "/dir/shot.png": []byte("abc")}
	set, err := writeRetainedArtifacts(tarOf(t, []tar.Header{reg(test+"/shot.png", 5), reg(browserSelection[1]+"/dir/shot.png", 3)}, [][]byte{[]byte("12345"), []byte("abc")}), dest, manifestFor(files))
	if err != nil || set.retained != 2 || set.bytes != 8 {
		t.Fatalf("a plain set was not kept: %+v %v", set, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, test, "shot.png")); string(data) != "12345" {
		t.Fatalf("the kept screenshot is wrong: %q", data)
	}
	if digest, silent := manifestDigest(manifestFor(files), browserSelection); digest == "" || len(silent) != 2 {
		t.Fatalf("digest=%q silent=%v", digest, silent)
	}
}

// The manifest covers EVERYTHING the tests wrote, retained or not, and anything
// the count cannot account for refuses the set.
func TestTheManifestAccountsForEveryFileTheTestsWrote(t *testing.T) {
	good := "" + strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/a.png\n" + browserManifestCount + " 1\n"
	if manifest, err := parseBrowserManifest([]byte(good)); err != nil || len(manifest) != 1 {
		t.Fatalf("a good manifest was refused: %v", err)
	}
	var tooMany strings.Builder
	for i := 0; i <= browserWrittenMaxFiles; i++ {
		tooMany.WriteString(strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/f" + strconv.Itoa(i) + ".png\n")
	}
	for name, body := range map[string]string{
		"a file that is not listed (count disagrees)": strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/a.png\n" + browserManifestCount + " 2\n",
		"a name outside the plain set":                strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/a b.png\n" + browserManifestCount + " 1\n",
		"a traversal":                                 strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/../x.png\n" + browserManifestCount + " 1\n",
		"a file outside any test":                     strings.Repeat("a", 64) + "  ./x.png\n" + browserManifestCount + " 1\n",
		"a duplicate":                                 good[:len(good)-len(browserManifestCount+" 1\n")] + good[:len(good)-len(browserManifestCount+" 1\n")] + browserManifestCount + " 2\n",
		"too many files written":                      tooMany.String() + browserManifestCount + " " + strconv.Itoa(browserWrittenMaxFiles+1) + "\n",
		"too deep":                                    strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/a/b/c/d/e.png\n" + browserManifestCount + " 1\n",
	} {
		if _, err := parseBrowserManifest([]byte(body)); !errors.Is(err, errBrowserArtifactsRefused) {
			t.Errorf("%s was not refused: %v", name, err)
		}
	}
	if _, err := parseBrowserManifest([]byte(strings.Repeat("a", 64) + "  ./" + browserSelection[0] + "/a.png\n")); !errors.Is(err, errBrowserArtifactsMissing) {
		t.Errorf("a manifest with no count concluded something: %v", err)
	}
}

// Over-limit output and unreadable output are classified differently: a tree
// that broke the bounds is the exercised code's doing and is judged, however
// green its tests; one that could not be read concludes nothing. Neither is
// evidence.
func TestOverLimitArtifactOutputIsJudgedAndUnreadableOutputIsInconclusive(t *testing.T) {
	over := screenshotsForEveryTest()
	over[browserSelection[0]+"/huge.png"] = bytes.Repeat([]byte("x"), browserRetainedMaxFileBytes+1)
	judged := newBrowserDocker(map[int][]byte{3: browserStdout(t, allBrowserTests("pass"), over)}, nil, nil)
	result, err, _ := browserRun(t, judged, true)
	checks := browserChecks(result)
	if err != nil || result.Passed || result.FailureClass != FailureVerification || len(checks) != 1 || checks[0].Outcome != CheckOutcomeFailed {
		t.Fatalf("over-limit output was not judged: passed=%v class=%v err=%v checks=%+v", result.Passed, result.FailureClass, err, result.Checks)
	}
	good := browserStdout(t, allBrowserTests("pass"), screenshotsForEveryTest())
	for name, stdout := range map[string][]byte{
		"a cut-off export":    good[:len(good)-len(browserArtifactsEnd)-8],
		"undecodable bytes":   append(goTestJSON(allBrowserTests("pass")), exportedSections(browserManifestCount+" 0\n", "!!!not base64!!!")...),
		"no export section":   goTestJSON(allBrowserTests("pass")),
		"a truncated archive": append(goTestJSON(allBrowserTests("pass")), exportedSections(browserManifestCount+" 0\n", base64.StdEncoding.EncodeToString([]byte("not a tar archive at all")))...),
	} {
		result, err, _ := browserRun(t, newBrowserDocker(map[int][]byte{3: stdout}, nil, nil), true)
		if err == nil || result.Passed || len(browserChecks(result)) != 0 {
			t.Errorf("%s became evidence: passed=%v err=%v checks=%+v", name, result.Passed, err, result.Checks)
		}
	}
}
