// Package journal is the append-only flight log shared by sessions and jobs.
//
// Storage is JSONL (one compact object per line) so every event is a single
// O(1) append. A crash mid-write can only truncate the last line, never the
// history behind it — Read skips partial lines silently.
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/connorbell133/groundcontrol/internal/util"
)

type Journal struct {
	dataDir  string
	mu       sync.Mutex
	migrated bool // guarded by mu; migration runs at most once per process
}

func New(dataDir string) *Journal {
	return &Journal{dataDir: dataDir}
}

func (j *Journal) path() string       { return filepath.Join(j.dataDir, "journal.jsonl") }
func (j *Journal) legacyPath() string { return filepath.Join(j.dataDir, "journal.json") }

// migrateLocked converts a pre-v0.4.x journal.json (one big JSON array)
// to JSONL, once. Caller must hold mu. The legacy file is renamed to
// journal.json.bak, never deleted — history is the whole point of a journal.
func (j *Journal) migrateLocked() {
	if j.migrated {
		return
	}
	j.migrated = true
	if _, err := os.Stat(j.path()); err == nil {
		return // already on JSONL
	}
	raw, err := os.ReadFile(j.legacyPath())
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
	tmp := j.path() + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, j.path()); err != nil {
		return
	}
	_ = os.Rename(j.legacyPath(), j.legacyPath()+".bak")
}

func (j *Journal) Append(entry map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = os.MkdirAll(j.dataDir, 0o755)
	j.migrateLocked()
	e := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		e[k] = v
	}
	e["at"] = util.NowISO()
	f, err := os.OpenFile(j.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e) // one compact line; Encode appends the trailing "\n"
}

func (j *Journal) Read() []map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.migrateLocked()
	raw, err := os.ReadFile(j.path())
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
