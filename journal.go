package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

/* ---------- journal: append-only flight log, shared by sessions and jobs ---------- */

// Storage is JSONL (one compact object per line) so every event is a single
// O(1) append. A crash mid-write can only truncate the last line, never the
// history behind it — readJournal skips partial lines silently.

func (a *app) journalPath() string       { return filepath.Join(a.dataDir, "journal.jsonl") }
func (a *app) legacyJournalPath() string { return filepath.Join(a.dataDir, "journal.json") }

// migrateJournalLocked converts a pre-v0.4.x journal.json (one big JSON array)
// to JSONL, once. Caller must hold journalMu. The legacy file is renamed to
// journal.json.bak, never deleted — history is the whole point of a journal.
func (a *app) migrateJournalLocked() {
	if a.journalMigrated {
		return
	}
	a.journalMigrated = true
	if _, err := os.Stat(a.journalPath()); err == nil {
		return // already on JSONL
	}
	raw, err := os.ReadFile(a.legacyJournalPath())
	if err != nil {
		return // fresh install, nothing to migrate
	}
	// []any, not []map[string]any: a stray non-object element in the legacy
	// array must not abort the migration of everything around it
	var all []any
	if err := json.Unmarshal(raw, &all); err != nil {
		return // unreadable legacy file — leave it untouched for a human
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, v := range all {
		if m, ok := v.(map[string]any); ok {
			_ = enc.Encode(m) // Encode appends the "\n" that terminates a JSONL line
		}
	}
	// write via temp + rename: if we crashed mid-write of journal.jsonl itself,
	// its existence would block a retry and silently orphan the legacy history
	tmp := a.journalPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, a.journalPath()); err != nil {
		return
	}
	_ = os.Rename(a.legacyJournalPath(), a.legacyJournalPath()+".bak")
}

func (a *app) journal(entry map[string]any) {
	a.journalMu.Lock()
	defer a.journalMu.Unlock()
	_ = os.MkdirAll(a.dataDir, 0o755)
	a.migrateJournalLocked()
	e := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		e[k] = v
	}
	e["at"] = nowISO()
	f, err := os.OpenFile(a.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e) // one compact line; Encode appends the trailing "\n"
}

func (a *app) readJournal() []map[string]any {
	a.journalMu.Lock()
	defer a.journalMu.Unlock()
	a.migrateJournalLocked()
	raw, err := os.ReadFile(a.journalPath())
	if err != nil {
		return []map[string]any{}
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// blank, truncated, or non-object lines drop out here — a torn final
		// line from a crash must not hide the rest of the history
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m == nil {
			continue
		}
		out = append(out, m)
	}
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	return out
}
