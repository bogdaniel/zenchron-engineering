package controlplane

// readRunDetail shares the canonical status and journal-owned operator pause
// across ordinary reads and fresh SSE snapshots. It records no state.
func (a *API) readRunDetail(id string) (RunDetail, error) {
	status, err := a.Store.Status(id, a.now())
	if err != nil {
		return RunDetail{}, err
	}
	paused, err := a.Store.RunPaused(id)
	if err != nil {
		return RunDetail{}, err
	}
	detail := runDetailProjection(status)
	detail.Paused = &paused
	return detail, nil
}
