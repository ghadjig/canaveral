package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Conn is everything a harness needs to find the agent it is being asked
// about: the URL of its server, for the harnesses that have one, and the
// directory the conversation is rooted at, for the ones that key sessions by
// working directory instead. Both are recorded per agent in state.Agent.
type Conn struct {
	// URL is the agent server's base URL, empty for a harness that has no
	// server (Harness.Serves reports false).
	URL string
	// Dir is the directory the agent works in — the feature's worktree,
	// unless the manifest's `dir` says otherwise.
	Dir string
}

// Selection is the manifest's per-agent choice of model and sub-agent
// persona, which each harness expresses in its own environment variables.
type Selection struct {
	// Model is the manifest's `model`, e.g. "anthropic/claude-opus-4-5".
	Model string
	// Agent is the manifest's `agent`: the named persona or mode to start
	// in, e.g. opencode's "build" or "plan".
	Agent string
}

// ErrNoEvents is returned by Harness.Watch for a harness that has no push
// notifications. It is not a failure: callers fall back to polling, which
// they do anyway as a safety net.
var ErrNoEvents = errors.New("harness has no event stream")

// Harness is one coding agent canaveral knows how to run.
//
// The interface is deliberately shaped around the two things that actually
// differ between tools: how the agent gets started, and how canaveral finds
// out what it is doing. Everything downstream — status rows, the watch
// stream, the widgets — speaks only Health.
type Harness interface {
	// Name is the value a manifest's `tool =` uses to ask for this harness.
	Name() string

	// Resolve returns the absolute path to the harness's executable, or an
	// error naming what is missing.
	Resolve() (string, error)

	// Serves reports whether the agent runs as a long-lived server that
	// canaveral supervises in its own systemd unit.
	//
	// A serving harness (opencode) is started by canaveral, announces a URL,
	// and outlives the windows that attach to it; a non-serving one (Claude
	// Code) *is* the terminal program, so canaveral starts nothing, records
	// no unit and no URL, and learns what it is doing from whatever the tool
	// leaves on disk. Almost every difference in handling follows from this
	// one bit.
	Serves() bool

	// ServeCmd is the shell command that starts the server. Only meaningful
	// when Serves reports true.
	ServeCmd(bin string) string

	// DiscoverURL waits for the server to announce its listen address in
	// logPath. Only meaningful when Serves reports true.
	DiscoverURL(ctx context.Context, logPath string, timeout time.Duration, alive func() error) (string, error)

	// EnvFor maps a manifest's model and agent selection onto the harness's
	// own environment variables.
	EnvFor(sel Selection) map[string]string

	// Probe reports what the agent is doing right now.
	Probe(ctx context.Context, c Conn) Health

	// Fork copies sessionID into dir and returns the copy's ID, so a new
	// feature can start from a namespace sibling's conversation instead of
	// from nothing. Callers treat any error as "no continuity available":
	// a fresh session is always an acceptable outcome.
	Fork(ctx context.Context, c Conn, sessionID, dir string) (string, error)

	// AttachArgv is the argv (argv[0] included) for a terminal that opens
	// this agent. cont asks for its last conversation rather than a new
	// one, which is what `canaveral attach --continue` means.
	//
	// The caller runs it with the working directory set to c.Dir, so a
	// harness that keys sessions by directory needs no flag for it.
	AttachArgv(c Conn, cont bool) []string

	// SessionFlag renders the flags that make AttachArgv's command reopen
	// session id, for splicing into a manifest window's own command via
	// {{.Agent.<name>.Session}}. Empty id yields an empty string.
	SessionFlag(id string) string

	// Watch calls fn whenever something happens that could change the
	// agent's Health, and blocks until ctx is done. Returns ErrNoEvents for
	// a harness with no such stream.
	Watch(ctx context.Context, c Conn, fn func()) error
}

// registry holds every harness canaveral can run, keyed by manifest name.
//
// A map rather than a switch so that adding a tool is one file plus one line
// here, and so that manifest validation, shell completion and the error
// message for an unknown tool all enumerate the same list — three places
// that would otherwise drift apart the first time a fourth harness landed.
var registry = map[string]Harness{
	opencode{}.Name(): opencode{},
	claude{}.Name():   claude{},
}

// For returns the harness named tool.
func For(tool string) (Harness, error) {
	h, ok := registry[tool]
	if !ok {
		return nil, fmt.Errorf("unsupported agent tool %q (known: %v)", tool, Tools())
	}
	return h, nil
}

// Known reports whether tool names a harness canaveral can run.
func Known(tool string) bool {
	_, ok := registry[tool]
	return ok
}

// Tools lists every known harness name, sorted.
func Tools() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Probeable reports whether an agent recorded with this tool and URL has
// enough on file to be asked anything.
//
// A serving harness needs the URL it announced at startup, so an agent whose
// server never came up cannot be probed; a non-serving one needs only its
// directory, which is always recorded.
func Probeable(tool, url string) bool {
	h, err := For(tool)
	if err != nil {
		return false
	}
	return !h.Serves() || url != ""
}

// Probe reports what the agent described by tool and c is doing, looking the
// harness up on the caller's behalf.
//
// An unknown tool comes back as an unreachable agent carrying the error
// rather than as a hard failure: a status view that omits one row is far
// better than one that refuses to draw, and the manifest layer has already
// rejected the tool by the time anything can be started with it — the only
// way to get here is a state file written by a newer canaveral.
func Probe(ctx context.Context, tool string, c Conn) Health {
	h, err := For(tool)
	if err != nil {
		return Health{Err: err}
	}
	return h.Probe(ctx, c)
}

// Fork copies a session into dir using tool's harness. See Harness.Fork.
func Fork(ctx context.Context, tool string, c Conn, sessionID, dir string) (string, error) {
	h, err := For(tool)
	if err != nil {
		return "", err
	}
	return h.Fork(ctx, c, sessionID, dir)
}
