package main

// metric extracts one value from an observation; ok is false when the run
// did not establish it (unknown, or not applicable to the mode).
type metric func(o observation) (float64, bool)

func summarizeGroup(m mode, obs []observation) group {
	g := group{Denominators: newDenominators(0)}
	var executed []observation
	for _, o := range obs {
		g.Denominators.add(o)
		if o.HarnessError == "" {
			executed = append(executed, o)
		}
	}
	measure := func(m metric) spread {
		var values []float64
		for _, o := range executed {
			if v, ok := m(o); ok {
				values = append(values, v)
			}
		}
		return spreadOf(values, len(executed)-len(values))
	}
	g.WallMillis = measure(func(o observation) (float64, bool) { return o.WallMillis, true })
	intel := m != modeBaseline
	g.IndexOpenMillis = applicable(intel, measure(func(o observation) (float64, bool) {
		if o.Index == nil {
			return 0, false
		}
		return o.Index.OpenMillis, true
	}))
	g.RebuildMillis = applicable(m == modeDrift, measure(func(o observation) (float64, bool) {
		if o.Index == nil || o.Index.RebuildMillis == nil {
			return 0, false
		}
		return *o.Index.RebuildMillis, true
	}))
	g.RetrievalMillis = applicable(intel, measure(retrievalMillis))
	g.EstimatedInputTokens = measure(func(o observation) (float64, bool) { return count(o.Estimated.Input) })
	g.ReportedInputTokens = measure(func(o observation) (float64, bool) { return count(o.Reported.Input) })
	g.ToolCalls = measure(func(o observation) (float64, bool) { return float64(o.ToolCalls), true })
	g.ToolOutputBytes = measure(func(o observation) (float64, bool) { return float64(o.ToolOutputBytes), true })
	g.FilesRead = measure(func(o observation) (float64, bool) {
		if o.FilesRead == nil {
			return 0, false
		}
		return float64(*o.FilesRead), true
	})
	g.DerivedStoreBytes = applicable(intel, measure(func(o observation) (float64, bool) {
		return float64(o.StoreBytes), o.Index != nil
	}))
	for _, o := range executed {
		g.addCounts(o)
	}
	return g
}

func (g *group) addCounts(o observation) {
	if o.Rereads != nil {
		g.RedundantRereads += o.Rereads.Redundant
		g.JustifiedRereads += o.Rereads.Justified
	}
	g.Retries += o.Retries
	if o.Memory != nil {
		g.MemoryItemsSelected += o.Memory.ItemsSelected
		g.MemoryInvalidated += o.Memory.Invalidated
	}
	if o.Index == nil {
		return
	}
	if o.Index.CacheHit {
		g.CacheHits++
	}
	if o.Index.CacheDegraded != "" {
		g.CacheDegraded++
	}
	switch m := o.Index.OverlayMatchesRebuild; {
	case m == nil:
	case *m:
		g.OverlayMatchesRebuild++
	default:
		g.OverlayDiffers++
	}
}

func retrievalMillis(o observation) (float64, bool) {
	if len(o.Retrieval) == 0 {
		return 0, false
	}
	total := 0.0
	for _, r := range o.Retrieval {
		total += r.Millis
	}
	return total, true
}

func count(n *int64) (float64, bool) {
	if n == nil {
		return 0, false
	}
	return float64(*n), true
}

func applicable(ok bool, s spread) *spread {
	if !ok {
		return nil
	}
	return &s
}
