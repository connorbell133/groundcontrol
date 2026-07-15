package sessions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/claudex"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/testutil"
	"github.com/connorbell133/groundcontrol/internal/util"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

/* ---------- fixtures ---------- */

// regHarness swaps the registry poller's exec seams for in-memory answers, so
// loop tests need no real CLI, no real ps, and no wall-clock cadence.
type regHarness struct {
	mu      sync.Mutex
	rows    []claudex.Agent
	err     error
	ps      map[int]int
	calls   int
	onQuery func() // optional gate, invoked outside the harness lock
}

func (h *regHarness) query(time.Duration) ([]claudex.Agent, error) {
	h.mu.Lock()
	h.calls++
	rows := append([]claudex.Agent(nil), h.rows...)
	err := h.err
	hook := h.onQuery
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return []claudex.Agent{}, err
	}
	return rows, nil
}

func (h *regHarness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *regHarness) set(rows []claudex.Agent, err error) {
	h.mu.Lock()
	h.rows, h.err = rows, err
	h.mu.Unlock()
}

func (h *regHarness) setPs(ps map[int]int) {
	h.mu.Lock()
	h.ps = ps
	h.mu.Unlock()
}

func (h *regHarness) setHook(f func()) {
	h.mu.Lock()
	h.onQuery = f
	h.mu.Unlock()
}

