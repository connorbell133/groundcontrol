package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/config"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/gitx"
	"github.com/connorbell133/groundcontrol/internal/jobs"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/sessions"
	"github.com/connorbell133/groundcontrol/internal/testutil"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

// testEnv is the full wiring main() builds, on temp dirs, plus the handles the
// tests poke at directly.
type testEnv struct {
	handler    http.Handler
	configPath string
	journal    *journal.Journal
	browser    *browse.Browser
	sessions   *sessions.Manager
	jobs       *jobs.Manager
}

// newTestEnv builds an isolated instance wired the way main() wires it.
func newTestEnv(t *testing.T, cfg config.Config) *testEnv {
	t.Helper()
	if cfg.Roots == nil {
		cfg.Roots = []string{testutil.ResolvedTempDir(t)}
	}
	jnl := journal.New(t.TempDir())
	bus := events.NewBus(jnl)
	browser := browse.New()
	ws := workspace.New(t.TempDir(), jnl)
	sessionMgr := sessions.NewManager(jnl, bus, ws, browser)
	jobMgr := jobs.NewManager(jnl, bus, ws)
	browser.Configure(cfg.Roots, cfg.ShowHidden)
	bus.ConfigureWebhooks(cfg.Webhooks)
	if cfg.Jobs != nil {
		jobMgr.Configure(cfg.Jobs.Concurrency, cfg.Jobs.TimeoutMs)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	srv := NewServer(configPath, cfg, browser, bus, ws, sessionMgr, jobMgr)
	return &testEnv{
		handler:    srv.Handler(),
		configPath: configPath,
		journal:    jnl,
		browser:    browser,
		sessions:   sessionMgr,
		jobs:       jobMgr,
	}
}

func doReq(t *testing.T, h http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// errCode decodes the standard error envelope and asserts its shape.
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if e.Error.Code == "" || e.Error.Message == "" {
		t.Fatalf("error body missing code or message: %s", rec.Body.String())
	}
	return e.Error.Code
}

func TestScopeSet(t *testing.T) {
	t.Parallel()
	set := scopeSet([]scope{scopeRead, scopeLaunch})
	if !set[scopeRead] || !set[scopeLaunch] || set[scopeAdmin] {
		t.Errorf("scopeSet wrong: %v", set)
	}
	if len(scopeSet(nil)) != 0 {
		t.Error("scopeSet(nil) should be empty")
	}
}

func TestNonNil(t *testing.T) {
	t.Parallel()
	if got := nonNil[int](nil); got == nil || len(got) != 0 {
		t.Errorf("nonNil(nil) = %v", got)
	}
	in := []string{"a"}
	if got := nonNil(in); len(got) != 1 || got[0] != "a" {
		t.Errorf("nonNil passthrough = %v", got)
	}
}

func TestQRSVG(t *testing.T) {
	t.Parallel()
	svg, err := qrSVG("https://example.com/pair/abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, `<path fill="#1c1a14"`) || !strings.Contains(svg, `fill="#fffdf6"`) {
		t.Errorf("svg missing expected shape: %.120s", svg)
	}
	dec := xml.NewDecoder(strings.NewReader(svg))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("svg is not well-formed XML: %v", err)
		}
	}
	if _, err := qrSVG(""); err == nil {
		t.Error("empty content should error")
	}
}

func TestAuthAnonymous(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{})
	h := env.handler

	// no tokens configured: anonymous gets every scope
	if rec := doReq(t, h, "GET", "/sessions", "", nil); rec.Code != 200 {
		t.Errorf("anonymous read = %d, want 200", rec.Code)
	}
	if rec := doReq(t, h, "PUT", "/config", "{}", nil); rec.Code != 200 {
		t.Errorf("anonymous admin = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "POST", "/sessions", "{}", nil); rec.Code != 400 || errCode(t, rec) != "missing_param" {
		t.Errorf("anonymous launch should reach validation, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthToken(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{AuthToken: "sekret"})
	h := env.handler

	cases := []struct {
		name   string
		target string
		header map[string]string
		want   int
	}{
		{"no token", "/sessions", nil, 401},
		{"wrong bearer", "/sessions", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"wrong query token", "/sessions?token=nope", nil, 401},
		{"correct bearer", "/sessions", map[string]string{"Authorization": "Bearer sekret"}, 200},
		{"correct query token", "/sessions?token=sekret", nil, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, "GET", tc.target, "", tc.header)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 401 && errCode(t, rec) != "unauthorized" {
				t.Errorf("code = %s, want unauthorized", errCode(t, rec))
			}
		})
	}
}

