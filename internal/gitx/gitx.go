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
	"regexp"
	"strconv"
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

// Stderr runs git and returns the full trimmed stderr alongside err, for
// callers that must classify a failure by its complete message. Run/Out keep
// only the last stderr line, which drops multi-line git messages (e.g. the
// "not fully merged" line that precedes git's delete-hint).
func Stderr(dir string, timeout time.Duration, args ...string) (stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// force the C locale so callers can classify failures by git's stable
	// English message text (e.g. "not fully merged") regardless of the runner's
	// locale
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return strings.TrimSpace(errBuf.String()), err
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

// DefaultRef finds the branch "merged" is measured against: the remote's
// declared default when one is set, else conventional local names. "" means
// there is nothing to measure against.
func DefaultRef(root string) string {
	if out, err := Out(root, 2*time.Second, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return out
	}
	for _, name := range []string{"main", "master"} {
		if Run(root, 2*time.Second, "rev-parse", "--verify", "--quiet", "refs/heads/"+name) == nil {
			return "refs/heads/" + name
		}
	}
	return ""
}

// SessionBranch is one gc/* run branch as the orbit view needs it: merged
// verdict, last-commit time, and which worktree (if any) still has it
// checked out.
type SessionBranch struct {
	Branch       string
	Merged       bool
	LastCommitAt string
	WorktreePath string // "" unless some worktree has the branch checked out
}

// sessionBranchTimeout bounds each listing call: the orbit scan walks every
// recently-used repo, so one wedged repo must not stall the whole response.
const sessionBranchTimeout = 10 * time.Second

// SessionBranches lists the repo's gc/* run branches. Merged means the tip
// is reachable from the repo's default branch (DefaultRef) — measured
// against the default, never HEAD, so a feature-branch checkout doesn't skew
// the verdict; with no default at all every branch reads unmerged, the
// honest answer when there is nothing to merge into.
func SessionBranches(root string) ([]SessionBranch, error) {
	// iso8601-strict so lastCommitAt is RFC3339 like every other timestamp on the wire
	out, err := Out(root, sessionBranchTimeout, "for-each-ref", "refs/heads/gc/*",
		"--format=%(refname:short)%00%(objectname)%00%(committerdate:iso8601-strict)%00%(worktreepath)")
	if err != nil {
		return nil, err
	}
	list := []SessionBranch{}
	if out == "" {
		return list, nil
	}
	def := DefaultRef(root)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 4 {
			continue
		}
		b := SessionBranch{Branch: parts[0], LastCommitAt: parts[2], WorktreePath: parts[3]}
		if def != "" {
			b.Merged = Run(root, sessionBranchTimeout, "merge-base", "--is-ancestor", parts[1], def) == nil
		}
		list = append(list, b)
	}
	return list, nil
}

// DiffStats summarizes a run's work: committed plus working-tree changes
// measured from the merge-base with the launch base, and the count of paths
// with uncommitted changes. JSON tags because sessions embeds it in the
// debrief it serves and journals.
type DiffStats struct {
	FilesChanged int `json:"filesChanged"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
	Uncommitted  int `json:"uncommitted"`
}

var (
	filesChangedRE = regexp.MustCompile(`(\d+) files? changed`)
	insertionsRE   = regexp.MustCompile(`(\d+) insertions?\(\+\)`)
	deletionsRE    = regexp.MustCompile(`(\d+) deletions?\(-\)`)
)

// diffStatTimeout bounds each git call: DiffStat runs on session-exit paths
// that must never hang teardown.
const diffStatTimeout = 10 * time.Second

// DiffStat measures dir's work since it diverged from base. It diffs from the
// merge-base, not base itself: agents pull and merge upstream inside
// worktrees, and a diff against the launch commit would count all of upstream
// as the run's own work. The diff is taken against the working tree (not
// HEAD) so uncommitted edits still count as work. Any git failure returns an
// error — callers degrade to an absent debrief.
func DiffStat(dir, base string) (DiffStats, error) {
	mergeBase, err := Out(dir, diffStatTimeout, "merge-base", base, "HEAD")
	if err != nil {
		return DiffStats{}, err
	}
	shortstat, err := Out(dir, diffStatTimeout, "diff", "--shortstat", mergeBase)
	if err != nil {
		return DiffStats{}, err
	}
	num := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(shortstat); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0 // shortstat omits zero-valued parts (and is empty with no changes)
	}
	st := DiffStats{FilesChanged: num(filesChangedRE), Insertions: num(insertionsRE), Deletions: num(deletionsRE)}
	status, err := Out(dir, diffStatTimeout, "status", "--porcelain")
	if err != nil {
		return DiffStats{}, err
	}
	if status != "" {
		st.Uncommitted = len(strings.Split(status, "\n"))
	}
	return st, nil
}
