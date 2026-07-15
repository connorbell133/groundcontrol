// Package workspace manages runner-owned git worktrees: one branch, one
// worktree, cleaned up honestly. It also owns the spawn-mode vocabulary shared
// by sessions and jobs, and the runner lock that gates the orphan sweep.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/connorbell133/groundcontrol/internal/gitx"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/util"
)

// SpawnMode keeps string underneath: JSON marshals identically, and the
// untyped literals other files compare against still compile.
type SpawnMode string

const (
	SpawnSameDir  SpawnMode = "same-dir"
	SpawnWorktree SpawnMode = "worktree"
)

type Manager struct {
	base    string // base directory for runner-managed worktrees
	journal *journal.Journal
}

func New(base string, j *journal.Journal) *Manager {
	return &Manager{base: base, journal: j}
}

// runnerLockFile stays referenced for the process lifetime — a finalizer
// closing it would release the flock.
//
//lint:ignore U1000 write-only by design; the assignment itself keeps the *os.File alive
var runnerLockFile *os.File

// AcquireRunnerLock takes a non-blocking exclusive flock scoped to the shared
// worktree base. Held until exit (the kernel releases it with the process), it
// marks this instance as the one allowed to treat on-disk worktrees as orphans.
func (m *Manager) AcquireRunnerLock() bool {
	if err := os.MkdirAll(filepath.Dir(m.base), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(m.base), "runner.lock"), os.O_CREATE|os.O_RDWR, 0o644)
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

