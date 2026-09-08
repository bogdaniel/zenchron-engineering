package runtime

// Parallel candidate clones turn disk into an operator-level resource.
//
// One run's workspace is a full clone plus its artifacts; ten concurrent runs
// are ten of them. Without a bound the failure mode is the worst one available:
// ENOSPC in the middle of a Git operation, which corrupts nothing the runtime
// can prove and leaves an operator with a half-written workspace and no typed
// reason for it.
//
// So the ceiling is checked BEFORE a workspace is allocated, and exceeding it
// is a typed WAIT: an operator frees space or raises the bound, and the same
// run continues against the same candidate. It is deliberately not a failure -
// nothing about the engineering work went wrong - and deliberately not a
// silent reclamation: active run state is never deleted to make room for other
// active run state, because that would trade one run's evidence for another's
// progress.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// StateStorageError is the typed refusal to allocate more local state.
type StateStorageError struct {
	Dir           string
	UsedBytes     int64
	CeilingBytes  int64
	RequiredBytes int64
}

func (e *StateStorageError) Error() string {
	return fmt.Sprintf(
		"state storage at %s holds %d bytes against an operator ceiling of %d, and this run needs about %d more; "+
			"free space or raise storage.max_state_bytes, then resume - the candidate is untouched",
		e.Dir, e.UsedBytes, e.CeilingBytes, e.RequiredBytes)
}

// StateStorage is the operator's bound on local runtime state.
type StateStorage struct {
	// Dir is the operator state directory the bound applies to.
	Dir string
	// CeilingBytes is the operator-authorized maximum. Zero means UNBOUNDED,
	// which is the pre-#63 behaviour and stays the default: introducing a
	// ceiling nobody configured would start refusing work that used to run.
	CeilingBytes int64
}

// estimatedCandidateBytes is what one new run is assumed to need before its
// workspace exists. It cannot be measured - the clone has not happened - so it
// is a stated reservation rather than a prediction.
//
// ponytail: a fixed reservation, sized for an ordinary source repository. If
// repositories vary enough that this is wrong in practice, measuring the base
// clone once per repository and reusing that figure is the upgrade.
const estimatedCandidateBytes = 512 << 20

// Reserve admits ONE candidate workspace and holds the admission until the
// workspace exists.
//
// Admit alone was a check with no reservation, and the supervisor deliberately
// drives runs concurrently: two runs could measure the same `used`, both find
// room for one more candidate, and both then clone - overshooting the ceiling
// and hitting ENOSPC mid-work, which is the exact failure the bound exists to
// prevent.
//
// The lock is an flock on a file in the state directory, so it serializes
// across PROCESSES as well as goroutines: two supervisors, or a supervisor and
// an `autonomy run` command, share one state directory and one ceiling. It is
// released by the kernel if a process dies holding it.
//
// The returned release must be called once the workspace is created or the
// attempt is abandoned.
func (s StateStorage) Reserve() (release func(), err error) {
	if s.CeilingBytes <= 0 || s.Dir == "" {
		return func() {}, nil
	}
	unlock, err := lockStateAllocation(s.Dir)
	if err != nil {
		// A directory that cannot be locked is not a reason to refuse work, for
		// the same reason an unmeasurable one is not: the bound is a resource
		// courtesy, not a security boundary.
		return func() {}, nil
	}
	if err := s.Admit(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// Admit reports whether another candidate workspace may be allocated. An
// unconfigured ceiling admits everything, which is exactly what it means.
//
// It measures and does not reserve; callers that are about to allocate use
// Reserve, which holds the admission across the allocation.
func (s StateStorage) Admit() error {
	if s.CeilingBytes <= 0 || s.Dir == "" {
		return nil
	}
	used, err := s.Usage()
	if err != nil {
		// A directory that cannot be measured is not a reason to refuse work.
		// The bound is a resource courtesy, not a security boundary, and
		// failing closed here would turn an unreadable subdirectory into an
		// outage.
		return nil
	}
	if used+estimatedCandidateBytes <= s.CeilingBytes {
		return nil
	}
	return &StateStorageError{
		Dir: s.Dir, UsedBytes: used, CeilingBytes: s.CeilingBytes, RequiredBytes: estimatedCandidateBytes,
	}
}

// Usage is the bytes the state directory currently holds. Symlinks are counted
// as links rather than followed, so a link into the operator's home cannot make
// the runtime measure - or later reclaim against - something outside its own
// state.
//
// ponytail: a full walk per allocation. It runs once per new run, not per
// operation, and a state directory is a few thousand files; caching the figure
// with an invalidation on write is the upgrade if that stops being true.
func (s StateStorage) Usage() (int64, error) {
	if s.Dir == "" {
		return 0, fmt.Errorf("a state directory is required")
	}
	var total int64
	err := filepath.WalkDir(s.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable subtree does not invalidate the measurement of
			// the rest; it is skipped and the total is a lower bound.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// lockStateAllocation serializes candidate allocation for one state directory.
// It is a separate file from the control-endpoint start lock: they guard
// different things and holding one must never imply the other.
func lockStateAllocation(dir string) (func(), error) {
	return acquireStateAllocationLock(dir)
}
