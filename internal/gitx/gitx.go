// Package gitx wraps the git CLI: every call is bounded by a hard timeout and
// surfaces git's own stderr message, the most useful thing when a command fails.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Exec runs git -C dir <args...> with a hard timeout. It returns the trimmed
// stdout plus the last non-empty stderr line — git's own message is the most
// useful thing to surface when a command fails.
func Exec(dir string, timeout time.Duration, args ...string) (stdout string, stderrLine string, err error) {
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

func Out(dir string, timeout time.Duration, args ...string) (string, error) {
	out, stderrLine, err := Exec(dir, timeout, args...)
	if err != nil {
		if stderrLine != "" {
			return "", errors.New(stderrLine)
		}
		return "", err
	}
	return out, nil
}

func Run(dir string, timeout time.Duration, args ...string) error {
	_, err := Out(dir, timeout, args...)
	return err
}

// Root returns the toplevel of the repo containing path, or "" when not a repo.
func Root(path string) string {
	out, err := Out(path, 2*time.Second, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// Branch: "(detached)" when git succeeds but reports no current branch,
// nil when git fails (not a repo, timeout, ...).
func Branch(path string) *string {
	out, err := Out(path, 2*time.Second, "branch", "--show-current")
	if err != nil {
		return nil
	}
	if out == "" {
		out = "(detached)"
	}
	return &out
}

// CurrentBranch returns the current branch of the repo containing folder, or ""
// when detached/not a repo.
func CurrentBranch(folder string) string {
	out, err := Out(folder, 5*time.Second, "branch", "--show-current")
	if err != nil {
		return ""
	}
	return out
}

func IsGitFolder(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// ResolveBranch resolves a picked branch name to a checkout-able ref: local branch
// as-is, else the remote-tracking ref (the picker offers remote branches a fresh
// clone never checked out). Returns "" when not found.
func ResolveBranch(folder, branch string) string {
	if Run(folder, 2*time.Second, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil {
		return branch
	}
	// not a local branch
	out, err := Out(folder, 2*time.Second, "for-each-ref", "--format=%(refname:short)", "refs/remotes/*/"+branch)
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

func BranchExists(folder, branch string) bool {
	return ResolveBranch(folder, branch) != ""
}
