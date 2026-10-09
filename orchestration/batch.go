package orchestration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// BatchSchemaVersion versions the durable batch document.
const BatchSchemaVersion = "0.1"

// MaxBatchItems bounds one batch. The bootstrap target is ten explicit issues;
// the ceiling leaves room for that cohort without letting one request fan out
// without bound. It is not a concurrency number - the scheduler's ceiling is
// the only one of those.
const MaxBatchItems = 32

// Batch is one operator request to execute an explicit set of existing issues
// with one execution agent. It is written ONCE, with every item's child run
// identity already decided, and never updated: item state is a projection over
// the child runs, not a field of this document.
type Batch struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	Repository    string      `json:"repository"`
	AgentID       string      `json:"agent_id"`
	RequestedBy   string      `json:"requested_by,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	Items         []BatchItem `json:"items"`
	// Origin names the WorkGraph unit execution this batch exists to perform
	// (#472), and the EXACT upstream outputs it was created against.
	//
	// It is what makes a graph unit's child run its own. Without it a batch's
	// identity was the issue alone, so a unit would have silently reused
	// whatever run some earlier, unrelated orchestration of that issue had
	// already produced - a run that never executed against these inputs - and
	// read as satisfied by it.
	//
	// It is a POINTER member of the identity digest and omitempty, so a direct
	// operator batch digests, and reads back, exactly as it did before work
	// graphs existed.
	Origin *BatchOrigin `json:"origin,omitempty"`
}

// BatchOrigin is one WorkGraph unit execution: which unit, under which graph,
// against which exact admitted upstream outputs.
//
// Inputs is the whole input set, not its digest, because the batch is the
// durable record the child run's execution is reconstructed from: the inputs it
// was activated against have to be readable, not merely checkable.
type BatchOrigin struct {
	GraphID string         `json:"graph_id"`
	UnitID  string         `json:"unit_id"`
	Inputs  WorkUnitInputs `json:"inputs,omitempty"`
	// ExecutionKind is the unit's own execution algorithm (#475), exactly as
	// the WorkGraph decided it at adoption - never re-derived from Role, this
	// issue's purpose, title or anything a provider wrote. It is what #475
	// reads to dispatch an integration_compose unit's run to deterministic
	// composition instead of an ordinary execution invocation, and it is
	// frozen the moment this batch is written: ValidateMutation already
	// refuses a graph revision that changes an activated unit's execution
	// kind, so this field can never disagree with the WorkGraph that named
	// it.
	//
	// Omitempty and additive: a batch stored before this field existed
	// decodes with it empty, which is exactly ExecutionKindProvider - the
	// safe default for every batch #475 did not create.
	ExecutionKind WorkUnitExecutionKind `json:"execution_kind,omitempty"`
}

// canonical is the origin with its input set in canonical order. The identity
// below digests this rather than the member directly: the inputs are a SET, and
// an identity that moved with the order a caller happened to build them in would
// name two batches for one unit execution.
func (o BatchOrigin) canonical() BatchOrigin {
	o.Inputs = o.Inputs.canonical()
	return o
}

// Validate refuses an origin that does not name one unit execution.
func (o BatchOrigin) Validate() error {
	if strings.TrimSpace(o.GraphID) == "" || strings.TrimSpace(o.UnitID) == "" {
		return errors.New("a work graph batch origin names its graph and its unit")
	}
	return o.Inputs.Validate()
}

// BatchItem is one explicit issue and the one child run that performs it.
type BatchItem struct {
	Issue int    `json:"issue"`
	RunID string `json:"run_id"`
}

// NormalizeIssues refuses anything but a bounded set of distinct positive
// issue numbers, and returns them in ascending order. A duplicate is refused
// rather than collapsed: the operator stated it twice, and guessing which one
// they meant is not this function's job.
func NormalizeIssues(issues []int) ([]int, error) {
	if len(issues) == 0 {
		return nil, errors.New("an orchestration batch names at least one explicit issue")
	}
	if len(issues) > MaxBatchItems {
		return nil, fmt.Errorf("an orchestration batch names %d issues, above the %d issue bound", len(issues), MaxBatchItems)
	}
	seen := make(map[int]bool, len(issues))
	out := make([]int, 0, len(issues))
	for _, issue := range issues {
		if issue <= 0 {
			return nil, fmt.Errorf("issue number must be positive, got %d", issue)
		}
		if seen[issue] {
			return nil, fmt.Errorf("issue %d is named more than once", issue)
		}
		seen[issue] = true
		out = append(out, issue)
	}
	sort.Ints(out)
	return out, nil
}

// BatchID is the deterministic identity of a direct operator request: the same
// repository, agent and issue set always name the same batch. That is what makes
// a lost control reply harmless - sending the same request again finds the batch
// it already created instead of creating a second fleet.
func BatchID(repository, agentID string, issues []int) (string, error) {
	return batchIdentity(repository, agentID, issues, nil)
}

// WorkUnitBatchID is the deterministic identity of ONE WorkGraph unit execution
// (#472): the same unit of the same graph against the same exact inputs always
// names the same batch, and a different unit, graph or input set never does.
//
// That is the whole correctness property. Replaying a crashed activation finds
// the batch and child run it already created; nothing else can, so no unit can
// be satisfied by work performed for something other than itself against other
// inputs.
func WorkUnitBatchID(repository, agentID string, issue int, origin BatchOrigin) (string, error) {
	if err := origin.Validate(); err != nil {
		return "", err
	}
	return batchIdentity(repository, agentID, []int{issue}, &origin)
}

func batchIdentity(repository, agentID string, issues []int, origin *BatchOrigin) (string, error) {
	normalized, err := NormalizeIssues(issues)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(agentID) == "" {
		return "", errors.New("an orchestration batch needs a repository and an execution agent")
	}
	identity := origin
	if origin != nil {
		canonical := origin.canonical()
		identity = &canonical
	}
	digest, err := domain.Digest(struct {
		Repository string       `json:"repository"`
		Agent      string       `json:"agent"`
		Issues     []int        `json:"issues"`
		Origin     *BatchOrigin `json:"origin,omitempty"`
	}{strings.ToLower(repository), agentID, normalized, identity})
	if err != nil {
		return "", err
	}
	return "batch-" + digest[:32], nil
}

// Validate refuses a batch document that is not exactly what its identity
// says it is. A stored batch that fails this is corrupt, and is reported as
// such rather than repaired.
func (b Batch) Validate() error {
	if b.SchemaVersion != BatchSchemaVersion {
		return fmt.Errorf("orchestration batch schema version %q is not %q", b.SchemaVersion, BatchSchemaVersion)
	}
	issues := make([]int, 0, len(b.Items))
	runs := make(map[string]bool, len(b.Items))
	for _, item := range b.Items {
		if strings.TrimSpace(item.RunID) == "" || runs[item.RunID] {
			return fmt.Errorf("orchestration batch item for issue %d has a missing or repeated child run", item.Issue)
		}
		runs[item.RunID] = true
		issues = append(issues, item.Issue)
	}
	if b.Origin != nil {
		if err := b.Origin.Validate(); err != nil {
			return err
		}
		// A unit performs ONE issue. An origin over several would claim one
		// unit execution produced several issues' output.
		if len(b.Items) != 1 {
			return fmt.Errorf("orchestration batch %s names a work graph unit and %d issues; a unit performs one", b.ID, len(b.Items))
		}
	}
	id, err := batchIdentity(b.Repository, b.AgentID, issues, b.Origin)
	if err != nil {
		return err
	}
	if id != b.ID {
		return fmt.Errorf("orchestration batch %s does not match the identity %s of its own contents", b.ID, id)
	}
	if b.CreatedAt.IsZero() {
		return errors.New("orchestration batch creation time is required")
	}
	return nil
}
