package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

type actionReferenceKey struct{}
type actionReference struct{ id, digest string }

// Capture the exact path and bounded wire body after auth/CSRF checks, before
// ordinary closed-shape decoding. The API remains a read-only store consumer.
func correlateAction(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	id := r.Header.Get("X-Zenchron-Operation-ID")
	if id == "" {
		return r, true
	}
	if len(r.Header.Values("X-Zenchron-Operation-ID")) != 1 || !rt.ValidActionReference(id, fmt.Sprintf("%064x", 0)) {
		fail(w, 400, "invalid_request")
		return r, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
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
	if receipt == nil {
		fail(w, 404, "not_found")
		return
	}
	if receipt.RequestDigest != digest {
		fail(w, 409, "subject_mismatch")
		return
	}
	send(w, 200, receipt)
}
