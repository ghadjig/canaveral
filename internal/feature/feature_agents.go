package feature

// Agent reconciliation: starting or adopting the agents a manifest declares,
// and carrying a namespace sibling's conversation into a freshly created
// feature.
//
// Not every agent is a process canaveral starts. A serving harness
// (opencode) gets a systemd unit of its own, announces a URL and outlives
// the windows attached to it; a non-serving one (Claude Code) is the
// terminal program itself, so there is nothing here to launch and the record
// exists purely so that status, watch and session continuity know the agent
// is meant to be there. agent.Harness.Serves is the bit that decides which.

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/unit"
)

// agentConn describes a recorded agent to its harness: the server URL for
// the harnesses that have one, and the directory the conversation is rooted
// at for the ones that key sessions by working directory.
func agentConn(f *state.Feature, a state.Agent) agent.Conn {
	dir := a.Dir
	if dir == "" {
		dir = f.Worktree
	}
	return agent.Conn{URL: a.URL, Dir: dir, Password: a.Password}
}

// forkedSessionFor returns the ID of a fresh fork of a namespace sibling's
// conversation, for the window's command to open, or "" when there's nothing
// to fork from — no namespace, no sibling, none with a session on record
// yet, or a harness that cannot copy a conversation at all.
//
// Checks two sources and takes whichever is newer: skills.LatestSession,
// which survives a sibling being removed (recorded in Remove, below), and
// every currently-existing sibling's live agent, for the case where it's
// still running and has moved on since that was last recorded.
//
// Only a session written by the same tool is a candidate: the id is
// meaningless to any other harness, and a newer one from a tool the
// namespace has since moved away from would otherwise shadow an older one
// that could actually be resumed.
//
// Only ever consulted when the feature itself was just created with
// Options.Continue (varsFor's fork argument) — once a feature has its own session building up,
// re-forking on every later reset would silently discard it in favour of a
// sibling's, possibly stale, history.
func forkedSessionFor(ctx context.Context, f *state.Feature, a state.Agent) string {
	ns := Namespace(f.Name)
	if ns == "" || !agent.Probeable(a.Tool, a.URL) {
		return ""
	}

	best, have := recordedSiblingSession(f.Project, ns, a.Name, a.Tool)
	best, have = newestLiveSiblingSession(ctx, f.Project, ns, f.Name, a.Name, a.Tool, best, have)
	if !have {
		return ""
	}

	// Fork here rather than letting the agent's own attach do it, so the
	// copy's ID is known and it can be re-homed into this feature's
	// worktree. Without that the copy keeps the source's directory: it
	// would operate in another feature's checkout, or in one that has since
	// been deleted, and would be invisible to anything scoping sessions by
	// directory (canaveral status included).
	//
	// The Conn deliberately mixes the two: our own URL, because an opencode
	// server can fork any session it can see and this feature's is the one
	// we have, and the *source's* worktree, because a harness with no server
	// finds the conversation by the directory it was held in.
	c := agent.Conn{URL: a.URL, Dir: best.Worktree, Password: a.Password}
	forked, err := agent.Fork(ctx, a.Tool, c, best.SessionID, f.Worktree)
	if err != nil {
		// Continuity is a convenience; a fresh session is a fine outcome.
		return ""
	}
	return forked
}

// briefSiblingMax bounds how many siblings a brief lists, newest first. It is
// sent with every start of the agent, and a namespace that has been in use
// for months would otherwise list every feature it ever had.
const briefSiblingMax = 8

