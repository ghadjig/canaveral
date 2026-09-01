package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

func TestRestartTargetTreatsKnownServiceAsService(t *testing.T) {
	// No feature state exists, so a named feature cannot resolve; the point
	// is that "web" is recognised as a service and never tried as a feature.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")

	m := &manifest.Manifest{
		Name:     "norules",
		Services: []manifest.Service{{Name: "web"}, {Name: "jobs"}},
	}
	_, _, err := restartTarget(m, []string{"web", "jobs"})
	// currentFeature fails here (the test is not inside a worktree), which is
	// itself proof the service branch was taken rather than the feature one.
	if err == nil || !strings.Contains(err.Error(), "not inside a feature worktree") {
		t.Fatalf("err = %v, want the current-feature lookup to have been attempted", err)
	}
}

func TestRestartTargetUnknownFirstArgIsAFeature(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")

	m := &manifest.Manifest{
		Name:     "norules",
		Services: []manifest.Service{{Name: "web"}},
	}
	f := &state.Feature{Project: "norules", Name: "small-fixes", Slot: 0}
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}

	got, services, err := restartTarget(m, []string{"small-fixes", "web"})
	if err != nil {
		t.Fatalf("restartTarget: %v", err)
	}
	if got.Name != "small-fixes" {
		t.Errorf("feature = %q, want small-fixes", got.Name)
	}
	if len(services) != 1 || services[0] != "web" {
		t.Errorf("services = %v, want [web]", services)
	}

	// A feature with no services named is a mistake, not "restart everything".
	if _, _, err := restartTarget(m, []string{"small-fixes"}); err == nil {
		t.Error("naming only a feature succeeded, want an error")
	}
}

func TestRestartTargetRefusesAmbiguousName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")

	m := &manifest.Manifest{
		Name:     "norules",
		Services: []manifest.Service{{Name: "web"}},
	}
	// A feature called "web" satisfies both readings. Guessing either way
	// would be wrong half the time, so it has to be refused.
	if err := state.Save(&state.Feature{Project: "norules", Name: "web"}); err != nil {
		t.Fatal(err)
	}

	_, _, err := restartTarget(m, []string{"web"})
	if err == nil || !strings.Contains(err.Error(), "both a service and a feature") {
		t.Fatalf("err = %v, want an ambiguity error", err)
	}
}

func TestRunRestartRequiresAtLeastOneService(t *testing.T) {
	err := runRestart(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "specify at least one service") {
		t.Errorf("err = %v, want a specify-a-service error", err)
	}
}

func TestRunRestartPropagatesAnUnresolvableTarget(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "restart-unresolvable"))

	// "does-not-exist" is neither a declared service (completeProject's
	// manifest only has "web") nor an existing feature, so runRestart must
	// surface restartTarget's error rather than reaching RestartServices.
	err := runRestart(context.Background(), []string{"does-not-exist"})
	if err == nil {
		t.Error("runRestart should fail for a name that is neither a service nor a feature")
	}
}
