package runtime

// The `serve` control endpoint is an OPERATOR-AUTHORITY BOUNDARY, not a
// convenience socket. Anything that can submit an accepted request to it can
// cause operator_trusted coding agents to execute under the operator's local
// account, against the operator's authenticated subscriptions, with the
// operator's repository credentials owning publication.
//
// So the floor is stated here and enforced before the listener exists:
//
//   - local machine only. There is no TCP listener in this file and no
//     configuration member anywhere that could introduce one. A network
//     control protocol is a separate design decision with its own authority
//     analysis; it is not something #63 grows into by accident.
//   - a Unix-domain socket inside the operator's own state directory, which is
//     already owner-only, with the socket itself owner-only as well.
//   - nothing authenticating is stored beside it. There is no bearer token, no
//     cookie and no shared secret in the runtime or configuration state,
//     because the ONLY credential here is filesystem access to an owner-only
//     path. That is deliberate: a token in a file is a token that can be read,
//     copied and leaked, and it would buy nothing that the directory
//     permission does not already provide.
//   - repository configuration cannot reach any of it. The endpoint path is
//     derived from the operator state directory, and repositoryScope has no
//     member that names a socket, a host, a port or a transport.
//
// Stale-socket recovery is deterministic rather than optimistic: a socket file
// left behind by a crash is only removed after a connection attempt PROVES
// nobody is listening. Unlinking first would let a second supervisor steal a
// live endpoint from the first.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ControlSocketName is the endpoint file inside the operator state directory.
const ControlSocketName = "serve.sock"

// maxControlSocketPath is the conservative cross-platform bound on a Unix
// socket address. macOS allows 104 bytes and Linux 108; 100 is under both and
// leaves room for the file name.
const maxControlSocketPath = 100

// ControlSocketPath is where a supervisor listens for one state directory. It
// is derived, never configured: an operator names their state directory, and
// the endpoint follows it, so there is no member a repository or a flag could
// use to move the boundary somewhere world-writable.
func ControlSocketPath(stateDir string) string {
	return filepath.Join(stateDir, ControlSocketName)
}

// ControlEndpointError is a typed refusal to open or trust the endpoint.
type ControlEndpointError struct {
	Path   string
	Detail string
}

func (e *ControlEndpointError) Error() string {
	return "control endpoint " + e.Path + ": " + e.Detail
}

// ErrSupervisorAlreadyRunning is the refusal when a live supervisor already
// owns the endpoint. It is deliberately distinct from a permission fault: the
// operator's action is to talk to the running supervisor, not to repair
// anything.
var ErrSupervisorAlreadyRunning = errors.New("a supervisor is already listening on this state directory's control endpoint")

// ControlRequest is one operator command. It is a small closed vocabulary
// rather than a general RPC surface: every verb here is an operator action that
// already exists as a command, and nothing accepts a path, a program, an
// environment or a credential.
type ControlRequest struct {
	Command string `json:"command"`
	// Repository is owner/name for a submission. It must already be a
	// repository this supervisor was constructed to govern.
	Repository string `json:"repository,omitempty"`
	Issue      int    `json:"issue,omitempty"`
	// Agent is the named execution agent, resolved against the operator's own
	// registry. A request naming an agent the operator did not configure is
	// refused; it cannot introduce one.
	Agent string `json:"agent,omitempty"`
	RunID string `json:"run_id,omitempty"`
	// NewGeneration asks for a fresh generation rather than continuing a live
	// one, exactly as the command-line flag does.
	NewGeneration bool `json:"new_generation,omitempty"`
	// Reason is the operator's stated cause for a cancellation.
	Reason string `json:"reason,omitempty"`
}

// Control commands. Each maps to one supervisor action.
const (
	ControlSubmit   = "submit"
	ControlStatus   = "status"
	ControlAgents   = "agents"
	ControlDrain    = "drain"
	ControlShutdown = "shutdown"
	ControlStop     = "stop"
	ControlStopAll  = "stop-all"
	ControlPing     = "ping"
)

// ControlResponse is the answer. Payload is the command's own JSON result.
type ControlResponse struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ControlListener owns the endpoint for one supervisor's lifetime.
type ControlListener struct {
	path     string
	listener net.Listener
	// closed makes Close idempotent WITHOUT clearing the listener. Clearing it
	// was a nil dereference waiting for a shutdown to land between two Accept
	// calls, which is every shutdown: Serve runs on its own goroutine for the
	// supervisor's whole life and Close is what ends it. The listener field is
	// therefore written once, at construction, and only ever read afterwards.
	closed sync.Once
}

// ListenControl opens the endpoint, refusing every unsafe state rather than
// repairing it. The order matters: the containing directory is checked BEFORE
// anything is created, so a supervisor never publishes a control path into a
// directory other users can reach.
func ListenControl(stateDir string) (*ControlListener, error) {
	if stateDir == "" {
		return nil, &ControlEndpointError{Detail: "an operator state directory is required"}
	}
	path := ControlSocketPath(stateDir)
	// A Unix socket address is a fixed-size field in the kernel, and every
	// platform's limit is around a hundred bytes. Exceeding it fails with
	// "invalid argument", which tells an operator nothing at all, so the bound
	// is stated here with the one thing that fixes it.
	if len(path) > maxControlSocketPath {
		return nil, &ControlEndpointError{Path: path, Detail: fmt.Sprintf(
			"is %d bytes long, above the %d-byte limit an operating system allows for a socket address; choose a shorter state_dir",
			len(path), maxControlSocketPath)}
	}
	if err := assertOwnerOnlyDir(stateDir); err != nil {
		return nil, &ControlEndpointError{Path: path, Detail: err.Error()}
	}
	if err := reclaimStaleControlSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, &ControlEndpointError{Path: path, Detail: err.Error()}
	}
	// Owner-only on the socket itself, in addition to the owner-only directory.
	// Two independent statements of the same boundary: a directory mode that
	// was widened later must not silently widen the endpoint too.
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, &ControlEndpointError{Path: path, Detail: "the control socket could not be made owner-only: " + err.Error()}
	}
	if err := AssertControlEndpointSecure(path); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &ControlListener{path: path, listener: listener}, nil
}

