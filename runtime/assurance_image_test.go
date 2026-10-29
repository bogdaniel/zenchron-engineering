package runtime

// The browser-capable assurance image is built from assurance-image/. What the
// verifier trusts is the image an operator pins, so the build that produces it
// must not drift: these tests keep every input pinned and keep the paths the
// runtime requires (browser_acceptance.go) in step with the ones the build makes.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const assuranceImageDirectory = "../assurance-image"

func assuranceImageFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(assuranceImageDirectory, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Both base images are named by immutable digest, and nothing in the build can
// float: a tag alone would let the same Dockerfile build a different image.
func TestTheAssuranceImageBasesArePinnedByDigest(t *testing.T) {
	dockerfile := assuranceImageFile(t, "Dockerfile")
	pinned := regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	bases := map[string]string{}
	for _, line := range strings.Split(dockerfile, "\n") {
		if name, value, found := strings.Cut(strings.TrimPrefix(line, "ARG "), "="); strings.HasPrefix(line, "ARG ") && found {
			bases[name] = value
		}
	}
	for _, name := range []string{"GO_IMAGE", "NODE_IMAGE"} {
		if !pinned.MatchString(bases[name]) {
			t.Fatalf("%s is %q, which is not pinned by a sha256 digest", name, bases[name])
		}
	}
	for _, line := range strings.Split(dockerfile, "\n") {
		if !strings.HasPrefix(line, "FROM ") {
			continue
		}
		if image := strings.Fields(line)[1]; image != "${GO_IMAGE}" && image != "${NODE_IMAGE}" {
			t.Fatalf("%q builds from something other than the two pinned bases", line)
		}
	}
	for _, floating := range []string{":latest", "REPLACE", "TODO", "FIXME", "<"} {
		if strings.Contains(dockerfile, floating) {
			t.Fatalf("the Dockerfile contains %q, so something in it is floating or unresolved", floating)
		}
	}
}

// Chromium and everything it pulls in come from one dated snapshot at exact
// versions, so the package set is a reviewed file, not whatever the archive
// serves on the day of the build.
func TestTheDebianPackagesAreAtExactVersionsFromOneSnapshot(t *testing.T) {
	timestamps := regexp.MustCompile(`/archive/debian(?:-security)?/(\d{8}T\d{6}Z)`).FindAllStringSubmatch(assuranceImageFile(t, "debian-snapshot.sources"), -1)
	if len(timestamps) < 2 {
		t.Fatalf("the snapshot sources name %d dated archives, want the main and security archives", len(timestamps))
	}
	for _, found := range timestamps[1:] {
		if found[1] != timestamps[0][1] {
			t.Fatalf("the sources use two snapshots, %s and %s", timestamps[0][1], found[1])
		}
	}

	exact := regexp.MustCompile(`^[a-z0-9][a-z0-9+.\-]*=[0-9][^\s=*]*$`)
	var names []string
	lines := strings.Split(strings.TrimSpace(assuranceImageFile(t, "chromium-packages.txt")), "\n")
	for _, line := range lines {
		if !exact.MatchString(line) {
			t.Fatalf("%q is not a package at an exact version", line)
		}
		names = append(names, strings.SplitN(line, "=", 2)[0])
	}
	slices.Sort(names)
	if !slices.IsSorted(lines) || len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatal("the package list is not sorted (byte order) and free of duplicate packages, so a change to it is not a readable diff")
	}
	for _, required := range []string{"chromium", "chromium-common", "fonts-liberation"} {
		if !slices.Contains(names, required) {
			t.Fatalf("the package list does not pin %s", required)
		}
	}
}

// The Playwright driver is pinned by the lockfile and installed from it.
func TestThePlaywrightDriverIsPinnedByTheLockfile(t *testing.T) {
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	var lock struct {
		Packages map[string]struct {
			Version, Resolved, Integrity string
		} `json:"packages"`
	}
	for name, into := range map[string]any{"package.json": &manifest, "package-lock.json": &lock} {
		if err := json.Unmarshal([]byte(assuranceImageFile(t, name)), into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	exact := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	for name, version := range manifest.Dependencies {
		if !exact.MatchString(version) {
			t.Fatalf("%s is %q, not an exact version", name, version)
		}
		if locked := lock.Packages["node_modules/"+name]; locked.Version != version {
			t.Fatalf("%s is %q in package.json and %q in the lockfile", name, version, locked.Version)
		}
	}
	for path, locked := range lock.Packages {
		if path == "" {
			continue
		}
		if !strings.HasPrefix(locked.Resolved, "https://registry.npmjs.org/") || !strings.HasPrefix(locked.Integrity, "sha512-") {
			t.Fatalf("%s is not resolved to the registry with an integrity hash: %+v", path, locked)
		}
	}
	if dockerfile := assuranceImageFile(t, "Dockerfile"); !strings.Contains(dockerfile, "npm ci --ignore-scripts") {
		t.Fatal("the driver is not installed with npm ci --ignore-scripts, so the lockfile would not be what is installed")
	}
}

// The runtime requires the Playwright module and Chrome at fixed paths; the
// build must be what creates exactly those.
func TestTheImageBuildCreatesTheExactPathsTheRuntimeRequires(t *testing.T) {
	dockerfile := assuranceImageFile(t, "Dockerfile")
	for _, want := range []string{
		"WORKDIR " + filepath.Dir(browserPlaywrightPath),
		"ln -s node_modules/playwright-core/index.mjs " + filepath.Base(browserPlaywrightPath),
		"mkdir -p " + filepath.Dir(browserChromePath),
		"ln -s /usr/bin/chromium " + browserChromePath,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("the Dockerfile does not contain %q, so %s and %s would not both exist", want, browserPlaywrightPath, browserChromePath)
		}
	}
}

// TestTheBuiltAssuranceImageProvidesTheBrowserPrerequisites runs verify.sh,
// the one place the image's requirements are checked, against a built image
// under the verifier's restrictions. It is opt-in: it needs Docker and a built
// image, and an unset variable is a skip that says so.
//
// ZENCHRON_BROWSER_IMAGE names the image; ZENCHRON_REFERENCE_IMAGE, when set, is
// the image in service today, whose Go environment must be unchanged.
func TestTheBuiltAssuranceImageProvidesTheBrowserPrerequisites(t *testing.T) {
	image := os.Getenv("ZENCHRON_BROWSER_IMAGE")
	if image == "" {
		t.Skip("SKIPPED, NOT PASSED: ZENCHRON_BROWSER_IMAGE names a built browser assurance image; unset")
	}
	arguments := []string{filepath.Join(assuranceImageDirectory, "verify.sh"), image}
	if reference := os.Getenv("ZENCHRON_REFERENCE_IMAGE"); reference != "" {
		arguments = append(arguments, reference)
	}
	output, err := exec.Command("sh", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("the image does not meet the requirements: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "image prerequisites hold") {
		t.Fatalf("verify.sh did not report success:\n%s", output)
	}
}