// installRegistryHarness swaps the exec seams and shrinks the cadence tiers,
// restoring both once the loop has parked. Tests using it must not call
// t.Parallel (package vars), and must arrange for the loop to park — kill
// their sessions or cancel their context — before test end.
func installRegistryHarness(t *testing.T, m *Manager) *regHarness {
	t.Helper()
	h := &regHarness{}
	prevQuery, prevPs, prevProbe := registryQuery, psParentMap, registryProbeVersion
	prevFast, prevSlow, prevCap, prevFloor := registryFastInterval, registrySlowInterval, registryFailureCap, registryGraceFloor
	registryQuery = h.query
	psParentMap = func() map[int]int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ps
	}
	registryProbeVersion = func() (string, error) { return "2.1.210", nil }
	registryFastInterval = 5 * time.Millisecond
	registrySlowInterval = 15 * time.Millisecond
	registryFailureCap = 60 * time.Millisecond
	registryGraceFloor = 40 * time.Millisecond
	t.Cleanup(func() {
		// the loop reads these package vars every tick — wait for it to park
		// before restoring so a straggler tick can't race the writes
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.Lock()
			running := m.regRunning
			m.mu.Unlock()
			if !running {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		registryQuery, psParentMap, registryProbeVersion = prevQuery, prevPs, prevProbe
		registryFastInterval, registrySlowInterval, registryFailureCap, registryGraceFloor = prevFast, prevSlow, prevCap, prevFloor
	})
	return h
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func killSessionAndWait(t *testing.T, m *Manager, id string) {
	t.Helper()
	m.Kill(id, "test")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := m.Get(id)
		if s == nil || s.State == StateExited || s.State == StateError {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("session %s did not exit after kill", id)
}

// createFakeSession launches through the real Create path against the PATH
// fake claude; the caller must have installed testutil.FakeClaude first.
func createFakeSession(t *testing.T, m *Manager, folder string) Session {
	t.Helper()
	s, err := m.Create(CreateOpts{Folder: folder})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { killSessionAndWait(t, m, s.ID) })
	return s
}

func spawnedPid(m *Manager, id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func sessionCwd(m *Manager, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s.cwd
	}
	return ""
}

// insertLive plants a synthetic live session for apply-level tests that need
// no real process behind the record.
func insertLive(t *testing.T, m *Manager, id, cwd string, state State) *liveSession {
	t.Helper()
	s := &liveSession{Session: Session{ID: id, Name: id, Folder: cwd, State: state, StartedAt: util.NowISO()}, cwd: cwd}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func managerOverJournal(t *testing.T, dataDir string, roots []string) *Manager {
	t.Helper()
	jnl := journal.New(dataDir)
	bus := events.NewBus(jnl)
	ws := workspace.New(t.TempDir(), jnl)
	browser := browse.New()
	browser.Configure(roots, false)
	return NewManager(jnl, bus, ws, browser)
}

func journalEntries(m *Manager, event, id string) []map[string]any {
	out := []map[string]any{}
	for _, e := range m.journal.Read() {
		if e["event"] == event && (id == "" || e["id"] == id) {
			out = append(out, e)
		}
	}
	return out
}

/* ---------- pure join ---------- */

func TestJoinRegistryAncestry(t *testing.T) {
	t.Parallel()
	// two same-dir launches: A spawned pid 100, B spawned pid 200; the
	// characterization topology is launcher → forked server → session child
	snaps := []regSessionSnap{
		{id: "a", pid: 100, cwd: "/w/shared"},
		{id: "b", pid: 200, cwd: "/w/shared"},
	}
	ps := map[int]int{
		101: 100, 110: 101, 111: 101, // A: server 101, sessions 110 + 111
		201: 200, 210: 201, // B: server 201, session 210
		300: 1, // manual claude in the same folder
	}
	rows := []claudex.Agent{
		{PID: 111, Cwd: "/w/shared", SessionID: "u-a2", Name: "gc-a-2", Status: "idle", StartedAt: 2000},
		{PID: 110, Cwd: "/w/shared", SessionID: "u-a1", Name: "gc-a-1", Status: "busy", StartedAt: 1000},
		{PID: 210, Cwd: "/w/shared", SessionID: "u-b1", Name: "gc-b-1", Status: "compacting", StartedAt: 1500},
		{PID: 300, Cwd: "/w/shared", SessionID: "u-m", Name: "manual", Status: "idle", StartedAt: 500},
		{PID: 400, Cwd: "/w/other", SessionID: "u-x", Name: "elsewhere", Status: "busy", StartedAt: 500},
	}
	joins := joinRegistry(snaps, rows, ps)

	a := joins["a"]
	if !a.rowSeen || a.uuid != "u-a1" || a.activity != "busy" {
		t.Fatalf("a joined wrong: %+v (primary must be the earliest-started descendant)", a)
	}
	aExtras := map[string]string{}
	for _, e := range a.extras {
		aExtras[e.name] = e.status
	}
	if len(aExtras) != 2 || aExtras["gc-a-2"] != "idle" || aExtras["manual"] != "idle" {
		t.Errorf("a extras = %v, want later descendant gc-a-2 and the manual folder-mate", aExtras)
	}

	b := joins["b"]
	if !b.rowSeen || b.uuid != "u-b1" {
		t.Fatalf("b joined wrong: %+v", b)
	}
	if b.activity != "" {
		t.Errorf("unknown status %q must read as absent, got %q", "compacting", b.activity)
	}
	// no cross-binding: A's descendants never appear on B, and vice versa —
	// only the unowned manual session is shared
	bExtras := map[string]string{}
	for _, e := range b.extras {
		bExtras[e.name] = e.status
	}
	if len(bExtras) != 1 || bExtras["manual"] != "idle" {
		t.Errorf("b extras = %v, want only the manual folder-mate", bExtras)
	}
}

func TestJoinRegistryAncestryBounds(t *testing.T) {
	t.Parallel()
	snaps := []regSessionSnap{{id: "a", pid: 100, cwd: "/w/a"}}

	// cycle in the ps snapshot: the walk terminates and the row never joins
	cycle := map[int]int{110: 111, 111: 110}
	joins := joinRegistry(snaps, []claudex.Agent{{PID: 110, Cwd: "/w/elsewhere", SessionID: "u-cycle"}}, cycle)
	if joins["a"].rowSeen {
		t.Errorf("row in a ppid cycle must never join: %+v", joins["a"])
	}

	// a chain deeper than the bound does not join; a shallow chain does
	deep := map[int]int{}
	pid := 100
	for i := 0; i < maxAncestryDepth+2; i++ {
		deep[pid+i+1] = pid + i
	}
	tooDeep := pid + maxAncestryDepth + 2
	joins = joinRegistry(snaps, []claudex.Agent{{PID: tooDeep, Cwd: "/w/elsewhere", SessionID: "u-deep"}}, deep)
	if joins["a"].rowSeen {
		t.Errorf("row beyond the ancestry bound must not join")
	}
	joins = joinRegistry(snaps, []claudex.Agent{{PID: 102, Cwd: "/w/elsewhere", SessionID: "u-near"}}, deep)
	if !joins["a"].rowSeen || joins["a"].uuid != "u-near" {
		t.Errorf("shallow descendant should join: %+v", joins["a"])
	}

	// a row with no path to any launch never joins
	joins = joinRegistry(snaps, []claudex.Agent{{PID: 999, Cwd: "/w/elsewhere", SessionID: "u-stranger"}}, map[int]int{999: 1})
	if joins["a"].rowSeen {
		t.Errorf("unrelated row must not join: %+v", joins["a"])
	}
}

func TestJoinRegistryFallback(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	after := base.Add(5 * time.Second).UnixMilli()
	before := base.Add(-5 * time.Second).UnixMilli()

	t.Run("unambiguous match binds", func(t *testing.T) {
		t.Parallel()
		snaps := []regSessionSnap{{id: "a", pid: 100, cwd: "/w/a", startedAt: base}}
		rows := []claudex.Agent{{PID: 110, Cwd: "/w/a", SessionID: "u-1", Status: "busy", StartedAt: after}}
		j := joinRegistry(snaps, rows, nil)["a"]
		if !j.rowSeen || j.uuid != "u-1" || j.activity != "busy" {
			t.Errorf("unambiguous fallback should bind: %+v", j)
		}
	})

	t.Run("row started before launch never binds", func(t *testing.T) {
		t.Parallel()
		snaps := []regSessionSnap{{id: "a", pid: 100, cwd: "/w/a", startedAt: base}}
		rows := []claudex.Agent{{PID: 110, Cwd: "/w/a", SessionID: "u-old", StartedAt: before}}
		j := joinRegistry(snaps, rows, nil)["a"]
		if j.rowSeen {
			t.Errorf("pre-launch row bound via fallback: %+v", j)
		}
		// still visible as a folder-mate
		if len(j.extras) != 1 {
			t.Errorf("pre-launch row should list as an extra: %+v", j.extras)
		}
	})

	t.Run("two candidate rows is no capture", func(t *testing.T) {
		t.Parallel()
		snaps := []regSessionSnap{{id: "a", pid: 100, cwd: "/w/a", startedAt: base}}
		rows := []claudex.Agent{
			{PID: 110, Cwd: "/w/a", SessionID: "u-1", StartedAt: after},
			{PID: 120, Cwd: "/w/a", SessionID: "u-2", StartedAt: after},
		}
		j := joinRegistry(snaps, rows, nil)["a"]
		if j.rowSeen || j.uuid != "" {
			t.Errorf("ambiguous fallback must not guess: %+v", j)
		}
		if len(j.extras) != 2 {
			t.Errorf("ambiguous rows should still list as extras: %+v", j.extras)
		}
	})

	t.Run("one row matching two sessions is no capture", func(t *testing.T) {
		t.Parallel()
		snaps := []regSessionSnap{
			{id: "a", pid: 100, cwd: "/w/a", startedAt: base},
			{id: "b", pid: 200, cwd: "/w/a", startedAt: base},
		}
		rows := []claudex.Agent{{PID: 110, Cwd: "/w/a", SessionID: "u-1", StartedAt: after}}
		joins := joinRegistry(snaps, rows, nil)
		if joins["a"].rowSeen || joins["b"].rowSeen {
			t.Errorf("row matching two launches must bind to neither: %+v %+v", joins["a"], joins["b"])
		}
	})

	t.Run("captured uuid still binds without ps", func(t *testing.T) {
		t.Parallel()
		snaps := []regSessionSnap{{id: "a", pid: 100, cwd: "/w/a", startedAt: base, uuid: "u-known"}}
		rows := []claudex.Agent{{PID: 110, Cwd: "/w/elsewhere", SessionID: "u-known", Status: "idle", StartedAt: before}}
		j := joinRegistry(snaps, rows, nil)["a"]
		if !j.rowSeen || j.activity != "idle" {
			t.Errorf("uuid match should bind regardless of cwd and ps: %+v", j)
		}
	})
}

func TestNormalizeActivity(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"busy": "busy", "idle": "idle", "": "", "compacting": "", "BUSY": ""}
	for in, want := range cases {
		if got := normalizeActivity(in); got != want {
			t.Errorf("normalizeActivity(%q) = %q, want %q", in, got, want)
		}
	}
}

/* ---------- apply: capture, guard, grace ---------- */

func TestApplyRegistryTickFirstCaptureWins(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	s := insertLive(t, m, "s1", "/w/a", StateReady)
	now := time.Now()
	window := time.Minute
	snaps := []regSessionSnap{{id: "s1"}}

	captures, _ := m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, uuid: "conv-1", activity: "busy"}}, window, now)
	if len(captures) != 1 || captures[0].uuid != "conv-1" {
		t.Fatalf("first tick should capture conv-1, got %+v", captures)
	}
	got := m.Get("s1")
	if got.ClaudeSessionID == nil || *got.ClaudeSessionID != "conv-1" {
		t.Fatalf("claudeSessionId = %v, want conv-1", got.ClaudeSessionID)
	}
	if got.Activity == nil || *got.Activity != "busy" {
		t.Errorf("activity = %v, want busy", got.Activity)
	}

	// a later disagreeing sessionId never overwrites and never re-captures
	captures, _ = m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, uuid: "conv-2", activity: "idle"}}, window, now.Add(time.Second))
	if len(captures) != 0 {
		t.Errorf("disagreement must not produce a capture: %+v", captures)
	}
	got = m.Get("s1")
	if got.ClaudeSessionID == nil || *got.ClaudeSessionID != "conv-1" {
		t.Errorf("first capture must win, got %v", got.ClaudeSessionID)
	}
	if !s.claudeIDConflictLogged {
		t.Errorf("disagreement should be flagged (rate-limited log)")
	}
	// activity still tracks the joined row even when the uuid disagrees
	if got.Activity == nil || *got.Activity != "idle" {
		t.Errorf("activity = %v, want idle", got.Activity)
	}
}