func TestScopedTokens(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{
		AuthToken: "owner-token",
		Tokens: []config.TokenConfig{
			{Name: "reader", Token: "tok-read", Scopes: []string{"read", "bogus-scope"}}, // unknown scope filtered
			{Name: "runner", Token: "tok-launch", Scopes: []string{"read", "launch"}},
		},
	})
	h := env.handler
	read := map[string]string{"Authorization": "Bearer tok-read"}
	launch := map[string]string{"Authorization": "Bearer tok-launch"}
	owner := map[string]string{"Authorization": "Bearer owner-token"}

	if rec := doReq(t, h, "GET", "/sessions", "", read); rec.Code != 200 {
		t.Errorf("read-scoped GET /sessions = %d", rec.Code)
	}
	if rec := doReq(t, h, "POST", "/sessions", "{}", read); rec.Code != 403 || errCode(t, rec) != "insufficient_scope" {
		t.Errorf("read-scoped POST /sessions = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "PUT", "/config", "{}", read); rec.Code != 403 || errCode(t, rec) != "insufficient_scope" {
		t.Errorf("read-scoped PUT /config = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "DELETE", "/worktrees?path=/x", "", read); rec.Code != 403 {
		t.Errorf("read-scoped DELETE /worktrees = %d", rec.Code)
	}
	// the unknown scope grants nothing beyond read
	if rec := doReq(t, h, "POST", "/jobs", "{}", read); rec.Code != 403 {
		t.Errorf("bogus scope must not grant launch: %d", rec.Code)
	}

	// launch-scoped token passes the scope gate and reaches validation
	if rec := doReq(t, h, "POST", "/sessions", "{}", launch); rec.Code != 400 || errCode(t, rec) != "missing_param" {
		t.Errorf("launch-scoped POST /sessions = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "PUT", "/config", "{}", launch); rec.Code != 403 {
		t.Errorf("launch-scoped PUT /config = %d", rec.Code)
	}

	// orbit: listing is read, sweeping is admin-only
	if rec := doReq(t, h, "GET", "/orbit", "", read); rec.Code != 200 {
		t.Errorf("read-scoped GET /orbit = %d", rec.Code)
	}
	if rec := doReq(t, h, "DELETE", "/orbit?repo=/x&branch=gc/y", "", read); rec.Code != 403 || errCode(t, rec) != "insufficient_scope" {
		t.Errorf("read-scoped DELETE /orbit = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, "DELETE", "/orbit?repo=/x&branch=gc/y", "", launch); rec.Code != 403 {
		t.Errorf("launch-scoped DELETE /orbit = %d", rec.Code)
	}

	// the legacy authToken keeps full scope
	if rec := doReq(t, h, "PUT", "/config", "{}", owner); rec.Code != 200 {
		t.Errorf("owner PUT /config = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPostSessionsValidation(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})
	h := env.handler

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"empty body", "", 400, "invalid_json"},
		{"malformed json", "{not json", 400, "invalid_json"},
		{"non-object body", "[1,2]", 400, "invalid_json"},
		{"missing folder", "{}", 400, "missing_param"},
		{"wrong-typed folder", `{"folder":123}`, 400, "invalid_param"},
		{"folder outside roots", `{"folder":"/definitely/not/in/roots"}`, 400, "outside_roots"},
		{"bad callbackUrl", fmt.Sprintf(`{"folder":%q,"callbackUrl":"not-a-url"}`, root), 400, "invalid_param"},
		{"empty callbackUrl still validated", fmt.Sprintf(`{"folder":%q,"callbackUrl":""}`, root), 400, "invalid_param"},
		{"unknown permissionMode", fmt.Sprintf(`{"folder":%q,"permissionMode":"yolo"}`, root), 400, "invalid_param"},
		{"wrong-typed capacity", fmt.Sprintf(`{"folder":%q,"capacity":"lots"}`, root), 400, "invalid_param"},
		// fails inside Create before any spawn: worktree mode needs a branch
		{"worktree without branch", fmt.Sprintf(`{"folder":%q,"spawnMode":"worktree"}`, root), 409, "launch_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, "POST", "/sessions", tc.body, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := errCode(t, rec); got != tc.wantErr {
				t.Errorf("error code = %s, want %s", got, tc.wantErr)
			}
		})
	}
	if list := env.sessions.List(); len(list) != 0 {
		t.Errorf("validation failures must not create sessions: %+v", list)
	}
}

// killAndWait tears down a fake-claude session and blocks until its exit
// lifecycle has run, so nothing outlives the test.
func killAndWait(t *testing.T, env *testEnv, id string) {
	t.Helper()
	env.sessions.Kill(id, "test")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := env.sessions.Get(id)
		if s == nil || s.State == sessions.StateExited || s.State == sessions.StateError {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("session %s did not exit after kill", id)
}

// createSession POSTs a launch and registers teardown; the caller must have
// installed testutil.FakeClaude first.
func createSession(t *testing.T, env *testEnv, body string) sessions.Session {
	t.Helper()
	rec := doReq(t, env.handler, "POST", "/sessions", body, nil)
	if rec.Code != 201 {
		t.Fatalf("POST /sessions = %d: %s", rec.Code, rec.Body.String())
	}
	var created sessions.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("session body not JSON: %v (%s)", err, rec.Body.String())
	}
	t.Cleanup(func() { killAndWait(t, env, created.ID) })
	return created
}

// waitForExitEntry polls for the session.exit journal entry: the exit
// lifecycle (debrief, worktree cleanup, journal write) runs after the state
// flips to exited, so killAndWait alone doesn't guarantee it has landed.
func waitForExitEntry(t *testing.T, env *testEnv, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range env.journal.Read() {
			if e["event"] == "session.exit" && e["id"] == id {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no session.exit journal entry for %s", id)
	return nil
}

// startEntry finds the session.start journal entry for id — Create writes it
// synchronously, so no polling is needed.
func startEntry(t *testing.T, env *testEnv, id string) map[string]any {
	t.Helper()
	for _, e := range env.journal.Read() {
		if e["event"] == "session.start" && e["id"] == id {
			return e
		}
	}
	t.Fatalf("no session.start journal entry for %s", id)
	return nil
}

func TestPostSessionsPermissionModes(t *testing.T) {
	testutil.FakeClaude(t)
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})

	for _, mode := range []string{"default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"} {
		created := createSession(t, env, fmt.Sprintf(`{"folder":%q,"name":"pm-%s","permissionMode":%q}`, root, mode, mode))
		if created.PermissionMode != mode {
			t.Errorf("%s: session permissionMode = %q", mode, created.PermissionMode)
		}
		// the journal entry records the mode that reached the spawn args
		if got := startEntry(t, env, created.ID)["permissionMode"]; got != mode {
			t.Errorf("%s: journaled permissionMode = %v", mode, got)
		}
	}
}

func TestPostSessionsCapacity(t *testing.T) {
	testutil.FakeClaude(t)
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})

	cases := []struct {
		name string
		body string
		want int  // capacity on the wire
		flag bool // --capacity spawned — the journal entry mirrors the args
	}{
		{"absent defaults", fmt.Sprintf(`{"folder":%q,"name":"cap-absent"}`, root), 32, false},
		{"zero defaults", fmt.Sprintf(`{"folder":%q,"name":"cap-zero","capacity":0}`, root), 32, false},
		{"explicit default omits the flag", fmt.Sprintf(`{"folder":%q,"name":"cap-default","capacity":32}`, root), 32, false},
		{"non-default passes through", fmt.Sprintf(`{"folder":%q,"name":"cap-four","capacity":4}`, root), 4, true},
		{"oversized clamps", fmt.Sprintf(`{"folder":%q,"name":"cap-big","capacity":500}`, root), 256, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := createSession(t, env, tc.body)
			if created.Capacity != tc.want {
				t.Errorf("session capacity = %d, want %d", created.Capacity, tc.want)
			}
			got, journaled := startEntry(t, env, created.ID)["capacity"]
			if journaled != tc.flag {
				t.Errorf("capacity journaled = %v, want %v", journaled, tc.flag)
			}
			if tc.flag && got != float64(tc.want) {
				t.Errorf("journaled capacity = %v, want %d", got, tc.want)
			}
		})
	}

	// a default launch shows the resolved capacity on GET /sessions too
	rec := doReq(t, env.handler, "GET", "/sessions", "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /sessions = %d", rec.Code)
	}
	var listed struct {
		Sessions []sessions.Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("sessions body not JSON: %v", err)
	}
	found := false
	for _, s := range listed.Sessions {
		if s.Name == "cap-absent" {
			found = true
			if s.Capacity != 32 {
				t.Errorf("listed capacity = %d, want 32", s.Capacity)
			}
		}
	}
	if !found {
		t.Error("cap-absent session missing from GET /sessions")
	}
}

