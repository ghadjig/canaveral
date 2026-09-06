package cli

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
)

func has(sigs []os.Signal, want os.Signal) bool {
	for _, s := range sigs {
		if s == want {
			return true
		}
	}
	return false
}

func TestStopSignalsAlwaysCoversInterruptAndTerm(t *testing.T) {
	sigs := StopSignals()
	if !has(sigs, os.Interrupt) {
		t.Error("SIGINT missing: Ctrl-C would no longer tear down what a run had started")
	}
	if !has(sigs, syscall.SIGTERM) {
		t.Error("SIGTERM missing")
	}
}

// Closing the terminal is the case this exists for.
func TestStopSignalsCatchesHangupByDefault(t *testing.T) {
	if !has(StopSignals(), syscall.SIGHUP) {
		t.Error("SIGHUP missing: a launch whose terminal closes leaves its services running")
	}
}

// `nohup canaveral new ...` is a deliberate instruction to outlive the
// terminal, and signal.Notify would quietly overrule it.
func TestStopSignalsLeavesAnIgnoredHangupAlone(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	// Reset rather than Ignore: the disposition inherited by `go test` is the
	// default one, and leaving SIGHUP ignored would change how every test
	// after this one behaves.
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })

	if has(StopSignals(), syscall.SIGHUP) {
		t.Error("SIGHUP claimed even though it was ignored on entry; nohup stops working")
	}
}
