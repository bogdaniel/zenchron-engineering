package runtime

import (
	"context"
	"strings"
	"testing"
)

// Two controllers have separate state directories and one Docker daemon. The
// per-revision claim lives in the state directory, so it cannot make them wait
// for each other; the container name must therefore be theirs alone. Before it
// carried the state directory, main and armA derived the same name for the same
// revision, the slower one met the daemon's name conflict and its updater was
// refused (#708), and a retry that "released" a created-but-not-started
// container of that name would have removed the other controller's container.
func TestTwoControllersBuildingOneRevisionOnOneDaemonNeverShareAContainer(t *testing.T) {
	noAdoptedBuildBackoff(t)
	// What one controller's whole build creates, measured on its own daemon.
	solo := newLockstepDocker()
	close(solo.gate)
	if _, err := runAdoptedBuild(context.Background(), lockstepSpec(t, solo, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	perBuild := solo.creates

	docker := newLockstepDocker()
	first, second := t.TempDir(), t.TempDir()
	firstDone := make(chan error, 1)
	go func() {
		_, err := runAdoptedBuild(context.Background(), lockstepSpec(t, docker, first))
		firstDone <- err
	}()
	<-docker.started // the first controller is held between create and start

	if _, err := runAdoptedBuild(context.Background(), lockstepSpec(t, docker, second)); err != nil {
		t.Fatalf("the second controller's build of the same revision was refused or failed: %v", err)
	}
	docker.mu.Lock()
	if docker.creates < perBuild+1 {
		t.Fatalf("the second controller created %d containers beside the first's %d", docker.creates-1, 1)
	}
	for _, name := range docker.removed {
		if name == docker.firstCreated {
			t.Fatalf("the second controller removed the first controller's container %q", name)
		}
	}
	docker.mu.Unlock()

	close(docker.gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("the first controller's build failed after the second finished: %v", err)
	}
	docker.mu.Lock()
	defer docker.mu.Unlock()
	if docker.creates != 2*perBuild || len(docker.removed) != 2*perBuild {
		t.Fatalf("two controllers made %d containers and removed %d, want %d of each (%d per build)", docker.creates, len(docker.removed), 2*perBuild, perBuild)
	}
	seen := map[string]bool{}
	for _, name := range docker.removed {
		if seen[name] {
			t.Fatalf("container %q was removed twice: the controllers shared it", name)
		}
		seen[name] = true
	}
}

// The same controller, restarted, must find the container name it derived
// before: that is how it reconciles what a crash left behind. Controllers that
// differ only by state directory must not.
func TestAContainerNameIsStableForOneStateDirectoryAndDistinctAcrossThem(t *testing.T) {
	docker := newLockstepDocker()
	name := func(state string) string {
		sandbox := DockerSandbox{Image: "sha256:pinned", Executor: docker, OperationID: "adopted-build-" + strings.Repeat("a", 40), StateDir: state}
		record, _, err := sandbox.operationRecord(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return record.ContainerName
	}
	one, two := t.TempDir(), t.TempDir()
	if name(one) != name(one) || name(two) != name(two) {
		t.Fatal("a controller derives a different container name on each call, so it could never reconcile its own leftovers")
	}
	if name(one) == name(two) {
		t.Fatal("controllers with different state directories derive the same container name")
	}
	if name(one) != name(one+"/") || name(one) != name(one+"/../"+lastElement(one)) {
		t.Fatal("equivalent spellings of one state directory derive different container names")
	}
}

func lastElement(path string) string { return path[strings.LastIndex(path, "/")+1:] }
