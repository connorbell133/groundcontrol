package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/config"
	"github.com/connorbell133/groundcontrol/internal/events"
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
		{"/sessions", []string{`"sessions":[]`, `"lost":[]`}},
		{"/jobs", []string{`"jobs":[]`}},
		{"/worktrees", []string{`"worktrees":[]`}},
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
