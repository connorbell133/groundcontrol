package gitx

import (
	"os"
	"path/filepath"
	"testing"

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
