// Command evidence selects and runs the T1 impact-directed evidence for the
// changes on this branch. See docs/ci-evidence.md.
//
//	go run ./ci/evidence -base origin/main         # plan, then run T1
//	go run ./ci/evidence -base origin/main -race   # plan, then run the race escalation
//	go run ./ci/evidence -base origin/main -plan   # plan only
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const testTimeout = "30m"

// raceTimeout bounds the -race escalation. A whole-package fallback (for
// example a change to runtime/operations.go, a declared cross-domain hotspot)
// escalates every high-risk domain: about 1,000 runtime tests under -race,
// which does not fit 30m on a hosted runner. The bound still catches a hang.
const raceTimeout = "60m"

func main() {
	var opts options
	flag.StringVar(&opts.base, "base", "origin/main", "revision the branch is compared against (merge base)")
	flag.BoolVar(&opts.planOnly, "plan", false, "print the evidence plan without running it")
	// The race escalation is about four times slower than the plain run, so CI
	// runs it as parallel jobs instead of adding it to T1's latency.
	flag.BoolVar(&opts.race, "race", false, "run only the -race escalation of high-risk domains")
	flag.StringVar(&opts.raceShard, "race-shard", "", "partition the same race selection as part/count, e.g. 1/8")
	flag.StringVar(&opts.shard, "shard", "", "run only part/count of the selected plain runtime tests, e.g. 1/4")
	flag.BoolVar(&opts.packages, "packages", false, "run only the packages other than runtime, and check that sharding sees every runtime test")
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "evidence:", err)
		os.Exit(1)
	}
}

// options select which slice of the T1 evidence one invocation runs. With none
// of -race, -shard and -packages it runs all of it, as a developer does locally;
// CI runs the same evidence as parallel invocations whose union is that run.
type options struct {
	base             string
	planOnly         bool
	race             bool
	raceShard, shard string
	packages         bool
}

func (o options) validate() error {
	switch {
	case o.raceShard != "" && !o.race:
		return fmt.Errorf("-race-shard requires -race")
	case o.race && (o.shard != "" || o.packages), o.shard != "" && o.packages:
		return fmt.Errorf("-race, -shard and -packages each select a different slice of the evidence; pass one")
	// -plan prints and exits before any test runs, so beside a slice it would
	// make that slice's job green having run nothing.
	case o.planOnly && (o.race || o.shard != "" || o.packages):
		return fmt.Errorf("-plan only prints the plan and cannot be combined with -race, -shard or -packages")
	}
	return nil
}

func run(opts options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	reg, err := LoadRegistry()
	if err != nil {
		return err
	}
	costs, err := LoadCosts()
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
	diff, err := output(root, "git", "diff", "--name-only", "--no-renames", opts.base+"...HEAD")
	if err != nil {
		return err
	}
	changed := strings.Fields(diff)
	plan := PlanEvidence(reg, repo, changed)
	fmt.Printf("T1 evidence for %d changed path(s) since %s:\n", len(changed), opts.base)
	for _, reason := range plan.Reasons {
		fmt.Println("  " + reason)
	}
	fmt.Printf("./%s: whole=%v, %d selected test(s), %d under -race\n", reg.FocusedPackage, plan.Whole, len(plan.Run), len(plan.Race))
	if opts.planOnly {
		return nil
	}
	switch {
	case opts.race:
		raceTests, err := ShardOf(plan.Race, costs.Race, orWhole(opts.raceShard))
		if err != nil {
			return err
		}
		if opts.raceShard != "" {
			fmt.Printf("race shard %s: %d of %d selected tests; coverage is the union of all shards\n", opts.raceShard, len(raceTests), len(plan.Race))
		}
		if len(raceTests) == 0 {
			return nil
		}
		return execute(root, [][]string{{"go", "test", "-race", "-json", "-timeout", raceTimeout, "-run", RunPattern(raceTests), "./" + reg.FocusedPackage}})
	case opts.packages:
		return errors.Join(execute(root, packageCommands(reg.FocusedPackage, repo.PackageDirs)), verifyEveryTestIsSeen(root, reg.FocusedPackage, repo))
	case opts.shard != "":
		selected := plan.selected(repo)
		tests, err := ShardOf(selected, costs.Plain, opts.shard)
		if err != nil {
			return err
		}
		fmt.Printf("shard %s: %d of %d selected tests; coverage is the union of all shards\n", opts.shard, len(tests), len(selected))
		if len(tests) == 0 {
			return nil
		}
		return execute(root, [][]string{{"go", "test", "-timeout", testTimeout, "-run", RunPattern(tests), "./" + reg.FocusedPackage}})
	}
	return execute(root, t1Commands(reg.FocusedPackage, repo.PackageDirs, plan))
}

// orWhole turns "no shard requested" into the one-shard partition.
func orWhole(shard string) string {
	if shard == "" {
		return "1/1"
	}
	return shard
}

// packageCommands runs every package but the focused one whole.
func packageCommands(focused string, packageDirs []string) [][]string {
	var others []string
	for _, dir := range packageDirs {
		if dir != focused {
			others = append(others, "./"+dir)
		}
	}
	return [][]string{append([]string{"go", "test", "-timeout", testTimeout}, others...)}
}

func t1Commands(focused string, packageDirs []string, plan Plan) [][]string {
	commands := packageCommands(focused, packageDirs)
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
		tests, err := testFunctions(file)
		if err != nil {
			return repo, err
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

// testFunctions lists the top-level func TestXxx(*testing.T) declarations in a
// test file, read through the Go parser rather than a pattern over its text.
func testFunctions(file string) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tests := []string{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || len(fn.Type.Params.List) != 1 {
			continue
		}
		if star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr); ok && types.ExprString(star) == "*testing.T" {
			tests = append(tests, fn.Name.Name)
		}
	}
	return tests, nil
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
