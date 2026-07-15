package sessions

/* Injection lifecycle tests ride the FakeClaude launch stub (t.Setenv, so no
t.Parallel except where nothing launches). The exit-side assertions wait on
the session.exit journal entry: it is written after the settings removal, so
"exit entry exists" means the removal decision has already been made. */

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/testutil"
)

const testSettingsJSON = `{"env":{"GC_TEST":"1"}}`

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
	if string(got[settingsMarkerKey]) != "true" {
		t.Errorf("marker missing or wrong: %s", got[settingsMarkerKey])
	}
	if _, ok := got["env"]; !ok {
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
	if string(got[settingsMarkerKey]) != "true" {
		t.Errorf("replacement lost the marker: %v", got)
	}
	entry := journalEntries(m, evSessionStart, s.ID)[0]
	if entry["settingsInjected"] != true || entry["settingsNote"] != noteReplacedStale {
		t.Errorf("replacement journal = %v, want settingsInjected + %q", entry, noteReplacedStale)
	}
}

func TestInjectSharedFolderDefersRemoval(t *testing.T) {
	testutil.FakeClaude(t)
	folder := testutil.ResolvedTempDir(t)
	m := testManager(t, []string{folder})
	path := settingsFilePath(folder)

	s1, err := m.Create(CreateOpts{Folder: folder, Name: "share-1", SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	s2, err := m.Create(CreateOpts{Folder: folder, Name: "share-2", SettingsJSON: testSettingsJSON})
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	// the second launch finds a marked file owned by a live session
	if s2.SettingsSkipReason == nil || *s2.SettingsSkipReason != skipInUse {
		t.Fatalf("second launch skip reason = %v, want %q", s2.SettingsSkipReason, skipInUse)
	}

	// first exit defers: s2 still works in the folder
	killSessionAndWait(t, m, s1.ID)
	waitExitEntry(t, m, s1.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("removal must defer while a live session shares the folder: %v", err)
	}

	// the last same-folder exit removes it
	killSessionAndWait(t, m, s2.ID)
	waitExitEntry(t, m, s2.ID)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("last exit must remove the settings file: %v", err)
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
