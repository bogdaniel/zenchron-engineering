package runtime

import "fmt"

// THE OPERATOR'S VIEW OF TRUST (ADR-0007 §5).
//
// A held updater is a degraded state an operator has to be able to see and
// explain without reading logs: what the floor is and why it might be
// missing, where trust resolved to, where main is, and why every commit
// between them is not trusted. The same report is carried by the serve
// upgrade line, `controller status` and `autonomy doctor`, so the three can
// never disagree about it.

// maxReportedSkipped bounds the skipped commits a report carries. The walk may
// skip up to 256; an operator needs the newest few and the total.
const maxReportedSkipped = 10

// TrustReport is one trusted-main observation, bounded for display.
type TrustReport struct {
	Floor        string            `json:"floor,omitempty"`
	FloorError   string            `json:"floor_error,omitempty"`
	TrustedMain  string            `json:"trusted_main,omitempty"`
	MainHead     string            `json:"main_head,omitempty"`
	Skipped      []SkippedRevision `json:"skipped,omitempty"`
	SkippedTotal int               `json:"skipped_total"`
	// Deciding is the T2 attempt that made TrustedMain trusted.
	Deciding     *T2Attempt `json:"deciding,omitempty"`
	Inconsistent bool       `json:"inconsistent,omitempty"`
}

func (u *ControllerUpdater) trustReport(view TrustedMainView) *TrustReport {
	report := &TrustReport{
		Floor: u.ports.Floor.Revision, TrustedMain: view.TrustedMain.Revision, MainHead: view.MainHead,
		Skipped: view.Skipped, SkippedTotal: len(view.Skipped),
		Deciding: view.Evidence.Deciding, Inconsistent: view.Evidence.Inconsistent,
	}
	if u.ports.FloorError != nil {
		report.FloorError = u.ports.FloorError.Error()
	}
	if len(report.Skipped) > maxReportedSkipped {
		report.Skipped = report.Skipped[:maxReportedSkipped]
	}
	return report
}

// Lines renders the report as labelled status lines.
func (r TrustReport) Lines() [][2]string {
	floor := orUnknown(shortSHA(r.Floor))
	if r.FloorError != "" {
		floor += " (" + r.FloorError + ")"
	}
	lines := [][2]string{
		{"trust floor", floor},
		{"trusted main", orUnknown(shortSHA(r.TrustedMain))},
		{"main head", orUnknown(shortSHA(r.MainHead))},
	}
	if r.Deciding != nil {
		evidence := fmt.Sprintf("T2 %s, run %d attempt %d", r.Deciding.Conclusion, r.Deciding.RunID, r.Deciding.Attempt)
		if r.Inconsistent {
			evidence += ", inconsistent (an earlier attempt disagreed)"
		}
		lines = append(lines, [2]string{"trust evidence", evidence})
	}
	for _, skipped := range r.Skipped {
		lines = append(lines, [2]string{"not trusted", shortSHA(skipped.Revision) + ": " + skipped.Reason})
	}
	if hidden := r.SkippedTotal - len(r.Skipped); hidden > 0 {
		lines = append(lines, [2]string{"not trusted", fmt.Sprintf("… and %d older", hidden)})
	}
	return lines
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// TrustFloor is the floor a controller adopted from this provenance holds
// trust to: its recorded trusted_main, read through Projected. It is never the
// source, which may be an older commit inside a newer trusted main, and a
// record that does not project yields no floor at all rather than a fallback.
func (p AdoptedBuildProvenance) TrustFloor() (RevisionRecord, error) {
	projected, err := p.Projected()
	if err != nil {
		return RevisionRecord{}, fmt.Errorf("the running controller's provenance gives no trust floor: %w", err)
	}
	return projected.TrustedMain, nil
}

// ControllerUpdate reports the bound upgrade's latest update, if there is one.
func (s *Supervisor) ControllerUpdate() (ControllerUpdate, bool) {
	upgrade := s.controllerUpgrade()
	if upgrade == nil {
		return ControllerUpdate{}, false
	}
	return upgrade.updater.Current()
}