// namespaceBrief is what an agent in f is told about its namespace: that its
// siblings exist, that the namespace's skill holds their shared notes, and
// where each sibling's conversation can be read should its history help. ""
// for a feature outside any namespace.
//
// This, not a copy of a sibling's conversation, is the default way history
// crosses between features: the agent reads what it needs, when it needs
// it, and its own session is unmistakably its own.
func namespaceBrief(f *state.Feature) string {
	ns := Namespace(f.Name)
	if ns == "" {
		return ""
	}
	type sibling struct {
		name, branch string
		at           time.Time
		hints        []string
	}
	var sibs []sibling
	seen := map[string]bool{f.Name: true}
	names, _ := state.List(f.Project)
	for _, name := range names {
		if seen[name] || Namespace(name) != ns {
			continue
		}
		sf, err := state.Load(f.Project, name)
		if err != nil {
			continue
		}
		seen[name] = true
		sib := sibling{name: name, branch: sf.Branch, at: sf.CreatedAt}
		for _, a := range sf.Agents {
			if h := agent.HistoryHint(a.Tool, agentConn(sf, a).Dir); h != "" {
				sib.hints = append(sib.hints, h)
			}
		}
		// An agent can be run in a worktree without canaveral having a
		// record of it, by hand or from a feature created --no-agents.
		// Claude Code's history is found by directory alone, so look there.
		if len(sib.hints) == 0 {
			if h := agent.HistoryHint("claude", sf.Worktree); h != "" {
				sib.hints = append(sib.hints, h)
			}
		}
		sibs = append(sibs, sib)
	}
	// A removed sibling leaves its last session on record, and its
	// transcripts on disk.
	if rec, ok, err := skills.LatestSession(f.Project, ns, "main"); err == nil && ok && !seen[rec.Feature] {
		if h := agent.HistoryHint(rec.Harness(), rec.Worktree); h != "" {
			sibs = append(sibs, sibling{name: rec.Feature + " (removed)", at: rec.UpdatedAt, hints: []string{h}})
		}
	}
	sort.Slice(sibs, func(i, j int) bool { return sibs[i].at.After(sibs[j].at) })
	if len(sibs) > briefSiblingMax {
		sibs = sibs[:briefSiblingMax]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are working in the canaveral feature %q", f.Name)
	if f.Branch != "" {
		fmt.Fprintf(&b, " (branch %s)", f.Branch)
	}
	fmt.Fprintf(&b, ", one of the features in the %q namespace. Each is its own piece of related work, with its own worktree, branch and conversation; this conversation is this feature's own.\n\n", ns)
	fmt.Fprintf(&b, "The %s skill holds notes shared between the namespace's features. Read it before starting, and add to it what the next feature should know.", skills.FlattenName(ns))
	if len(sibs) > 0 {
		b.WriteString("\n\nOther features in the namespace, newest first. Their conversations are there to read when their history would help; they are large, so search them rather than reading them whole.")
		for _, s := range sibs {
			fmt.Fprintf(&b, "\n- %s", s.name)
			if s.branch != "" && s.branch != s.name {
				fmt.Fprintf(&b, " (branch %s)", s.branch)
			}
			for _, h := range s.hints {
				fmt.Fprintf(&b, ": %s", h)
			}
		}
	}
	return b.String()
}

// recordedSiblingSession returns the namespace's recorded newest session for
// agentName, if any and if tool wrote it — the fallback used when no sibling
// feature is live to ask directly.
func recordedSiblingSession(project, ns, agentName, tool string) (skills.SessionRecord, bool) {
	rec, ok, err := skills.LatestSession(project, ns, agentName)
	if err != nil || !ok || rec.Harness() != tool {
		return skills.SessionRecord{}, false
	}
	return rec, true
}

// newestLiveSiblingSession scans every other feature in ns for a reachable
// agentName running tool, returning the most recently updated session found — best/have
// if none of them beat it, or if there are none to check.
func newestLiveSiblingSession(ctx context.Context, project, ns, name, agentName, tool string,
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
		if !ok || rec.Harness() != tool {
			continue
		}
		if !have || rec.UpdatedAt.After(best.UpdatedAt) {
			best, have = rec, true
		}
	}
	return best, have
}

// siblingSession probes a single sibling feature's agent, returning its
// newest session if the agent can be asked and has one.
func siblingSession(ctx context.Context, project, sib, agentName string) (skills.SessionRecord, bool) {
	sf, err := state.Load(project, sib)
	if err != nil {
		return skills.SessionRecord{}, false
	}
	a, ok := sf.Agent(agentName)
	if !ok || !agent.Probeable(a.Tool, a.URL) {
		return skills.SessionRecord{}, false
	}
	h := agent.Probe(ctx, a.Tool, agentConn(sf, *a))
	if !h.Reachable || h.SessionID == "" {
		return skills.SessionRecord{}, false
	}
	return skills.SessionRecord{
		Feature: sib, SessionID: h.SessionID, Tool: a.Tool,
		Worktree: sf.Worktree, UpdatedAt: h.Updated,
	}, true
}