func TestApplyRegistryTickStateGuard(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	for _, state := range []State{StateExited, StateError} {
		id := "dead-" + string(state)
		insertLive(t, m, id, "/w/a", state)
		captures, scans := m.applyRegistryTick(
			[]regSessionSnap{{id: id}},
			map[string]*regJoin{id: {rowSeen: true, uuid: "conv-late", activity: "busy", extras: []extraRow{{key: "k", name: "n"}}}},
			time.Minute, time.Now(),
		)
		if len(scans) != 0 {
			t.Errorf("%s: non-live session produced scan work: %+v", state, scans)
		}
		if len(captures) != 0 {
			t.Errorf("%s: capture on a non-live session (pid-reuse hazard): %+v", state, captures)
		}
		got := m.Get(id)
		if got.ClaudeSessionID != nil || got.Activity != nil || got.ExtraSessions != nil {
			t.Errorf("%s: registry writes leaked onto a non-live session: %+v", state, got)
		}
	}
}

func TestApplyRegistryTickRowGoneClearsActivity(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	insertLive(t, m, "s1", "/w/a", StateReady)
	now := time.Now()
	snaps := []regSessionSnap{{id: "s1"}}
	window := time.Minute

	m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, activity: "busy"}}, window, now)
	// a successful query with the row gone clears immediately — the registry
	// answered authoritatively, even though the grace window hasn't passed
	m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {}}, window, now.Add(time.Millisecond))
	got := m.Get("s1")
	if got.Activity != nil {
		t.Errorf("activity = %q, want cleared on an authoritative miss", *got.Activity)
	}
	if got.State != StateReady {
		t.Errorf("state = %s; a vanished row must never drive a transition", got.State)
	}
}

