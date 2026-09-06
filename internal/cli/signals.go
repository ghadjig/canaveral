package cli

// Which signals cut a run short.

import (
	"os"
	"os/signal"
	"syscall"
)

// StopSignals lists the signals that should cancel canaveral's context, so a
// run cut short still tears down what it had started.
//
// SIGHUP is here because closing the terminal a launch is running in used to
// kill canaveral outright: no deferred cleanup, so the services that run had
// already started stayed up holding the feature's ports, with nothing left
// that knew to stop them. From canaveral's side that is the same event as
// Ctrl-C — the person who asked for this feature is no longer waiting for it
// — and Result.abort already limits itself to units this run started, so
// adopting a healthy pre-existing service is not at risk.
//
// Unless SIGHUP was already ignored, which is a deliberate instruction not to
// die with the terminal. `nohup canaveral new ...` is a reasonable thing to
// do for a launch that takes half an hour, and signal.Notify would override
// that inherited disposition and break it. Asking first is what keeps both
// behaviours: hang up when nobody asked otherwise, survive when they did.
func StopSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}
