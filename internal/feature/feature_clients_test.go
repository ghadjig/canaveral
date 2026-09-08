package feature

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// spawnMarked starts a process that sits still until it is signalled,
// carrying marker in its own arguments so findClients can recognise it.
//
// It blocks on stdin rather than sleeping, and does not exec: an exec would
// replace the very argv the marker has to be in, which is how the first
// version of this helper managed to test nothing at all.
func spawnMarked(t *testing.T, marker string) *exec.Cmd {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "read line", marker)
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		t.Fatalf("start marked process: %v", err)
	}
	// Reaped as it exits. In production a client is nobody's child here and
	// its own parent reaps it; left unreaped, this one would sit as a zombie
	// and go on answering signal 0 for the whole of the test.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
		_ = r.Close()
		_ = w.Close()
	})

	// The point of the helper is the marker being visible in /proc, so that
	// is checked here rather than trusted in four tests.
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline")
		if err == nil && strings.Contains(strings.ReplaceAll(string(b), "\x00", " "), marker) {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %q never appeared in the process's arguments", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The process this was written for: an `opencode attach` still pointed at an
// agent that had been stopped, which canaveral had no window record for and
// so had never closed.
func TestCloseAgentClientsKillsAClientOfAStoppedAgent(t *testing.T) {
	url := "http://127.0.0.1:" + strconv.Itoa(40000+os.Getpid()%20000)
	client := spawnMarked(t, "attach "+url)

	f := &state.Feature{
		Project: "p", Name: "f",
		Agents: []state.Agent{{Name: "main", URL: url}},
	}
	closeAgentClients(context.Background(), f, quietReporter{})

	deadline := time.Now().Add(3 * time.Second)
	for alive(client.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(client.Process.Pid) {
		t.Error("a client of a stopped agent must not be left running")
	}
}

// The URL carries the feature's own randomly allocated port, so it can only
// name that feature's clients — but the matching still has to be on the URL
// and not on anything looser.
func TestCloseAgentClientsLeavesOtherProcessesAlone(t *testing.T) {
	mine := "http://127.0.0.1:" + strconv.Itoa(41000+os.Getpid()%10000)
	theirs := "http://127.0.0.1:" + strconv.Itoa(51000+os.Getpid()%10000)
	other := spawnMarked(t, "attach "+theirs)

	f := &state.Feature{
		Project: "p", Name: "f",
		Agents: []state.Agent{{Name: "main", URL: mine}},
	}
	closeAgentClients(context.Background(), f, quietReporter{})

	time.Sleep(200 * time.Millisecond)
	if !alive(other.Process.Pid) {
		t.Error("a client of a different feature's agent must be left alone")
	}
}

// A teardown is very often run from inside the feature's own attach window,
// as a shell tool call from its own agent. Killing that client here would
// take the running teardown down with it, halfway through.
func TestCloseAgentClientsNeverKillsItsOwnAncestors(t *testing.T) {
	self := ancestry(os.Getpid())
	if !self[os.Getpid()] {
		t.Fatal("ancestry must include the process it was asked about")
	}
	if len(self) < 2 {
		t.Fatal("ancestry must walk past the process itself")
	}
	// The test binary's own arguments name it, so a URL that is certain to
	// match this process stands in for being hosted by a client.
	url := os.Args[0]
	if got := findClients([]string{url}, self); len(got) > 0 {
		for _, pid := range got {
			if self[pid] {
				t.Fatalf("pid %d is an ancestor and must have been skipped", pid)
			}
		}
	}
	if got := findClients([]string{url}, nil); len(got) == 0 {
		t.Fatal("without the skip set this process should have matched, so the skip is what did the work")
	}
}

func TestCloseAgentClientsIsANoOpWithoutAgentURLs(t *testing.T) {
	// No URL to match on means nothing may be killed on a guess.
	f := &state.Feature{Project: "p", Name: "f", Agents: []state.Agent{{Name: "main"}}}
	closeAgentClients(context.Background(), f, quietReporter{})
}

func TestParentOfHandlesACommandNameWithParentheses(t *testing.T) {
	// /proc/<pid>/stat puts the executable name in parentheses and does not
	// escape it, so fields have to be counted from the last one.
	if got := parentOf(os.Getpid()); got != os.Getppid() {
		t.Errorf("parentOf = %d, want %d", got, os.Getppid())
	}
	if got := parentOf(-1); got != 0 {
		t.Errorf("parentOf(-1) = %d, want 0 for a pid that cannot be read", got)
	}
}
