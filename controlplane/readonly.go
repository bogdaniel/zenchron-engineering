package controlplane

import (
	"errors"
	"net/http"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

var errAuxiliaryReadOnly = errors.New("this is an auxiliary read-only control plane; use the primary dashboard for governed actions")

// ReadOnlyError is what an auxiliary (read-only) Control Plane answers to
// anything that is not a read.
const ReadOnlyError = "auxiliary_read_only"

// ReadOnly wraps a handler so only reads get through. It is enforced HERE, on
// the server, not by hiding buttons: an auxiliary dashboard shares the state
// directory with the primary and must never be a second path to a governed
// action, whatever a client sends. Safe methods (GET, HEAD, OPTIONS) pass; every
// other method is refused before it reaches any route.
func ReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"` + ReadOnlyError + `"}`))
		}
	})
}

// RefuseSend is the Send an auxiliary instance carries: the second, independent
// guard, so a governed request cannot reach the controller socket even if a
// route were ever mounted outside ReadOnly.
func RefuseSend(rt.ControlRequest) (rt.ControlResponse, error) {
	return rt.ControlResponse{}, errAuxiliaryReadOnly
}
