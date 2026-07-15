package sessions

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/testutil"
	"github.com/connorbell133/groundcontrol/internal/util"
)

/* Bridge tests mutate package seams (claudeProjectsDir, bridgePSMap, the poll
window) and some install a fake claude on PATH via t.Setenv, so none of them
call t.Parallel — the serial phase is what keeps the seams safe to swap. */

// overrideBridgeEnv points the pointer reads at a temp projects dir, injects a
// synthetic process tree, and shrinks the poll window so rejection paths fail
// in milliseconds instead of the production 15s.
func overrideBridgeEnv(t *testing.T, ps map[int]int) {
	t.Helper()
	origDir, origPS := claudeProjectsDir, bridgePSMap
	origInterval, origWindow := bridgePollInterval, bridgePollWindow
	claudeProjectsDir = t.TempDir()
	bridgePSMap = func(time.Duration) (map[int]int, error) { return ps, nil }
	bridgePollInterval = 5 * time.Millisecond
	bridgePollWindow = 400 * time.Millisecond
	t.Cleanup(func() {
		claudeProjectsDir, bridgePSMap = origDir, origPS
		bridgePollInterval, bridgePollWindow = origInterval, origWindow
	})
}

// insertStarting plants a hand-built starting session so the watcher can be
// driven synchronously, without a PTY spawn.
func insertStarting(m *Manager, cwd string) *liveSession {
	s := &liveSession{
		Session: Session{
			ID:        util.RandomID(8),
			Name:      "bridge-" + util.RandomID(4),
			Folder:    cwd,
			State:     StateStarting,
			StartedAt: util.NowISO(),
		},
		cwd: cwd,
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s
}

func writePointerRaw(t *testing.T, cwd, raw string) {
	t.Helper()
	dir := filepath.Join(claudeProjectsDir, claudeProjectDirName(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project slug dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, bridgePointerFile), []byte(raw), 0o644); err != nil {
		t.Fatalf("write bridge pointer: %v", err)
	}
}

func writePointer(t *testing.T, cwd string, fields map[string]any) {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	writePointerRaw(t, cwd, string(b))
}

func nowProcStart() string { return time.Now().Format(bridgeProcStartLayout) }

// readyEntries counts a session's journaled session.ready events and returns
// the pairingUrl of the last one — one entry regardless of source is R11's
// core promise, and the journaled URL is race-free evidence of who won.
func readyEntries(m *Manager, id string) (n int, pairingURL string) {
	for _, e := range m.journal.Read() {
		if e["event"] == evSessionReady && e["id"] == id {
			n++
			pairingURL, _ = e["pairingUrl"].(string)
		}
	}
	return n, pairingURL
}

func TestBridgePointerMatchingDescendantFlipsReady(t *testing.T) {
	// pointer records the forked server (300), a grandchild of the spawn (100)
	overrideBridgeEnv(t, map[int]int{300: 200, 200: 100})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	writePointer(t, cwd, map[string]any{
		"sessionId": "session_01x", "environmentId": "env_01abc",
		"source": "standalone", "pid": 300, "procStart": nowProcStart(),
	})

	m.watchBridgePointer(s, 100)

	got := m.Get(s.ID)
	want := "https://claude.ai/code?environment=env_01abc"
	if got.State != StateReady || got.PairingURL == nil || *got.PairingURL != want {
		t.Fatalf("expected ready with %q, got state=%s url=%v", want, got.State, got.PairingURL)
	}
	if n, url := readyEntries(m, s.ID); n != 1 || url != want {
		t.Errorf("journal: want exactly one session.ready carrying %q, got n=%d url=%q", want, n, url)
	}
	// a second watcher run must be a no-op: the ready guard is idempotent
	m.watchBridgePointer(s, 100)
	if n, _ := readyEntries(m, s.ID); n != 1 {
		t.Errorf("second watcher run journaled another session.ready (n=%d)", n)
	}
}

func TestBridgePointerExactPIDNeedsNoPSMap(t *testing.T) {
	// equality short-circuits before the ps snapshot — an empty map can't
	// block the common case where the pointer already names our spawn
	overrideBridgeEnv(t, map[int]int{})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	writePointer(t, cwd, map[string]any{"environmentId": "env_direct", "pid": 100, "procStart": nowProcStart()})

	m.watchBridgePointer(s, 100)

	got := m.Get(s.ID)
	if got.PairingURL == nil || *got.PairingURL != "https://claude.ai/code?environment=env_direct" {
		t.Fatalf("exact-pid pointer rejected: url=%v", got.PairingURL)
	}
}

func TestBridgePointerForeignPIDIgnored(t *testing.T) {
	// pointers persist after shutdown by design — a live-looking foreign pid
	// with no ancestry path to our spawn must never pair the card
	overrideBridgeEnv(t, map[int]int{999: 1})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	writePointer(t, cwd, map[string]any{"environmentId": "env_stale", "pid": 999, "procStart": nowProcStart()})

	m.watchBridgePointer(s, 100)

	got := m.Get(s.ID)
	if got.State != StateStarting || got.PairingURL != nil {
		t.Fatalf("foreign pointer accepted: state=%s url=%v", got.State, got.PairingURL)
	}
	if n, _ := readyEntries(m, s.ID); n != 0 {
		t.Errorf("foreign pointer journaled session.ready (n=%d)", n)
	}
}

func TestBridgePointerProcStartSanity(t *testing.T) {
	cases := []struct {
		name      string
		procStart string
		accept    bool
	}{
		{"fresh procStart accepted", nowProcStart(), true},
		// pid ancestry can freak-match after reuse; an era-off procStart is
		// the guard that catches it
		{"year-old procStart rejected", time.Now().AddDate(-1, 0, 0).Format(bridgeProcStartLayout), false},
		// formats drift and the pid already vouched — unparseable passes
		{"unparseable procStart accepted", "not a timestamp", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrideBridgeEnv(t, map[int]int{300: 100})
			m := testManager(t, nil)
			cwd := testutil.ResolvedTempDir(t)
			s := insertStarting(m, cwd)
			writePointer(t, cwd, map[string]any{"environmentId": "env_ps", "pid": 300, "procStart": tc.procStart})

			m.watchBridgePointer(s, 100)

			got := m.Get(s.ID)
			if tc.accept && (got.State != StateReady || got.PairingURL == nil) {
				t.Fatalf("expected acceptance, got state=%s url=%v", got.State, got.PairingURL)
			}
			if !tc.accept && got.PairingURL != nil {
				t.Fatalf("expected rejection, got url=%q", *got.PairingURL)
			}
		})
	}
}

