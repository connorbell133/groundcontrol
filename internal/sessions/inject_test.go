package sessions

/* Injection lifecycle tests ride the FakeClaude launch stub (t.Setenv, so no
t.Parallel except where nothing launches). The exit-side assertions wait on
the session.exit journal entry: it is written after the settings removal, so
"exit entry exists" means the removal decision has already been made. */

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/testutil"
)

// an inert preset payload: "theme" is on the injection allowlist, so it round-
// trips. Command-executing / credential-moving keys like "env" are refused by
// ForbiddenSettingsKey and would skip injection instead.
const testSettingsJSON = `{"theme":"dark"}`

// assertOwnedMarker fails unless got carries our marker branded with this
// runner's pid — the owner-object form that lets a second runner recognize a
// live peer's file.
func assertOwnedMarker(t *testing.T, got map[string]json.RawMessage) {
	t.Helper()
	raw, ok := got[settingsMarkerKey]
	if !ok {
		t.Fatalf("marker missing: %v", got)
	}
	var o settingsOwner
	if err := json.Unmarshal(raw, &o); err != nil || o.Pid != os.Getpid() {
		t.Errorf("marker owner = %s, want pid %d: %v", raw, os.Getpid(), err)
	}
}

// readSettings parses the injected file, failing the test on anything
// unreadable — these tests always expect a well-formed file when one exists.
func readSettings(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings file not JSON: %v (%s)", err, b)
	}
	return m
}

// waitExitEntry blocks until the session.exit entry lands — the settings
// removal runs before that write on the exit path.
func waitExitEntry(t *testing.T, m *Manager, id string) {
	t.Helper()
	waitFor(t, 10*time.Second, "session.exit journal entry", func() bool {
		return len(journalEntries(m, evSessionExit, id)) == 1
	})
}

func TestInjectSettingsLifecycle(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason != nil {
		t.Errorf("injected launch must carry no skip reason, got %q", *s.SettingsSkipReason)
	}

	// the file exists for the session's lifetime, marker added, payload kept
	path := settingsFilePath(folder)
	got := readSettings(t, path)
	assertOwnedMarker(t, got)
	if _, ok := got["theme"]; !ok {
		t.Errorf("preset payload lost: %v", got)
	}

	// outcome flattened into session.start, never a standalone event
	entry := journalEntries(m, evSessionStart, s.ID)[0]
	if entry["settingsInjected"] != true {
		t.Errorf("settingsInjected = %v, want true", entry["settingsInjected"])
	}
	if _, ok := entry["settingsSkipReason"]; ok {
		t.Errorf("injected launch must not journal a skip reason: %v", entry)
	}
	if _, ok := entry["settingsNote"]; ok {
		t.Errorf("fresh injection must not journal a replacement note: %v", entry)
	}

	killSessionAndWait(t, m, s.ID)
	waitExitEntry(t, m, s.ID)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("settings file must be removed after exit: %v", err)
	}
}

func TestInjectRefusesUnmarkedUserFile(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	// a user's own settings file, no marker
	path := settingsFilePath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte(`{"model":"opus"}` + "\n")
	if err := os.WriteFile(path, userContent, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("launch must proceed despite the skip: %v", err)
	}
	if s.SettingsSkipReason == nil || *s.SettingsSkipReason != skipUserFile {
		t.Errorf("skip reason = %v, want %q", s.SettingsSkipReason, skipUserFile)
	}
	entry := journalEntries(m, evSessionStart, s.ID)[0]
	if entry["settingsSkipReason"] != skipUserFile {
		t.Errorf("journaled skip reason = %v, want %q", entry["settingsSkipReason"], skipUserFile)
	}
	if _, ok := entry["settingsInjected"]; ok {
		t.Errorf("skipped launch must not journal settingsInjected: %v", entry)
	}

	// teardown leaves the unmarked file exactly as it was
	killSessionAndWait(t, m, s.ID)
	waitExitEntry(t, m, s.ID)
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(userContent) {
		t.Errorf("user file must survive teardown untouched: %v %q", err, b)
	}
}

