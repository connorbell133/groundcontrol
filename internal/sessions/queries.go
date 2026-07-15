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
	At             string  `json:"at"`
	Stale          bool    `json:"stale"` // launch config whose branch no longer exists
}

type LostSession struct {
	ID           string `json:"id"`
	RecentLaunch        // embedded — flattens in JSON like the TS interface extension
}

const lostWindow = 7 * 24 * time.Hour

func jStr(e map[string]any, key string) string {
	if v, ok := e[key].(string); ok {
		return v
	}
	return ""
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
				At:             at,
				Stale:          branch != "" && !gitx.BranchExists(folder, branch),
			},
		})
	}
	m.lostCache = out
	m.lostComputed = true
	return append([]LostSession(nil), m.lostCache...)
}
