package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/space"
	"github.com/bandito/canaveral/internal/state"
)

// spaceSubcommands is the `canaveral space` verb table. Listing is the
// default because it is the one you reach for without having decided
// anything yet.
func spaceSubcommands() []command {
	return []command{
		{"ls", "list the spaces you have defined", runSpaceLs},
		{"new", "write a starter definition for a new space", runSpaceNew},
		{"open", "open a space, whatever the current directory is", runSpaceOpen},
		{"close", "stop a space's units and close its windows, keeping the definition", runSpaceClose},
		{"edit", "open a space's definition in $EDITOR", runSpaceEdit},
		{"path", "print the path of a space's definition file", runSpacePath},
		{"rm", "delete a space's definition, closing it first", runSpaceRm},
	}
}

func spaceUsage(w *os.File) {
	fmt.Fprintln(w, "Usage: canaveral space <command> [args]")
	fmt.Fprintln(w, "\nA space is a workspace with no project behind it: no repository, no")
	fmt.Fprintln(w, "worktree, no branch — just the windows, services and agents you declared,")
	fmt.Fprintln(w, "on a Hyprland workspace of its own. Its definition lives in canaveral's")
	fmt.Fprintln(w, "config directory rather than in a checkout, because there is no checkout")
	fmt.Fprintln(w, "to put it in.")
	fmt.Fprintln(w, "\nOnce defined, open it from anywhere by name:")
	fmt.Fprintln(w, "\n  canaveral 3d-printing")
	fmt.Fprintln(w, "\nCommands:")
	for _, c := range spaceSubcommands() {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
}

// runSpace dispatches `canaveral space <verb>`, listing when given no verb.
func runSpace(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return runSpaceLs(ctx, nil)
	}
	switch args[0] {
	case "-h", "--help", "help":
		spaceUsage(os.Stdout)
		return nil
	}
	for _, c := range spaceSubcommands() {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	// A bare `canaveral space 3d-printing` is a natural thing to type and
	// unambiguously means the space, so it opens one rather than complaining
	// about a verb that was never required.
	if space.Exists(spaceName(args[0])) {
		return runSpaceOpen(ctx, args)
	}
	spaceUsage(os.Stderr)
	return fmt.Errorf("unknown space command %q", args[0])
}

// spaceName normalises a name the same way feature names are normalised, so
// `canaveral space new "3D Printing"` and `canaveral 3d-printing` agree about
// what was created.
//
// Spaces are flat, unlike features: a "/" would nest the definition file and
// collide with the namespace syntax, which a space cannot take part in
// anyway — there is no sibling worktree to share a skill with. Slug keeps
// any slash it is given, so the name is refused later by space.Path rather
// than quietly flattened into something else.
func spaceName(s string) string { return feature.Slug(s) }

func runSpaceLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("space ls", flag.ContinueOnError)
	names := fs.Bool("names", false, "print only names, one per line (for shell completion)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	list, err := space.List()
	if err != nil {
		return err
	}
	if *names {
		for _, n := range list {
			fmt.Println(n)
		}
		return nil
	}
	if len(list) == 0 {
		dir, _ := space.Dir()
		fmt.Println(dim("no spaces defined"))
		fmt.Printf("  %s canaveral space new 3d-printing\n", dim("create one:"))
		fmt.Printf("  %s %s\n", dim("they live in:"), homeTilde(dir))
		return nil
	}
	for _, n := range list {
		fmt.Printf("%s  %s\n", color(cBold, n), dim(spaceSummary(n)))
	}
	return nil
}

// spaceSummary is the one-line description shown beside a space in the
// listing: whether it is currently up, and what it opens.
//
// A definition that no longer parses is reported rather than skipped. The
// listing is exactly where you would go to find out why opening it failed.
func spaceSummary(name string) string {
	m, err := space.Load(name)
	if err != nil {
		return "broken: " + oneLine(err.Error())
	}
	var parts []string
	if _, ok := spaceRecord(name); ok {
		parts = append(parts, "open")
	}
	if n := len(m.Windows); n > 0 {
		parts = append(parts, fmt.Sprintf("%d window(s)", n))
	}
	if n := len(m.Services); n > 0 {
		parts = append(parts, fmt.Sprintf("%d service(s)", n))
	}
	if n := len(m.Agents); n > 0 {
		parts = append(parts, fmt.Sprintf("%d agent(s)", n))
	}
	return strings.Join(parts, " · ")
}

// spaceRecord returns a space's state record when it is currently up.
//
// A space's project and feature name are the same word, because a space is a
// singleton: there is nothing to have several of.
func spaceRecord(name string) (*state.Feature, bool) {
	f, err := state.Load(name, name)
	if err != nil || !f.Space {
		return nil, false
	}
	return f, true
}

func runSpaceNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("space new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral space new <name> [flags]\n\nWrite a starter definition for a space and, unless told otherwise, open it\nin $EDITOR so you can say what it should contain.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		noEdit = fs.Bool("no-edit", false, "just write the file, do not open an editor")
		open   = fs.Bool("open", false, "open the space once the definition is written")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("specify one name, e.g. `canaveral space new 3d-printing`")
	}
	name := spaceName(pos[0])
	if reserved()[name] {
		return fmt.Errorf("%q is a canaveral command; space names cannot shadow commands", name)
	}

	path, err := space.Create(name)
	if err != nil {
		return err
	}
	r := reporter{}
	r.OK("wrote %s", homeTilde(path))

	if !*noEdit {
		if err := editFile(ctx, path); err != nil {
			// The file is written either way, and saying where it is beats
			// failing over an editor that could not start.
			r.Warn("%v", err)
			r.Info("edit it yourself, then run: canaveral %s", name)
			return nil
		}
	}
	if *open {
		return openSpace(ctx, name, feature.Options{}, false)
	}
	r.Info("open it with: canaveral %s", name)
	return nil
}

