// Command kernel-eval runs the fixed offline benchmark corpus against the
// Agent Execution Kernel under the paired baselines of issue #446 section 16
// (items 1-4) and writes a JSON report of raw observations, denominators,
// unknowns and limitations. Item 5 (native CLI vs kernel) is Gate B work and
// is reported as not run.
//
//	go run ./cmd/kernel-eval -corpus testdata/eval -trials 3 -out /tmp/kernel-eval.json
//
// It exits nonzero only on a harness error or a run whose termination or
// verification checks differ from the task's expectation. There is no
// savings threshold.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func main() {
	corpusDir := flag.String("corpus", "testdata/eval", "corpus directory holding corpus.json and fixtures/")
	trials := flag.Int("trials", 3, "trials per task and mode")
	out := flag.String("out", "", "report path (default: stdout)")
	modeFlag := flag.String("mode", "all", "all, or a comma list of baseline,cold,warm,drift")
	flag.Parse()
	code, err := run(*corpusDir, *trials, *out, *modeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kernel-eval:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(corpusDir string, trials int, out, modeFlag string) (int, error) {
	if trials < 1 {
		return 0, fmt.Errorf("-trials must be at least 1")
	}
	modes, err := parseModes(modeFlag)
	if err != nil {
		return 0, err
	}
	c, err := loadCorpus(corpusDir)
	if err != nil {
		return 0, err
	}
	scratch, err := os.MkdirTemp("", "kernel-eval-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(scratch)
	rep, err := evaluate(context.Background(), &harness{c: c, scratch: scratch}, trials, modes)
	if err != nil {
		return 0, err
	}
	if err := writeReport(rep, out); err != nil {
		return 0, err
	}
	d := rep.Denominators
	fmt.Fprintf(os.Stderr, "kernel-eval: planned %d attempted %d executed %d verified %d verification_failed %d harness_errors %d (%.0f ms)\n",
		d.Planned, d.Attempted, d.Executed, d.Verified, d.VerificationFailed, d.HarnessErrors, rep.DurationMillis)
	for _, f := range rep.Failures {
		fmt.Fprintf(os.Stderr, "kernel-eval: FAIL %s trial %d %s: %s\n", f.Task, f.Trial, f.Mode, f.Reason)
	}
	if d.HarnessErrors > 0 || d.VerificationFailed > 0 || d.Attempted != d.Planned {
		return 1, nil
	}
	return 0, nil
}

func parseModes(s string) ([]mode, error) {
	if s == "all" {
		return allModes, nil
	}
	var modes []mode
	for _, name := range strings.Split(s, ",") {
		m := mode(strings.TrimSpace(name))
		if !containsMode(allModes, m) || containsMode(modes, m) {
			return nil, fmt.Errorf("-mode: unknown or repeated mode %q", name)
		}
		modes = append(modes, m)
	}
	// Run order matters: warm and drift reuse the cold run's stores.
	var ordered []mode
	for _, m := range allModes {
		if containsMode(modes, m) {
			ordered = append(ordered, m)
		}
	}
	return ordered, nil
}

func containsMode(list []mode, m mode) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

func evaluate(ctx context.Context, h *harness, trials int, modes []mode) (report, error) {
	start := time.Now()
	rep := report{
		Schema: reportSchema, GeneratedAt: start.UTC(), KernelVersion: api.KernelVersion,
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Command: append([]string{"kernel-eval"}, os.Args[1:]...),
		Config: reportConfig{
			Trials: trials, Settings: h.c.Settings, Binding: h.c.Binding, DeadlineSeconds: h.c.DeadlineSeconds,
			MemoryLimits: fmt.Sprintf("%d records, %d bytes", memoryLimits.MaxRecords, memoryLimits.MaxBytes),
		},
		NotRun: []notRun{gateBNotRun}, Limitations: limitations,
	}
	for _, m := range modes {
		rep.Config.Modes = append(rep.Config.Modes, string(m))
	}
	cfg, err := encodeJSON(rep.Config)
	if err != nil {
		return rep, err
	}
	rep.ConfigDigest = api.Digest(cfg)
	if rep.Corpus, err = corpusInfoOf(h.c); err != nil {
		return rep, err
	}
	for trial := 1; trial <= trials; trial++ {
		for _, t := range h.c.Tasks {
			rep.Observations = append(rep.Observations, h.runTask(ctx, trial, t, modes)...)
		}
	}
	planned := len(h.c.Tasks) * trials * len(modes)
	rep.Denominators, rep.ByMode, rep.Unknowns, rep.Failures = summarize(planned, rep.Observations)
	rep.DurationMillis = millis(time.Since(start))
	return rep, nil
}

func corpusInfoOf(c *corpus) (corpusInfo, error) {
	info := corpusInfo{
		Dir: c.dir, Digest: c.digest, FixtureDigests: c.fixtureDigests, ScriptDigests: map[string]string{},
		Tasks: len(c.Tasks),
	}
	for _, t := range c.Tasks {
		d, err := t.scriptDigest()
		if err != nil {
			return info, err
		}
		info.ScriptDigests[t.ID] = d
		if t.HeldOut {
			info.HeldOut++
		}
	}
	return info, nil
}

func writeReport(rep report, path string) error {
	data, err := encodeJSON(rep)
	if err != nil {
		return err
	}
	if path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