// A recent journaled before capacity existed replays cleanly: the recents
// view defaults capacity to 32, and re-POSTing it spawns no --capacity.
func TestPostSessionsPreCapacityRelaunch(t *testing.T) {
	testutil.FakeClaude(t)
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})

	// a pre-capacity session.start entry: no capacity or presetName keys at all
	env.journal.Append(map[string]any{"event": "session.start", "id": "old1", "name": "old", "folder": root, "permissionMode": "acceptEdits"})

	rec := doReq(t, env.handler, "GET", "/journal/recent", "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /journal/recent = %d", rec.Code)
	}
	var recents struct {
		Recent []sessions.RecentLaunch `json:"recent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &recents); err != nil {
		t.Fatalf("recent body not JSON: %v", err)
	}
	if len(recents.Recent) != 1 || recents.Recent[0].Capacity != 32 || recents.Recent[0].PresetName != "" {
		t.Fatalf("pre-capacity recent did not default cleanly: %+v", recents.Recent)
	}

	// relaunch exactly what the recent carries
	r0 := recents.Recent[0]
	created := createSession(t, env, fmt.Sprintf(`{"folder":%q,"name":"relaunch","spawnMode":%q,"permissionMode":%q,"capacity":%d}`,
		r0.Folder, r0.SpawnMode, r0.PermissionMode, r0.Capacity))
	if created.Capacity != 32 {
		t.Errorf("relaunch capacity = %d, want 32", created.Capacity)
	}
	if _, ok := startEntry(t, env, created.ID)["capacity"]; ok {
		t.Error("default-capacity relaunch journaled a capacity (would spawn --capacity)")
	}
}

// createWorktreeSession launches a fake-claude worktree session off main and
// waits for it to pair, returning the session and its worktree path.
func createWorktreeSession(t *testing.T, env *testEnv, folder, name string) (sessions.Session, string) {
	t.Helper()
	created := createSession(t, env, fmt.Sprintf(`{"folder":%q,"name":%q,"spawnMode":"worktree","branch":"main"}`, folder, name))
	if created.WorktreePath == nil || *created.WorktreePath == "" {
		t.Fatal("worktree session missing worktreePath")
	}
	if outcome := env.sessions.WaitForReady(created.ID, 10*time.Second); outcome != "ready" {
		t.Fatalf("WaitForReady = %q, want ready", outcome)
	}
	return created, *created.WorktreePath
}

func TestSessionExitDebriefInOrbit(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "orbit")

	// the run's work: one committed two-line file in the worktree
	if err := os.WriteFile(filepath.Join(wt, "feat.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.MustGit(t, wt, "add", ".")
	testutil.MustGit(t, wt, "commit", "-m", "feat")

	killAndWait(t, env, created.ID)
	exit := waitForExitEntry(t, env, created.ID)
	if exit["branchState"] != "in-orbit" {
		t.Errorf("journal branchState = %v, want in-orbit", exit["branchState"])
	}
	if exit["filesChanged"] != float64(1) || exit["insertions"] != float64(2) || exit["deletions"] != float64(0) || exit["uncommitted"] != float64(0) {
		t.Errorf("journal debrief fields wrong: %v", exit)
	}

	// the session object carries the same debrief
	s := env.sessions.Get(created.ID)
	if s == nil || s.Debrief == nil {
		t.Fatalf("exited session missing debrief: %+v", s)
	}
	if s.Debrief.FilesChanged != 1 || s.Debrief.Insertions != 2 || s.Debrief.BranchState != "in-orbit" {
		t.Errorf("session debrief = %+v", s.Debrief)
	}
	// worktree cleaned, branch kept in orbit
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after a clean exit")
	}
	if out := testutil.MustGit(t, repo, "branch", "--list", "gc/*"); out == "" {
		t.Error("gc/ branch with commits should survive the exit")
	}

	// while the exited session is still listed, landed must not double-report it
	rec := doReq(t, env.handler, "GET", "/sessions", "", nil)
	var list struct {
		Sessions []sessions.Session       `json:"sessions"`
		Landed   []sessions.LandedSession `json:"landed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Landed) != 0 {
		t.Errorf("landed must exclude sessions the manager still lists: %+v", list.Landed)
	}

	// dismissing the record surfaces the journal-derived landed entry
	if rec := doReq(t, env.handler, "DELETE", "/sessions/"+created.ID+"/record", "", nil); rec.Code != 200 {
		t.Fatalf("dismiss = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, env.handler, "GET", "/sessions", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Landed) != 1 || list.Landed[0].ID != created.ID {
		t.Fatalf("landed after dismissal = %+v", list.Landed)
	}
	l := list.Landed[0]
	if l.Debrief == nil || l.Debrief.BranchState != "in-orbit" || l.Debrief.FilesChanged != 1 || l.Debrief.Insertions != 2 {
		t.Errorf("landed debrief = %+v", l.Debrief)
	}
	if l.Name != "orbit" || l.Folder != repo || l.SpawnMode != "worktree" || l.StartedAt == "" || l.ExitedAt == "" {
		t.Errorf("landed entry = %+v", l)
	}
	if l.ExitCode == nil || *l.ExitCode != 1 {
		t.Errorf("landed exitCode = %v, want 1 (signal-killed)", l.ExitCode)
	}
}

func TestSessionExitDebriefMergedOnCleanExit(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "no-commits")
	killAndWait(t, env, created.ID)
	exit := waitForExitEntry(t, env, created.ID)
	if exit["branchState"] != "merged" {
		t.Errorf("journal branchState = %v, want merged", exit["branchState"])
	}
	if exit["filesChanged"] != float64(0) || exit["uncommitted"] != float64(0) {
		t.Errorf("no-work exit should report zeros: %v", exit)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed")
	}
	if out := testutil.MustGit(t, repo, "branch", "--list", "gc/*"); out != "" {
		t.Errorf("commit-less gc/ branch should be deleted, still have %q", out)
	}
}

