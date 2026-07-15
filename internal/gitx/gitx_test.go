package gitx

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/testutil"
)

func TestGitHelpers(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	nonRepo := testutil.ResolvedTempDir(t)

	if got := Root(repo); got != repo {
		t.Errorf("Root(repo) = %q, want %q", got, repo)
	}
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Root(sub); got != repo {
		t.Errorf("Root(subdir) = %q, want %q", got, repo)
	}
	if got := Root(nonRepo); got != "" {
		t.Errorf("Root(non-repo) = %q, want empty", got)
	}

	if got := Branch(repo); got == nil || *got != "main" {
		t.Errorf("Branch(repo) = %v, want main", got)
	}
	if got := Branch(nonRepo); got != nil {
		t.Errorf("Branch(non-repo) = %q, want nil", *got)
	}
	if got := CurrentBranch(repo); got != "main" {
		t.Errorf("CurrentBranch(repo) = %q, want main", got)
	}
	if got := CurrentBranch(nonRepo); got != "" {
		t.Errorf("CurrentBranch(non-repo) = %q, want empty", got)
	}

	// detached HEAD: Branch reports "(detached)", CurrentBranch reports ""
	head := testutil.MustGit(t, repo, "rev-parse", "HEAD")
	testutil.MustGit(t, repo, "checkout", "--detach", head)
	if got := Branch(repo); got == nil || *got != "(detached)" {
		t.Errorf("Branch(detached) = %v, want (detached)", got)
	}
	if got := CurrentBranch(repo); got != "" {
		t.Errorf("CurrentBranch(detached) = %q, want empty", got)
	}
}

func TestResolveBranch(t *testing.T) {
	t.Parallel()
	src := testutil.InitRepo(t)
	testutil.MustGit(t, src, "switch", "-c", "feature")
	testutil.CommitFile(t, src, "feat.txt", "feature work", "2026-01-02T00:00:00Z")
	testutil.MustGit(t, src, "switch", "main")

	if got := ResolveBranch(src, "main"); got != "main" {
		t.Errorf("ResolveBranch local = %q, want main", got)
	}
	if got := ResolveBranch(src, "feature"); got != "feature" {
		t.Errorf("ResolveBranch local feature = %q, want feature", got)
	}
	if got := ResolveBranch(src, "nope"); got != "" {
		t.Errorf("ResolveBranch missing = %q, want empty", got)
	}
	if !BranchExists(src, "feature") || BranchExists(src, "nope") {
		t.Error("BranchExists disagrees with ResolveBranch")
	}

	// remote-tracking resolution: a second repo adds src as a remote and fetches
	other := testutil.InitRepo(t)
	testutil.MustGit(t, other, "remote", "add", "peer", src)
	testutil.MustGit(t, other, "fetch", "peer")
	if got := ResolveBranch(other, "feature"); got != "peer/feature" {
		t.Errorf("ResolveBranch remote-only = %q, want peer/feature", got)
	}
	if got := ResolveBranch(other, "main"); got != "main" {
		t.Errorf("local branch must win over remote-tracking, got %q", got)
	}
	if !BranchExists(other, "feature") {
		t.Error("BranchExists should see remote-only branches")
	}
}

func TestDefaultRef(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	if got := DefaultRef(repo); got != "refs/heads/main" {
		t.Errorf("DefaultRef(main repo) = %q, want refs/heads/main", got)
	}

	// a repo that only has master resolves to it
	master := testutil.InitRepo(t)
	testutil.MustGit(t, master, "branch", "-m", "main", "master")
	if got := DefaultRef(master); got != "refs/heads/master" {
		t.Errorf("DefaultRef(master repo) = %q, want refs/heads/master", got)
	}

	// a clone records origin/HEAD — the remote's declared default wins
	parent := testutil.ResolvedTempDir(t)
	testutil.MustGit(t, parent, "clone", repo, "clone")
	if got := DefaultRef(filepath.Join(parent, "clone")); got != "refs/remotes/origin/main" {
		t.Errorf("DefaultRef(clone) = %q, want refs/remotes/origin/main", got)
	}

	// no origin/HEAD, no main, no master: nothing to measure against
	trunk := testutil.InitRepo(t)
	testutil.MustGit(t, trunk, "branch", "-m", "main", "trunk")
	if got := DefaultRef(trunk); got != "" {
		t.Errorf("DefaultRef(trunk-only repo) = %q, want empty", got)
	}
}