// editFile runs $EDITOR (then $VISUAL) on a path, inheriting the terminal so
// a full-screen editor works.
func editFile(ctx context.Context, path string) error {
	ed := os.Getenv("EDITOR")
	if ed == "" {
		ed = os.Getenv("VISUAL")
	}
	if ed == "" {
		return errors.New("$EDITOR is not set")
	}
	// Split so EDITOR="code -w" works; anything more elaborate than flags is
	// a shell command and out of scope.
	fields := strings.Fields(ed)
	cmd := exec.CommandContext(ctx, fields[0], append(fields[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", ed, err)
	}
	return nil
}

func runSpaceOpen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("space open", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral space open <name> [flags]\n\nOpen a space explicitly. Bare `canaveral <name>` does the same thing unless\nthe project you are standing in has a feature by that name too.\n\nFlags:")
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
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("specify one space, e.g. `canaveral space open 3d-printing`")
	}
	opt := feature.Options{NoWindows: *noWindows, NoServices: *noServices, NoAgents: *noAgents}
	return openSpace(ctx, spaceName(pos[0]), opt, *focus && !*noWindows)
}

// openSpace reconciles a space: the repo-less half of openFeature.
//
// It is markedly shorter than its sibling, and everything missing is
// something a repository brought with it — a stash to restore, a slot to
// allocate against sibling features, a worktree to create, a corrupt record
// that must not be mistaken for a free name. A space is a singleton and
// canaveral makes nothing on disk for it, so opening one is only ever
// "reconcile this".
func openSpace(ctx context.Context, name string, opt feature.Options, focus bool) error {
	m, err := space.Load(name)
	if err != nil {
		return err
	}
	r := reporter{}
	r.Step("%s  %s", color(cBold, name), color(cDim, homeTilde(m.Root)))
	res, err := feature.Reconcile(ctx, m, name, opt, r)
	if err != nil {
		return err
	}
	return reportFeature(ctx, res, r, focus)
}

func runSpaceClose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("space close", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral space close <name>\n\nStop a space's services and agents and close its windows. The definition is\nkept, so `canaveral <name>` brings it back.")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("specify one space, e.g. `canaveral space close 3d-printing`")
	}
	return closeSpace(ctx, spaceName(pos[0]), reporter{})
}

// closeSpace tears down a running space, leaving its definition alone.
//
// Remove does the work unchanged: stopping units, closing windows and
// releasing the workspace never had anything to do with git, and the parts
// that do — the merge check, removing the worktree, deleting the branch —
// ask Space first and decline.
func closeSpace(ctx context.Context, name string, r reporter) error {
	f, ok := spaceRecord(name)
	if !ok {
		if space.Exists(name) {
			r.Info("%s is not open", name)
			return nil
		}
		return unknownSpace(name)
	}
	r.Step("closing %s", color(cBold, name))
	return feature.Remove(ctx, f, false, true, false, r)
}

func runSpaceEdit(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("space edit", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("specify one space, e.g. `canaveral space edit 3d-printing`")
	}
	name := spaceName(pos[0])
	if !space.Exists(name) {
		return unknownSpace(name)
	}
	path, err := space.Path(name)
	if err != nil {
		return err
	}
	return editFile(ctx, path)
}

func runSpacePath(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("space path", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		dir, err := space.Dir()
		if err != nil {
			return err
		}
		fmt.Println(dir)
		return nil
	}
	name := spaceName(pos[0])
	if !space.Exists(name) {
		return unknownSpace(name)
	}
	path, err := space.Path(name)
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func runSpaceRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("space rm", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral space rm <name> [flags]\n\nClose a space and delete its definition. This is the only copy of that file;\nto put a space away without losing it, use `canaveral space close`.\n\nFlags:")
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "do not ask for confirmation")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("specify one space, e.g. `canaveral space rm 3d-printing`")
	}
	name := spaceName(pos[0])
	if !space.Exists(name) {
		return unknownSpace(name)
	}
	path, _ := space.Path(name)

	// A definition is something you wrote and canaveral cannot rebuild, and
	// there is no branch or worktree left behind to recover it from — unlike
	// `canaveral rm`, which refuses to delete unmerged work precisely because
	// git is still holding it.
	if !*force && !confirm(fmt.Sprintf("delete %s?", homeTilde(path))) {
		return errors.New("cancelled")
	}

	r := reporter{}
	if _, ok := spaceRecord(name); ok {
		if err := closeSpace(ctx, name, r); err != nil {
			return err
		}
	}
	if err := space.Remove(name); err != nil {
		return err
	}
	r.OK("deleted %s", homeTilde(path))
	return nil
}

// confirm asks a yes/no question, defaulting to no. A non-interactive stdin
// answers no rather than hanging, which is why --force exists.
func confirm(question string) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(os.Stderr, "canaveral: stdin is not a terminal; pass --force to confirm")
		return false
	}
	fmt.Printf("%s [y/N] ", question)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// unknownSpace is the error for a name no space answers to, listing the ones
// that do — the same shape as the unknown-feature error.
func unknownSpace(name string) error {
	list, err := space.List()
	if err != nil || len(list) == 0 {
		return fmt.Errorf("no space named %q; create one with `canaveral space new %s`", name, name)
	}
	sort.Strings(list)
	msg := fmt.Sprintf("no space named %q", name)
	if s := nearest(name, list); s != "" {
		return fmt.Errorf("%s\n  did you mean `canaveral %s`?", msg, s)
	}
	return fmt.Errorf("%s\n  spaces: %s", msg, strings.Join(list, ", "))
}