func TestSessionExitDebriefWorktreeKept(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "dirty")
	// an untracked file makes the worktree unremovable without --force
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("half-done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	killAndWait(t, env, created.ID)
	exit := waitForExitEntry(t, env, created.ID)
	if exit["branchState"] != "worktree-kept" {
		t.Errorf("journal branchState = %v, want worktree-kept", exit["branchState"])
	}
	if exit["uncommitted"] != float64(1) || exit["filesChanged"] != float64(0) {
		t.Errorf("dirty exit debrief fields wrong: %v", exit)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("dirty worktree must be kept on disk: %v", err)
	}
	s := env.sessions.Get(created.ID)
	if s == nil || s.Debrief == nil || s.Debrief.BranchState != "worktree-kept" {
		t.Errorf("session debrief = %+v", s)
	}
}

func TestSessionExitDebriefAbsentOnGitFailure(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "broken")
	// nuking the worktree makes every exit-path git call fail
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	killAndWait(t, env, created.ID)
	exit := waitForExitEntry(t, env, created.ID)
	if _, ok := exit["branchState"]; ok {
		t.Errorf("failed git work must not journal debrief fields: %v", exit)
	}
	s := env.sessions.Get(created.ID)
	if s == nil {
		t.Fatal("session record gone")
	}
	if s.Debrief != nil {
		t.Errorf("debrief should be absent on git failure: %+v", s.Debrief)
	}
	if s.State != sessions.StateExited {
		t.Errorf("teardown must complete despite git failure; state = %s", s.State)
	}
	rec := doReq(t, env.handler, "GET", "/sessions/"+created.ID, "", nil)
	if strings.Contains(rec.Body.String(), "debrief") {
		t.Errorf("debrief key must be absent when not captured: %s", rec.Body.String())
	}
}

