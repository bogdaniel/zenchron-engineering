package controlplane

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/planning"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestCorrelateActionBodyBytesIsPathScoped is the one case this path cannot
// know its command the way answerControlConnection does - it runs before the
// mux dispatches at all, so it must decide its ceiling from the path alone.
// Planning-artifact-propose gets the shared contract ceiling; every other
// path keeps the ordinary default, unchanged.
func TestCorrelateActionBodyBytesIsPathScoped(t *testing.T) {
	if got := correlateActionBodyBytes("/v1/planning/artifacts"); got != planning.MaxPlanningArtifactRequestBytes {
		t.Fatalf("planning artifacts path: got %d, want %d", got, planning.MaxPlanningArtifactRequestBytes)
	}
	for _, path := range []string{"/v1/runs/s/resume", "/v1/plans/p/revise", "/v1/products", ""} {
		if got := correlateActionBodyBytes(path); got != defaultActionBodyBytes {
			t.Fatalf("path %q: got %d, want the ordinary default %d", path, got, defaultActionBodyBytes)
		}
	}
}

const recoveryTestID = "01234567-89ab-4cde-8fab-0123456789ab"

func TestOperatorActionReferenceBindsExactPathBodyAndLeavesReadsAuthoritative(t *testing.T) {
	f := newActionFixture(t)
	body := `{"controller_binding":"binding"}`
	path := "/v1/runs/s/resume"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(path+"\n"+body)))
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.OperationID != recoveryTestID || request.RequestDigest != digest || request.ExpectedController != "binding" {
			t.Fatalf("lost original wire binding: %+v", request)
		}
		return rt.ControlResponse{Code: rt.ControlCodeOutcomeUnknown}, nil
	}
	req := validRequest(path, body)
	req.Header.Set("X-Zenchron-Operation-ID", recoveryTestID)
	out := post(t, f.api, req, 504)
	if out["outcome"] != "unknown" {
		t.Fatal("uncertain completed action was called refused")
	}
	for _, id := range []string{"short", strings.ToUpper(recoveryTestID)} {
		req := validRequest(path, body)
		req.Header.Set("X-Zenchron-Operation-ID", id)
		post(t, f.api, req, 400)
	}
	if f.sends.Load() != 1 {
		t.Fatal("invalid correlation reached the controller")
	}
	receipt := rt.OperatorActionReceipt{OperationID: recoveryTestID, RequestDigest: digest, Action: rt.ControlResume, ControllerBinding: "binding", SourceRunID: "s", RunID: "s"}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.store.AppendEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: "operator-action-" + recoveryTestID, OperationID: recoveryTestID, RunID: "s", Type: rt.EventRunOperatorActionReceipt, OccurredAt: time.Unix(200, 0).UTC(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if event.StateBefore != event.StateAfter {
		t.Fatal("delivery receipt changed runtime state")
	}
	url := "/v1/operator-action-receipts/" + recoveryTestID + "?digest=" + digest
	f.api.Observe = nil
	if err := json.Unmarshal(wire(t, f.api, url, "GET", f.api.Token, "operator-action-receipt", 200), &receipt); err != nil {
		t.Fatal(err)
	}
	wire(t, f.api, url, "GET", "", "error", 401)
	wire(t, f.api, "/v1/operator-action-receipts/"+recoveryTestID+"?digest="+strings.Repeat("0", 64), "GET", f.api.Token, "error", 409)
	wire(t, f.api, "/v1/operator-action-receipts/00000000-0000-0000-0000-000000000000?digest="+digest, "GET", f.api.Token, "error", 404)
	wire(t, f.api, url+"&digest="+digest, "GET", f.api.Token, "error", 400)
}

// TestOperatorActionReceiptEndpointAlsoServesArtifactActionReceipts proves
// the ONE endpoint serves BOTH receipt stores transparently: an operation id
// that only exists in artifact_action_receipts (never the event journal) is
// still found, validated against the SAME response schema (extended for the
// two new action kinds), and returned exactly as written.
func TestOperatorActionReceiptEndpointAlsoServesArtifactActionReceipts(t *testing.T) {
	f := newActionFixture(t)
	const opID = "11234567-89ab-4cde-8fab-0123456789ab"
	digest := strings.Repeat("3", 64)
	written := rt.ArtifactActionReceipt{
		OperationID: opID, RequestDigest: digest, Action: rt.ControlPlanningArtifactPropose,
		ControllerBinding: "binding", ArtifactKind: "agent_profile", ArtifactID: "reviewer",
		ArtifactDigest: strings.Repeat("4", 64),
	}
	if claimed, err := f.store.ClaimArtifactActionOperation(opID, time.Unix(200, 0).UTC()); err != nil || !claimed {
		t.Fatalf("claimed=%v, err=%v", claimed, err)
	}
	if err := f.store.FinalizeArtifactActionReceipt(written, time.Unix(200, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	url := "/v1/operator-action-receipts/" + opID + "?digest=" + digest
	f.api.Observe = nil
	var read rt.ArtifactActionReceipt
	if err := json.Unmarshal(wire(t, f.api, url, "GET", f.api.Token, "operator-action-receipt", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read != written {
		t.Fatalf("read back %+v, wrote %+v", read, written)
	}
}
