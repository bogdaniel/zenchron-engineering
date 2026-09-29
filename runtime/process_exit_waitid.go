//go:build darwin || linux

package runtime

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// rootExited asks the kernel whether pid, a child of this process, has
// exited, WITHOUT reaping it: waitid(P_PID, pid, WEXITED|WNOHANG|WNOWAIT).
// The reaper goroutine in runBoundedProcess stays the only thing that reaps.
//
// ECHILD means the child is already reaped - by that reaper, which has simply
// not delivered yet - so it has exited. Any other failure is not evidence of
// exit and reports false: the process is then treated as still running and
// terminated for the context's cause, which is the pre-#213 behaviour.
//
// With WNOHANG and nothing waitable both kernels leave si_signo zero in a
// zeroed buffer; an exited child sets it to SIGCHLD. si_signo is the first
// int of siginfo_t on both platforms, and the buffer is larger than either
// siginfo_t.
func rootExited(pid int) bool {
	const pPID = 1 // P_PID on both linux and darwin
	for {
		var info [256]byte
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOHANG|syscall.WNOWAIT, 0, 0)
		switch errno {
		case 0:
			return int32(binary.NativeEndian.Uint32(info[:4])) == int32(syscall.SIGCHLD)
		case syscall.EINTR:
			continue
		case syscall.ECHILD:
			return true
		default:
			return false
		}
	}
}
