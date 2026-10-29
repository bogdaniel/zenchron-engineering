package conformance

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/memory"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

var linux = intelligence.Settings{GoVersion: "go1.25.0", GOOS: "linux", GOARCH: "amd64"}

// goWorkspace writes a tiny Go module whose symbols match the objective.
func goWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/ledger\n\ngo 1.25\n")
	writeFile(t, filepath.Join(dir, "ledger", "ledger.go"),
		"package ledger\n\n// Balance sums entries.\nfunc Balance(entries []int) int {\n\ttotal := 0\n"+
			"\tfor _, e := range entries {\n\t\ttotal += e\n\t}\n\treturn total\n}\n")
	return dir
}

// corruptAll flips the first byte of every regular file under dir.
func corruptAll(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 {
			return err
		}
		data[0] ^= 0xff
		n++
		return os.WriteFile(p, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func openIndex(t *testing.T, rec storage.Records, root string) *intelligence.Index {
	t.Helper()
	ix, err := intelligence.Open(context.Background(), rec, intelligence.BuildConfig{Root: root, Settings: linux})
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func newMemory(t *testing.T, rec storage.Records) *memory.Store {
	t.Helper()
	store, err := memory.New(rec, memory.Limits{MaxRecords: 100, MaxBytes: 1 << 20}, func() time.Time { return epoch })
	if err != nil {
		t.Fatal(err)
	}
	return store
}

var partition = memory.Partition{Repository: "ledger", Workspace: "ws-1", Access: "exec"}

func remember(t *testing.T, store *memory.Store, id, content string) {
	t.Helper()
	err := store.Put(context.Background(), memory.Record{
		ID: id, Kind: memory.KindObservation, Partition: partition, Content: content,
		SourceDigests: []string{api.Digest([]byte(id))}, Derivation: memory.Derivation{Method: "test", Version: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestA15CorruptCachesYieldCorrectBoundedExecution: with the index cache and
// memory records corrupted on disk, the execution still completes inside its
// bounds, the index is rebuilt with identical facts, and both degradations
// are explicit (index stats, a memory-degraded context item).
func TestA15CorruptCachesYieldCorrectBoundedExecution(t *testing.T) {
	root := goWorkspace(t)
	indexDir, memDir := filepath.Join(t.TempDir(), "index"), filepath.Join(t.TempDir(), "memory")
	indexRecs, err := storage.OpenFileRecords(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	clean := openIndex(t, indexRecs, root)
	memRecs, err := storage.OpenFileRecords(memDir)
	if err != nil {
		t.Fatal(err)
	}
	remember(t, newMemory(t, memRecs), "balance-note", "Balance returns the sum of entries")
	if corruptAll(t, indexDir) == 0 || corruptAll(t, memDir) == 0 {
		t.Fatal("nothing was corrupted; the test is vacuous")
	}

	rebuilt := openIndex(t, indexRecs, root)
	if rebuilt.Stats().CacheHit || rebuilt.Stats().CacheDegraded == "" {
		t.Fatalf("corrupt index cache not reported: %+v", rebuilt.Stats())
	}
	if rebuilt.Key() != clean.Key() {
		t.Fatal("rebuilt index describes different bytes")
	}
	ws := api.WorkspaceRef{ID: "ws-1", ManifestDigest: rebuilt.Identity().ManifestDigest}
	view, err := intelligence.NewView(rebuilt, ws)
	if err != nil {
		t.Fatal(err)
	}
	p := scripted.New(end("ok"))
	k := newKernel(t, config{providers: providers(p),
		sources: []api.ContextSource{view, newMemory(t, memRecs).Source(partition)}})
	req := request("a15")
	req.Workspace, req.Objective = ws, "explain Balance in ledger"
	res := k.run(t, context.Background(), req)
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	if res.Usage.Iterations > req.Budget.MaxIterations {
		t.Fatalf("unbounded: %+v", res.Usage)
	}
	_, user := systemAndUser(t, p)
	if !strings.Contains(user, "Balance") {
		t.Fatal("rebuilt index contributed no context")
	}
	if strings.Contains(user, "sum of entries") {
		t.Fatal("corrupt memory record was served as data")
	}
	if !manifestHas(res, memory.DegradedItemID) {
		t.Fatal("memory degradation is not visible in the context manifest")
	}
}

// TestA15FailingSourceDegradesExplicitly: a source error never blocks the
// execution and is stated in the context.compiled observation.
func TestA15FailingSourceDegradesExplicitly(t *testing.T) {
	src := &countingSource{err: storage.ErrCorrupt}
	p := scripted.New(end("ok"))
	k := newKernel(t, config{providers: providers(p), sources: []api.ContextSource{src}})
	res := k.run(t, context.Background(), request("a15-src"))
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	for _, o := range res.Observations {
		if o.Kind == api.EventContextCompiled && strings.Contains(o.Detail, "unavailable") {
			return
		}
	}
	t.Fatal("source failure not observable")
}

func manifestHas(res api.ExecutionResult, id string) bool {
	if res.Context == nil {
		return false
	}
	for _, e := range res.Context.Entries {
		if e.ItemID == id {
			return true
		}
	}
	return false
}

// TestA16ReopenDerivedStateAfterRestart: artifacts recorded by an execution,
// a cached index and memory records reopen from their file-backed roots in
// fresh store instances, verified on read.
func TestA16ReopenDerivedStateAfterRestart(t *testing.T) {
	artDir, indexDir, memDir := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "i"), filepath.Join(t.TempDir(), "m")
	arts, err := storage.OpenFileArtifacts(artDir, 1<<22)
	if err != nil {
		t.Fatal(err)
	}
	p := scripted.New(toolUse(call("c1", "read_file", map[string]any{"path": "big.txt"})), end("ok"))
	k := newKernel(t, config{providers: providers(p), artifacts: arts})
	writeFile(t, filepath.Join(k.dir, "big.txt"), strings.Repeat("line of evidence\n", 200))
	res := k.run(t, context.Background(), request("a16"))
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	if len(res.Artifacts) == 0 {
		t.Fatal("no artifact recorded for truncated output")
	}

	root := goWorkspace(t)
	indexRecs, err := storage.OpenFileRecords(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	first := openIndex(t, indexRecs, root)
	memRecs, err := storage.OpenFileRecords(memDir)
	if err != nil {
		t.Fatal(err)
	}
	remember(t, newMemory(t, memRecs), "note", "remembered across restart")

	// "Restart": every store is reopened from its root by a new instance.
	arts2, err := storage.OpenFileArtifacts(artDir, 1<<22)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range res.Artifacts {
		data, err := arts2.Get(context.Background(), ref)
		if err != nil || api.Digest(data) != ref.Digest {
			t.Fatalf("artifact %s did not reopen: %v", ref.Digest, err)
		}
	}
	indexRecs2, err := storage.OpenFileRecords(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	second := openIndex(t, indexRecs2, root)
	if !second.Stats().CacheHit || second.Key() != first.Key() {
		t.Fatalf("index did not reopen: %+v", second.Stats())
	}
	memRecs2, err := storage.OpenFileRecords(memDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := newMemory(t, memRecs2).Get(context.Background(), partition, "note")
	if err != nil || got.Content != "remembered across restart" {
		t.Fatalf("memory did not reopen: %+v %v", got, err)
	}
}