// reclaimStaleControlSocket removes a socket file only after PROVING nobody is
// listening on it. A supervisor that unlinked first would be able to steal a
// live endpoint from another supervisor mid-flight.
func reclaimStaleControlSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return &ControlEndpointError{Path: path, Detail: err.Error()}
	}
	if info.Mode()&os.ModeSocket == 0 {
		// Something that is not a socket is at the endpoint path. Removing it
		// would be this process deleting an operator's file on a guess.
		return &ControlEndpointError{Path: path, Detail: "exists and is not a socket; refusing to remove it"}
	}
	connection, err := net.DialTimeout("unix", path, 2*time.Second)
	if err == nil {
		_ = connection.Close()
		return ErrSupervisorAlreadyRunning
	}
	if err := os.Remove(path); err != nil {
		return &ControlEndpointError{Path: path, Detail: "a stale socket could not be removed: " + err.Error()}
	}
	return nil
}

// Path is the endpoint file, for diagnostics.
func (l *ControlListener) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close stops listening and removes the endpoint file. It is idempotent and
// safe to call while Serve is running: closing the listener is what makes
// Accept return, which is how Serve learns to stop.
func (l *ControlListener) Close() error {
	if l == nil || l.listener == nil {
		return nil
	}
	var err error
	l.closed.Do(func() {
		err = l.listener.Close()
		_ = os.Remove(l.path)
	})
	return err
}

// maxControlRequestBytes bounds one request line. The vocabulary above
// canonicalizes to a few hundred bytes; the ceiling exists so a local process
// cannot make the supervisor allocate without bound.
const maxControlRequestBytes = 8 << 10

// Serve answers requests until the listener is closed. Each connection carries
// exactly one request and one response, which keeps the protocol trivially
// stateless: there is no session to hijack and no ordering to reason about.
func (l *ControlListener) Serve(handle func(ControlRequest) ControlResponse) error {
	for {
		connection, err := l.listener.Accept()
		if err != nil {
			// A closed listener is a shutdown, not a fault.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go answerControlConnection(connection, handle)
	}
}

func answerControlConnection(connection net.Conn, handle func(ControlRequest) ControlResponse) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	// The ceiling is enforced by the READER, not checked afterwards.
	// bufio.ReadBytes grows its own buffer past the size hint, so a local
	// process sending a line with no newline could make the supervisor allocate
	// without bound and be refused only once it already had. One byte of
	// headroom distinguishes "exactly at the bound" from "over it".
	bounded := io.LimitReader(connection, maxControlRequestBytes+1)
	reader := bufio.NewReaderSize(bounded, maxControlRequestBytes)
	line, err := reader.ReadBytes('\n')
	if len(line) > maxControlRequestBytes {
		writeControlResponse(connection, ControlResponse{Error: "control request exceeds the size bound"})
		return
	}
	if err != nil && len(line) == 0 {
		return
	}
	var request ControlRequest
	if err := json.Unmarshal(line, &request); err != nil {
		writeControlResponse(connection, ControlResponse{Error: "control request is not a JSON object"})
		return
	}
	writeControlResponse(connection, handle(request))
}

func writeControlResponse(connection net.Conn, response ControlResponse) {
	encoded, err := json.Marshal(response)
	if err != nil {
		encoded = []byte(`{"ok":false,"error":"the response could not be encoded"}`)
	}
	_, _ = connection.Write(append(encoded, '\n'))
}

// SendControl performs one request against a supervisor's endpoint. It is the
// client half, used by ordinary operator commands so they can delegate to a
// running supervisor instead of driving the work in their own terminal.
func SendControl(stateDir string, request ControlRequest) (ControlResponse, error) {
	path := ControlSocketPath(stateDir)
	if err := AssertControlEndpointSecure(path); err != nil {
		return ControlResponse{}, err
	}
	connection, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return ControlResponse{}, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(60 * time.Second))
	encoded, err := json.Marshal(request)
	if err != nil {
		return ControlResponse{}, err
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return ControlResponse{}, err
	}
	reader := bufio.NewReaderSize(connection, 1<<20)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ControlResponse{}, err
	}
	var response ControlResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return ControlResponse{}, fmt.Errorf("control response is not a JSON object")
	}
	return response, nil
}

// SupervisorRunning reports whether a supervisor currently owns the endpoint.
// It is used to decide whether an operator command should delegate, so a
// missing or stale endpoint simply means "drive it here" rather than an error.
func SupervisorRunning(stateDir string) bool {
	connection, err := net.DialTimeout("unix", ControlSocketPath(stateDir), 2*time.Second)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
