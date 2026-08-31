package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/launcherhistory"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/registry"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
)

// Candidate kinds. The launcher styles each differently and decides what
// pressing Enter means from them, so they are part of the output contract and
// not merely decoration.
const (
	candProject   = "project"
	candCommand   = "command"
	candFeature   = "feature"
	candNamespace = "namespace"
	candService   = "service"
	candAgent     = "agent"
	candFlag      = "flag"
	// candNew is the candidate that creates something that does not exist yet.
	// It is the whole reason the launcher exists, and it has to be visible
	// rather than implied: bare dispatch will happily create a feature from a
	// typo, and seeing "create X" before pressing Enter is the only warning
	// anyone gets.
	candNew = "new"
	// candHistory is a whole line typed before, offered so the second time
	// costs a few keystrokes instead of all of them. It only ever appears
	// alongside the project list, at the very start of the line — see
	// historyCandidates.
	candHistory = "history"
)

// candidate is one completion suggestion.
type candidate struct {
	Value string `json:"value"`
	Kind  string `json:"kind"`
	Desc  string `json:"desc,omitempty"`
	// Continues marks a value that is a prefix of a longer answer rather than a
	// complete one — a namespace, whose trailing "/" must not be followed by a
	// space because there is always more to type after it.
	Continues bool `json:"continues,omitempty"`
}

// completion is the JSON document `canaveral complete` prints.
type completion struct {
	// Prefix is the partial word these candidates complete.
	Prefix string `json:"prefix"`
	// Common is the longest prefix shared by every candidate, which is what a
	// Tab press should insert. Computed here so the shell and the launcher
	// cannot disagree about it.
	Common     string      `json:"common"`
	Candidates []candidate `json:"candidates"`
	// Project is the project the candidates were resolved against, empty when
	// completing the project name itself.
	Project string `json:"project,omitempty"`
	// Command is what the line currently means: a command name, or "open" when
	// a bare feature name is being used for dispatch.
	Command string `json:"command,omitempty"`
	// Destructive marks a line the launcher should confirm before running. A
	// mistyped `rm` costs far more from a global hotkey than from a shell, and
	// only canaveral knows which commands tear things down.
	Destructive bool `json:"destructive,omitempty"`
	// Fuzzy reports that nothing matched the prefix directly and these are
	// substring matches instead, so a caller can show them as the guesses they
	// are rather than as completions.
	Fuzzy bool `json:"fuzzy,omitempty"`
	// Error explains why there are no candidates. Not a process failure: a
	// completer that exits non-zero mid-keystroke is useless to a UI, so
	// problems are reported in-band.
	Error string `json:"error,omitempty"`
}

// destructive lists commands that remove work.
var destructive = map[string]bool{"rm": true, "merge": true}

// argKind names what a command's positional argument at some index is.
type argKind int

const (
	argNone argKind = iota
	argFeature
	// argNewFeature is a feature name that must NOT already exist. Existing
	// features are deliberately not offered: `new` refuses one that is already
	// there, so listing them would be offering an error. Namespaces still are,
	// since creating inside an existing one is normal.
	argNewFeature
	argAgent
	// argLogTarget is a service or an agent: `logs` accepts either.
	argLogTarget
	argService
	// argFeatureOrService is `restart`'s first argument, which is genuinely
	// ambiguous — restartTarget resolves it at run time and refuses when a name
	// is both — so completion offers both rather than picking a side.
	argFeatureOrService
)

// commandArgs describes each command's positional arguments by index. The last
// entry repeats for every further argument, so a command taking a list of
// features ends with argFeature and one taking exactly one ends with argNone.
//
// Every command in commands() must appear here; TestCompletionCoversEveryCommand
// enforces that, so a new command cannot silently complete nothing.
var commandArgs = map[string][]argKind{
	"init":      {argNone},
	"new":       {argNewFeature, argNone},
	"open":      {argFeature, argNone},
	"reset":     {argFeature},
	"restart":   {argFeatureOrService, argService},
	"ls":        {argNone},
	"status":    {argFeature, argNone},
	"rm":        {argFeature},
	"prune":     {argNone},
	"rebase":    {argFeature, argNone},
	"merge":     {argFeature, argNone},
	"attach":    {argFeature, argAgent, argNone},
	"logs":      {argFeature, argLogTarget, argNone},
	"path":      {argFeature, argNone},
	"exec":      {argFeature, argNone},
	"projects":  {argNone},
	"complete":  {argNone},
	"hyprwatch": {argNone},
	"ws-slot":   {argNone},
	"watch":     {argNone},
}

