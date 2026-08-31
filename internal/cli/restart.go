package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

func runRestart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral restart [feature] <service> [service...]\n\n"+
			"Stop and restart a feature's services, waiting on each one's `ready`\n"+
			"probe before moving on. The log is truncated, so what you see after is\n"+
			"only this run.\n\n"+
			"The feature defaults to whichever worktree you are in, so from inside\n"+
			"it the services are all you need to name. A leading argument that is\n"+
			"not one of the manifest's services is taken as the feature.\n\n"+
			"Services must be named. There is no \"restart everything\": bouncing a\n"+
			"long-running job by accident is worse than typing its name.\n\n"+
			"  canaveral restart web              # this feature's web service\n"+
			"  canaveral restart web jobs\n"+
			"  canaveral restart small-fixes web  # a different feature")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fs.Usage()
		return fmt.Errorf("specify at least one service")
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}

	f, services, err := restartTarget(m, pos)
	if err != nil {
		return err
	}

	r := reporter{}
	r.Step("restart %s", color(cBold, f.Key()))
	return feature.RestartServices(ctx, m, f, services, r)
}

// restartTarget decides whether the first argument names a feature or a
// service, so `restart web` works from inside a worktree while
// `restart small-fixes web` still reaches another feature.
//
// The manifest's service names are a small closed set, which makes this
// decidable rather than a guess — but a feature named after a service would
// satisfy both readings, so that case is refused instead of picked.
func restartTarget(m *manifest.Manifest, pos []string) (*state.Feature, []string, error) {
	isService := false
	for _, s := range m.Services {
		if s.Name == pos[0] {
			isService = true
			break
		}
	}

	named, namedErr := state.Load(m.Name, feature.Slug(pos[0]))
	if isService && namedErr == nil {
		return nil, nil, fmt.Errorf(
			"%q is both a service and a feature; say which you mean:\n"+
				"  canaveral restart %s %s   (that feature's services)\n"+
				"  cd into the feature, then: canaveral restart %s",
			pos[0], pos[0], strings.Join(m.ServiceNames(), " "), pos[0])
	}

	if isService {
		f, err := currentFeature(m)
		if err != nil {
			return nil, nil, err
		}
		return f, pos, nil
	}

	if namedErr != nil {
		// Neither reading worked. Saying only "feature not found" sends people
		// looking for the wrong mistake when they meant a service name.
		declared := "none declared"
		if names := m.ServiceNames(); len(names) > 0 {
			declared = strings.Join(names, ", ")
		}
		return nil, nil, fmt.Errorf("%q is not a service in %s (%s), and %s has no feature by that name",
			pos[0], manifest.FileName, declared, m.Name)
	}
	if len(pos) < 2 {
		return nil, nil, fmt.Errorf("specify at least one service of %s", named.Key())
	}
	return named, pos[1:], nil
}
