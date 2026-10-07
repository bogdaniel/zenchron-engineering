package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeVerificationFixture(t *testing.T, v verificationExecution, target string, ctx context.Context) (string, verificationToolGrant, func() error) {
	t.Helper()
	bin, err := prepareVerificationTools(v, []string{"probe"}, []string{os.Args[0]}, target+string(os.PathListSeparator)+os.Getenv("PATH"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(filepath.Dir(bin), "grant.json")
	var grant verificationToolGrant
	if err := readVerificationMessage(descriptor, &grant); err != nil {
		t.Fatal(err)
	}
	_, stop, err := serveVerificationTools(ctx, v, grant)
	if err != nil {
		t.Fatal(err)
	}
	var published verificationToolGrant
	if err := readVerificationMessage(descriptor, &published); err != nil {
		t.Fatal(err)
	}
	return descriptor, published, stop
}

func TestNestedVerificationNativeToolCancellationFuseAndShutdown(t *testing.T) {
	for _, cause := range []string{"cancel", "fuse", "shutdown"} {
		t.Run(cause, func(t *testing.T) {
			dir, store, s := toolFixture(t, 1)
			target := t.TempDir()
			fifo := filepath.Join(target, "block")
			if _, err := (OSCommandExecutor{}).Run(context.Background(), "mkfifo", []string{fifo}, target, os.Environ(), time.Second); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(target, "started")
			script := "#!/bin/sh\nprintf '%s' \"$$\" > \"$1\"\nexec cat \"$2\"\n"
			if err := os.WriteFile(filepath.Join(target, "probe"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := store.PutRun(newJournalRun("producer")); err != nil {
				t.Fatal(err)
			}
			planKind(t, s, "producer", OpExecutionInvoke)
			leased := mustNext(t, s, "producer")
			parent, err := s.StartWithin(leased.ID, &AttemptLimit{Within: 3 * time.Second, Bound: BoundAttemptWall})
			if err != nil {
				t.Fatal(err)
			}
			v := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
			service, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			descriptor, _, stop := nativeVerificationFixture(t, v, target, service)
			t.Cleanup(func() {
				if err := stop(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := RunVerificationTool(ctx, descriptor, "probe", []string{marker, fifo})
				finished <- err
			}()
			if err := waitForFile(marker, 2*time.Second); err != nil {
				t.Fatal(err)
			}
			if occupied, err := s.VerificationSaturated("other"); !occupied || err != nil {
				t.Fatal("running native tool did not own capacity")
			}
			switch cause {
			case "cancel":
				cancel()
			case "shutdown":
				shutdown()
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("interrupted native tool succeeded")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("native tool outlived its bound")
			}
			if processFromFileAlive(marker) {
				t.Fatal("released while native workload still alive")
			}
			// A stopped transport cannot acknowledge cleanup. Recovery retains the
			// grant until expiry and positive ownership-lock death evidence agree.
			s.Clock = &fakeClock{now: time.Now().Add(2 * time.Minute)}
			if err := s.reclaimVerificationPermits(); err != nil {
				t.Fatal(err)
			}
			if occupied, err := s.VerificationSaturated("other"); occupied || err != nil {
				t.Fatal("stopped workload leaked verification capacity")
			}
		})
	}
}

func TestNestedVerificationScratchCannotForgeAControllerGrant(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	holder := nestedParent(t, store, s, "holder")
	v := verificationExecution{s, ExecutionAttemptRef{RunID: holder.RunID, OperationID: holder.ID, Attempt: holder.AttemptIdentity}, dir}
	p, lock, err := v.begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.finish(p, lock, nil); err != nil {
			t.Error(err)
		}
	})
	parent := nestedParent(t, store, s, "native")
	native := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
	target := t.TempDir()
	marker := filepath.Join(target, "executed")
	if err := os.WriteFile(filepath.Join(target, "probe"), []byte("#!/bin/sh\nprintf ran > "+shellSingleQuoted(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	descriptor, grant, stop := nativeVerificationFixture(t, native, target, context.Background())
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := RunVerificationTool(ctx, descriptor, "probe", nil); finished <- err }()
	awaitVerificationWait(t, store, parent.RunID)
	entries, err := os.ReadDir(grant.RequestDir)
	if err != nil {
		t.Fatal(err)
	}
	var request verificationRequest
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".request.json") {
			if err := readVerificationMessage(filepath.Join(grant.RequestDir, entry.Name()), &request); err != nil {
				t.Fatal(err)
			}
		}
	}
	if request.ID == "" {
		t.Fatal("no native request")
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		t.Fatal(err)
	}
	var fake VerificationPermit
	for _, p := range permits {
		if p.Parent == native.Parent {
			fake = p
		}
	}
	now := time.Now().UTC()
	fake.State = VerificationGranted
	fake.GrantedAt = &now
	if err := writeVerificationMessage(filepath.Join(grant.RequestDir, request.ID+".reply.json"), verificationReply{Permit: fake}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("worker-forged grant executed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forged grant was not refused")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("tool ran without controller authority")
	}
	recorded, _, _, err := store.VerificationPermit(fake.ID)
	if err != nil || recorded.State == VerificationGranted {
		t.Fatal("scratch message altered durable admission")
	}
}

func TestNestedVerificationDescendantOwnershipSurvivesWrapperDeath(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	parent := nestedParent(t, store, s, "producer")
	v := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
	p, lock, err := v.begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := t.TempDir()
	fifo := filepath.Join(target, "block")
	if _, err := (OSCommandExecutor{}).Run(ctx, "mkfifo", []string{fifo}, target, os.Environ(), time.Second); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "started")
	child := exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' \"$$\" > \"$1\"; exec cat \"$2\"", "probe", marker, fifo)
	child.ExtraFiles = []*os.File{lock.file}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = child.Wait() }()
	if err := waitForFile(marker, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// The wrapper's descriptor is gone, as after process death. The child
	// retains the same inode and lock; never unlink that ownership path.
	if err := lock.file.Close(); err != nil {
		t.Fatal(err)
	}
	lock.file = nil
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	s.Clock = &fakeClock{now: time.Now().Add(2 * time.Minute)}
	if err := s.reclaimVerificationPermits(); err != nil {
		t.Fatal(err)
	}
	if saturated, err := s.VerificationSaturated("other"); !saturated || err != nil {
		t.Fatal("restart released a live descendant after wrapper death")
	}
	cancel()
	if err := child.Wait(); err == nil {
		t.Fatal("interrupted descendant succeeded")
	}
	if processFromFileAlive(marker) {
		t.Fatal("tool death was not established")
	}
	if err := s.reclaimVerificationPermits(); err != nil {
		t.Fatal(err)
	}
	current, _, _, err := reopened.VerificationPermit(p.ID)
	if err != nil || current.State != VerificationReleased {
		t.Fatal("proved descendant death did not release expired capacity")
	}
}

func TestNestedVerificationKilledWrapperRetainsCapacityThroughGuardCleanup(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	s.LeaseDuration = 100 * time.Millisecond
	parent := nestedParent(t, store, s, "producer")
	v := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
	target := t.TempDir()
	fifo := filepath.Join(target, "block")
	if _, err := (OSCommandExecutor{}).Run(context.Background(), "mkfifo", []string{fifo}, target, os.Environ(), time.Second); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "started")
	executionCount := filepath.Join(target, "executions")
	// Tools can close inherited descriptors. The existing death guard must
	// therefore retain ownership throughout its graceful/forced cleanup.
	script := "#!/bin/sh\nprintf . >> \"$3\"\nexec 3>&-\ntrap '' TERM HUP INT\nprintf '%s' \"$$\" > \"$1\"\nexec cat \"$2\"\n"
	if err := os.WriteFile(filepath.Join(target, "probe"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	descriptor, grant, stop := nativeVerificationFixture(t, v, target, context.Background())
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	wrapper := exec.Command(os.Args[0], "-test.run=^TestVerificationBrokerHelper$", "--", descriptor, "probe", marker, fifo, executionCount)
	wrapper.Env = append(os.Environ(), "ZENCHRON_TEST_VERIFICATION=1")
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wrapper.Process.Kill(); _ = wrapper.Wait() }()
	if err := waitForFile(marker, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wrapper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := wrapper.Wait(); err == nil {
		t.Fatal("killed wrapper succeeded")
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	s.Clock = &fakeClock{now: time.Now().Add(2 * time.Minute)}
	if err := s.reclaimVerificationPermits(); err != nil {
		t.Fatal(err)
	}
	if !processFromFileAlive(marker) {
		t.Fatal("fixture did not reach the guard cleanup interval")
	}
	if saturated, err := s.VerificationSaturated("other"); !saturated || err != nil {
		t.Fatal("restart released capacity while the death guard was still cleaning up a live tool")
	}
	// No further Scheduler.Next, acquisition, release request, or service stop:
	// the existing controller request loop must converge the orphan itself.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var released VerificationPermit
	for released.State != VerificationReleased {
		permits, err := reopened.VerificationPermits()
		if err != nil {
			t.Fatal(err)
		}
		if len(permits) == 0 {
			entries, err := os.ReadDir(grant.RequestDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".request.json") {
					continue
				}
				var request verificationRequest
				if err := readVerificationMessage(filepath.Join(grant.RequestDir, entry.Name()), &request); err != nil {
					t.Fatal(err)
				}
				reply, err := readVerificationReply(grant, request)
				if err != nil {
					t.Fatal(err)
				}
				released = reply.Permit
			}
		}
		if released.State == VerificationReleased {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("request service stranded a granted orphan")
		}
	}
	if processFromFileAlive(marker) {
		t.Fatal("released capacity before tool cleanup")
	}
	current, _, found, err := reopened.VerificationPermit(released.ID)
	if err != nil || !found || current.State != VerificationReleased || current.ReleasedAt == nil {
		t.Fatalf("release not durable: %+v/%v", current, err)
	}
	v.Scheduler.Store = reopened
	silence, err := v.silence(*current.GrantedAt, current.ReleasedAt.Add(5*time.Second))
	if err != nil || silence != 5*time.Second {
		t.Fatalf("orphan still suppresses inactivity: %s/%v", silence, err)
	}
	count, err := os.ReadFile(executionCount)
	if err != nil || string(count) != "." {
		t.Fatalf("tool execution duplicated: %q/%v", count, err)
	}
	operations, err := reopened.Operations(parent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].AttemptIdentity != parent.AttemptIdentity {
		t.Fatal("orphan recovery duplicated the parent attempt")
	}
}

func TestNestedVerificationToolLockOverridesTheSchedulerProcessProbe(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	parent := nestedParent(t, store, s, "producer")
	v := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
	p, lock, err := v.begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.finish(p, lock, nil); err != nil {
			t.Error(err)
		}
	})
	s.Clock = &fakeClock{now: time.Now().Add(2 * time.Minute)}
	// A process probe can report the wrapper gone while an ownership lock
	// still covers its live execution. The durable tool binding must win.
	s.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner == s.Owner })
	if _, err := s.Next(parent.RunID); err != nil {
		t.Fatal(err)
	}
	if saturated, err := s.VerificationSaturated("other"); !saturated || err != nil {
		t.Fatal("a process-only probe released a held tool ownership lock")
	}
}