// commandFlags lists each command's flags for completion.
//
// Hand-maintained, and deliberately so: the real flag sets are built inside
// each run function and are not reachable from here without threading a
// constructor through all of them. TestCompletionCoversEveryCommand catches a
// new *command* with no entry, but nothing catches a new *flag* on an existing
// command — add it here when you add it there.
var commandFlags = map[string]map[string]string{
	"init":     {"--force": "overwrite an existing canaveral.toml"},
	"new":      {"--no-windows": "skip spawning windows", "--no-services": "skip starting services", "--no-agents": "skip starting agents", "--focus": "switch to the workspace once ready", "--base": "base ref for the new feature branch"},
	"open":     {"--no-windows": "skip spawning windows", "--no-services": "skip starting services", "--no-agents": "skip starting agents", "--focus": "switch to the workspace once ready", "--base": "base ref for a new feature branch"},
	"reset":    {"--all": "reset every feature of the project", "--no-windows": "skip windows"},
	"ls":       {"--all": "list features across every project", "--names": "print only feature names"},
	"status":   {"--json": "print as JSON", "--watch": "redraw on an interval", "--all": "every project"},
	"rm":       {"--keep-worktree": "leave the worktree on disk", "--force": "remove despite uncommitted changes or an unmerged branch", "--keep-branch": "never delete the branch", "--all": "remove every feature of the project"},
	"prune":    {"--dry-run": "list what would be stopped, and stop nothing"},
	"rebase":   {"--onto": "branch or ref to rebase onto", "--remote": "remote to fetch from", "--no-fetch": "skip the fetch"},
	"merge":    {"--into": "branch to merge into", "--ff-only": "refuse a merge commit", "--keep": "do not tear the feature down afterwards"},
	"attach":   {"--continue": "resume the last session", "--url": "print the agent URL instead"},
	"logs":     {"-f": "follow", "-n": "number of lines"},
	"projects": {"--add": "register a checkout", "--forget": "drop an entry", "--scan": "walk a directory for checkouts", "--prune": "drop dead entries", "--json": "print as JSON", "--names": "print only names"},
	"complete": {"--launcher": "first word is a project name", "--record": "remember the line as launcher history", "--format": "json or lines"},
	"watch":    {"--all": "every project", "--debounce": "coalescing window", "--rescan": "rescan interval", "--safety": "safety-net interval", "--git": "git refresh interval"},
	"ws-slot":  {"--json": "print as JSON"},

	"hyprwatch": {"--install": "write a systemd user unit", "--verbose": "log every event"},
	"restart":   {},
	"path":      {},
	"exec":      {},
}

func runComplete(ctx context.Context, args []string) error {
	launcher := false
	record := false
	format := "json"
	var words []string

	// Parsed by hand rather than with a FlagSet: the words being completed are
	// arbitrary and routinely start with "-", which any flag parser would
	// swallow. Everything after "--" is data.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			words = args[i+1:]
			i = len(args)
		case a == "--launcher" || a == "-launcher":
			launcher = true
		case a == "--record" || a == "-record":
			record = true
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case a == "--format" || a == "-format":
			if i+1 < len(args) {
				i++
				format = args[i]
			}
		case a == "-h" || a == "--help":
			completeUsage()
			return nil
		default:
			return fmt.Errorf("unknown argument %q (pass the line to complete after --)", a)
		}
	}

	// A separate mode rather than a candidate query: the launcher calls this
	// once per keystroke and once more when a line actually runs, and those
	// are different events. Recording candidate lookups would remember every
	// half-typed line ever seen; recording only lets the launcher say "this
	// one ran".
	if record {
		return launcherhistory.Record(strings.Join(words, " "))
	}

	c := complete(words, launcher)

	switch format {
	case "lines":
		// Bare values, one per line, for shells that do their own filtering and
		// rendering. A namespace keeps its trailing slash, which is what stops
		// compgen appending a space and ending the word too early.
		for _, cand := range c.Candidates {
			fmt.Println(cand.Value)
		}
		return nil
	case "json":
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(c)
	default:
		return fmt.Errorf("unknown format %q (want json or lines)", format)
	}
}

