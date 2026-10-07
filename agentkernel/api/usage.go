package api

// TokenUsage is a set of token counts. A nil count is unknown, never zero.
// Input is every input token; CachedInput (served from a provider cache) and
// CacheWriteInput (written to one) are the parts of Input billed at their own
// rates.
type TokenUsage struct {
	Input           *int64 `json:"input"`
	Output          *int64 `json:"output"`
	CachedInput     *int64 `json:"cached_input"`
	CacheWriteInput *int64 `json:"cache_write_input"`
}

// Usage is the resource account of one execution. Provider-reported counts and
// local estimates are kept apart and never summed into one number.
type Usage struct {
	Reported      TokenUsage `json:"reported"`
	Estimated     TokenUsage `json:"estimated"`
	ProviderCalls int        `json:"provider_calls"`
	ToolCalls     int        `json:"tool_calls"`
	Iterations    int        `json:"iterations"`
	Retries       int        `json:"retries"`
	ArtifactBytes int64      `json:"artifact_bytes"`
	LatencyMillis int64      `json:"latency_millis"`
	Cost          Cost       `json:"cost"`
	// Unknowns names every dimension the account could not establish.
	Unknowns []string `json:"unknowns,omitempty"`
}

// Cost is a monetary account. Known is false whenever any contributing call
// lacked trusted pricing or reported usage; Micros is then a lower bound.
type Cost struct {
	Known       bool   `json:"known"`
	Micros      int64  `json:"micros"`
	Currency    string `json:"currency,omitempty"`
	RateSource  string `json:"rate_source,omitempty"`
	RateVersion string `json:"rate_version,omitempty"`
}

// Count returns a pointer to n, for building TokenUsage literals.
func Count(n int64) *int64 { return &n }
