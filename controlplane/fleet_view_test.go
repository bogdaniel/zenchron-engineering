package controlplane

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestFleetAggregateReusesCanonicalProjectionAndOmitsCollections(t *testing.T) {
	a := fixture(t, true)
	body := wire(t, a, "/v1/fleet", "GET", a.Token, "fleet", 200)
	var got Fleet
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Capacity != 0 || got.Counts.Waiting != 1 || got.Active != 1 {
		t.Fatalf("canonical facts lost: %+v", got)
	}
	if bytes.Contains(body, []byte(`"runs"`)) || bytes.Contains(body, []byte(`"plans"`)) || bytes.Contains(body, []byte("control.sock")) {
		t.Fatalf("unbounded or private collection escaped: %s", body)
	}
	wire(t, a, "/v1/fleet", "GET", "", "error", 401)
	wire(t, a, "/v1/fleet", "POST", a.Token, "error", 403)
	a.Store.Close()
	wire(t, a, "/v1/fleet", "GET", a.Token, "error", 500)
}