func completeUsage() {
	fmt.Fprintln(os.Stderr, `Usage: canaveral complete [flags] -- <word>...

List completion candidates for a partial command line. Every word typed so far
is passed after --, with the word being completed last; when the line ends in a
space, pass an extra empty word, exactly as bash's COMP_WORDS does.

Flags:
  --launcher     treat the first word as a project name (for the popup launcher)
  --record       remember the line after -- as launcher history, instead of
                 completing it
  --format json  json (default) or lines

Examples:
  canaveral complete -- rm ''
  canaveral complete --launcher -- norules r
  canaveral complete --record -- norules rm my-feature`)
}

// complete resolves a partial command line to its candidates.
//
// In launcher mode the first word is a project name and everything after it is
// an ordinary canaveral argv, so the two grammars share this one implementation
// and cannot drift apart. That is also why `canaveral -C <project> <argv>` is
// spelled the way it is: a launcher line maps onto it with no translation.
func complete(words []string, launcher bool) completion {
	if len(words) == 0 {
		words = []string{""}
	}
	prefix := words[len(words)-1]
	done := words[:len(words)-1]

	if launcher {
		if len(done) == 0 {
			return completeProjects(prefix)
		}
		return inProject(done[0], done[1:], prefix)
	}

	// A leading -C mirrors what Main does with it, so completion follows the
	// flag to the project it names rather than answering for the current
	// directory — which is the whole reason anyone typed it.
	if len(done) > 0 {
		switch a := done[0]; {
		case a == "-C" || a == "--project":
			if len(done) == 1 {
				return completeProjects(prefix)
			}
			return inProject(done[1], done[2:], prefix)
		case strings.HasPrefix(a, "-C="):
			return inProject(strings.TrimPrefix(a, "-C="), done[1:], prefix)
		case strings.HasPrefix(a, "--project="):
			return inProject(strings.TrimPrefix(a, "--project="), done[1:], prefix)
		}
	}

	m, err := manifestHere()
	if err != nil {
		return completion{Prefix: prefix, Error: err.Error(), Candidates: []candidate{}}
	}
	c := completeArgs(m, done, prefix)
	c.Project = m.Name
	return c
}

// inProject completes an argv against a named project.
func inProject(name string, done []string, prefix string) completion {
	m, err := manifestForProject(name)
	if err != nil {
		return completion{Prefix: prefix, Error: err.Error(), Candidates: []candidate{}}
	}
	c := completeArgs(m, done, prefix)
	c.Project = m.Name
	return c
}

// manifestForProject loads a project's manifest by registry name or path,
// without recording it. Completion runs on every keystroke; letting it touch
// the registry would make merely *looking* at the launcher reorder it.
func manifestForProject(name string) (*manifest.Manifest, error) {
	root, err := resolveProject(name)
	if err != nil {
		return nil, err
	}
	return manifest.Load(root)
}

// manifestHere loads the manifest for the current directory, without recording.
func manifestHere() (*manifest.Manifest, error) {
	root, err := manifest.Find(".")
	if err != nil {
		return nil, err
	}
	return manifest.Load(root)
}

func completeProjects(prefix string) completion {
	projects, err := registry.MRU()
	if err != nil {
		return completion{Prefix: prefix, Error: err.Error(), Candidates: []candidate{}}
	}
	var all []candidate
	for _, p := range projects {
		// A checkout that has moved or been deleted is noise in a launcher: it
		// can only ever produce an error, and it is one `projects --prune`
		// away from being gone for good.
		if !p.Alive() {
			continue
		}
		all = append(all, candidate{Value: p.Name, Kind: candProject, Desc: homeTilde(p.Root)})
	}
	// History lines come last, and are filtered exactly like everything else
	// in finish: a line only survives once the user has typed something it
	// does not start with, which is what makes it "pop up immediately, then
	// vanish" rather than clutter the list forever. They only make sense here,
	// at the very first word, because a history line is a whole command —
	// project, command and arguments together — and replacing "the last word"
	// with one when there is only one word so far replaces the entire input,
	// exactly as if it had been retyped.
	all = append(all, historyCandidates(prefix)...)
	return finish(prefix, all, completion{Prefix: prefix})
}

