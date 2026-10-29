package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestAuthorityReceiptReadsBeyondFirstJournalPageAndRedactsIdentity(t *testing.T) {
	f := newActionFixture(t)
	for i := range 505 {
		if _, err := f.store.AppendEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: fmt.Sprintf("history-%d", i), RunID: "s", Type: rt.EventRunCreated, OccurredAt: time.Unix(int64(i+200), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := json.Marshal(rt.HumanAuthorityRecordedPayload{SchemaVersion: rt.SchemaVersion, EvidenceID: "authority", Request: rt.Ref{ID: "request", Revision: "digest"}, Decision: "approve", Operator: rt.RecordedOperator{ID: "SECRET_OPERATOR", Provenance: rt.ProvenanceLocalUnverified}, Action: domain.Action{Type: "publish", Target: "main"}, Repository: "example/repo", Controller: rt.Ref{ID: "controller", Revision: "1"}, Candidate: rt.Candidate{Revision: "commit", Tree: "tree"}, Contract: rt.Ref{ID: "contract", Revision: "1"}, StateSHA256: "state", OccurredAt: time.Unix(900, 0).UTC(), Note: "SECRET_NOTE"})
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.store.AppendEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: "authority", RunID: "s", Type: rt.EventHumanAuthorityRecorded, OccurredAt: time.Unix(900, 0).UTC(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RunID    string             `json:"run_id"`
		Receipts []AuthorityReceipt `json:"receipts"`
	}
	read := func(digest string) {
		t.Helper()
		if err := json.Unmarshal(wire(t, f.api, "/v1/runs/s/authority-receipts?request_id=request&digest="+digest, "GET", f.api.Token, "authority-receipts", 200), &got); err != nil {
			t.Fatal(err)
		}
	}
	read("digest")
	if got.RunID != "s" || len(got.Receipts) != 1 || got.Receipts[0].Sequence != event.Sequence || got.Receipts[0].Decision != "approve" {
		t.Fatalf("lost exact receipt: %+v", got)
	}
	read("other")
	if len(got.Receipts) != 0 {
		t.Fatal("different subject returned a receipt")
	}
	for _, query := range []string{"", "?request_id=request", "?request_id=request&digest=digest&digest=other"} {
		wire(t, f.api, "/v1/runs/s/authority-receipts"+query, "GET", f.api.Token, "error", 400)
	}
	wire(t, f.api, "/v1/runs/s/authority-receipts?request_id=request&digest=digest", "GET", "", "error", 401)
	if err := f.api.Store.Close(); err != nil {
		t.Fatal(err)
	}
	wire(t, f.api, "/v1/runs/s/authority-receipts?request_id=request&digest=digest", "GET", f.api.Token, "error", 500)
}
