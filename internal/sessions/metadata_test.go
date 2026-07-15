package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

const (
	prLine1 = `{"type":"pr-link","sessionId":"abc","prNumber":9,"prUrl":"https://github.com/o/r/pull/9"}` + "\n"
	prLine2 = `{"type":"pr-link","sessionId":"abc","prNumber":12,"prUrl":"https://github.com/o/r/pull/12"}` + "\n"
	chatter = `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":"hello"}}` + "\n"
)

func TestScanPRLink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    *PRLink
	}{
		{"single record among chatter", chatter + prLine1 + chatter, &PRLink{Number: 9, URL: "https://github.com/o/r/pull/9"}},
		{"multiple records yield the newest", prLine1 + chatter + prLine2, &PRLink{Number: 12, URL: "https://github.com/o/r/pull/12"}},
		{"only chatter yields absence", chatter, nil},
		{"empty file yields absence", "", nil},
		{"pr-link without a url is skipped", `{"type":"pr-link","prNumber":3}` + "\n", nil},
		{"torn final line is skipped", prLine1 + `{"type":"pr-link","prNumber":99,"prUrl":"https://git`, &PRLink{Number: 9, URL: "https://github.com/o/r/pull/9"}},
		{"non-json garbage lines are skipped", "not json at all\n" + prLine1 + "\x00\x01garbage\n", &PRLink{Number: 9, URL: "https://github.com/o/r/pull/9"}},
		{
			// a line past the 8MB scanner buffer aborts the scan, but records
			// before it are still found
			"over-long line skipped, earlier records found",
			prLine1 + strings.Repeat("x", 9*1024*1024) + "\n" + prLine2,
			&PRLink{Number: 9, URL: "https://github.com/o/r/pull/9"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "conv.jsonl")
			writeTranscript(t, path, tc.content)
			got := scanPRLink(path)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("scanPRLink = %+v, want absence", got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("scanPRLink = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPRLinkFromTranscriptStatGate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	writeTranscript(t, path, chatter+prLine1)

	link, size, rescanned := prLinkFromTranscript(path, 0)
	if !rescanned || link == nil || link.Number != 9 {
		t.Fatalf("first scan = (%+v, %d, %v), want a rescan finding PR 9", link, size, rescanned)
	}
	if st, _ := os.Stat(path); size != st.Size() {
		t.Fatalf("size = %d, want the stat size %d", size, st.Size())
	}

	// unchanged size: no read occurs — the gate reports no rescan, so the
	// caller keeps its current value
	link, size2, rescanned := prLinkFromTranscript(path, size)
	if rescanned || link != nil || size2 != size {
		t.Fatalf("unchanged file = (%+v, %d, %v), want the stat-gate to skip", link, size2, rescanned)
	}

	// a record appended after a prior scan is found once the size changes
	appendTranscript(t, path, prLine2)
	link, size3, rescanned := prLinkFromTranscript(path, size)
	if !rescanned || link == nil || link.Number != 12 {
		t.Fatalf("after append = (%+v, %d, %v), want a rescan finding PR 12", link, size3, rescanned)
	}
	if size3 <= size {
		t.Errorf("size after append = %d, want > %d", size3, size)
	}
}

func TestPRLinkFromTranscriptMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gone.jsonl")

	// the first miss reports a rescan (so a previously shown link clears),
	// then the sentinel settles the gate
	link, size, rescanned := prLinkFromTranscript(path, 4096)
	if !rescanned || link != nil || size != missingTranscriptSize {
		t.Fatalf("first miss = (%+v, %d, %v), want a clearing rescan", link, size, rescanned)
	}
	link, size, rescanned = prLinkFromTranscript(path, size)
	if rescanned || link != nil || size != missingTranscriptSize {
		t.Fatalf("settled miss = (%+v, %d, %v), want silence", link, size, rescanned)
	}

	// the transcript reappearing always triggers a rescan
	writeTranscript(t, path, prLine1)
	link, _, rescanned = prLinkFromTranscript(path, size)
	if !rescanned || link == nil || link.Number != 9 {
		t.Fatalf("reappeared file = (%+v, %v), want a rescan finding PR 9", link, rescanned)
	}
}

func TestPRLinkTornLineRecoveredOnRescan(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	torn := strings.TrimSuffix(prLine1, "\"}\n") // record cut mid-write
	writeTranscript(t, path, chatter+torn)

	link, size, rescanned := prLinkFromTranscript(path, 0)
	if !rescanned || link != nil {
		t.Fatalf("torn-only scan = (%+v, %v), want a rescan finding nothing", link, rescanned)
	}

	// the write completes: the size changes, the full rescan recovers the
	// record — nothing is permanently skipped (the no-offset-tracking rule)
	appendTranscript(t, path, "\"}\n")
	link, _, rescanned = prLinkFromTranscript(path, size)
	if !rescanned || link == nil || link.Number != 9 {
		t.Fatalf("completed line = (%+v, %v), want the recovered record", link, rescanned)
	}
}

/* ---------- the registry-tick call site ---------- */

// overrideProjectsDir points the transcript root at a temp dir (precedent for
// swapping the package var: transcripts.go declares it for exactly this).
// Tests using it must not call t.Parallel.
func overrideProjectsDir(t *testing.T) string {
	t.Helper()
	prev := claudeProjectsDir
	dir := t.TempDir()
	claudeProjectsDir = dir
	t.Cleanup(func() { claudeProjectsDir = prev })
	return dir
}

func TestApplyPRLinksSetsAndUpdates(t *testing.T) {
	projects := overrideProjectsDir(t)
	m := testManager(t, nil)
	cwd := "/w/pr-project"
	insertLive(t, m, "s1", cwd, StateReady)
	m.mu.Lock()
	uuid := "conv-pr"
	m.sessions["s1"].ClaudeSessionID = &uuid
	m.mu.Unlock()

	transcript := filepath.Join(projects, claudeProjectDirName(cwd), "conv-pr.jsonl")
	writeTranscript(t, transcript, chatter+prLine1)

	// one tick's worth of scan inputs comes straight from applyRegistryTick,
	// pinning the path construction (projects dir + slug + uuid)
	tick := func() {
		_, scans := m.applyRegistryTick(
			[]regSessionSnap{{id: "s1"}},
			map[string]*regJoin{"s1": {rowSeen: true, activity: "busy"}},
			time.Minute, time.Now(),
		)
		if len(scans) != 1 || scans[0].path != transcript {
			t.Fatalf("scan inputs = %+v, want the uuid-named transcript %s", scans, transcript)
		}
		m.applyPRLinks(scans)
	}

	tick()
	got := m.Get("s1")
	if got.PRLink == nil || got.PRLink.Number != 9 {
		t.Fatalf("prLink = %+v, want PR 9", got.PRLink)
	}

	// unchanged file: the stat-gate keeps the link without a read
	tick()
	if got = m.Get("s1"); got.PRLink == nil || got.PRLink.Number != 9 {
		t.Fatalf("prLink after gated tick = %+v, want PR 9 retained", got.PRLink)
	}

	// a newer record wins once the size changes
	appendTranscript(t, transcript, prLine2)
	tick()
	if got = m.Get("s1"); got.PRLink == nil || got.PRLink.Number != 12 {
		t.Fatalf("prLink after append = %+v, want PR 12", got.PRLink)
	}
}

func TestApplyPRLinksAbsenceAndStateGuard(t *testing.T) {
	projects := overrideProjectsDir(t)
	m := testManager(t, nil)

	// no transcript on disk (uuid captured but the file was moved/cleaned):
	// absence, no error
	insertLive(t, m, "gone", "/w/gone", StateReady)
	m.mu.Lock()
	goneUUID := "conv-gone"
	m.sessions["gone"].ClaudeSessionID = &goneUUID
	m.mu.Unlock()
	_, scans := m.applyRegistryTick([]regSessionSnap{{id: "gone"}}, map[string]*regJoin{"gone": {}}, time.Minute, time.Now())
	m.applyPRLinks(scans)
	if got := m.Get("gone"); got.PRLink != nil {
		t.Errorf("missing transcript must yield absence, got %+v", got.PRLink)
	}

	// state guard: a session that exited between scan and apply takes no write
	deadCwd := "/w/dead"
	insertLive(t, m, "dead", deadCwd, StateReady)
	m.mu.Lock()
	deadUUID := "conv-dead"
	m.sessions["dead"].ClaudeSessionID = &deadUUID
	m.mu.Unlock()
	writeTranscript(t, filepath.Join(projects, claudeProjectDirName(deadCwd), "conv-dead.jsonl"), prLine1)
	_, scans = m.applyRegistryTick([]regSessionSnap{{id: "dead"}}, map[string]*regJoin{"dead": {}}, time.Minute, time.Now())
	m.mu.Lock()
	m.sessions["dead"].State = StateExited
	m.mu.Unlock()
	m.applyPRLinks(scans)
	if got := m.Get("dead"); got.PRLink != nil {
		t.Errorf("state guard must drop pr-link writes to exited sessions, got %+v", got.PRLink)
	}

	// sessions without a captured uuid produce no scan work at all
	insertLive(t, m, "no-uuid", "/w/nouuid", StateReady)
	_, scans = m.applyRegistryTick([]regSessionSnap{{id: "no-uuid"}}, map[string]*regJoin{"no-uuid": {}}, time.Minute, time.Now())
	if len(scans) != 0 {
		t.Errorf("uuid-less session produced scan inputs: %+v", scans)
	}
}
