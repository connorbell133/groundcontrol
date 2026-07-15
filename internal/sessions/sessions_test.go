package sessions

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/testutil"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

// testManager builds an isolated manager wired the way main() wires it.
func testManager(t *testing.T, roots []string) *Manager {
	t.Helper()
	if roots == nil {
		roots = []string{testutil.ResolvedTempDir(t)}
	}
	jnl := journal.New(t.TempDir())
	bus := events.NewBus(jnl)
	ws := workspace.New(t.TempDir(), jnl)
	browser := browse.New()
	browser.Configure(roots, false)
	m := NewManager(jnl, bus, ws, browser)
	t.Cleanup(func() {
		// drain watcher goroutines before later-registered cleanups restore the
		// package seams they read (cleanups run LIFO, so this runs first)
		for _, s := range m.List() {
			m.Kill(s.ID, "test-cleanup")
		}
		m.watchers.Wait()
	})
	return m
}

func TestLastMeaningfulLine(t *testing.T) {
	t.Parallel()
	// meaningful text followed by 8 junk chunks: only the tail is scanned
	pushedOut := []string{"real text\n", "more real\n"}
	for i := 0; i < 8; i++ {
		pushedOut = append(pushedOut, "───\n")
	}
	cases := []struct {
		name string
		log  []string
		want string
	}{
		{"empty log", []string{}, ""},
		{"nil log", nil, ""},
		{"single line", []string{"hello world\n"}, "hello world"},
		{"picks last meaningful of tail", []string{"one\ntwo\n", "─────\n⠋⠙\n"}, "two"},
		{"box drawing only", []string{"─────\n│ │\n"}, ""},
		{"spinners and rules only", []string{"⠋ ⠙ ⠹\n", "|/-\\\n", "· • ●\n"}, ""},
		{"spinner with text is meaningful", []string{"⠋ Loading files\n"}, "⠋ Loading files"},
		{"chunks join before splitting", []string{"hel", "lo\n─\n"}, "hello"},
		{"only last 8 chunks considered", pushedOut, ""},
		{"crlf splits", []string{"first\r\nsecond"}, "second"},
		{
			"120-rune truncation",
			[]string{strings.Repeat("a", 130) + "\n"},
			strings.Repeat("a", 120),
		},
		{
			// needs an ASCII alnum to count as meaningful; truncation is rune-based
			"truncation counts runes not bytes",
			[]string{"x" + strings.Repeat("é", 129) + "\n"},
			"x" + strings.Repeat("é", 119),
		},
		{"non-ASCII with no alnum is junk", []string{strings.Repeat("é", 10) + "\n"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lastMeaningfulLine(tc.log); got != tc.want {
				t.Errorf("lastMeaningfulLine(%q) = %q, want %q", tc.log, got, tc.want)
			}
		})
	}
}

func TestWaitForReadyMissingSession(t *testing.T) {
	t.Parallel()
	m := testManager(t, nil)
	if got := m.WaitForReady("nope", 10*time.Millisecond); got != "dead" {
		t.Errorf("WaitForReady on a missing session = %q, want dead", got)
	}
}

func TestListLostSessions(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{root})

	m.journal.Append(map[string]any{"event": evSessionStart, "id": "lost1", "name": "one", "folder": root})
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "done1", "name": "two", "folder": root})
	m.journal.Append(map[string]any{"event": evSessionExit, "id": "done1", "code": 0})

	lost := m.ListLost()
	if len(lost) != 1 || lost[0].ID != "lost1" {
		t.Fatalf("expected one lost session lost1, got %+v", lost)
	}
	if lost[0].SpawnMode != string(workspace.SpawnSameDir) || lost[0].PermissionMode != "default" {
		t.Errorf("expected defaulted spawnMode/permissionMode, got %+v", lost[0])
	}

	// headstone dismissal splices the cache
	removed, err := m.Remove("lost1")
	if err != nil || !removed {
		t.Fatalf("Remove(lost1) = %v, %v", removed, err)
	}
	if again := m.ListLost(); len(again) != 0 {
		t.Errorf("dismissed lost session still listed: %+v", again)
	}
}

func TestRecentLaunchesDedupAndOrder(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{root})

	m.journal.Append(map[string]any{"event": evSessionStart, "id": "a", "name": "n1", "folder": root})
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "b", "name": "n2", "folder": root, "spawnMode": "worktree"})
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "c", "name": "n3", "folder": root}) // dup of first key

	out := m.RecentLaunches(10)
	if len(out) != 2 {
		t.Fatalf("expected 2 deduped launches, got %+v", out)
	}
	// newest first; the duplicate keeps its most recent occurrence
	if out[0].Name != "n3" || out[1].Name != "n2" {
		t.Errorf("unexpected order: %+v", out)
	}
}

func TestScanCapacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		text      string
		wantOK    bool
		used, max int
	}{
		{"no line", "·✔︎· Connected · repo · main", false, 0, 0},
		{"plain line", "Capacity: 1/32 · New sessions will be created in the current directory", true, 1, 32},
		{"redraw supersedes", "Capacity: 1/32 · x\nstuff\nCapacity: 4/32 · x", true, 4, 32},
		{"split then joined", "Capaci" + "ty: 2/32", true, 2, 32},
		{"malformed numbers ignored", "Capacity: /32", false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			used, max, ok := scanCapacity(tc.text)
			if ok != tc.wantOK || used != tc.used || max != tc.max {
				t.Errorf("scanCapacity(%q) = %d, %d, %v; want %d, %d, %v", tc.text, used, max, ok, tc.used, tc.max, tc.wantOK)
			}
		})
	}
}

