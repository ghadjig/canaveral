// Package discover reads back the ports a service chose for itself.
//
// Most services are told which port to listen on, and canaveral's [ports]
// allocation is enough. Some pick their own and only announce it afterwards:
// a dev server asked to bind :0, a tunnel handed a random public port, a
// remote-development wrapper that derives its port forwards from a hash of
// the branch name. For those, the port cannot be known before the process
// runs, so it is read out of what the process itself reports.
//
// Discovery runs after the service starts and before its readiness probe, so
// a probe may refer to a port discovered from that same service.
package discover

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
)

// DefaultTimeout applies when the manifest does not set one. Deliberately
// shorter than a readiness probe's: what is being waited for is a line of
// output, not a booted application, and a service that has not said which
// port it is using within a minute is not going to.
const DefaultTimeout = 60 * time.Second

const interval = 250 * time.Millisecond

// ErrTimeout reports that discovery did not resolve every name before its
// deadline. The message names the ones still missing, which is the whole
// diagnostic: a pattern that matched nothing is nearly always a pattern that
// no longer matches the output it was written against.
var ErrTimeout = errors.New("port discovery timed out")

// Ports resolves every name declared in d, polling until they are all known,
// the timeout expires, or alive reports the process has died.
//
// logPath is the service's log; dir is the working directory for d.Cmd. alive
// may be nil. The returned map is complete or the error is non-nil — a partial
// result is never returned, because a caller that rendered half its ports
// would produce a URL pointing somewhere arbitrary rather than failing.
func Ports(ctx context.Context, d manifest.Discover, logPath, dir string, alive func() error) (map[string]int, error) {
	if !d.Enabled() {
		return nil, nil
	}

	patterns, err := compile(d.Port)
	if err != nil {
		return nil, err
	}

	timeout := d.Timeout.Or(DefaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	found := make(map[string]int, len(d.Port))
	var lastErr error
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if alive != nil {
			if err := alive(); err != nil {
				return nil, err
			}
		}
		if err := attempt(ctx, d, patterns, logPath, dir, found); err != nil {
			// Keep the first real explanation. Past the deadline every
			// attempt fails with the context's error instead, which says
			// nothing about why the port was never announced.
			if lastErr == nil || ctx.Err() == nil {
				lastErr = err
			}
		} else {
			return found, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w after %s: %w", ErrTimeout, timeout, lastErr)
		case <-ticker.C:
		}
	}
}

// attempt fills in whatever is still missing from found, returning nil once
// every declared name is resolved. Names already found are never re-read: a
// log grows, and a second match appearing later would otherwise silently move
// a port that callers have already been handed.
//
// Under discover.cmd the names are whatever the command prints rather than
// anything declared, so success is simply a clean exit reporting at least one
// port. A script that cannot answer yet is expected to fail; that is what
// keeps it being retried.
func attempt(ctx context.Context, d manifest.Discover, patterns map[string]*regexp.Regexp,
	logPath, dir string, found map[string]int) error {

	if d.Cmd != "" {
		out, err := runCmd(ctx, d.Cmd, dir)
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return fmt.Errorf("%s reported no ports", d.Cmd)
		}
		for name, p := range out {
			found[name] = p
		}
		return nil
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("read service log: %w", err)
	}
	log := string(b)
	for name, re := range patterns {
		if _, ok := found[name]; ok {
			continue
		}
		m := re.FindStringSubmatch(log)
		if m == nil {
			continue
		}
		port, err := parsePort(m[1])
		if err != nil {
			return fmt.Errorf("discover %q: %w", name, err)
		}
		found[name] = port
	}
	return missing(found, d.Names())
}

// runCmd executes d.Cmd and parses its `name=port` output.
func runCmd(ctx context.Context, cmd, dir string) (map[string]int, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseLines(string(out))
}

// parseLines reads `name=port` pairs, ignoring blank lines and # comments so
// a discovery script may explain itself.
func parseLines(s string) (map[string]int, error) {
	out := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("expected name=port, got %q", line)
		}
		port, err := parsePort(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("discover %q: %w", strings.TrimSpace(name), err)
		}
		out[strings.TrimSpace(name)] = port
	}
	return out, sc.Err()
}

// missing reports the names not yet resolved, or nil when none remain.
func missing(found map[string]int, want []string) error {
	var out []string
	for _, name := range want {
		if _, ok := found[name]; !ok {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return fmt.Errorf("no port yet for %s", strings.Join(out, ", "))
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", s)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range", p)
	}
	return p, nil
}

func compile(patterns map[string]string) (map[string]*regexp.Regexp, error) {
	out := make(map[string]*regexp.Regexp, len(patterns))
	for name, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("discover.port.%s: %w", name, err)
		}
		out[name] = re
	}
	return out, nil
}
