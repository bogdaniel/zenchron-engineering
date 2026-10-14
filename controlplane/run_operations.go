package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func (a *API) pause(w http.ResponseWriter, r *http.Request)   { a.setPause(w, r, rt.ControlPause) }
func (a *API) unpause(w http.ResponseWriter, r *http.Request) { a.setPause(w, r, rt.ControlUnpause) }
func (a *API) setPause(w http.ResponseWriter, r *http.Request, command string) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		Note              string `json:"note"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	effect := func() (bool, any, error) {
		found, err := a.Store.HasRun(id)
		if err != nil || !found {
			return false, nil, err
		}
		// Pause state is reduced by runtime from the complete ordered journal.
		events := []rt.EngineeringEvent{}
		after := int64(0)
		for {
			page, more, err := a.Store.EventsPage(id, after, 500)
			if err != nil {
				return false, nil, err
			}
			events = append(events, page...)
			if !more {
				break
			}
			after = page[len(page)-1].Sequence
		}
		paused := rt.JournalPause(events) != nil
		status, err := a.Store.Status(id, a.now())
		if err != nil {
			return false, nil, err
		}
		return paused == (command == rt.ControlPause), runDetailProjection(status), nil
	}
	a.act(w, r, ActionResult{Action: command, Target: ActionTarget{RunID: id}}, body.ControllerBinding,
		rt.ControlRequest{Command: command, RunID: id, Reason: rt.BoundedNote(body.Note)}, effect, func(state any) (int, string) {
			if state == nil {
				return 404, "run_not_found"
			}
			detail := state.(RunDetail)
			if detail.Disposition == rt.Completed || detail.Disposition == rt.Cancelled || detail.Disposition == rt.Failed {
				return 409, "run_terminal"
			}
			return 0, ""
		})
}

func (a *API) resume(w http.ResponseWriter, r *http.Request) { a.runLifecycle(w, r, rt.ControlResume) }
func (a *API) refresh(w http.ResponseWriter, r *http.Request) {
	a.runLifecycle(w, r, rt.ControlRefresh)
}
func (a *API) runLifecycle(w http.ResponseWriter, r *http.Request, command string) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	successor := ""
	effect := func() (bool, any, error) {
		target := id
		if successor != "" {
			target = successor
		}
		found, err := a.Store.HasRun(target)
		if err != nil || !found {
			return false, nil, err
		}
		status, err := a.Store.Status(target, a.now())
		if err != nil {
			return false, nil, err
		}
		return successor != "", runDetailProjection(status), nil
	}
	precheck := func(state any) (int, string) {
		if state == nil {
			return 404, "run_not_found"
		}
		status := state.(RunDetail)
		if command == rt.ControlResume && (status.Disposition == rt.Cancelled || status.Disposition == rt.Completed || status.Disposition == rt.Failed) {
			return 409, "run_terminal"
		}
		return 0, ""
	}
	a.actResponse(w, r, ActionResult{Action: command, Target: ActionTarget{RunID: id}}, body.ControllerBinding, rt.ControlRequest{Command: command, RunID: id}, effect, precheck, func(response rt.ControlResponse) error {
		if command != rt.ControlRefresh {
			return nil
		}
		var outcome rt.StartOutcome
		if err := json.Unmarshal(response.Payload, &outcome); err != nil {
			return err
		}
		if outcome.RunID == "" {
			return fmt.Errorf("refresh response has no successor")
		}
		successor = outcome.RunID
		return nil
	})
}
