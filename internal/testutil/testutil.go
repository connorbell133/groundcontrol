// Package testutil holds the git-fixture helpers shared by the domain
// packages' tests. It is internal and imported only from _test files.
package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ResolvedTempDir returns a t.TempDir() with symlinks resolved — on macOS the
// temp root lives under /var, a symlink to /private/var, and comparisons
// against symlink-resolved paths would otherwise spuriously fail.
func ResolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func MustGitEnv(t *testing.T, dir string, env []string, args ...string) string {
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

func MustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return MustGitEnv(t, dir, nil, args...)
}

func CommitFile(t *testing.T, dir, name, msg, date string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	MustGit(t, dir, "add", ".")
	env := []string{}
	if date != "" {
		env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	MustGitEnv(t, dir, env, "commit", "-m", msg)
}

func InitRepo(t *testing.T) string {
	t.Helper()
	dir := ResolvedTempDir(t)
	MustGit(t, dir, "init", "-b", "main")
	MustGit(t, dir, "config", "user.email", "test@example.com")
	MustGit(t, dir, "config", "user.name", "groundcontrol test")
	MustGit(t, dir, "config", "commit.gpgsign", "false")
	CommitFile(t, dir, "readme.txt", "init", "2026-01-01T00:00:00Z")
	return dir
}
