package cli

import "syscall"

// linuxSIGRTMIN is glibc's fixed base for real-time signals. The first few
// numbers above SIGRTMIN are reserved by glibc/pthread internals, which is
// exactly why waybar's own convention (and this one) starts counting user
// signals from SIGRTMIN, not SIGRTMIN+0 being special — SIGRTMIN+8 matches
// what "signal": 8 means in a waybar module config.
const linuxSIGRTMIN = 34

// syscallRTMIN returns the concrete signal number for SIGRTMIN+n.
func syscallRTMIN(n int) syscall.Signal {
	return syscall.Signal(linuxSIGRTMIN + n)
}

// signalPID sends sig to a process by PID.
func signalPID(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
