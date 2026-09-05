package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
	"github.com/bandito/canaveral/internal/worktree"
)

func runLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	all := fs.Bool("all", false, "list features across every project")
	names := fs.Bool("names", false, "print only feature names, one per line (for shell completion)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	var features []*state.Feature
	var stashes []*state.Stash
	var err error
	if *all {
		features, err = state.LoadAll()
		stashes, _ = state.LoadAllStashes()
	} else {
		m, mErr := loadManifest()
		if mErr != nil {
			return mErr
		}
		features, err = state.LoadProject(m.Name)
		stashes, _ = state.LoadStashes(m.Name)
	}
	if err != nil {
		return err
	}
	if *names {
		// Stashes are deliberately absent here. --names feeds shell
		// completion and scripts that act on running features, and a name
		// that has no ports, no units and no windows behind it would break
		// every one of them.
		for _, f := range features {
			fmt.Println(f.Name)
		}
		return nil
	}
	if len(features) == 0 {
		if len(stashes) == 0 {
			fmt.Println("no features yet — create one with `canaveral new <feature>`")
			return nil
		}
		fmt.Println("nothing running")
		printStashes(stashes)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Header and data cells here must stay plain: tabwriter measures raw
	// byte length, invisible ANSI escape codes and all, so coloring only
	// some cells (or a header inconsistently with its data) inflates that
	// column's computed width and pushes every later column out of
	// alignment with its header — see printStatus below for the version of
	// this bug that motivated the fix.
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		"FEATURE", "BRANCH", "PORTS", "SVC", "WINDOWS", "AGE")
	for _, f := range features {
		live := 0
		for _, s := range f.Services {
			if st, err := unit.Query(ctx, s.Unit); err == nil && st.Running() {
				live++
			}
		}
		openWindows := countOpenWindows(ctx, f)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n",
			f.Name, shorten(f.Branch, 28), portSummary(f.Ports),
			live, len(f.Services), openWindows, humanAgo(f.CreatedAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	printStashes(stashes)
	return nil
}

func countOpenWindows(ctx context.Context, f *state.Feature) string {
	if len(f.Windows) == 0 {
		return "-"
	}
	if err := hypr.Available(ctx); err != nil {
		return "?"
	}
	clients, err := hypr.Clients(ctx)
	if err != nil {
		return "?"
	}
	n := 0
	for _, w := range f.Windows {
		if windowOpen(clients, w) {
			n++
		}
	}
	return fmt.Sprintf("%d/%d", n, len(f.Windows))
}

// windowOpen reports whether a declared window currently has a client.
//
// Only the class canaveral assigned is matched: anything looser could adopt one
// of the user's own windows.
func windowOpen(clients []hypr.Client, w state.Window) bool {
	for _, c := range clients {
		if c.InitialClass == w.Class {
			return true
		}
	}
	return false
}

func portSummary(ports map[string]int) string {
	if len(ports) == 0 {
		return "-"
	}
	names := sortedPortNames(ports)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprint(ports[n]))
	}
	return strings.Join(parts, ",")
}

type rowKind string

const (
	kindService rowKind = "service"
	kindAgent   rowKind = "agent"
	kindWindow  rowKind = "window"
	// kindBranch carries a feature's git position. It is a row rather than a
	// new top-level shape so that --json stays a flat array and existing
	// consumers, which already switch on Kind, are unaffected.
	kindBranch rowKind = "branch"
)

