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

// FakeClaude puts an executable "claude" stub first on PATH so lifecycle
// tests can launch real sessions without the CLI: it prints a pairing-URL
// line (which flips the session to ready) and then blocks until killed —
// exec'ing a bounded sleep so a missed kill can't leak a process forever.
// Uses t.Setenv, so callers must not also call t.Parallel.
func FakeClaude(t *testing.T) {
	t.Helper()
	FakeClaudeWith(t, FakeClaudeConfig{})
}

// FakeClaudeConfig shapes the stub's answers to the state-query subcommands
// claudex execs; zero values keep fixtures terse at call sites.
type FakeClaudeConfig struct {
	Version    string // --version stdout; default "2.1.172 (Claude Code)"
	AgentsJSON string // `agents` stdout; default "[]"
}

// FakeClaudeWith is FakeClaude with subcommand dispatch: --version and agents
// answer from cfg so tests exercise the real claudex detection paths, and
// every other invocation keeps the launch stub's pairing-URL-then-block
// behavior. Uses t.Setenv, so callers must not also call t.Parallel.
func FakeClaudeWith(t *testing.T, cfg FakeClaudeConfig) {
	t.Helper()
	if cfg.Version == "" {
		cfg.Version = "2.1.172 (Claude Code)"
	}
	if cfg.AgentsJSON == "" {
		cfg.AgentsJSON = "[]"
	}
	dir := t.TempDir()
	// fixtures live in files the script cats, so arbitrary JSON (quotes,
	// dollar signs, banner text) never needs shell escaping
	versionFile := filepath.Join(dir, "version.txt")
	agentsFile := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(versionFile, []byte(cfg.Version+"\n"), 0o644); err != nil {
		t.Fatalf("write fake claude version fixture: %v", err)
	}
	if err := os.WriteFile(agentsFile, []byte(cfg.AgentsJSON+"\n"), 0o644); err != nil {
		t.Fatalf("write fake claude agents fixture: %v", err)
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"--version) cat \"" + versionFile + "\" ;;\n" +
		"agents) cat \"" + agentsFile + "\" ;;\n" +
		"*)\n" +
		"echo \"Capacity: 1/32 · New sessions will be created in the current directory\"\n" +
		"echo \"https://claude.ai/remote/abc123\"\n" +
		"exec sleep 300\n" +
		";;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
