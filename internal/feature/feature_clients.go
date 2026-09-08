package feature

// Agent clients that outlive the agent they were talking to: an `opencode
// attach` running in a terminal canaveral did not open, still pointed at a
// server that has since been stopped.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// clientGrace is how long a client is given to exit on its own before it is
// killed outright. A TUI has a terminal to hand back and wants a moment for
// it; it does not want long, because the server it would otherwise flush to
// is already gone.
const clientGrace = 2 * time.Second

// closeAgentClients terminates any process still attached to one of f's
// agents, and reports how many it took down.
//
// A client canaveral spawned goes down with its window. One started by hand —
// `canaveral attach` in a terminal of the user's own — does not. There is no
// record of it to close: closeFeatureWindows walks the windows canaveral
// opened, and rehomeStrays deliberately moves a user's own window aside
// rather than closing it. What survives is a TUI talking to a port nothing is
// listening on any more, which is not a state these exit on.
//
// Measured on the one that prompted this: abandoned for a day and a half
// against a worktree that had already been deleted, holding 250MB and a third
// of a core, having burned through twelve hours of CPU time reconnecting to a
// socket that was never coming back.
//
// Matched on the agent's URL, which carries the feature's own randomly
// allocated port and so cannot name anyone else's client.
func closeAgentClients(ctx context.Context, f *state.Feature, r Reporter) {
	var urls []string
	for _, a := range f.Agents {
		if a.URL != "" {
			urls = append(urls, a.URL)
		}
	}
	if len(urls) == 0 {
		return
	}

	pids := findClients(urls, ancestry(os.Getpid()))
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	// Only survivors are waited on, and only once: everything here is already
	// orphaned, so there is nothing to be gained by being patient with it.
	if alive := waitGone(ctx, pids, clientGrace); len(alive) > 0 {
		for _, pid := range alive {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	r.OK("closed %d stray agent client(s)", len(pids))
}

// findClients returns the pids of processes whose arguments name one of the
// given URLs, skipping any pid in skip.
func findClients(urls []string, skip map[int]bool) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue
		}
		// A process that exits mid-scan simply is not a client any more.
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.ReplaceAll(string(b), "\x00", " ")
		for _, u := range urls {
			if strings.Contains(argv, u) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// waitGone polls until every pid has exited or the timeout expires, and
// returns those still running.
//
// A pid that has exited but not yet been reaped by its parent is still
// signallable and so still counts as running here. That costs nothing: a
// zombie holds no memory and burns no CPU, and the SIGKILL it earns by
// outlasting the timeout is delivered to nothing.
func waitGone(ctx context.Context, pids []int, timeout time.Duration) []int {
	end := time.Now().Add(timeout)
	for {
		var alive []int
		for _, pid := range pids {
			// Signal 0 delivers nothing and only asks whether the pid is
			// still signallable.
			if syscall.Kill(pid, 0) == nil {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || !time.Now().Before(end) {
			return alive
		}
		select {
		case <-ctx.Done():
			return alive
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ancestry returns pid and every process above it.
//
// A teardown is very often run from inside one of the feature's own windows —
// typed into its terminal, or called as a shell tool by its own agent — and
// the attach client hosting that terminal matches the URL like any other.
// Killing it here would take the running teardown down with it, halfway.
// closeFeatureWindows already handles that window, and handles it correctly
// by leaving its own until everything durable has been written.
func ancestry(pid int) map[int]bool {
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		pid = parentOf(pid)
	}
	return seen
}

// parentOf reads a process's parent from /proc, or returns 0.
func parentOf(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// The second field is the executable name in parentheses and may itself
	// contain spaces and parentheses, so fields are counted from the last
	// closing one rather than from the start.
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(b)[i+1:])
	// After the name come state and ppid.
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}