func TestSessionBranches(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)

	// merged: parked at main's tip; unmerged: one commit main lacks
	testutil.MustGit(t, repo, "branch", "gc/merged-aaaa")
	testutil.MustGit(t, repo, "switch", "-c", "gc/unmerged-bbbb")
	testutil.CommitFile(t, repo, "orbit.txt", "orbit work", "2026-01-02T00:00:00Z")
	testutil.MustGit(t, repo, "switch", "main")
	// held: checked out in a second worktree
	wt := filepath.Join(testutil.ResolvedTempDir(t), "held")
	testutil.MustGit(t, repo, "worktree", "add", "-b", "gc/held-cccc", wt, "main")
	// non-gc branches never appear
	testutil.MustGit(t, repo, "branch", "feature")

	list, err := SessionBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("SessionBranches = %+v, want 3 gc/ branches", list)
	}
	byName := map[string]SessionBranch{}
	for _, b := range list {
		byName[b.Branch] = b
		if _, err := time.Parse(time.RFC3339, b.LastCommitAt); err != nil {
			t.Errorf("%s lastCommitAt %q is not RFC3339: %v", b.Branch, b.LastCommitAt, err)
		}
	}
	if b := byName["gc/merged-aaaa"]; !b.Merged || b.WorktreePath != "" {
		t.Errorf("gc/merged-aaaa = %+v, want merged and unattached", b)
	}
	if b := byName["gc/unmerged-bbbb"]; b.Merged || b.WorktreePath != "" {
		t.Errorf("gc/unmerged-bbbb = %+v, want unmerged and unattached", b)
	}
	if b := byName["gc/held-cccc"]; !b.Merged || b.WorktreePath != wt {
		t.Errorf("gc/held-cccc = %+v, want merged and held by %q", b, wt)
	}
	// the commit date rides through, not the query time
	if got := byName["gc/unmerged-bbbb"].LastCommitAt; got[:10] != "2026-01-02" {
		t.Errorf("gc/unmerged-bbbb lastCommitAt = %q, want the fixture commit date", got)
	}

	// a repo with no gc/ branches lists empty without error
	empty := testutil.InitRepo(t)
	if got, err := SessionBranches(empty); err != nil || len(got) != 0 {
		t.Errorf("SessionBranches(no gc branches) = %+v, %v", got, err)
	}
	// a non-repo errors so callers can drop it from the scan
	if _, err := SessionBranches(testutil.ResolvedTempDir(t)); err == nil {
		t.Error("SessionBranches outside a repo must error")
	}
}

func TestSessionBranchesMergedVsDefaultNotHead(t *testing.T) {
	t.Parallel()
	// HEAD sits on a feature branch: merged must still be measured against main
	repo := testutil.InitRepo(t)
	testutil.MustGit(t, repo, "switch", "-c", "feature")
	testutil.CommitFile(t, repo, "feat.txt", "feature work", "2026-01-03T00:00:00Z")
	testutil.MustGit(t, repo, "branch", "gc/onfeature-aaaa")      // reachable from HEAD, not from main
	testutil.MustGit(t, repo, "branch", "gc/onmain-bbbb", "main") // reachable from main

	list, err := SessionBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]SessionBranch{}
	for _, b := range list {
		byName[b.Branch] = b
	}
	if b := byName["gc/onfeature-aaaa"]; b.Merged {
		t.Errorf("gc/onfeature-aaaa flagged merged — measured against HEAD instead of the default branch: %+v", b)
	}
	if b := byName["gc/onmain-bbbb"]; !b.Merged {
		t.Errorf("gc/onmain-bbbb = %+v, want merged", b)
	}

	// with no default branch at all, unmerged is the honest answer
	trunk := testutil.InitRepo(t)
	testutil.MustGit(t, trunk, "branch", "-m", "main", "trunk")
	testutil.MustGit(t, trunk, "branch", "gc/orphan-cccc")
	list, err = SessionBranches(trunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Merged {
		t.Errorf("SessionBranches(no default) = %+v, want one unmerged branch", list)
	}
}

func TestDiffStat(t *testing.T) {
	t.Parallel()
	repo := testutil.InitRepo(t)
	testutil.MustGit(t, repo, "switch", "-c", "work")

	// nothing since branching → all zeros
	st, err := DiffStat(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st != (DiffStats{}) {
		t.Errorf("clean branch DiffStat = %+v, want zeros", st)
	}

	// one committed two-line file vs the base
	if err := os.WriteFile(filepath.Join(repo, "feat.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.MustGit(t, repo, "add", ".")
	testutil.MustGit(t, repo, "commit", "-m", "feat")
	st, err = DiffStat(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st != (DiffStats{FilesChanged: 1, Insertions: 2}) {
		t.Errorf("committed change DiffStat = %+v, want 1 file / 2 insertions", st)
	}

	// an untracked file counts as uncommitted but never in the diff stat
	scratch := filepath.Join(repo, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = DiffStat(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st != (DiffStats{FilesChanged: 1, Insertions: 2, Uncommitted: 1}) {
		t.Errorf("untracked file DiffStat = %+v, want uncommitted 1", st)
	}
	if err := os.Remove(scratch); err != nil {
		t.Fatal(err)
	}

	// upstream moves on and the branch merges it in: only the branch's own
	// change counts, because the stat measures from the merge-base
	testutil.MustGit(t, repo, "switch", "main")
	testutil.CommitFile(t, repo, "upstream.txt", "upstream work", "")
	testutil.MustGit(t, repo, "switch", "work")
	testutil.MustGit(t, repo, "merge", "main")
	st, err = DiffStat(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st != (DiffStats{FilesChanged: 1, Insertions: 2}) {
		t.Errorf("post-merge DiffStat = %+v, want only the branch's own change", st)
	}

	// an uncommitted tracked edit shows in both the diff stat and the count
	if err := os.WriteFile(filepath.Join(repo, "readme.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = DiffStat(repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st != (DiffStats{FilesChanged: 2, Insertions: 3, Deletions: 1, Uncommitted: 1}) {
		t.Errorf("dirty tracked edit DiffStat = %+v", st)
	}

	// failures surface as errors so callers can degrade to an absent debrief
	if _, err := DiffStat(testutil.ResolvedTempDir(t), "main"); err == nil {
		t.Error("DiffStat outside a repo must error")
	}
	if _, err := DiffStat(repo, "no-such-base"); err == nil {
		t.Error("DiffStat with an unknown base must error")
	}
}
