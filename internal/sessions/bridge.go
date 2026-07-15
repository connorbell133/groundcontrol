package sessions

/* ---------- pairing URL from the bridge pointer ----------
The remote-control server writes ~/.claude/projects/<slug(cwd)>/bridge-pointer.json
— {sessionId, environmentId, source, pid, procStart} — about 1.3s after spawn,
and the pairing URL is exactly https://claude.ai/code?environment=<environmentId>.
Both the file format and the URL shape were observed on CLI 2.1.210 and are
undocumented, which is why the PTY scrape keeps running as fallback and WINS on
disagreement: the CLI's own printed URL is ground truth, and a mismatch is the
tripwire that the observed shape drifted.

Validation is load-bearing, not paranoia. Pointers deliberately persist after
shutdown (they enable resume), and there is exactly one pointer per folder —
so stale pointers from past runs, foreign pointers from manual launches, and
overwrites by a concurrent same-dir launch are all expected states. The spawned
launcher forks once and the pointer records the forked server's pid, so a
pointer belongs to this launch when its pid is the spawned pid or a descendant
of it; procStart is a secondary sanity guard against freak pid reuse. */

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// which source won a session's pairing URL; the scrape is terminal — once it
// speaks, the pointer path never writes again
const (
	pairingSourcePointer = "bridge-pointer"
	pairingSourceScrape  = "pty-scrape"
)

const (
	bridgePointerFile = "bridge-pointer.json"
	bridgePSTimeout   = 5 * time.Second
	// procStart is a ctime-style local-time string with no format guarantee;
	// the window is generous because it only needs to catch pointers from a
	// different era, not race the clock
	bridgeProcStartSlack = time.Hour
	// ctime shape observed in the pointer: "Wed Jul 15 16:45:57 2026"
	bridgeProcStartLayout = "Mon Jan _2 15:04:05 2006"
)

// vars, not consts: tests shrink the window so rejection paths don't take 15s
var (
	bridgePollInterval = 300 * time.Millisecond
	bridgePollWindow   = 15 * time.Second
)

// bridgePSMap is the pid→ppid snapshot the descendant test walks (shared exec
// in ps.go); a package var kept separate from the registry poller's seam so
// their test harnesses can swap process trees independently.
var bridgePSMap = execPSParents

// bridgePointer decodes only what acceptance needs; unknown fields ignored
// per the tolerant-parse house rule (R7).
type bridgePointer struct {
	EnvironmentID string `json:"environmentId"`
	PID           int    `json:"pid"`
	ProcStart     string `json:"procStart"`
}

// readBridgePointer returns nil for a missing, malformed, or incomplete file
// — silence, not error: the pointer may simply not exist yet, or be mid-write.
func readBridgePointer(path string) *bridgePointer {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p bridgePointer
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	if p.EnvironmentID == "" || p.PID <= 0 {
		return nil
	}
	return &p
}

// acceptBridgePointer decides whether a pointer belongs to this launch.
// Primary: the pointer pid is the spawned pid or a descendant of it (the
// launcher forks once; the pointer records the fork). Secondary: procStart
// within a generous window of our launch time — but an unparseable procStart
// passes, because formats drift and ancestry already vouched for the pid.
func acceptBridgePointer(p *bridgePointer, spawnPID int, launched time.Time, haveLaunched bool, ps func() map[int]int) bool {
	if spawnPID <= 0 {
		return false
	}
	if p.PID != spawnPID {
		if m := ps(); m == nil || !reachesAncestor(m, p.PID, spawnPID) {
			// a live pointer we can't tie to our spawn is indistinguishable
			// from a concurrent same-dir launch's — reject and ride the scrape
			return false
		}
	}
	if haveLaunched {
		// local zone: the pointer is written on this machine by a process we
		// just started, and the ctime string carries no zone of its own
		if st, err := time.ParseInLocation(bridgeProcStartLayout, strings.TrimSpace(p.ProcStart), time.Local); err == nil {
			if d := st.Sub(launched); d < -bridgeProcStartSlack || d > bridgeProcStartSlack {
				return false
			}
		}
	}
	return true
}

// setReadyLocked is the single ready transition both URL sources share:
// first writer wins and exactly one session.ready leaves the building.
// Caller holds m.mu; the returned snapshot is journaled and announced after
// release, nil means another source already won (or the session died first —
// a pointer must never resurrect an errored launch).
func (s *liveSession) setReadyLocked(url, source string) *Session {
	if s.PairingURL != nil || s.State != StateStarting {
		return nil
	}
	s.PairingURL = &url
	s.State = StateReady
	s.pairingSource = source
	log.Printf("session %s: ready via %s", s.ID, source)
	snap := s.Session
	return &snap
}

// reconcileScrapedURLLocked handles the scrape speaking after the pointer
// already won ready. Agreement just retires the scan; disagreement means the
// observed URL shape drifted — the scraped URL overwrites the constructed one
// (CLI output is ground truth) and the tripwire logs. At most once per
// session by construction: the source flips to scrape either way, and the
// readLoop only scans while the source is still the pointer. Caller holds m.mu.
func (s *liveSession) reconcileScrapedURLLocked(url string) {
	if s.pairingSource != pairingSourcePointer || s.PairingURL == nil {
		return
	}
	if *s.PairingURL != url {
		log.Printf("session %s: scraped pairing URL %q disagrees with bridge-pointer URL %q — scrape wins (URL-shape drift tripwire)", s.ID, url, *s.PairingURL)
		s.PairingURL = &url
	}
	s.pairingSource = pairingSourceScrape
}

// watchBridgePointer polls the launch cwd's project dir for a pid-validated
// pointer and flips ready with the constructed URL. Short-lived by design:
// it ends at first resolution by either source, when the session dies, or at
// the poll window — after which the scrape carries the launch alone.
func (m *Manager) watchBridgePointer(s *liveSession, spawnPID int) {
	// s.cwd and s.StartedAt are immutable after Create; read lock-free
	path := filepath.Join(claudeProjectsDir, claudeProjectDirName(s.cwd), bridgePointerFile)
	launched, launchedErr := time.Parse(time.RFC3339, s.StartedAt)
	deadline := time.Now().Add(bridgePollWindow)
	// stale/foreign pointers persist by design, so a parked pointer would
	// otherwise cost one ps exec per poll iteration for the whole window —
	// cache the snapshot and refresh at most once a second
	var psCache map[int]int
	var psAt time.Time
	psFn := func() map[int]int {
		if psCache == nil || time.Since(psAt) > time.Second {
			if m, err := bridgePSMap(bridgePSTimeout); err == nil {
				psCache, psAt = m, time.Now()
			}
		}
		return psCache
	}
	for {
		m.mu.Lock()
		resolved := s.PairingURL != nil
		alive := s.State == StateStarting
		m.mu.Unlock()
		if resolved || !alive {
			return
		}
		if p := readBridgePointer(path); p != nil && acceptBridgePointer(p, spawnPID, launched, launchedErr == nil, psFn) {
			// URL shape observed on 2.1.210, undocumented — drift surfaces via
			// the scrape reconcile above, never as a silent wrong link
			url := "https://claude.ai/code?environment=" + p.EnvironmentID
			m.mu.Lock()
			snap := s.setReadyLocked(url, pairingSourcePointer)
			id := s.ID
			m.mu.Unlock()
			if snap != nil {
				m.journal.Append(map[string]any{"event": evSessionReady, "id": id, "pairingUrl": url})
				m.announce(evSessionReady, *snap, false)
			}
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(bridgePollInterval)
	}
}
