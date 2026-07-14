package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
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

var browserMu sync.RWMutex
var browserCfg struct {
	roots      []string
	showHidden bool
}

func configureBrowser(roots []string, showHidden bool) {
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			a = r
		}
		abs = append(abs, a)
	}
	browserMu.Lock()
	browserCfg.roots = abs
	browserCfg.showHidden = showHidden
	browserMu.Unlock()
}

func withinRoots(path string) bool {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	browserMu.RLock()
	defer browserMu.RUnlock()
	for _, root := range browserCfg.roots {
		if resolved == root || strings.HasPrefix(resolved, root+"/") {
			return true
		}
	}
	return false
}

// gitRoot returns the toplevel of the repo containing path, or "" when not a repo.
func gitRoot(path string) string {
	out, err := gitOut(path, 2*time.Second, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// gitBranch: "(detached)" when git succeeds but reports no current branch,
// nil when git fails (not a repo, timeout, ...).
func gitBranch(path string) *string {
	out, err := gitOut(path, 2*time.Second, "branch", "--show-current")
	if err != nil {
		return nil
	}
	if out == "" {
		out = "(detached)"
	}
	return &out
}

func isGitFolder(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func listRoots() []FolderEntry {
	browserMu.RLock()
	roots := append([]string(nil), browserCfg.roots...)
	browserMu.RUnlock()
	entries := []FolderEntry{}
	for _, root := range roots {
		git := isGitFolder(root)
		var branch *string
		if git {
			branch = gitBranch(root)
		}
		entries = append(entries, FolderEntry{Name: root, Path: root, IsGit: git, Branch: branch})
	}
	return entries
}

func browse(path string) (BrowseResult, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return BrowseResult{}, err
	}
	if !withinRoots(resolved) {
		return BrowseResult{}, errors.New("path outside configured roots")
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return BrowseResult{}, err
	}
	if !st.IsDir() {
		return BrowseResult{}, errors.New("not a directory")
	}

	browserMu.RLock()
	showHidden := browserCfg.showHidden
	atRoot := false
	for _, root := range browserCfg.roots {
		if root == resolved {
			atRoot = true
		}
	}
	browserMu.RUnlock()

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
		// symlinks must still resolve inside the roots
		if isSymlink && !withinRoots(full) {
			continue
		}
		git := isGitFolder(full)
		var branch *string
		if git {
			branch = gitBranch(full)
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
	repoRoot := gitRoot(resolved)
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
		branch = gitBranch(resolved)
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

// branchList is the TS `branches()` — renamed to avoid clashing with git terms.
func branchList(path string) ([]string, error) {
	if !withinRoots(path) {
		return nil, errors.New("path outside configured roots")
	}
	out, err := gitOut(path, 3*time.Second, "for-each-ref", "--format=%(refname)", "--sort=-committerdate", "refs/heads/", "refs/remotes/")
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