func TestAgeRegistryStateGrace(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	insertLive(t, m, "s1", "/w/a", StateReady)
	now := time.Now()
	window := 100 * time.Millisecond
	m.applyRegistryTick(
		[]regSessionSnap{{id: "s1"}},
		map[string]*regJoin{"s1": {rowSeen: true, activity: "busy", extras: []extraRow{{key: "k1", name: "mate", status: "idle"}}}},
		window, now,
	)

	// a failed query inside the window retains the last answer
	m.ageRegistryState(window, now.Add(50*time.Millisecond))
	got := m.Get("s1")
	if got.Activity == nil || *got.Activity != "busy" {
		t.Fatalf("activity should ride out a failure inside the window, got %v", got.Activity)
	}
	if len(got.ExtraSessions) != 1 {
		t.Fatalf("extras should ride out a failure inside the window, got %v", got.ExtraSessions)
	}

	// past the window both degrade to absence
	m.ageRegistryState(window, now.Add(150*time.Millisecond))
	got = m.Get("s1")
	if got.Activity != nil {
		t.Errorf("activity = %q, want cleared past the grace window", *got.Activity)
	}
	if got.ExtraSessions != nil {
		t.Errorf("extras = %v, want aged out past the window", got.ExtraSessions)
	}
}

func TestExtraRowNameFallback(t *testing.T) {
	t.Parallel()
	// nameless rows are real (IDE/SDK sessions, fresh pre-created ones) and
	// must never reach the card as a blank line
	cases := []struct {
		in   claudex.Agent
		want string
	}{
		{claudex.Agent{Name: "gc-auto-1", SessionID: "aaaabbbb-cccc", PID: 7}, "gc-auto-1"},
		{claudex.Agent{SessionID: "947c45fb-ad3d-453d", PID: 7}, "947c45fb"},
		{claudex.Agent{PID: 61863}, "pid 61863"},
	}
	for _, c := range cases {
		if got := extraRowOf(c.in).name; got != c.want {
			t.Errorf("extraRowOf(%+v).name = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtrasRefreshAndAging(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	insertLive(t, m, "s1", "/w/a", StateReady)
	now := time.Now()
	window := 100 * time.Millisecond
	snaps := []regSessionSnap{{id: "s1"}}

	m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, extras: []extraRow{
		{key: "u-1", name: "mate-1", status: "busy"},
		{key: "u-2", name: "mate-2", status: ""}, // unknown status renders as absent
	}}}, window, now)
	got := m.Get("s1")
	if len(got.ExtraSessions) != 2 {
		t.Fatalf("extras = %+v, want 2", got.ExtraSessions)
	}
	if got.ExtraSessions[0].Name != "mate-1" || got.ExtraSessions[0].Status != "busy" {
		t.Errorf("extras[0] = %+v", got.ExtraSessions[0])
	}
	if got.ExtraSessions[1].Name != "mate-2" || got.ExtraSessions[1].Status != "" {
		t.Errorf("extras[1] = %+v, want empty status", got.ExtraSessions[1])
	}

	// a tick that no longer sees mate-2 keeps it inside the window and drops
	// it once unconfirmed past the window
	m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, extras: []extraRow{{key: "u-1", name: "mate-1", status: "busy"}}}}, window, now.Add(50*time.Millisecond))
	if got = m.Get("s1"); len(got.ExtraSessions) != 2 {
		t.Errorf("extras inside the window = %+v, want both retained", got.ExtraSessions)
	}
	m.applyRegistryTick(snaps, map[string]*regJoin{"s1": {rowSeen: true, extras: []extraRow{{key: "u-1", name: "mate-1", status: "busy"}}}}, window, now.Add(150*time.Millisecond))
	got = m.Get("s1")
	if len(got.ExtraSessions) != 1 || got.ExtraSessions[0].Name != "mate-1" {
		t.Errorf("extras past the window = %+v, want only mate-1", got.ExtraSessions)
	}
}

