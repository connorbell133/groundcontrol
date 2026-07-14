package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

func (a *app) configureWebhooks(cfg []WebhookConfig) {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	if cfg == nil {
		cfg = []WebhookConfig{}
	}
	a.webhooks = cfg
}

func (a *app) onEvent(fn func(LifecycleEvent)) func() {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	a.nextListenerID++
	id := a.nextListenerID
	a.eventListeners = append(a.eventListeners, eventListener{id: id, fn: fn})
	return func() {
		a.eventsMu.Lock()
		defer a.eventsMu.Unlock()
		for i, l := range a.eventListeners {
			if l.id == id {
				a.eventListeners = append(a.eventListeners[:i], a.eventListeners[i+1:]...)
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

func (a *app) emit(event string, data map[string]any, opts emitOpts) {
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
	a.eventsMu.Lock()
	ls := make([]eventListener, len(a.eventListeners))
	copy(ls, a.eventListeners)
	hooks := make([]WebhookConfig, len(a.webhooks))
	copy(hooks, a.webhooks)
	a.eventsMu.Unlock()
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
			a.deliverWebhook(hook.URL, e)
		}
	}
}

// Fire-and-forget POST: same contract as the old ntfy path — bounded, and a
// hanging endpoint must never delay lifecycle handling. Failures are journaled
// so silent drops stay diagnosable.
func (a *app) deliverWebhook(url string, payload LifecycleEvent) {
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
			a.journal(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		req.Header.Set("content-type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			a.journal(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			a.journal(map[string]any{"event": "webhook.failed", "url": url, "status": res.StatusCode, "for": payload.Event})
		}
	}()
}
