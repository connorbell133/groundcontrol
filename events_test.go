package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		filter []string
		tokens []string
		want   bool
	}{
		{"nil filter matches everything", nil, []string{"session.exit"}, true},
		{"empty filter matches everything", []string{}, []string{"job.exit"}, true},
		{"star matches everything", []string{"*"}, []string{"whatever"}, true},
		{"exact match", []string{"session.exit"}, []string{"session.exit"}, true},
		{"exact miss", []string{"job.exit"}, []string{"session.exit"}, false},
		{"prefix wildcard", []string{"session.*"}, []string{"session.exit"}, true},
		{"prefix wildcard miss", []string{"session.*"}, []string{"job.exit"}, false},
		{"derived token exact (alsoMatch)", []string{"session.failed"}, []string{"session.exit", "session.failed"}, true},
		{"derived token via wildcard", []string{"job.*"}, []string{"job.exit", "job.failed"}, true},
		{"suffix star is not a wildcard", []string{"*.exit"}, []string{"session.exit"}, false},
		{"later filter entry still matches", []string{"job.*", "session.exit"}, []string{"session.exit"}, true},
		{"no tokens, no star", []string{"session.exit"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matches(tc.filter, tc.tokens); got != tc.want {
				t.Errorf("matches(%v, %v) = %v, want %v", tc.filter, tc.tokens, got, tc.want)
			}
		})
	}
}

func TestEmitListeners(t *testing.T) {
	t.Parallel()
	a := testApp(t, Config{})

	var got []LifecycleEvent
	a.onEvent(func(LifecycleEvent) { panic("bad subscriber") })
	unsub := a.onEvent(func(e LifecycleEvent) { got = append(got, e) })

	a.emit("session.exit", map[string]any{"k": "v"}, emitOpts{message: "msg"})
	if len(got) != 1 {
		t.Fatalf("expected 1 event past the panicking subscriber, got %d", len(got))
	}
	e := got[0]
	if e.Event != "session.exit" || e.Message != "msg" {
		t.Errorf("unexpected event: %+v", e)
	}
	if e.Title != "session.exit" {
		t.Errorf("empty title should default to the event name, got %q", e.Title)
	}
	if e.At == "" || e.Data["k"] != "v" {
		t.Errorf("event missing at/data: %+v", e)
	}

	unsub()
	a.emit("session.exit", nil, emitOpts{})
	if len(got) != 1 {
		t.Errorf("unsubscribed listener still received events: %d", len(got))
	}
}

func TestWebhookFiltering(t *testing.T) {
	t.Parallel()
	type delivery struct {
		path string
		ev   LifecycleEvent
	}
	ch := make(chan delivery, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev LifecycleEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		ch <- delivery{r.URL.Path, ev}
	}))
	defer srv.Close()

	a := testApp(t, Config{})
	failedOnly := []string{"session.failed"}
	jobsOnly := []string{"job.*"}
	a.configureWebhooks([]WebhookConfig{
		{URL: srv.URL + "/all"}, // no filter: everything
		{URL: srv.URL + "/failed", Events: &failedOnly},
		{URL: srv.URL + "/jobs", Events: &jobsOnly},
	})

	// a failed exit carries the derived session.failed token
	a.emit("session.exit", map[string]any{}, emitOpts{title: "t", alsoMatch: []string{"session.failed"}})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case d := <-ch:
			seen[d.path] = true
			if d.ev.Event != "session.exit" {
				t.Errorf("delivered wrong event %q to %s", d.ev.Event, d.path)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for webhook deliveries; got %v", seen)
		}
	}
	if !seen["/all"] || !seen["/failed"] {
		t.Errorf("expected deliveries to /all and /failed, got %v", seen)
	}
	select {
	case d := <-ch:
		t.Errorf("unexpected extra delivery to %s", d.path)
	case <-time.After(50 * time.Millisecond):
	}
}