type row struct {
	Feature string  `json:"feature"`
	Kind    rowKind `json:"kind"`
	Name    string  `json:"name"`
	// Tool is the agent harness this row's agent runs, e.g. "opencode" or
	// "claude". Set only on kindAgent rows.
	Tool        string        `json:"tool,omitempty"`
	Unit        string        `json:"unit,omitempty"`
	State       string        `json:"state"`
	MemBytes    uint64        `json:"mem_bytes,omitempty"`
	CPUNanos    time.Duration `json:"cpu_nanos,omitempty"`
	URL         string        `json:"url,omitempty"`
	Detail      string        `json:"detail,omitempty"`
	Busy        bool          `json:"busy,omitempty"`
	AgentState  string        `json:"agent_state,omitempty"`
	Idle        time.Duration `json:"idle_nanos,omitempty"`
	Worked      time.Duration `json:"worked_nanos,omitempty"`
	Working     time.Duration `json:"working_nanos,omitempty"`
	SincePrompt time.Duration `json:"since_prompt_nanos,omitempty"`
	Sessions    int           `json:"sessions,omitempty"`
	TodoTotal   int           `json:"todo_total,omitempty"`
	TodoDone    int           `json:"todo_done,omitempty"`
	TodoNow     string        `json:"todo_current,omitempty"`
	ActTool     string        `json:"activity_tool,omitempty"`
	ActTitle    string        `json:"activity_title,omitempty"`
	LastUser    string        `json:"last_user,omitempty"`
	LastAgent   string        `json:"last_assistant,omitempty"`
	PendKind    string        `json:"pending_kind,omitempty"`
	PendHeader  string        `json:"pending_header,omitempty"`
	PendDetail  string        `json:"pending_detail,omitempty"`
	PendExtra   string        `json:"pending_extra,omitempty"`
	Tokens      int64         `json:"tokens,omitempty"`
	Cost        float64       `json:"cost,omitempty"`
	Model       string        `json:"model,omitempty"`
	Variant     string        `json:"variant,omitempty"`
	Provider    string        `json:"provider,omitempty"`
	SubAgents   int           `json:"subagents,omitempty"`
	LastError   string        `json:"last_error,omitempty"`

	// Set only on kindBranch rows. Not omitempty: "+0 -0, nothing committed"
	// is a meaningful answer and must survive the round trip.
	Base         string `json:"base,omitempty"`
	Ahead        *int   `json:"ahead,omitempty"`
	Behind       *int   `json:"behind,omitempty"`
	FilesChanged *int   `json:"files_changed,omitempty"`
	Insertions   *int   `json:"insertions,omitempty"`
	Deletions    *int   `json:"deletions,omitempty"`
	Uncommitted  *int   `json:"uncommitted,omitempty"`
}

// branchRows renders each feature's git status as its own row, so --json
// carries what the human output has always printed. Before this, the JSON
// path returned one line before collectBranchStatus was even called.
func branchRows(features []*state.Feature, branches map[string]worktree.BranchStatus) []row {
	out := make([]row, 0, len(branches))
	for _, f := range features {
		bs, ok := branches[f.Name]
		if !ok {
			continue
		}
		// bs is declared fresh each iteration, so these pointers are all
		// distinct.
		out = append(out, row{
			Feature:      f.Name,
			Kind:         kindBranch,
			Name:         bs.Base,
			State:        bs.Label(),
			Base:         bs.Base,
			Ahead:        &bs.Ahead,
			Behind:       &bs.Behind,
			FilesChanged: &bs.FilesChanged,
			Insertions:   &bs.Insertions,
			Deletions:    &bs.Deletions,
			Uncommitted:  &bs.Uncommitted,
		})
	}
	return out
}

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral status [feature...] [flags]\n\nShow services, agents, windows and agent telemetry.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		asJSON = fs.Bool("json", false, "emit machine-readable JSON")
		watch  = fs.Duration("watch", 0, "refresh continuously, e.g. 2s")
		all    = fs.Bool("all", false, "every project")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	render := func() error { return renderStatus(ctx, pos, *all, *asJSON) }
	if *watch <= 0 {
		return render()
	}
	return renderLoop(ctx, *watch, render)
}

