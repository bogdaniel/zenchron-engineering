package runtime

// The control socket's per-command request-size enforcement: planning-
// artifact-propose carries more than every other command, and this proves
// that allowance is scoped to exactly that command, not the connection.

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/planning"
)

// TestOrdinaryCommandsStayBoundToTheirOwnCeilingNotThePlanningArtifactOne
// proves the per-command check added for planning-artifact-propose
// (answerControlConnection) did not just move the connection-wide ceiling: a
// COMPLETE, well-formed, newline-terminated "ping" between the ordinary
// 8 KiB bound and the much larger planning-artifact one must still be
// refused, read in full first (never silently truncated), and never
// dispatched to the handler.
func TestOrdinaryCommandsStayBoundToTheirOwnCeilingNotThePlanningArtifactOne(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var dispatched atomic.Bool
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse {
			dispatched.Store(true)
			return ControlResponse{OK: true}
		})
	}()

	connection, err := net.DialTimeout("unix", listener.Path(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	// Comfortably over maxControlRequestBytes, comfortably under
	// maxControlConnectionBytes: the reader must accept the whole line (it
	// has to, for a genuine planning-artifact-propose this size to work at
	// all), and only the per-command check after parsing may refuse it.
	padding := maxControlRequestBytes + 1024
	oversized, err := json.Marshal(ControlRequest{Command: "ping", Reason: strings.Repeat("A", padding)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(append(oversized, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("the endpoint did not answer: %v", err)
	}
	var response ControlResponse
	if err := json.Unmarshal(reply, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || !strings.Contains(response.Error, "size bound") {
		t.Fatalf("an ordinary command over its own ceiling was not refused: %#v", response)
	}
	if dispatched.Load() {
		t.Fatal("an oversized ordinary command reached the handler before being refused")
	}
}

// TestPlanningArtifactProposeReachesTheHandlerAboveTheOrdinaryCeiling is the
// positive half of the two tests above: a planning-artifact-propose request
// bigger than the ordinary 8 KiB bound, and comfortably within
// planning.MaxPlanningArtifactRequestBytes, must actually be read whole and
// dispatched - the per-command allowance is not merely theoretical.
func TestPlanningArtifactProposeReachesTheHandlerAboveTheOrdinaryCeiling(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var dispatched atomic.Bool
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse {
			dispatched.Store(true)
			return ControlResponse{OK: true}
		})
	}()

	connection, err := net.DialTimeout("unix", listener.Path(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	document, err := json.Marshal(map[string]string{"instructions": strings.Repeat("A", maxControlRequestBytes*4)})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(ControlRequest{
		Command:          ControlPlanningArtifactPropose,
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindInstructionPack, ID: "big", Document: document},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) <= maxControlRequestBytes {
		t.Fatalf("this request is %d bytes, not above the ordinary %d byte bound it is meant to exceed", len(line), maxControlRequestBytes)
	}
	if len(line) >= maxControlConnectionBytes {
		t.Fatalf("this request is %d bytes, not comfortably under the %d byte connection ceiling it is meant to fit", len(line), maxControlConnectionBytes)
	}
	if _, err := connection.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("the endpoint did not answer: %v", err)
	}
	var response ControlResponse
	if err := json.Unmarshal(reply, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("a planning-artifact-propose request within its own ceiling was refused: %#v", response)
	}
	if !dispatched.Load() {
		t.Fatal("the request was never dispatched to the handler")
	}
}

// TestPlanningArtifactProposeReachesTheHandlerNearTheArtifactBound is the
// transport-layer half of the coverage gap an independent review found: the
// test above exercises ~32 KiB, comfortably inside the ceiling but nowhere
// near planning.MaxPlanningArtifactBytes itself. This sends a request within
// a few hundred bytes of the 1 MiB bound - the size the contract exists to
// carry - and confirms the control socket actually reads and dispatches it.
func TestPlanningArtifactProposeReachesTheHandlerNearTheArtifactBound(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var dispatched atomic.Bool
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse {
			dispatched.Store(true)
			return ControlResponse{OK: true}
		})
	}()

	connection, err := net.DialTimeout("unix", listener.Path(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	const sealingAndWireOverheadMargin = 1 << 10
	padding := int(planning.MaxPlanningArtifactBytes) - sealingAndWireOverheadMargin
	document, err := json.Marshal(map[string]string{"instructions": strings.Repeat("A", padding)})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(ControlRequest{
		Command:          ControlPlanningArtifactPropose,
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindInstructionPack, ID: "near-max", Document: document},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) >= maxControlConnectionBytes {
		t.Fatalf("this request is %d bytes, not comfortably under the %d byte connection ceiling it is meant to fit", len(line), maxControlConnectionBytes)
	}
	if _, err := connection.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("the endpoint did not answer: %v", err)
	}
	var response ControlResponse
	if err := json.Unmarshal(reply, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("a near-maximal planning-artifact-propose request was refused: %#v", response)
	}
	if !dispatched.Load() {
		t.Fatal("the request was never dispatched to the handler")
	}
}