func orbitList(t *testing.T, env *testEnv) []sessions.OrbitBranch {
	t.Helper()
	rec := doReq(t, env.handler, "GET", "/orbit", "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /orbit = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"orbit":null`) {
		t.Fatal("orbit must be an array, never null")
	}
	var out struct {
		Orbit []sessions.OrbitBranch `json:"orbit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Orbit
}

func orbitDelete(t *testing.T, env *testEnv, repo, branch string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, env.handler, "DELETE", "/orbit?repo="+url.QueryEscape(repo)+"&branch="+url.QueryEscape(branch), "", nil)
}

func TestOrbitListAndSweep(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	testutil.MustGit(t, repo, "branch", "gc/merged-fix")
	testutil.MustGit(t, repo, "switch", "-c", "gc/unmerged-feat")
	testutil.CommitFile(t, repo, "work.txt", "orbit work", "2026-01-02T00:00:00Z")
	testutil.MustGit(t, repo, "switch", "main")

	// the scan is journal-driven; a launch from a subfolder resolves to the repo root
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	env.journal.Append(map[string]any{"event": "session.start", "id": "s1", "folder": sub})

	orbit := orbitList(t, env)
	if len(orbit) != 2 {
		t.Fatalf("orbit = %+v, want 2 entries", orbit)
	}
	byBranch := map[string]sessions.OrbitBranch{}
	for _, o := range orbit {
		byBranch[o.Branch] = o
		if o.Repo != repo {
			t.Errorf("entry repo = %q, want %q", o.Repo, repo)
		}
		if o.HeldBy != nil {
			t.Errorf("unattached branch carries heldBy: %+v", o)
		}
		if _, err := time.Parse(time.RFC3339, o.LastCommitAt); err != nil {
			t.Errorf("lastCommitAt %q is not RFC3339: %v", o.LastCommitAt, err)
		}
	}
	if !byBranch["gc/merged-fix"].Merged {
		t.Errorf("gc/merged-fix = %+v, want merged", byBranch["gc/merged-fix"])
	}
	if byBranch["gc/unmerged-feat"].Merged {
		t.Errorf("gc/unmerged-feat = %+v, want unmerged", byBranch["gc/unmerged-feat"])
	}

	// unmerged: the safe delete is refused and the branch survives
	rec := orbitDelete(t, env, repo, "gc/unmerged-feat")
	if rec.Code != 409 || errCode(t, rec) != "branch_not_merged" {
		t.Errorf("sweep unmerged = %d %s, want 409 branch_not_merged", rec.Code, rec.Body.String())
	}
	if out := testutil.MustGit(t, repo, "branch", "--list", "gc/unmerged-feat"); out == "" {
		t.Error("unmerged branch must survive a refused sweep")
	}

	// merged: swept and journaled
	rec = orbitDelete(t, env, repo, "gc/merged-fix")
	if rec.Code != 200 {
		t.Fatalf("sweep merged = %d: %s", rec.Code, rec.Body.String())
	}
	if out := testutil.MustGit(t, repo, "branch", "--list", "gc/merged-fix"); out != "" {
		t.Errorf("merged branch should be gone, still have %q", out)
	}
	found := false
	for _, e := range env.journal.Read() {
		if e["event"] == "orbit.swept" && e["repo"] == repo && e["branch"] == "gc/merged-fix" {
			found = true
		}
	}
	if !found {
		t.Error("a successful sweep must journal orbit.swept")
	}

	// the swept branch is gone from the next list, and re-sweeping 404s
	if got := orbitList(t, env); len(got) != 1 || got[0].Branch != "gc/unmerged-feat" {
		t.Errorf("orbit after sweep = %+v, want only gc/unmerged-feat", got)
	}
	rec = orbitDelete(t, env, repo, "gc/merged-fix")
	if rec.Code != 404 || errCode(t, rec) != "not_found" {
		t.Errorf("re-sweep = %d %s, want 404 not_found", rec.Code, rec.Body.String())
	}
}

func TestOrbitDeleteValidation(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	plain := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo, plain}})

	cases := []struct {
		name     string
		target   string
		wantCode int
		wantErr  string
	}{
		{"missing params", "/orbit", 400, "missing_param"},
		{"missing branch", "/orbit?repo=" + url.QueryEscape(repo), 400, "missing_param"},
		// the gc/ gate fires before the repo is even looked at — a flag-shaped
		// or user branch name never reaches git
		{"flag-shaped branch", "/orbit?repo=/definitely/not/there&branch=-D", 400, "invalid_param"},
		{"user branch", "/orbit?repo=" + url.QueryEscape(repo) + "&branch=main", 400, "invalid_param"},
		{"repo outside roots", "/orbit?repo=/definitely/not/there&branch=gc/x", 400, "invalid_path"},
		{"repo path gone", "/orbit?repo=" + url.QueryEscape(filepath.Join(repo, "nope")) + "&branch=gc/x", 400, "invalid_path"},
		{"repo not a git repo", "/orbit?repo=" + url.QueryEscape(plain) + "&branch=gc/x", 400, "invalid_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, env.handler, "DELETE", tc.target, "", nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := errCode(t, rec); got != tc.wantErr {
				t.Errorf("error code = %s, want %s", got, tc.wantErr)
			}
		})
	}
	// nothing was deleted along the way
	if out := testutil.MustGit(t, repo, "branch", "--list", "main"); out == "" {
		t.Error("main must survive every rejected sweep")
	}
}

func TestOrbitLiveSessionBranchExcluded(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "orbit-live")
	branch := gitx.CurrentBranch(wt)
	if !strings.HasPrefix(branch, "gc/") {
		t.Fatalf("worktree branch = %q, want gc/*", branch)
	}
	// commit real work so the branch will be in orbit after exit
	if err := os.WriteFile(filepath.Join(wt, "feat.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.MustGit(t, wt, "add", ".")
	testutil.MustGit(t, wt, "commit", "-m", "feat")

	// a running session's branch is never listed and never sweepable
	for _, o := range orbitList(t, env) {
		if o.Branch == branch {
			t.Errorf("live session branch %q must not be in orbit: %+v", branch, o)
		}
	}
	rec := orbitDelete(t, env, repo, branch)
	if rec.Code != 409 || errCode(t, rec) != "branch_held" {
		t.Errorf("sweep of live branch = %d %s, want 409 branch_held", rec.Code, rec.Body.String())
	}

	killAndWait(t, env, created.ID)
	waitForExitEntry(t, env, created.ID)

	// exited but undismissed: the surviving branch is exactly what orbit surfaces
	found := false
	for _, o := range orbitList(t, env) {
		if o.Branch == branch {
			found = true
			if o.Merged {
				t.Errorf("branch with unmerged commits flagged merged: %+v", o)
			}
			if o.HeldBy != nil {
				t.Errorf("worktree is gone, heldBy = %q", *o.HeldBy)
			}
		}
	}
	if !found {
		t.Errorf("exited session's surviving branch %q missing from orbit", branch)
	}
	// still unmerged, so the safe delete refuses
	rec = orbitDelete(t, env, repo, branch)
	if rec.Code != 409 || errCode(t, rec) != "branch_not_merged" {
		t.Errorf("sweep after exit = %d %s, want 409 branch_not_merged", rec.Code, rec.Body.String())
	}
}

func TestOrbitHeldByKeptWorktree(t *testing.T) {
	testutil.FakeClaude(t)
	repo := testutil.InitRepo(t)
	env := newTestEnv(t, config.Config{Roots: []string{repo}})

	created, wt := createWorktreeSession(t, env, repo, "orbit-dirty")
	branch := gitx.CurrentBranch(wt)
	// an untracked file blocks removal, so the exit keeps the worktree —
	// which then holds the branch
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("half-done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	killAndWait(t, env, created.ID)
	waitForExitEntry(t, env, created.ID)

	var entry *sessions.OrbitBranch
	for _, o := range orbitList(t, env) {
		if o.Branch == branch {
			entry = &o
		}
	}
	if entry == nil {
		t.Fatalf("kept-worktree branch %q missing from orbit", branch)
	}
	if entry.HeldBy == nil {
		t.Fatalf("kept-worktree branch must carry heldBy: %+v", entry)
	}
	// compare symlink-resolved: git reports the worktree's resolved path
	wantWt, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	gotWt, err := filepath.EvalSymlinks(*entry.HeldBy)
	if err != nil {
		t.Fatal(err)
	}
	if gotWt != wantWt {
		t.Errorf("heldBy = %q, want %q", *entry.HeldBy, wt)
	}

	// held branches route to DELETE /worktrees, not the sweep
	rec := orbitDelete(t, env, repo, branch)
	if rec.Code != 409 || errCode(t, rec) != "branch_held" {
		t.Errorf("sweep of held branch = %d %s, want 409 branch_held", rec.Code, rec.Body.String())
	}
	if out := testutil.MustGit(t, repo, "branch", "--list", branch); out == "" {
		t.Error("held branch must survive the refused sweep")
	}
}

func TestPostJobsValidation(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})
	h := env.handler

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"missing folder", `{"prompt":"x"}`, 400, "missing_param"},
		{"missing prompt", fmt.Sprintf(`{"folder":%q}`, root), 400, "missing_param"},
		{"whitespace prompt", fmt.Sprintf(`{"folder":%q,"prompt":"   "}`, root), 400, "missing_param"},
		{"outside roots", `{"folder":"/not/in/roots","prompt":"x"}`, 400, "outside_roots"},
		{"bad callbackUrl", fmt.Sprintf(`{"folder":%q,"prompt":"x","callbackUrl":"ws://x"}`, root), 400, "invalid_param"},
		{"isolation true", fmt.Sprintf(`{"folder":%q,"prompt":"x","isolation":true}`, root), 501, "not_implemented"},
		{"docker true", fmt.Sprintf(`{"folder":%q,"prompt":"x","docker":true}`, root), 501, "not_implemented"},
		{"isolation non-bool", fmt.Sprintf(`{"folder":%q,"prompt":"x","isolation":"yes"}`, root), 400, "invalid_param"},
		// fails inside Create before any spawn: worktree mode needs a repo
		{"worktree in non-repo", fmt.Sprintf(`{"folder":%q,"prompt":"x","spawnMode":"worktree"}`, root), 409, "launch_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, "POST", "/jobs", tc.body, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := errCode(t, rec); got != tc.wantErr {
				t.Errorf("error code = %s, want %s", got, tc.wantErr)
			}
		})
	}
	if list := env.jobs.List(); len(list) != 0 {
		t.Errorf("validation failures must not create jobs: %+v", list)
	}
}

func TestPutConfigValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{})
	h := env.handler

	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		// wrong JSON types fail in decode as invalid_param / invalid_json
		{"roots not a list", `{"roots":"x"}`, "invalid_param"},
		{"roots element not a string", `{"roots":[123]}`, "invalid_param"},
		{"events element not a string", `{"webhooks":[{"url":"http://x","events":[1]}]}`, "invalid_param"},
		{"array body", `[]`, "invalid_json"},
		// semantic failures are invalid_config
		{"empty roots", `{"roots":[]}`, "invalid_config"},
		{"relative root", `{"roots":["relative/path"]}`, "invalid_config"},
		{"non-directory root", `{"roots":["/definitely/not/a/dir-xyz"]}`, "invalid_config"},
		{"bad webhook url", `{"webhooks":[{"url":"ftp://x"}]}`, "invalid_config"},
		{"empty webhook event", `{"webhooks":[{"url":"http://x","events":[""]}]}`, "invalid_config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, "PUT", "/config", tc.body, nil)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != tc.wantErr {
				t.Errorf("error code = %s, want %s", got, tc.wantErr)
			}
		})
	}
}

func TestPutConfigPersists(t *testing.T) {
	t.Parallel()
	oldRoot := testutil.ResolvedTempDir(t)
	newRoot := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{oldRoot}})
	h := env.handler

	body := fmt.Sprintf(`{"roots":[%q],"showHidden":true,"webhooks":[{"url":"http://example.com/hook","events":["job.*"]}]}`, newRoot)
	if rec := doReq(t, h, "PUT", "/config", body, nil); rec.Code != 200 {
		t.Fatalf("PUT /config = %d: %s", rec.Code, rec.Body.String())
	}

	// persisted to disk
	raw, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("config file not JSON: %v", err)
	}
	if len(onDisk.Roots) != 1 || onDisk.Roots[0] != newRoot || !onDisk.ShowHidden || len(onDisk.Webhooks) != 1 {
		t.Errorf("persisted config wrong: %+v", onDisk)
	}

	// reflected by GET /config
	rec := doReq(t, h, "GET", "/config", "", nil)
	var got struct {
		Roots      []string               `json:"roots"`
		ShowHidden bool                   `json:"showHidden"`
		Webhooks   []events.WebhookConfig `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Roots) != 1 || got.Roots[0] != newRoot || !got.ShowHidden {
		t.Errorf("GET /config after PUT = %+v", got)
	}
	if len(got.Webhooks) != 1 || got.Webhooks[0].URL != "http://example.com/hook" {
		t.Errorf("webhooks not reflected: %+v", got.Webhooks)
	}

	// the browser was reconfigured live
	if !env.browser.WithinRoots(newRoot) || env.browser.WithinRoots(oldRoot) {
		t.Error("PUT /config did not re-apply the roots to the browser")
	}
}

