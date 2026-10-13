package controlplane

import "net/http"

// fleet exposes the existing fleet projection's aggregate facts, without its
// unbounded run and plan arrays. Zero capacity remains unknown to this reader.
func (a *API) fleet(w http.ResponseWriter, r *http.Request) {
	view, err := a.Store.Fleet(a.now())
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	out := fleetProjection(view)
	out.Runs, out.Plans = nil, nil
	send(w, 200, out)
}