func TestInjectReplacesStaleLeftover(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	// a marked leftover from a crashed run; nobody live in the folder
	path := settingsFilePath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"_groundcontrol":true,"env":{"STALE":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason != nil {
		t.Errorf("stale replacement must report injected, got skip %q", *s.SettingsSkipReason)
	}
	got := readSettings(t, path)
	if _, ok := got["STALE"]; ok {
		t.Errorf("stale payload survived the replacement: %v", got)
	}
	assertOwnedMarker(t, got)
	entry := journalEntries(m, evSessionStart, s.ID)[0]
	if entry["settingsInjected"] != true || entry["settingsNote"] != noteReplacedStale {
		t.Errorf("replacement journal = %v, want settingsInjected + %q", entry, noteReplacedStale)
	}
}

func TestInjectSharedFolderDefersRemoval(t *testing.T) {
	// the one-live-environment-per-folder guard makes two same-dir launches
	// unreachable through Create, but the deferral path still guards the
	// exit-vs-launch race — fabricate the second live occupant directly
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	path := settingsFilePath(folder)

	s1, err := m.Create(CreateOpts{Folder: folder, Name: "share-1", SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	insertLive(t, m, "squatter", folder, StateReady)

	// s1's exit defers: a live session still shares the folder
	killSessionAndWait(t, m, s1.ID)
	waitExitEntry(t, m, s1.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("removal must defer while a live session shares the folder: %v", err)
	}

	// once the folder empties, the removal path may run again (teardown of the
	// squatter is synthetic — exercise the helper the real exit path calls)
	m.mu.Lock()
	m.sessions["squatter"].State = StateExited
	m.mu.Unlock()
	m.removeSettingsIfLast(path, folder)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("last exit must remove the settings file: %v", err)
	}
}

func TestSameDirSecondLaunchBlocked(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	s1, err := m.Create(CreateOpts{Folder: folder, Name: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if _, err := m.Create(CreateOpts{Folder: folder, Name: "second"}); err == nil || !strings.Contains(err.Error(), "already live in this folder") {
		t.Fatalf("second same-dir launch must be rejected, got err=%v", err)
	}

	// the folder frees up once the live session exits
	killSessionAndWait(t, m, s1.ID)
	waitExitEntry(t, m, s1.ID)
	if _, err := m.Create(CreateOpts{Folder: folder, Name: "third"}); err != nil {
		t.Fatalf("launch after exit must succeed: %v", err)
	}
}

func TestSameDirGuardIgnoresWorktreeLaunches(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	m := testManager(t, []string{repo})

	if _, err := m.Create(CreateOpts{Folder: repo, Name: "in-folder"}); err != nil {
		t.Fatalf("Create same-dir: %v", err)
	}
	// a worktree launch runs in its own directory and must not be blocked
	if _, err := m.Create(CreateOpts{Folder: repo, Name: "isolated", SpawnMode: "worktree", Branch: "main"}); err != nil {
		t.Fatalf("worktree launch alongside a same-dir session must succeed: %v", err)
	}
}

func TestInjectWorktreeLaunchWritesIntoWorktree(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	m := testManager(t, []string{repo})

	s, err := m.Create(CreateOpts{Folder: repo, SpawnMode: "worktree", Branch: "main", SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.WorktreePath == nil || *s.WorktreePath == "" {
		t.Fatal("worktree session missing worktreePath")
	}
	wt := *s.WorktreePath

	// the launch cwd is the worktree — the file lands there, not the repo
	if _, err := os.Stat(settingsFilePath(wt)); err != nil {
		t.Fatalf("settings file missing from the worktree: %v", err)
	}
	if _, err := os.Stat(settingsFilePath(repo)); !os.IsNotExist(err) {
		t.Errorf("worktree launch must not touch the repo checkout: %v", err)
	}

	// removal precedes the debrief and cleanup: the injected file must not
	// read as the run's own uncommitted work and keep the worktree dirty
	killSessionAndWait(t, m, s.ID)
	waitExitEntry(t, m, s.ID)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("clean worktree must be removed despite the injection: %v", err)
	}
	exit := journalEntries(m, evSessionExit, s.ID)[0]
	if exit["uncommitted"] != float64(0) {
		t.Errorf("injected file counted as the run's work: %v", exit)
	}
}

func TestTeardownLeavesFileThatLostItsMarker(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// mid-session the user replaced our file with their own — no marker, not ours anymore
	path := settingsFilePath(folder)
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	killSessionAndWait(t, m, s.ID)
	waitExitEntry(t, m, s.ID)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("teardown must leave an unmarked file alone: %v", err)
	}
}

func TestSweepSettingsLeftovers(t *testing.T) {
	t.Parallel() // nothing launches, no PATH mutation
	m := testManager(t, nil)

	marked := []byte(`{"_groundcontrol":true,"env":{"X":"1"}}`)
	unmarked := []byte(`{"model":"opus"}`)
	write := func(folder string, content []byte) string {
		t.Helper()
		path := settingsFilePath(folder)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// injected-launch folder with a marked orphan: swept
	orphaned := testutil.ResolvedTempDir(t)
	orphanPath := write(orphaned, marked)
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "a", "folder": orphaned, "settingsInjected": true})
	// injected-launch folder where the user since wrote their own file: kept
	userOwned := testutil.ResolvedTempDir(t)
	userPath := write(userOwned, unmarked)
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "b", "folder": userOwned, "settingsInjected": true})
	// marked file in a folder whose entry recorded no injection: kept
	uninvolved := testutil.ResolvedTempDir(t)
	uninvolvedPath := write(uninvolved, marked)
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "c", "folder": uninvolved})
	// injected entry whose folder is gone: tolerated
	m.journal.Append(map[string]any{"event": evSessionStart, "id": "d", "folder": filepath.Join(orphaned, "no", "such", "dir"), "settingsInjected": true})

	m.SweepSettingsLeftovers()

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("marked orphan must be swept: %v", err)
	}
	if _, err := os.Stat(userPath); err != nil {
		t.Errorf("unmarked file must survive the sweep: %v", err)
	}
	if _, err := os.Stat(uninvolvedPath); err != nil {
		t.Errorf("folder without a recorded injection must not be swept: %v", err)
	}
}