// historyCandidates offers previously typed launcher lines, most recent first.
//
// Capped well below what Recent could return: this is a shortcut for the
// handful of things typed recently, not a second history browser competing
// with the project list for space in an 8-row popup.
func historyCandidates(prefix string) []candidate {
	const shown = 5
	entries, err := launcherhistory.Recent(shown)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, e := range entries {
		out = append(out, candidate{Value: e.Line, Kind: candHistory, Desc: humanAgo(e.LastUsed)})
	}
	return out
}

// completeArgs completes a canaveral argv, with the project already resolved.
func completeArgs(m *manifest.Manifest, done []string, prefix string) completion {
	if len(done) == 0 {
		return completeFirstWord(m, prefix)
	}

	cmd := done[0]
	rest := done[1:]
	if !reserved()[cmd] {
		// Bare dispatch: the first word is a feature name, so this is `open`,
		// which takes nothing further.
		return completion{Prefix: prefix, Command: "open", Candidates: []candidate{}, Common: prefix}
	}

	base := completion{Prefix: prefix, Command: cmd, Destructive: destructive[cmd]}

	if strings.HasPrefix(prefix, "-") {
		var all []candidate
		for name, desc := range commandFlags[cmd] {
			all = append(all, candidate{Value: name, Kind: candFlag, Desc: desc})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Value < all[j].Value })
		return finish(prefix, all, base)
	}

	// Flags already on the line are not positional arguments; counting them as
	// such would offer an agent name where `rm --force <tab>` wants a feature.
	positional := 0
	for _, w := range rest {
		if !strings.HasPrefix(w, "-") {
			positional++
		}
	}

	switch kindAt(cmd, positional) {
	case argFeature:
		return finish(prefix, featureCandidates(m, prefix, false), base)
	case argNewFeature:
		return finish(prefix, featureCandidates(m, prefix, true), base)
	case argFeatureOrService:
		return finish(prefix, append(featureCandidates(m, prefix, false), serviceCandidates(m, false)...), base)
	case argService:
		return finish(prefix, serviceCandidates(m, false), base)
	case argAgent:
		return finish(prefix, agentCandidates(m), base)
	case argLogTarget:
		return finish(prefix, serviceCandidates(m, true), base)
	default:
		base.Candidates = []candidate{}
		base.Common = prefix
		return base
	}
}

// completeFirstWord offers commands and existing features together, because
// the first word can be either: `canaveral rm` is a command and
// `canaveral small-fixes` is a feature, and the user has not decided which they
// are typing yet.
//
// It does NOT offer to create anything. Bare dispatch only opens features that
// already exist — a mistyped command must fail rather than quietly build a
// worktree, a branch, a server and an agent (see openFeature). Creating is
// `canaveral new`, and the create candidate lives there, one word further in.
func completeFirstWord(m *manifest.Manifest, prefix string) completion {
	var all []candidate
	for _, c := range commands() {
		all = append(all, candidate{Value: c.name, Kind: candCommand, Desc: c.summary})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Value < all[j].Value })
	all = append(all, featureCandidates(m, prefix, false)...)
	return finish(prefix, all, completion{Prefix: prefix, Command: "open"})
}

