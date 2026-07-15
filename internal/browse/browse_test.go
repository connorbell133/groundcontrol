package browse

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/connorbell133/groundcontrol/internal/testutil"
)

func testBrowser(t *testing.T, roots []string, showHidden bool) *Browser {
	t.Helper()
	b := New()
	b.Configure(roots, showHidden)
	return b
}

// buildRootFixture creates root, a prefix-trickster sibling root2, an outside
// dir, a real subdir, and two symlinks: one escaping, one staying inside.
func buildRootFixture(t *testing.T) (root, root2, outside, sub string) {
	t.Helper()
	base := testutil.ResolvedTempDir(t)
	root = filepath.Join(base, "root")
	root2 = filepath.Join(base, "root2")
	outside = filepath.Join(base, "outside")
	sub = filepath.Join(root, "sub")
	for _, d := range []string{root, root2, outside, sub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, filepath.Join(root, "inlink")); err != nil {
		t.Fatal(err)
	}
	return root, root2, outside, sub
}

func TestWithinRoots(t *testing.T) {
	t.Parallel()
	root, root2, outside, sub := buildRootFixture(t)
	b := testBrowser(t, []string{root}, false)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root itself", root, true},
		{"child dir", sub, true},
		{"not-yet-created child (lexical fallback)", filepath.Join(root, "newdir"), true},
		{"outside dir", outside, false},
		{"prefix trickster sibling", root2, false},
		{"symlink escaping the root", filepath.Join(root, "escape"), false},
		{"symlink staying inside", filepath.Join(root, "inlink"), true},
		{"parent of root", filepath.Dir(root), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.WithinRoots(tc.path); got != tc.want {
				t.Errorf("WithinRoots(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestBrowse(t *testing.T) {
	t.Parallel()
	root, _, outside, sub := buildRootFixture(t)
	for _, d := range []string{"beta", "Alpha", ".hid", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := testBrowser(t, []string{root}, false)

	res, err := b.Browse(root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != root {
		t.Errorf("Path = %q, want the resolved root %q", res.Path, root)
	}
	if res.Parent != nil {
		t.Errorf("Parent at a root should be nil, got %q", *res.Parent)
	}
	if res.IsGit || res.RepoRoot != nil {
		t.Errorf("plain dir reported as git: %+v", res)
	}
	var names []string
	for _, f := range res.Folders {
		names = append(names, f.Name)
		if f.Path != filepath.Join(root, f.Name) {
			t.Errorf("entry path %q not under the resolved root", f.Path)
		}
	}
	want := []string{"Alpha", "beta", "inlink", "sub"} // escape filtered, hidden and node_modules skipped
	if len(names) != len(want) {
		t.Fatalf("folders = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("folders = %v, want %v", names, want)
		}
	}

	subRes, err := b.Browse(sub)
	if err != nil {
		t.Fatal(err)
	}
	if subRes.Parent == nil || *subRes.Parent != root {
		t.Errorf("Parent of subdir = %v, want %q", subRes.Parent, root)
	}

	if _, err := b.Browse(outside); err == nil {
		t.Error("browse outside the roots should fail")
	}
	if _, err := b.Browse(filepath.Join(root, "escape")); err == nil {
		t.Error("browsing an escaping symlink should fail")
	}
	if _, err := b.Browse(filepath.Join(root, "file.txt")); err == nil {
		t.Error("browsing a file should fail")
	}
	if _, err := b.Browse(filepath.Join(root, "does-not-exist")); err == nil {
		t.Error("browsing a missing dir should fail")
	}
}

func TestBrowseShowHidden(t *testing.T) {
	t.Parallel()
	root := testutil.ResolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".hid"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := testBrowser(t, []string{root}, true)
	res, err := b.Browse(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Folders) != 1 || res.Folders[0].Name != ".hid" {
		t.Errorf("showHidden should list dotdirs, got %+v", res.Folders)
	}
}
