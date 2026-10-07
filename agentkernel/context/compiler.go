// Package context compiles a reproducible context manifest for one execution.
// Importers alias it (kcontext) to keep the standard library's context clear.
package context

import "github.com/bogdaniel/zenchron-engineering/agentkernel/api"

// Estimator counts tokens for one provider; approximate counts say so.
type Estimator func(text string) api.TokenEstimate

// Input is one compilation.
type Input struct {
	// Required items are never dropped, truncated or summarized.
	Required []api.ContextItem
	// Optional items are untrusted candidates selected within remaining capacity.
	Optional       []api.ContextItem
	Window         int64
	ReservedOutput int64
	Estimate       Estimator
}

// Output is the selection plus its manifest.
type Output struct {
	Selected []api.ContextItem
	Manifest api.ContextManifest
}

// InsufficientCapacityError reports mandatory context that cannot fit.
type InsufficientCapacityError struct {
	Needed, Available int64
}

func (e *InsufficientCapacityError) Error() string { return "required context exceeds capacity" }

// Compile selects context deterministically. It returns
// *InsufficientCapacityError when required items alone exceed capacity.
func Compile(in Input) (Output, error) { panic("lane D") }
