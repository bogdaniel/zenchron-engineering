package runtime

// THE CHILD HALF of the two-process crash suite.
//
// This test binary re-executes itself as a controller. The parent drives it one
// step at a time over a pipe and decides, at every step boundary, whether to let
// it continue or to kill it. That is the only way to place a real process at an
// exact protocol boundary: timing cannot be trusted to put it there, and a
// process that decides its own lifetime will eventually decide it at the wrong
// moment and look like a protocol failure.
//
// THE PARENT OWNS THE LIFETIME. The child never exits on its own initiative, has
// no retries, no respawn, and no opportunistic repair. It performs exactly the
// step it was told to perform and waits.
//
// WHAT IS SYNTHETIC HERE, stated plainly: the child's generation identity is
// SUPPLIED rather than measured. Two genuinely different adopted builds cannot
// be produced inside a unit test, so the fixture hands each child the identity
// it claims. The property that identity cannot be forged into authority is
// proven elsewhere, against a real measured binary, in
// TestSameGenerationWithoutTheLeaseHasNoAuthority. What this suite proves is
// what the protocol does with those identities once several processes hold them.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// controllerChildEnv turns this binary into a driven controller.
	controllerChildEnv = "ZENCHRON_TEST_CONTROLLER_CHILD"
	// childGenerationEnv is the generation the child claims to be.
	childGenerationEnv = "ZENCHRON_TEST_CHILD_GENERATION"
	childStateEnv      = "ZENCHRON_TEST_CHILD_STATE"
	childRootEnv       = "ZENCHRON_TEST_CHILD_ROOT"
	childArtifactEnv   = "ZENCHRON_TEST_CHILD_ARTIFACT"
)

// Child steps. Each is one protocol action and one acknowledgement, so a cut
// point is "after this ack, before the next command".
const (
	stepStart    = "start"    // take the controller role
	stepActivate = "activate" // revalidate, admit successions, activate
	stepRecover  = "recover"  // resume an activation already on disk
	stepEnable   = "enable"   // open work admission
	stepDrain    = "drain"    // close admission and release the role
	stepSnapshot = "snapshot" // report one coherent self-observation
	stepExit     = "exit"     // leave deliberately
)

// runControllerChild is the child's whole life. Every reply is one line so the
// parent can read it without guessing framing.
func runControllerChild() {
	state, root := os.Getenv(childStateEnv), os.Getenv(childRootEnv)
	self := childIdentity()
	admission := newFakeAdmission()
	var service *ControllerService

	reader := bufio.NewReader(os.Stdin)
	reply := func(format string, args ...any) {
		os.Stdout.WriteString(fmt.Sprintf(format, args...) + "\n")
		_ = os.Stdout.Sync()
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// The parent closed the pipe without saying exit. Leaving quietly
			// is right: the parent owns this process's lifetime, and a child
			// that outlived its driver would be a stray controller.
			os.Exit(0)
		}
		command := strings.TrimSpace(line)
		switch {
		case command == stepStart:
			service, err = StartControllerService(state, root, childStore(state), self, admission)
			if err != nil {
				reply("error %s", err)
				continue
			}
			service.now = func() time.Time { return time.Now().UTC() }
			reply("ok")
		case command == stepExit:
			reply("ok")
			os.Exit(0)
		case service == nil:
			reply("error the controller service has not started")
		case strings.HasPrefix(command, stepActivate):
			reply("%s", childActivate(service, strings.Fields(command)))
		case strings.HasPrefix(command, stepRecover):
			reply("%s", childRecover(service, strings.Fields(command)))
		case strings.HasPrefix(command, stepEnable):
			reply("%s", childEnable(service, strings.Fields(command)))
		case command == stepDrain:
			if err := service.DrainAndReleaseRole(); err != nil {
				reply("error %s", err)
				continue
			}
			reply("ok")
		case strings.HasPrefix(command, stepSnapshot):
			fields := strings.Fields(command)
			handoff := ""
			if len(fields) > 1 {
				handoff = fields[1]
			}
			encoded, err := json.Marshal(service.DescribeLiveController(handoff, time.Now().UTC()))
			if err != nil {
				reply("error %s", err)
				continue
			}
			reply("snapshot %s", encoded)
		default:
			reply("error unknown step %q", command)
		}
	}
}

func childActivate(service *ControllerService, fields []string) string {
	if len(fields) < 2 {
		return "error activate requires a transition id"
	}
	record, found, err := service.store.ControllerHandoff(fields[1])
	if err != nil || !found {
		return fmt.Sprintf("error the transition could not be read: %v found=%v", err, found)
	}
	expect, err := Expect(record)
	if err != nil {
		return fmt.Sprintf("error %s", err)
	}
	activated, err := service.ActivateSuccessor(expect, nil)
	switch {
	case err == nil:
		return "ok " + string(activated.Phase)
	case isProjectionRepairFailure(err):
		// The distinction the protocol makes, carried over the wire: the
		// successor IS active and the pointer is stale.
		return "activated-with-drift " + string(activated.Phase)
	default:
		return fmt.Sprintf("error %s", err)
	}
}

func childRecover(service *ControllerService, fields []string) string {
	if len(fields) < 2 {
		return "error recover requires a transition id"
	}
	recovered, err := service.RecoverActivated(fields[1])
	switch {
	case err == nil:
		return "ok " + string(recovered.Phase)
	case isProjectionRepairFailure(err):
		return "recovered-with-drift " + string(recovered.Phase)
	default:
		return fmt.Sprintf("error %s", err)
	}
}

func childEnable(service *ControllerService, fields []string) string {
	if len(fields) < 2 {
		return "error enable requires a transition id"
	}
	if err := service.EnableWorkAdmission(fields[1]); err != nil {
		return fmt.Sprintf("error %s", err)
	}
	return "ok"
}

func isProjectionRepairFailure(err error) bool {
	_, ok := err.(*ProjectionRepairFailedError)
	return ok
}

// childIdentity is the generation this child claims. It is supplied rather than
// measured; see the note at the top of this file.
func childIdentity() ControllerSelfRecord {
	var build ControllerBuild
	if err := json.Unmarshal([]byte(os.Getenv(childGenerationEnv)), &build); err != nil {
		os.Stderr.WriteString("child generation is unreadable: " + err.Error())
		os.Exit(2)
	}
	return ControllerSelfRecord{
		Build: build, ExecutablePath: os.Getenv(childArtifactEnv), Measured: build.BinarySHA256,
	}
}

func childStore(stateDir string) *SQLiteOperationStore {
	store, err := OpenSQLiteOperationStore(stateDir)
	if err != nil {
		os.Stderr.WriteString("child store: " + err.Error())
		os.Exit(2)
	}
	return store
}
