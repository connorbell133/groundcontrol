package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type Session struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Folder         string  `json:"folder"`
	SpawnMode      string  `json:"spawnMode"` // "same-dir" | "worktree"
	Branch         *string `json:"branch"`
	WorktreePath   *string `json:"worktreePath"`
	PermissionMode string  `json:"permissionMode"`
	State          string  `json:"state"` // "starting" | "ready" | "exited" | "error"
	PairingURL     *string `json:"pairingUrl"`
	CallbackURL    *string `json:"callbackUrl"`
	StartedAt      string  `json:"startedAt"`
	ExitedAt       *string `json:"exitedAt"`
	ExitCode       *int    `json:"exitCode"`
	LastOutputAt   *string `json:"lastOutputAt"`
	LastLine       *string `json:"lastLine"`
}

type liveSession struct {
	Session
	ptmx   *os.File
	cmd    *exec.Cmd
	log    []string
	killed bool
	// worktree cleanup context, captured at launch
	repoRoot   string
	wtBranch   string
	baseCommit string
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*liveSession{}
	// names reserved by launches still between duplicate-check and insertion
	pendingNames = map[string]bool{}
)

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07?`)
	urlRE  = regexp.MustCompile(`https://[^\s"'\x1b]+`)
	// box-drawing, blocks, braille spinners, and ASCII rule/spinner chars — lines of only these are visual noise
	junkRE      = regexp.MustCompile(`[─-╿▀-▟⠀-⣿|\\/·•●◐◓◑◒~\-_=+*.\s]`)
	trailingRE  = regexp.MustCompile(`[).,]+$`)
	lineSplitRE = regexp.MustCompile(`[\r\n]+`)
	alnumRE     = regexp.MustCompile(`[A-Za-z0-9]`)
)

func lastMeaningfulLine(log []string) string {
	start := len(log) - 8
	if start < 0 {
		start = 0
	}
	tail := strings.Join(log[start:], "")
	lines := lineSplitRE.Split(tail, -1)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && alnumRE.MatchString(junkRE.ReplaceAllString(line, "")) {
			r := []rune(line)
			if len(r) > 120 {
				r = r[:120]
			}
			return strings.TrimSpace(string(r))
		}
	}
	return ""
}

// lifecycle fan-out: in-process bus (SSE, wait=ready), configured webhook
// subscribers, and the per-launch callbackUrl — one payload shape for all
func announce(event string, s Session, killed bool) {
	where := filepath.Base(s.Folder)
	if s.Branch != nil {
		where += " @ " + *s.Branch
	}
	failed := event == "session.exit" && !killed && (s.ExitCode == nil || *s.ExitCode != 0 || s.State == "error")
	pairing := "null"
	if s.PairingURL != nil {
		pairing = *s.PairingURL
	}
	code := "null"
	if s.ExitCode != nil {
		code = strconv.Itoa(*s.ExitCode)
	}
	exitTitle := "session exited: " + s.Name
	exitMessage := where + " exited cleanly"
	if failed {
		exitTitle = "session failed: " + s.Name
		suffix := ""
		if s.State == "error" {
			suffix = " (died before ready)"
		}
		exitMessage = fmt.Sprintf("%s exited with code %s%s", where, code, suffix)
	}
	titles := map[string]string{
		"session.start": "session started: " + s.Name,
		"session.ready": "session ready: " + s.Name,
		"session.kill":  "session killed: " + s.Name,
		"session.exit":  exitTitle,
	}
	messages := map[string]string{
		"session.start": where,
		"session.ready": where + " — " + pairing,
		"session.kill":  where,
		"session.exit":  exitMessage,
	}
	title, ok := titles[event]
	if !ok {
		title = event
	}
	message := messages[event]
	alsoMatch := []string{}
	if failed {
		alsoMatch = []string{"session.failed"}
	}
	emit(event, map[string]any{"session": s}, emitOpts{title: title, message: message, alsoMatch: alsoMatch})
	if s.CallbackURL != nil && event != "session.start" {
		deliverWebhook(*s.CallbackURL, LifecycleEvent{
			Event:   event,
			At:      nowISO(),
			Title:   title,
			Message: message,
			Data:    map[string]any{"session": s},
		})
	}
}

