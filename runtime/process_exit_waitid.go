//go:build darwin || linux

package runtime

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// rootExited asks the kernel whether pid, a child of this process, has
// reached a TERMINAL state, WITHOUT reaping it:
// waitid(P_PID, pid, WEXITED|WNOHANG|WNOWAIT). The reaper goroutine in
// runBoundedProcess stays the only thing that reaps.
//
// ECHILD means the child is already reaped - by that reaper, which has simply
// not delivered yet - so it has exited. Any other failure is not evidence of
// exit and reports false: the process is then treated as still running and
// terminated for the context's cause.
//
// Only a TERMINAL transition reads as exited. With WNOHANG and nothing
// waitable both kernels leave the zeroed buffer zero. A reported child sets
// si_signo to SIGCHLD and si_code to the transition, which is not always an
// exit: darwin's waitid reports a STOPPED child even under WEXITED alone. So
// si_code must be one of the terminal codes, which have the same values on
// both kernels:
//
//	darwin <sys/signal.h>:          CLD_EXITED 1, CLD_KILLED 2, CLD_DUMPED 3,
//	                                CLD_TRAPPED 4, CLD_STOPPED 5, CLD_CONTINUED 6
//	linux <asm-generic/siginfo.h>:  the same six values
//
// si_code sits at the same offset on both: siginfo_t begins int32 si_signo,
// si_errno, si_code (darwin <sys/signal.h> struct __siginfo; linux as
// golang.org/x/sys/unix ztypes_linux_{amd64,arm64}.go declares Siginfo
// {Signo, Errno, Code int32, ...}). The buffer is larger than either.
func rootExited(pid int) bool {
	const pPID = 1 // P_PID on both linux and darwin
	for {
		var info [256]byte
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOHANG|syscall.WNOWAIT, 0, 0)
		switch errno {
		case 0:
			signo := int32(binary.NativeEndian.Uint32(info[0:4]))
			code := int32(binary.NativeEndian.Uint32(info[8:12]))
			return signo == int32(syscall.SIGCHLD) && terminalChildCode(code)
		case syscall.EINTR:
			continue
		case syscall.ECHILD:
			return true
		default:
			return false
		}
	}
}

// The terminal si_code values (see rootExited).
const (
	cldExited = 1 // exited normally
	cldKilled = 2 // killed by a signal
	cldDumped = 3 // killed by a signal, core dumped
)

func terminalChildCode(code int32) bool {
	return code == cldExited || code == cldKilled || code == cldDumped
}
