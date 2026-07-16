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
	"syscall"
)

// settingsMarkerKey is the top-level key that brands an injected file as
// GroundControl's own; nothing without it is ever replaced or removed.
const settingsMarkerKey = "_groundcontrol"

// injection skip reasons — durable launch facts, flattened into the
// session.start entry and surfaced on the wire as settingsSkipReason.
const (
	skipPresetGone   = "preset no longer exists"
	skipUserFile     = "settings file already exists"
	skipInUse        = "in use by another session"
	skipBadSettings  = "preset settings are not a JSON object"
	skipTooLarge     = "preset settings exceed 64 KB"
	skipForbiddenKey = "preset settings carry a non-inert key"
	skipWriteFailed  = "settings write failed"
)

// settingsMaxBytes bounds an injected preset. Enforced at the PUT /config
// boundary and re-enforced here, since config.json is hand-editable.
const settingsMaxBytes = 64 * 1024

// settingsInertKeys is the allowlist of settings.local.json top-level keys
// safe to inject into a user's repo before launch: display, model-selection,
// and UX toggles with no command execution, no credential/traffic
// redirection, and no permission/sandbox weakening. Everything else is
// refused. An allowlist fails closed — a key added in a future CLI release is
// rejected until vetted, rather than injected sight-unseen the way a denylist
// would let a newly-dangerous key through. The set is deliberately
// conservative and pinned to the Claude Code settings schema as of CLI
// 2.1.210 (see docs/solutions/2026-07-15-hooks-push-tier-spike.md); re-probe
// the settings surface on each CLI bump and widen only after review. Keys
// intentionally excluded because they execute commands, move credentials, or
// alter policy: hooks, apiKeyHelper, statusLine, awsAuthRefresh,
// awsCredentialExport, gcpAuthRefresh, fileSuggestion, env, permissions,
// sandbox, additionalDirectories, forceLogin*, and the MCP-server keys.
var settingsInertKeys = map[string]struct{}{
	"model":                    {},
	"fallbackModel":            {},
	"effortLevel":              {},
	"alwaysThinkingEnabled":    {},
	"editorMode":               {},
	"autoScrollEnabled":        {},
	"verbose":                  {},
	"theme":                    {},
	"tui":                      {},
	"language":                 {},
	"spinnerTipsEnabled":       {},
	"awaySummaryEnabled":       {},
	"askUserQuestionTimeout":   {},
	"attribution":              {},
	"includeCoAuthoredBy":      {},
	"cleanupPeriodDays":        {},
	"fileCheckpointingEnabled": {},
}

// ForbiddenSettingsKey returns the first top-level key in a parsed settings
// object that is not on the inert allowlist, or "" when every key is inert.
// The marker key is always permitted — injectSettings adds it itself. The
// caller confirms object-ness before calling. Shared by the PUT /config
// boundary (fail-fast rejection) and injectSettings (skip-with-reason) so the
// two enforcement points can never drift apart.
func ForbiddenSettingsKey(obj map[string]json.RawMessage) string {
	for k := range obj {
		if k == settingsMarkerKey {
			continue
		}
		if _, ok := settingsInertKeys[k]; !ok {
			return k
		}
	}
	return ""
}

// noteReplacedStale journals alongside settingsInjected when a crash leftover
// was overwritten rather than freshly created.
const noteReplacedStale = "replaced stale injection"

// settingsOwner identifies the runner instance that wrote a marker file, so a
// second runner sharing the base checkout (an acknowledged reality — the
// runner lock exists precisely because dev copies share it) can tell a peer's
// live injection from a crash leftover instead of clobbering it.
type settingsOwner struct {
	Pid  int    `json:"pid"`
	Host string `json:"host"`
	ID   string `json:"sessionId,omitempty"`
}

var runnerHost, _ = os.Hostname()

// settingsMarkerValue is the marker payload for a fresh injection: the owning
// runner's pid and host plus the session id. settingsFileMarked only checks
// key presence, so teardown and the sweep are unaffected by the value shape.
func settingsMarkerValue(sessionID string) json.RawMessage {
	b, err := json.Marshal(settingsOwner{Pid: os.Getpid(), Host: runnerHost, ID: sessionID})
	if err != nil {
		return json.RawMessage("true")
	}
	return b
}

