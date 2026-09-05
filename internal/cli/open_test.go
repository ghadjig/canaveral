package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

func TestCheckFeatureExistence(t *testing.T) {
	corrupt := errors.New("corrupt state file")
	notFound := state.ErrNotFound

	cases := []struct {
		name    string
		loadErr error
		create  bool
		wantErr bool
		wantIs  error // errors.Is target, if non-nil
	}{
		{"corrupt state file always errors", corrupt, true, true, corrupt},
		{"corrupt state file always errors (reconcile)", corrupt, false, true, corrupt},
		{"new refuses an existing feature", nil, true, true, nil},
		{"reconcile refuses an unknown feature", notFound, false, true, nil},
		{"new is fine with a free name", notFound, true, false, nil},
		{"reconcile is fine with an existing feature", nil, false, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkFeatureExistence(c.loadErr, c.create, "norules", "small-fixes")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Errorf("err = %v, want it to wrap %v", err, c.wantIs)
			}
		})
	}
}

func TestCheckFeatureExistenceAlreadyExistsMentionsHowToBringItUpToDate(t *testing.T) {
	err := checkFeatureExistence(nil, true, "norules", "small-fixes")
	if err == nil || !strings.Contains(err.Error(), "canaveral small-fixes") {
		t.Errorf("err = %v, want it to point at reconciling the existing feature", err)
	}
}

func TestCheckFeatureExistenceUnknownFeatureOffersToCreateIt(t *testing.T) {
	err := checkFeatureExistence(state.ErrNotFound, false, "norules", "small-fixes")
	if err == nil || !strings.Contains(err.Error(), "canaveral new small-fixes") {
		t.Errorf("err = %v, want it to point at creating the feature", err)
	}
}

func TestOpenFeatureRequiresAFeatureName(t *testing.T) {
	if err := openFeature(context.Background(), "new", nil, true); err == nil {
		t.Error("openFeature(create) with no args should require a name")
	}
	if err := openFeature(context.Background(), "open", nil, false); err == nil {
		t.Error("openFeature(reconcile) with no args should require a name")
	}
}

func TestOpenFeatureRejectsMultipleNames(t *testing.T) {
	err := openFeature(context.Background(), "new", []string{"a", "b"}, true)
	if err == nil || !strings.Contains(err.Error(), "expected one feature name") {
		t.Errorf("err = %v, want a one-name-expected error", err)
	}
}

func TestOpenFeatureRejectsAReservedName(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "open-reserved"))

	err := openFeature(context.Background(), "new", []string{"status"}, true)
	if err == nil || !strings.Contains(err.Error(), "canaveral command") {
		t.Errorf("err = %v, want a reserved-name error", err)
	}
}

// TestStarterTemplateParsesForEveryTool guards the starter manifest against
// the one failure that would matter most: `canaveral init` writing a file
// that `canaveral new` then refuses to load. The agent block is generated
// from the harness, so a new tool gets a manifest nobody hand-checked.
func TestStarterTemplateParsesForEveryTool(t *testing.T) {
	for _, tool := range agent.Tools() {
		t.Run(tool, func(t *testing.T) {
			dir := t.TempDir()
			body := fmt.Sprintf(starterTemplate, "demo", "", "", detectService(dir), starterAgent(tool))
			if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			m, err := manifest.Load(dir)
			if err != nil {
				t.Fatalf("the starter manifest for %s does not load: %v\n%s", tool, err, body)
			}
			if len(m.Agents) != 1 || m.Agents[0].Tool != tool {
				t.Fatalf("agents = %+v, want a single %s agent", m.Agents, tool)
			}
			// The window must actually open the agent, or `init` produces a
			// project whose agent nothing ever attaches to.
			var run string
			for _, w := range m.Windows {
				if w.Name == tool && w.Run != nil {
					run = *w.Run
				}
			}
			if !strings.Contains(run, "{{.Agent.main.Session}}") {
				t.Errorf("window %q run = %q, want it to splice in the session flags", tool, run)
			}
		})
	}
}
