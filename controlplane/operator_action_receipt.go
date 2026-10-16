package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"

	"github.com/bogdaniel/zenchron-engineering/planning"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

type actionReferenceKey struct{}
type actionReference struct{ id, digest string }

// correlateActionBodyBytes is this path's body-capture ceiling, matching
// decodeAction's own ordinary default: an operation-id-bearing request is
// almost always a tiny identifier set. Planning-artifact-propose is the one
// exception that needs more, by the SAME contract every other layer uses -
// see planning.MaxPlanningArtifactRequestBytes - not a bespoke number here.
func correlateActionBodyBytes(path string) int64 {
	if path == "/v1/planning/artifacts" {
		return planning.MaxPlanningArtifactRequestBytes
	}
	return defaultActionBodyBytes
}

// Capture the exact path and bounded wire body after auth/CSRF checks, before
// ordinary closed-shape decoding. The API remains a read-only store consumer.
//
// This runs for every POST, before the mux dispatches to a specific route,
// so it cannot yet rely on decodeActionWithLimit's per-route call to bound
// the body: it must pick its own ceiling from the path alone, the same way
// answerControlConnection must pick the transport ceiling before it knows
// the command.
func correlateAction(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	id := r.Header.Get("X-Zenchron-Operation-ID")
	if id == "" {
		return r, true
	}
	if len(r.Header.Values("X-Zenchron-Operation-ID")) != 1 || !rt.ValidActionReference(id, fmt.Sprintf("%064x", 0)) {
		fail(w, 400, "invalid_request")
		return r, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, correlateActionBodyBytes(r.URL.Path)))
	if err != nil {
		fail(w, 400, "invalid_request")
		return r, false
	}
	if err := r.Body.Close(); err != nil {
		fail(w, 400, "invalid_request")
		return r, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	digest := fmt.Sprintf("%x", sha256.Sum256(append([]byte(r.URL.EscapedPath()+"\n"), body...)))
	return r.WithContext(context.WithValue(r.Context(), actionReferenceKey{}, actionReference{id, digest})), true
}

// operatorActionReceipt serves GET /v1/operator-action-receipts/{id} for
// EITHER receipt store: the event-journal-backed one (plan-revise, resume,
// refresh) or artifact_action_receipts (planning-artifact-propose,
// product-propose). Each dispatch path refuses to write when the OTHER
// store already holds the same operation id (cmd/zenchron-engineering's
// dispatchPlanOrRunWithReceipt and dispatchArtifactWithReceipt), so trying
// the first and falling back to the second is exact for any id that
// reached only one path - the one case it is not is a genuinely concurrent
// collision racing both dispatch paths at once, which is the same accepted
// residual as the single-process crash window documented on
// ClaimArtifactActionOperation.
func (a *API) operatorActionReceipt(w http.ResponseWriter, r *http.Request) {
	id, digest := r.PathValue("id"), r.URL.Query().Get("digest")
	if !rt.ValidActionReference(id, digest) || len(r.URL.Query()["digest"]) != 1 {
		fail(w, 400, "invalid_request")
		return
	}
	receipt, err := a.Store.OperatorActionReceipt(id)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	if receipt != nil {
		if receipt.RequestDigest != digest {
			fail(w, 409, "subject_mismatch")
			return
		}
		send(w, 200, receipt)
		return
	}
	artifactReceipt, err := a.Store.ArtifactActionReceipt(id)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	if artifactReceipt == nil {
		fail(w, 404, "not_found")
		return
	}
	if artifactReceipt.RequestDigest != digest {
		fail(w, 409, "subject_mismatch")
		return
	}
	send(w, 200, artifactReceipt)
}
