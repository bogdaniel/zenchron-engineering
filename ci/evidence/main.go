// Command evidence selects and runs the T1 impact-directed evidence for the
// changes on this branch. See docs/ci-evidence.md.
//
//	go run ./ci/evidence -base origin/main         # plan, then run T1
//	go run ./ci/evidence -base origin/main -race   # plan, then run the race escalation
//	go run ./ci/evidence -base origin/main -plan   # plan only
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const testTimeout = "30m"

var testFunc = regexp.MustCompile(`(?m)^func (Test\w*)\(\w+ \*testing\.T\)`)

func main() {
	base := flag.String("base", "origin/main", "revision the branch is compared against (merge base)")
	planOnly := flag.Bool("plan", false, "print the evidence plan without running it")
	// The race escalation is about four times slower than the plain run, so CI
	// runs it as a parallel job instead of adding it to T1's latency.
	race := flag.Bool("race", false, "run only the -race escalation of high-risk domains")
	flag.Parse()
	if err := run(*base, *planOnly, *race); err != nil {
		fmt.Fprintln(os.Stderr, "evidence:", err)
		os.Exit(1)
	}
}

func run(base string, planOnly, race bool) error {
	reg, err := LoadRegistry()
	if err != nil {
		return err
	}
	root, err := output("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repo, err := ScanRepo(root, reg.FocusedPackage)
	if err != nil {
		return err
	}
	diff, err := output(root, "git", "diff", "--name-only", "--no-renames", base+"...HEAD")
	if err != nil {
		return err
	}
	changed := strings.Fields(diff)
	plan := PlanEvidence(reg, repo, changed)
	fmt.Printf("T1 evidence for %d changed path(s) since %s:\n", len(changed), base)
	for _, reason := range plan.Reasons {
		fmt.Println("  " + reason)
	}
	fmt.Printf("./%s: whole=%v, %d selected test(s), %d under -race\n", reg.FocusedPackage, plan.Whole, len(plan.Run), len(plan.Race))
	if planOnly {
		return nil
	}
	if race {
		if len(plan.Race) == 0 {
			return nil
		}
		return execute(root, [][]string{{"go", "test", "-race", "-timeout", testTimeout, "-run", RunPattern(plan.Race), "./" + reg.FocusedPackage}})
	}
	return execute(root, t1Commands(reg.FocusedPackage, repo.PackageDirs, plan))
}

func t1Commands(focused string, packageDirs []string, plan Plan) [][]string {
	var others []string
	for _, dir := range packageDirs {
		if dir != focused {
			others = append(others, "./"+dir)
		}
	}
	commands := [][]string{append([]string{"go", "test", "-timeout", testTimeout}, others...)}
	switch {
	case plan.Whole:
		commands = append(commands, []string{"go", "test", "-timeout", testTimeout, "./" + focused})
	case len(plan.Run) > 0:
		commands = append(commands, []string{"go", "test", "-timeout", testTimeout, "-run", RunPattern(plan.Run), "./" + focused})
	}
	return commands
}

// execute runs every command even after a failure, so one red check does not
// hide what the rest of the evidence says.
func execute(root string, commands [][]string) error {
	failed := 0
	for _, args := range commands {
		fmt.Printf("::group::%s\n", summarize(args))
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, os.Stdout, os.Stderr
		err := cmd.Run()
		fmt.Println("::endgroup::")
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED: %s: %v\n", summarize(args), err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d evidence command(s) failed", failed, len(commands))
	}
	return nil
}

func summarize(args []string) string {
	line := strings.Join(args, " ")
	if len(line) > 160 {
		line = line[:160] + "..."
	}
	return line
}

// ScanRepo collects the facts PlanEvidence needs from the checkout at root.
func ScanRepo(root, focused string) (Repo, error) {
	repo := Repo{TestsInFile: map[string][]string{}}
	files, err := filepath.Glob(filepath.Join(root, focused, "*_test.go"))
	if err != nil {
		return repo, err
	}
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			return repo, err
		}
		tests := []string{}
		for _, m := range testFunc.FindAllSubmatch(source, -1) {
			tests = append(tests, string(m[1]))
		}
		repo.TestsInFile[focused+"/"+filepath.Base(file)] = tests
	}
	module, err := output(root, "go", "list", "-m")
	if err != nil {
		return repo, err
	}
	deps, err := output(root, "go", "list", "-deps", "-f", "{{.ImportPath}}", "./"+focused)
	if err != nil {
		return repo, err
	}
	repo.DepDirs = moduleDirs(module, deps, focused)
	all, err := output(root, "go", "list", "./...")
	if err != nil {
		return repo, err
	}
	repo.PackageDirs = moduleDirs(module, all, "")
	return repo, nil
}

func moduleDirs(module, importPaths, exclude string) []string {
	var dirs []string
	for _, p := range strings.Fields(importPaths) {
		dir, ok := strings.CutPrefix(p, module+"/")
		if ok && dir != exclude {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
