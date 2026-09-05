package discover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
)

// logWith writes a service log and returns its path.
func logWith(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withTimeout(d manifest.Discover, s string) manifest.Discover {
	v, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	d.Timeout.Duration = v
	return d
}

func TestPortsDisabledReturnsNothing(t *testing.T) {
	got, err := Ports(context.Background(), manifest.Discover{}, "", "", nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestPortsFromLog(t *testing.T) {
	// The line canaveral was written against: several mappings on one line,
	// only one of which is wanted per name.
	path := logWith(t, "==> Port mappings: 3050:3000, 3051:3001, 3858:3808\n")
	d := withTimeout(manifest.Discover{Port: map[string]string{
		"web":     `Port mappings: (\d+):3000`,
		"webpack": `Port mappings:.*, (\d+):3808`,
	}}, "2s")

	got, err := Ports(context.Background(), d, path, "", nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 3050 {
		t.Errorf("web = %d, want 3050", got["web"])
	}
	if got["webpack"] != 3858 {
		t.Errorf("webpack = %d, want 3858", got["webpack"])
	}
}

func TestPortsWaitsForLineToAppear(t *testing.T) {
	path := logWith(t, "starting up\n")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "5s")

	// Append the line only after discovery has already begun polling.
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString("listening on 4321\n")
	}()

	got, err := Ports(context.Background(), d, path, "", nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 4321 {
		t.Errorf("web = %d, want 4321", got["web"])
	}
}

func TestPortsTimesOutNamingWhatIsMissing(t *testing.T) {
	path := logWith(t, "==> Port mappings: 3050:3000\n")
	d := withTimeout(manifest.Discover{Port: map[string]string{
		"web":     `Port mappings: (\d+):3000`,
		"webpack": `Port mappings:.*, (\d+):3808`,
	}}, "600ms")

	_, err := Ports(context.Background(), d, path, "", nil)
	if err == nil {
		t.Fatal("Ports succeeded, want timeout")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error is not ErrTimeout: %v", err)
	}
	// The name that never resolved is the whole diagnostic; a bare "timed
	// out" leaves the reader to guess which pattern went stale.
	if !strings.Contains(err.Error(), "webpack") {
		t.Errorf("error does not name the missing port: %v", err)
	}
	if strings.Contains(err.Error(), "web,") {
		t.Errorf("error names a port that was found: %v", err)
	}
}

func TestPortsPartialResultIsNeverReturned(t *testing.T) {
	path := logWith(t, "==> Port mappings: 3050:3000\n")
	d := withTimeout(manifest.Discover{Port: map[string]string{
		"web":     `Port mappings: (\d+):3000`,
		"webpack": `nothing matches this (\d+)`,
	}}, "500ms")

	got, _ := Ports(context.Background(), d, path, "", nil)
	if got != nil {
		t.Errorf("got partial result %v, want nil", got)
	}
}

func TestPortsAbortsWhenProcessDies(t *testing.T) {
	path := logWith(t, "starting up\n")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "10s")

	dead := errors.New("process exited immediately")
	start := time.Now()
	_, err := Ports(context.Background(), d, path, "", func() error { return dead })
	if !errors.Is(err, dead) {
		t.Fatalf("err = %v, want %v", err, dead)
	}
	// Must not sit out the full timeout waiting for a log nothing is writing.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s, want an immediate abort", elapsed)
	}
}

func TestPortsRejectsOutOfRangeValue(t *testing.T) {
	path := logWith(t, "listening on 99999\n")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "500ms")

	_, err := Ports(context.Background(), d, path, "", nil)
	if err == nil {
		t.Fatal("Ports succeeded, want out-of-range error")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want it to mention the range", err)
	}
}

func TestPortsFirstMatchWins(t *testing.T) {
	// A log grows. A later line matching the same pattern must not silently
	// move a port that has already been handed out.
	path := logWith(t, "listening on 4000\nlistening on 5000\n")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "2s")

	got, err := Ports(context.Background(), d, path, "", nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 4000 {
		t.Errorf("web = %d, want 4000 (the first match)", got["web"])
	}
}

func TestPortsFromCmd(t *testing.T) {
	d := withTimeout(manifest.Discover{
		Cmd: "echo '# a comment'; echo web=3050; echo ' webpack = 3858 '",
	}, "2s")

	got, err := Ports(context.Background(), d, "", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 3050 || got["webpack"] != 3858 {
		t.Errorf("got %v, want web=3050 webpack=3858", got)
	}
}

func TestPortsCmdRunsInDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte("7777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := withTimeout(manifest.Discover{Cmd: "echo web=$(cat port)"}, "2s")

	got, err := Ports(context.Background(), d, "", dir, nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 7777 {
		t.Errorf("web = %d, want 7777", got["web"])
	}
}

func TestPortsCmdRetriedUntilItSucceeds(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "ready")
	// Fails until the file appears, which is how a discovery script is
	// expected to say "not yet".
	d := withTimeout(manifest.Discover{
		Cmd: "test -f ready && echo web=8080",
	}, "5s")

	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(stamp, nil, 0o600)
	}()

	got, err := Ports(context.Background(), d, "", dir, nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 8080 {
		t.Errorf("web = %d, want 8080", got["web"])
	}
}

func TestPortsCmdRejectsMalformedOutput(t *testing.T) {
	d := withTimeout(manifest.Discover{Cmd: "echo 'not a pair'"}, "500ms")

	_, err := Ports(context.Background(), d, "", t.TempDir(), nil)
	if err == nil {
		t.Fatal("Ports succeeded, want a parse error")
	}
	if !strings.Contains(err.Error(), "name=port") {
		t.Errorf("error = %v, want it to state the expected format", err)
	}
}

func TestPortsMissingLogIsRetriedNotFatal(t *testing.T) {
	// The unit truncates its log at start, but discovery can begin before
	// the file exists. That is early, not broken.
	path := filepath.Join(t.TempDir(), "not-yet.log")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "5s")

	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(path, []byte("listening on 6060\n"), 0o600)
	}()

	got, err := Ports(context.Background(), d, path, "", nil)
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if got["web"] != 6060 {
		t.Errorf("web = %d, want 6060", got["web"])
	}
}

func TestPortsHonoursCancellation(t *testing.T) {
	path := logWith(t, "nothing here\n")
	d := withTimeout(manifest.Discover{
		Port: map[string]string{"web": `listening on (\d+)`},
	}, "30s")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := Ports(ctx, d, path, "", nil); err == nil {
		t.Fatal("Ports succeeded, want cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want to stop when cancelled", elapsed)
	}
}
