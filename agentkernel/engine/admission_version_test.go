package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// undeletable is an admission store whose Delete always fails, so a claim,
// once written, stays: what a refusal would leave behind if it relied on
// deleting a claim it never needed.
type undeletable struct{ *storage.MemoryRecords }

func (undeletable) Delete(context.Context, string, string) error { return errors.New("delete refused") }

const (
	legacyRecord  = `{"budget":{},"attempts":[{"attempt_id":"att-1","consumed":{"iterations":2}}]}`
	unknownRecord = `{"version":"agentkernel.admission/v9","budget":{},"attempts":[]}`
	// emptyLegacy is a present unversioned record holding no attempts: still
	// legacy state, never "no record".
	emptyLegacy = `{"budget":{},"attempts":[]}`
)

func versionRun(t *testing.T, records storage.Records, record string) *run {
	t.Helper()
	if err := records.Put(context.Background(), admissionPartition, "exec-1", []byte(record)); err != nil {
		t.Fatal(err)
	}
	e := &Engine{admissions: records, clock: api.NewManualClock(time.Unix(0, 0))}
	return newRun(e, context.Background(), api.ExecutionRequest{ExecutionID: "exec-1", AttemptID: "att-2"})
}

func claimHolder(t *testing.T, records storage.Records) string {
	t.Helper()
	holder, err := records.Get(context.Background(), claimPartition, "exec-1")
	if errors.Is(err, storage.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(holder)
}

// TestUnreadableRecordIsRefusedBeforeClaiming: the version check precedes
// the claim, so the refusal never writes one; with a store that cannot
// delete, a claim written and then "released" would still be there.
func TestUnreadableRecordIsRefusedBeforeClaiming(t *testing.T) {
	for name, tc := range map[string]struct{ record, detail string }{
		"legacy_v0.1":          {legacyRecord, "legacy unversioned (v0.1)"},
		"legacy_zero_attempts": {emptyLegacy, "legacy unversioned (v0.1)"},
		"legacy_empty_object":  {`{}`, "legacy unversioned (v0.1)"},
		"unknown":              {unknownRecord, "unknown version"},
	} {
		t.Run(name, func(t *testing.T) {
			records := undeletable{storage.NewMemoryRecords()}
			term, ok := versionRun(t, records, tc.record).admit(context.Background())
			if ok || term.Cause != api.CauseInvalidRequest || !strings.Contains(term.Detail, tc.detail) {
				t.Fatalf("admit = %+v, %v; want refused with %q", term, ok, tc.detail)
			}
			if holder := claimHolder(t, records); holder != "" {
				t.Fatalf("refusal left a claim held by %q", holder)
			}
		})
	}
}

// TestLegacyRecordAfterClaimReleasesTheClaim: a legacy record that appears
// between the unclaimed read and the claim is refused by the claim holder,
// which releases its claim. If the release itself fails, the claim stays
// and the detail says so: the kernel fails closed rather than guess.
func TestLegacyRecordAfterClaimReleasesTheClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		records    storage.Records
		wantHolder string
		wantDetail string
	}{
		"released":        {storage.NewMemoryRecords(), "", "legacy unversioned (v0.1)"},
		"release_failure": {undeletable{storage.NewMemoryRecords()}, "att-2", "admission claim not released: delete refused"},
	} {
		t.Run(name, func(t *testing.T) {
			r := versionRun(t, tc.records, legacyRecord)
			if err := tc.records.PutIfAbsent(context.Background(), claimPartition, "exec-1", []byte("att-2")); err != nil {
				t.Fatal(err)
			}
			term, ok := r.admitClaimed(context.Background())
			if ok || term.Cause != api.CauseInvalidRequest || !strings.Contains(term.Detail, tc.wantDetail) {
				t.Fatalf("admitClaimed = %+v, %v; want refused with %q", term, ok, tc.wantDetail)
			}
			if holder := claimHolder(t, tc.records); holder != tc.wantHolder {
				t.Fatalf("claim held by %q, want %q", holder, tc.wantHolder)
			}
		})
	}
}

// TestAbsentRecordIsANewExecution: only a missing record is new; the attempt
// is admitted and holds the claim.
func TestAbsentRecordIsANewExecution(t *testing.T) {
	records := storage.NewMemoryRecords()
	e := &Engine{admissions: records, clock: api.NewManualClock(time.Unix(0, 0))}
	r := newRun(e, context.Background(), api.ExecutionRequest{ExecutionID: "exec-1", AttemptID: "att-1"})
	if term, ok := r.admit(context.Background()); !ok {
		t.Fatalf("admit = %+v; want an absent record admitted", term)
	}
	if holder := claimHolder(t, records); holder != "att-1" {
		t.Fatalf("claim held by %q, want att-1", holder)
	}
}

// TestSettlementNeverExtendsALegacyRecord: a present unversioned record seen
// at settlement, even one with no attempts, is not extended or upgraded.
func TestSettlementNeverExtendsALegacyRecord(t *testing.T) {
	records := storage.NewMemoryRecords()
	r := versionRun(t, records, emptyLegacy)
	if err := r.recordConsumption(context.Background()); err == nil || !strings.Contains(err.Error(), "legacy unversioned (v0.1)") {
		t.Fatalf("recordConsumption = %v; want the legacy refusal", err)
	}
	if got, err := records.Get(context.Background(), admissionPartition, "exec-1"); err != nil || string(got) != emptyLegacy {
		t.Fatalf("legacy record rewritten: %s (%v)", got, err)
	}
}
