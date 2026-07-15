package sessions

import (
	"time"

	"github.com/connorbell133/groundcontrol/internal/gitx"
	"github.com/connorbell133/groundcontrol/internal/util"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

/* ---------- journal queries ---------- */

type RecentLaunch struct {
	Folder         string  `json:"folder"`
	Name           string  `json:"name"`
	Branch         *string `json:"branch"`
	SpawnMode      string  `json:"spawnMode"`
	PermissionMode string  `json:"permissionMode"`
	InitialPrompt  *string `json:"initialPrompt"`
	At             string  `json:"at"`
	Stale          bool    `json:"stale"` // launch config whose branch no longer exists
}

type LostSession struct {
	ID           string `json:"id"`
	RecentLaunch        // embedded — flattens in JSON like the TS interface extension
}

const lostWindow = 7 * 24 * time.Hour

// landedCap bounds the landed list: it's a recent-debriefs feed, not an archive.
const landedCap = 20

func jStr(e map[string]any, key string) string {
	if v, ok := e[key].(string); ok {
		return v
	}
	return ""
}

// jInt reads a journal number — always float64 after the JSON round-trip.
func jInt(e map[string]any, key string) int {
	if v, ok := e[key].(float64); ok {
		return int(v)
	}
	return 0
}

func (m *Manager) RecentLaunches(limit int) []RecentLaunch {
	seen := map[string]bool{}
	out := []RecentLaunch{}
	entries := m.journal.Read()
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		folder := jStr(e, "folder")
		if jStr(e, "event") != evSessionStart || folder == "" {
			continue
		}
		if !util.PathExists(folder) || !m.browser.WithinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		rawSpawnMode := jStr(e, "spawnMode")
		key := folder + "\x00" + branch + "\x00" + rawSpawnMode
		if seen[key] {
			continue
		}
		seen[key] = true
		mode := rawSpawnMode
		if mode == "" {
			mode = string(workspace.SpawnSameDir)
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		out = append(out, RecentLaunch{
			Folder:         folder,
			Name:           jStr(e, "name"),
			Branch:         util.StrPtr(branch),
			SpawnMode:      mode,
			PermissionMode: permissionMode,
			InitialPrompt:  util.StrPtr(jStr(e, "initialPrompt")),
			At:             jStr(e, "at"),
			Stale:          branch != "" && !gitx.BranchExists(folder, branch),
		})
	}
	return out
}

func (m *Manager) ListLost() []LostSession {
	// snapshot live ids before taking lostMu — never hold both locks
	liveIDs := map[string]bool{}
	m.mu.Lock()
	for id := range m.sessions {
		liveIDs[id] = true
	}
	m.mu.Unlock()

	m.lostMu.Lock()
	defer m.lostMu.Unlock()
	if m.lostComputed {
		// copy: a dismissal splicing lostCache must not race a caller still marshaling
		return append([]LostSession(nil), m.lostCache...)
	}
	entries := m.journal.Read()
	terminated := map[string]bool{}
	for _, e := range entries {
		ev := jStr(e, "event")
		if (ev == evSessionExit || ev == evSessionKill) && jStr(e, "id") != "" {
			terminated[jStr(e, "id")] = true
		}
	}
	cutoff := time.Now().Add(-lostWindow)
	out := []LostSession{}
	for _, e := range entries {
		id := jStr(e, "id")
		folder := jStr(e, "folder")
		if jStr(e, "event") != evSessionStart || id == "" || folder == "" {
			continue
		}
		if terminated[id] || liveIDs[id] {
			continue
		}
		at := jStr(e, "at")
		if at == "" {
			continue
		}
		// unparsable timestamps pass through, like NaN < cutoff in JS
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.Before(cutoff) {
			continue
		}
		if !util.PathExists(folder) || !m.browser.WithinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		mode := jStr(e, "spawnMode")
		if mode == "" {
			mode = string(workspace.SpawnSameDir)
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		out = append(out, LostSession{
			ID: id,
			RecentLaunch: RecentLaunch{
				Folder:         folder,
				Name:           jStr(e, "name"),
				Branch:         util.StrPtr(branch),
				SpawnMode:      mode,
				PermissionMode: permissionMode,
				InitialPrompt:  util.StrPtr(jStr(e, "initialPrompt")),
				At:             at,
				Stale:          branch != "" && !gitx.BranchExists(folder, branch),
			},
		})
	}
	m.lostCache = out
	m.lostComputed = true
	return append([]LostSession(nil), m.lostCache...)
}

// LandedSession is a finished session reconstructed from its
// session.start/session.exit journal pair — the debrief that survives a
// runner restart.
type LandedSession struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Folder         string   `json:"folder"`
	Branch         *string  `json:"branch"`
	SpawnMode      string   `json:"spawnMode"`
	PermissionMode string   `json:"permissionMode"`
	InitialPrompt  *string  `json:"initialPrompt"`
	StartedAt      string   `json:"startedAt"`
	ExitedAt       string   `json:"exitedAt"`
	ExitCode       *int     `json:"exitCode"`
	Debrief        *Debrief `json:"debrief,omitempty"`
}

// ListLanded joins session.start entries with their session.exit entries,
// newest first. Ids the manager still lists (live, or exited but not yet
// dismissed) are excluded — those already appear under "sessions". Unlike
// ListLost this is never cached: exits happen while the process runs; the
// journal read (capped at 2000 entries) and landedCap bound the work.
func (m *Manager) ListLanded() []LandedSession {
	liveIDs := map[string]bool{}
	m.mu.Lock()
	for id := range m.sessions {
		liveIDs[id] = true
	}
	m.mu.Unlock()

	entries := m.journal.Read()
	exits := map[string]map[string]any{}
	for _, e := range entries {
		if jStr(e, "event") == evSessionExit && jStr(e, "id") != "" {
			exits[jStr(e, "id")] = e
		}
	}
	cutoff := time.Now().Add(-lostWindow)
	seen := map[string]bool{}
	out := []LandedSession{}
	for i := len(entries) - 1; i >= 0 && len(out) < landedCap; i-- {
		e := entries[i]
		id := jStr(e, "id")
		folder := jStr(e, "folder")
		if jStr(e, "event") != evSessionStart || id == "" || folder == "" || seen[id] {
			continue
		}
		seen[id] = true
		exit, ok := exits[id]
		if !ok || liveIDs[id] {
			continue
		}
		exitedAt := jStr(exit, "at")
		// unparsable timestamps pass through, matching ListLost
		if t, err := time.Parse(time.RFC3339, exitedAt); err == nil && t.Before(cutoff) {
			continue
		}
		if !util.PathExists(folder) || !m.browser.WithinRoots(folder) {
			continue
		}
		mode := jStr(e, "spawnMode")
		if mode == "" {
			mode = string(workspace.SpawnSameDir)
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		var exitCode *int
		if v, ok := exit["code"].(float64); ok {
			exitCode = util.IntPtr(int(v))
		}
		var debrief *Debrief
		if bs := jStr(exit, "branchState"); bs != "" {
			debrief = &Debrief{
				DiffStats: gitx.DiffStats{
					FilesChanged: jInt(exit, "filesChanged"),
					Insertions:   jInt(exit, "insertions"),
					Deletions:    jInt(exit, "deletions"),
					Uncommitted:  jInt(exit, "uncommitted"),
				},
				BranchState: bs,
			}
		}
		out = append(out, LandedSession{
			ID:             id,
			Name:           jStr(e, "name"),
			Folder:         folder,
			Branch:         util.StrPtr(jStr(e, "branch")),
			SpawnMode:      mode,
			PermissionMode: permissionMode,
			InitialPrompt:  util.StrPtr(jStr(e, "initialPrompt")),
			StartedAt:      jStr(e, "at"),
			ExitedAt:       exitedAt,
			ExitCode:       exitCode,
			Debrief:        debrief,
		})
	}
	return out
}
