package ocevents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseServer streams the given frames, then holds the connection open until
// the client disconnects (or closes immediately if hold is false).
func sseServer(t *testing.T, hold bool, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if fl != nil {
				fl.Flush()
			}
		}
		if hold {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSubscribeDecodesEvents(t *testing.T) {
	srv := sseServer(t, false,
		"data: {\"id\":\"evt_1\",\"type\":\"server.connected\",\"properties\":{}}\n\n",
		"data: {\"id\":\"evt_2\",\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_a\"}}\n\n",
	)

	var got []Event
	err := Subscribe(context.Background(), srv.URL, func(e Event) { got = append(got, e) })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Type != "server.connected" || got[1].Type != "session.idle" {
		t.Errorf("types = %q, %q", got[0].Type, got[1].Type)
	}
	var props struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(got[1].Payload(), &props); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if props.SessionID != "ses_a" {
		t.Errorf("sessionID = %q, want ses_a", props.SessionID)
	}
}

func TestPayloadPrefersDataForV2Envelope(t *testing.T) {
	// ".v2" events carry their payload under "data" rather than
	// "properties"; both shapes must decode through the same accessor.
	srv := sseServer(t, false,
		"data: {\"id\":\"evt_1\",\"type\":\"question.v2.asked\",\"data\":{\"id\":\"que_1\"}}\n\n",
	)
	var got []Event
	if err := Subscribe(context.Background(), srv.URL, func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(got[0].Payload(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ID != "que_1" {
		t.Errorf("id = %q, want que_1", p.ID)
	}
}

func TestSubscribeSkipsUnparseableFrames(t *testing.T) {
	// Version skew must not tear the stream down: a frame we cannot decode
	// is skipped, and the following good one still arrives.
	srv := sseServer(t, false,
		"data: {not json at all}\n\n",
		"data: {\"id\":\"evt_2\",\"type\":\"session.idle\"}\n\n",
	)
	var got []Event
	if err := Subscribe(context.Background(), srv.URL, func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 1 || got[0].Type != "session.idle" {
		t.Errorf("got %+v, want just the parseable event", got)
	}
}

func TestSubscribeIgnoresNonDataLines(t *testing.T) {
	srv := sseServer(t, false,
		": this is an SSE comment\n",
		"event: message\n",
		"data: {\"id\":\"evt_1\",\"type\":\"session.idle\"}\n\n",
	)
	var got []Event
	if err := Subscribe(context.Background(), srv.URL, func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d events, want 1", len(got))
	}
}

func TestSubscribeHandlesLinesOverTheDefaultScannerLimit(t *testing.T) {
	// Tool output rides along in some events; a single frame larger than
	// bufio's default 64KB must not end the stream.
	big := strings.Repeat("x", 200_000)
	srv := sseServer(t, false,
		"data: {\"id\":\"evt_1\",\"type\":\"message.updated\",\"properties\":{\"blob\":\""+big+"\"}}\n\n",
	)
	var got []Event
	if err := Subscribe(context.Background(), srv.URL, func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (oversized line dropped?)", len(got))
	}
}

func TestSubscribeErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := Subscribe(context.Background(), srv.URL, func(Event) {}); err == nil {
		t.Error("want an error for a non-200 response")
	}
}

func TestSubscribeErrorsWhenServerIsDown(t *testing.T) {
	if err := Subscribe(context.Background(), "http://127.0.0.1:1", func(Event) {}); err == nil {
		t.Error("want an error for an unreachable server")
	}
}

func TestSubscribeStopsOnContextCancel(t *testing.T) {
	srv := sseServer(t, true, "data: {\"id\":\"evt_1\",\"type\":\"server.connected\"}\n\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Subscribe(ctx, srv.URL, func(Event) {}) }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after context cancellation")
	}
}

func TestWatchReconnectsAfterTheStreamDrops(t *testing.T) {
	// An agent's server restarts whenever its feature is reset; the watcher
	// has to come back rather than going quiet for the rest of the session.
	var mu sync.Mutex
	connections := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections++
		n := connections
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"id\":\"evt_%d\",\"type\":\"server.connected\"}\n\n", n)
		// Return immediately, closing the stream and forcing a reconnect.
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen sync.WaitGroup
	seen.Add(2)
	count := 0
	go Watch(ctx, srv.URL, func(Event) {
		mu.Lock()
		count++
		c := count
		mu.Unlock()
		if c <= 2 {
			seen.Done()
		}
	})

	done := make(chan struct{})
	go func() { seen.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		mu.Lock()
		c := connections
		mu.Unlock()
		t.Fatalf("Watch did not reconnect; only %d connection(s) made", c)
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	srv := sseServer(t, true, "data: {\"id\":\"evt_1\",\"type\":\"server.connected\"}\n\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, srv.URL, func(Event) {}) }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}
