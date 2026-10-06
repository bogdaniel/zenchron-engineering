package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const EventVerificationPermitChanged = "verification.permit_changed"

const ReasonVerificationCleanup = "verification_tool_cleanup_pending"

type VerificationPermitState string

const (
	VerificationWaiting  VerificationPermitState = "waiting"
	VerificationGranted  VerificationPermitState = "granted"
	VerificationReleased VerificationPermitState = "released"
)

// VerificationPermit is a nested resource grant, never another engineering
// operation or work slot. The owning tool retains its OS ownership lock until
// its whole execution has stopped; expiry alone does not release capacity.
type VerificationPermit struct {
	ID              string                  `json:"id"`
	Parent          ExecutionAttemptRef     `json:"parent"`
	ControllerOwner string                  `json:"controller_owner"`
	ToolOwner       string                  `json:"tool_owner"`
	State           VerificationPermitState `json:"state"`
	RequestedAt     time.Time               `json:"requested_at"`
	GrantedAt       *time.Time              `json:"granted_at,omitempty"`
	ReleasedAt      *time.Time              `json:"released_at,omitempty"`
	ExpiresAt       time.Time               `json:"expires_at"`
	Sandbox         *VerificationSandbox    `json:"sandbox,omitempty"`
	ToolLockDir     string                  `json:"tool_lock_dir,omitempty"`
}

// VerificationSandbox retains only the exact container recovery binding.
// Native process grants instead retain an inherited OS ownership lock.
type VerificationSandbox struct {
	Image    string         `json:"image"`
	StateDir string         `json:"state_dir"`
	Endpoint DockerEndpoint `json:"endpoint"`
}

func (p VerificationPermit) validate() error {
	if p.Sandbox != nil {
		if p.Sandbox.Image == "" || p.Sandbox.StateDir == "" {
			return errors.New("verification sandbox recovery binding is incomplete")
		}
		if _, err := p.Sandbox.Endpoint.identity(); err != nil {
			return err
		}
	}
	if err := p.Parent.Validate(); err != nil {
		return err
	}
	if p.ID == "" || p.ControllerOwner == "" || p.ToolOwner == "" || p.RequestedAt.IsZero() || p.ExpiresAt.Before(p.RequestedAt) {
		return errors.New("incomplete verification permit binding")
	}
	switch p.State {
	case VerificationWaiting:
		if p.GrantedAt != nil || p.ReleasedAt != nil {
			return errors.New("waiting verification carries execution times")
		}
	case VerificationGranted:
		if p.GrantedAt == nil || p.ReleasedAt != nil {
			return errors.New("granted verification has invalid times")
		}
	case VerificationReleased:
		if p.ReleasedAt == nil {
			return errors.New("released verification lacks its end")
		}
	default:
		return errors.New("unknown verification permit state")
	}
	if p.GrantedAt != nil && p.GrantedAt.Before(p.RequestedAt) {
		return errors.New("verification granted before request")
	}
	if p.ReleasedAt != nil && (p.ReleasedAt.Before(p.RequestedAt) || (p.GrantedAt != nil && p.ReleasedAt.Before(*p.GrantedAt))) {
		return errors.New("verification released before execution")
	}
	return nil
}

// VerificationPermitStore is the scheduler's existing durable I/O boundary
// specialized for nested grants. Transition and occupancy share the operation
// database's transaction, including the corresponding journal observation.
type VerificationPermitStore interface {
	Events(string) ([]EngineeringEvent, error)
	// VerificationPermits lists unreleased requests and grants; completed
	// identities remain durable and readable through VerificationPermit.
	VerificationPermits() ([]VerificationPermit, error)
	VerificationPermit(id string) (VerificationPermit, int64, bool, error)
	TransitionVerificationPermit(VerificationPermit, int64, int) (bool, error)
}

func (s Scheduler) permitStore() (VerificationPermitStore, error) {
	store, ok := s.Store.(VerificationPermitStore)
	if !ok {
		return nil, errors.New("operation store cannot enforce nested verification capacity")
	}
	return store, nil
}

func (s Scheduler) RequestVerification(parent ExecutionAttemptRef, toolID, toolOwner string) (VerificationPermit, error) {
	return s.requestVerification(parent, toolID, toolOwner, nil, "")
}

func (s Scheduler) requestVerification(parent ExecutionAttemptRef, toolID, toolOwner string, sandbox *VerificationSandbox, lockDir string) (VerificationPermit, error) {
	s = s.defaults()
	if err := parent.Validate(); err != nil {
		return VerificationPermit{}, err
	}
	if toolID == "" || toolOwner == "" {
		return VerificationPermit{}, errors.New("verification requires one tool execution identity and owner")
	}
	store, err := s.permitStore()
	if err != nil {
		return VerificationPermit{}, err
	}
	now := s.Clock.Now()
	p := VerificationPermit{ID: verificationPermitID(parent, toolID),
		Parent: parent, ControllerOwner: s.Owner, ToolOwner: toolOwner, State: VerificationWaiting,
		RequestedAt: now, ExpiresAt: now.Add(s.LeaseDuration), Sandbox: sandbox, ToolLockDir: lockDir}
	ok, err := store.TransitionVerificationPermit(p, 0, s.MaxConcurrentVerifications)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, errors.New("verification parent is unavailable or tool identity already exists")
	}
	return p, nil
}

