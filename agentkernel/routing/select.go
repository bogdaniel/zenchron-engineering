// Package routing ranks providers deterministically inside the host-supplied
// eligible set. A score never makes a provider eligible.
package routing

import (
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Observation is optional historical data about one provider. It may inform
// ranking among eligible bindings only.
type Observation struct {
	ProviderID    string
	MedianLatency time.Duration
	// CostMicrosPerCall is nil when unknown; unknown is never zero.
	CostMicrosPerCall *int64
	ObservedAt        time.Time
}

// Input is one routing decision.
type Input struct {
	Bindings                   []api.ProviderBinding
	RequireHostProvenIsolation bool
	RequiredFeatures           []string
	Observations               []Observation
	Now                        time.Time
	// StaleAfter marks observations older than this as stale.
	StaleAfter time.Duration
}

// Select returns the decision; Chosen is empty and Blocked set when no
// binding is permitted.
func Select(in Input) api.RoutingDecision { panic("lane D") }
