package watch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/agent"
)

// Explicitly opt in: this probes a running server but never sends a prompt.
func TestLiveTraffic(t *testing.T) {
	base := os.Getenv("CANAVERAL_TEST_WATCH_URL")
	if base == "" {
		t.Skip("set CANAVERAL_TEST_WATCH_URL and CANAVERAL_TEST_WATCH_DIR for live measurements")
	}
	original := http.DefaultTransport
	tr := &trafficTransport{base: original, counts: map[string]int64{}, calls: map[string]int{}}
	http.DefaultTransport = tr
	defer func() { http.DefaultTransport = original }()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if os.Getenv("CANAVERAL_TEST_WATCH_PROJECT") == "" {
		h := agent.Probe(ctx, "opencode", agent.Conn{URL: base, Dir: os.Getenv("CANAVERAL_TEST_WATCH_DIR")})
		t.Logf("reachable=%v state=%s session=%s model=%s tokens=%d error=%v", h.Reachable, h.State, h.SessionID, h.Model, h.Tokens.Total(), h.Err)
	} else {
		r := NewRunner(Options{Project: os.Getenv("CANAVERAL_TEST_WATCH_PROJECT")})
		if err := r.Run(ctx, &liveStates{t: t}); err != nil {
			t.Fatal(err)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for path, n := range tr.calls {
		t.Logf("GET %s calls=%d bytes=%d", path, n, tr.counts[path])
	}
}

type liveStates struct{ t *testing.T }

func (w *liveStates) Write(b []byte) (int, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, err
	}
	for _, f := range s.Features {
		for _, a := range f.Agents {
			w.t.Logf("snapshot feature=%s agent=%s status=%s model=%s tokens=%d", f.Key, a.Name, a.Status, a.Model, a.Tokens)
		}
	}
	return len(b), nil
}

type trafficTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	counts map[string]int64
	calls  map[string]int
}

func (tr *trafficTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Host + req.URL.RequestURI()
	tr.mu.Lock()
	tr.calls[path]++
	tr.mu.Unlock()
	resp, err := tr.base.RoundTrip(req)
	if err == nil {
		resp.Body = &trafficBody{ReadCloser: resp.Body, tr: tr, path: path}
	}
	return resp, err
}

type trafficBody struct {
	io.ReadCloser
	tr   *trafficTransport
	path string
}

func (b *trafficBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.tr.mu.Lock()
	b.tr.counts[b.path] += int64(n)
	b.tr.mu.Unlock()
	return n, err
}
