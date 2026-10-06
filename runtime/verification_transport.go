package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This is transport through an already-granted scratch directory, not a
// resource queue. Only Scheduler and its atomic store decide admission.
type verificationRequest struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Owner   string `json:"owner"`
	Release bool   `json:"release,omitempty"`
}

type verificationReply struct {
	Permit    VerificationPermit `json:"permit"`
	Error     string             `json:"error,omitempty"`
	Signature []byte             `json:"signature,omitempty"`
}

const verificationCleanupLimit = 5 * time.Second

func writeVerificationReply(path string, reply verificationReply, key ed25519.PrivateKey) error {
	data, err := CanonicalJSON(reply)
	if err != nil {
		return err
	}
	reply.Signature = ed25519.Sign(key, data)
	return writeVerificationMessage(path, reply)
}

func readVerificationMessage(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCanonicalPayloadBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxCanonicalPayloadBytes {
		return errors.New("verification message exceeds payload ceiling")
	}
	canonical, err := CanonicalJSON(json.RawMessage(data))
	if err != nil {
		return err
	}
	return strictJSON(canonical, target)
}

func writeVerificationMessage(path string, message any) error {
	data, err := CanonicalJSON(message)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".verification-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func serveVerificationTools(ctx context.Context, v verificationExecution, grant verificationToolGrant) (context.Context, func() error, error) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return ctx, nil, err
	}
	grant.PublicKey = public
	base, err := GitGuardDir(v.StateDir, v.Parent)
	if err != nil {
		return ctx, nil, err
	}
	if err := writeVerificationMessage(filepath.Join(base, "verification", "grant.json"), grant); err != nil {
		return ctx, nil, err
	}
	if err := os.MkdirAll(grant.RequestDir, 0700); err != nil {
		return ctx, nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan error, 1)
	go func() {
		pending := map[string]VerificationPermit{}
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			if err := reconcileVerificationRequests(v, grant, pending, key); err != nil {
				cancel(err)
				done <- err
				return
			}
			select {
			case <-ctx.Done():
				var stopped error
				for id, p := range pending {
					// Waiting owns no workload. A held workload remains durable
					// until its ownership lock proves it stopped.
					if p.State == VerificationWaiting {
						stopped = errors.Join(stopped, v.Scheduler.ReleaseVerification(p))
					}
					stopped = errors.Join(stopped, writeVerificationReply(filepath.Join(grant.RequestDir, id+".reply.json"), verificationReply{Permit: p, Error: "parent execution stopped"}, key))
				}
				done <- stopped
				return
			case <-tick.C:
			}
		}
	}()
	return ctx, func() error {
		cancel(nil)
		return errors.Join(<-done, v.finishNativeTools())
	}, nil
}

func (v verificationExecution) finishNativeTools() error {
	deadline := time.NewTimer(verificationCleanupLimit)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		store, err := v.Scheduler.permitStore()
		if err != nil {
			return err
		}
		permits, err := store.VerificationPermits()
		if err != nil {
			return err
		}
		live := false
		for _, p := range permits {
			if p.Parent != v.Parent || p.State != VerificationGranted {
				continue
			}
			if v.Scheduler.verificationOwnerAlive(p) {
				live = true
				continue
			}
			if err := v.Scheduler.ReleaseVerification(p); err != nil {
				return err
			}
		}
		if !live {
			return nil
		}
		select {
		case <-deadline.C:
			return errors.New("native tool cleanup remains unverified; durable capacity retained")
		case <-tick.C:
		}
	}
}