func reconcileAgents(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter, prog *progress) error {

	if len(m.Agents) == 0 {
		return nil
	}
	// Resolve every harness's binary up front, once per tool: a missing
	// toolchain becomes an immediate, clear error rather than a unit that
	// starts and dies, or a window that vanishes the instant it spawns.
	bins := map[string]string{}
	for _, a := range m.Agents {
		if _, done := bins[a.Tool]; done {
			continue
		}
		h, err := agent.For(a.Tool)
		if err != nil {
			return fmt.Errorf("agent %q: %w", a.Name, err)
		}
		bin, err := h.Resolve()
		if err != nil {
			return err
		}
		bins[a.Tool] = bin
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
		dir := serviceDir(f, m, a.Dir)
		h, _ := agent.For(a.Tool) // already validated above

		if !h.Serves() {
			// Nothing to start: the agent is whatever the window runs. The
			// record still matters — status, watch and session continuity
			// all key off it — so it is written with no unit and no URL.
			records = append(records, state.Agent{Name: a.Name, Tool: a.Tool, Dir: dir})
			r.OK("agent %s (%s, started by its window)", a.Name, a.Tool)
			prog.done()
			continue
		}

		unitName := unit.Name(f.Project+"-"+f.Name, "agent", a.Name)
		logPath := filepath.Join(logDir, "agent-"+a.Name+".log")

		if st, err := unit.Query(ctx, unitName); err == nil && st.Running() {
			rec := adoptRunningAgent(ctx, h, f, a, unitName, dir, logPath)
			records = append(records, rec)
			prog.done()
			continue
		}

		rec, err := startAgent(ctx, h, f, a, bins[a.Tool], base, vars, unitName, dir, logPath, res, r)
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
func adoptRunningAgent(ctx context.Context, h agent.Harness, f *state.Feature, a manifest.Agent,
	unitName, dir, logPath string) state.Agent {
	rec := state.Agent{Name: a.Name, Tool: a.Tool, Unit: unitName, Dir: dir, LogPath: logPath}
	if prev, ok := f.Agent(a.Name); ok {
		rec.URL, rec.Port, rec.Password = prev.URL, prev.Port, prev.Password
	}
	if rec.URL == "" || rec.Password == "" {
		// The unit is alive but we lost how to reach it; recover that from
		// the log. A record from before agents had passwords lands here
		// too, with a URL but nothing to authenticate with.
		if ep, err := h.Discover(ctx, logPath, 5*time.Second, nil); err == nil {
			rec.URL, rec.Port, rec.Password = ep.URL, portOf(ep.URL), ep.Password
		}
	}
	return rec
}

// startAgent renders a's environment, starts its unit, and waits for it to
// announce its listen URL, tearing the unit back down if it never does. An
// agent that never announces a URL is unusable and, unlike a service,
// nothing can adopt it later — the URL is only ever printed once, at
// startup — so leaving it running would strand a server that no window can
// attach to.
func startAgent(ctx context.Context, h agent.Harness, f *state.Feature, a manifest.Agent,
	bin string, base map[string]string, vars tmpl.Vars, unitName, dir, logPath string,
	res *Result, r Reporter) (state.Agent, error) {
	rec := state.Agent{Name: a.Name, Tool: a.Tool, Unit: unitName, Dir: dir, LogPath: logPath}

	agentEnv, err := tmpl.RenderMap("agent."+a.Name+".env", a.Env, vars)
	if err != nil {
		return rec, err
	}
	env := manifest.MergeEnv(base, agentEnv)
	env = manifest.MergeEnv(env, h.EnvFor(agent.Selection{Model: a.Model, Agent: a.Agent}))

	unit.Reset(ctx, unitName)
	r.Step("agent %s", a.Name)
	res.launched = append(res.launched, unitName)
	if err := unit.Start(ctx, unit.Spec{
		Name:        unitName,
		Description: fmt.Sprintf("canaveral %s/%s agent %s", f.Project, f.Name, a.Name),
		Dir:         dir,
		Cmd:         h.ServeCmd(bin),
		Env:         env,
		LogPath:     logPath,
	}); err != nil {
		return rec, err
	}
	ep, err := h.Discover(ctx, logPath, 45*time.Second, aliveCheck(ctx, unitName, logPath))
	if err != nil {
		_ = unit.Stop(ctx, unitName)
		unit.Reset(ctx, unitName)
		return rec, fmt.Errorf("agent %q: %w", a.Name, err)
	}
	rec.URL, rec.Port, rec.Password = ep.URL, portOf(ep.URL), ep.Password
	r.OK("agent %s listening on %s", a.Name, ep.URL)
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
