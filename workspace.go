package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

/* ---------- worktrees: one branch, one worktree, cleaned up honestly ---------- */

// runnerLockFile stays referenced for the process lifetime — a finalizer
// closing it would release the flock.
var runnerLockFile *os.File

// acquireRunnerLock takes a non-blocking exclusive flock scoped to the shared
// worktree base. Held until exit (the kernel releases it with the process), it
// marks this instance as the one allowed to treat on-disk worktrees as orphans.
func (a *app) acquireRunnerLock() bool {
	if err := os.MkdirAll(filepath.Dir(a.wtBase), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(a.wtBase), "runner.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	runnerLockFile = f
	return true
}

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

// slugify reduces a human label (session name, job prompt) to a branch-safe
// fragment: lowercase, runs of non-alphanumerics collapse to single dashes,
// capped so the id stays readable at the end of the branch name.
var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(label string) string {
	s := slugRE.ReplaceAllString(strings.ToLower(label), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

func (a *app) addWorktree(folder, branch, id, label string) (worktreeInfo, error) {
	wtPath := filepath.Join(a.wtBase, filepath.Base(folder), id)
	if err := os.MkdirAll(filepath.Join(a.wtBase, filepath.Base(folder)), 0o755); err != nil {
		return worktreeInfo{}, err
	}
	base := resolveBranch(folder, branch)
	if base == "" {
		return worktreeInfo{}, fmt.Errorf("branch %s not found locally or on a remote", branch)
	}
	// gc/<what-this-run-is-for>-<id>: the label answers "what is this branch"
	// at a glance in `git branch`, the id keeps it collision-free
	wtBranch := "gc/" + id
	if slug := slugify(label); slug != "" {
		wtBranch = "gc/" + slug + "-" + id
	}
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
func (a *app) removeWorktree(folder, wtPath, wtBranch, baseCommit string) {
	if err := gitRun(folder, 15*time.Second, "worktree", "remove", wtPath); err != nil {
		// dirty or already gone — keep it rather than destroy work, but make it visible
		a.journal(map[string]any{"event": "worktree.kept", "folder": folder, "wtPath": wtPath, "reason": "dirty or removal failed"})
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
