package sessions

import (
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
	return NewManager(jnl, bus, ws, browser)
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
