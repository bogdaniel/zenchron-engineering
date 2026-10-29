package runtime

import "strings"

// ClassifyProviderFailure preserves the existing narrow capacity semantics:
// only recognizable transient-capacity diagnostics may consume retry/fallback
// budget. Every other provider failure is diagnosable and bounded as unknown.
func ClassifyProviderFailure(stdout, stderr []byte) FailureClass {
	diagnostic := strings.ToLower(string(stdout) + "\n" + string(stderr))
	for _, signal := range []string{"model is at capacity", "selected model is at capacity", "capacity. please try", "temporarily unavailable"} {
		if strings.Contains(diagnostic, signal) {
			return FailureTransientProvider
		}
	}
	return FailureUnknown
}