// featureCandidates lists a project's features one path segment at a time.
//
// Namespaced features are stored as "namespace/feature", and offering the whole
// string at once would make a project with several namespaces unreadable long
// before it became unusable. So a prefix of "" offers each namespace as a
// single entry, and accepting one narrows to its contents — the same shape as
// completing a directory path, for the same reason.
//
// `creating` switches this from naming something that exists to naming
// something that must not: existing features are dropped, since `new` refuses
// one that is already there and offering it would be offering an error, while
// namespaces stay, since creating inside an existing one is ordinary. It also
// widens where namespaces come from — see skills.Namespaces.
func featureCandidates(m *manifest.Manifest, prefix string, creating bool) []candidate {
	names, err := state.List(m.Name)
	if err != nil {
		names = nil
	}
	records := map[string]*state.Feature{}
	if fs, err := state.LoadProject(m.Name); err == nil {
		for _, f := range fs {
			records[f.Name] = f
		}
	}

	// Everything up to and including the last "/" is settled; only the segment
	// after it is being completed.
	base := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		base = prefix[:i+1]
	}

	leaves, ns, exact := partitionByNamespace(names, prefix, base)

	var out []candidate
	if creating {
		// Namespaces survive, the features inside them do not.
		namespacesFromSkills(m.Name, base, ns)
	} else {
		for _, n := range leaves {
			out = append(out, candidate{Value: n, Kind: candFeature, Desc: featureDesc(records[n])})
		}
	}

	// These land after the namespaces that still have features in them,
	// which is the order worth having: what is open now, then what could be
	// reopened.
	for _, name := range ns.order {
		out = append(out, candidate{
			Value:     name + "/",
			Kind:      candNamespace,
			Desc:      namespaceDesc(ns.count[name]),
			Continues: true,
		})
	}

	if creating && !exact {
		if c, ok := newFeatureCandidate(prefix, base, records); ok {
			out = append(out, c)
		}
	}
	return out
}

// nsBucket accumulates the namespaces to offer as completions, in the order
// first seen, deduplicated, alongside how many currently-recorded features
// each holds.
type nsBucket struct {
	order []string
	seen  map[string]bool
	count map[string]int
}

func newNsBucket() *nsBucket {
	return &nsBucket{seen: map[string]bool{}, count: map[string]int{}}
}

func (b *nsBucket) note(ns string) {
	if !b.seen[ns] {
		b.seen[ns] = true
		b.order = append(b.order, ns)
	}
}

// partitionByNamespace splits names (recorded feature names) into the ones
// that complete prefix directly (leaves) and the namespaces one level below
// base that some of them share. exact reports whether prefix itself already
// names an existing feature.
func partitionByNamespace(names []string, prefix, base string) (leaves []string, ns *nsBucket, exact bool) {
	ns = newNsBucket()
	for _, n := range names {
		if n == prefix {
			exact = true
		}
		if !strings.HasPrefix(n, base) {
			continue
		}
		rest := n[len(base):]
		if i := strings.Index(rest, "/"); i >= 0 {
			name := base + rest[:i]
			ns.note(name)
			ns.count[name]++
			continue
		}
		leaves = append(leaves, n)
	}
	return leaves, ns, exact
}

// namespacesFromSkills adds any namespace under base that still has a
// recorded skill or session even though its last feature is gone.
//
// An emptied namespace is still a namespace: its shared skill and recorded
// sessions are exactly what the next feature under it wants to inherit, so
// it has to stay offerable once its last feature is gone. Failures are
// swallowed because a completer that goes silent mid-keystroke is worse
// than one missing a few entries.
func namespacesFromSkills(project, base string, b *nsBucket) {
	known, err := skills.Namespaces(project)
	if err != nil {
		return
	}
	for _, full := range known {
		if !strings.HasPrefix(full, base) {
			continue
		}
		rest := full[len(base):]
		if rest == "" {
			continue
		}
		// A deeper namespace still only contributes its next segment, the
		// same as a feature path does.
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		b.note(base + rest)
	}
}

// newFeatureCandidate offers to create prefix as a new feature, when it
// does not already name one and genuinely extends what is settled.
func newFeatureCandidate(prefix, base string, records map[string]*state.Feature) (candidate, bool) {
	// Slugged, because that is the name that will actually be created — a
	// launcher that shows "My Feature" and produces "my-feature" is lying
	// about what Enter does.
	//
	// The slug has to genuinely extend what is settled, not just be
	// non-empty: Slug drops empty segments, so "onboarding/" and
	// "onboarding/!!!" both come back as "onboarding" and would offer to
	// create the namespace itself as a flat feature — the opposite of what
	// someone who just typed a separator is asking for.
	slug := feature.Slug(prefix)
	if len(slug) > len(base) && strings.HasPrefix(slug, base) && !reserved()[slug] && records[slug] == nil {
		return candidate{Value: slug, Kind: candNew, Desc: "create this feature"}, true
	}
	return candidate{}, false
}