/* ---------- journal queries ---------- */

type RecentLaunch struct {
	Folder         string  `json:"folder"`
	Name           string  `json:"name"`
	Branch         *string `json:"branch"`
	SpawnMode      string  `json:"spawnMode"`
	PermissionMode string  `json:"permissionMode"`
	At             string  `json:"at"`
	Stale          bool    `json:"stale"` // launch config whose branch no longer exists
}

type LostSession struct {
	ID           string `json:"id"`
	RecentLaunch        // embedded — flattens in JSON like the TS interface extension
}

const lostWindow = 7 * 24 * time.Hour

func jStr(e map[string]any, key string) string {
	if v, ok := e[key].(string); ok {
		return v
	}
	return ""
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func recentLaunches(limit int) []RecentLaunch {
	seen := map[string]bool{}
	out := []RecentLaunch{}
	entries := readJournal()
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		folder := jStr(e, "folder")
		if jStr(e, "event") != "session.start" || folder == "" {
			continue
		}
		if !pathExists(folder) || !withinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		rawSpawnMode := jStr(e, "spawnMode")
		key := folder + "\x00" + branch + "\x00" + rawSpawnMode
		if seen[key] {
			continue
		}
		seen[key] = true
		spawnMode := rawSpawnMode
		if spawnMode == "" {
			spawnMode = "same-dir"
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		out = append(out, RecentLaunch{
			Folder:         folder,
			Name:           jStr(e, "name"),
			Branch:         strPtr(branch),
			SpawnMode:      spawnMode,
			PermissionMode: permissionMode,
			At:             jStr(e, "at"),
			Stale:          branch != "" && !branchExists(folder, branch),
		})
	}
	return out
}

var (
	lostMu       sync.Mutex
	lostCache    []LostSession
	lostComputed bool
)

func listLostSessions() []LostSession {
	// snapshot live ids before taking lostMu — never hold both locks
	liveIDs := map[string]bool{}
	sessionsMu.Lock()
	for id := range sessions {
		liveIDs[id] = true
	}
	sessionsMu.Unlock()

	lostMu.Lock()
	defer lostMu.Unlock()
	if lostComputed {
		// copy: a dismissal splicing lostCache must not race a caller still marshaling
		return append([]LostSession(nil), lostCache...)
	}
	entries := readJournal()
	terminated := map[string]bool{}
	for _, e := range entries {
		ev := jStr(e, "event")
		if (ev == "session.exit" || ev == "session.kill") && jStr(e, "id") != "" {
			terminated[jStr(e, "id")] = true
		}
	}
	cutoff := time.Now().Add(-lostWindow)
	out := []LostSession{}
	for _, e := range entries {
		id := jStr(e, "id")
		folder := jStr(e, "folder")
		if jStr(e, "event") != "session.start" || id == "" || folder == "" {
			continue
		}
		if terminated[id] || liveIDs[id] {
			continue
		}
		at := jStr(e, "at")
		if at == "" {
			continue
		}
		// unparsable timestamps pass through, like NaN < cutoff in JS
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.Before(cutoff) {
			continue
		}
		if !pathExists(folder) || !withinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		spawnMode := jStr(e, "spawnMode")
		if spawnMode == "" {
			spawnMode = "same-dir"
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		out = append(out, LostSession{
			ID: id,
			RecentLaunch: RecentLaunch{
				Folder:         folder,
				Name:           jStr(e, "name"),
				Branch:         strPtr(branch),
				SpawnMode:      spawnMode,
				PermissionMode: permissionMode,
				At:             at,
				Stale:          branch != "" && !branchExists(folder, branch),
			},
		})
	}
	lostCache = out
	lostComputed = true
	return append([]LostSession(nil), lostCache...)
}

/* ---------- kept worktrees (dirty orphans the sweeps refused to delete) ---------- */

type KeptWorktree struct {
	Path   string  `json:"path"`
	Repo   string  `json:"repo"`
	ID     string  `json:"id"`
	Branch *string `json:"branch"`
	Dirty  bool    `json:"dirty"`
}

func listKeptWorktrees() []KeptWorktree {
	out := []KeptWorktree{}
	if !pathExists(wtBase) {
		return out
	}
	live := map[string]bool{}
	sessionsMu.Lock()
	for _, s := range sessions {
		if s.WorktreePath != nil && *s.WorktreePath != "" {
			live[*s.WorktreePath] = true
		}
	}
	sessionsMu.Unlock()
	repos, _ := os.ReadDir(wtBase)
	for _, repo := range repos {
		ids, err := os.ReadDir(filepath.Join(wtBase, repo.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			wtPath := filepath.Join(wtBase, repo.Name(), id.Name())
			if live[wtPath] {
				continue // belongs to a running session
			}
			branch := currentBranch(wtPath)
			dirty := false
			if status, err := gitOut(wtPath, 5*time.Second, "status", "--porcelain"); err == nil {
				dirty = len(status) > 0
			}
			// unreadable — still listed so it can be cleaned
			out = append(out, KeptWorktree{Path: wtPath, Repo: repo.Name(), ID: id.Name(), Branch: strPtr(branch), Dirty: dirty})
		}
	}
	return out
}

func forceRemoveWorktree(wtPath string) error {
	resolved := filepath.Clean(wtPath) // normalize
	if !strings.HasPrefix(resolved, wtBase+"/") {
		return errors.New("not a runner-managed worktree")
	}
	commonDir, err := gitOut(resolved, 5*time.Second, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	mainRoot := commonDir
	if strings.HasSuffix(commonDir, "/.git") {
		mainRoot = commonDir[:len(commonDir)-len("/.git")]
	}
	wtBranch := currentBranch(resolved)
	if err := gitRun(mainRoot, 15*time.Second, "worktree", "remove", "--force", resolved); err != nil {
		return err
	}
	if err := gitRun(mainRoot, 15*time.Second, "worktree", "prune"); err != nil {
		return err
	}
	// safe delete only: commits on the session branch stay reachable after a force-clean
	if strings.HasPrefix(wtBranch, "gc/") {
		// unmerged work — keep the branch
		_ = gitRun(mainRoot, 5*time.Second, "branch", "-d", wtBranch)
	}
	journal(map[string]any{"event": "worktree.force-removed", "wtPath": resolved})
	return nil
}

func listSessions() []Session {
	sessionsMu.Lock()
	out := make([]Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Session)
	}
	sessionsMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func getSession(id string) *Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessions[id]; ok {
		snap := s.Session
		return &snap
	}
	return nil
}

func getSessionLog(id string) *string {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessions[id]; ok {
		joined := strings.Join(s.log, "")
		return &joined
	}
	return nil
}

// Boot sweep: sessions are in-memory, so any worktree on disk at startup is an
// orphan from a previous runner. Remove the clean ones; keep dirty ones and journal.
func sweepOrphanWorktrees() {
	if !pathExists(wtBase) {
		return
	}
	repos, _ := os.ReadDir(wtBase)
	for _, repo := range repos {
		repoDir := filepath.Join(wtBase, repo.Name())
		ids, err := os.ReadDir(repoDir)
		if err != nil {
			continue
		}
		for _, wt := range ids {
			wtPath := filepath.Join(repoDir, wt.Name())
			swept := func() bool {
				commonDir, err := gitOut(wtPath, 5*time.Second, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if err != nil {
					return false
				}
				mainRoot := commonDir
				if strings.HasSuffix(commonDir, "/.git") {
					mainRoot = commonDir[:len(commonDir)-len("/.git")]
				}
				wtBranch := currentBranch(wtPath)
				if err := gitRun(mainRoot, 15*time.Second, "worktree", "remove", wtPath); err != nil {
					return false
				}
				// -d not -D: an orphan's base commit is unknown, so only drop the session
				// branch when git agrees it holds nothing unmerged
				if strings.HasPrefix(wtBranch, "gc/") {
					// unmerged work — the branch keeps it reachable
					_ = gitRun(mainRoot, 5*time.Second, "branch", "-d", wtBranch)
				}
				return true
			}()
			if swept {
				journal(map[string]any{"event": "worktree.swept", "wtPath": wtPath})
			} else {
				journal(map[string]any{"event": "worktree.kept", "wtPath": wtPath, "reason": "orphan is dirty or unresolvable"})
			}
		}
	}
}

type createSessionOpts struct {
	folder, name, spawnMode, branch, permissionMode, callbackURL, actor string
}

func createSession(opts createSessionOpts) (Session, error) {
	name := strings.TrimSpace(opts.name)
	if name == "" {
		name = filepath.Base(opts.folder) + "-" + randomID(4)
	}
	sessionsMu.Lock()
	duplicate := pendingNames[name]
	if !duplicate {
		for _, s := range sessions {
			if s.Name == name && s.State != "exited" && s.State != "error" {
				duplicate = true
				break
			}
		}
	}
	if !duplicate {
		// reserve the name across the slow worktree/switch/spawn work below, so two
		// concurrent launches can't both pass the duplicate check
		pendingNames[name] = true
	}
	sessionsMu.Unlock()
	if duplicate {
		return Session{}, fmt.Errorf("a live session named \"%s\" already exists", name)
	}
	defer func() {
		sessionsMu.Lock()
		delete(pendingNames, name)
		sessionsMu.Unlock()
	}()

	spawnMode := "same-dir"
	if opts.spawnMode == "worktree" {
		spawnMode = "worktree"
	}
	permissionMode := opts.permissionMode
	if permissionMode == "" {
		permissionMode = "default"
	}
	id := randomID(8)

	worktreePath := ""
	branch := ""
	wtBranch := ""
	baseCommit := ""
	cwd := opts.folder
	repoRootForCleanup := ""
	if spawnMode == "worktree" {
		if opts.branch == "" {
			return Session{}, errors.New("worktree mode requires a branch")
		}
		root := gitRoot(opts.folder)
		if root == "" {
			return Session{}, errors.New("folder is not inside a git repository")
		}
		branch = opts.branch
		repoRootForCleanup = root
		wt, err := addWorktree(root, branch, id) // worktree of the nearest repo root, cleaned up on exit
		if err != nil {
			return Session{}, err
		}
		worktreePath = wt.wtPath
		wtBranch = wt.wtBranch
		baseCommit = wt.baseCommit
		// land in the equivalent subfolder inside the worktree when it exists on that branch
		rel, relErr := filepath.Rel(root, opts.folder)
		cwd = worktreePath
		if relErr == nil && rel != "" && rel != "." {
			sub := filepath.Join(worktreePath, rel)
			if pathExists(sub) {
				cwd = sub
			}
		}
	} else if opts.branch != "" {
		// in-folder launch on a chosen branch: switch the checkout first — git refuses if
		// that would clobber local changes, and DWIMs a local branch for remote-only picks
		root := gitRoot(opts.folder)
		if root != "" {
			if currentBranch(root) != opts.branch {
				if err := gitRun(root, 15*time.Second, "switch", opts.branch); err != nil {
					msg := strings.TrimSpace(err.Error())
					// no stderr to surface — fall back to a generic message, like the TS
					if msg == "" || strings.HasPrefix(msg, "exit status") || strings.HasPrefix(msg, "signal:") {
						msg = "could not switch to branch " + opts.branch
					}
					return Session{}, errors.New(msg)
				}
			}
			branch = opts.branch
		}
	}

	args := []string{"remote-control", "--name", name, "--spawn", "same-dir", "--permission-mode", permissionMode}
	// real PTY: the CLI prints its pairing URL and stays alive as it would in a terminal
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1", "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if worktreePath != "" {
			removeWorktree(repoRootForCleanup, worktreePath, wtBranch, baseCommit)
		}
		return Session{}, err
	}

	s := &liveSession{
		Session: Session{
			ID:             id,
			Name:           name,
			Folder:         opts.folder,
			SpawnMode:      spawnMode,
			Branch:         strPtr(branch),
			WorktreePath:   strPtr(worktreePath),
			PermissionMode: permissionMode,
			State:          "starting",
			PairingURL:     nil,
			CallbackURL:    strPtr(opts.callbackURL),
			StartedAt:      nowISO(),
			ExitedAt:       nil,
			ExitCode:       nil,
			LastOutputAt:   nil,
			LastLine:       nil,
		},
		ptmx:       ptmx,
		cmd:        cmd,
		repoRoot:   repoRootForCleanup,
		wtBranch:   wtBranch,
		baseCommit: baseCommit,
	}
	sessionsMu.Lock()
	sessions[id] = s
	snap := s.Session
	sessionsMu.Unlock()

	entry := map[string]any{
		"event":          "session.start",
		"id":             id,
		"name":           name,
		"folder":         opts.folder,
		"spawnMode":      spawnMode,
		"branch":         strPtr(branch),
		"permissionMode": permissionMode,
	}
	if opts.actor != "" {
		entry["actor"] = opts.actor
	}
	journal(entry)
	announce("session.start", snap, false)

	go readLoop(s)

	return snap, nil
}

func readLoop(s *liveSession) {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			var readySnap *Session
			var readyURL string
			sessionsMu.Lock()
			s.LastOutputAt = strPtr(nowISO())
			text := ansiRE.ReplaceAllString(chunk, "")
			s.log = append(s.log, text)
			if len(s.log) > 400 {
				s.log = s.log[len(s.log)-400:]
			}
			if line := lastMeaningfulLine(s.log); line != "" {
				s.LastLine = strPtr(line)
			}
			if s.PairingURL == nil {
				if m := urlRE.FindString(strings.Join(s.log, "")); m != "" {
					url := trailingRE.ReplaceAllString(m, "")
					s.PairingURL = &url
					s.State = "ready"
					readyURL = url
					snap := s.Session
					readySnap = &snap
				}
			}
			id := s.ID
			sessionsMu.Unlock()
			if readySnap != nil {
				journal(map[string]any{"event": "session.ready", "id": id, "pairingUrl": readyURL})
				announce("session.ready", *readySnap, false)
			}
		}
		if err != nil {
			break
		}
	}

	// EOF / PTY closed: the CLI exited. Reap it and run the exit lifecycle.
	s.ptmx.Close()
	waitErr := s.cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		exitCode = 1
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
			if exitCode < 0 {
				exitCode = 1 // killed by signal — report failure, not a raw -1
			}
		}
	}

	sessionsMu.Lock()
	if s.State == "starting" {
		s.State = "error"
	} else {
		s.State = "exited"
	}
	s.ExitedAt = strPtr(nowISO())
	s.ExitCode = intPtr(exitCode)
	s.ptmx = nil
	id := s.ID
	killed := s.killed
	wtPath := ""
	if s.WorktreePath != nil {
		wtPath = *s.WorktreePath
	}
	cleanupRoot := s.repoRoot
	if cleanupRoot == "" {
		cleanupRoot = s.Folder
	}
	wtBranch, baseCommit := s.wtBranch, s.baseCommit
	snap := s.Session
	sessionsMu.Unlock()

	if wtPath != "" {
		removeWorktree(cleanupRoot, wtPath, wtBranch, baseCommit)
	}
	journal(map[string]any{"event": "session.exit", "id": id, "code": exitCode})
	announce("session.exit", snap, killed)
}

func killSession(id, actor string) *Session {
	sessionsMu.Lock()
	s, ok := sessions[id]
	if !ok {
		sessionsMu.Unlock()
		return nil
	}
	s.killed = true // set before the kill so onExit never notifies for user-initiated stops
	if s.ptmx != nil && s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM) // pty close also HUPs the whole session tree; already gone is fine
	}
	snap := s.Session
	sessionsMu.Unlock()

	entry := map[string]any{"event": "session.kill", "id": id}
	if actor != "" {
		entry["actor"] = actor
	}
	journal(entry)
	announce("session.kill", snap, true)
	return &snap
}

func removeSession(id string) (bool, error) {
	sessionsMu.Lock()
	s, ok := sessions[id]
	if !ok {
		sessionsMu.Unlock()
		// lost-session headstones dismiss through the same endpoint
		lostMu.Lock()
		defer lostMu.Unlock()
		for i, l := range lostCache {
			if l.ID == id {
				lostCache = append(lostCache[:i], lostCache[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	}
	defer sessionsMu.Unlock()
	if s.State != "exited" && s.State != "error" {
		return false, errors.New("session is still live; kill it first")
	}
	delete(sessions, id)
	return true, nil
}
