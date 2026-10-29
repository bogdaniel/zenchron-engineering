package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestRunHTTPFiltersBeforePaginationAndKeepsJournalAuthority(t *testing.T) {
	f := newActionFixture(t)
	at := time.Now().UTC()
	for i := range 61 {
		id := fmt.Sprintf("run-%03d", i)
		if err := f.store.PutRun(rt.EngineeringRun{SchemaVersion: rt.SchemaVersion, ID: id, Repository: "example/repo", Goal: "github-issue:example/repo#42",
			Phase: rt.Execute, Disposition: rt.Active, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{rt.EventRunCreated, rt.EventRunWaiting} {
			var payload json.RawMessage
			if kind == rt.EventRunWaiting {
				payload = json.RawMessage(`{"reason":"awaiting_input"}`)
			}
			if _, err := f.store.AppendEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: id + kind, RunID: id, Type: kind, OccurredAt: at, Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var page Runs
	read := func(path string) {
		t.Helper()
		if err := json.Unmarshal(wire(t, f.api, path, "GET", f.api.Token, "runs", 200), &page); err != nil {
			t.Fatal(err)
		}
	}
	read("/v1/runs?status=waiting&limit=50")
	if len(page.Runs) != 50 || !page.HasMore {
		t.Fatalf("first filtered page: %+v", page)
	}
	read("/v1/runs?status=waiting&limit=50&offset=50")
	if len(page.Runs) != 11 || page.HasMore {
		t.Fatalf("last filtered page: %+v", page)
	}
	read("/v1/runs?status=active&limit=50")
	if len(page.Runs) != 2 {
		t.Fatalf("journal waiting must override active rows: %+v", page)
	}
	read("/v1/runs?status=held")
	if len(page.Runs) != 0 {
		t.Fatal("held is a material fact, not a disposition")
	}
	read("/v1/runs?source=42&status=waiting&limit=50")
	if len(page.Runs) != 50 || !page.HasMore {
		t.Fatal("bare issue filter must cover the full fleet")
	}
	read("/v1/runs?source=example%2Frepo%2342&offset=50&limit=50")
	if len(page.Runs) != 11 || page.HasMore {
		t.Fatal("repository/issue filter must precede paging")
	}
	read("/v1/runs?source=0")
	if len(page.Runs) != 0 {
		t.Fatal("issue zero must not select runs without a source issue")
	}
	read("/v1/runs?source=example%2Frepo%23999")
	if len(page.Runs) != 0 {
		t.Fatal("a non-issue run cannot satisfy an issue filter")
	}
	for _, query := range []string{"status=bogus", "status=active%7Cwaiting", "status=active&status=waiting", "source=1&source=2"} {
		wire(t, f.api, "/v1/runs?"+query, "GET", f.api.Token, "error", 400)
	}
}