func TestBranchStateAfterRemove(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	gonePath := filepath.Join(repo, "no-such-worktree")

	// a surviving worktree directory wins over any ref inspection
	if got := branchStateAfterRemove(repo, repo, "gc/x"); got != branchWorktreeKept {
		t.Errorf("existing path = %q, want %q", got, branchWorktreeKept)
	}

	// branch deleted by cleanup: nothing beyond the base survives
	if got := branchStateAfterRemove(repo, gonePath, "gc/deleted"); got != branchMerged {
		t.Errorf("missing branch = %q, want %q", got, branchMerged)
	}

	// branch with a commit main lacks
	testutil.MustGit(t, repo, "switch", "-c", "gc/orbit")
	testutil.CommitFile(t, repo, "orbit.txt", "orbit work", "")
	testutil.MustGit(t, repo, "switch", "main")
	if got := branchStateAfterRemove(repo, gonePath, "gc/orbit"); got != branchInOrbit {
		t.Errorf("unmerged branch = %q, want %q", got, branchInOrbit)
	}

	// once merged into the default branch it stops being in orbit
	testutil.MustGit(t, repo, "merge", "gc/orbit")
	if got := branchStateAfterRemove(repo, gonePath, "gc/orbit"); got != branchMerged {
		t.Errorf("merged branch = %q, want %q", got, branchMerged)
	}
}

func TestListLanded(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{root})

	// worktree session whose exit entry carries a debrief
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "w1", "name": "wt", "folder": root, "spawnMode": "worktree", "branch": "main", "permissionMode": "acceptEdits"})
	m.journal.Append(map[string]any{"event": evSessionExit, "id": "w1", "code": 0, "filesChanged": 2, "insertions": 5, "deletions": 1, "uncommitted": 1, "branchState": "in-orbit"})
	// same-dir session: no debrief fields on the exit entry
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "s1", "name": "plain", "folder": root})
	m.journal.Append(map[string]any{"event": evSessionExit, "id": "s1", "code": 1})
	// never exited → not landed (it's lost or live, not landed)
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "r1", "name": "running", "folder": root})
	// exited but the manager still lists it → excluded, it's already in "sessions"
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "live1", "name": "listed", "folder": root})
	m.journal.Append(map[string]any{"event": evSessionExit, "id": "live1", "code": 0})
	m.mu.Lock()
	m.sessions["live1"] = &liveSession{Session: Session{ID: "live1", State: StateExited}}
	m.mu.Unlock()
	// folder outside the configured roots → excluded
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "out1", "name": "outside", "folder": "/definitely/not/in/roots"})
	m.journal.Append(map[string]any{"event": evSessionExit, "id": "out1", "code": 0})

	landed := m.ListLanded()
	if len(landed) != 2 {
		t.Fatalf("expected 2 landed sessions, got %+v", landed)
	}
	// newest start first
	if landed[0].ID != "s1" || landed[1].ID != "w1" {
		t.Fatalf("unexpected order: %+v", landed)
	}
	s1 := landed[0]
	if s1.Debrief != nil {
		t.Errorf("same-dir session carries a debrief: %+v", s1.Debrief)
	}
	if s1.SpawnMode != string(workspace.SpawnSameDir) || s1.PermissionMode != "default" {
		t.Errorf("defaults not applied: %+v", s1)
	}
	if s1.ExitCode == nil || *s1.ExitCode != 1 {
		t.Errorf("s1 exitCode = %v, want 1", s1.ExitCode)
	}
	w1 := landed[1]
	if w1.Debrief == nil {
		t.Fatalf("worktree session missing debrief: %+v", w1)
	}
	if w1.Debrief.FilesChanged != 2 || w1.Debrief.Insertions != 5 || w1.Debrief.Deletions != 1 || w1.Debrief.Uncommitted != 1 || w1.Debrief.BranchState != "in-orbit" {
		t.Errorf("w1 debrief = %+v", w1.Debrief)
	}
	if w1.Branch == nil || *w1.Branch != "main" || w1.SpawnMode != "worktree" || w1.PermissionMode != "acceptEdits" {
		t.Errorf("w1 launch config = %+v", w1)
	}
	if w1.StartedAt == "" || w1.ExitedAt == "" {
		t.Errorf("w1 timestamps missing: %+v", w1)
	}
	if w1.ExitCode == nil || *w1.ExitCode != 0 {
		t.Errorf("w1 exitCode = %v, want 0", w1.ExitCode)
	}
}

func TestListLandedCap(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{root})
	for i := 0; i < landedCap+5; i++ {
		id := fmt.Sprintf("id-%02d", i)
		m.journal.Append(map[string]any{"event": evSessionStart, "id": id, "name": id, "folder": root})
		m.journal.Append(map[string]any{"event": evSessionExit, "id": id, "code": 0})
	}
	landed := m.ListLanded()
	if len(landed) != landedCap {
		t.Fatalf("expected cap of %d, got %d", landedCap, len(landed))
	}
	// newest first: the last-started session leads, the oldest five fall off
	if landed[0].ID != fmt.Sprintf("id-%02d", landedCap+4) || landed[landedCap-1].ID != "id-05" {
		t.Errorf("unexpected window: first %s, last %s", landed[0].ID, landed[landedCap-1].ID)
	}
}
