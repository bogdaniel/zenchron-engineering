package benchmark

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseSHA = "d4f3a37d4f3a37d4f3a37d4f3a37d4f3a37d4f3a"

func validInput(t *testing.T, l Log, arm Arm) Input {
	t.Helper()
	interval(t, l, arm, KindActiveSupervision, "", 0, 10)
	return Input{Arm: arm, Case: Case{ID: "issue-344"}, Repository: "o/r", BaseSHA: baseSHA, RecordedAt: minute(120),
		Outcome: Outcome{Result: OutcomeAccepted}}
}

func rawLog(t *testing.T, l Log) []byte {
	t.Helper()
	b, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEmitWritesImmutableEvidenceSetAndRefusesOverwrite(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmDirect)
	dir := t.TempDir()
	em, err := Emit(dir, in, rawLog(t, l))
	if err != nil {
		t.Fatal(err)
	}
	if !em.Result.Validity.Valid {
		t.Fatalf("expected a valid run: %v", em.Result.Validity.Reasons)
	}
	if len(em.Paths) != 3 { // direct arm: log copy, result, report (no runtime collection)
		t.Fatalf("paths = %v", em.Paths)
	}
	for _, p := range em.Paths {
		if info, _ := os.Stat(p); info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s must be read-only", p)
		}
	}
	original, _ := os.ReadFile(em.Paths[1])
	in.Outcome.Result = OutcomeRejected
	if _, err := Emit(dir, in, rawLog(t, l)); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a second emission over an existing result must be refused, got %v", err)
	}
	after, _ := os.ReadFile(em.Paths[1])
	if string(original) != string(after) {
		t.Fatal("existing result was modified")
	}
}

func TestContentDigestIsIdentityAndDetectsChange(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmDirect)
	in.Ledger, _ = l.Ledger()
	a, _ := Build(in)
	b, _ := Build(in)
	if a.ContentDigest == "" || a.ContentDigest != b.ContentDigest {
		t.Fatalf("same content must have the same digest: %q %q", a.ContentDigest, b.ContentDigest)
	}
	if recomputed, _ := Digest(a); recomputed != a.ContentDigest {
		t.Fatal("digest must verify against the record it seals")
	}
	in.Outcome.Result = OutcomeRejected
	c, _ := Build(in)
	if c.ContentDigest == a.ContentDigest {
		t.Fatal("different content must have a different digest")
	}
	a.Outcome.Result = OutcomeRejected // tampering after sealing
	if recomputed, _ := Digest(a); recomputed == a.ContentDigest {
		t.Fatal("tampering must be detectable")
	}
}

func TestProvenanceAndSourceDigestsAreRecorded(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmDirect)
	raw := rawLog(t, l)
	em, err := Emit(t.TempDir(), in, raw)
	if err != nil {
		t.Fatal(err)
	}
	ins := em.Result.Instrumentation
	if ins.Version != InstrumentationVersion || ins.Arm != ArmDirect || ins.BaseSHA != baseSHA {
		t.Fatalf("provenance incomplete: %+v", ins)
	}
	if ins.ProtocolFrozen || ins.ProtocolDigest != ProtocolPlaceholder {
		t.Fatalf("no protocol digest supplied: must be the explicit placeholder, got %+v", ins)
	}
	if len(em.Result.Sources) != 1 || em.Result.Sources[0].SHA256 != sha(raw) {
		t.Fatalf("source digest must be the digest of the preserved log: %+v", em.Result.Sources)
	}
	copied, _ := os.ReadFile(em.Paths[0])
	if sha(copied) != em.Result.Sources[0].SHA256 {
		t.Fatal("preserved copy does not match the recorded digest")
	}

	in.ProtocolDigest = strings.Repeat("b", 64)
	in.Case.ID = "issue-344-frozen"
	em2, err := Emit(t.TempDir(), in, raw)
	if err != nil || !em2.Result.Instrumentation.ProtocolFrozen {
		t.Fatalf("a supplied protocol digest is recorded as frozen: %v %+v", err, em2.Result.Instrumentation)
	}
}

func TestInvalidRunsAreEmittedFlaggedNotDropped(t *testing.T) {
	cases := map[string]func(l Log, in *Input){
		"no entries for the arm": func(l Log, in *Input) { in.Arm = ArmPlanned },
		"never stopped": func(l Log, in *Input) {
			if _, err := l.Start(ArmDirect, "op-1", KindReviewingOutput, "", "", minute(20)); err != nil {
				t.Fatal(err)
			}
		},
		"no outcome":        func(l Log, in *Input) { in.Outcome.Result = "" },
		"bad base revision": func(l Log, in *Input) { in.BaseSHA = "main" },
		"planned with no runtime": func(l Log, in *Input) {
			interval(t, l, ArmPlanned, KindActiveSupervision, "", 30, 31)
			in.Arm = ArmPlanned
		},
		"direct carrying runtime evidence": func(l Log, in *Input) { in.Runtime = &RuntimeEvidence{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLog(t)
			in := validInput(t, l, ArmDirect)
			mutate(l, &in)
			em, err := Emit(t.TempDir(), in, rawLog(t, l))
			if err != nil {
				t.Fatalf("an invalid run must still be emitted: %v", err)
			}
			if em.Result.Validity.Valid || len(em.Result.Validity.Reasons) == 0 {
				t.Fatalf("expected invalid with reasons: %+v", em.Result.Validity)
			}
			report, _ := os.ReadFile(em.Paths[len(em.Paths)-1])
			if !strings.Contains(string(report), "INVALID") {
				t.Fatalf("report must say the run is invalid:\n%s", report)
			}
		})
	}
}

