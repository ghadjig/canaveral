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
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	var features []*state.Feature
	var err error
	if *all {
		features, err = state.LoadAll()
	} else {
		m, mErr := loadManifest()
		if mErr != nil {
			return mErr
		}
		features, err = state.LoadProject(m.Name)
	}
	if err != nil {
		return err
	}
	if len(features) == 0 {
		fmt.Println("no features yet — create one with `canaveral <feature>`")
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
	return tw.Flush()
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
)

type row struct {
	Feature     string        `json:"feature"`
	Kind        rowKind       `json:"kind"`
	Name        string        `json:"name"`
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

	targets := func() ([]*state.Feature, error) {
		if *all {
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

	render := func() error {
		fsList, err := targets()
		if err != nil {
			return err
		}
		if len(fsList) == 0 {
			if *asJSON {
				fmt.Println("[]")
				return nil
			}
			fmt.Println("no features yet — create one with `canaveral <feature>`")
			return nil
		}
		rows := collect(ctx, fsList)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		branches := collectBranchStatus(ctx, fsList)
		printStatus(fsList, rows, branches)
		return nil
	}

	if *watch <= 0 {
		return render()
	}
	if *watch < 500*time.Millisecond {
		*watch = 500 * time.Millisecond
	}
	for {
		fmt.Print("\033[H\033[2J")
		if err := render(); err != nil {
			return err
		}
		fmt.Printf("\n%s\n", dim("refreshing every "+watch.String()+" — ctrl-c to exit"))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*watch):
		}
	}
}

// collect queries units, agents and windows concurrently; serial probing with
// HTTP timeouts would make --watch unusable.
func collect(ctx context.Context, features []*state.Feature) []row {
	type slot struct {
		idx int
		r   row
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		slots []slot
		n     int
	)
	add := func(fn func(int)) {
		i := n
		n++
		wg.Add(1)
		go func() { defer wg.Done(); fn(i) }()
	}

	// Query the window list once rather than per feature.
	var clients []hypr.Client
	haveWindows := false
	if err := hypr.Available(ctx); err == nil {
		if cs, err := hypr.Clients(ctx); err == nil {
			clients, haveWindows = cs, true
		}
	}

	for _, f := range features {
		f := f
		for _, s := range f.Services {
			s := s
			add(func(i int) {
				r := row{Feature: f.Name, Kind: kindService, Name: s.Name, Unit: s.Unit}
				fillUnit(ctx, &r)
				mu.Lock()
				slots = append(slots, slot{i, r})
				mu.Unlock()
			})
		}
		for _, a := range f.Agents {
			a := a
			add(func(i int) {
				r := row{Feature: f.Name, Kind: kindAgent, Name: a.Name, Unit: a.Unit, URL: a.URL}
				fillUnit(ctx, &r)
				if r.State == "active" && a.URL != "" {
					h := agent.Probe(ctx, a.URL, a.Dir)
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
					if p := h.Pending; p != nil {
						r.PendKind, r.PendHeader, r.PendDetail = string(p.Kind), p.Header, p.Detail
						switch {
						case len(p.Options) > 0:
							r.PendExtra = strings.Join(p.Options, " / ")
						case len(p.Resources) > 0:
							r.PendExtra = strings.Join(p.Resources, ", ")
						}
					}
					if !h.Reachable {
						r.Detail = "unreachable"
					}
				}
				mu.Lock()
				slots = append(slots, slot{i, r})
				mu.Unlock()
			})
		}
		for _, w := range f.Windows {
			w := w
			add(func(i int) {
				r := row{Feature: f.Name, Kind: kindWindow, Name: w.Name}
				switch {
				case !haveWindows:
					r.State = "unknown"
				case windowOpen(clients, w):
					r.State = "open"
				default:
					r.State = "closed"
				}
				mu.Lock()
				slots = append(slots, slot{i, r})
				mu.Unlock()
			})
		}
	}
	wg.Wait()

	out := make([]row, n)
	for _, s := range slots {
		out[s.idx] = s.r
	}
	return out
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
		hdr := fmt.Sprintf("%s  %s", color(cBold, f.Key()), dim(f.Branch))
		if len(f.Ports) > 0 {
			hdr += dim("  ports " + portSummary(f.Ports))
		}
		hdr += dim("  " + humanAgo(f.CreatedAt))
		fmt.Println(hdr)

		if bs, ok := branches[f.Name]; ok {
			line := fmt.Sprintf("  vs %s: %s", bs.Base, bs.Label())
			if bs.FilesChanged > 0 {
				line += fmt.Sprintf("  +%d/-%d across %d file(s)", bs.Insertions, bs.Deletions, bs.FilesChanged)
			}
			fmt.Println(dim(line))
		}

		// The agent's current state is the thing you're most likely checking
		// this for — is it waiting on me, still working, stuck retrying? —
		// so it gets its own prominent line instead of being just another
		// row buried in the service/agent/window table below.
		for _, r := range byFeature[f.Name] {
			if r.Kind == kindAgent && r.URL != "" {
				fmt.Println(agentSummaryLine(r))
			}
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		// Plain header and data cells throughout — see stateLabelPlain's doc
		// comment for why color and tabwriter don't mix.
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			"KIND", "NAME", "STATE", "IDLE", "WORKED", "CPU", "MEM",
			"TOKENS", "COST", "ENDPOINT")
		var errs []row
		for _, r := range byFeature[f.Name] {
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
	line := fmt.Sprintf("  agent %s: %s", r.Name, strings.Join(parts, " · "))
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
		line += "\n    " + color(cYellow, "needs: "+shorten(oneLine(needs), 90))
	}
	if r.TodoNow != "" {
		line += "\n    " + dim("task: "+shorten(oneLine(r.TodoNow), 80))
	}
	if r.ActTool != "" {
		now := r.ActTool
		if r.ActTitle != "" {
			now += ": " + r.ActTitle
		}
		line += "\n    " + dim("now:  "+shorten(oneLine(now), 80))
	}
	if r.LastUser != "" {
		line += "\n    " + dim("you:  "+shorten(oneLine(r.LastUser), 80))
	}
	if r.LastAgent != "" {
		line += "\n    " + dim("said: "+shorten(oneLine(r.LastAgent), 80))
	}
	return line
}

func runAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral attach <feature> [agent] [flags]\n\nAttach an opencode TUI to a feature's agent.\n\nFlags:")
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

	if a.URL == "" {
		return fmt.Errorf("agent %q has no recorded URL", a.Name)
	}
	if *printURL {
		fmt.Println(a.URL)
		return nil
	}
	if st, err := unit.Query(ctx, a.Unit); err != nil || !st.Running() {
		return fmt.Errorf("agent %q is not running (run `canaveral reset %s`)", a.Name, f.Name)
	}

	bin, err := exec.LookPath("opencode")
	if err != nil {
		return fmt.Errorf("opencode not found in PATH: %w", err)
	}
	argv := []string{"opencode", "attach", a.URL, "--dir", a.Dir}
	if *cont {
		argv = append(argv, "--continue")
	}
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
	for _, s := range f.Services {
		if s.Name == target {
			path = s.LogPath
		}
	}
	for _, a := range f.Agents {
		if a.Name == target {
			path = a.LogPath
		}
	}
	if path == "" {
		return fmt.Errorf("no service or agent named %q in feature %q", target, f.Name)
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
