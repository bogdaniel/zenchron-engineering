package runtime

// The image scripts decide what an operator pins, so their failure paths are
// tested without Docker: a fake `docker` on PATH stands in for the daemon, and
// fakes for apt-get and go stand in for the tools inside the container.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runScript(t *testing.T, path string, path0 string, env []string, args ...string) (string, string, error) {
	t.Helper()
	command := exec.Command("sh", append([]string{path}, args...)...)
	command.Env = append(os.Environ(), append([]string{"PATH=" + path0 + ":" + os.Getenv("PATH")}, env...)...)
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// A failed apt simulation must fail the resolver. Output parsed from a failed
// simulation is a short package list that would look like a result.
func TestAFailedAptSimulationDoesNotProduceAPackageList(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(assuranceImageDirectory, "simulate-install.sh")
	cases := map[string]struct {
		apt     string
		success bool
	}{
		"apt fails after printing packages":  {"echo 'Inst libfoo (1.0 Debian:13)'\necho 'Inst chromium (156 Debian:13)'\nexit 100", false},
		"apt succeeds without chromium":      {"echo 'Inst libfoo (1.0 Debian:13)'", false},
		"apt succeeds and resolves chromium": {"echo 'Inst libfoo (1.0 Debian:13)'\necho 'Inst chromium (156 Debian:13)'\necho 'Inst libold [1] (2.0 Debian:13)'", true},
	}
	for name, c := range cases {
		writeExecutable(t, filepath.Join(bin, "apt-get"), c.apt)
		stdout, _, err := runScript(t, script, bin, nil)
		if c.success {
			if err != nil || stdout != "chromium=156\nlibfoo=1.0\nlibold=2.0\n" {
				t.Errorf("%s: want a sorted list, got %q (%v)", name, stdout, err)
			}
			continue
		}
		if err == nil || stdout != "" {
			t.Errorf("%s: the resolver must fail and print nothing, got %q (%v)", name, stdout, err)
		}
	}
}

// fakeGo answers `go env [names...]` and `go version` from the file named by
// FAKE_GOENV, one KEY=VALUE per line, so the script decides which names are read.
const fakeGo = `case "$1" in
version) echo "go version go1.25.14 linux/arm64" ;;
env) shift
  if [ $# -eq 0 ]; then cat "$FAKE_GOENV"; exit; fi
  for name in "$@"; do sed -n "s/^$name=//p" "$FAKE_GOENV"; done ;;
esac
`

// fakeDocker runs the script handed to `docker run ... <image> -c <script>`
// locally against the fake go, with the image's own environment file.
func fakeDocker(t *testing.T, dir string) {
	writeExecutable(t, filepath.Join(dir, "go"), fakeGo)
	writeExecutable(t, filepath.Join(dir, "docker"), `image=; script=
while [ $# -gt 0 ]; do
  case "$1" in -c) script=$2; shift ;; sha256:*|ref-*|img-*) image=$1 ;; esac
  shift
done
case "$script" in *dpkg-query*) exit 0 ;; esac
[ -f "$FAKE_ENVS/$image" ] || exit 7
FAKE_GOENV="$FAKE_ENVS/$image" exec sh -c "$script"
`)
}

func TestTheGoEnvironmentParityCheckComparesEveryEnvironmentValue(t *testing.T) {
	bin, envs := t.TempDir(), t.TempDir()
	fakeDocker(t, bin)
	base := "GOROOT=/usr/local/go\nGOTOOLCHAIN=local\nGOARCH=arm64\nGOOS=linux\nCGO_ENABLED=1\nGOFLAGS=\nGOGCCFLAGS=-fdebug-prefix-map=/tmp/go-build123=/tmp/go-build\n"
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(envs, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("img-new", base)
	script := filepath.Join(assuranceImageDirectory, "verify.sh")
	verify := func(reference string) error {
		_, _, err := runScript(t, script, bin, []string{"FAKE_ENVS=" + envs}, "img-new", reference)
		return err
	}

	write("ref-same", strings.Replace(base, "go-build123", "go-build999", 1)) // only the per-process path differs
	if err := verify("ref-same"); err != nil {
		t.Fatalf("an identical environment was refused: %v", err)
	}
	for name, change := range map[string][2]string{
		"GOARCH":      {"GOARCH=arm64", "GOARCH=amd64"},
		"CGO_ENABLED": {"CGO_ENABLED=1", "CGO_ENABLED=0"},
		"GOOS":        {"GOOS=linux", "GOOS=darwin"},
		"GOFLAGS":     {"GOFLAGS=", "GOFLAGS=-mod=mod"},
	} {
		write("ref-"+name, strings.Replace(base, change[0], change[1], 1))
		if err := verify("ref-" + name); err == nil {
			t.Errorf("a different %s was accepted as an identical Go environment", name)
		}
	}
	// Two unreadable environments must not compare equal.
	if err := verify("ref-unreadable"); err == nil {
		t.Error("an unreadable reference environment was accepted")
	}
}

func TestTheBuildRecordNamesEveryInputFile(t *testing.T) {
	bin, work := t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(bin, "docker"), `case "$*" in
  *"--format {{.Id}}"*) echo sha256:abc ;;
  *"--format"*) echo arm64 ;;
  "image inspect"*) exit 1 ;;
  *) echo probe ;;
esac
`)
	record := func(packageJSON string) map[string]string {
		dir := filepath.Join(t.TempDir(), "assurance-image")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"Dockerfile", "build.sh", "package-lock.json", "chromium-packages.txt", "debian-snapshot.sources"} {
			data, err := os.ReadFile(filepath.Join(assuranceImageDirectory, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(work, "record.json")
		os.Remove(out)
		if _, stderr, err := runScript(t, filepath.Join(dir, "build.sh"), bin, nil, "tag-"+t.Name(), out); err != nil {
			t.Fatalf("build.sh: %v: %s", err, stderr)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]string
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("the record is not JSON: %v: %s", err, data)
		}
		return parsed
	}

	original := assuranceImageFile(t, "package.json")
	sum := sha256.Sum256([]byte(original))
	first := record(original)
	if first["package_json_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("package.json is not recorded by its SHA-256: %v", first)
	}
	for _, input := range []string{"package_json_sha256", "lockfile_sha256", "packages_sha256", "snapshot_sha256", "dockerfile_sha256"} {
		if first[input] == "" {
			t.Errorf("the record omits %s", input)
		}
	}
	if changed := record(original + " "); changed["package_json_sha256"] == first["package_json_sha256"] {
		t.Fatal("a changed package.json produced the same recorded hash")
	}
}
