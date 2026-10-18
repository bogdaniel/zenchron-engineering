package benchmark

import (
	"sort"
	"time"
)

// Span is one real execution interval taken from durable state.
type Span struct {
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Concurrency is computed from real intervals, never from a configured limit.
type Concurrency struct {
	Spans int `json:"spans"`
	// Ignored counts spans with a non-positive duration, which carry no overlap.
	Ignored int `json:"ignored"`
	Peak    int `json:"peak"`
	// UnionSeconds is time with at least one span active; OverlapSeconds is time
	// with at least two; SumSeconds is the total of every span.
	UnionSeconds   float64 `json:"union_seconds"`
	OverlapSeconds float64 `json:"overlap_seconds"`
	SumSeconds     float64 `json:"sum_seconds"`
}

// ComputeConcurrency sweeps the span boundaries once.
func ComputeConcurrency(spans []Span) Concurrency {
	type edge struct {
		at    time.Time
		delta int
	}
	out := Concurrency{Spans: len(spans)}
	var edges []edge
	for _, s := range spans {
		if !s.End.After(s.Start) {
			out.Ignored++
			continue
		}
		out.SumSeconds += s.End.Sub(s.Start).Seconds()
		edges = append(edges, edge{s.Start, 1}, edge{s.End, -1})
	}
	// Ends sort before starts at the same instant: touching spans do not overlap.
	sort.Slice(edges, func(a, b int) bool {
		if !edges[a].at.Equal(edges[b].at) {
			return edges[a].at.Before(edges[b].at)
		}
		return edges[a].delta < edges[b].delta
	})
	active := 0
	for i, e := range edges {
		if i > 0 {
			gap := e.at.Sub(edges[i-1].at).Seconds()
			if active >= 1 {
				out.UnionSeconds += gap
			}
			if active >= 2 {
				out.OverlapSeconds += gap
			}
		}
		active += e.delta
		if active > out.Peak {
			out.Peak = active
		}
	}
	return out
}
