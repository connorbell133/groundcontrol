package browse

import (
	"testing"

	"github.com/connorbell133/groundcontrol/internal/testutil"
)

func TestBranchList(t *testing.T) {
	t.Parallel()
	src := testutil.InitRepo(t)
	testutil.MustGit(t, src, "switch", "-c", "feature")
	testutil.CommitFile(t, src, "feat.txt", "feature work", "2026-01-02T00:00:00Z")
	testutil.MustGit(t, src, "switch", "main")

	clone := testutil.InitRepo(t)
	testutil.MustGit(t, clone, "remote", "add", "origin", src)
	testutil.MustGit(t, clone, "fetch", "origin")
	testutil.MustGit(t, clone, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	br := testBrowser(t, []string{src, clone}, false)

	// src has distinct committer dates, so the -committerdate order is exact
	names, err := br.BranchList(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "feature" || names[1] != "main" {
		t.Errorf("BranchList(src) = %v, want [feature main]", names)
	}

	// clone: local main + remote-tracking refs; HEAD skipped, main deduped
	names, err = br.BranchList(clone)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	if seen["main"] != 1 || seen["feature"] != 1 {
		t.Errorf("BranchList(clone) = %v, want main and feature exactly once", names)
	}
	if seen["HEAD"] != 0 {
		t.Errorf("BranchList(clone) leaked HEAD: %v", names)
	}

	if _, err := br.BranchList(testutil.ResolvedTempDir(t)); err == nil {
		t.Error("BranchList outside the roots should fail")
	}

	// inside roots but not a repo: empty list, no error
	bare := testutil.ResolvedTempDir(t)
	br2 := testBrowser(t, []string{bare}, false)
	list, err := br2.BranchList(bare)
	if err != nil {
		t.Fatalf("BranchList(non-repo) err = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("BranchList(non-repo) = %v, want empty", list)
	}
}
