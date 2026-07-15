// Package events is the one notification mechanism: lifecycle events fan out
// to an in-process bus (SSE, wait=ready) and to configured webhook subscribers.
// ntfy, n8n, or anything that accepts a JSON POST catches them — no receiver
// is special.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/util"
)

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

type listener struct {
	id int
	fn func(LifecycleEvent)
}

// Bus fans lifecycle events out to in-process listeners and webhooks. Webhook
// delivery failures are journaled so silent drops stay diagnosable.
type Bus struct {
	mu             sync.Mutex
	webhooks       []WebhookConfig
	listeners      []listener
	nextListenerID int
	journal        *journal.Journal
}

func NewBus(j *journal.Journal) *Bus {
	return &Bus{journal: j}
}

func (b *Bus) ConfigureWebhooks(cfg []WebhookConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cfg == nil {
		cfg = []WebhookConfig{}
	}
	b.webhooks = cfg
}

func (b *Bus) OnEvent(fn func(LifecycleEvent)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextListenerID++
	id := b.nextListenerID
	b.listeners = append(b.listeners, listener{id: id, fn: fn})
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, l := range b.listeners {
			if l.id == id {
				b.listeners = append(b.listeners[:i], b.listeners[i+1:]...)
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

type EmitOpts struct {
	Title, Message string
	AlsoMatch      []string
}

func (b *Bus) Emit(event string, data map[string]any, opts EmitOpts) {
	title := opts.Title
	if title == "" {
		title = event
	}
	e := LifecycleEvent{
		Event:   event,
		At:      util.NowISO(),
		Title:   title,
		Message: opts.Message,
		Data:    data,
	}
	b.mu.Lock()
	ls := make([]listener, len(b.listeners))
	copy(ls, b.listeners)
	hooks := make([]WebhookConfig, len(b.webhooks))
	copy(hooks, b.webhooks)
	b.mu.Unlock()
	for _, l := range ls {
		func() {
			// one bad subscriber must not break the others
			defer func() { _ = recover() }()
			l.fn(e)
		}()
	}
	tokens := append([]string{event}, opts.AlsoMatch...)
	for _, hook := range hooks {
		var filter []string
		if hook.Events != nil {
			filter = *hook.Events
		}
		if matches(filter, tokens) {
			b.DeliverWebhook(hook.URL, e)
		}
	}
}

// DeliverWebhook is a fire-and-forget POST: same contract as the old ntfy path
// — bounded, and a hanging endpoint must never delay lifecycle handling.
// Failures are journaled so silent drops stay diagnosable.
func (b *Bus) DeliverWebhook(url string, payload LifecycleEvent) {
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
			b.journal.Append(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		req.Header.Set("content-type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			b.journal.Append(map[string]any{"event": "webhook.failed", "url": url, "reason": err.Error(), "for": payload.Event})
			return
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			b.journal.Append(map[string]any{"event": "webhook.failed", "url": url, "status": res.StatusCode, "for": payload.Event})
		}
	}()
}
