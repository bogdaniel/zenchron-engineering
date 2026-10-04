package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A CANDIDATE WRITER LOCK OUTLIVES ITS OWNER FOR AS LONG AS ANY WRITER DOES
// (#168). The owner-death guard stops the provider's process GROUP, but a
// descendant that left the group - setsid(2), or a provider that puts each
// tool command in a group of its own - survives a dead supervisor and keeps
// writing the candidate. Once its owner is dead its ancestry is gone too, so
// nothing a new owner can read from the process table says it belongs to this
// run. What it does still carry is every descriptor it inherited, and a flock
// belongs to the open file description, not to the process that took it: the
// provider is handed the lock descriptor, so every descendant that keeps it
// keeps the candidate locked, however it detached and whoever died.
//
// IT IS COOPERATIVE, NOT A SECURITY BOUNDARY. It sees only processes that kept
// the inherited descriptor: a provider can `flock -u 3`, close it, or (when
// unsandboxed) unlink the lock file. Whether a provider's tool commands
// inherit it at all is a property of the provider (docs/agents.md records
// which are verified).
//
// ponytail: refuses rather than terminates a writer that escaped its dead
// owner; naming and stopping the lock's holders is the upgrade if these
// refusals show up in the field.
type candidateWriterKey struct{}

func withCandidateWriter(ctx context.Context, lock *os.File) context.Context {
	return context.WithValue(ctx, candidateWriterKey{}, lock)
}

func candidateWriterFrom(ctx context.Context) *os.File {
	lock, _ := ctx.Value(candidateWriterKey{}).(*os.File)
	return lock
}

func candidateWriterLockPath(candidateDir string) string {
	return filepath.Clean(candidateDir) + ".writer.lock"
}

// candidateWriterSettle is how long a FIRST claim waits for a held lock to be
// released before refusing: three times the default owner-death guard grace,
// so a supervisor restarted while its predecessor's guard is still stopping
// the old group does not wait spuriously. A run already waiting on a held lock
// probes once and does not settle again: every waiting pass is a work-class
// operation, and a 15s sleep per pass would hold a work slot and be charged
// as active time. A parent-context cancellation (a controller shutdown) ends
// the settle; a durable operator stop does not, because it is observed later,
// just before the provider would start. A var only so tests can shorten it.
var candidateWriterSettle = 15 * time.Second

// candidateWriterSettling, when set, is called each time a claim finds the
// lock held, before it consults ctx: a test seam that observes the settle from
// inside it rather than guessing when it started. Nil in production.
var candidateWriterSettling func()

// claimCandidateWriter takes the candidate's writer lock before anything
// touches the candidate for a provider attempt. It answers exactly one of:
//
//   - acquired: the lock, which the caller closes when its attempt ends;
//   - held by another after settle: *CandidateWriterAliveError, Cause nil;
//   - unavailable, the lock could not be checked: the same type with Cause;
//   - cancelled: ctx ended first, returned as ctx.Err() wrapped.
//
// What a cancellation MEANS - a controller shutdown, a deadline - is the
// caller's to say; this primitive never reports one as a live writer.
func claimCandidateWriter(ctx context.Context, candidateDir string, settle time.Duration) (*os.File, error) {
	path := candidateWriterLockPath(candidateDir)
	deadline := time.Now().Add(settle)
	for {
		lock, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			return nil, &CandidateWriterAliveError{Lock: path, Cause: err}
		}
		locked, err := tryLockFile(lock, true)
		if err == nil && locked {
			return lock, nil
		}
		_ = lock.Close()
		if err != nil {
			return nil, &CandidateWriterAliveError{Lock: path, Cause: err}
		}
		if candidateWriterSettling != nil {
			candidateWriterSettling()
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("claiming the candidate writer lock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			return nil, &CandidateWriterAliveError{Lock: path}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeCandidateWriterLock removes the lock file of a candidate that was
// itself removed.
func removeCandidateWriterLock(candidateDir string) error {
	if err := os.Remove(candidateWriterLockPath(candidateDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// candidateWriterFree reports whether nothing holds the lock at path. A lock
// file that is absent is free; one that cannot be checked is not.
func candidateWriterFree(path string) bool {
	lock, err := os.Open(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	defer lock.Close()
	locked, err := tryLockFile(lock, true)
	return err == nil && locked
}

// CandidateWriterAliveError is the pre-dispatch refusal for a candidate an
// earlier invocation's process may still be writing. It is the runtime's own
// refusal, not a provider fault: nothing was invoked and nothing was touched.
// With a Cause the lock could not be checked at all.
type CandidateWriterAliveError struct {
	Lock  string
	Cause error
}

func (e *CandidateWriterAliveError) Error() string {
	if e.Cause != nil {
		return "refusing to dispatch: the candidate writer lock " + e.Lock + " could not be checked: " + e.Cause.Error() +
			". No provider was invoked"
	}
	return "refusing to dispatch: a process from an earlier invocation still holds the candidate writer lock " + e.Lock +
		" and may still be writing the candidate workspace; stop it (`lsof " + e.Lock + "` names it) and resume." +
		" No provider was invoked. This check sees only processes that kept the provider's inherited descriptor" +
		" (verified for codex tool commands; node-spawned children do not keep it; unverified for Claude Code)"
}

func (e *CandidateWriterAliveError) Unwrap() error { return e.Cause }
