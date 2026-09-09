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

// BoundedNote is one operator annotation, truncated to the bound a durable
// payload field holds. It is the same bound the local path applies, so a note
// reaches the journal identically whichever process records it.
func BoundedNote(note string) string { return boundedField(note) }

// ControlDeadline is how long one control command may take, on BOTH sides of
// the socket. It is per-command because the commands are not alike: a status
// read answers immediately, while a plan revision clones a repository and
// invokes a planner in its own non-mutating mode, which is bounded in minutes.
//
// A deadline shorter than the work does not stop the work - the supervisor
// finishes and persists the revision either way - it only stops the operator
// from being told. That is the worst of both: the effect without the answer.
func ControlDeadline(command string) time.Duration {
	if command == ControlPlanRevise {
		return 20 * time.Minute
	}
	return 30 * time.Second
}

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
	// DefaultBranch is the base branch the REQUESTER resolved for that
	// repository, the same way the local path resolves it from origin/HEAD. It
	// selects a branch within a repository the supervisor already governs; it
	// is not a permission, and a request that carries none falls back to what
	// durable state or the enrolment assumption says.
	DefaultBranch string `json:"default_branch,omitempty"`
	Issue         int    `json:"issue,omitempty"`
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
	// PlanID, Revision, Digest and Note are one plan decision. They exist here
	// because a plan decision is an OPERATOR act against work a supervisor is
	// executing, and the supervisor is the process that owns that work: routing
	// the decision to it means one writer applies it against the state the
	// supervisor is reconciling, under the same lock that reconciler holds.
	//
	// The digest is not optional in the service that receives this, so a
	// request naming a revision without its content decides nothing.
	PlanID   string `json:"plan_id,omitempty"`
	Revision int    `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Note     string `json:"note,omitempty"`
	// AssignmentsDigest is the assignment set the requester read, where they
	// named one. The revision digest binds the plan DOCUMENT and does not move
	// when a profile, an instruction pack or the workforce is edited, so this
	// is what binds who would perform the work. It is optional, and checked
	// where present.
	AssignmentsDigest string `json:"assignments_digest,omitempty"`
	// Operator is the identity of the person MAKING the request, resolved by
	// their own process exactly as the local path resolves it.
	//
	// It is carried because a decision records who made it, and the supervisor
	// resolving its own identity would record the supervisor - a service
	// account, or another person's login - for a decision somebody else made in
	// their terminal. The endpoint is owner-only, so the requester is the owner
	// of this state directory; recording their stated identity is exactly what
	// the local path records, with the same unverified provenance.
	Operator string `json:"operator,omitempty"`
	// Template, Deterministic and SubstituteHuman are a plan REVISION request,
	// mirroring the flags of the command that would otherwise drive it here.
	Template        string `json:"template,omitempty"`
	Deterministic   bool   `json:"deterministic,omitempty"`
	SubstituteHuman string `json:"substitute_human,omitempty"`
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
	// The plan lifecycle verbs. Each is an operator decision that already
	// exists as a command; the endpoint exists so the decision reaches the
	// supervisor that owns the work rather than racing it from a second
	// process.
	ControlPlanApprove = "plan-approve"
	ControlPlanReject  = "plan-reject"
	ControlPlanRevise  = "plan-revise"
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
	// Held across reclaim AND bind. Without it two supervisors starting at once
	// can both dial a stale socket, both find nobody listening, and the second
	// unlink the socket the first just bound - leaving two supervisors driving
	// one store.
	release, err := acquireControlStartLock(stateDir)
	if err != nil {
		return nil, err
	}
	defer release()
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
	_ = connection.SetDeadline(time.Now().Add(ControlDeadline("")))
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
	// The deadline is extended to what THIS command needs, now that the command
	// is known. Reading the request stays on the short one: a client that opens
	// a connection and says nothing holds a supervisor goroutine for exactly as
	// long as it takes to say nothing.
	_ = connection.SetDeadline(time.Now().Add(ControlDeadline(request.Command)))
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
	// The operator's note is TRUNCATED to what a payload field holds, exactly
	// as the local path truncates it before journalling. Sending it whole and
	// letting the request bound refuse the connection made a long note behave
	// differently depending on which process applied the decision, which is the
	// one thing the delegated path is supposed to make invisible.
	request.Note = BoundedNote(request.Note)
	request.Reason = BoundedNote(request.Reason)
	path := ControlSocketPath(stateDir)
	if err := AssertControlEndpointSecure(path); err != nil {
		return ControlResponse{}, err
	}
	connection, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return ControlResponse{}, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(ControlDeadline(request.Command) + 30*time.Second))
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
	running, _ := SupervisorPresence(stateDir)
	return running
}

// SupervisorPresence answers two different questions a dial cannot separate on
// its own: is a supervisor listening, and does an endpoint EXIST that this
// caller could not reach.
//
// They differ where it matters. A caller that reads "no supervisor" from a
// transient dial failure applies its decision locally - beside a live
// reconciler, outside the lock that exists to stop exactly that. A socket file
// that is present but unreachable is a supervisor to be waited for, not an
// absence to act around.
func SupervisorPresence(stateDir string) (running bool, endpointPresent bool) {
	path := ControlSocketPath(stateDir)
	connection, err := net.DialTimeout("unix", path, 2*time.Second)
	if err == nil {
		_ = connection.Close()
		return true, true
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return false, true
	}
	return false, false
}