// markerOwner extracts the owner recorded in path's marker. known is false for
// a missing/unparseable file, an unmarked file, or a legacy boolean marker
// (`"_groundcontrol": true`) with no owner object — the caller then falls back
// to the live-same-folder check, preserving pre-ownership behavior.
func markerOwner(path string) (owner settingsOwner, known bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return owner, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return owner, false
	}
	raw, ok := m[settingsMarkerKey]
	if !ok {
		return owner, false
	}
	if json.Unmarshal(raw, &owner) != nil || owner.Pid == 0 {
		return owner, false
	}
	return owner, true
}

// ownerLive reports whether a marker owner is still running. A foreign-host
// owner returns true unconditionally — we cannot probe another host's process
// table, so we never clobber a file that may be live elsewhere (the shared
// checkout can be on an NFS/synced dir). Same-host pid reuse fails safe: a
// recycled pid reads as live, so we skip rather than clobber.
func ownerLive(o settingsOwner) bool {
	if o.Host != runnerHost {
		return true
	}
	return pidAlive(o.Pid)
}

// pidAlive probes process existence with signal 0 — no signal is delivered.
// EPERM means the process exists but is owned by another user (still alive).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// writeFileMarkedAtomic writes content to path via a temp file in the same
// directory plus rename(2). Unlike os.WriteFile, rename replaces the
// destination inode rather than following a symlink planted at path between
// the marker check and the write, closing the stale-replacement TOCTOU (R11).
func writeFileMarkedAtomic(path string, content []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "._gc-settings-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

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
func (m *Manager) injectSettings(path, cwd, settingsJSON, sessionID string) (injected, replaced bool, skipReason string) {
	// re-parse and re-validate rather than trust the PUT /config validation:
	// config.json is hand-editable, so the size cap, object-ness, and
	// inert-key allowlist must all be re-checked here or a hand-edited preset
	// carrying hooks (or any other command-executing key) would be injected
	// verbatim. Every failure skips, never crashes the launch.
	if len(settingsJSON) > settingsMaxBytes {
		return false, false, skipTooLarge
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil || settings == nil {
		return false, false, skipBadSettings
	}
	if ForbiddenSettingsKey(settings) != "" {
		return false, false, skipForbiddenKey
	}
	settings[settingsMarkerKey] = settingsMarkerValue(sessionID)
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
	// marked file exists. A live session in THIS runner sharing the folder
	// always wins. Otherwise consult the marker's own owner: a second runner
	// instance must not misread a peer's live file as a crash leftover and
	// clobber it (the two-instance clobber this guards). A legacy/ownerless
	// marker falls back to the live-same-folder check alone.
	if m.liveSameFolderExists(cwd) {
		return false, false, skipInUse
	}
	if o, known := markerOwner(path); known && ownerLive(o) {
		return false, false, skipInUse
	}
	// stale crash leftover: ours by marker, owner gone, nobody live in the
	// folder. Replace atomically so a symlink planted since the marker check
	// is not followed (R11).
	if err := writeFileMarkedAtomic(path, content); err != nil {
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
// runner. It scans two journal sources: session.start entries that recorded an
// injection (settingsInjected true), and session.inject-intent entries written
// just before each file write. The intent source closes the crash window where
// the runner died after writing the file but before journaling session.start —
// session.start would never exist, so a start-only sweep would miss the
// leftover (R12). Boot runs this before any session exists, so there are no
// live sharers to respect, and the marker gate makes sweeping a folder that
// exited cleanly a no-op (the file was already removed at teardown).
// Worktree-launch leftovers die with the orphan-worktree sweep instead.
// Tolerant throughout — missing dirs, missing files, unmarked files all no-op.
func (m *Manager) SweepSettingsLeftovers() {
	seen := map[string]bool{}
	sweep := func(folder string) {
		if folder == "" || seen[folder] {
			return
		}
		seen[folder] = true
		removeMarkedSettings(settingsFilePath(folder))
	}
	for _, e := range m.journal.Read() {
		switch jStr(e, "event") {
		case evSessionInjectIntent:
			sweep(jStr(e, "folder"))
		case evSessionStart:
			if injected, ok := e["settingsInjected"].(bool); ok && injected {
				sweep(jStr(e, "folder"))
			}
		}
	}
}