// getPresets decodes the presets list out of GET /config.
func getPresets(t *testing.T, h http.Handler) []config.Preset {
	t.Helper()
	rec := doReq(t, h, "GET", "/config", "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /config = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Presets []config.Preset `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Presets
}

func TestPutConfigPresets(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{})
	h := env.handler

	// two presets round-trip: one fully loaded (env key in settings is fine),
	// one bare name (no settings JSON is valid)
	body := `{"presets":[
		{"name":"yolo","permissionMode":"bypassPermissions","spawnMode":"worktree","capacity":4,"settingsJson":"{\"env\":{\"FOO\":\"bar\"}}"},
		{"name":"plain"}
	]}`
	if rec := doReq(t, h, "PUT", "/config", body, nil); rec.Code != 200 {
		t.Fatalf("PUT /config = %d: %s", rec.Code, rec.Body.String())
	}
	presets := getPresets(t, h)
	if len(presets) != 2 {
		t.Fatalf("presets = %+v, want 2", presets)
	}
	p := presets[0]
	if p.Name != "yolo" || p.PermissionMode != "bypassPermissions" || p.SpawnMode != "worktree" || p.Capacity != 4 || p.SettingsJSON != `{"env":{"FOO":"bar"}}` {
		t.Errorf("preset did not round-trip: %+v", p)
	}
	if presets[1].Name != "plain" || presets[1].SettingsJSON != "" {
		t.Errorf("bare preset wrong: %+v", presets[1])
	}

	// survives a reload: a fresh instance built from the persisted file
	// serves the same presets
	onDisk, err := config.Load(env.configPath)
	if err != nil {
		t.Fatalf("persisted config does not load: %v", err)
	}
	env2 := newTestEnv(t, onDisk)
	if reloaded := getPresets(t, env2.handler); len(reloaded) != 2 || reloaded[0].Name != "yolo" {
		t.Errorf("presets after reload = %+v", reloaded)
	}

	// a partial update that omits presets leaves them alone
	if rec := doReq(t, h, "PUT", "/config", `{"showHidden":true}`, nil); rec.Code != 200 {
		t.Fatalf("partial PUT /config = %d: %s", rec.Code, rec.Body.String())
	}
	if presets := getPresets(t, h); len(presets) != 2 {
		t.Errorf("partial update dropped presets: %+v", presets)
	}

	// an explicit empty array clears them
	if rec := doReq(t, h, "PUT", "/config", `{"presets":[]}`, nil); rec.Code != 200 {
		t.Fatalf("clearing PUT /config = %d: %s", rec.Code, rec.Body.String())
	}
	if presets := getPresets(t, h); len(presets) != 0 {
		t.Errorf("empty array did not clear presets: %+v", presets)
	}
}

