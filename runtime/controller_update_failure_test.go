package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A REFUSED UPGRADE NAMES ITS STEP. The live controller twice refused trusted
// main with only "exit status 1" (#674). The refusal now carries the failing
// step, the attempts, the Docker operation and the sanitised stderr, and the
// controller's status record keeps them as a structured field.
func TestARefusedUpgradeCarriesTheFailingStepIntoTheUpdateRecord(t *testing.T) {
	cause := &AdoptedBuildFailure{
		Step: "toolchain", Attempts: 3, Transient: true,
		OperationID: "adopted-build-toolchain-" + movedRevision, Container: "zenchron-0123456789ab",
		Stderr: "Conflict. The container name is already in use",
		Err:    errors.New("exit status 1"),
	}
	harness := &updaterHarness{
		trusted:  RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
		buildErr: fmt.Errorf("the adopted build failed, so no binary was produced: %w", cause),
	}
	refused := settle(t, newUpdater(t, harness), UpdateRefused)

	if refused.Failure == nil {
		t.Fatalf("the refusal dropped its build step: %+v", refused)
	}
	got := *refused.Failure
	if got.Step != "toolchain" || got.Attempts != 3 || !got.Transient ||
		got.OperationID != cause.OperationID || got.Container != cause.Container || got.Stderr != cause.Stderr {
		t.Fatalf("the failure was not carried faithfully: %+v", got)
	}
	// The structured form is what `controller status --json` prints.
	encoded, err := json.Marshal(refused)
	if err != nil || !strings.Contains(string(encoded), `"failure":{"step":"toolchain","attempts":3,"transient":true`) {
		t.Fatalf("the status JSON omits the failure: %s (%v)", encoded, err)
	}
	// And the human line still carries the cause, stderr included.
	if line := refused.Describe(); !strings.Contains(line, "already in use") || !strings.Contains(line, "toolchain step") {
		t.Fatalf("the upgrade line lost the step or the stderr: %s", line)
	}
}

// A refusal that is not a sandbox build step (a trust refusal) has no failure
// record: the field describes build steps and must not be invented for the
// rest.
func TestARefusalThatIsNotABuildStepHasNoFailureRecord(t *testing.T) {
	harness := &updaterHarness{
		trusted:  RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
		buildErr: errors.New("trusted main could not be proven contained"),
	}
	if refused := settle(t, newUpdater(t, harness), UpdateRefused); refused.Failure != nil {
		t.Fatalf("a trust refusal was described as a build step: %+v", refused.Failure)
	}
}

func TestTheUpdateFailureLinesSayWhetherTheRetryWasSpent(t *testing.T) {
	spent := UpdateFailure{Step: "build", Attempts: 3, Transient: true, OperationID: "adopted-build-x", Container: "zenchron-1"}.Lines()
	if !strings.Contains(spent[0][1], "bounded retry spent") || spent[1][1] != "adopted-build-x" || spent[2][1] != "zenchron-1" {
		t.Fatalf("lines = %v", spent)
	}
	deterministic := UpdateFailure{Step: "build", Attempts: 1, OperationID: "adopted-build-x"}.Lines()
	if !strings.Contains(deterministic[0][1], "deterministic (not retried)") {
		t.Fatalf("lines = %v", deterministic)
	}
}