type Info struct {
	Path, Branch, BaseCommit string
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

func (m *Manager) Add(folder, branch, id, label string) (Info, error) {
	wtPath := filepath.Join(m.base, filepath.Base(folder), id)
	if err := os.MkdirAll(filepath.Join(m.base, filepath.Base(folder)), 0o755); err != nil {
		return Info{}, err
	}
	base := gitx.ResolveBranch(folder, branch)
	if base == "" {
		return Info{}, fmt.Errorf("branch %s not found locally or on a remote", branch)
	}
	// gc/<what-this-run-is-for>-<id>: the label answers "what is this branch"
	// at a glance in `git branch`, the id keeps it collision-free
	wtBranch := "gc/" + id
	if slug := slugify(label); slug != "" {
		wtBranch = "gc/" + slug + "-" + id
	}
	// each run works on its own branch cut from the base: the base may be checked
	// out in the main folder (git refuses a second checkout) or exist only on a remote
	if _, stderrLine, err := gitx.Exec(folder, 15*time.Second, "worktree", "add", "-b", wtBranch, wtPath, base); err != nil {
		// git's own message is the most useful thing to surface
		if stderrLine == "" {
			stderrLine = fmt.Sprintf("could not create worktree for %s", branch)
		}
		return Info{}, errors.New(stderrLine)
	}
	baseCommit, err := gitx.Out(wtPath, 5*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return Info{}, err
	}
	return Info{Path: wtPath, Branch: wtBranch, BaseCommit: baseCommit}, nil
}

// Remove removes the worktree and, when the run branch never accumulated
// commits, its gc/ branch too. wtBranch/baseCommit may be "" (skip branch deletion).
func (m *Manager) Remove(folder, wtPath, wtBranch, baseCommit string) {
	if err := gitx.Run(folder, 15*time.Second, "worktree", "remove", wtPath); err != nil {
		// dirty or already gone — keep it rather than destroy work, but make it visible
		m.journal.Append(map[string]any{"event": "worktree.kept", "folder": folder, "wtPath": wtPath, "reason": "dirty or removal failed"})
		return
	}
	// the run branch outlives the worktree only if it accumulated commits
	if wtBranch != "" && baseCommit != "" {
		tip, err := gitx.Out(folder, 5*time.Second, "rev-parse", "refs/heads/"+wtBranch)
		if err == nil && tip == baseCommit {
			// branch already gone / delete failure is fine to ignore
			_ = gitx.Run(folder, 5*time.Second, "branch", "-D", wtBranch)
		}
	}
}

/* ---------- kept worktrees (dirty orphans the sweeps refused to delete) ---------- */

type Kept struct {
	Path   string  `json:"path"`
	Repo   string  `json:"repo"`
	ID     string  `json:"id"`
	Branch *string `json:"branch"`
	Dirty  bool    `json:"dirty"`
}

// ListKept lists on-disk worktrees that no live run owns; live is the set of
// worktree paths belonging to running sessions (the caller snapshots it).
func (m *Manager) ListKept(live map[string]bool) []Kept {
	out := []Kept{}
	if !util.PathExists(m.base) {
		return out
	}
	repos, _ := os.ReadDir(m.base)
	for _, repo := range repos {
		ids, err := os.ReadDir(filepath.Join(m.base, repo.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			wtPath := filepath.Join(m.base, repo.Name(), id.Name())
			if live[wtPath] {
				continue // belongs to a running session
			}
			branch := gitx.CurrentBranch(wtPath)
			dirty := false
			if status, err := gitx.Out(wtPath, 5*time.Second, "status", "--porcelain"); err == nil {
				dirty = len(status) > 0
			}
			// unreadable — still listed so it can be cleaned
			out = append(out, Kept{Path: wtPath, Repo: repo.Name(), ID: id.Name(), Branch: util.StrPtr(branch), Dirty: dirty})
		}
	}
	return out
}

func (m *Manager) ForceRemove(wtPath string) error {
	resolved := filepath.Clean(wtPath) // normalize
	if !strings.HasPrefix(resolved, m.base+"/") {
		return errors.New("not a runner-managed worktree")
	}
	commonDir, err := gitx.Out(resolved, 5*time.Second, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	mainRoot := commonDir
	if strings.HasSuffix(commonDir, "/.git") {
		mainRoot = commonDir[:len(commonDir)-len("/.git")]
	}
	wtBranch := gitx.CurrentBranch(resolved)
	if err := gitx.Run(mainRoot, 15*time.Second, "worktree", "remove", "--force", resolved); err != nil {
		return err
	}
	if err := gitx.Run(mainRoot, 15*time.Second, "worktree", "prune"); err != nil {
		return err
	}
	// safe delete only: commits on the session branch stay reachable after a force-clean
	if strings.HasPrefix(wtBranch, "gc/") {
		// unmerged work — keep the branch
		_ = gitx.Run(mainRoot, 5*time.Second, "branch", "-d", wtBranch)
	}
	m.journal.Append(map[string]any{"event": "worktree.force-removed", "wtPath": resolved})
	return nil
}

// SweepOrphans is the boot sweep: sessions are in-memory, so any worktree on
// disk at startup is an orphan from a previous runner. Remove the clean ones;
// keep dirty ones and journal.
func (m *Manager) SweepOrphans() {
	if !util.PathExists(m.base) {
		return
	}
	repos, _ := os.ReadDir(m.base)
	for _, repo := range repos {
		repoDir := filepath.Join(m.base, repo.Name())
		ids, err := os.ReadDir(repoDir)
		if err != nil {
			continue
		}
		for _, wt := range ids {
			wtPath := filepath.Join(repoDir, wt.Name())
			swept := func() bool {
				commonDir, err := gitx.Out(wtPath, 5*time.Second, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if err != nil {
					return false
				}
				mainRoot := commonDir
				if strings.HasSuffix(commonDir, "/.git") {
					mainRoot = commonDir[:len(commonDir)-len("/.git")]
				}
				wtBranch := gitx.CurrentBranch(wtPath)
				if err := gitx.Run(mainRoot, 15*time.Second, "worktree", "remove", wtPath); err != nil {
					return false
				}
				// -d not -D: an orphan's base commit is unknown, so only drop the session
				// branch when git agrees it holds nothing unmerged
				if strings.HasPrefix(wtBranch, "gc/") {
					// unmerged work — the branch keeps it reachable
					_ = gitx.Run(mainRoot, 5*time.Second, "branch", "-d", wtBranch)
				}
				return true
			}()
			if swept {
				m.journal.Append(map[string]any{"event": "worktree.swept", "wtPath": wtPath})
			} else {
				m.journal.Append(map[string]any{"event": "worktree.kept", "wtPath": wtPath, "reason": "orphan is dirty or unresolvable"})
			}
		}
	}
}
