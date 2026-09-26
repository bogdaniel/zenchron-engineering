package runtime

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sessionFixture is a serving controller: its own identity, its own lease, and
// an endpoint. Everything a caller can reach goes through the real socket.
type sessionFixture struct {
	state    string
	identity ControllerSelfRecord
	binding  ControllerBinding
	lease    *ControllerRoleLease
	listener *ControlListener
	executed int64
	observed int64
	mu       sync.Mutex
	hold     chan struct{}
}

func newSessionFixture(t *testing.T, withLease bool) *sessionFixture {
	t.Helper()
	state := shortStateDir(t)
	build := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	fixture := &sessionFixture{
		state:    state,
		identity: ControllerSelfRecord{Build: build, ExecutablePath: "/controller/zenchron-engineering", Measured: build.BinarySHA256},
		binding:  ControllerBinding{Controller: "zenchron-engineering", Build: &build, Config: ConfigDigest{Global: "config-a"}},
	}
	if withLease {
		lease, err := AcquireControllerRole(state)
		if err != nil {
			t.Fatal(err)
		}
		fixture.lease = lease
		t.Cleanup(func() { _ = lease.Release() })
	}
	listener, err := ListenControl(state)
	if err != nil {
		t.Fatal(err)
	}
	fixture.listener = listener
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		_ = listener.ServeSessions(PrivilegedControl{
			Identity: fixture.identity,
			Lease:    fixture.lease,
			Execute: func(request ControlRequest) ControlResponse {
				if hold := fixture.holdChannel(); hold != nil {
					<-hold
				}
				atomic.AddInt64(&fixture.executed, 1)
				return ControlResponse{OK: true}
			},
			Observe: func(request ControlRequest) ControlResponse {
				atomic.AddInt64(&fixture.observed, 1)
				return ControlResponse{OK: true}
			},
		})
	}()
	return fixture
}

// holdChannel parks a privileged operation mid-commit, under a mutex because
// the test goroutine clears it while a handler goroutine reads it.
func (f *sessionFixture) holdChannel() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hold
}

func (f *sessionFixture) setHold(hold chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = hold
}

func (f *sessionFixture) dial(t *testing.T) *ControlSession {
	t.Helper()
	session, err := DialControlSession(f.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// THE WHOLE BOUNDARY, in the order the protocol requires it.
func TestPrivilegedControlRequiresProofIdentityAndLease(t *testing.T) {
	fixture := newSessionFixture(t, true)
	session := fixture.dial(t)

	// A privileged operation before any proof is refused by the SERVER, not
	// merely declined by the client.
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a privileged operation ran before the peer was proven")
	}
	if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err != nil {
		t.Fatalf("a proven peer holding the role refused a privileged operation: %v", err)
	}
	if atomic.LoadInt64(&fixture.executed) != 1 {
		t.Fatalf("the operation ran %d time(s), want 1", fixture.executed)
	}
}

// The proof answers THIS exchange. A reply carrying somebody else's challenge
// is a document about another moment, which is what a replay looks like.
func TestIdentityProofIsBoundToItsExchange(t *testing.T) {
	state := shortStateDir(t)
	build := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	binding := ControllerBinding{Controller: "zenchron-engineering", Build: &build, Config: ConfigDigest{Global: "config-a"}}
	// A HOSTILE SERVER, spoken directly rather than through ServeControlSession:
	// it echoes a STALE challenge, which is the shape of a captured proof being
	// replayed at a later exchange.
	socket := ControlSocketPath(state)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				// READ THE REQUEST FIRST. Replying and closing immediately
				// races the client's write and surfaces as a broken pipe,
				// which would fail this test for a reason that has nothing to
				// do with what it is testing.
				reader := bufio.NewReader(connection)
				if _, err := reader.ReadBytes('\n'); err != nil {
					return
				}
				_, _ = connection.Write([]byte(`{"proof":{"challenge":"an-older-exchange","self":{"build":{"kind":"adopted"}}}}` + "\n"))
				// Stay open until the client hangs up, so the close never
				// overtakes the reply either.
				_, _ = reader.ReadBytes('\n')
			}(connection)
		}
	}()
	session, err := DialControlSession(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.ProveControllerIdentity(binding); err == nil {
		t.Fatal("a proof from another exchange was accepted")
	} else if !strings.Contains(err.Error(), "this exchange") {
		t.Fatalf("error = %v, want one naming the exchange binding", err)
	}
}

// A proof on one connection cannot authorize an action on another.
func TestProofDoesNotCarryToAnotherConnection(t *testing.T) {
	fixture := newSessionFixture(t, true)
	proving := fixture.dial(t)
	if _, err := proving.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatal(err)
	}
	acting := fixture.dial(t)
	if _, err := acting.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a proof from another connection authorized an operation")
	}
	if executed := atomic.LoadInt64(&fixture.executed); executed != 0 {
		t.Fatalf("the operation ran %d time(s) on an unproven connection", executed)
	}
}

