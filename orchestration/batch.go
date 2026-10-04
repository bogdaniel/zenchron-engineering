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

// BatchID is the deterministic identity of a request: the same repository,
// agent and issue set always name the same batch. That is what makes a lost
// control reply harmless - sending the same request again finds the batch it
// already created instead of creating a second fleet.
func BatchID(repository, agentID string, issues []int) (string, error) {
	normalized, err := NormalizeIssues(issues)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(agentID) == "" {
		return "", errors.New("an orchestration batch needs a repository and an execution agent")
	}
	digest, err := domain.Digest(struct {
		Repository string `json:"repository"`
		Agent      string `json:"agent"`
		Issues     []int  `json:"issues"`
	}{strings.ToLower(repository), agentID, normalized})
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
	id, err := BatchID(b.Repository, b.AgentID, issues)
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
