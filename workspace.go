package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/* ---------- shared helpers (used by every module) ---------- */

// nowISO matches JS Date.toISOString(): UTC, millisecond precision.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(i int) *int {
	return &i
}

// randomID returns n hex characters from crypto/rand (TS: randomUUID().slice(0, n)).
func randomID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)[:n]
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return h
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}

// gitExec runs git -C dir <args...> with a hard timeout. It returns the trimmed
// stdout plus the last non-empty stderr line — git's own message is the most
// useful thing to surface when a command fails.
func gitExec(dir string, timeout time.Duration, args ...string) (stdout string, stderrLine string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	for _, line := range strings.Split(errBuf.String(), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			stderrLine = l
		}
	}
	return strings.TrimSpace(out.String()), stderrLine, err
}

func gitOut(dir string, timeout time.Duration, args ...string) (string, error) {
	out, stderrLine, err := gitExec(dir, timeout, args...)
	if err != nil {
		if stderrLine != "" {
			return "", errors.New(stderrLine)
		}
		return "", err
	}
	return out, nil
}

func gitRun(dir string, timeout time.Duration, args ...string) error {
	_, err := gitOut(dir, timeout, args...)
	return err
}

/* ---------- journal: append-only flight log, shared by sessions and jobs ---------- */

var dataDir = filepath.Join(mustCwd(), "data")
var journalPath = filepath.Join(dataDir, "journal.json")
var journalMu sync.Mutex

func journal(entry map[string]any) {
	journalMu.Lock()
	defer journalMu.Unlock()
	_ = os.MkdirAll(dataDir, 0o755)
	// []any, not []map[string]any: a stray non-object element must not wipe the
	// whole history on the next append (TS JSON.parse preserved any array as-is)
	var all []any
	if raw, err := os.ReadFile(journalPath); err == nil {
		if err := json.Unmarshal(raw, &all); err != nil {
			all = nil // first write
		}
	}
	e := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		e[k] = v
	}
	e["at"] = nowISO()
	all = append(all, e)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(all); err != nil {
		return
	}
	// JSON.stringify(all, null, 2): 2-space indent, no trailing newline
	_ = os.WriteFile(journalPath, bytes.TrimRight(buf.Bytes(), "\n"), 0o644)
}

func readJournal() []map[string]any {
	journalMu.Lock()
	defer journalMu.Unlock()
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return []map[string]any{}
	}
	var all []any
	if err := json.Unmarshal(raw, &all); err != nil {
		return []map[string]any{}
	}
	if len(all) > 2000 {
		all = all[len(all)-2000:]
	}
	// non-object elements drop out here, like TS queries skipping entries
	// whose fields come back undefined
	out := make([]map[string]any, 0, len(all))
	for _, v := range all {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

/* ---------- worktrees: one branch, one worktree, cleaned up honestly ---------- */

var wtBase = filepath.Join(mustHome(), ".groundcontrol", "worktrees")

type worktreeInfo struct {
	wtPath, wtBranch, baseCommit string
}

// resolveBranch resolves a picked branch name to a checkout-able ref: local branch
// as-is, else the remote-tracking ref (the picker offers remote branches a fresh
// clone never checked out). Returns "" when not found.
func resolveBranch(folder, branch string) string {
	if gitRun(folder, 2*time.Second, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil {
		return branch
	}
	// not a local branch
	out, err := gitOut(folder, 2*time.Second, "for-each-ref", "--format=%(refname:short)", "refs/remotes/*/"+branch)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			return line
		}
	}
	return ""
}

func branchExists(folder, branch string) bool {
	return resolveBranch(folder, branch) != ""
}

func addWorktree(folder, branch, id string) (worktreeInfo, error) {
	wtPath := filepath.Join(wtBase, filepath.Base(folder), id)
	if err := os.MkdirAll(filepath.Join(wtBase, filepath.Base(folder)), 0o755); err != nil {
		return worktreeInfo{}, err
	}
	base := resolveBranch(folder, branch)
	if base == "" {
		return worktreeInfo{}, fmt.Errorf("branch %s not found locally or on a remote", branch)
	}
	wtBranch := "gc/" + id
	// each run works on its own branch cut from the base: the base may be checked
	// out in the main folder (git refuses a second checkout) or exist only on a remote
	if _, stderrLine, err := gitExec(folder, 15*time.Second, "worktree", "add", "-b", wtBranch, wtPath, base); err != nil {
		// git's own message is the most useful thing to surface
		if stderrLine == "" {
			stderrLine = fmt.Sprintf("could not create worktree for %s", branch)
		}
		return worktreeInfo{}, errors.New(stderrLine)
	}
	baseCommit, err := gitOut(wtPath, 5*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return worktreeInfo{}, err
	}
	return worktreeInfo{wtPath: wtPath, wtBranch: wtBranch, baseCommit: baseCommit}, nil
}

// removeWorktree removes the worktree and, when the run branch never accumulated
// commits, its gc/ branch too. wtBranch/baseCommit may be "" (skip branch deletion).
func removeWorktree(folder, wtPath, wtBranch, baseCommit string) {
	if err := gitRun(folder, 15*time.Second, "worktree", "remove", wtPath); err != nil {
		// dirty or already gone — keep it rather than destroy work, but make it visible
		journal(map[string]any{"event": "worktree.kept", "folder": folder, "wtPath": wtPath, "reason": "dirty or removal failed"})
		return
	}
	// the run branch outlives the worktree only if it accumulated commits
	if wtBranch != "" && baseCommit != "" {
		tip, err := gitOut(folder, 5*time.Second, "rev-parse", "refs/heads/"+wtBranch)
		if err == nil && tip == baseCommit {
			// branch already gone / delete failure is fine to ignore
			_ = gitRun(folder, 5*time.Second, "branch", "-D", wtBranch)
		}
	}
}

// currentBranch returns the current branch of the repo containing folder, or ""
// when detached/not a repo.
func currentBranch(folder string) string {
	out, err := gitOut(folder, 5*time.Second, "branch", "--show-current")
	if err != nil {
		return ""
	}
	return out
}
