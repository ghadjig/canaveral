package hypr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpawnInstallsClassPlacementBeforeExec(t *testing.T) {
	for _, mode := range []string{"modern", "legacy", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$RULE_LOG"
case "$1" in
  keyword)
    if [ "$RULE_MODE" = rejected ] || { [ "$RULE_MODE" = legacy ] && [ "$2" = windowrule ]; }; then
      printf 'invalid rule syntax'
    else
      printf ok
    fi ;;
  dispatch) printf ok ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("RULE_LOG", log)
			t.Setenv("RULE_MODE", mode)
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			err := Spawn(context.Background(), SpawnSpec{Class: "canaveral-p-f-chrome", Workspace: "p:f", Cmd: "true", Env: map[string]string{"A": "1"}})
			b, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			calls := string(b)
			if mode == "rejected" {
				if err == nil || strings.Contains(calls, "dispatch exec") {
					t.Fatalf("rejected rules must prevent spawn: %v, %s", err, calls)
				}
				entries, _ := os.ReadDir(runtimeDir())
				if len(entries) != 0 {
					t.Fatal("rule failure leaked environment file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "keyword windowrule match:initial_class ^canaveral-p-f-chrome$, workspace name:p:f silent, no_initial_focus on"
			if mode == "legacy" {
				want = "keyword windowrulev2 workspace name:p:f silent,initialclass:^canaveral-p-f-chrome$"
			}
			if i := strings.Index(calls, want); i < 0 || i > strings.Index(calls, "dispatch exec") {
				t.Fatalf("placement rule must precede exec: %s", calls)
			}
		})
	}
}
