package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// One notification mechanism: lifecycle events fan out to an in-process bus
// (SSE, wait=ready) and to configured webhook subscribers. ntfy, n8n, or
// anything that accepts a JSON POST catches them — no receiver is special.
type LifecycleEvent struct {
	Event   string         `json:"event"`
	At      string         `json:"at"`
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

type WebhookConfig struct {
	URL string `json:"url"`
	// match tokens; omitted or ["*"] = everything. Pointer so a configured
	// "events": [] round-trips as [] while an absent key stays absent, like the TS.
	Events *[]string `json:"events,omitempty"`
}

type eventListener struct {
	id int
	fn func(LifecycleEvent)
}

var (
	eventsMu       sync.Mutex
	webhooks       []WebhookConfig
	eventListeners []eventListener
	nextListenerID int
)

func configureWebhooks(cfg []WebhookConfig) {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if cfg == nil {
		cfg = []WebhookConfig{}
	}
	webhooks = cfg
}

func onEvent(fn func(LifecycleEvent)) func() {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	nextListenerID++
	id := nextListenerID
	eventListeners = append(eventListeners, eventListener{id: id, fn: fn})
	return func() {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		for i, l := range eventListeners {
			if l.id == id {
				eventListeners = append(eventListeners[:i], eventListeners[i+1:]...)
				break
			}
		}
	}
}

// A subscriber's filter matches the event name, a prefix wildcard ("session.*"),
// "*", or a derived token (a failed exit also matches "session.failed"/"job.failed").
func matches(filter []string, tokens []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if f == "*" {
			return true
		}
		for _, t := range tokens {
			if t == f {
				return true
			}
		}
		if strings.HasSuffix(f, ".*") {
			prefix := f[:len(f)-1]
			for _, t := range tokens {
				if strings.HasPrefix(t, prefix) {
					return true
				}
			}
		}
	}
	return false
}

type emitOpts struct {
	title, message string
	alsoMatch      []string
}

func emit(event string, data map[string]any, opts emitOpts) {
	title := opts.title
	if title == "" {
		title = event
	}
	e := LifecycleEvent{
		Event:   event,
		At:      nowISO(),
		Title:   title,
		Message: opts.message,
		Data:    data,
	}
	eventsMu.Lock()
	ls := make([]eventListener, len(eventListeners))
	copy(ls, eventListeners)
	hooks := make([]WebhookConfig, len(webhooks))
	copy(hooks, webhooks)
	eventsMu.Unlock()
	for _, l := range ls {
		func() {
			// one bad subscriber must not break the others
			defer func() { _ = recover() }()
			l.fn(e)
		}()
	}
	tokens := append([]string{event}, opts.alsoMatch...)
	for _, hook := range hooks {
		var filter []string
		if hook.Events != nil {
			filter = *hook.Events
		}
		if matches(filter, tokens) {
			deliverWebhook(hook.URL, e)
		}
	}
}

// Fire-and-forget POST: same contract as the old ntfy path — bounded, and a
// hanging endpoint must never delay lifecycle handling. Failures are journaled
// so silent drops stay diagnosable.
func deliverWebhook(url string, payload LifecycleEvent) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			// invalid URL slipped through validation — journal so the drop stays diagnosable
			journal(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		req.Header.Set("content-type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			journal(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			journal(map[string]any{"event": "webhook.failed", "url": url, "status": res.StatusCode, "for": payload.Event})
		}
	}()
}