func reconcileVerificationRequests(v verificationExecution, grant verificationToolGrant, pending map[string]VerificationPermit, key ed25519.PrivateKey) error {
	// ponytail: scan attempt-local request files, fine below ~10k tools per
	// attempt; index live requests if that ceiling is reached.
	entries, err := os.ReadDir(grant.RequestDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".request.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".request.json")
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			return errors.New("invalid verification request identity")
		}
		var request verificationRequest
		if err := readVerificationMessage(filepath.Join(grant.RequestDir, entry.Name()), &request); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if request.ID != id || grant.Tools[request.Tool] == "" {
			return errors.New("verification request exceeds its tool grant")
		}
		p, found := pending[id]
		if !found {
			if request.Release {
				continue
			}
			p, err = v.Scheduler.requestVerification(grant.Parent, id, request.Owner, nil, grant.ScratchDir)
			if err != nil {
				return err
			}
			pending[id] = p
		}
		if request.Owner != p.ToolOwner {
			return errors.New("verification request changed its owner")
		}
		if request.Release || (p.State == VerificationWaiting && !v.Scheduler.verificationOwnerAlive(p)) {
			if p.State != VerificationGranted || !v.Scheduler.verificationOwnerAlive(p) {
				if err := v.Scheduler.ReleaseVerification(p); err != nil {
					return err
				}
			}
		} else if p.State == VerificationGranted {
			if err := v.Scheduler.reclaimVerificationPermit(p); err != nil {
				return err
			}
		} else if p.State == VerificationWaiting {
			if _, err := v.Scheduler.AcquireVerification(p); err != nil {
				return err
			}
		}
		store, err := v.Scheduler.permitStore()
		if err != nil {
			return err
		}
		current, _, found, err := store.VerificationPermit(p.ID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("verification grant disappeared")
		}
		if current.State != p.State || !foundReply(grant.RequestDir, id) {
			if err := writeVerificationReply(filepath.Join(grant.RequestDir, id+".reply.json"), verificationReply{Permit: current}, key); err != nil {
				return err
			}
		}
		pending[id] = current
	}
	return nil
}

func foundReply(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, id+".reply.json"))
	return err == nil
}

func waitVerificationReply(ctx context.Context, grant verificationToolGrant, request verificationRequest, released bool) (VerificationPermit, error) {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		reply, err := readVerificationReply(grant, request)
		if err != nil && !os.IsNotExist(err) {
			return VerificationPermit{}, err
		}
		if err == nil {
			if reply.Error != "" {
				return reply.Permit, errors.New(reply.Error)
			}
			p := reply.Permit
			if released && p.State == VerificationReleased {
				return p, nil
			}
			if !released && p.State == VerificationGranted {
				return p, nil
			}
			if !released && p.State == VerificationReleased {
				return p, errors.New("verification request ended before execution")
			}
		}
		select {
		case <-ctx.Done():
			return VerificationPermit{}, ctx.Err()
		case <-tick.C:
		}
	}
}

func readVerificationReply(grant verificationToolGrant, request verificationRequest) (verificationReply, error) {
	var reply verificationReply
	if err := readVerificationMessage(filepath.Join(grant.RequestDir, request.ID+".reply.json"), &reply); err != nil {
		return reply, err
	}
	signature := reply.Signature
	reply.Signature = nil
	data, err := CanonicalJSON(reply)
	if err != nil {
		return reply, err
	}
	if !ed25519.Verify(grant.PublicKey, data, signature) {
		return reply, errors.New("verification response is not controller signed")
	}
	p := reply.Permit
	if p.Parent != grant.Parent || p.ToolOwner != request.Owner || p.ControllerOwner != grant.ControllerOwner ||
		p.ID != verificationPermitID(grant.Parent, request.ID) || p.ToolLockDir != grant.ScratchDir {
		return reply, errors.New("verification response binding mismatch")
	}
	return reply, p.validate()
}

func finishNativeVerification(grant verificationToolGrant, request verificationRequest, lock *ControllerInstanceLock) error {
	err := lock.file.Close()
	lock.file = nil
	request.Release = true
	err = errors.Join(err, writeVerificationMessage(filepath.Join(grant.RequestDir, request.ID+".request.json"), request))
	if NewLockOwnerLiveness(grant.ScratchDir).Alive(request.Owner) {
		return errors.Join(err, fmt.Errorf("verification descendants still hold their ownership lock"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), verificationCleanupLimit)
	defer cancel()
	_, releaseErr := waitVerificationReply(ctx, grant, request, true)
	if releaseErr == nil {
		err = errors.Join(err, os.Remove(filepath.Join(grant.RequestDir, request.ID+".request.json")), os.Remove(filepath.Join(grant.RequestDir, request.ID+".reply.json")))
	}
	return errors.Join(err, releaseErr)
}
