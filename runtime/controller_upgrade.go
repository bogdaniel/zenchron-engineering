package runtime

// THE WHOLE OF #234 IN ONE PASS, and no new decisions.
//
// The updater notices trusted main moved, builds the successor and prepares
// the transition. The launcher starts that successor and hands the role over.
// This file is the join: it asks the first for a settled answer and, on the one
// answer that means "there is a transition ready", gives it to the second.
//
// IT IS DRIVEN BY THE SUPERVISOR PASS, like the reconciler and for the same
// reasons: the supervisor already owns a poll interval, cancellation and a
// report, and a controller upgrade loop with its own timer would be a second
// lifecycle whose first mistake is dying at a different moment from the thing
// it upgrades.
//
// A LAUNCH BLOCKS THE PASS, deliberately. Up to the point of no return it is
// seconds - starting a process and reading one line - and after it this
// controller has drained and released the role, so there is no work a pass
// could be doing instead. Returning early to keep the loop responsive would
// mean a supervisor scheduling work it no longer has the authority to schedule.
//
// ONE TRANSITION PER PROCESS. Once a launch is committed this controller is no
// longer the controller, whatever happened next, and it will not start another:
// the attempt is remembered so a subsequent pass reports it instead of
// beginning a second handover from a process that has already given up the
// role.

import (
	"context"
	"sync"
	"time"
)

// ControllerUpgrade joins the updater to the launcher.
type ControllerUpgrade struct {
	updater *ControllerUpdater
	launch  func(ctx context.Context, prepared ControllerHandoff, subject RevisionRecord) SuccessionLaunch

	mu        sync.Mutex
	committed *SuccessionLaunch
}

// NewControllerUpgrade binds the two halves.
func NewControllerUpgrade(updater *ControllerUpdater, launch func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch) *ControllerUpgrade {
	return &ControllerUpgrade{updater: updater, launch: launch}
}

// ControllerUpgradeAttempt is what one pass did about upgrading this
// controller.
type ControllerUpgradeAttempt struct {
	// Update is the updater's settled answer, always present. Most passes end
	// here, saying "trusted main is what this controller is built from".
	Update ControllerUpdate `json:"update"`
	// Launch is present only when a prepared transition was attempted.
	Launch *SuccessionLaunch `json:"launch,omitempty"`
}

// Superseded reports that this controller has given up the role and must stop.
//
// It is the one question serve asks of this type, and it is deliberately not
// "did the upgrade succeed": a launch that committed and then failed still
// leaves this process without the role, and a controller that kept serving on
// the strength of its successor having disappointed it would be exactly the
// two-live-controllers outcome the protocol spends itself preventing.
func (a ControllerUpgradeAttempt) Superseded() bool {
	return a.Launch != nil && a.Launch.Committed
}

// Attempt advances the upgrade by at most one step.
func (u *ControllerUpgrade) Attempt(ctx context.Context, now time.Time) ControllerUpgradeAttempt {
	u.mu.Lock()
	settled := u.committed
	u.mu.Unlock()
	if settled != nil {
		// A COMMITTED LAUNCH IS FINAL FOR THIS PROCESS. Re-reporting it is
		// honest; attempting another would be a controller that has released
		// the role trying to hand it over again.
		current, _ := u.updater.Current()
		return ControllerUpgradeAttempt{Update: current, Launch: settled}
	}

	update := u.updater.Attempt(ctx, now)
	if update.State != UpdateReady {
		return ControllerUpgradeAttempt{Update: update}
	}
	prepared, ok := u.updater.Prepared()
	if !ok {
		// The updater says ready and cannot produce the record it prepared.
		// Nothing is launched from a transition nobody can show.
		return ControllerUpgradeAttempt{Update: update}
	}

	launch := u.launch(ctx, prepared, update.Subject)
	if launch.Committed {
		u.mu.Lock()
		u.committed = &launch
		u.mu.Unlock()
	}
	return ControllerUpgradeAttempt{Update: update, Launch: &launch}
}

// Describe renders one attempt for a report line.
func (a ControllerUpgradeAttempt) Describe() string {
	if a.Launch != nil {
		return a.Launch.Summary()
	}
	return string(a.Update.State) + ": " + a.Update.Detail
}