// resolveStatusTargets resolves which features `status` should report on:
// every project's if all is set, otherwise the given positional feature
// names, or the whole current project when none are given.
func resolveStatusTargets(pos []string, all bool) ([]*state.Feature, error) {
	if all {
		return state.LoadAll()
	}
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 {
		return state.LoadProject(m.Name)
	}
	var out []*state.Feature
	for _, n := range pos {
		f, err := state.Load(m.Name, feature.Slug(n))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// renderStatus resolves targets, collects their rows, and prints once, as
// JSON or as the human table depending on asJSON.
func renderStatus(ctx context.Context, pos []string, all, asJSON bool) error {
	fsList, err := resolveStatusTargets(pos, all)
	if err != nil {
		return err
	}
	if len(fsList) == 0 {
		if asJSON {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("no features yet — create one with `canaveral new <feature>`")
		return nil
	}
	rows := collect(ctx, fsList)
	branches := collectBranchStatus(ctx, fsList)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(append(rows, branchRows(fsList, branches)...))
	}
	printStatus(fsList, rows, branches)
	return nil
}

// renderLoop calls render repeatedly on interval (raised to a 500ms floor),
// clearing the screen each time, until ctx is cancelled.
func renderLoop(ctx context.Context, interval time.Duration, render func() error) error {
	if interval < 500*time.Millisecond {
		interval = 500 * time.Millisecond
	}
	for {
		fmt.Print("\033[H\033[2J")
		if err := render(); err != nil {
			return err
		}
		fmt.Printf("\n%s\n", dim("refreshing every "+interval.String()+" — ctrl-c to exit"))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// collect queries units, agents and windows concurrently; serial probing with
// HTTP timeouts would make --watch unusable.
func collect(ctx context.Context, features []*state.Feature) []row {
	// Query the window list once rather than per feature.
	var clients []hypr.Client
	haveWindows := false
	if err := hypr.Available(ctx); err == nil {
		if cs, err := hypr.Clients(ctx); err == nil {
			clients, haveWindows = cs, true
		}
	}

	var fns []func() row
	for _, f := range features {
		f := f
		for _, s := range f.Services {
			s := s
			fns = append(fns, func() row { return serviceRow(ctx, f, s) })
		}
		for _, a := range f.Agents {
			a := a
			fns = append(fns, func() row { return agentRow(ctx, f, a) })
		}
		for _, w := range f.Windows {
			w := w
			fns = append(fns, func() row { return windowRow(f, w, clients, haveWindows) })
		}
	}
	return runConcurrent(fns)
}

// runConcurrent runs each fn concurrently and returns their results in the
// same order fns were given, regardless of completion order. Each fn writes
// to its own index only, so no further synchronization is needed.
func runConcurrent(fns []func() row) []row {
	out := make([]row, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() row) {
			defer wg.Done()
			out[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	return out
}

// serviceRow queries a single service's unit status.
func serviceRow(ctx context.Context, f *state.Feature, s state.Service) row {
	r := row{Feature: f.Name, Kind: kindService, Name: s.Name, Unit: s.Unit}
	fillUnit(ctx, &r)
	return r
}

// agentRow reports a single agent: its unit status where it has one, and the
// fuller telemetry its harness can give (busy/state, tokens, task list,
// pending question or permission, and so on).
//
// An agent with no unit is one canaveral does not supervise — Claude Code is
// started by its own window, not by us — so there is nothing to ask systemd
// and the harness answers the "is it up" question itself. Its telemetry is
// still worth reading when it is down, since it survives the program
// exiting, which is why the probe is not gated on being live.
func agentRow(ctx context.Context, f *state.Feature, a state.Agent) row {
	r := row{Feature: f.Name, Kind: kindAgent, Name: a.Name, Tool: a.Tool, Unit: a.Unit, URL: a.URL}
	if a.Unit != "" {
		fillUnit(ctx, &r)
		if r.State != "active" || a.URL == "" {
			return r
		}
	}
	h := agent.Probe(ctx, a.Tool, agent.Conn{URL: a.URL, Dir: a.Dir})
	if a.Unit == "" {
		r.State = unsupervisedState(h)
	}
	r.Busy, r.Sessions = h.Busy, h.Sessions
	r.AgentState = string(h.State)
	r.Worked, r.Working = h.Worked, h.Working
	r.SincePrompt = h.SincePrompt
	r.Idle = idleFor(h)
	r.TodoTotal = h.Todos.Total
	r.TodoDone = h.Todos.Completed + h.Todos.Cancelled
	r.TodoNow = h.Todos.Current
	r.Tokens, r.Cost = h.Tokens.Total(), h.Cost
	r.Model, r.LastError = h.Model, h.LastError
	r.Variant, r.Provider = h.Variant, h.Provider
	r.SubAgents = h.SubSessions
	if h.Activity != nil {
		r.ActTool, r.ActTitle = h.Activity.Tool, h.Activity.Title
	}
	r.LastUser, r.LastAgent = h.LastUser, h.LastAssistant
	applyPending(&r, h.Pending)
	if !h.Reachable {
		r.Detail = "unreachable"
	}
	return r
}

// unsupervisedState maps a harness's own answer onto the same vocabulary a
// systemd unit's ActiveState uses, so the status table needs no special case
// for agents canaveral does not start.
func unsupervisedState(h agent.Health) string {
	switch {
	case !h.Reachable:
		return "gone"
	case h.Live:
		return "active"
	}
	return "inactive"
}

// applyPending copies what an agent is blocked on, if anything, onto r.
func applyPending(r *row, p *agent.Pending) {
	if p == nil {
		return
	}
	r.PendKind, r.PendHeader, r.PendDetail = string(p.Kind), p.Header, p.Detail
	switch {
	case len(p.Options) > 0:
		r.PendExtra = strings.Join(p.Options, " / ")
	case len(p.Resources) > 0:
		r.PendExtra = strings.Join(p.Resources, ", ")
	}
}

// windowRow reports whether a single declared window is currently open,
// against a client list already fetched once for every feature.
func windowRow(f *state.Feature, w state.Window, clients []hypr.Client, haveWindows bool) row {
	r := row{Feature: f.Name, Kind: kindWindow, Name: w.Name}
	switch {
	case !haveWindows:
		r.State = "unknown"
	case windowOpen(clients, w):
		r.State = "open"
	default:
		r.State = "closed"
	}
	return r
}

// idleFor is how long an agent has been idle, or 0 when it's busy or has
// never had a session at all — Health.Updated is the zero Time in that
// case, and time.Since of a zero Time is a multi-million-hour nonsense
// duration, not "idle since forever".
func idleFor(h agent.Health) time.Duration {
	if h.Busy || h.Updated.IsZero() {
		return 0
	}
	return time.Since(h.Updated)
}

func fillUnit(ctx context.Context, r *row) {
	st, err := unit.Query(ctx, r.Unit)
	if err != nil {
		r.State = "gone"
		return
	}
	r.State = st.ActiveState
	r.MemBytes = st.Memory
	r.CPUNanos = st.CPU
}

// collectBranchStatus computes each feature's git status against the
// project's default branch concurrently, the same reasoning as collect:
// several git subprocesses run serially would make --watch noticeably slow.
func collectBranchStatus(ctx context.Context, features []*state.Feature) map[string]worktree.BranchStatus {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]worktree.BranchStatus, len(features))
	)
	for _, f := range features {
		if f.Worktree == "" {
			continue
		}
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := worktree.Status(ctx, f.Worktree)
			if err != nil {
				return
			}
			mu.Lock()
			out[f.Name] = s
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func printStatus(features []*state.Feature, rows []row, branches map[string]worktree.BranchStatus) {
	byFeature := map[string][]row{}
	for _, r := range rows {
		byFeature[r.Feature] = append(byFeature[r.Feature], r)
	}

	for i, f := range features {
		if i > 0 {
			fmt.Println()
		}
		bs, ok := branches[f.Name]
		printFeatureBlock(f, byFeature[f.Name], bs, ok)
	}
}

// printFeatureBlock prints one feature's status block: a header line, its
// branch position against its target if known, one summary line per agent,
// and a table of every service/agent/window row followed by any errors.
func printFeatureBlock(f *state.Feature, rows []row, bs worktree.BranchStatus, hasBranch bool) {
	fmt.Println(featureHeaderLine(f))
	if hasBranch {
		fmt.Println(dim(branchStatusLine(bs)))
	}

	// The agent's current state is the thing you're most likely checking
	// this for — is it waiting on me, still working, stuck retrying? —
	// so it gets its own prominent line instead of being just another
	// row buried in the service/agent/window table below.
	for _, r := range rows {
		// Probeable rather than "has a URL": an agent canaveral does not
		// supervise has no URL to have, and its summary is exactly as
		// worth reading.
		if r.Kind == kindAgent && agent.Probeable(r.Tool, r.URL) {
			fmt.Println(agentSummaryLine(r))
		}
	}

	printRowTable(rows)
}

// featureHeaderLine is the first line printed for a feature: its key and
// branch, its declared ports if any, and how long ago it was created.
func featureHeaderLine(f *state.Feature) string {
	hdr := fmt.Sprintf("%s  %s", color(cBold, f.Key()), dim(f.Branch))
	if len(f.Ports) > 0 {
		hdr += dim("  ports " + portSummary(f.Ports))
	}
	hdr += dim("  " + humanAgo(f.CreatedAt))
	return hdr
}

// branchStatusLine reports a feature's branch position against its target:
// ahead/behind counts, and how big the diff is when there is one.
func branchStatusLine(bs worktree.BranchStatus) string {
	line := fmt.Sprintf("  vs %s: %s", bs.Base, bs.Label())
	if bs.FilesChanged > 0 {
		line += fmt.Sprintf("  +%d/-%d across %d file(s)", bs.Insertions, bs.Deletions, bs.FilesChanged)
	}
	if bs.Uncommitted > 0 {
		line += fmt.Sprintf("  %d uncommitted", bs.Uncommitted)
	}
	return line
}

// printRowTable prints the KIND/NAME/STATE/... table for rows, followed by
// a block of any agent errors — which get their own lines rather than being
// truncated inside a table cell.
func printRowTable(rows []row) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Plain header and data cells throughout — see stateLabelPlain's doc
	// comment for why color and tabwriter don't mix.
	fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		"KIND", "NAME", "STATE", "IDLE", "WORKED", "CPU", "MEM",
		"TOKENS", "COST", "ENDPOINT")
	var errs []row
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			string(r.Kind), r.Name, stateLabelPlain(r), idleDetail(r), workedDetail(r),
			humanDuration(r.CPUNanos), humanBytes(r.MemBytes),
			humanCount(r.Tokens), humanCost(r.Cost), r.URL)
		if r.LastError != "" {
			errs = append(errs, r)
		}
	}
	tw.Flush()
	// Agent errors are what the user is watching for, so they get their own
	// block instead of being truncated inside a table cell.
	for _, r := range errs {
		fmt.Printf("  %s %s: %s\n", color(cRed, "!"), r.Name, shorten(oneLine(r.LastError), 100))
	}
}

// stateWord returns a state's plain text and the color it should be shown in
// wherever color is safe to use (see stateLabel vs stateLabelPlain).
func stateWord(r row) (text, colorCode string) {
	switch r.State {
	case "active":
		if r.Kind == kindAgent {
			if r.Detail == "unreachable" {
				return "no-api", cYellow
			}
			switch {
			case r.LastError != "":
				return "error", cRed
			case r.AgentState == string(agent.StateRetrying):
				return "retrying", cRed
			case r.AgentState == string(agent.StateWaiting):
				return "waiting", cYellow
			case r.AgentState == string(agent.StateBusy), r.Busy:
				return "busy", cCyan
			}
			return "idle", cGreen
		}
		return "active", cGreen
	case "activating":
		return "starting", cYellow
	case "open":
		return "open", cGreen
	case "closed":
		return "closed", cYellow
	case "failed", "gone":
		return r.State, cRed
	case "inactive":
		return "inactive", cDim
	}
	return r.State, ""
}

// stateLabel is a state's colored label, for output that isn't
// column-aligned (e.g. agentSummaryLine).
func stateLabel(r row) string {
	text, c := stateWord(r)
	if c == "" {
		return text
	}
	return color(c, text)
}

// stateLabelPlain is a state's label with no color at all, for use inside a
// tabwriter table. tabwriter measures raw byte length, invisible ANSI
// escape codes included, so a colored cell in a column where other cells
// (or that column's own header) aren't colored gets over-counted, and every
// column after it drifts out of alignment with its header — confirmed live,
// this was happening to every column following STATE.
func stateLabelPlain(r row) string {
	text, _ := stateWord(r)
	return text
}

// idleDetail shows how long an agent has been idle, blank while it's busy or
// there's nothing to report.
func idleDetail(r row) string {
	if r.Kind != kindAgent || r.Idle <= 0 {
		return "-"
	}
	return humanDuration(r.Idle)
}

// workedDetail shows cumulative active generation time for the session.
//
// The per-message generation timer is deliberately not shown: one prompt
// produces an assistant message per tool round trip, so it resets every few
// seconds and reads like a command timer without being one. "on this",
// measured from the user's own message, is the timer that answers that
// question, and is still available as working_nanos in --json.
func workedDetail(r row) string {
	if r.Kind != kindAgent || r.Worked <= 0 {
		return "-"
	}
	return humanDuration(r.Worked)
}

// agentSummaryLine is the at-a-glance line printed once per agent, right
// under the branch status — the state you're most likely checking for
// (waiting on you? still working? stuck retrying?) shouldn't require
// parsing it out of a table row shared with services and windows.
func agentSummaryLine(r row) string {
	line := fmt.Sprintf("  agent %s: %s", r.Name, strings.Join(summaryParts(r), " · "))
	for _, d := range summaryDetailLines(r) {
		line += "\n    " + d
	}
	return line
}

// summaryParts builds the headline pieces for an agent's summary line: its
// state, how long it has worked and (while busy or waiting) on this prompt,
// how long it has been idle, todo progress, model, and session count.
func summaryParts(r row) []string {
	parts := []string{stateLabel(r)}
	if w := workedDetail(r); w != "-" {
		parts = append(parts, "worked "+w)
	}
	// How long since you last asked for something. Distinct from worked,
	// which is cumulative across the whole session.
	if r.SincePrompt > 0 && (r.AgentState == string(agent.StateBusy) || r.AgentState == string(agent.StateWaiting)) {
		parts = append(parts, "on this "+humanDuration(r.SincePrompt))
	}
	if idl := idleDetail(r); idl != "-" {
		parts = append(parts, "idle "+idl)
	}
	if r.TodoTotal > 0 {
		parts = append(parts, fmt.Sprintf("todo %d/%d", r.TodoDone, r.TodoTotal))
	}
	if r.Model != "" {
		m := r.Model
		if r.Variant != "" {
			m += " (" + r.Variant + ")"
		}
		parts = append(parts, m)
	}
	if r.Sessions > 0 {
		sess := fmt.Sprintf("%d session(s)", r.Sessions)
		if r.SubAgents > 0 {
			sess += fmt.Sprintf(" +%d subagent(s)", r.SubAgents)
		}
		parts = append(parts, sess)
	}
	return parts
}

// summaryDetailLines builds the indented detail lines under an agent's
// summary headline: what it's blocked on, its current todo, its current
// tool activity, and the newest message on each side of the conversation.
func summaryDetailLines(r row) []string {
	var lines []string
	if r.PendKind != "" {
		needs := r.PendKind
		if r.PendHeader != "" {
			needs += ": " + r.PendHeader
		}
		if r.PendDetail != "" && r.PendDetail != r.PendHeader {
			needs += " — " + r.PendDetail
		}
		if r.PendExtra != "" {
			needs += " [" + r.PendExtra + "]"
		}
		lines = append(lines, color(cYellow, "needs: "+shorten(oneLine(needs), 90)))
	}
	if r.TodoNow != "" {
		lines = append(lines, dim("task: "+shorten(oneLine(r.TodoNow), 80)))
	}
	if r.ActTool != "" {
		now := r.ActTool
		if r.ActTitle != "" {
			now += ": " + r.ActTitle
		}
		lines = append(lines, dim("now:  "+shorten(oneLine(now), 80)))
	}
	if r.LastUser != "" {
		lines = append(lines, dim("you:  "+shorten(oneLine(r.LastUser), 80)))
	}
	if r.LastAgent != "" {
		lines = append(lines, dim("said: "+shorten(oneLine(r.LastAgent), 80)))
	}
	return lines
}

func runAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral attach <feature> [agent] [flags]\n\nOpen a feature's agent in this terminal.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		cont     = fs.Bool("continue", false, "continue the agent's last session")
		printURL = fs.Bool("url", false, "print the agent URL instead of attaching")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("specify a feature, e.g. `canaveral attach small-fixes`")
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	f, err := state.Load(m.Name, feature.Slug(pos[0]))
	if err != nil {
		return err
	}
	if len(f.Agents) == 0 {
		return fmt.Errorf("feature %q has no agents", f.Name)
	}

	var a *state.Agent
	if len(pos) > 1 {
		found, ok := f.Agent(pos[1])
		if !ok {
			return fmt.Errorf("no agent %q in feature %q", pos[1], f.Name)
		}
		a = found
	} else if len(f.Agents) > 1 {
		names := make([]string, 0, len(f.Agents))
		for _, x := range f.Agents {
			names = append(names, x.Name)
		}
		return fmt.Errorf("feature %q has %d agents; pick one: %s",
			f.Name, len(f.Agents), strings.Join(names, ", "))
	} else {
		a = &f.Agents[0]
	}

	h, err := agent.For(a.Tool)
	if err != nil {
		return err
	}
	if *printURL {
		if a.URL == "" {
			return fmt.Errorf("agent %q runs %s, which has no server to point at", a.Name, a.Tool)
		}
		fmt.Println(a.URL)
		return nil
	}
	// Only a supervised agent can be "not running" in a way `reset` fixes.
	// One canaveral does not start is started by this very command, so
	// checking whether it is up first would refuse to do the only thing it
	// is for.
	if h.Serves() {
		if a.URL == "" {
			return fmt.Errorf("agent %q has no recorded URL", a.Name)
		}
		if st, err := unit.Query(ctx, a.Unit); err != nil || !st.Running() {
			return fmt.Errorf("agent %q is not running (run `canaveral reset %s`)", a.Name, f.Name)
		}
	}

	bin, err := h.Resolve()
	if err != nil {
		return err
	}
	// The agent's directory scopes its session history, and a harness with
	// no server has no flag to say so — it reads its own working directory.
	if a.Dir != "" {
		if err := os.Chdir(a.Dir); err != nil {
			return fmt.Errorf("enter %s: %w", a.Dir, err)
		}
	}
	argv := h.AttachArgv(agent.Conn{URL: a.URL, Dir: a.Dir}, *cont)
	// Replace this process so the TUI owns the terminal directly.
	return syscallExec(bin, argv, os.Environ())
}

func runLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral logs <feature> <name> [flags]\n\nPrint or follow a service or agent log.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		follow = fs.Bool("f", false, "follow the log")
		lines  = fs.Int("n", 200, "number of trailing lines")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("usage: canaveral logs <feature> <service|agent>")
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	f, err := state.Load(m.Name, feature.Slug(pos[0]))
	if err != nil {
		return err
	}

	target, path := pos[1], ""
	named := false
	for _, s := range f.Services {
		if s.Name == target {
			named, path = true, s.LogPath
		}
	}
	for _, a := range f.Agents {
		if a.Name == target {
			named, path = true, a.LogPath
		}
	}
	switch {
	case !named:
		return fmt.Errorf("no service or agent named %q in feature %q", target, f.Name)
	case path == "":
		// An agent canaveral does not start writes to its own terminal,
		// not to a log file we own; there is nothing here to tail.
		return fmt.Errorf("agent %q is started by its own window and keeps no log", target)
	}

	tailArgs := []string{"-n", fmt.Sprint(*lines)}
	if *follow {
		tailArgs = append(tailArgs, "-F")
	}
	tailArgs = append(tailArgs, path)
	cmd := exec.CommandContext(ctx, "tail", tailArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
