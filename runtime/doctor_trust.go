package runtime

import "strings"

// doctorTrust reports the serving controller's trusted-main state (ADR-0007
// §5). A HOLD is a degraded state, not a failure: the controller keeps serving
// and nothing is adopted, so it warns with everything an operator needs to see
// why. Doctor never resolves trust itself: the answer is the serving
// controller's own, so doctor and `controller status` cannot disagree.
func doctorTrust(in DoctorInput) DoctorCheck {
	const id = "controller.trusted_main"
	switch {
	case in.ControllerUpdateError != nil:
		return warn(doctorGroupController, id,
			"no serving controller answered, so its trusted-main state was not observed: "+in.ControllerUpdateError.Error())
	case in.ControllerUpdate == nil:
		return warn(doctorGroupController, id,
			"the serving controller has made no trusted-main observation yet (no upgrade is bound, or none has run)")
	}
	update := *in.ControllerUpdate
	reason := update.Describe()
	if update.Trust != nil {
		parts := []string{reason}
		for _, l := range update.Trust.Lines() {
			parts = append(parts, l[0]+" "+l[1])
		}
		reason = strings.Join(parts, "; ")
	}
	switch update.State {
	case UpdateHeld, UpdateObservationFailed:
		return warn(doctorGroupController, id, reason)
	default:
		return pass(doctorGroupController, id, reason)
	}
}
