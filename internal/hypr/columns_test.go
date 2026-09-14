package hypr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func columnCompositor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "batch")
	script := `#!/bin/sh
case "$1" in
  clients) printf '%s' '[{"address":"0xa","workspace":{"name":"p:new"}},{"address":"0xb","workspace":{"name":"p:new"}},{"address":"0xc","workspace":{"name":"p:new"}}]' ;;
  getoption) printf '%s' "$COLUMN_OPTION" ;;
  monitors) printf '%s' '[{"name":"DP-1","focused":false,"activeWorkspace":{"name":"2"}},{"name":"DP-2","focused":true,"activeWorkspace":{"name":"current"}}]' ;;
  workspaces) printf '%s' '[{"name":"p:new","monitor":"DP-1"}]' ;;
  activewindow) printf '%s' "$COLUMN_ACTIVE" ;;
  --batch) printf '%s' "$2" > "$COLUMN_LOG"; printf '%s' "${COLUMN_REPLY:-ok}" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("COLUMN_LOG", log)
	t.Setenv("COLUMN_OPTION", `{"int":0}`)
	t.Setenv("COLUMN_ACTIVE", `{"address":"0xff"}`)
	t.Setenv("COLUMN_REPLY", "ok")
	return log
}

func TestColumnsBatchRestoresFocusBeforeEnablingWarps(t *testing.T) {
	log := columnCompositor(t)
	if err := ApplyColumns(context.Background(), []string{"0xa", "0xb", "0xc"}, []float64{0.8, 1}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	batch := string(b)
	// Assert ordering across the whole transaction, not just the existence
	// of a restore somewhere: the cursor must stay put until focus is back.
	ordered := []string{
		"keyword cursor:no_warps 1",
		"dispatch focuswindow address:0xa",
		"dispatch setfloating address:0xa",
		"dispatch setfloating address:0xb",
		"dispatch setfloating address:0xc",
		"dispatch settiled address:0xa",
		"dispatch focuswindow address:0xa",
		"dispatch layoutmsg preselect r",
		"dispatch settiled address:0xb",
		"dispatch layoutmsg splitratio 0.8000 exact",
		"dispatch focuswindow address:0xb",
		"dispatch layoutmsg preselect r",
		"dispatch settiled address:0xc",
		"dispatch layoutmsg splitratio 1.0000 exact",
		"dispatch layoutmsg preselect none",
		"dispatch focusmonitor DP-1",
		"dispatch workspace 2",
		"dispatch focusmonitor DP-2",
		"dispatch focuswindow address:0xff",
		"keyword dwindle:use_active_for_splits 0",
		"keyword cursor:no_warps 0",
	}
	rest := batch
	for _, want := range ordered {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing/out of order %q in batch:\n%s", want, batch)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(batch, "movecursor") || strings.Contains(batch, "moveworkspacetomonitor") {
		t.Fatalf("layout must not move the cursor or borrow a monitor: %s", batch)
	}
}

func TestColumnsPreservesEnabledOptionsAndEmptyWorkspaceFocus(t *testing.T) {
	log := columnCompositor(t)
	t.Setenv("COLUMN_OPTION", `{"int":1}`)
	t.Setenv("COLUMN_ACTIVE", `{}`)
	if err := ApplyColumns(context.Background(), []string{"0xa", "0xb"}, []float64{1}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.HasSuffix(string(b), "keyword cursor:no_warps 1") || strings.Contains(string(b), "address:0xff") {
		t.Fatalf("did not preserve preferences/empty workspace: %s", b)
	}
}

func TestColumnsFailureBeforeBatchDoesNotTouchFocus(t *testing.T) {
	for _, mode := range []string{"invalid-option", "cancelled", "missing-window", "invalid-address"} {
		t.Run(mode, func(t *testing.T) {
			log := columnCompositor(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			addresses := []string{"0xa", "0xb"}
			switch mode {
			case "invalid-option":
				t.Setenv("COLUMN_OPTION", `{}`)
			case "cancelled":
				cancel()
			case "missing-window":
				addresses[1] = "0xd"
			case "invalid-address":
				addresses[1] = "0xb; dispatch exit"
			}
			if err := ApplyColumns(ctx, addresses, []float64{1}); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("failed preflight submitted a focus-changing batch")
			}
		})
	}
}

func TestColumnsReportsDispatcherErrorsWithCleanupInSameBatch(t *testing.T) {
	log := columnCompositor(t)
	t.Setenv("COLUMN_REPLY", "ok\nWindow not found\nok\n")
	err := ApplyColumns(context.Background(), []string{"0xa", "0xb"}, []float64{1})
	if err == nil || !strings.Contains(err.Error(), "Window not found") {
		t.Fatalf("dispatcher error = %v", err)
	}
	b, _ := os.ReadFile(log)
	if !strings.HasSuffix(string(b), "keyword cursor:no_warps 0") {
		t.Fatalf("cleanup missing from failed batch: %s", b)
	}
}
