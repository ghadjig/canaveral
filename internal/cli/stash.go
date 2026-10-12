package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/parked"
	"github.com/bandito/canaveral/internal/state"
)

func runStash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stash", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral stash [feature...] [flags]\n\nPark a feature: stop its services, agents and windows, and release its\nports, keeping the worktree, the branch and the agent's conversation\nexactly as they are. Defaults to whichever feature's worktree you're\ncurrently in.\n\nNothing on disk is removed, so unlike `rm` this never refuses over\nuncommitted changes or an unmerged branch. Bring it back with\n`canaveral pop <feature>`, or just `canaveral <feature>`.\n\nFlags:")
		fs.PrintDefaults()
	}
	all := fs.Bool("all", false, "stash every feature of the project")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	names := pos
	if *all {
		if names, err = state.List(m.Name); err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("no active features in %s", m.Name)
		}
	}
	if len(names) == 0 {
		// The feature you are standing in, exactly as `rm` and `merge`
		// default: stashing from inside the worktree you are done with for
		// today is the common case.
		f, err := currentFeature(m)
		if err != nil {
			return fmt.Errorf("not inside a feature worktree; specify a feature name, or use --all")
		}
		names = []string{f.Name}
	}

	r := reporter{}
	for _, n := range names {
		name := feature.Slug(n)
		f, err := state.Load(m.Name, name)
		if err != nil {
			r.Warn("%s: %v", name, err)
			continue
		}
		r.Step("stashing %s", color(cBold, f.Key()))
		s, err := feature.Stash(ctx, f, r)
		if err != nil {
			if len(names) == 1 {
				return err
			}
			r.Warn("%v", err)
			continue
		}
		r.OK("stashed %s", color(cBold, f.Key()))
		printStashSummary(s)
	}
	return nil
}

func runPop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pop", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral pop [feature...] [flags]\n\nRestore a stashed feature: the same worktree, the same branch, and the\nsame agent conversation it was in when stashed. Failed restores can be\nretried with the same command. With no argument, retries a single unfinished\nrestore, otherwise pops the project's most recently stashed feature.\n\n`canaveral new <feature>` and `canaveral <feature>` also restore stashes.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		noWindows  = fs.Bool("no-windows", false, "skip spawning windows")
		noServices = fs.Bool("no-services", false, "skip starting services")
		noAgents   = fs.Bool("no-agents", false, "skip starting agents")
		focus      = fs.Bool("focus", false, "switch to the workspace once everything is ready")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	names := pos
	if len(names) == 0 {
		active, err := state.LoadProject(m.Name)
		if err != nil {
			return err
		}
		for _, f := range active {
			if f.RestorePending {
				names = append(names, f.Name)
			}
		}
		if len(names) > 1 {
			return fmt.Errorf("multiple unfinished restores in %s; specify one: %s", m.Name, strings.Join(names, ", "))
		}
	}
	if len(names) == 0 {
		// Newest first, like `git stash pop` — LoadStashes orders by when
		// each was parked precisely so this line can mean something.
		stashes, err := state.LoadStashes(m.Name)
		if err != nil {
			return err
		}
		if len(stashes) == 0 {
			return fmt.Errorf("no stashed features in %s (stash one with `canaveral stash <feature>`)", m.Name)
		}
		names = []string{stashes[0].Feature.Name}
	}

	opt := feature.Options{NoWindows: *noWindows, NoServices: *noServices, NoAgents: *noAgents}
	r := reporter{}
	for _, n := range names {
		name := feature.Slug(n)
		// Checked before announcing anything: Pop would report the same
		// ErrNotFound, but only after "restoring demo/nope" had already
		// gone to the terminal.
		s, err := state.LoadStash(m.Name, name)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				if f, loadErr := state.Load(m.Name, name); loadErr == nil && f.RestorePending {
					r.Step("retrying restore %s", color(cBold, f.Key()))
					res, err := feature.Pop(ctx, m, name, opt, r)
					if err != nil {
						return err
					}
					if err := reportFeature(ctx, res, r, *focus && !*noWindows); err != nil {
						return err
					}
					continue
				}
				return unknownStash(m.Name, name)
			}
			return err
		}
		r.Step("restoring %s  %s",
			color(cBold, s.Feature.Key()), color(cDim, "stashed "+humanAgo(s.StashedAt)))
		res, err := feature.Pop(ctx, m, name, opt, r)
		if err != nil {
			return err
		}
		// Focus only ever applies to the last one restored; asking to be
		// moved to several workspaces at once has no coherent answer.
		if err := reportFeature(ctx, res, r, *focus && !*noWindows); err != nil {
			return err
		}
	}
	return nil
}

