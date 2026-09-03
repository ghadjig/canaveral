package feature

// Agent reconciliation: starting/adopting opencode agent units, and
// forking a namespace sibling's session into a freshly created one.

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/unit"
)

// forkArgsFor returns "--session <id> --fork" for an opencode agent when a
// namespace sibling has a more recently active session to hand off, or ""
// when there's nothing to fork from — no namespace, no sibling, or none has
// an opencode session on record yet.
//
// Checks two sources and takes whichever is newer: skills.LatestSession,
// which survives a sibling being removed (recorded in Remove, below), and
// every currently-existing sibling's live agent server, for the case where
// it's still running and has moved on since that was last recorded.
//
// Only ever consulted when the feature itself was just created (varsFor's
// fresh argument) — once a feature has its own session building up,
// re-forking on every later reset would silently discard it in favour of a
// sibling's, possibly stale, history.
func forkArgsFor(ctx context.Context, project, name, agentName, baseURL, worktree string) string {
	ns := Namespace(name)
	if ns == "" {
		return ""
	}

	best, have := recordedSiblingSession(project, ns, agentName)
	best, have = newestLiveSiblingSession(ctx, project, ns, name, agentName, best, have)
	if !have {
		return ""
	}

	// Fork here rather than letting `opencode attach --fork` do it, so the
	// copy's ID is known and it can be re-homed into this feature's
	// worktree. Without that the copy keeps the source's directory: it
	// would operate in another feature's checkout, or in one that has since
	// been deleted, and would be invisible to anything scoping sessions by
	// directory (canaveral status included).
	forked, err := agent.ForkInto(ctx, baseURL, best.SessionID, worktree)
	if err != nil {
		// Continuity is a convenience; a fresh session is a fine outcome.
		return ""
	}
	return fmt.Sprintf("--session %s", forked)
}

// recordedSiblingSession returns the namespace's recorded newest session for
// agentName, if any — the fallback used when no sibling feature is live to
// ask directly.
func recordedSiblingSession(project, ns, agentName string) (skills.SessionRecord, bool) {
	rec, ok, err := skills.LatestSession(project, ns, agentName)
	if err != nil || !ok {
		return skills.SessionRecord{}, false
	}
	return rec, true
}

// newestLiveSiblingSession scans every other feature in ns for a reachable
// agentName, returning the most recently updated session found — best/have
// if none of them beat it, or if there are none to check.
func newestLiveSiblingSession(ctx context.Context, project, ns, name, agentName string,
	best skills.SessionRecord, have bool) (skills.SessionRecord, bool) {
	siblings, err := state.List(project)
	if err != nil {
		return best, have
	}
	for _, sib := range siblings {
		if sib == name || Namespace(sib) != ns {
			continue
		}
		rec, ok := siblingSession(ctx, project, sib, agentName)
		if !ok {
			continue
		}
		if !have || rec.UpdatedAt.After(best.UpdatedAt) {
			best, have = rec, true
		}
	}
	return best, have
}

// siblingSession probes a single sibling feature's agent, returning its
// newest session if the agent is reachable and has one.
func siblingSession(ctx context.Context, project, sib, agentName string) (skills.SessionRecord, bool) {
	sf, err := state.Load(project, sib)
	if err != nil {
		return skills.SessionRecord{}, false
	}
	a, ok := sf.Agent(agentName)
	if !ok || a.URL == "" {
		return skills.SessionRecord{}, false
	}
	h := agent.Probe(ctx, a.URL, sf.Worktree)
	if !h.Reachable || h.SessionID == "" {
		return skills.SessionRecord{}, false
	}
	return skills.SessionRecord{
		Feature: sib, SessionID: h.SessionID,
		Worktree: sf.Worktree, UpdatedAt: h.Updated,
	}, true
}

