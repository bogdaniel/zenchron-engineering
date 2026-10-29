package benchmark

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func minute(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func newLog(t *testing.T) Log { return Log{Path: filepath.Join(t.TempDir(), "activity.jsonl")} }

// interval appends a finished start/stop pair.
func interval(t *testing.T, l Log, arm Arm, kind Kind, relay RelayKind, from, to int) string {
	t.Helper()
	id, err := l.Start(arm, "op-1", kind, relay, "", minute(from))
	if err != nil {
		t.Fatalf("start %s: %v", kind, err)
	}
	if _, err := l.Stop(id, minute(to)); err != nil {
		t.Fatalf("stop %s: %v", kind, err)
	}
	return id
}

func mustLedger(t *testing.T, l Log) Ledger {
	t.Helper()
	ledger, err := l.Ledger()
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return ledger
}

func TestUnattendedWaitIsNeverHumanAttention(t *testing.T) {
	l := newLog(t)
	interval(t, l, ArmPlanned, KindActiveSupervision, "", 0, 10)
	interval(t, l, ArmPlanned, KindUnattendedWait, "", 10, 40)
	interval(t, l, ArmPlanned, KindReviewingOutput, "", 40, 45)
	s, ok := mustLedger(t, l).Summarize(ArmPlanned)
	if !ok {
		t.Fatal("arm has entries")
	}
	if s.ActiveHumanSeconds != 15*60 {
		t.Fatalf("active human seconds = %v, want 900: unattended wait must not count", s.ActiveHumanSeconds)
	}
	if s.UnattendedWaitSeconds != 30*60 {
		t.Fatalf("unattended seconds = %v, want 1800", s.UnattendedWaitSeconds)
	}
}

func TestRelayClassificationAndAuditBasis(t *testing.T) {
	l := newLog(t)
	first := interval(t, l, ArmDirect, KindManualRelay, RelayFinding, 0, 2)
	interval(t, l, ArmDirect, KindManualRelay, RelayPrompt, 2, 3)
	interval(t, l, ArmDirect, KindManualRelay, RelayFinding, 3, 4)

	s, _ := mustLedger(t, l).Summarize(ArmDirect)
	if s.Relays.Total != 3 || s.Relays.ByKind[RelayFinding] != 2 || s.Relays.ByKind[RelayPrompt] != 1 {
		t.Fatalf("relay classification wrong: %+v", s.Relays)
	}
	if s.Relays.Basis != RelaySelfReported || s.Relays.SelfReported != 3 {
		t.Fatalf("unaudited relays must be self_reported: %+v", s.Relays)
	}

	if err := l.Audit(first, "op-1", strings.Repeat("a", 64), minute(60)); err == nil {
		t.Fatal("the operator must not audit their own relay")
	}
	if err := l.Audit(first, "auditor-9", "not-a-digest", minute(60)); err == nil {
		t.Fatal("audit evidence must be a sha256 digest")
	}
	if err := l.Audit(first, "auditor-9", strings.Repeat("a", 64), minute(60)); err != nil {
		t.Fatalf("audit: %v", err)
	}
	s, _ = mustLedger(t, l).Summarize(ArmDirect)
	if s.Relays.Basis != RelayPartiallyAudited || s.Relays.Audited != 1 || s.Relays.SelfReported != 2 {
		t.Fatalf("one audited relay of three: %+v", s.Relays)
	}
}

func TestZeroRelaysIsAlwaysSelfReported(t *testing.T) {
	l := newLog(t)
	interval(t, l, ArmPlanned, KindActiveSupervision, "", 0, 5)
	s, _ := mustLedger(t, l).Summarize(ArmPlanned)
	if s.Relays.Total != 0 || s.Relays.Basis != RelaySelfReported {
		t.Fatalf("a zero-relay claim cannot be audited and must stay self_reported: %+v", s.Relays)
	}
}

func TestAuthorityInterventionIsNotAWorkaround(t *testing.T) {
	l := newLog(t)
	interval(t, l, ArmPlanned, KindAuthorityIntervened, "", 0, 1)
	interval(t, l, ArmPlanned, KindFinalAcceptanceMerge, "", 1, 2)
	interval(t, l, ArmPlanned, KindRecoveryWorkaround, "", 2, 4)
	interval(t, l, ArmPlanned, KindManualRelay, RelayStatus, 4, 5)
	s, _ := mustLedger(t, l).Summarize(ArmPlanned)
	if s.AuthorityInterventions != 1 || s.FinalAcceptanceMerges != 1 || s.RecoveryWorkarounds != 1 {
		t.Fatalf("authority/workaround split wrong: %+v", s)
	}
	if s.NonAuthorityInterventions != 2 {
		t.Fatalf("non-authority interventions = %d, want workaround + relay = 2", s.NonAuthorityInterventions)
	}
}

func TestInvalidEntriesAreRefusedAndNeverReachTheFile(t *testing.T) {
	l := newLog(t)
	interval(t, l, ArmPlanned, KindActiveSupervision, "", 10, 20)
	before, _ := os.ReadFile(l.Path)

	refused := map[string]func() error{
		"invalid kind": func() error { _, e := l.Start(ArmPlanned, "op-1", "napping", "", "", minute(30)); return e },
		"unknown arm": func() error {
			_, e := l.Start("zenchron_explicit", "op-1", KindActiveSupervision, "", "", minute(30))
			return e
		},
		"no operator": func() error { _, e := l.Start(ArmPlanned, " ", KindActiveSupervision, "", "", minute(30)); return e },
		"relay without sub-kind": func() error {
			_, e := l.Start(ArmPlanned, "op-1", KindManualRelay, "", "", minute(30))
			return e
		},
		"sub-kind on non-relay": func() error {
			_, e := l.Start(ArmPlanned, "op-1", KindReviewingOutput, RelayPrompt, "", minute(30))
			return e
		},
		"duplicate": func() error { _, e := l.Start(ArmPlanned, "op-1", KindActiveSupervision, "", "", minute(10)); return e },
		"overlap":   func() error { _, e := l.Start(ArmPlanned, "op-1", KindReviewingOutput, "", "", minute(15)); return e },
		"different operator across arms": func() error {
			_, e := l.Start(ArmDirect, "someone-else", KindActiveSupervision, "", "", minute(30))
			return e
		},
		"stop of unknown entry": func() error { _, e := l.Stop("act-nope", minute(31)); return e },
	}
	for name, attempt := range refused {
		if err := attempt(); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	after, _ := os.ReadFile(l.Path)
	if string(before) != string(after) {
		t.Fatalf("a refused entry reached the log:\n%s", after)
	}
}

func TestZeroAndNegativeIntervalsAreRefused(t *testing.T) {
	l := newLog(t)
	id, err := l.Start(ArmPlanned, "op-1", KindActiveSupervision, "", "", minute(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Stop(id, minute(10)); err == nil {
		t.Fatal("zero-length interval must be refused")
	}
	if _, err := l.Stop(id, minute(5)); err == nil {
		t.Fatal("negative interval must be refused")
	}
	if _, err := l.Stop(id, minute(11)); err != nil {
		t.Fatalf("a valid stop after refusals must still work: %v", err)
	}
}

func TestStopWithoutIDRefusesAmbiguity(t *testing.T) {
	l := newLog(t)
	if _, err := l.Stop("", minute(1)); err == nil {
		t.Fatal("stopping with nothing open must be refused")
	}
	id, _ := l.Start(ArmPlanned, "op-1", KindActiveSupervision, "", "", minute(1))
	got, err := l.Stop("", minute(2))
	if err != nil || got != id {
		t.Fatalf("a single open interval is stopped by default: id=%q err=%v", got, err)
	}
}

func TestTamperedLogIsReportedNotTrusted(t *testing.T) {
	l := newLog(t)
	interval(t, l, ArmPlanned, KindActiveSupervision, "", 0, 5)
	raw, _ := os.ReadFile(l.Path)
	forged := strings.Replace(string(raw), `"kind":"active_supervision"`, `"kind":"unattended_wait"`, 1)
	if err := os.WriteFile(l.Path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Ledger(); err == nil {
		t.Fatal("an entry whose id no longer matches its content must be refused")
	}
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	if _, err := ParseRecords(strings.NewReader(`{"schema_version":"0.1","op":"start","surprise":1}` + "\n")); err == nil {
		t.Fatal("unknown fields must be refused")
	}
}

func TestConcurrencyFromRealIntervals(t *testing.T) {
	c := ComputeConcurrency([]Span{
		{"a", minute(0), minute(10)},
		{"b", minute(5), minute(15)},
		{"c", minute(15), minute(20)}, // touches b: no overlap
		{"z", minute(30), minute(30)}, // zero-length: ignored
	})
	if c.Peak != 2 {
		t.Fatalf("peak = %d, want 2", c.Peak)
	}
	if c.OverlapSeconds != 5*60 {
		t.Fatalf("overlap = %v, want 300", c.OverlapSeconds)
	}
	if c.UnionSeconds != 20*60 || c.SumSeconds != 25*60 {
		t.Fatalf("union=%v sum=%v, want 1200/1500", c.UnionSeconds, c.SumSeconds)
	}
	if c.Ignored != 1 {
		t.Fatalf("ignored = %d, want 1", c.Ignored)
	}
	if seq := ComputeConcurrency([]Span{{"a", minute(0), minute(5)}, {"b", minute(5), minute(9)}}); seq.Peak != 1 || seq.OverlapSeconds != 0 {
		t.Fatalf("sequential spans must not overlap: %+v", seq)
	}
}
