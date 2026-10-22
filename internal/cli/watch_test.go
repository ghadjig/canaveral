package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchExecutable(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"renamed over": func(t *testing.T, path string) {
			next := path + ".new"
			if err := os.WriteFile(next, []byte("v2"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, path); err != nil {
				t.Fatal(err)
			}
		},
		"rewritten in place": func(t *testing.T, path string) {
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		},
		"removed": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, replace := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "canaveral")
			if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			replaced := make(chan struct{})
			go watchExecutable(ctx, path, 10*time.Millisecond, func() { close(replaced) })

			select {
			case <-replaced:
				t.Fatal("reported a replacement before anything changed")
			case <-time.After(50 * time.Millisecond):
			}
			replace(t, path)
			select {
			case <-replaced:
			case <-time.After(2 * time.Second):
				t.Fatal("did not notice the binary was replaced")
			}
		})
	}
}