func TestForbiddenSettingsKey(t *testing.T) {
	t.Parallel()
	obj := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("bad fixture %q: %v", s, err)
		}
		return m
	}
	forbidden := []string{
		`{"hooks":{}}`, `{"apiKeyHelper":"x"}`, `{"statusLine":{}}`,
		`{"env":{"ANTHROPIC_BASE_URL":"http://evil"}}`, `{"permissions":{}}`,
		`{"sandbox":{}}`, `{"awsAuthRefresh":"x"}`, `{"additionalDirectories":[]}`,
		`{"model":"x","hooks":{}}`, // one bad key among inert ones
	}
	for _, s := range forbidden {
		if ForbiddenSettingsKey(obj(s)) == "" {
			t.Errorf("%s: expected a forbidden key, got none", s)
		}
	}
	allowed := []string{
		`{"model":"claude-sonnet-4-6"}`, `{"theme":"dark","verbose":true}`,
		`{"_groundcontrol":true}`, `{}`,
	}
	for _, s := range allowed {
		if bad := ForbiddenSettingsKey(obj(s)); bad != "" {
			t.Errorf("%s: unexpected forbidden key %q", s, bad)
		}
	}
}

func TestInjectSkipsForbiddenKey(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: `{"env":{"ANTHROPIC_BASE_URL":"http://evil"}}`})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason == nil || *s.SettingsSkipReason != skipForbiddenKey {
		t.Errorf("skip reason = %v, want %q", s.SettingsSkipReason, skipForbiddenKey)
	}
	if _, err := os.Stat(settingsFilePath(folder)); !os.IsNotExist(err) {
		t.Errorf("forbidden preset must not write a settings file: %v", err)
	}
}

func TestInjectSkipsOversize(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	big := `{"model":"` + strings.Repeat("x", settingsMaxBytes) + `"}`
	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: big})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason == nil || *s.SettingsSkipReason != skipTooLarge {
		t.Errorf("skip reason = %v, want %q", s.SettingsSkipReason, skipTooLarge)
	}
}

func TestInjectSkipsBadSettings(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: `["not","an","object"]`})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason == nil || *s.SettingsSkipReason != skipBadSettings {
		t.Errorf("skip reason = %v, want %q", s.SettingsSkipReason, skipBadSettings)
	}
}

func TestInjectSkipsLivePeerRunnerFile(t *testing.T) {
	// a marked file owned by a live process (this test's own pid) simulates a
	// second runner instance's live injection. The current runner has no live
	// session in the folder, so without the owner check it would misread the
	// file as a crash leftover and clobber it (the two-instance clobber).
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	path := settingsFilePath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	peer, _ := json.Marshal(map[string]any{
		settingsMarkerKey: settingsOwner{Pid: os.Getpid(), Host: runnerHost, ID: "peer"},
		"theme":           "light",
	})
	if err := os.WriteFile(path, peer, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason == nil || *s.SettingsSkipReason != skipInUse {
		t.Errorf("live peer file must skip with %q, got %v", skipInUse, s.SettingsSkipReason)
	}
	got := readSettings(t, path)
	if _, ok := got["theme"]; !ok || string(got["theme"]) != `"light"` {
		t.Errorf("peer's live file was clobbered: %v", got)
	}
}

func TestInjectReplacesDeadOwnerFile(t *testing.T) {
	// a marked file owned by a pid that cannot be alive (pid 0 is never a
	// running process) is a crash leftover: replaced, not skipped.
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	path := settingsFilePath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	dead, _ := json.Marshal(map[string]any{
		settingsMarkerKey: settingsOwner{Pid: 0, Host: runnerHost, ID: "dead"},
	})
	if err := os.WriteFile(path, dead, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := m.Create(CreateOpts{Folder: folder, SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.SettingsSkipReason != nil {
		t.Errorf("dead-owner leftover must be replaced, got skip %q", *s.SettingsSkipReason)
	}
	assertOwnedMarker(t, readSettings(t, path))
}

func TestInjectIntentSweepsPreJournalCrashLeftover(t *testing.T) {
	// simulate a crash after the file write but before session.start: only the
	// inject-intent entry exists. The sweep must still reclaim the marked file.
	t.Parallel()
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})

	path := settingsFilePath(folder)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	leftover, _ := json.Marshal(map[string]any{
		settingsMarkerKey: settingsOwner{Pid: 0, Host: runnerHost, ID: "crashed"},
	})
	if err := os.WriteFile(path, leftover, 0o644); err != nil {
		t.Fatal(err)
	}
	// intent only — no session.start followed it
	m.journal.Append(map[string]any{"event": evSessionInjectIntent, "id": "crashed", "folder": folder})

	m.SweepSettingsLeftovers()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("inject-intent leftover must be swept: %v", err)
	}
}
