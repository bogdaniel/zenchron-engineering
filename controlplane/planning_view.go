package controlplane

// Governed authoring of planning registry artifacts (ADR-0008): a write is
// validated by the identical seal function LoadRegistry uses on read, then
// durably written where it would find it. It is not a second store, and this
// file adds no projection of its own beyond echoing what was written.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/planning"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// proposePlanningArtifact serves POST /v1/planning/artifacts: validate and
// durably write one InstructionPack, ContextPolicy, AgentProfile or
// EngineeringPlanTemplate. A write is idempotent by content - writing the
// identical document twice reseals to the identical digest and overwrites
// with the identical bytes - so, unlike a plan or run action, there is no
// prior state to CAS against here; every well-formed request reaches the
// socket.
func (a *API) proposePlanningArtifact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string          `json:"controller_binding"`
		Kind              string          `json:"kind"`
		ID                string          `json:"id"`
		Document          json.RawMessage `json:"document"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Kind) == "" || strings.TrimSpace(body.ID) == "" || len(body.Document) == 0 {
		fail(w, 400, "invalid_request")
		return
	}
	var result json.RawMessage
	a.actResponse(w, r,
		ActionResult{Action: rt.ControlPlanningArtifactPropose, Target: ActionTarget{ArtifactKind: body.Kind, ArtifactID: body.ID}},
		body.ControllerBinding,
		rt.ControlRequest{
			Command:          rt.ControlPlanningArtifactPropose,
			PlanningArtifact: &planning.ArtifactWrite{Kind: planning.ArtifactKind(body.Kind), ID: body.ID, Document: body.Document},
		},
		func() (bool, any, error) {
			if result != nil {
				return true, result, nil
			}
			return false, nil, nil
		},
		func(any) (int, string) { return 0, "" },
		func(response rt.ControlResponse) error {
			result = response.Payload
			return nil
		})
}

// reloadPlanningRegistry serves POST /v1/planning/reload: re-read the
// planning registry from its unchanged, controller-fixed directory so a
// write just proposed is what new work resolves against, without a
// restart.
func (a *API) reloadPlanningRegistry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	var result json.RawMessage
	a.actResponse(w, r,
		ActionResult{Action: rt.ControlPlanningReload},
		body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlPlanningReload},
		func() (bool, any, error) {
			if result != nil {
				return true, result, nil
			}
			return false, nil, nil
		},
		func(any) (int, string) { return 0, "" },
		func(response rt.ControlResponse) error {
			result = response.Payload
			return nil
		})
}