// namespaceDesc describes a namespace by what is currently open under it. A
// count of zero means one held open by its skill alone, which is worth saying
// outright: it looks identical to a populated one otherwise, and "0 features"
// reads like an error rather than an invitation.
func namespaceDesc(n int) string {
	if n == 0 {
		return "shared skill, no open features"
	}
	return plural(n, "feature")
}

func featureDesc(f *state.Feature) string {
	if f == nil {
		return ""
	}
	var parts []string
	if f.WSlot > 0 {
		parts = append(parts, fmt.Sprintf("slot %d", f.WSlot))
	}
	// The default branch template is "{{.Feature}}", so for most features the
	// branch is the name again. Repeating it back at twice the width says
	// nothing; showing it only when it differs makes it worth reading.
	if f.Branch != "" && f.Branch != f.Name {
		parts = append(parts, f.Branch)
	}
	return strings.Join(parts, "  ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// serviceCandidates lists a project's services, and its agents too when the
// command accepts either (`logs` reads both kinds of log).
func serviceCandidates(m *manifest.Manifest, withAgents bool) []candidate {
	var out []candidate
	for _, s := range m.Services {
		out = append(out, candidate{Value: s.Name, Kind: candService, Desc: oneLine(s.Cmd)})
	}
	if withAgents {
		out = append(out, agentCandidates(m)...)
	}
	return out
}

func agentCandidates(m *manifest.Manifest) []candidate {
	var out []candidate
	for _, a := range m.Agents {
		out = append(out, candidate{Value: a.Name, Kind: candAgent, Desc: a.Tool})
	}
	return out
}

// kindAt returns the kind of the positional argument at index i, treating the
// last declared entry as repeating.
func kindAt(cmd string, i int) argKind {
	kinds, ok := commandArgs[cmd]
	if !ok || len(kinds) == 0 {
		return argNone
	}
	if i >= len(kinds) {
		return kinds[len(kinds)-1]
	}
	return kinds[i]
}

// finish filters candidates against the prefix and fills in the derived fields.
//
// Prefix matching first, substring matching only if that found nothing: bash
// takes the list verbatim and would show a substring match as a completion that
// does not complete anything, so the fallback has to stay a fallback. When it
// fires the result says so, and a launcher can present the results as the
// guesses they are.
func finish(prefix string, all []candidate, base completion) completion {
	lower := strings.ToLower(prefix)
	var hits []candidate
	for _, c := range all {
		// The create candidate is exempt from filtering: it is derived from the
		// prefix rather than matched against it, and slugging can move it out
		// of its own filter's reach — "my_feature" becomes "my-feature", which
		// is not a prefix match for what was typed. Dropping it would leave the
		// user staring at an empty list while typing a perfectly good name.
		if c.Kind == candNew || strings.HasPrefix(strings.ToLower(c.Value), lower) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 0 && prefix != "" {
		for _, c := range all {
			if strings.Contains(strings.ToLower(c.Value), lower) {
				hits = append(hits, c)
			}
		}
		base.Fuzzy = len(hits) > 0
	}
	if hits == nil {
		hits = []candidate{}
	}
	base.Prefix = prefix
	base.Candidates = hits
	base.Common = commonPrefix(hits, prefix)
	return base
}

// commonPrefix returns the longest prefix every candidate shares, which is what
// a Tab press inserts. Falls back to what was typed when there is nothing to
// add, so a caller can always insert the result unconditionally.
func commonPrefix(cands []candidate, fallback string) string {
	if len(cands) == 0 {
		return fallback
	}
	common := cands[0].Value
	for _, c := range cands[1:] {
		i := 0
		for i < len(common) && i < len(c.Value) && common[i] == c.Value[i] {
			i++
		}
		common = common[:i]
	}
	if len(common) < len(fallback) {
		return fallback
	}
	return common
}
