package agentkernel

import (
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// One host operation is one kernel execution; one physical attempt is one
// attempt_id (integration plan §5.11).
func TestIdentityIsOneExecutionPerOperationAndOneAttemptIDPerAttempt(t *testing.T) {
	first := execution.AttemptRef{RunID: "A", OperationID: "B", Attempt: 1}
	second := execution.AttemptRef{RunID: "A", OperationID: "B", Attempt: 2}
	if executionID(first) != executionID(second) {
		t.Errorf("attempts of one operation have execution ids %q and %q: a retry would escape the envelope",
			executionID(first), executionID(second))
	}
	if attemptID(first) == attemptID(second) {
		t.Errorf("attempts 1 and 2 share attempt id %q", attemptID(first))
	}
	if executionID(first) != executionID(first) || attemptID(first) != attemptID(first) {
		t.Error("replaying (A,B,1) does not reproduce its identity")
	}
	ref := func(run, op string, attempt int) execution.AttemptRef {
		return execution.AttemptRef{RunID: run, OperationID: op, Attempt: attempt}
	}
	for name, pair := range map[string][2]execution.AttemptRef{
		"other run":       {first, ref("A2", "B", 1)},
		"other operation": {first, ref("A", "B2", 1)},
		// Raw concatenation with a separator would make each pair collide.
		"separator in run vs operation": {ref("a:b", "c", 1), ref("a", "b:c", 1)},
		"digits across the boundary":    {ref("A", "B1", 1), ref("A", "B", 11)},
	} {
		if executionID(pair[0]) == executionID(pair[1]) {
			t.Errorf("%s: execution ids collide: %q", name, executionID(pair[0]))
		}
		if attemptID(pair[0]) == attemptID(pair[1]) {
			t.Errorf("%s: attempt ids collide: %q", name, attemptID(pair[0]))
		}
	}
}

// Whatever characters or length the host's ids have, the encoding is a valid
// kernel identifier.
func TestIdentityIsAlwaysAValidKernelIdentifier(t *testing.T) {
	long := string(make([]byte, 4096))
	for _, ref := range []execution.AttemptRef{
		{RunID: "run-1", OperationID: "op-1", Attempt: 1},
		{RunID: "run with spaces/and/slashes", OperationID: "ops ünïcode ✓", Attempt: 7},
		{RunID: long, OperationID: long, Attempt: 1 << 30},
	} {
		for _, id := range []string{executionID(ref), attemptID(ref)} {
			if !api.ValidIdentifier(id) {
				t.Errorf("%q is not a valid kernel identifier", id)
			}
		}
	}
}