func TestLogFaultsInvalidateTheResult(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmDirect)
	// Hand-append a second operator for the other arm, bypassing Append's guard.
	f, _ := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	rec := Record{SchemaVersion: ActivitySchemaVersion, Op: OpStart, Arm: ArmPlanned, Operator: "intruder", Kind: KindActiveSupervision, At: minute(50)}
	rec.EntryID = EntryID(rec.Arm, rec.Operator, rec.Kind, "", rec.At)
	line, _ := json.Marshal(rec)
	f.Write(append(line, '\n'))
	f.Close()
	em, err := Emit(t.TempDir(), in, rawLog(t, l))
	if err != nil {
		t.Fatal(err)
	}
	if em.Result.Validity.Valid || !strings.Contains(strings.Join(em.Result.Validity.Reasons, "|"), "one human must operate every arm") {
		t.Fatalf("same-operator rule must invalidate: %+v", em.Result.Validity)
	}
}

func TestFailedOutcomeIsAsMeasurableAsAccepted(t *testing.T) {
	for _, outcome := range []string{OutcomeAccepted, OutcomeRejected, OutcomeFailed, OutcomeAbandoned} {
		l := newLog(t)
		in := validInput(t, l, ArmDirect)
		in.Outcome = Outcome{Result: outcome, Reason: "stated"}
		em, err := Emit(t.TempDir(), in, rawLog(t, l))
		if err != nil || !em.Result.Validity.Valid {
			t.Fatalf("%s: err=%v validity=%+v", outcome, err, em.Result.Validity)
		}
		if got := em.Result.Measured.SupervisionMinutes; !got.IsKnown() || *got.Value != 10 {
			t.Fatalf("%s: supervision minutes must be measured identically, got %+v", outcome, got)
		}
	}
}

func TestPlannedArmWithRuntimeEvidenceIsValidAndMeasured(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmPlanned)
	ev := Collect(twoWorkerSource(), Scope{GraphID: "g"}, minute(60))
	in.Runtime = &ev
	em, err := Emit(t.TempDir(), in, rawLog(t, l))
	if err != nil {
		t.Fatal(err)
	}
	if !em.Result.Validity.Valid {
		t.Fatalf("reasons: %v", em.Result.Validity.Reasons)
	}
	m := em.Result.Measured
	if *m.ChildRuns.Value != 2 || *m.PeakExecConcurrency.Value != 2 || *m.RemediationCycles.Value != 1 {
		t.Fatalf("runtime measures wrong: %+v", m)
	}
	if m.Cost.Known || m.Tokens.IsKnown() {
		t.Fatal("cost and tokens had no durable source and must remain unknown")
	}
	if len(em.Result.Sources) != 2 || filepath.Ext(em.Paths[1]) != ".json" {
		t.Fatalf("raw runtime collection must be preserved and referenced: %v", em.Result.Sources)
	}

	broken := twoWorkerSource()
	delete(broken.status, "r1")
	bad := Collect(broken, Scope{GraphID: "g"}, minute(60))
	in.Runtime, in.Case.ID = &bad, "issue-344-broken"
	em, _ = Emit(t.TempDir(), in, rawLog(t, l))
	if em.Result.Validity.Valid {
		t.Fatal("an unreadable run must invalidate the result")
	}
}

func TestEmitRefusesUnsafeCaseIDs(t *testing.T) {
	l := newLog(t)
	in := validInput(t, l, ArmDirect)
	in.Case.ID = "../escape"
	if _, err := Emit(t.TempDir(), in, rawLog(t, l)); err == nil {
		t.Fatal("a case id must not be able to escape the output directory")
	}
}

// A synthetic example must never be mistakable for real evidence: every result
// under benchmarks/synthetic declares itself, and still verifies against its own digest.
func TestSyntheticExamplesDeclareThemselvesAndVerify(t *testing.T) {
	paths, _ := filepath.Glob("../benchmarks/synthetic/*/*.json")
	var results int
	for _, p := range paths {
		if strings.HasSuffix(p, ".runtime.json") {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		results++
		if !r.Synthetic {
			t.Errorf("%s lives under benchmarks/synthetic but is not marked synthetic", p)
		}
		if digest, _ := Digest(r); digest != r.ContentDigest {
			t.Errorf("%s: content digest does not verify", p)
		}
	}
	if results == 0 {
		t.Fatal("expected the synthetic example results to exist")
	}
}

func TestMeasureRefusesAmbiguousStates(t *testing.T) {
	v := 1
	if (Measure[int]{Value: &v, UnknownReason: "x"}).Validate() == nil || (Measure[int]{}).Validate() == nil {
		t.Fatal("known+unknown and reasonless unknown must both be refused")
	}
}
