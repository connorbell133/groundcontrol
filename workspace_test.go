package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitHelpers(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	nonRepo := resolvedTempDir(t)

	if got := gitRoot(repo); got != repo {
		t.Errorf("gitRoot(repo) = %q, want %q", got, repo)
	}
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitRoot(sub); got != repo {
		t.Errorf("gitRoot(subdir) = %q, want %q", got, repo)
	}
	if got := gitRoot(nonRepo); got != "" {
		t.Errorf("gitRoot(non-repo) = %q, want empty", got)
	}

	if got := gitBranch(repo); got == nil || *got != "main" {
		t.Errorf("gitBranch(repo) = %v, want main", got)
	}
	if got := gitBranch(nonRepo); got != nil {
		t.Errorf("gitBranch(non-repo) = %q, want nil", *got)
	}
	if got := currentBranch(repo); got != "main" {
		t.Errorf("currentBranch(repo) = %q, want main", got)
	}
	if got := currentBranch(nonRepo); got != "" {
		t.Errorf("currentBranch(non-repo) = %q, want empty", got)
	}

	// detached HEAD: gitBranch reports "(detached)", currentBranch reports ""
	head := mustGit(t, repo, "rev-parse", "HEAD")
	mustGit(t, repo, "checkout", "--detach", head)
	if got := gitBranch(repo); got == nil || *got != "(detached)" {
		t.Errorf("gitBranch(detached) = %v, want (detached)", got)
	}
	if got := currentBranch(repo); got != "" {
		t.Errorf("currentBranch(detached) = %q, want empty", got)
	}
}

func TestResolveBranch(t *testing.T) {
	t.Parallel()
	src := initRepo(t)
	mustGit(t, src, "switch", "-c", "feature")
	commitFile(t, src, "feat.txt", "feature work", "2026-01-02T00:00:00Z")
	mustGit(t, src, "switch", "main")

	if got := resolveBranch(src, "main"); got != "main" {
		t.Errorf("resolveBranch local = %q, want main", got)
	}
	if got := resolveBranch(src, "feature"); got != "feature" {
		t.Errorf("resolveBranch local feature = %q, want feature", got)
	}
	if got := resolveBranch(src, "nope"); got != "" {
		t.Errorf("resolveBranch missing = %q, want empty", got)
	}
	if !branchExists(src, "feature") || branchExists(src, "nope") {
		t.Error("branchExists disagrees with resolveBranch")
	}

	// remote-tracking resolution: a second repo adds src as a remote and fetches
	other := initRepo(t)
	mustGit(t, other, "remote", "add", "peer", src)
	mustGit(t, other, "fetch", "peer")
	if got := resolveBranch(other, "feature"); got != "peer/feature" {
		t.Errorf("resolveBranch remote-only = %q, want peer/feature", got)
	}
	if got := resolveBranch(other, "main"); got != "main" {
		t.Errorf("local branch must win over remote-tracking, got %q", got)
	}
	if !branchExists(other, "feature") {
		t.Error("branchExists should see remote-only branches")
	}
}

func TestBranchList(t *testing.T) {
	t.Parallel()
	src := initRepo(t)
	mustGit(t, src, "switch", "-c", "feature")
	commitFile(t, src, "feat.txt", "feature work", "2026-01-02T00:00:00Z")
	mustGit(t, src, "switch", "main")

	clone := initRepo(t)
	mustGit(t, clone, "remote", "add", "origin", src)
	mustGit(t, clone, "fetch", "origin")
	mustGit(t, clone, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	a := testApp(t, Config{Roots: []string{src, clone}})

	// src has distinct committer dates, so the -committerdate order is exact
	names, err := a.branchList(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "feature" || names[1] != "main" {
		t.Errorf("branchList(src) = %v, want [feature main]", names)
	}

	// clone: local main + remote-tracking refs; HEAD skipped, main deduped
	names, err = a.branchList(clone)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	if seen["main"] != 1 || seen["feature"] != 1 {
		t.Errorf("branchList(clone) = %v, want main and feature exactly once", names)
	}
	if seen["HEAD"] != 0 {
		t.Errorf("branchList(clone) leaked HEAD: %v", names)
	}

	if _, err := a.branchList(resolvedTempDir(t)); err == nil {
		t.Error("branchList outside the roots should fail")
	}

	// inside roots but not a repo: empty list, no error
	bare := resolvedTempDir(t)
	b := testApp(t, Config{Roots: []string{bare}})
	list, err := b.branchList(bare)
	if err != nil {
		t.Fatalf("branchList(non-repo) err = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("branchList(non-repo) = %v, want empty", list)
	}
}
