package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/testutil"
)

func TestManages(t *testing.T) {
	t.Parallel()
	tmp := testutil.ResolvedTempDir(t)
	base := filepath.Join(tmp, "worktrees")
	m := New(base, journal.New(t.TempDir()))

	inside := filepath.Join(base, "repo", "fix-race-a1b2c3d4")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	if !m.Manages(inside) {
		t.Errorf("Manages(%q) = false, want true", inside)
	}
	if m.Manages(base) {
		t.Error("the base itself is a container, not a launchable worktree")
	}
	if m.Manages(outside) {
		t.Errorf("Manages(%q) = true for a sibling of the base", outside)
	}
	if m.Manages(filepath.Join(base, "repo", "gone")) {
		t.Error("a nonexistent path must not count as managed")
	}
	file := filepath.Join(base, "repo", "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.Manages(file) {
		t.Error("a plain file must not count as managed")
	}

	// a symlink inside the base pointing outside must not smuggle its target in
	link := filepath.Join(base, "repo", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if m.Manages(link) {
		t.Error("a symlink escaping the base must not count as managed")
	}
}
