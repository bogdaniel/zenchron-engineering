package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/benchmark"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const benchmarkUsage = "usage: zenchron-engineering benchmark " +
	"{log start|stop|record|audit|summary --log <file> ...|emit --log <file> --arm <arm> --case-id <id> --repo <owner/name> --base-sha <sha> --outcome <accepted|rejected|failed|abandoned> --out <dir> [--state-dir <dir> --graph <id> --run <id>]}\n" +
	"  log start   --log F --arm A --operator O --kind K [--relay-kind R] [--note N]\n" +
	"  log stop    --log F [--id ID]\n" +
	"  log record  --log F --arm A --operator O --kind K [--relay-kind R] --start T --end T   (RFC 3339)\n" +
	"  log audit   --log F --id ID --auditor X --evidence-sha256 H\n" +
	"  log summary --log F"

// benchmarkCommand is the #66 measurement surface: the operator activity log
// and the immutable result emitter. It never writes runtime state.
func benchmarkCommand(args []string, now func() time.Time, stdout io.Writer) (int, error) {
	if len(args) == 0 {
		return runtime.ExitInvalid, errors.New(benchmarkUsage)
	}
	var err error
	switch {
	case args[0] == "emit":
		err = benchmarkEmit(args[1:], now, stdout)
	case args[0] == "log" && len(args) >= 2:
		err = benchmarkLog(args[1], args[2:], now, stdout)
	default:
		err = errors.New(benchmarkUsage)
	}
	if err != nil {
		return runtime.ExitInvalid, err
	}
	return runtime.ExitCompleted, nil
}

type logFlags struct {
	path, arm, operator, kind, relay, note, id, start, end, auditor, evidence string
}

func benchmarkLog(sub string, args []string, now func() time.Time, stdout io.Writer) error {
	var f logFlags
	set := flag.NewFlagSet("benchmark log "+sub, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	for name, target := range map[string]*string{"log": &f.path, "arm": &f.arm, "operator": &f.operator,
		"kind": &f.kind, "relay-kind": &f.relay, "note": &f.note, "id": &f.id, "start": &f.start, "end": &f.end,
		"auditor": &f.auditor, "evidence-sha256": &f.evidence} {
		set.StringVar(target, name, "", "")
	}
	if err := set.Parse(args); err != nil || set.NArg() != 0 {
		return errors.New(benchmarkUsage)
	}
	if f.path == "" {
		return errors.New("--log is required")
	}
	log := benchmark.Log{Path: f.path}
	at := now()
	switch sub {
	case "start":
		id, err := log.Start(benchmark.Arm(f.arm), f.operator, benchmark.Kind(f.kind), benchmark.RelayKind(f.relay), f.note, at)
		return printLine(stdout, "started", id, err)
	case "stop":
		id, err := log.Stop(f.id, at)
		return printLine(stdout, "stopped", id, err)
	case "record":
		return benchmarkRecord(log, f, stdout)
	case "audit":
		return log.Audit(f.id, f.auditor, f.evidence, at)
	case "summary":
		return benchmarkSummary(log, stdout)
	}
	return errors.New(benchmarkUsage)
}

func printLine(stdout io.Writer, verb, id string, err error) error {
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s %s\n", verb, id)
	return err
}

// benchmarkRecord appends a finished interval as a start/stop pair at explicit
// times, for an activity noted after the fact.
func benchmarkRecord(log benchmark.Log, f logFlags, stdout io.Writer) error {
	start, err := time.Parse(time.RFC3339Nano, f.start)
	if err != nil {
		return fmt.Errorf("--start: %w", err)
	}
	end, err := time.Parse(time.RFC3339Nano, f.end)
	if err != nil {
		return fmt.Errorf("--end: %w", err)
	}
	if !end.After(start) {
		return errors.New("--end must be after --start")
	}
	id, err := log.Start(benchmark.Arm(f.arm), f.operator, benchmark.Kind(f.kind), benchmark.RelayKind(f.relay), f.note, start)
	if err != nil {
		return err
	}
	if _, err := log.Stop(id, end); err != nil {
		return fmt.Errorf("recorded start %s but could not stop it: %w", id, err)
	}
	return printLine(stdout, "recorded", id, nil)
}

func benchmarkSummary(log benchmark.Log, stdout io.Writer) error {
	ledger, foldErr := log.Ledger()
	out := map[string]any{}
	for _, arm := range []benchmark.Arm{benchmark.ArmPlanned, benchmark.ArmDirect} {
		if s, ok := ledger.Summarize(arm); ok {
			out[string(arm)] = s
		}
	}
	if foldErr != nil {
		out["invalid"] = strings.Split(foldErr.Error(), "\n")
	}
	if err := writeJSON(stdout, out); err != nil {
		return err
	}
	return foldErr
}

type emitFlags struct {
	log, arm, caseID, caseClass, caseSource, acceptance, repo, baseSHA string
	outcome, outcomeReason, artifact, candidate, protocol, stateDir    string
	graph, out, corpus                                                 string
	runs                                                               stringList
	synthetic                                                          bool
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func benchmarkEmit(args []string, now func() time.Time, stdout io.Writer) error {
	var f emitFlags
	set := flag.NewFlagSet("benchmark emit", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	for name, target := range map[string]*string{"log": &f.log, "arm": &f.arm, "case-id": &f.caseID,
		"case-class": &f.caseClass, "case-source": &f.caseSource, "acceptance": &f.acceptance, "repo": &f.repo,
		"base-sha": &f.baseSHA, "outcome": &f.outcome, "outcome-reason": &f.outcomeReason, "artifact": &f.artifact,
		"candidate-revision": &f.candidate, "protocol-digest": &f.protocol, "state-dir": &f.stateDir,
		"graph": &f.graph, "out": &f.out, "corpus-version": &f.corpus} {
		set.StringVar(target, name, "", "")
	}
	set.Var(&f.runs, "run", "")
	set.BoolVar(&f.synthetic, "synthetic", false, "")
	if err := set.Parse(args); err != nil || set.NArg() != 0 {
		return errors.New(benchmarkUsage)
	}
	if f.log == "" || f.out == "" {
		return errors.New("--log and --out are required")
	}
	rawLog, err := os.ReadFile(f.log)
	if err != nil {
		return fmt.Errorf("read activity log: %w", err)
	}
	in := benchmark.Input{
		Arm: benchmark.Arm(f.arm), CorpusVersion: f.corpus, Repository: f.repo, BaseSHA: f.baseSHA,
		ProtocolDigest: f.protocol, RecordedAt: now(), Synthetic: f.synthetic,
		Case:    benchmark.Case{ID: f.caseID, Class: f.caseClass, Source: f.caseSource, Acceptance: f.acceptance},
		Outcome: benchmark.Outcome{Result: f.outcome, Reason: f.outcomeReason, Artifact: f.artifact, CandidateRevision: f.candidate},
	}
	if f.stateDir != "" {
		store, err := runtime.OpenReadStore(f.stateDir)
		if err != nil {
			return fmt.Errorf("open runtime state read-only: %w", err)
		}
		defer store.Close()
		evidence := benchmark.Collect(store, benchmark.Scope{GraphID: f.graph, RunIDs: f.runs}, now())
		in.Runtime = &evidence
	}
	emission, err := benchmark.Emit(f.out, in, rawLog)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"valid": emission.Result.Validity.Valid,
		"reasons": emission.Result.Validity.Reasons, "content_digest": emission.Result.ContentDigest, "files": emission.Paths})
}
