package runtime

// THE PRIVILEGED CONTROL SESSION: a transport carrying proofs and commands,
// and never a source of authority.
//
// The ordinary control endpoint answers one request per connection, which is
// right for an operator command and wrong for a handoff: proving who is
// listening and then acting on a SECOND connection proves nothing, because the
// path can change hands in between. A session keeps the proof and the action on
// one live connection.
//
// THREE INDEPENDENT BINDINGS, and a privileged operation needs all of them:
//
//	connection   the action reaches the same peer that answered the proof
//	identity     that peer is the generation the caller expected
//	role lease   that peer holds exclusive controller-role authority, live
//	             through the operation's commit
//
// They are kept apart on purpose. Collapsing them into an "authenticated
// controller" would hide which proof failed, and worse, would let any one of
// them speak for the others. Two processes of one immutable generation prove
// the SAME identity - that is what a generation is - so identity can never
// answer which of them owns the role. Only possession answers that.
//
// THE CONNECTION IS NOT A CREDENTIAL. A successful proof does not upgrade the
// socket into a trusted session for its lifetime; every privileged operation
// crosses WithAuthority again. So a connection may outlive the role and simply
// stop being able to change anything, which is the honest behaviour: connection
// lifetime and authority lifetime are different lifetimes.
//
// THE PROOF IS EVIDENCE, NOT A BEARER TOKEN. It is bound to the exchange that
// requested it by echoing the caller's challenge, so a captured proof document
// cannot be replayed later as though it were permission.
//
// READ-ONLY STAYS READABLE. Identity and status do not require the role,
// because losing ownership must not also cost observability - an operator
// diagnosing a stuck handoff needs to see the state of the thing that is stuck.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Control session commands. They are deliberately separate from the one-shot
// ControlRequest vocabulary: an operator command and a controller transition
// are different conversations, and sharing a verb space would make the
// privileged ones reachable from the ordinary path.
const (
	// ControlProveIdentity asks the serving process who it is. It requires no
	// authority and is answered by any live endpoint.
	ControlProveIdentity = "session.prove_identity"
	// ControlPrivileged carries an operation that may only run under the
	// serving process's own live controller-role lease.
	ControlPrivileged = "session.privileged"
)

// ControllerProof is the serving process's answer to "which generation are
// you". It is measured by that process, about that process.
type ControllerProof struct {
	// Challenge echoes the caller's nonce, binding this document to this
	// exchange. A proof that echoed nothing would be replayable as a bearer
	// token, which is exactly what it must not become.
	Challenge string               `json:"challenge"`
	Self      ControllerSelfRecord `json:"self"`
}

// controlSessionMessage is one line on a session connection.
type controlSessionMessage struct {
	Command   string          `json:"command"`
	Challenge string          `json:"challenge,omitempty"`
	Request   *ControlRequest `json:"request,omitempty"`
}