/* ---------- cadence math ---------- */

func TestNextRegistryInterval(t *testing.T) {
	if got := nextRegistryInterval(registrySlowInterval, true, false); got != registryFastInterval {
		t.Errorf("watched tick = %s, want fast tier", got)
	}
	if got := nextRegistryInterval(registryFastInterval, false, false); got != registrySlowInterval {
		t.Errorf("unwatched tick = %s, want slow tier", got)
	}
	// failures double from the previous interval up to the cap
	d := registrySlowInterval
	d = nextRegistryInterval(d, false, true)
	if d != 2*registrySlowInterval {
		t.Errorf("first failure = %s, want doubled", d)
	}
	d = nextRegistryInterval(d, false, true)
	if d != registryFailureCap {
		t.Errorf("second failure = %s, want the cap", d)
	}
	if d = nextRegistryInterval(d, false, true); d != registryFailureCap {
		t.Errorf("failures past the cap = %s, must hold at the cap", d)
	}
	// success resets to the tier, regardless of how high the backoff went
	if got := nextRegistryInterval(d, true, false); got != registryFastInterval {
		t.Errorf("success after backoff = %s, want fast tier", got)
	}
}

func TestRetentionWindow(t *testing.T) {
	if got := retentionWindow(time.Second); got != registryGraceFloor {
		t.Errorf("small interval window = %s, want the %s floor", got, registryGraceFloor)
	}
	if got := retentionWindow(30 * time.Second); got != time.Minute {
		t.Errorf("large interval window = %s, want 2x", got)
	}
}

