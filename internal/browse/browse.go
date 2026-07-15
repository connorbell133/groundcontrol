// Package browse is the folder browser: configured roots, containment checks,
// directory listings with git context, and the branch picker's branch list.
package browse

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/connorbell133/groundcontrol/internal/gitx"
)

type FolderEntry struct {
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	IsGit  bool    `json:"isGit"`
	Branch *string `json:"branch"`
}

type BrowseResult struct {
	Path     string        `json:"path"`
	Parent   *string       `json:"parent"`
	IsGit    bool          `json:"isGit"`
	RepoRoot *string       `json:"repoRoot"`
	RepoName *string       `json:"repoName"`
	Branch   *string       `json:"branch"`
	Folders  []FolderEntry `json:"folders"`
}

type Browser struct {
	mu         sync.RWMutex
	roots      []string
	showHidden bool
}

func New() *Browser {
	return &Browser{}
}

func (b *Browser) Configure(roots []string, showHidden bool) {
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		// roots are stored symlink-resolved so WithinRoots compares like with like:
		// e.g. /tmp on macOS is a symlink to /private/tmp, and resolved candidates
		// land on the /private form. Fall back to Abs when the root doesn't exist yet.
		resolved, err := filepath.EvalSymlinks(r)
		if err != nil {
			resolved, err = filepath.Abs(r)
			if err != nil {
				resolved = r
			}
		}
		if _, err := os.ReadDir(resolved); err != nil {
			if errors.Is(err, os.ErrPermission) {
				log.Printf("root %s is not readable: permission denied — the OS is blocking this process (on macOS, folders like Desktop/Documents need Full Disk Access in System Settings > Privacy & Security)", resolved)
			} else {
				log.Printf("root %s is not readable: %v", resolved, err)
			}
		}
		abs = append(abs, resolved)
	}
	b.mu.Lock()
	b.roots = abs
	b.showHidden = showHidden
	b.mu.Unlock()
}

func (b *Browser) WithinRoots(path string) bool {
	// containment is decided on the symlink-RESOLVED path — a lexical prefix check
	// alone lets a symlink inside a root (e.g. root/link -> /etc) escape, since its
	// Abs path sits "inside" the root while its target can be anywhere on disk.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// path doesn't exist (or is unresolvable): fall back to the lexical Abs
		// form. Limitation: a not-yet-created path under a symlinked subdir can't
		// be checked against its real target here; it's re-checked once it exists.
		resolved, err = filepath.Abs(path)
		if err != nil {
			return false
		}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, root := range b.roots {
		if resolved == root || strings.HasPrefix(resolved, root+"/") {
			return true
		}
	}
	return false
}

func (b *Browser) ListRoots() []FolderEntry {
	b.mu.RLock()
	roots := append([]string(nil), b.roots...)
	b.mu.RUnlock()
	entries := []FolderEntry{}
	for _, root := range roots {
		git := gitx.IsGitFolder(root)
		var branch *string
		if git {
			branch = gitx.Branch(root)
		}
		entries = append(entries, FolderEntry{Name: root, Path: root, IsGit: git, Branch: branch})
	}
	return entries
}

func (b *Browser) Browse(path string) (BrowseResult, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return BrowseResult{}, err
	}
	// resolve symlinks so the atRoot comparison (and returned paths) line up with
	// the symlink-resolved roots stored by Configure — e.g. browsing /tmp
	// must count as being at the /private/tmp root on macOS. A failure means the
	// path doesn't exist; keep the Abs form and let os.Stat report the error.
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	if !b.WithinRoots(resolved) {
		return BrowseResult{}, errors.New("path outside configured roots")
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return BrowseResult{}, err
	}
	if !st.IsDir() {
		return BrowseResult{}, errors.New("not a directory")
	}

	b.mu.RLock()
	showHidden := b.showHidden
	atRoot := false
	for _, root := range b.roots {
		if root == resolved {
			atRoot = true
		}
	}
	b.mu.RUnlock()

	dirEntries, err := os.ReadDir(resolved)
	if err != nil {
		return BrowseResult{}, err
	}

	folders := []FolderEntry{}
	for _, entry := range dirEntries {
		isSymlink := entry.Type()&os.ModeSymlink != 0
		if !entry.IsDir() && !isSymlink {
			continue
		}
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.Name() == "node_modules" {
			continue
		}
		full := filepath.Join(resolved, entry.Name())
		fi, err := os.Stat(full)
		if err != nil {
			continue // unreadable entry: skip
		}
		if !fi.IsDir() {
			continue
		}
		// symlinks must still resolve inside the roots — WithinRoots resolves the
		// link target, so root/link -> /etc is filtered even though `full` is
		// lexically inside the root
		if isSymlink && !b.WithinRoots(full) {
			continue
		}
		git := gitx.IsGitFolder(full)
		var branch *string
		if git {
			branch = gitx.Branch(full)
		}
		folders = append(folders, FolderEntry{Name: entry.Name(), Path: full, IsGit: git, Branch: branch})
	}
	sort.Slice(folders, func(i, j int) bool {
		a, b := folders[i], folders[j]
		if a.IsGit != b.IsGit {
			return a.IsGit
		}
		// case-insensitive like localeCompare — "apple" sorts before "Banana"
		al, bl := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if al != bl {
			return al < bl
		}
		return a.Name < b.Name
	})

	// repo context comes from the nearest parent with .git, not just this folder
	repoRoot := gitx.Root(resolved)
	var parent, repoRootPtr, repoName, branch *string
	if !atRoot {
		d := filepath.Dir(resolved)
		parent = &d
	}
	if repoRoot != "" {
		repoRootPtr = &repoRoot
		parts := strings.Split(repoRoot, "/")
		name := parts[len(parts)-1]
		repoName = &name
		branch = gitx.Branch(resolved)
	}
	return BrowseResult{
		Path:     resolved,
		Parent:   parent,
		IsGit:    repoRoot != "",
		RepoRoot: repoRootPtr,
		RepoName: repoName,
		Branch:   branch,
		Folders:  folders,
	}, nil
}

var remoteRefRe = regexp.MustCompile(`^refs/remotes/[^/]+/`)

// BranchList is the TS `branches()` — renamed to avoid clashing with git terms.
func (b *Browser) BranchList(path string) ([]string, error) {
	if !b.WithinRoots(path) {
		return nil, errors.New("path outside configured roots")
	}
	out, err := gitx.Out(path, 3*time.Second, "for-each-ref", "--format=%(refname)", "--sort=-committerdate", "refs/heads/", "refs/remotes/")
	if err != nil {
		return []string{}, nil
	}
	// remote-tracking refs collapse to plain branch names so a fresh clone (one local
	// branch) still offers everything on origin; first occurrence wins the dedup
	seen := map[string]bool{}
	names := []string{}
	for _, ref := range strings.Split(out, "\n") {
		if ref == "" {
			continue
		}
		var name string
		if strings.HasPrefix(ref, "refs/heads/") {
			name = ref[len("refs/heads/"):]
		} else {
			name = remoteRefRe.ReplaceAllString(ref, "")
		}
		if name == "HEAD" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}
