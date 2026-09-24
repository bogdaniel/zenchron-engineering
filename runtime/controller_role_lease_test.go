package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE RACE THAT MATTERS: an operation inside the authority section, a release
// arriving mid-flight, and the guarantee that once the release returns nothing
// authorized by that lease can still commit.
//
// It is the same property #271 established for work admission, on a different
// resource: act while authority is structurally held, never check and act later.
func TestReleaseWaitsForAuthorityAndNothingCommitsAfterIt(t *testing.T) {
	state := t.TempDir()
	lease, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}

	var committed int64
	var entered sync.WaitGroup
	var finished sync.WaitGroup
	hold := make(chan struct{})

	const operations = 16
	for i := 0; i < operations; i++ {
		entered.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			once := sync.OnceFunc(entered.Done)
			_ = lease.WithAuthority(func() error {
				once()
				<-hold
				atomic.AddInt64(&committed, 1)
				return nil
			})
			once()
		}()
	}
	entered.Wait()

	released := make(chan struct{})
	go func() { _ = lease.Release(); close(released) }()
	select {
	case <-released:
		t.Fatal("the release returned while privileged operations were still committing")
	case <-time.After(50 * time.Millisecond):
	}
	close(hold)
	<-released

	atRelease := atomic.LoadInt64(&committed)
	finished.Wait()
	if after := atomic.LoadInt64(&committed); after != atRelease {
		t.Fatalf("%d operation(s) committed after the release returned", after-atRelease)
	}
	if atRelease == 0 {
		t.Fatal("the test did not exercise in-flight authority")
	}

	// And nothing new is authorized, in any interleaving.
	var late int64
	var lateGroup sync.WaitGroup
	for i := 0; i < 8; i++ {
		lateGroup.Add(1)
		go func() {
			defer lateGroup.Done()
			_ = lease.WithAuthority(func() error { atomic.AddInt64(&late, 1); return nil })
		}()
	}
	lateGroup.Wait()
	if late != 0 {
		t.Fatalf("%d operation(s) were authorized by a released lease", late)
	}
}

// Release is terminal, and reacquisition is a NEW capability rather than a
// revived one.
func TestReleasedLeaseIsNeverRevived(t *testing.T) {
	state := t.TempDir()
	first, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	// Idempotent, and still dead.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	var unheld *ControllerRoleUnheldError
	if err := first.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("a released lease authorized an operation")
	} else if !asUnheld(err, &unheld) {
		t.Fatalf("error = %v, want a role-unheld refusal", err)
	}

	second, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Release() })
	if second == first {
		t.Fatal("reacquisition revived the released lease")
	}
	if err := second.WithAuthority(func() error { return nil }); err != nil {
		t.Fatalf("the new lease did not authorize: %v", err)
	}
	// The old capability stays dead even though the role is held again.
	if err := first.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("an old lease authorized while a new one held the role")
	}
}

// A nil lease is not an absent check that quietly passes.
func TestNoLeaseAuthorizesNothing(t *testing.T) {
	var absent *ControllerRoleLease
	if err := absent.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("a nil lease authorized an operation")
	}
}

// shortStateDir is a state directory whose path fits inside the ~100-byte
// ceiling an operating system puts on a unix socket address. The standard
// per-test temp directory carries the test's name and blows through it.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func asUnheld(err error, target **ControllerRoleUnheldError) bool {
	unheld, ok := err.(*ControllerRoleUnheldError)
	if ok {
		*target = unheld
	}
	return ok
}

// SOCKET POSSESSION GRANTS NO ROLE AUTHORITY. The control endpoint used to BE
// the exclusion mechanism, so this is the regression that keeps it from
// quietly becoming one again.
func TestOwningTheControlSocketGrantsNoRoleAuthority(t *testing.T) {
	state := shortStateDir(t)
	holdRoleInAnotherProcess(t, state)

	// This process takes the socket - the thing that used to make a supervisor
	// exclusive - while another process holds the role.
	listener, err := ListenControl(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if _, err := os.Stat(listener.Path()); err != nil {
		t.Fatalf("the control endpoint is not present: %v", err)
	}

	// It cannot obtain the capability, so there is nothing it can act under.
	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("binding the control socket produced controller-role authority")
	}
}

// ROLE AUTHORITY SURVIVES A BROKEN OR ABSENT ENDPOINT. Communication
// availability is not ownership.
func TestRoleAuthoritySurvivesAnAbsentControlSocket(t *testing.T) {
	state := shortStateDir(t)
	lease, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	listener, err := ListenControl(state)
	if err != nil {
		t.Fatal(err)
	}
	socket := listener.Path()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socket)
	if _, err := os.Stat(socket); err == nil {
		t.Fatal("the socket was not removed, so the case is not the one being tested")
	}

	authorized := false
	if err := lease.WithAuthority(func() error { authorized = true; return nil }); err != nil {
		t.Fatalf("losing the endpoint removed controller-role authority: %v", err)
	}
	if !authorized {
		t.Fatal("the authority section did not run")
	}
}

// SAME GENERATION, WRONG PROCESS. Two processes of one generation return the
// same identity, so identity cannot separate them - only possession can. The
// holder here is this test binary re-executed, so its generation identity is
// identical to this process's by construction.
func TestSameGenerationWithoutTheLeaseHasNoAuthority(t *testing.T) {
	state := t.TempDir()
	holdRoleInAnotherProcess(t, state)

	declared := ControllerDeclaration{
		Kind: ControllerAdopted, Version: "main-same", SourceRevision: predecessorRevision, SourceTree: "tree-a",
	}
	here, err := CurrentControllerIdentity(declared)
	if err != nil {
		t.Fatal(err)
	}
	// The other process would measure the same executable and declare the same
	// build, so it proves the same generation. Identity is therefore useless
	// for telling them apart, which is the point.
	there, err := CurrentControllerIdentity(declared)
	if err != nil {
		t.Fatal(err)
	}
	if here.Build != there.Build {
		t.Fatal("the fixture does not model two processes of one generation")
	}

	// Proving the generation gains this process nothing: the capability is
	// held elsewhere and cannot be derived from identity.
	if _, err := AcquireControllerRole(state); err == nil {
		t.Fatal("a process of the right generation took a role another process holds")
	} else if !strings.Contains(err.Error(), "another live process") {
		t.Fatalf("error = %v, want one naming the live holder", err)
	}
	var absent *ControllerRoleLease
	if err := absent.WithAuthority(func() error { return nil }); err == nil {
		t.Fatal("proving the expected generation authorized an operation without the lease")
	}
}

// The lease answers ONE question. It says nothing about work admission or
// about which generation is durably active.
func TestLeaseIsNarrow(t *testing.T) {
	state := t.TempDir()
	lease, err := AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	gate := newWorkAdmissionGate(false)
	if gate.permitted() {
		t.Fatal("holding the role opened work admission")
	}
	if err := lease.WithAuthority(func() error { return nil }); err != nil {
		t.Fatalf("the role did not authorize while work admission was withheld: %v", err)
	}
	// And no activation exists anywhere on account of holding it.
	if _, err := os.Stat(filepath.Join(state, StableEntrypointName)); err == nil {
		t.Fatal("holding the role created a stable entrypoint")
	}
}