type controlSessionReply struct {
	Proof    *ControllerProof `json:"proof,omitempty"`
	Response *ControlResponse `json:"response,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// PrivilegedControl is what a serving process brings to a session: its own
// identity, its own role lease, and the operation it is willing to perform.
//
// The lease is held BY THE SERVER. A client cannot supply one, which is the
// point: authorization is the serving process's possession of a capability,
// never the caller's assertion about it.
type PrivilegedControl struct {
	Identity ControllerSelfRecord
	Lease    *ControllerRoleLease
	// Execute performs the privileged operation. It runs inside the lease's
	// authority section, so it is already serialized against release.
	Execute func(ControlRequest) ControlResponse
	// Observe answers read-only commands and requires no authority at all.
	Observe func(ControlRequest) ControlResponse
}

// ServeSessions answers session connections until the listener closes. It is
// the privileged counterpart to Serve, which answers one operator command per
// connection: a transition needs a conversation, and an operator command does
// not.
func (l *ControlListener) ServeSessions(control PrivilegedControl) error {
	for {
		connection, err := l.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() { _ = ServeControlSession(connection, control) }()
	}
}

// ServeControlSession answers one session connection.
//
// It tracks whether a proof happened ON THIS CONNECTION, so a caller cannot
// prove against one endpoint and then act against another. That is a server
// obligation rather than a client courtesy: a client's verification answers
// "did I reach the process I expected", which must never be mistaken for "that
// process may therefore perform this transition".
func ServeControlSession(connection net.Conn, control PrivilegedControl) error {
	defer connection.Close()
	reader := bufio.NewReaderSize(io.LimitReader(connection, maxControlRequestBytes*8), maxControlRequestBytes)
	proven := false
	for {
		_ = connection.SetDeadline(time.Now().Add(ControlDeadline("")))
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			return nil // the peer hung up
		}
		var message controlSessionMessage
		if err := json.Unmarshal(line, &message); err != nil {
			writeSessionReply(connection, controlSessionReply{Error: "control session message is not a JSON object"})
			return nil
		}
		_ = connection.SetDeadline(time.Now().Add(ControlDeadline(message.Command)))
		switch message.Command {
		case ControlProveIdentity:
			proven = true
			writeSessionReply(connection, controlSessionReply{
				Proof: &ControllerProof{Challenge: message.Challenge, Self: control.Identity},
			})
		case ControlPrivileged:
			writeSessionReply(connection, control.privileged(proven, message.Request))
		default:
			// Anything else is read-only observation, which needs no authority.
			writeSessionReply(connection, control.observe(message.Request))
		}
		if err != nil {
			return nil
		}
	}
}

// privileged is the server-side authorization boundary.
//
// EVERY privileged operation crosses it, including the second one on a
// connection whose first already succeeded. The commit happens inside
// WithAuthority, so a release that begins mid-operation waits for it and an
// operation that begins after a release is refused - on a connection that is
// still perfectly open.
func (p PrivilegedControl) privileged(proven bool, request *ControlRequest) controlSessionReply {
	if !proven {
		return controlSessionReply{Error: "a privileged control operation requires an identity proof on this connection"}
	}
	if request == nil {
		return controlSessionReply{Error: "a privileged control operation requires a request"}
	}
	if p.Execute == nil {
		return controlSessionReply{Error: "this controller performs no privileged control operations"}
	}
	var response ControlResponse
	if err := p.Lease.WithAuthority(func() error {
		response = p.Execute(*request)
		return nil
	}); err != nil {
		return controlSessionReply{Error: err.Error()}
	}
	return controlSessionReply{Response: &response}
}

func (p PrivilegedControl) observe(request *ControlRequest) controlSessionReply {
	if p.Observe == nil || request == nil {
		return controlSessionReply{Error: "this controller answers no read-only control commands"}
	}
	response := p.Observe(*request)
	return controlSessionReply{Response: &response}
}

func writeSessionReply(connection net.Conn, reply controlSessionReply) {
	encoded, err := json.Marshal(reply)
	if err != nil {
		encoded = []byte(`{"error":"the response could not be encoded"}`)
	}
	_, _ = connection.Write(append(encoded, '\n'))
}

// ---------------------------------------------------------------------------
// Client half
// ---------------------------------------------------------------------------

// ControlSession is one live conversation with a serving controller. Proof and
// action share it, because a proof about a path is not a proof about a peer.
type ControlSession struct {
	connection net.Conn
	reader     *bufio.Reader
	proven     *ControllerProof
}

// DialControlSession opens a session against the endpoint in a state directory.
func DialControlSession(stateDir string) (*ControlSession, error) {
	path := ControlSocketPath(stateDir)
	if err := AssertControlEndpointSecure(path); err != nil {
		return nil, err
	}
	connection, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &ControlSession{connection: connection, reader: bufio.NewReaderSize(connection, 1<<20)}, nil
}

// Close ends the conversation. It does not end anybody's authority: the role
// lease belongs to the serving process and is unaffected.
func (s *ControlSession) Close() error {
	if s == nil || s.connection == nil {
		return nil
	}
	return s.connection.Close()
}

// ProveControllerIdentity asks the peer which generation it is and checks the
// answer against what the caller expected.
//
// The name is deliberately not "authenticate". Authentication suggests one
// broad trusted state; this establishes exactly one fact - which generation is
// on the other end of this connection - and no authority follows from it.
func (s *ControlSession) ProveControllerIdentity(expected ControllerBinding) (ControllerProof, error) {
	challenge, err := controlChallenge()
	if err != nil {
		return ControllerProof{}, err
	}
	reply, err := s.exchange(controlSessionMessage{Command: ControlProveIdentity, Challenge: challenge})
	if err != nil {
		return ControllerProof{}, err
	}
	if reply.Proof == nil {
		return ControllerProof{}, fmt.Errorf("the endpoint returned no identity proof: %s", reply.Error)
	}
	if reply.Proof.Challenge != challenge {
		// A proof that does not answer THIS exchange is a document about some
		// other moment, and a document about some other moment is a replay.
		return ControllerProof{}, fmt.Errorf("the identity proof does not answer this exchange")
	}
	if err := reply.Proof.Self.ProvesGeneration(expected); err != nil {
		return ControllerProof{}, fmt.Errorf("the endpoint is not the expected generation: %w", err)
	}
	s.proven = reply.Proof
	return *reply.Proof, nil
}

// ExecutePrivilegedControl sends an operation the peer may only perform under
// its own live role lease.
//
// The client refuses to send one before proving the peer, which is a courtesy
// to the caller rather than a security boundary: the server refuses it anyway.
func (s *ControlSession) ExecutePrivilegedControl(request ControlRequest) (ControlResponse, error) {
	if s.proven == nil {
		return ControlResponse{}, fmt.Errorf("prove the controller's identity on this session before a privileged operation")
	}
	reply, err := s.exchange(controlSessionMessage{Command: ControlPrivileged, Request: &request})
	if err != nil {
		return ControlResponse{}, err
	}
	if reply.Error != "" {
		return ControlResponse{}, fmt.Errorf("%s", reply.Error)
	}
	if reply.Response == nil {
		return ControlResponse{}, fmt.Errorf("the endpoint returned no response")
	}
	return *reply.Response, nil
}

// ObserveControl sends a read-only command, which needs no authority.
func (s *ControlSession) ObserveControl(request ControlRequest) (ControlResponse, error) {
	reply, err := s.exchange(controlSessionMessage{Command: request.Command, Request: &request})
	if err != nil {
		return ControlResponse{}, err
	}
	if reply.Error != "" {
		return ControlResponse{}, fmt.Errorf("%s", reply.Error)
	}
	if reply.Response == nil {
		return ControlResponse{}, fmt.Errorf("the endpoint returned no response")
	}
	return *reply.Response, nil
}

func (s *ControlSession) exchange(message controlSessionMessage) (controlSessionReply, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return controlSessionReply{}, err
	}
	_ = s.connection.SetDeadline(time.Now().Add(ControlDeadline(message.Command) + 30*time.Second))
	if _, err := s.connection.Write(append(encoded, '\n')); err != nil {
		return controlSessionReply{}, err
	}
	line, err := s.reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return controlSessionReply{}, err
	}
	var reply controlSessionReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return controlSessionReply{}, fmt.Errorf("control session reply is not a JSON object")
	}
	return reply, nil
}

// controlChallenge is a fresh nonce per proof. It exists to bind a proof to one
// exchange, not to authenticate anybody: the socket is local and owner-only,
// and the authority question is answered by a capability rather than by this.
func controlChallenge() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
