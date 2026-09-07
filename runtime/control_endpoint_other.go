//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package runtime

// On a platform where this runtime cannot verify POSIX ownership modes, the
// control endpoint is REFUSED rather than opened on trust.
//
// That is the same conservative choice owner_lock_other.go and
// process_windows.go already make: an authority boundary that cannot be
// checked is not a boundary. The endpoint decides whether coding agents run
// under the operator's account, so "probably fine" is not an acceptable
// answer, and a supervisor that cannot prove the boundary declines to offer
// one instead of publishing a control path whose exposure it cannot describe.
//
// Everything else still works on such a platform: runs are driven by the
// operator command that started them, exactly as they were before `serve`
// existed. What is unavailable is the persistent supervisor's control path,
// and it is unavailable loudly.

import "errors"

const ControlEndpointMechanism = "unavailable: this platform cannot verify owner-only permissions on a local control endpoint"

var errControlEndpointUnsupported = errors.New(
	"a local control endpoint requires a platform where owner-only permissions can be verified; " +
		"anything able to reach the endpoint can start coding agents under this account, so it is refused rather than opened unverified")

func assertOwnerOnlyDir(string) error { return errControlEndpointUnsupported }

func AssertControlEndpointSecure(path string) error {
	return &ControlEndpointError{Path: path, Detail: errControlEndpointUnsupported.Error()}
}

// acquireControlStartLock is unreachable here: this platform refuses the
// endpoint before startup serialization could matter.
func acquireControlStartLock(string) (func(), error) { return func() {}, ErrControlEndpointUnsupported }