// THE CRITICAL CASE, through the real protocol: the peer is the right
// generation and proves it honestly, and it does not hold the role. Identity
// cannot separate two processes of one generation; possession can.
func TestRightGenerationWithoutTheLeaseCannotMutate(t *testing.T) {
	fixture := newSessionFixture(t, false) // serves the socket, holds no lease
	// Another process really does hold the role, so this is not a vacuous case.
	holdRoleInAnotherProcess(t, fixture.state)

	session := fixture.dial(t)
	if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatalf("the peer could not prove the generation it really is: %v", err)
	}
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a proven peer without the role mutated state")
	} else if !strings.Contains(err.Error(), "controller role") {
		t.Fatalf("error = %v, want one naming the missing role", err)
	}
	if executed := atomic.LoadInt64(&fixture.executed); executed != 0 {
		t.Fatalf("the privileged operation ran %d time(s) without the role", executed)
	}
}

// A peer of the WRONG generation is refused by the caller before it asks for
// anything privileged.
func TestWrongGenerationIsRefusedBeforeActing(t *testing.T) {
	fixture := newSessionFixture(t, true)
	session := fixture.dial(t)
	other := attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	expected := ControllerBinding{Controller: "zenchron-engineering", Build: &other, Config: ConfigDigest{Global: "config-a"}}
	if _, err := session.ProveControllerIdentity(expected); err == nil {
		t.Fatal("a peer of the wrong generation was accepted")
	}
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a privileged operation followed a failed proof")
	}
	if executed := atomic.LoadInt64(&fixture.executed); executed != 0 {
		t.Fatal("the operation ran against an unexpected generation")
	}
}

// READ-ONLY SURVIVES THE ROLE. Losing ownership must not cost observability.
func TestObservationNeedsNoRole(t *testing.T) {
	fixture := newSessionFixture(t, false)
	session := fixture.dial(t)
	if _, err := session.ObserveControl(ControlRequest{Command: "status"}); err != nil {
		t.Fatalf("a read-only command required the role: %v", err)
	}
	// And identity is answerable without it too, which is what an operator
	// diagnosing a stuck handoff needs.
	if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatalf("identity required the role: %v", err)
	}
	if observed := atomic.LoadInt64(&fixture.observed); observed != 1 {
		t.Fatalf("the read-only command ran %d time(s), want 1", observed)
	}
}

// THE CONNECTION IS NOT A CREDENTIAL. A release lands mid-operation: it waits
// for the commit, and the NEXT operation on that same still-open connection is
// refused.
func TestReleaseEndsAuthorityOnAStillOpenConnection(t *testing.T) {
	fixture := newSessionFixture(t, true)
	fixture.setHold(make(chan struct{}))
	session := fixture.dial(t)
	if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() {
		_, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"})
		first <- err
	}()
	// Wait until the operation is really inside the authority section.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&fixture.executed) == 0 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	released := make(chan struct{})
	go func() { _ = fixture.lease.Release(); close(released) }()
	select {
	case <-released:
		t.Fatal("the release returned while a privileged operation was mid-commit")
	case <-time.After(50 * time.Millisecond):
	}
	hold := fixture.holdChannel()
	fixture.setHold(nil)
	close(hold)
	if err := <-first; err != nil {
		t.Fatalf("the in-flight operation failed: %v", err)
	}
	<-released

	// The connection is still open, still proven, and no longer able to change
	// anything: connection lifetime is not authority lifetime.
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a still-open proven connection mutated state after the role was released")
	}
	if executed := atomic.LoadInt64(&fixture.executed); executed != 1 {
		t.Fatalf("the operation ran %d time(s), want exactly the one that was in flight", executed)
	}
	// Observation still works on the same connection.
	if _, err := session.ObserveControl(ControlRequest{Command: "status"}); err != nil {
		t.Fatalf("losing the role also cost observability: %v", err)
	}
}

// Concurrent privileged operations are permitted while the role is held, and
// all of them are refused once it is not.
func TestAuthorityIsRecheckedForEveryOperation(t *testing.T) {
	fixture := newSessionFixture(t, true)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			session, err := DialControlSession(fixture.state)
			if err != nil {
				return
			}
			defer session.Close()
			if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
				return
			}
			_, _ = session.ExecutePrivilegedControl(ControlRequest{Command: "stop"})
		}()
	}
	group.Wait()
	before := atomic.LoadInt64(&fixture.executed)
	if before == 0 {
		t.Fatal("no privileged operation ran while the role was held")
	}
	if err := fixture.lease.Release(); err != nil {
		t.Fatal(err)
	}
	session := fixture.dial(t)
	if _, err := session.ProveControllerIdentity(fixture.binding); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ExecutePrivilegedControl(ControlRequest{Command: "stop"}); err == nil {
		t.Fatal("a privileged operation ran after the role was released")
	}
	if after := atomic.LoadInt64(&fixture.executed); after != before {
		t.Fatalf("%d operation(s) ran after the release", after-before)
	}
}
