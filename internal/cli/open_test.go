package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// The starter manifest has to survive being loaded back. It did not: the
// chrome window's exec command omitted {{.Class}}, which manifest validation
// requires, so `canaveral init` followed by any other command failed on the
// file canaveral had just written itself.
func TestStarterTemplateLoads(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(starterTemplate, "demo", `"node_modules"`, `".env"`, "")
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(dir); err != nil {
		t.Fatalf("starter manifest does not load: %v", err)
	}
}
