package benchmark

import "time"

// RelayBasis states how far a relay count has been verified.
type RelayBasis string

const (
	// RelaySelfReported: the operator's own log, not independently inspected.
	// A count of zero is always self-reported: absence cannot be attached to.
	RelaySelfReported RelayBasis = "self_reported"
	// RelayPartiallyAudited: some, not all, relay entries carry an audit.
	RelayPartiallyAudited RelayBasis = "partially_audited"
	RelayAudited          RelayBasis = "audited"
)

// RelaySummary counts manual relays and says how trustworthy the count is.
type RelaySummary struct {
	Total        int               `json:"total"`
	ByKind       map[RelayKind]int `json:"by_kind"`
	Audited      int               `json:"audited"`
	SelfReported int               `json:"self_reported"`
	Basis        RelayBasis        `json:"basis"`
}

// Supervision is one arm's human time and intervention counts, derived from the
// activity log alone.
type Supervision struct {
	Arm      Arm    `json:"arm"`
	Operator string `json:"operator"`
	// ActiveHumanSeconds is every attended kind; UnattendedWaitSeconds is
	// provider/CI waiting and is never part of it.
	ActiveHumanSeconds    float64          `json:"active_human_seconds"`
	UnattendedWaitSeconds float64          `json:"unattended_wait_seconds"`
	SecondsByKind         map[Kind]float64 `json:"seconds_by_kind"`
	// AuthorityInterventions are intended approvals/decisions/merge intent;
	// FinalAcceptanceMerges is the closing acceptance step. Neither is a
	// workaround.
	AuthorityInterventions int `json:"authority_interventions"`
	FinalAcceptanceMerges  int `json:"final_acceptance_merges"`
	RecoveryWorkarounds    int `json:"recovery_workarounds"`
	// NonAuthorityInterventions are relays plus workarounds: human work that
	// exists because the system did not carry the state itself.
	NonAuthorityInterventions int          `json:"non_authority_interventions"`
	Relays                    RelaySummary `json:"relays"`
	OpenIntervals             int          `json:"open_intervals"`
	FirstStart                *time.Time   `json:"first_start,omitempty"`
	LastEnd                   *time.Time   `json:"last_end,omitempty"`
}

// Summarize derives one arm's Supervision. ok is false when the arm has no
// entries, which the caller reports as an invalid run, not as zero minutes.
func (l Ledger) Summarize(arm Arm) (s Supervision, ok bool) {
	s = Supervision{Arm: arm, SecondsByKind: map[Kind]float64{},
		Relays: RelaySummary{ByKind: map[RelayKind]int{}, Basis: RelaySelfReported}}
	for _, in := range l.Intervals {
		if in.Arm != arm {
			continue
		}
		ok = true
		s.Operator = in.Operator
		start := in.Start
		if s.FirstStart == nil || start.Before(*s.FirstStart) {
			s.FirstStart = &start
		}
		if in.End == nil {
			s.OpenIntervals++
			continue
		}
		if s.LastEnd == nil || in.End.After(*s.LastEnd) {
			s.LastEnd = in.End
		}
		seconds := in.Duration().Seconds()
		s.SecondsByKind[in.Kind] += seconds
		if in.Kind.CountsAsHumanAttention() {
			s.ActiveHumanSeconds += seconds
		} else {
			s.UnattendedWaitSeconds += seconds
		}
		s.countIntervention(in)
	}
	s.NonAuthorityInterventions = s.Relays.Total + s.RecoveryWorkarounds
	switch {
	case s.Relays.Total > 0 && s.Relays.Audited == s.Relays.Total:
		s.Relays.Basis = RelayAudited
	case s.Relays.Audited > 0:
		s.Relays.Basis = RelayPartiallyAudited
	}
	return s, ok
}

func (s *Supervision) countIntervention(in Interval) {
	switch in.Kind {
	case KindAuthorityIntervened:
		s.AuthorityInterventions++
	case KindFinalAcceptanceMerge:
		s.FinalAcceptanceMerges++
	case KindRecoveryWorkaround:
		s.RecoveryWorkarounds++
	case KindManualRelay:
		s.Relays.Total++
		s.Relays.ByKind[in.RelayKind]++
		if in.Audit != nil {
			s.Relays.Audited++
		} else {
			s.Relays.SelfReported++
		}
	}
}
