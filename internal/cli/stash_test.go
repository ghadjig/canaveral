package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/state"
)

func TestPopRetriesPendingActiveRestore(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "named"}[named], func(t *testing.T) {
			clearFeatureEnv(t)
			isolateDirs(t)
			t.Chdir(completeProject(t, "retry-pop"))
			// Fail before startup; reaching this error proves pop selected the
			// pending active record instead of claiming no stash exists.
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			f := &state.Feature{Project: "retry-pop", Name: "pending", RestorePending: true,
				RestoreSessions: map[string]string{"main": "ses_saved"}}
			if err := state.Save(f); err != nil {
				t.Fatal(err)
			}
			var args []string
			if named {
				args = []string{"pending"}
			}
			err := runPop(context.Background(), args)
			if err == nil || strings.Contains(err.Error(), "no stashed") || strings.Contains(err.Error(), "nothing is stashed") {
				t.Fatalf("expected retry startup error, got %v", err)
			}
			saved, err := state.Load(f.Project, f.Name)
			if err != nil || !saved.RestorePending || saved.RestoreSessions["main"] != "ses_saved" {
				t.Fatalf("retry lost metadata: %+v, %v", saved, err)
			}
		})
	}
}
