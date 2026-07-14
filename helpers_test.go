package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// resolvedTempDir returns a t.TempDir() with symlinks resolved — on macOS the
// temp root lives under /var, a symlink to /private/var, and comparisons
// against symlink-resolved paths would otherwise spuriously fail.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

// testApp builds an isolated instance wired the way main() wires it.
func testApp(t *testing.T, cfg Config) *app {
	t.Helper()
	if cfg.Roots == nil {
		cfg.Roots = []string{resolvedTempDir(t)}
	}
	a := newApp(filepath.Join(t.TempDir(), "config.json"), cfg)
	a.dataDir = t.TempDir()
	a.wtBase = t.TempDir()
	a.configureBrowser(cfg.Roots, cfg.ShowHidden)
	a.configureWebhooks(cfg.Webhooks)
	a.configureJobs(cfg.Jobs)
	return a
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

func mustGitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return mustGitEnv(t, dir, nil, args...)
}

func commitFile(t *testing.T, dir, name, msg, date string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustGit(t, dir, "add", ".")
	env := []string{}
	if date != "" {
		env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	mustGitEnv(t, dir, env, "commit", "-m", msg)
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := resolvedTempDir(t)
	mustGit(t, dir, "init", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "groundcontrol test")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	commitFile(t, dir, "readme.txt", "init", "2026-01-01T00:00:00Z")
	return dir
}