func verificationPermitID(parent ExecutionAttemptRef, toolID string) string {
	return StableOperationKey(parent.RunID, "verification.tool", parent.OperationID, strconv.Itoa(parent.Attempt), toolID)
}

func (s Scheduler) AcquireVerification(request VerificationPermit) (bool, error) {
	s = s.defaults()
	if err := s.reclaimVerificationPermits(); err != nil {
		return false, err
	}
	if !s.Liveness.Alive(s.Owner) {
		return false, errors.New("verification controller is no longer alive")
	}
	store, err := s.permitStore()
	if err != nil {
		return false, err
	}
	p, revision, found, err := store.VerificationPermit(request.ID)
	if err != nil {
		return false, err
	}
	if !found || p.ToolOwner != request.ToolOwner || p.ControllerOwner != s.Owner || p.Parent != request.Parent || p.State != VerificationWaiting {
		return false, errors.New("verification request is not owned or has already been consumed")
	}
	op, _, found, err := s.Store.Operation(p.Parent.OperationID)
	if err != nil {
		return false, err
	}
	now := s.Clock.Now()
	if !found || op.State != Running || op.Lease == nil || op.Lease.Owner != s.Owner || op.CancelRequested ||
		op.RunID != p.Parent.RunID || op.AttemptIdentity != p.Parent.Attempt || (op.Deadline != nil && !now.Before(*op.Deadline)) {
		return false, errors.New("verification parent attempt is no longer running")
	}
	if full, err := s.VerificationSaturated(p.Parent.RunID); err != nil || full {
		return false, err
	}
	p.State, p.GrantedAt, p.ExpiresAt = VerificationGranted, &now, now.Add(s.LeaseDuration)
	return store.TransitionVerificationPermit(p, revision, s.MaxConcurrentVerifications)
}

func (s Scheduler) ReleaseVerification(request VerificationPermit) error {
	s = s.defaults()
	store, err := s.permitStore()
	if err != nil {
		return err
	}
	for {
		p, revision, found, err := store.VerificationPermit(request.ID)
		if err != nil {
			return err
		}
		if !found || p.ToolOwner != request.ToolOwner || p.Parent != request.Parent {
			return errors.New("verification permit is not owned")
		}
		if p.State == VerificationReleased {
			return nil
		}
		if p.State == VerificationGranted && s.verificationOwnerAlive(p) {
			return errors.New("verification execution is still alive or its death is unverified")
		}
		now := s.Clock.Now()
		p.State, p.ReleasedAt = VerificationReleased, &now
		ok, err := store.TransitionVerificationPermit(p, revision, s.MaxConcurrentVerifications)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
}

func (s Scheduler) reclaimVerificationPermits() error {
	s = s.defaults()
	store, ok := s.Store.(VerificationPermitStore)
	if !ok {
		return nil
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		return err
	}
	for _, p := range permits {
		if err := s.reclaimVerificationPermit(p); err != nil {
			var uncertain *verificationCleanupUncertain
			if errors.As(err, &uncertain) {
				// This permit remains durable and occupies its slot. Its own run is
				// blocked by VerificationCleanupPending; unrelated runs remain eligible.
				continue
			}
			return err
		}
	}
	return nil
}

type verificationCleanupUncertain struct{ cause error }

func (e *verificationCleanupUncertain) Error() string {
	return "nested verification cleanup remains uncertain: " + e.cause.Error()
}
func (e *verificationCleanupUncertain) Unwrap() error { return e.cause }

func (s Scheduler) reclaimVerificationPermit(p VerificationPermit) error {
	s = s.defaults()
	if p.State == VerificationReleased || s.Clock.Now().Before(p.ExpiresAt) || s.verificationOwnerAlive(p) {
		return nil
	}
	// Death and expiry are both required, as for the enclosing lease.
	if p.Sandbox != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := p.Sandbox.sandbox(p.ID).ReconcileDockerOperation(ctx)
		cancel()
		if err != nil {
			return &verificationCleanupUncertain{cause: err}
		}
	}
	if err := s.ReleaseVerification(p); err != nil {
		return fmt.Errorf("reclaim verification: %w", err)
	}
	return nil
}

func (s Scheduler) verificationOwnerAlive(p VerificationPermit) bool {
	if p.ToolLockDir != "" {
		return NewLockOwnerLiveness(p.ToolLockDir).Alive(p.ToolOwner)
	}
	return s.Liveness.Alive(p.ToolOwner)
}

func (s Scheduler) VerificationCleanupPending(runID string) (bool, error) {
	store, ok := s.Store.(VerificationPermitStore)
	if !ok {
		return false, nil
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		return false, err
	}
	for _, p := range permits {
		if p.Parent.RunID == runID && p.State == VerificationGranted {
			return true, nil
		}
	}
	return false, nil
}

func (b VerificationSandbox) sandbox(id string) DockerSandbox {
	return DockerSandbox{Image: b.Image, StateDir: b.StateDir, Endpoint: b.Endpoint, OperationID: id}
}
