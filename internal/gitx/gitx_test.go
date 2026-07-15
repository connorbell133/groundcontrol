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