func TestJittered(t *testing.T) {
	for i := 0; i < 50; i++ {
		d := jittered(time.Second)
		if d < time.Second || d > time.Second+100*time.Millisecond {
			t.Fatalf("jittered(1s) = %s, want within [1s, 1.1s]", d)
		}
	}
}

func TestSleepInterruptWake(t *testing.T) {
	t.Parallel()
	wake := make(chan struct{}, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		wake <- struct{}{}
	}()
	start := time.Now()
	if !sleepInterrupt(context.Background(), wake, time.Minute, 10*time.Millisecond) {
		t.Fatal("sleep ended by wake should report true")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wake did not cut the slow sleep short: %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepInterrupt(ctx, wake, time.Minute, 10*time.Millisecond) {
		t.Error("cancelled context should end the sleep with false")
	}
}

/* ---------- exit precedence ---------- */

func TestExitForceClearsActivityAndFreezesExtras(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	created := createFakeSession(t, m, folder)
	if outcome := m.WaitForReady(created.ID, 10*time.Second); outcome != "ready" {
		t.Fatalf("WaitForReady = %q", outcome)
	}
	m.mu.Lock()
	ls := m.sessions[created.ID]
	busy := "busy"
	uuid := "conv-exit"
	ls.Activity = &busy
	ls.ClaudeSessionID = &uuid
	ls.ExtraSessions = []ExtraSession{{Name: "frozen-mate", Status: "idle"}}
	m.mu.Unlock()

	killSessionAndWait(t, m, created.ID)
	got := m.Get(created.ID)
	if got.Activity != nil {
		t.Errorf("exit must force-clear activity, got %q", *got.Activity)
	}
	if len(got.ExtraSessions) != 1 || got.ExtraSessions[0].Name != "frozen-mate" {
		t.Errorf("extras must freeze at exit, got %+v", got.ExtraSessions)
	}
	if got.ClaudeSessionID == nil || *got.ClaudeSessionID != "conv-exit" {
		t.Errorf("claudeSessionId must survive exit, got %v", got.ClaudeSessionID)
	}

	// the exit entry carries the UUID flattened, like the debrief fields
	waitFor(t, 5*time.Second, "session.exit journal entry", func() bool {
		return len(journalEntries(m, evSessionExit, created.ID)) == 1
	})
	exit := journalEntries(m, evSessionExit, created.ID)[0]
	if exit["claudeSessionId"] != "conv-exit" {
		t.Errorf("exit entry claudeSessionId = %v, want conv-exit", exit["claudeSessionId"])
	}
}

/* ---------- loop lifecycle and integration ---------- */

func TestRegistryLoopLifecycle(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	h := installRegistryHarness(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartRegistryLoop(ctx)

	// zero live sessions: no loop, no exec
	time.Sleep(20 * registryFastInterval)
	if got := h.count(); got != 0 {
		t.Fatalf("registry exec ran %d times with zero live sessions", got)
	}

	// 0→1 starts the loop
	first := createFakeSession(t, m, folder)
	waitFor(t, 5*time.Second, "registry ticks after first launch", func() bool { return h.count() >= 2 })

	// last exit parks it: the call counter freezes
	killSessionAndWait(t, m, first.ID)
	waitFor(t, 5*time.Second, "loop to park after last exit", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.regRunning
	})
	frozen := h.count()
	time.Sleep(20 * registryFastInterval)
	if got := h.count(); got != frozen {
		t.Fatalf("registry exec ran %d more times after the last exit", got-frozen)
	}

	// the next 0→1 transition starts it again
	second := createFakeSession(t, m, folder)
	waitFor(t, 5*time.Second, "loop restart on the next launch", func() bool { return h.count() > frozen })
	killSessionAndWait(t, m, second.ID)
}

func TestRegistryLoopCaptureActivityAndJournal(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	h := installRegistryHarness(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartRegistryLoop(ctx)

	var seenMu sync.Mutex
	seen := []string{}
	unsub := m.bus.OnEvent(func(e events.LifecycleEvent) {
		seenMu.Lock()
		seen = append(seen, e.Event)
		seenMu.Unlock()
	})
	defer unsub()

	created := createFakeSession(t, m, folder)
	launcher := spawnedPid(m, created.ID)
	cwd := sessionCwd(m, created.ID)
	if launcher == 0 || cwd == "" {
		t.Fatalf("missing launch facts: pid=%d cwd=%q", launcher, cwd)
	}
	// simulate the characterization topology: registry row 88001 is a
	// grandchild of the spawned launcher (via forked server 88000)
	h.setPs(map[int]int{88001: 88000, 88000: launcher})
	h.set([]claudex.Agent{
		{PID: 88001, Cwd: cwd, SessionID: "conv-1", Name: "gc-auto-1", Status: "busy", StartedAt: time.Now().UnixMilli()},
		{PID: 99001, Cwd: cwd, SessionID: "conv-extra", Name: "folder-mate", Status: "idle", StartedAt: time.Now().UnixMilli()},
	}, nil)

	waitFor(t, 10*time.Second, "uuid capture and busy activity", func() bool {
		s := m.Get(created.ID)
		return s.ClaudeSessionID != nil && *s.ClaudeSessionID == "conv-1" &&
			s.Activity != nil && *s.Activity == "busy"
	})
	waitFor(t, 10*time.Second, "folder-mate extra", func() bool {
		s := m.Get(created.ID)
		return len(s.ExtraSessions) == 1 && s.ExtraSessions[0].Name == "folder-mate" && s.ExtraSessions[0].Status == "idle"
	})

	// a later disagreeing sessionId never overwrites the capture
	h.set([]claudex.Agent{
		{PID: 88001, Cwd: cwd, SessionID: "conv-2", Name: "gc-auto-1", Status: "busy", StartedAt: time.Now().UnixMilli()},
	}, nil)
	time.Sleep(20 * registryFastInterval)
	if s := m.Get(created.ID); s.ClaudeSessionID == nil || *s.ClaudeSessionID != "conv-1" {
		t.Errorf("claudeSessionId = %v, first capture must win", s.ClaudeSessionID)
	}

	// exactly one journal capture across all those ticks, and never a bus event
	if entries := journalEntries(m, evSessionClaudeID, created.ID); len(entries) != 1 {
		t.Fatalf("session.claude-id journal entries = %d, want exactly 1", len(entries))
	} else if entries[0]["claudeSessionId"] != "conv-1" {
		t.Errorf("journaled claudeSessionId = %v", entries[0]["claudeSessionId"])
	}
	seenMu.Lock()
	for _, ev := range seen {
		if ev == evSessionClaudeID {
			t.Errorf("session.claude-id must never reach the bus (R8)")
		}
	}
	seenMu.Unlock()

	// successful query with the row gone: activity clears, uuid stays, state holds.
	// Wait for the scrape-driven ready flip first — the registry harness resolves
	// far faster than the PTY's first read, so asserting "state held" before ready
	// exists would race the readLoop rather than test the guard.
	waitFor(t, 10*time.Second, "ready via scrape before the row-loss check", func() bool {
		return m.Get(created.ID).State == StateReady
	})
	h.set([]claudex.Agent{}, nil)
	waitFor(t, 10*time.Second, "activity cleared after the row vanished", func() bool {
		return m.Get(created.ID).Activity == nil
	})
	if s := m.Get(created.ID); s.State != StateReady || s.ClaudeSessionID == nil {
		t.Errorf("row loss must not touch state or uuid: %+v", s)
	}

	killSessionAndWait(t, m, created.ID)
	waitFor(t, 5*time.Second, "exit entry", func() bool {
		return len(journalEntries(m, evSessionExit, created.ID)) == 1
	})
	if exit := journalEntries(m, evSessionExit, created.ID)[0]; exit["claudeSessionId"] != "conv-1" {
		t.Errorf("exit entry claudeSessionId = %v, want conv-1", exit["claudeSessionId"])
	}
}

func TestRegistryLoopFailuresKeepSessionsWorking(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	h := installRegistryHarness(t, m)
	h.set(nil, errors.New("registry down"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartRegistryLoop(ctx)

	created := createFakeSession(t, m, folder)
	waitFor(t, 10*time.Second, "several failed ticks", func() bool { return h.count() >= 3 })

	s := m.Get(created.ID)
	if s.State != StateStarting && s.State != StateReady {
		t.Errorf("session must keep working through registry failures: %+v", s)
	}
	if s.Activity != nil || s.ClaudeSessionID != nil {
		t.Errorf("failed queries must not invent enrichment: %+v", s)
	}
	if entries := journalEntries(m, evSessionClaudeID, ""); len(entries) != 0 {
		t.Errorf("failed queries journaled %d claude-id entries", len(entries))
	}
	killSessionAndWait(t, m, created.ID)
}

func TestRegistryLoopExecOutsideLock(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	h := installRegistryHarness(t, m)
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	h.setHook(func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartRegistryLoop(ctx)

	created := createFakeSession(t, m, folder)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("registry query never started")
	}

	// while the exec is mid-flight, m.mu must be free — if the loop held the
	// lock across the query, this acquisition would stall until release
	acquired := make(chan struct{})
	go func() {
		m.mu.Lock()
		_ = len(m.sessions)
		m.mu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Error("m.mu held across the registry exec")
	}
	close(release)
	h.setHook(nil)
	killSessionAndWait(t, m, created.ID)
}

func TestRegistryVersionFloorDisablesPolling(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	h := installRegistryHarness(t, m)
	registryProbeVersion = func() (string, error) { return "2.1.100", nil } // below the agents --json floor
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartRegistryLoop(ctx)

	created := createFakeSession(t, m, folder)
	waitFor(t, 5*time.Second, "loop to park on the version floor", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.regRunning
	})
	time.Sleep(10 * registryFastInterval)
	if got := h.count(); got != 0 {
		t.Errorf("an old CLI must disable polling entirely, saw %d execs", got)
	}
	killSessionAndWait(t, m, created.ID)
}

/* ---------- restart honesty ---------- */

func TestRestartHonestyLostAndLanded(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	dataDir := t.TempDir()

	before := managerOverJournal(t, dataDir, []string{root})
	before.journal.Append(map[string]any{"event": evSessionStart, "id": "lost1", "name": "l1", "folder": root})
	before.journal.Append(map[string]any{"event": evSessionClaudeID, "id": "lost1", "claudeSessionId": "uuid-lost"})
	before.journal.Append(map[string]any{"event": evSessionStart, "id": "lost2", "name": "l2", "folder": root, "spawnMode": "worktree"})
	before.journal.Append(map[string]any{"event": evSessionStart, "id": "done1", "name": "d1", "folder": root})
	before.journal.Append(map[string]any{"event": evSessionExit, "id": "done1", "code": 0, "claudeSessionId": "uuid-done"})
	before.journal.Append(map[string]any{"event": evSessionStart, "id": "done2", "name": "d2", "folder": root})
	before.journal.Append(map[string]any{"event": evSessionExit, "id": "done2", "code": 0})

	// a fresh manager over the same journal is the restarted runner
	after := managerOverJournal(t, dataDir, []string{root})

	lost := map[string]LostSession{}
	for _, l := range after.ListLost() {
		lost[l.ID] = l
	}
	if len(lost) != 2 {
		t.Fatalf("lost = %+v, want lost1 and lost2", lost)
	}
	if lost["lost1"].ClaudeSessionID == nil || *lost["lost1"].ClaudeSessionID != "uuid-lost" {
		t.Errorf("lost1 claudeSessionId = %v, want uuid-lost", lost["lost1"].ClaudeSessionID)
	}
	if lost["lost2"].ClaudeSessionID != nil {
		t.Errorf("lost2 without a claude-id entry must expose nothing, got %v", *lost["lost2"].ClaudeSessionID)
	}

	landed := map[string]LandedSession{}
	for _, l := range after.ListLanded() {
		landed[l.ID] = l
	}
	if len(landed) != 2 {
		t.Fatalf("landed = %+v, want done1 and done2", landed)
	}
	if landed["done1"].ClaudeSessionID == nil || *landed["done1"].ClaudeSessionID != "uuid-done" {
		t.Errorf("done1 claudeSessionId = %v, want uuid-done", landed["done1"].ClaudeSessionID)
	}
	if landed["done2"].ClaudeSessionID != nil {
		t.Errorf("done2 without a flattened uuid must expose nothing, got %v", *landed["done2"].ClaudeSessionID)
	}
}
