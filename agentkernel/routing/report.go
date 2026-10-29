package routing

import (
	"cmp"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ReportRow summarizes one provider across recorded decisions and
// observations. It is offline ranking data for #70: counts only, with unknown
// costs counted as unknown. It carries no quality claim and changes nothing.
type ReportRow struct {
	ProviderID          string `json:"provider_id"`
	Considered          int    `json:"considered"`
	Permitted           int    `json:"permitted"`
	Chosen              int    `json:"chosen"`
	Observations        int    `json:"observations"`
	KnownCostSamples    int    `json:"known_cost_samples"`
	UnknownCostSamples  int    `json:"unknown_cost_samples"`
	KnownLatencySamples int    `json:"known_latency_samples"`
}

// RoutingReport is the deterministic, JSON-serializable summary.
type RoutingReport struct {
	Decisions int         `json:"decisions"`
	Blocked   int         `json:"blocked"`
	Providers []ReportRow `json:"providers"`
}

// Report aggregates past decisions and observations, sorted by provider ID.
func Report(decisions []api.RoutingDecision, observations []Observation) RoutingReport {
	rows := map[string]*ReportRow{}
	row := func(id string) *ReportRow {
		if rows[id] == nil {
			rows[id] = &ReportRow{ProviderID: id}
		}
		return rows[id]
	}
	report := RoutingReport{Decisions: len(decisions), Providers: []ReportRow{}}
	for _, d := range decisions {
		if d.Chosen == "" {
			report.Blocked++
		}
		for _, c := range d.Candidates {
			r := row(c.ProviderID)
			r.Considered++
			if c.Eligible {
				r.Permitted++
			}
			if c.ProviderID == d.Chosen {
				r.Chosen++
			}
		}
	}
	for _, o := range observations {
		r := row(o.ProviderID)
		r.Observations++
		if o.CostMicrosPerCall == nil {
			r.UnknownCostSamples++
		} else {
			r.KnownCostSamples++
		}
		if o.MedianLatency > 0 {
			r.KnownLatencySamples++
		}
	}
	for _, r := range rows {
		report.Providers = append(report.Providers, *r)
	}
	slices.SortFunc(report.Providers, func(a, b ReportRow) int { return cmp.Compare(a.ProviderID, b.ProviderID) })
	return report
}
