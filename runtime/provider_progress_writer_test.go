package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// unreadableRowProvider makes its own operation row undecodable (valid JSON,
// so the store's JSON indexes accept it) for one progress write through each
// view the runtime installed, then restores it and completes.
type unreadableRowProvider struct {
	*wallProvider
	store       *SQLiteOperationStore
	writerErr   error
	recordedKey string
	hadWriter   bool
}

func (p *unreadableRowProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	var document string
	if err := p.store.db.QueryRow(`SELECT document FROM run_operations WHERE id = ?`, request.OperationID).Scan(&document); err != nil {
		return ExecutionResult{}, err
	}
	if _, err := p.store.db.Exec(`UPDATE run_operations SET document = '[]' WHERE id = ?`, request.OperationID); err != nil {
		return ExecutionResult{}, err
	}
	write := execution.ProgressWriterFrom(ctx)
	p.hadWriter = write != nil
	if write != nil {
		p.writerErr = write(ProviderProgress{Key: "1:1"})
	}
	// The best-effort view swallows the same failure, as it always has.
	execution.ProgressRecorder(ctx)(ProviderProgress{Key: "1:2"})
	if _, err := p.store.db.Exec(`UPDATE run_operations SET document = ? WHERE id = ?`, document, request.OperationID); err != nil {
		return ExecutionResult{}, err
	}
	// Once the row is readable again the recorder writes it, as it always did.
	execution.ProgressRecorder(ctx)(ProviderProgress{Key: "1:3"})
	op, _, _, err := p.store.Operation(request.OperationID)
	if err != nil {
		return ExecutionResult{}, err
	}
	p.recordedKey = op.NoProgressKey
	return p.wallProvider.Execute(ctx, request)
}

// The runtime's fallible writer returns RecordProviderProgress's error to its
// caller instead of discarding it, while the derived recorder still drops it
// and still writes the row when the store is healthy.
func TestRuntimeProgressWriterReturnsTheRecordError(t *testing.T) {
	fixture, wall := wallFixture(t, RunBudgets{WallLimit: time.Hour}, wallStep{complete: true, mutate: true, spend: time.Second})
	provider := &unreadableRowProvider{wallProvider: wall, store: fixture.store}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)
	if !provider.hadWriter {
		t.Fatal("the runtime installed no fallible progress writer")
	}
	if provider.writerErr == nil {
		t.Fatal("RecordProviderProgress failed and the writer reported success")
	}
	if provider.recordedKey != "1:3" {
		t.Fatalf("progress key %q: the native recorder path must still write the row", provider.recordedKey)
	}
	if ops, _ := executions(t, fixture.state(runID)); len(ops) != 1 || ops[0].State != Succeeded {
		t.Fatalf("operations %+v: the swallowed recorder failure must not change the attempt", ops)
	}
}
