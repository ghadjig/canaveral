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
		case "/session/s/message":
			reads++
			if r.URL.Query().Get("limit") != "16" {
				t.Error("unbounded history request")
			}
			fmt.Fprint(w, `[{"info":{"id":"m","role":"assistant","modelID":"test","time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"done"}]}]`)
		case "/session/status":
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `[]`)
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
				case "/session/s/message":
					fmt.Fprint(w, `[{"parts":[{"text":"`+strings.Repeat("x", maxRecentBytes)+`"}]}]`)
				case "/session/status":
					fmt.Fprint(w, `{"s":{"type":"busy"}}`)
				case "/question":
					if waiting {
						fmt.Fprint(w, `[{"sessionID":"s","questions":[{"header":"Answer me"}]}]`)
					} else {
						fmt.Fprint(w, `[]`)
					}
				default:
					fmt.Fprint(w, `[]`)
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
		if r.URL.Path != "/session/child/message/m" {
			t.Errorf("unexpected history request: %s", r.URL)
		}
		fmt.Fprint(w, `{"info":{"id":"m","time":{"completed":2}}}`)
	}))
	defer srv.Close()
	if toolCallStillOpen(context.Background(), srv.URL, "child", "m", "parent", nil) {
		t.Fatal("completed request should be stale")
	}
}

func TestBoundedProbeTracksGenerationAndCompletion(t *testing.T) {
	phase := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			fmt.Fprint(w, sessList(sess("s", "/target", int64(phase+1), "", 100, 20, 3)))
		case "/session/s/message":
			if r.URL.Query().Get("limit") == "" {
				t.Error("unbounded message request")
			}
			if phase == 0 {
				fmt.Fprint(w, `[{"info":{"id":"m","role":"assistant","modelID":"test","time":{"created":1}},"parts":[{"type":"tool","tool":"bash","state":{"status":"running","input":{"command":"go test ./..."},"time":{"start":1}}}]}]`)
			} else {
				fmt.Fprint(w, `[{"info":{"id":"m","role":"assistant","modelID":"test","time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"All tests passed"}]}]`)
			}
		case "/session/status":
			if phase == 0 {
				fmt.Fprint(w, `{"s":{"type":"busy"}}`)
			} else {
				fmt.Fprint(w, `{}`)
			}
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	c := Conn{URL: srv.URL, Dir: "/target"}
	h := (opencode{}).Probe(context.Background(), c)
	if h.State != StateBusy || h.Activity == nil || h.Activity.Tool != "bash" {
		t.Fatalf("running status lost: %+v", h)
	}
	phase = 1
	invalidateRecent(srv.URL)
	h = (opencode{}).Probe(context.Background(), c)
	if h.State != StateIdle || h.Activity != nil || h.LastAssistant != "All tests passed" || h.Cost != 3 {
		t.Fatalf("completion lost: %+v", h)
	}
}