func reconcileAgents(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter, prog *progress) error {

	if len(m.Agents) == 0 {
		return nil
	}
	bin, err := agent.Resolve()
	if err != nil {
		return err
	}
	base, err := envFor(m, f, tc, vars)
	if err != nil {
		return err
	}
	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		return err
	}

	var records []state.Agent
	for _, a := range m.Agents {
		prog.start("agent " + a.Name)
		unitName := unit.Name(f.Project+"-"+f.Name, "agent", a.Name)
		logPath := filepath.Join(logDir, "agent-"+a.Name+".log")
		dir := serviceDir(f, m, a.Dir)

		if st, err := unit.Query(ctx, unitName); err == nil && st.Running() {
			rec := adoptRunningAgent(ctx, f, a, unitName, dir, logPath)
			records = append(records, rec)
			prog.done()
			continue
		}

		rec, err := startAgent(ctx, m, f, a, bin, base, vars, unitName, dir, logPath, res, r)
		if err != nil {
			return err
		}
		records = append(records, rec)

		f.Agents = records
		if err := state.Save(f); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		prog.done()
	}
	f.Agents = records
	return nil
}

// adoptRunningAgent recovers connection info for an agent whose unit is
// already running: from the feature's previous state if we already knew it,
// otherwise from its log (the unit is alive but the URL was lost, e.g.
// after canaveral itself restarted).
func adoptRunningAgent(ctx context.Context, f *state.Feature, a manifest.Agent, unitName, dir, logPath string) state.Agent {
	rec := state.Agent{Name: a.Name, Tool: a.Tool, Unit: unitName, Dir: dir, LogPath: logPath}
	if prev, ok := f.Agent(a.Name); ok {
		rec.URL, rec.Port = prev.URL, prev.Port
	}
	if rec.URL == "" {
		// The unit is alive but we lost its URL; recover it from the log.
		if u, err := agent.DiscoverURL(ctx, logPath, 5*time.Second, nil); err == nil {
			rec.URL, rec.Port = u, portOf(u)
		}
	}
	return rec
}

// startAgent renders a's environment, starts its unit, and waits for it to
// announce its listen URL, tearing the unit back down if it never does. An
// agent that never announces a URL is unusable and, unlike a service,
// nothing can adopt it later — the URL is only ever printed once, at
// startup — so leaving it running would strand an opencode server that no
// window can attach to.
func startAgent(ctx context.Context, m *manifest.Manifest, f *state.Feature, a manifest.Agent,
	bin string, base map[string]string, vars tmpl.Vars, unitName, dir, logPath string,
	res *Result, r Reporter) (state.Agent, error) {
	rec := state.Agent{Name: a.Name, Tool: a.Tool, Unit: unitName, Dir: dir, LogPath: logPath}

	agentEnv, err := tmpl.RenderMap("agent."+a.Name+".env", a.Env, vars)
	if err != nil {
		return rec, err
	}
	env := manifest.MergeEnv(base, agentEnv)
	if a.Model != "" {
		env["OPENCODE_MODEL"] = a.Model
	}
	if a.Agent != "" {
		env["OPENCODE_AGENT"] = a.Agent
	}

	unit.Reset(ctx, unitName)
	r.Step("agent %s", a.Name)
	res.launched = append(res.launched, unitName)
	if err := unit.Start(ctx, unit.Spec{
		Name:        unitName,
		Description: fmt.Sprintf("canaveral %s/%s agent %s", f.Project, f.Name, a.Name),
		Dir:         dir,
		Cmd:         agent.ServeCmd(bin),
		Env:         env,
		LogPath:     logPath,
	}); err != nil {
		return rec, err
	}
	url, err := agent.DiscoverURL(ctx, logPath, 45*time.Second, aliveCheck(ctx, unitName, logPath))
	if err != nil {
		_ = unit.Stop(ctx, unitName)
		unit.Reset(ctx, unitName)
		return rec, fmt.Errorf("agent %q: %w", a.Name, err)
	}
	rec.URL, rec.Port = url, portOf(url)
	r.OK("agent %s listening on %s", a.Name, url)
	res.StartedAgent = append(res.StartedAgent, a.Name)
	return rec, nil
}

var portRe = regexp.MustCompile(`:(\d+)/?$`)

func portOf(url string) int {
	mm := portRe.FindStringSubmatch(strings.TrimSuffix(url, "/"))
	if mm == nil {
		return 0
	}
	var p int
	fmt.Sscanf(mm[1], "%d", &p)
	return p
}