// unknownStash explains a name that is not stashed, listing what is. The list
// is the useful part: a stash is out of sight by design, so the most likely
// mistake is misremembering which one was parked.
func unknownStash(project, name string) error {
	msg := fmt.Sprintf("no stashed feature %q in %s", name, project)
	known, err := state.ListStashes(project)
	if err != nil || len(known) == 0 {
		return fmt.Errorf("%s\n  nothing is stashed; stash a feature with `canaveral stash <feature>`", msg)
	}
	if s := nearest(name, known); s != "" {
		return fmt.Errorf("%s\n  did you mean `canaveral pop %s`?", msg, s)
	}
	return fmt.Errorf("%s\n  stashed: %s", msg, strings.Join(known, ", "))
}

func printStashSummary(s *state.Stash) {
	f := s.Feature
	fmt.Printf("    %s %s\n", dim("branch  "), f.Branch)
	fmt.Printf("    %s %s\n", dim("worktree"), homeTilde(f.Worktree))
	if len(s.Sessions) > 0 {
		var parts []string
		for _, a := range f.Agents {
			if id, ok := s.Sessions[a.Name]; ok {
				parts = append(parts, a.Name+"="+id)
			}
		}
		fmt.Printf("    %s %s\n", dim("sessions"), strings.Join(parts, "  "))
	}
	fmt.Printf("    %s %s\n", dim("restore "), "canaveral pop "+f.Name)
}

// printParked lists the workspaces under the `ls` table that are not running
// but could be: stashes and dormant worktrees.
//
// A separate block rather than extra rows: a parked workspace has no running
// services, no open windows and no ports to report, so four of the table's
// six columns would be dashes, and a reader scanning for what is actually up
// would have to check a status column on every row to tell the two apart.
//
// One block for both kinds, because the same command wakes either — bare
// dispatch pops a stash and adopts a dormant worktree — and STATE says which
// of them will come back with its agent's conversation.
func printParked(ws []parked.Workspace, withProject bool) {
	if len(ws) == 0 {
		return
	}
	hint := "canaveral <feature>"
	if withProject {
		hint = "canaveral -C <project> <feature>"
	}
	fmt.Printf("\n%s\n", dim(plural(len(ws), "parked workspace")+" — wake with `"+hint+"`"))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if withProject {
		fmt.Fprint(tw, "PROJECT\t")
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", "FEATURE", "BRANCH", "STATE", "SINCE")
	for _, w := range ws {
		if withProject {
			fmt.Fprintf(tw, "%s\t", w.Project)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			w.Name, shorten(w.Branch, 28), w.Kind, humanAgo(w.Since))
	}
	tw.Flush()
}

// stashedFeature loads a stashed record for a command that was given a name
// with no active feature behind it, so `rm` can discard a stash by name
// rather than needing a flag to say which tree to look in. A name can only be
// in one of the two trees at a time, so there is nothing to disambiguate.
func stashedFeature(m *manifest.Manifest, name string) (*state.Stash, bool) {
	s, err := state.LoadStash(m.Name, name)
	if err != nil {
		return nil, false
	}
	return s, true
}
