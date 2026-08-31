package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bandito/canaveral/internal/registry"
)

func runProjects(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral projects [flags]\n\nList the projects canaveral knows about, and where they live. Projects\nregister themselves the first time any command runs inside them, so this is\nnormally read-only; the flags are for repairing it.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		add    = fs.String("add", "", "register the project checkout at `path`")
		forget = fs.String("forget", "", "drop `project` from the registry (the checkout is untouched)")
		scan   = fs.String("scan", "", "walk `dir` for project checkouts and register each one")
		prune  = fs.Bool("prune", false, "drop entries whose checkout no longer exists")
		asJSON = fs.Bool("json", false, "print the registry as JSON")
		names  = fs.Bool("names", false, "print only project names, one per line")
	)
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	r := reporter{}
	switch {
	case *add != "":
		p, err := registry.Add(*add)
		if err != nil {
			return err
		}
		r.OK("registered %s  %s", color(cBold, p.Name), color(cDim, homeTilde(p.Root)))
		return nil
	case *forget != "":
		found, err := registry.Forget(*forget)
		if err != nil {
			return err
		}
		if !found {
			// Saying "not found" would be a lie: the name is still listable,
			// because it was never in the file to begin with.
			r.Warn("%s was not in the registry (it is derived from its feature state, and will keep appearing until those features are removed)", *forget)
			return nil
		}
		r.OK("forgot %s", *forget)
		return nil
	case *scan != "":
		found, conflicts, err := registry.Scan(*scan)
		if err != nil {
			return err
		}
		for _, p := range found {
			r.OK("%s  %s", color(cBold, p.Name), color(cDim, homeTilde(p.Root)))
		}
		for _, c := range conflicts {
			r.Warn("%v", c)
		}
		if len(found) == 0 && len(conflicts) == 0 {
			r.Info("no project checkouts found under %s", homeTilde(*scan))
		}
		return nil
	case *prune:
		dropped, err := registry.Prune()
		if err != nil {
			return err
		}
		if len(dropped) == 0 {
			r.OK("nothing to prune")
			return nil
		}
		for _, p := range dropped {
			r.OK("dropped %s (%s is gone)", p.Name, homeTilde(p.Root))
		}
		return nil
	}

	projects, err := registry.MRU()
	if err != nil {
		return err
	}
	if *names {
		for _, p := range projects {
			fmt.Println(p.Name)
		}
		return nil
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if projects == nil {
			projects = []registry.Project{}
		}
		return enc.Encode(projects)
	}
	if len(projects) == 0 {
		fmt.Println("no projects registered yet — run any canaveral command inside one, or `canaveral projects --scan ~/code`")
		return nil
	}

	// Plain cells only: tabwriter measures raw byte length, so colouring some
	// cells and not others inflates the computed column width.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tROOT\tLAST USED")
	for _, p := range projects {
		root := homeTilde(p.Root)
		if !p.Alive() {
			root += "  (missing)"
		}
		used := "-"
		if !p.LastUsed.IsZero() {
			used = humanAgo(p.LastUsed)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, root, used)
	}
	return tw.Flush()
}