func TestPutConfigPresetValidation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{})
	h := env.handler

	// seed a known-good preset so "config unchanged" is observable on disk
	if rec := doReq(t, h, "PUT", "/config", `{"presets":[{"name":"keep"}]}`, nil); rec.Code != 200 {
		t.Fatalf("seed PUT /config = %d: %s", rec.Code, rec.Body.String())
	}

	oversized := fmt.Sprintf(`{\"pad\":\"%s\"}`, strings.Repeat("a", 65*1024))
	cases := []struct {
		name string
		body string
	}{
		{"duplicate names", `{"presets":[{"name":"a"},{"name":"a"}]}`},
		{"empty name", `{"presets":[{"name":""}]}`},
		{"unknown permission mode", `{"presets":[{"name":"a","permissionMode":"yolo"}]}`},
		{"unknown spawn mode", `{"presets":[{"name":"a","spawnMode":"docker"}]}`},
		{"capacity over 256", `{"presets":[{"name":"a","capacity":257}]}`},
		{"negative capacity", `{"presets":[{"name":"a","capacity":-1}]}`},
		{"malformed settings", `{"presets":[{"name":"a","settingsJson":"{not json"}]}`},
		{"settings not an object", `{"presets":[{"name":"a","settingsJson":"[1,2]"}]}`},
		{"settings null", `{"presets":[{"name":"a","settingsJson":"null"}]}`},
		{"settings over 64KB", `{"presets":[{"name":"a","settingsJson":"` + oversized + `"}]}`},
		{"hooks key", `{"presets":[{"name":"a","settingsJson":"{\"hooks\":{}}"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, "PUT", "/config", tc.body, nil)
			if rec.Code != 400 {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != "invalid_config" {
				t.Errorf("error code = %s, want invalid_config", got)
			}
			// rejected writes must leave the persisted config untouched
			onDisk, err := config.Load(env.configPath)
			if err != nil {
				t.Fatalf("persisted config does not load: %v", err)
			}
			if len(onDisk.Presets) != 1 || onDisk.Presets[0].Name != "keep" {
				t.Errorf("rejected PUT changed the persisted presets: %+v", onDisk.Presets)
			}
		})
	}
}

func TestJournalRecentEndpoint(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})
	h := env.handler

	folders := make([]string, 7)
	for i := range folders {
		folders[i] = filepath.Join(root, fmt.Sprintf("f%d", i))
		if err := os.MkdirAll(folders[i], 0o755); err != nil {
			t.Fatal(err)
		}
		env.journal.Append(map[string]any{"event": "session.start", "id": fmt.Sprintf("s%d", i), "name": fmt.Sprintf("n%d", i), "folder": folders[i]})
	}
	// duplicate launch config: deduped, newest occurrence wins
	env.journal.Append(map[string]any{"event": "session.start", "id": "s3b", "name": "n3b", "folder": folders[3]})

	decode := func(rec *httptest.ResponseRecorder) []sessions.RecentLaunch {
		t.Helper()
		if rec.Code != 200 {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Recent []sessions.RecentLaunch `json:"recent"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Recent
	}

	recent := decode(doReq(t, h, "GET", "/journal/recent", "", nil))
	if len(recent) != 5 {
		t.Errorf("default limit = %d entries, want 5", len(recent))
	}
	if recent[0].Folder != folders[3] {
		t.Errorf("newest first: recent[0].Folder = %q, want %q", recent[0].Folder, folders[3])
	}
	if recent[0].SpawnMode != string(workspace.SpawnSameDir) || recent[0].PermissionMode != "default" {
		t.Errorf("defaults not applied: %+v", recent[0])
	}

	if got := decode(doReq(t, h, "GET", "/journal/recent?limit=20", "", nil)); len(got) != 7 {
		t.Errorf("limit=20 = %d entries, want all 7 (deduped)", len(got))
	}
	if got := decode(doReq(t, h, "GET", "/journal/recent?limit=100", "", nil)); len(got) != 7 {
		t.Errorf("limit clamps to 20; got %d entries", len(got))
	}
	if got := decode(doReq(t, h, "GET", "/journal/recent?limit=0", "", nil)); len(got) != 1 {
		t.Errorf("limit=0 clamps to 1; got %d entries", len(got))
	}
	if got := decode(doReq(t, h, "GET", "/journal/recent?limit=-5", "", nil)); len(got) != 1 {
		t.Errorf("negative limit clamps to 1; got %d entries", len(got))
	}
	if got := decode(doReq(t, h, "GET", "/journal/recent?limit=3", "", nil)); len(got) != 3 {
		t.Errorf("limit=3 = %d entries", len(got))
	}
	rec := doReq(t, h, "GET", "/journal/recent?limit=abc", "", nil)
	if rec.Code != 400 || errCode(t, rec) != "invalid_param" {
		t.Errorf("non-numeric limit = %d %s", rec.Code, rec.Body.String())
	}
}

func TestEmptyListsMarshalAsArrays(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})
	h := env.handler

	cases := []struct {
		target string
		want   []string
	}{
		{"/sessions", []string{`"sessions":[]`, `"lost":[]`, `"landed":[]`}},
		{"/jobs", []string{`"jobs":[]`}},
		{"/worktrees", []string{`"worktrees":[]`}},
		{"/orbit", []string{`"orbit":[]`}},
		{"/config", []string{`"webhooks":[]`}},
		{"/journal/recent", []string{`"recent":[]`}},
		{"/branches?path=" + root, []string{`"branches":[]`}}, // in roots, not a repo
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			rec := doReq(t, h, "GET", tc.target, "", nil)
			if rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("body %s missing %s", body, want)
				}
			}
			if strings.Contains(body, "null") {
				t.Errorf("empty list marshaled as null: %s", body)
			}
		})
	}
}

