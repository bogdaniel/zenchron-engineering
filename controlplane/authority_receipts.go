package controlplane

import (
	"encoding/json"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	"net/http"
	"time"
)

type AuthorityReceipt struct {
	RequestID  string    `json:"request_id"`
	Digest     string    `json:"digest"`
	Decision   string    `json:"decision"`
	Sequence   int64     `json:"sequence"`
	OccurredAt time.Time `json:"occurred_at"`
}

func readAuthorityReceipts(store *rt.ReadStore, id, requestID, digest string) ([]AuthorityReceipt, error) {
	receipts := []AuthorityReceipt{}
	after := int64(0)
	for {
		events, more, err := store.EventsPage(id, after, 500)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			after = event.Sequence
			if event.Type != rt.EventHumanAuthorityRecorded {
				continue
			}
			var recorded rt.HumanAuthorityRecordedPayload
			if err := json.Unmarshal(event.Payload, &recorded); err != nil {
				return nil, err
			}
			if recorded.Request.ID == requestID && recorded.Request.Revision == digest {
				receipts = append(receipts, AuthorityReceipt{RequestID: requestID, Digest: digest, Decision: recorded.Decision, Sequence: event.Sequence, OccurredAt: event.OccurredAt})
			}
		}
		if !more {
			return receipts, nil
		}
	}
}
func (a *API) authorityReceipts(w http.ResponseWriter, r *http.Request) {
	requestID, digest := r.URL.Query().Get("request_id"), r.URL.Query().Get("digest")
	if requestID == "" || len(requestID) > 200 || digest == "" || len(digest) > 200 || len(r.URL.Query()["request_id"]) != 1 || len(r.URL.Query()["digest"]) != 1 {
		fail(w, 400, "invalid_request")
		return
	}
	if !a.exists(w, r.PathValue("id")) {
		return
	}
	receipts, err := readAuthorityReceipts(a.Store, r.PathValue("id"), requestID, digest)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	send(w, 200, struct {
		RunID    string             `json:"run_id"`
		Receipts []AuthorityReceipt `json:"receipts"`
	}{r.PathValue("id"), receipts})
}
