package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	kcontext "github.com/bogdaniel/zenchron-engineering/agentkernel/context"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// compile selects context and renders the opening transcript. Required
// request items are never dropped; if they cannot fit, the execution blocks
// with insufficient_capacity before any provider call.
func (r *run) compile(ctx context.Context, b api.ProviderBinding, specs []api.ToolSpec) ([]api.Message, api.Termination, bool) {
	var required, optional []api.ContextItem
	for _, it := range r.req.Context {
		if it.Required {
			required = append(required, it)
		} else {
			optional = append(optional, it)
		}
	}
	sourced, notes := r.sourceItems(ctx)
	optional = append(optional, sourced...)

	boundary := boundaryText(r.req, specs)
	reserved := min(b.MaxOutputTokens, r.req.Budget.MaxOutputTokens)
	// The boundary, objective framing and tool specs occupy the prompt too.
	overhead := kcontext.ApproximateTokens(boundary + objectiveText(r.req.Objective) + promptText(nil, specs)).Count
	input := min(b.ContextWindow-reserved, r.req.Budget.MaxInputTokens) - overhead
	if input <= 0 {
		return nil, r.termination(api.OutcomeBlocked, api.CauseInsufficientCapacity,
			fmt.Sprintf("prompt overhead of %d tokens leaves no input capacity", overhead)), false
	}
	out, err := kcontext.Compile(kcontext.Input{
		Required: required, Optional: optional, Window: input + reserved, ReservedOutput: reserved, Estimate: kcontext.ApproximateTokens,
	})
	var capErr *kcontext.InsufficientCapacityError
	if errors.As(err, &capErr) {
		return nil, r.termination(api.OutcomeBlocked, api.CauseInsufficientCapacity,
			fmt.Sprintf("required context needs %d tokens, %d available", capErr.Needed, capErr.Available)), false
	}
	if err != nil {
		return nil, r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, err.Error()), false
	}
	r.manifest = &out.Manifest
	r.provenance.ContextManifestDigest = out.Manifest.Digest
	detail := "manifest " + out.Manifest.Digest
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	if err := r.emit(ctx, api.Event{Kind: api.EventContextCompiled, Detail: detail}); err != nil {
		return nil, r.recordingFailed(""), false
	}
	return []api.Message{
		{Role: api.RoleSystem, Content: renderSystem(boundary, out.Selected)},
		{Role: api.RoleUser, Content: renderUser(r.req.Objective, out.Selected)},
	}, api.Termination{}, true
}

// sourceItems gathers optional context. Whatever a source claims, its items
// are optional and never host-trusted; an item that is invalid once demoted
// (a "host instruction" from memory, say) is dropped and noted. A failing
// source degrades efficiency, never correctness, so it is noted, not fatal.
func (r *run) sourceItems(ctx context.Context) ([]api.ContextItem, []string) {
	query := api.ContextQuery{
		ExecutionID: r.req.ExecutionID, Objective: r.req.Objective, Workspace: r.req.Workspace, Limit: sourceItemLimit,
	}
	requestIDs := map[string]bool{}
	for _, it := range r.req.Context {
		requestIDs[it.ID] = true
	}
	var items []api.ContextItem
	var notes []string
	for i, src := range r.e.sources {
		got, err := r.askSource(ctx, i, src, query)
		if err != nil {
			notes = append(notes, fmt.Sprintf("source %d unavailable: %v", i, err))
			continue
		}
		for _, it := range got[:min(len(got), sourceItemLimit)] {
			it.Required = false
			if it.Trust == api.TrustHost {
				it.Trust = api.TrustMemory
			}
			// A sourced item sharing a request item's id would supersede it.
			if requestIDs[it.ID] {
				notes = append(notes, fmt.Sprintf("source %d item %q dropped: id belongs to a request item", i, it.ID))
				continue
			}
			if err := it.Validate(); err != nil {
				notes = append(notes, fmt.Sprintf("source %d item %q dropped: %v", i, it.ID, err))
				continue
			}
			items = append(items, it)
		}
	}
	return items, notes
}

// askSource hands query to source i's host worker and waits within the
// recording bound; no answer in time is an unavailable source.
func (r *run) askSource(ctx context.Context, i int, src chan<- api.ContextRequest, q api.ContextQuery) ([]api.ContextItem, error) {
	b := r.recordingBound(false)
	callCtx, cancel := recordingContext(ctx, b)
	defer cancel()
	id := fmt.Sprintf("%s/%s/source-%d", r.req.ExecutionID, r.req.AttemptID, i)
	reply, err := handoff.Exchange(fmt.Sprintf("context source %d", i), src, api.ContextRequest{ID: id, Context: callCtx, Request: q}, b)
	if err != nil {
		return nil, err
	}
	return reply.Value, reply.Err
}