func TestNotFoundAndParamErrors(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, config.Config{})
	h := env.handler

	notFound := []struct{ method, target string }{
		{"GET", "/sessions/nope"},
		{"GET", "/sessions/nope/qr"},
		{"GET", "/sessions/nope/log"},
		{"DELETE", "/sessions/nope"},
		{"DELETE", "/sessions/nope/record"},
		{"GET", "/jobs/nope"},
		{"GET", "/jobs/nope/log"},
		{"DELETE", "/jobs/nope"},
		{"DELETE", "/jobs/nope/record"},
	}
	for _, tc := range notFound {
		rec := doReq(t, h, tc.method, tc.target, "", nil)
		if rec.Code != 404 || errCode(t, rec) != "not_found" {
			t.Errorf("%s %s = %d %s, want 404 not_found", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}

	missing := []struct{ method, target string }{
		{"GET", "/browse"},
		{"GET", "/branches"},
		{"DELETE", "/worktrees"},
	}
	for _, tc := range missing {
		rec := doReq(t, h, tc.method, tc.target, "", nil)
		if rec.Code != 400 || errCode(t, rec) != "missing_param" {
			t.Errorf("%s %s = %d %s, want 400 missing_param", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}

	rec := doReq(t, h, "GET", "/browse?path=/outside/everything", "", nil)
	if rec.Code != 400 || errCode(t, rec) != "invalid_path" {
		t.Errorf("browse outside roots = %d %s, want 400 invalid_path", rec.Code, rec.Body.String())
	}
}

/* ---------- Claude state enrichment on the wire ---------- */

// enrichmentKeys are the registry-sourced session fields; all five must be
// absent (not null, not empty) when nothing was captured.
var enrichmentKeys = []string{`"claudeSessionId"`, `"activity"`, `"environmentSessions"`, `"folderSessions"`, `"prLink"`}

func TestSessionEnrichmentWireShape(t *testing.T) {
	t.Parallel()
	uuid := "6f0a2c1e-aaaa-bbbb-cccc-1234567890ab"
	activity := "busy"
	s := sessions.Session{
		ID:              "abc12345",
		Name:            "n",
		Folder:          "/f",
		ClaudeSessionID: &uuid,
		Activity:        &activity,
		EnvironmentSessions: []sessions.ExtraSession{
			{Name: "gc-x-1", Status: "idle"},
		},
		FolderSessions: []sessions.ExtraSession{
			{Name: "manual"}, // unknown status serializes to no status key
		},
		PRLink: &sessions.PRLink{Number: 9, URL: "https://github.com/o/r/pull/9"},
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		`"claudeSessionId":"` + uuid + `"`,
		`"activity":"busy"`,
		`"environmentSessions":[{"name":"gc-x-1","status":"idle"}]`,
		`"folderSessions":[{"name":"manual"}]`,
		`"prLink":{"number":9,"url":"https://github.com/o/r/pull/9"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session JSON missing %s: %s", want, body)
		}
	}

	// an unenriched session omits all five keys entirely
	empty, err := json.Marshal(sessions.Session{ID: "abc12345"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range enrichmentKeys {
		if strings.Contains(string(empty), key) {
			t.Errorf("unenriched session must omit %s: %s", key, empty)
		}
	}
}

func TestSessionsPayloadOmitsEnrichmentWhenAbsent(t *testing.T) {
	testutil.FakeClaude(t)
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})

	created := createSession(t, env, fmt.Sprintf(`{"folder":%q}`, root))
	for _, target := range []string{"/sessions", "/sessions/" + created.ID} {
		rec := doReq(t, env.handler, "GET", target, "", nil)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
		for _, key := range enrichmentKeys {
			if strings.Contains(rec.Body.String(), key) {
				t.Errorf("GET %s leaked %s for an unenriched session: %s", target, key, rec.Body.String())
			}
		}
	}
}

func TestLostAndLandedClaudeSessionID(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	env := newTestEnv(t, config.Config{Roots: []string{root}})

	// a lost session's UUID lives on its standalone session.claude-id entry
	env.journal.Append(map[string]any{"event": "session.start", "id": "lost1", "name": "l1", "folder": root})
	env.journal.Append(map[string]any{"event": "session.claude-id", "id": "lost1", "claudeSessionId": "uuid-lost"})
	env.journal.Append(map[string]any{"event": "session.start", "id": "lost2", "name": "l2", "folder": root})
	// a landed session's UUID rides the flattened session.exit entry
	env.journal.Append(map[string]any{"event": "session.start", "id": "done1", "name": "d1", "folder": root})
	env.journal.Append(map[string]any{"event": "session.exit", "id": "done1", "code": 0, "claudeSessionId": "uuid-done"})
	env.journal.Append(map[string]any{"event": "session.start", "id": "done2", "name": "d2", "folder": root})
	env.journal.Append(map[string]any{"event": "session.exit", "id": "done2", "code": 0})

	rec := doReq(t, env.handler, "GET", "/sessions", "", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /sessions = %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Lost   []sessions.LostSession   `json:"lost"`
		Landed []sessions.LandedSession `json:"landed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	lost := map[string]sessions.LostSession{}
	for _, l := range list.Lost {
		lost[l.ID] = l
	}
	if got := lost["lost1"].ClaudeSessionID; got == nil || *got != "uuid-lost" {
		t.Errorf("lost1 claudeSessionId = %v, want uuid-lost", got)
	}
	if got := lost["lost2"].ClaudeSessionID; got != nil {
		t.Errorf("lost2 claudeSessionId = %q, want absent", *got)
	}

	landed := map[string]sessions.LandedSession{}
	for _, l := range list.Landed {
		landed[l.ID] = l
	}
	if got := landed["done1"].ClaudeSessionID; got == nil || *got != "uuid-done" {
		t.Errorf("done1 claudeSessionId = %v, want uuid-done", got)
	}
	if got := landed["done2"].ClaudeSessionID; got != nil {
		t.Errorf("done2 claudeSessionId = %q, want absent", *got)
	}

	// absent means absent on the wire, not null
	body := rec.Body.String()
	if strings.Count(body, `"claudeSessionId"`) != 2 {
		t.Errorf("expected exactly two claudeSessionId keys (lost1, done1): %s", body)
	}
}

func TestSessionsExtrasOverHTTP(t *testing.T) {
	root := testutil.ResolvedTempDir(t)
	// one same-folder registry row that no launch owns: it must surface as a
	// folder row on the launched session. startedAt predates any launch, so
	// even the ps-less fallback can never mistake it for the primary.
	agents := fmt.Sprintf(`[{"pid":999999,"cwd":%q,"sessionId":"conv-extra","name":"folder-mate","status":"busy","startedAt":1}]`, root)
	testutil.FakeClaudeWith(t, testutil.FakeClaudeConfig{AgentsJSON: agents})
	env := newTestEnv(t, config.Config{Roots: []string{root}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env.sessions.StartRegistryLoop(ctx)

	created := createSession(t, env, fmt.Sprintf(`{"folder":%q}`, root))

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rec := doReq(t, env.handler, "GET", "/sessions", "", nil)
		if rec.Code != 200 {
			t.Fatalf("GET /sessions = %d: %s", rec.Code, rec.Body.String())
		}
		var list struct {
			Sessions []sessions.Session `json:"sessions"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, s := range list.Sessions {
			if s.ID != created.ID {
				continue
			}
			for _, e := range s.FolderSessions {
				if e.Name == "folder-mate" && e.Status == "busy" {
					// the unowned row must never join as the primary
					if s.ClaudeSessionID != nil {
						t.Errorf("folder-mate row must not be captured as the session's own uuid: %v", *s.ClaudeSessionID)
					}
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("folder-mate extra never appeared on GET /sessions")
}
