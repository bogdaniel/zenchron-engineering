package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
)

// runningControlPlane is a started instance: the address it printed, how to stop
// it and wait for it, and the result it will return.
type runningControlPlane struct {
	address string
	stop    func()
}

// startControlPlane starts controlPlaneUntil on an ephemeral address and waits
// for its startup line. It returns the error instead when the start is refused.
func startControlPlane(t *testing.T, state string, extra ...string) (*runningControlPlane, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := firstLineWriter{lines: make(chan string, 1)}
	args := append([]string{"--state-dir", state}, extra...)
	if !contains(extra, "--listen") && !contains(extra, "--auxiliary") {
		args = append(args, "--listen", "127.0.0.1:0")
	}
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := controlPlaneUntil(ctx, args, out)
		result <- err
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("control plane did not stop")
		}
	}
	select {
	case line := <-out.lines:
		m := regexp.MustCompile(`http://([0-9.]+:\d+)/app`).FindStringSubmatch(line)
		if m == nil {
			stop()
			t.Fatalf("no address in startup line %q", line)
		}
		return &runningControlPlane{address: m[1], stop: stop}, nil
	case err := <-result:
		cancel()
		return nil, err
	case <-time.After(10 * time.Second):
		stop()
		t.Fatal("no startup line")
		return nil, nil
	}
}

// primaryRecord waits for the primary to have written its record, which it does
// just after printing its startup line.
func primaryRecord(t *testing.T, state string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err := os.ReadFile(processRecordPath(state, serviceControlPlane))
		if err == nil && len(record) > 0 {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("the primary never recorded itself: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func seededState(t *testing.T) string {
	t.Helper()
	dir, config, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	state := built.config.StateDir
	built.release()
	return state
}

// #667: a second primary is refused, names the live one, and leaves its record
// alone.
func TestASecondPrimaryControlPlaneIsRefusedAndNamesTheFirst(t *testing.T) {
	state := seededState(t)
	first, err := startControlPlane(t, state)
	if err != nil {
		t.Fatal(err)
	}
	defer first.stop()
	before := primaryRecord(t, state)

	second, err := startControlPlane(t, state)
	if err == nil {
		second.stop()
		t.Fatal("a second primary control plane started on the same state directory")
	}
	if !strings.Contains(err.Error(), "already runs") || !strings.Contains(err.Error(), first.address) || !strings.Contains(err.Error(), "--auxiliary") {
		t.Fatalf("refusal does not name the live primary and the way out: %v", err)
	}
	after, err := os.ReadFile(processRecordPath(state, serviceControlPlane))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the refused start disturbed the primary's record: %v\n%s\n%s", err, before, after)
	}
}

// Starting together yields exactly one primary: every other start is refused
// before it binds or records anything.
func TestSimultaneousControlPlaneStartsYieldExactlyOnePrimary(t *testing.T) {
	state := seededState(t)
	const starters = 6
	var mu sync.Mutex
	var running []*runningControlPlane
	refused := 0
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			cp, err := startControlPlane(t, state)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			running = append(running, cp)
		}()
	}
	close(gate)
	wg.Wait()
	for _, cp := range running {
		cp.stop()
	}
	if len(running) != 1 || refused != starters-1 {
		t.Fatalf("%d primaries started and %d were refused, want 1 and %d", len(running), refused, starters-1)
	}
}

// An auxiliary instance needs its own address, never touches the primary's
// record, and refuses every write on the SERVER: a client sending a governed
// request gets 403 whatever it is.
func TestAnAuxiliaryControlPlaneIsReadOnlyOnTheServer(t *testing.T) {
	state := seededState(t)
	primary, err := startControlPlane(t, state)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.stop()
	record := primaryRecord(t, state)

	if cp, err := startControlPlane(t, state, "--auxiliary"); err == nil {
		cp.stop()
		t.Fatal("an auxiliary instance started without an explicit --listen")
	}
	if cp, err := startControlPlane(t, state, "--auxiliary", "--listen", primary.address); err == nil {
		cp.stop()
		t.Fatal("an auxiliary instance was allowed on the primary's own address")
	}

	aux, err := startControlPlane(t, state, "--auxiliary", "--listen", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(state, controlplane.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string) int {
		request, err := http.NewRequest(method, "http://"+aux.address+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := call(http.MethodGet, "/v1/runs?limit=1"); got != http.StatusOK {
		t.Fatalf("an auxiliary instance did not serve a read: %d", got)
	}
	for _, write := range []struct{ method, path string }{
		{http.MethodPost, "/v1/runs/run-x/resume"},
		{http.MethodPost, "/v1/objectives"},
		{http.MethodPost, "/v1/issues"},
		{http.MethodPut, "/v1/products"},
		{http.MethodDelete, "/v1/runs/run-x"},
	} {
		if got := call(write.method, write.path); got != http.StatusForbidden {
			t.Errorf("%s %s on an auxiliary instance = %d, want 403", write.method, write.path, got)
		}
	}
	aux.stop()

	after, err := os.ReadFile(processRecordPath(state, serviceControlPlane))
	if err != nil || string(after) != string(record) {
		t.Fatalf("the auxiliary instance changed or removed the primary's record: %v", err)
	}
}

// The auxiliary admission rules, decided without binding any port: an explicit
// address, not the primary's own, and the token the primary created.
func TestAuxiliaryNeedsItsOwnAddressAndThePrimarysToken(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, controlplane.TokenFile), []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessRecord(state, serviceControlPlane, processRecord{PID: 4242, Listen: "127.0.0.1:8789"}); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		address string
		given   bool
		want    string
	}{
		"no explicit address":   {"127.0.0.1:8787", false, "explicit --listen"},
		"the primary's address": {"127.0.0.1:8789", true, "primary control plane's own address"},
		"its own address":       {"127.0.0.1:8790", true, ""},
	} {
		err := checkAuxiliary(state, test.address, test.given)
		if (test.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, test.want)
		}
	}
	if err := checkAuxiliary(t.TempDir(), "127.0.0.1:8790", true); err == nil || !strings.Contains(err.Error(), "start the primary first") {
		t.Errorf("a state directory with no token was accepted: %v", err)
	}
}
