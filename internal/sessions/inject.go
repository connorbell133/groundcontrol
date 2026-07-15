package sessions

/* ---------- preset settings injection ----------
A preset's settings JSON becomes <launch cwd>/.claude/settings.local.json for
the session's lifetime — the project-scoped file the remote-control subcommand
reads natively (Path B, verified on 2.1.210 in
docs/solutions/2026-07-15-hooks-push-tier-spike.md). The file carries a
top-level "_groundcontrol": true marker so teardown and the startup sweep only
ever touch files GroundControl wrote: an existing unmarked file refuses
injection outright, and a marked file is replaced only when no live session
still works in that folder. Every failure degrades to a skip reason — the
launch itself is never blocked. */

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// settingsMarkerKey is the top-level key that brands an injected file as
// GroundControl's own; nothing without it is ever replaced or removed.
const settingsMarkerKey = "_groundcontrol"

// injection skip reasons — durable launch facts, flattened into the
// session.start entry and surfaced on the wire as settingsSkipReason.
const (
	skipPresetGone  = "preset no longer exists"
	skipUserFile    = "settings file already exists"
	skipInUse       = "in use by another session"
	skipBadSettings = "preset settings are not a JSON object"
	skipWriteFailed = "settings write failed"
)

// noteReplacedStale journals alongside settingsInjected when a crash leftover
// was overwritten rather than freshly created.
const noteReplacedStale = "replaced stale injection"

func settingsFilePath(cwd string) string {
	return filepath.Join(cwd, ".claude", "settings.local.json")
}

// settingsFileMarked reads path tolerantly: a missing, unparseable, or
// unmarked file all answer false — anything we can't positively identify as
// our own is treated as the user's (R7).
func settingsFileMarked(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m[settingsMarkerKey]
	return ok
}

// removeMarkedSettings deletes path only when the marker vouches for it;
// best-effort, tolerant of missing dirs and files.
func removeMarkedSettings(path string) {
	if settingsFileMarked(path) {
		os.Remove(path)
	}
}

// liveSameFolderExists reports whether any live session runs in cwd's
// normalized folder. Cwds are collected under m.mu and normalized outside:
// EvalSymlinks does filesystem work that must not run under the lock
// (precedent: the registry tick's normalize pass).
func (m *Manager) liveSameFolderExists(cwd string) bool {
	var cwds []string
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.State != StateExited && s.State != StateError {
			cwds = append(cwds, s.cwd)
		}
	}
	m.mu.Unlock()
	norm := normalizePath(cwd)
	for _, c := range cwds {
		if normalizePath(c) == norm {
			return true
		}
	}
	return false
}

// injectSettings writes the preset's settings JSON (marker added) to path,
// atomically: O_CREATE|O_EXCL makes the existence check and the write one
// operation, so two concurrent launches into the same folder can never
// interleave here. On EEXIST the file is read tolerantly — unmarked means a
// user file (refuse), marked with a live same-folder session means theirs
// (skip), marked with no live sharer means a crash leftover (replace).
func (m *Manager) injectSettings(path, cwd, settingsJSON string) (injected, replaced bool, skipReason string) {
	// re-parse rather than trust the PUT /config validation: config.json is
	// hand-editable, and an unparseable preset must skip, never crash a launch
	var settings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil || settings == nil {
		return false, false, skipBadSettings
	}
	settings[settingsMarkerKey] = json.RawMessage("true")
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, false, skipWriteFailed
	}
	content = append(content, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, false, skipWriteFailed
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_, werr := f.Write(content)
		f.Close()
		if werr != nil {
			os.Remove(path) // half-written, and provably ours — never leave it
			return false, false, skipWriteFailed
		}
		return true, false, ""
	}
	if !errors.Is(err, fs.ErrExist) {
		return false, false, skipWriteFailed
	}
	if !settingsFileMarked(path) {
		return false, false, skipUserFile
	}
	if m.liveSameFolderExists(cwd) {
		return false, false, skipInUse
	}
	// stale crash leftover: ours by marker, nobody live in the folder
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return false, false, skipWriteFailed
	}
	return true, true, ""
}

// removeSettingsIfLast is the teardown half: remove the marked file unless
// another live session still shares the normalized folder — then removal
// defers to the last same-folder exit (each exit re-checks). The exiting
// session's own state is already exited when this runs, so it never counts
// itself as a sharer.
func (m *Manager) removeSettingsIfLast(path, cwd string) {
	if m.liveSameFolderExists(cwd) {
		return
	}
	removeMarkedSettings(path)
}

// SweepSettingsLeftovers removes marked settings files orphaned by a crashed
// runner: it scans recent session.start entries that recorded an injection
// (settingsInjected true) and clears marked leftovers in their folders. Boot
// runs it before any session exists, so there are no live sharers to respect;
// worktree-launch leftovers die with the orphan-worktree sweep instead.
// Tolerant throughout — missing dirs, missing files, unmarked files all no-op.
func (m *Manager) SweepSettingsLeftovers() {
	seen := map[string]bool{}
	for _, e := range m.journal.Read() {
		if jStr(e, "event") != evSessionStart {
			continue
		}
		if injected, ok := e["settingsInjected"].(bool); !ok || !injected {
			continue
		}
		folder := jStr(e, "folder")
		if folder == "" || seen[folder] {
			continue
		}
		seen[folder] = true
		removeMarkedSettings(settingsFilePath(folder))
	}
}
