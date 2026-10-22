package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeBoundsAndCachesTranscript(t *testing.T) {
	reads := 0
	updated := int64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if r.URL.Query().Get("directory") != "/target" {
				t.Error("session list must be directory scoped")
			}
			fmt.Fprint(w, sessList(sess("s", "/target", updated, "", 20, 10, 5)))
		case "/api/session/s/message":
			reads++
			if r.URL.Query().Get("limit") != "16" {
				t.Error("unbounded history request")
			}
			if r.URL.Query().Get("order") != "desc" {
				t.Error("history must be read newest first, or a limit keeps the oldest messages")
			}
			fmt.Fprint(w, msgList(`{"id":"m","type":"assistant","model":{"id":"test","providerID":"p"},"time":{"created":1,"completed":2},"content":[{"type":"text","text":"done"}]}`))
		case "/api/session/active":
			fmt.Fprint(w, running())
		default:
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer srv.Close()
	c := Conn{URL: srv.URL, Dir: "/target"}
	for range 3 {
		h := (opencode{}).Probe(context.Background(), c)
		if h.State != StateIdle || h.Model != "test" || h.Cost != 5 || h.LastAssistant != "done" {
			t.Fatalf("bad cached health: %+v", h)
		}
	}
	if reads != 1 {
		t.Fatalf("unchanged session read history %d times", reads)
	}
	updated++
	_ = (opencode{}).Probe(context.Background(), c)
	if reads != 2 {
		t.Fatal("changed session did not invalidate cached history")
	}
	invalidateRecent(srv.URL)
	_ = (opencode{}).Probe(context.Background(), c)
	if reads != 3 {
		t.Fatal("event did not invalidate cached history")
	}
}

func TestOversizedHistoryStillReportsBusyAndWaiting(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprint(waiting), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/session":
					fmt.Fprint(w, sessList(sess("s", "/target", 1, "", 1, 1, 1)))
				case "/api/session/s/message":
					fmt.Fprint(w, `{"data":[{"type":"user","text":"`+strings.Repeat("x", maxRecentBytes)+`"}]}`)
				case "/api/session/active":
					fmt.Fprint(w, running("s"))
				case "/api/session/s/form":
					if waiting {
						fmt.Fprint(w, `{"data":`+list(form("s", "", "Answer me", "?"))+`}`)
					} else {
						fmt.Fprint(w, `{"data":[]}`)
					}
				default:
					fmt.Fprint(w, `{"data":[]}`)
				}
			}))
			defer srv.Close()
			h := (opencode{}).Probe(context.Background(), Conn{URL: srv.URL, Dir: "/target"})
			want := StateBusy
			if waiting {
				want = StateWaiting
			}
			if h.State != want {
				t.Fatalf("state = %s, want %s", h.State, want)
			}
		})
	}
}

func TestPendingSubagentFetchesOnlyReferencedMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/child/message/m" {
			t.Errorf("unexpected history request: %s", r.URL)
		}
		fmt.Fprint(w, `{"data":{"id":"m","type":"assistant","time":{"created":1,"completed":2}}}`)
	}))
	defer srv.Close()
	if toolCallStillOpen(context.Background(), Conn{URL: srv.URL}, "child", "m", "parent", nil) {
		t.Fatal("completed request should be stale")
	}
}

func TestBoundedProbeTracksGenerationAndCompletion(t *testing.T) {
	phase := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			fmt.Fprint(w, sessList(sess("s", "/target", int64(phase+1), "", 100, 20, 3)))
		case "/api/session/s/message":
			if r.URL.Query().Get("limit") == "" {
				t.Error("unbounded message request")
			}
			if phase == 0 {
				fmt.Fprint(w, msgList(`{"id":"m","type":"assistant","model":{"id":"test","providerID":"p"},"time":{"created":1},"content":[{"type":"tool","id":"c","name":"shell","state":{"status":"running","input":{"command":"go test ./..."},"metadata":{}},"time":{"created":1,"ran":1}}]}`))
			} else {
				fmt.Fprint(w, msgList(
					`{"id":"m","type":"assistant","model":{"id":"test","providerID":"p"},"time":{"created":1,"completed":2},"content":[{"type":"text","text":"All tests passed"}]}`,
					`{"id":"i","type":"idle","time":{"created":3},"outcome":"succeeded"}`,
				))
			}
		case "/api/session/active":
			if phase == 0 {
				fmt.Fprint(w, running("s"))
			} else {
				fmt.Fprint(w, running())
			}
		default:
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer srv.Close()
	c := Conn{URL: srv.URL, Dir: "/target"}
	h := (opencode{}).Probe(context.Background(), c)
	if h.State != StateBusy || h.Activity == nil || h.Activity.Tool != "shell" {
		t.Fatalf("running status lost: %+v", h)
	}
	phase = 1
	invalidateRecent(srv.URL)
	h = (opencode{}).Probe(context.Background(), c)
	if h.State != StateIdle || h.Activity != nil || h.LastAssistant != "All tests passed" || h.Cost != 3 {
		t.Fatalf("completion lost: %+v", h)
	}
}