func TestBridgePointerMalformedIgnored(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"truncated json", `{"environmentId":"env_1","pid":30`},
		{"not json", "bridge pointer goes here"},
		{"empty object", `{}`},
		{"missing environmentId", `{"pid":300,"procStart":"Wed Jul 15 16:45:57 2026"}`},
		{"missing pid", `{"environmentId":"env_1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrideBridgeEnv(t, map[int]int{300: 100})
			m := testManager(t, nil)
			cwd := testutil.ResolvedTempDir(t)
			s := insertStarting(m, cwd)
			writePointerRaw(t, cwd, tc.raw)

			m.watchBridgePointer(s, 100)

			got := m.Get(s.ID)
			if got.State != StateStarting || got.PairingURL != nil {
				t.Fatalf("malformed pointer accepted: state=%s url=%v", got.State, got.PairingURL)
			}
		})
	}
}

func TestBridgePointerDifferentSlugNeverMatches(t *testing.T) {
	// pins the slug munging against claudeProjectDirName: a valid pointer in
	// another folder's project dir is invisible to this launch
	overrideBridgeEnv(t, map[int]int{300: 100})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	otherCwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	writePointer(t, otherCwd, map[string]any{"environmentId": "env_other", "pid": 300, "procStart": nowProcStart()})

	m.watchBridgePointer(s, 100)

	if got := m.Get(s.ID); got.PairingURL != nil {
		t.Fatalf("pointer from another slug matched: url=%q", *got.PairingURL)
	}
}

func TestBridgePointerSameDirCollision(t *testing.T) {
	// one pointer per folder: two same-dir servers overwrite each other, the
	// pointer holds the winner's pid, and the loser must reject and ride the
	// scrape rather than pair against the other launch's environment
	overrideBridgeEnv(t, map[int]int{200: 150})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	first := insertStarting(m, cwd)  // spawned pid 100
	second := insertStarting(m, cwd) // spawned pid 150
	writePointer(t, cwd, map[string]any{"environmentId": "env_second", "pid": 200, "procStart": nowProcStart()})

	m.watchBridgePointer(first, 100)
	m.watchBridgePointer(second, 150)

	if got := m.Get(first.ID); got.PairingURL != nil {
		t.Fatalf("collision loser paired to the other launch's pointer: %q", *got.PairingURL)
	}
	if n, _ := readyEntries(m, first.ID); n != 0 {
		t.Errorf("collision loser journaled session.ready (n=%d)", n)
	}
	want := "https://claude.ai/code?environment=env_second"
	got := m.Get(second.ID)
	if got.PairingURL == nil || *got.PairingURL != want {
		t.Fatalf("collision winner: want %q, got %v", want, got.PairingURL)
	}
}

func TestBridgePointerAfterScrapeReadyIsNoOp(t *testing.T) {
	overrideBridgeEnv(t, map[int]int{300: 100})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)

	// scrape wins first, exactly as readLoop does it
	scraped := "https://claude.ai/remote/scrapedfirst"
	m.mu.Lock()
	snap := s.setReadyLocked(scraped, pairingSourceScrape)
	m.mu.Unlock()
	if snap == nil {
		t.Fatal("setReadyLocked refused a first scrape")
	}
	m.journal.Append(map[string]any{"event": evSessionReady, "id": s.ID, "pairingUrl": scraped})

	writePointer(t, cwd, map[string]any{"environmentId": "env_late", "pid": 300, "procStart": nowProcStart()})
	m.watchBridgePointer(s, 100)

	got := m.Get(s.ID)
	if got.PairingURL == nil || *got.PairingURL != scraped {
		t.Fatalf("late pointer overwrote the scraped URL: %v", got.PairingURL)
	}
	if n, _ := readyEntries(m, s.ID); n != 1 {
		t.Errorf("late pointer produced a second session.ready (n=%d)", n)
	}
}

func TestBridgePointerNeverResurrectsDeadSession(t *testing.T) {
	overrideBridgeEnv(t, map[int]int{300: 100})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	m.mu.Lock()
	s.State = StateError // died before ready; PairingURL still nil
	m.mu.Unlock()
	writePointer(t, cwd, map[string]any{"environmentId": "env_dead", "pid": 300, "procStart": nowProcStart()})

	m.watchBridgePointer(s, 100)

	if got := m.Get(s.ID); got.State != StateError || got.PairingURL != nil {
		t.Fatalf("pointer resurrected a dead session: state=%s url=%v", got.State, got.PairingURL)
	}
}

func TestReconcileScrapedURLScrapeWinsAndLogsOnce(t *testing.T) {
	logBuf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	overrideBridgeEnv(t, map[int]int{})
	m := testManager(t, nil)
	cwd := testutil.ResolvedTempDir(t)
	s := insertStarting(m, cwd)
	m.mu.Lock()
	s.setReadyLocked("https://claude.ai/code?environment=env_constructed", pairingSourcePointer)
	s.reconcileScrapedURLLocked("https://claude.ai/remote/truth")
	// second sighting must be inert: the source already flipped to scrape
	s.reconcileScrapedURLLocked("https://claude.ai/remote/truth")
	m.mu.Unlock()

	got := m.Get(s.ID)
	if got.PairingURL == nil || *got.PairingURL != "https://claude.ai/remote/truth" {
		t.Fatalf("scrape did not win the disagreement: %v", got.PairingURL)
	}
	if n := strings.Count(logBuf.String(), "disagrees"); n != 1 {
		t.Errorf("drift tripwire logged %d times, want exactly 1\n%s", n, logBuf.String())
	}
}

func TestReconcileScrapedURLAgreementRetiresScanQuietly(t *testing.T) {
	logBuf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	overrideBridgeEnv(t, map[int]int{})
	m := testManager(t, nil)
	s := insertStarting(m, testutil.ResolvedTempDir(t))
	url := "https://claude.ai/code?environment=env_same"
	m.mu.Lock()
	s.setReadyLocked(url, pairingSourcePointer)
	s.reconcileScrapedURLLocked(url)
	source := s.pairingSource
	m.mu.Unlock()

	if source != pairingSourceScrape {
		t.Errorf("agreement should retire the scan by flipping the source, got %q", source)
	}
	if strings.Contains(logBuf.String(), "disagrees") {
		t.Errorf("agreement logged the drift tripwire:\n%s", logBuf.String())
	}
	if got := m.Get(s.ID); got.PairingURL == nil || *got.PairingURL != url {
		t.Errorf("agreement changed the URL: %v", got.PairingURL)
	}
}

func TestBridgeIsDescendantBounds(t *testing.T) {
	t.Run("cycle-safe", func(t *testing.T) {
		if bridgeIsDescendant(2, 100, map[int]int{2: 3, 3: 2}) {
			t.Error("cycle walked to a false match")
		}
	})
	t.Run("depth-bounded", func(t *testing.T) {
		ps := map[int]int{}
		for pid := 2; pid <= 2+bridgeMaxAncestryDepth+5; pid++ {
			ps[pid] = pid - 1
		}
		deep := 2 + bridgeMaxAncestryDepth + 5
		if bridgeIsDescendant(deep, 1, ps) {
			t.Error("walk exceeded the ancestry depth bound")
		}
		if !bridgeIsDescendant(4, 2, ps) {
			t.Error("short chain within the bound should match")
		}
	})
	t.Run("nonpositive pids never match", func(t *testing.T) {
		if bridgeIsDescendant(0, 0, map[int]int{}) || bridgeIsDescendant(5, 0, map[int]int{5: 0}) {
			t.Error("pid 0 matched")
		}
	})
}

/* ---------- end-to-end through Create: real readLoop + watcher wiring ---------- */

// waitExited spins until the PTY exit path lands so a test never leaks its
// fake claude past its own scope.
func waitExited(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := m.Get(id)
		if s == nil || s.State == StateExited || s.State == StateError {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s never exited", id)
}

func TestBridgeForeignPointerRidesScrapeFallbackE2E(t *testing.T) {
	// empty ps map: the stale pointer's pid has no path to the spawned pid
	overrideBridgeEnv(t, map[int]int{})
	testutil.FakeClaude(t)
	m := testManager(t, nil)
	folder := testutil.ResolvedTempDir(t)
	writePointer(t, folder, map[string]any{"environmentId": "env_stale", "pid": 1, "procStart": nowProcStart()})

	s, err := m.Create(CreateOpts{Folder: folder})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(s.ID, ""); waitExited(t, m, s.ID) }()

	if got := m.WaitForReady(s.ID, 5*time.Second); got != "ready" {
		t.Fatalf("WaitForReady = %q, want ready via scrape fallback", got)
	}
	want := "https://claude.ai/remote/abc123" // FakeClaude's printed URL
	got := m.Get(s.ID)
	if got.PairingURL == nil || *got.PairingURL != want {
		t.Fatalf("scrape fallback URL: want %q, got %v", want, got.PairingURL)
	}
	if n, url := readyEntries(m, s.ID); n != 1 || url != want {
		t.Errorf("journal: want one session.ready carrying the scraped URL, got n=%d url=%q", n, url)
	}
}

func TestBridgeScrapedURLWinsOnDisagreementE2E(t *testing.T) {
	overrideBridgeEnv(t, map[int]int{})
	bridgePollWindow = 5 * time.Second // the pointer must get its chance before the stub prints

	// a claude stub that stays silent long enough for the pointer to win
	// ready, then prints a URL that disagrees with the constructed one
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 1\necho \"https://claude.ai/remote/scraped999\"\nexec sleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logBuf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	m := testManager(t, nil)
	folder := testutil.ResolvedTempDir(t)
	s, err := m.Create(CreateOpts{Folder: folder})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(s.ID, ""); waitExited(t, m, s.ID) }()

	// the pointer names the real spawned pid, written just after spawn — the
	// same overwrite-after-launch shape the CLI produces
	m.mu.Lock()
	spawnPID := m.sessions[s.ID].cmd.Process.Pid
	m.mu.Unlock()
	writePointer(t, folder, map[string]any{"environmentId": "env_pointer", "pid": spawnPID, "procStart": nowProcStart()})

	if got := m.WaitForReady(s.ID, 5*time.Second); got != "ready" {
		t.Fatalf("WaitForReady = %q", got)
	}
	constructed := "https://claude.ai/code?environment=env_pointer"
	if n, url := readyEntries(m, s.ID); n != 1 || url != constructed {
		t.Fatalf("pointer should win ready exactly once with %q, got n=%d url=%q", constructed, n, url)
	}

	// the scrape speaks at ~1s and must overwrite the constructed URL
	scraped := "https://claude.ai/remote/scraped999"
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := m.Get(s.ID)
		if got.PairingURL != nil && *got.PairingURL == scraped {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("scraped URL never overwrote the constructed one: %v", got.PairingURL)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if n, _ := readyEntries(m, s.ID); n != 1 {
		t.Errorf("overwrite produced a second session.ready (n=%d)", n)
	}
	if n := strings.Count(logBuf.String(), "disagrees"); n != 1 {
		t.Errorf("drift tripwire logged %d times, want exactly 1\n%s", n, logBuf.String())
	}
}

// syncBuffer makes captured log output safe to read while session goroutines
// may still be writing.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
