package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// sessionState and spawnMode keep string underneath: JSON marshals identically,
// and the untyped literals other files compare against still compile
type sessionState string

const (
	stateStarting sessionState = "starting"
	stateReady    sessionState = "ready"
	stateExited   sessionState = "exited"
	stateError    sessionState = "error"
)

type spawnMode string

const (
	spawnSameDir  spawnMode = "same-dir"
	spawnWorktree spawnMode = "worktree"
)

// session lifecycle event names; evSessionFailed is a derived match token
// (announce adds it alongside a failed exit), never an emitted event itself
const (
	evSessionStart  = "session.start"
	evSessionReady  = "session.ready"
	evSessionKill   = "session.kill"
	evSessionExit   = "session.exit"
	evSessionFailed = "session.failed"
)

type Session struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Folder         string       `json:"folder"`
	SpawnMode      spawnMode    `json:"spawnMode"`
	Branch         *string      `json:"branch"`
	WorktreePath   *string      `json:"worktreePath"`
	PermissionMode string       `json:"permissionMode"`
	State          sessionState `json:"state"`
	PairingURL     *string      `json:"pairingUrl"`
	CallbackURL    *string      `json:"callbackUrl"`
	StartedAt      string       `json:"startedAt"`
	ExitedAt       *string      `json:"exitedAt"`
	ExitCode       *int         `json:"exitCode"`
	LastOutputAt   *string      `json:"lastOutputAt"`
	LastLine       *string      `json:"lastLine"`
}

type liveSession struct {
	Session
	ptmx   *os.File
	cmd    *exec.Cmd
	log    []string
	killed bool
	cwd    string // where claude actually runs: the worktree (or subfolder) in worktree mode
	// worktree cleanup context, captured at launch
	repoRoot   string
	wtBranch   string
	baseCommit string
}

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
func (a *app) announce(event string, s Session, killed bool) {
	where := filepath.Base(s.Folder)
	if s.Branch != nil {
		where += " @ " + *s.Branch
	}
	failed := event == evSessionExit && !killed && (s.ExitCode == nil || *s.ExitCode != 0 || s.State == stateError)
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
		if s.State == stateError {
			suffix = " (died before ready)"
		}
		exitMessage = fmt.Sprintf("%s exited with code %s%s", where, code, suffix)
	}
	titles := map[string]string{
		evSessionStart: "session started: " + s.Name,
		evSessionReady: "session ready: " + s.Name,
		evSessionKill:  "session killed: " + s.Name,
		evSessionExit:  exitTitle,
	}
	messages := map[string]string{
		evSessionStart: where,
		evSessionReady: where + " — " + pairing,
		evSessionKill:  where,
		evSessionExit:  exitMessage,
	}
	title, ok := titles[event]
	if !ok {
		title = event
	}
	message := messages[event]
	alsoMatch := []string{}
	if failed {
		alsoMatch = []string{evSessionFailed}
	}
	a.emit(event, map[string]any{"session": s}, emitOpts{title: title, message: message, alsoMatch: alsoMatch})
	if s.CallbackURL != nil && event != evSessionStart {
		a.deliverWebhook(*s.CallbackURL, LifecycleEvent{
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

func (a *app) recentLaunches(limit int) []RecentLaunch {
	seen := map[string]bool{}
	out := []RecentLaunch{}
	entries := a.readJournal()
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		folder := jStr(e, "folder")
		if jStr(e, "event") != evSessionStart || folder == "" {
			continue
		}
		if !pathExists(folder) || !a.withinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		rawSpawnMode := jStr(e, "spawnMode")
		key := folder + "\x00" + branch + "\x00" + rawSpawnMode
		if seen[key] {
			continue
		}
		seen[key] = true
		mode := rawSpawnMode
		if mode == "" {
			mode = string(spawnSameDir)
		}
		permissionMode := jStr(e, "permissionMode")
		if permissionMode == "" {
			permissionMode = "default"
		}
		out = append(out, RecentLaunch{
			Folder:         folder,
			Name:           jStr(e, "name"),
			Branch:         strPtr(branch),
			SpawnMode:      mode,
			PermissionMode: permissionMode,
			At:             jStr(e, "at"),
			Stale:          branch != "" && !branchExists(folder, branch),
		})
	}
	return out
}

func (a *app) listLostSessions() []LostSession {
	// snapshot live ids before taking lostMu — never hold both locks
	liveIDs := map[string]bool{}
	a.sessionsMu.Lock()
	for id := range a.sessions {
		liveIDs[id] = true
	}
	a.sessionsMu.Unlock()

	a.lostMu.Lock()
	defer a.lostMu.Unlock()
	if a.lostComputed {
		// copy: a dismissal splicing lostCache must not race a caller still marshaling
		return append([]LostSession(nil), a.lostCache...)
	}
	entries := a.readJournal()
	terminated := map[string]bool{}
	for _, e := range entries {
		ev := jStr(e, "event")
		if (ev == evSessionExit || ev == evSessionKill) && jStr(e, "id") != "" {
			terminated[jStr(e, "id")] = true
		}
	}
	cutoff := time.Now().Add(-lostWindow)
	out := []LostSession{}
	for _, e := range entries {
		id := jStr(e, "id")
		folder := jStr(e, "folder")
		if jStr(e, "event") != evSessionStart || id == "" || folder == "" {
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
		if !pathExists(folder) || !a.withinRoots(folder) {
			continue
		}
		branch := jStr(e, "branch")
		mode := jStr(e, "spawnMode")
		if mode == "" {
			mode = string(spawnSameDir)
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
				SpawnMode:      mode,
				PermissionMode: permissionMode,
				At:             at,
				Stale:          branch != "" && !branchExists(folder, branch),
			},
		})
	}
	a.lostCache = out
	a.lostComputed = true
	return append([]LostSession(nil), a.lostCache...)
}

/* ---------- kept worktrees (dirty orphans the sweeps refused to delete) ---------- */

type KeptWorktree struct {
	Path   string  `json:"path"`
	Repo   string  `json:"repo"`
	ID     string  `json:"id"`
	Branch *string `json:"branch"`
	Dirty  bool    `json:"dirty"`
}

func (a *app) listKeptWorktrees() []KeptWorktree {
	out := []KeptWorktree{}
	if !pathExists(a.wtBase) {
		return out
	}
	live := map[string]bool{}
	a.sessionsMu.Lock()
	for _, s := range a.sessions {
		if s.WorktreePath != nil && *s.WorktreePath != "" {
			live[*s.WorktreePath] = true
		}
	}
	a.sessionsMu.Unlock()
	repos, _ := os.ReadDir(a.wtBase)
	for _, repo := range repos {
		ids, err := os.ReadDir(filepath.Join(a.wtBase, repo.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			wtPath := filepath.Join(a.wtBase, repo.Name(), id.Name())
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

func (a *app) forceRemoveWorktree(wtPath string) error {
	resolved := filepath.Clean(wtPath) // normalize
	if !strings.HasPrefix(resolved, a.wtBase+"/") {
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
	a.journal(map[string]any{"event": "worktree.force-removed", "wtPath": resolved})
	return nil
}

func (a *app) listSessions() []Session {
	a.sessionsMu.Lock()
	out := make([]Session, 0, len(a.sessions))
	for _, s := range a.sessions {
		out = append(out, s.Session)
	}
	a.sessionsMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func (a *app) getSession(id string) *Session {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if s, ok := a.sessions[id]; ok {
		snap := s.Session
		return &snap
	}
	return nil
}

func (a *app) getSessionLog(id string) *string {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if s, ok := a.sessions[id]; ok {
		joined := strings.Join(s.log, "")
		return &joined
	}
	return nil
}

/* ---------- conversation transcripts ----------
Claude Code writes one JSONL transcript per conversation under
~/.claude/projects/<munged-cwd>/. The remote-control host and every session the
phone adds inside it share the launch cwd, so that one directory holds the whole
launch's conversations. */

var claudeProjectsDir = filepath.Join(mustHome(), ".claude", "projects")

// claudeProjectDirName mirrors Claude Code's path munging: every character
// outside [A-Za-z0-9] becomes "-".
func claudeProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

type TranscriptMessage struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
	At   string `json:"at"`
}

type Transcript struct {
	SessionID string              `json:"sessionId"` // Claude Code's conversation id, not ours
	UpdatedAt string              `json:"updatedAt"`
	Messages  []TranscriptMessage `json:"messages"`
}

const (
	maxTranscriptMessages = 200
	maxTranscriptText     = 4000
)

// transcriptText flattens a message's content: plain strings pass through,
// text blocks join, tool calls compress to "→ ToolName". Tool results and
// thinking blocks are noise at phone-log altitude and drop out.
func transcriptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := []string{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			parts = append(parts, "→ "+b.Name)
		}
	}
	return strings.Join(parts, "\n")
}

func parseTranscript(path string) *Transcript {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	t := &Transcript{SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			Timestamp   string `json:"timestamp"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.IsMeta || e.IsSidechain || e.Message == nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		text := transcriptText(e.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if r := []rune(text); len(r) > maxTranscriptText {
			text = string(r[:maxTranscriptText]) + " …"
		}
		t.Messages = append(t.Messages, TranscriptMessage{Role: e.Message.Role, Text: text, At: e.Timestamp})
		t.UpdatedAt = e.Timestamp
	}
	if len(t.Messages) > maxTranscriptMessages {
		t.Messages = t.Messages[len(t.Messages)-maxTranscriptMessages:]
	}
	return t
}

// getSessionTranscripts returns the conversations of a session's launch cwd.
// found=false means no such session; an empty slice means none written yet.
func (a *app) getSessionTranscripts(id string) (transcripts []Transcript, found bool) {
	a.sessionsMu.Lock()
	s, ok := a.sessions[id]
	var cwd, startedAt string
	if ok {
		cwd = s.cwd
		startedAt = s.StartedAt
	}
	a.sessionsMu.Unlock()
	if !ok {
		return nil, false
	}
	dir := filepath.Join(claudeProjectsDir, claudeProjectDirName(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Transcript{}, true
	}
	started, startedErr := time.Parse(time.RFC3339, startedAt)
	out := []Transcript{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		t := parseTranscript(filepath.Join(dir, e.Name()))
		if t == nil || len(t.Messages) == 0 {
			continue
		}
		// same-dir sessions share their project dir with every past run in that
		// folder — keep only conversations that began after this session did
		// (worktree cwds are unique per launch, so everything passes)
		if startedErr == nil {
			if first, err := time.Parse(time.RFC3339, t.Messages[0].At); err == nil && first.Before(started.Add(-time.Minute)) {
				continue
			}
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt < out[b].UpdatedAt })
	return out, true
}

// Boot sweep: sessions are in-memory, so any worktree on disk at startup is an
// orphan from a previous runner. Remove the clean ones; keep dirty ones and journal.
func (a *app) sweepOrphanWorktrees() {
	if !pathExists(a.wtBase) {
		return
	}
	repos, _ := os.ReadDir(a.wtBase)
	for _, repo := range repos {
		repoDir := filepath.Join(a.wtBase, repo.Name())
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
				a.journal(map[string]any{"event": "worktree.swept", "wtPath": wtPath})
			} else {
				a.journal(map[string]any{"event": "worktree.kept", "wtPath": wtPath, "reason": "orphan is dirty or unresolvable"})
			}
		}
	}
}

type createSessionOpts struct {
	folder, name, spawnMode, branch, permissionMode, callbackURL, actor string
}

func (a *app) createSession(opts createSessionOpts) (Session, error) {
	name := strings.TrimSpace(opts.name)
	if name == "" {
		name = filepath.Base(opts.folder) + "-" + randomID(4)
	}
	a.sessionsMu.Lock()
	duplicate := a.pendingNames[name]
	if !duplicate {
		for _, s := range a.sessions {
			if s.Name == name && s.State != stateExited && s.State != stateError {
				duplicate = true
				break
			}
		}
	}
	if !duplicate {
		// reserve the name across the slow worktree/switch/spawn work below, so two
		// concurrent launches can't both pass the duplicate check
		a.pendingNames[name] = true
	}
	a.sessionsMu.Unlock()
	if duplicate {
		return Session{}, fmt.Errorf("a live session named \"%s\" already exists", name)
	}
	defer func() {
		a.sessionsMu.Lock()
		delete(a.pendingNames, name)
		a.sessionsMu.Unlock()
	}()

	// opts carries the raw request string; everything past here is typed
	mode := spawnSameDir
	if opts.spawnMode == string(spawnWorktree) {
		mode = spawnWorktree
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
	if mode == spawnWorktree {
		if opts.branch == "" {
			return Session{}, errors.New("worktree mode requires a branch")
		}
		root := gitRoot(opts.folder)
		if root == "" {
			return Session{}, errors.New("folder is not inside a git repository")
		}
		branch = opts.branch
		repoRootForCleanup = root
		wt, err := a.addWorktree(root, branch, id, name) // worktree of the nearest repo root, cleaned up on exit
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

	// always --spawn same-dir: the runner already handled worktree creation itself
	args := []string{"remote-control", "--name", name, "--spawn", string(spawnSameDir), "--permission-mode", permissionMode}
	// real PTY: the CLI prints its pairing URL and stays alive as it would in a terminal
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1", "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if worktreePath != "" {
			a.removeWorktree(repoRootForCleanup, worktreePath, wtBranch, baseCommit)
		}
		return Session{}, err
	}

	s := &liveSession{
		Session: Session{
			ID:             id,
			Name:           name,
			Folder:         opts.folder,
			SpawnMode:      mode,
			Branch:         strPtr(branch),
			WorktreePath:   strPtr(worktreePath),
			PermissionMode: permissionMode,
			State:          stateStarting,
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
		cwd:        cwd,
		repoRoot:   repoRootForCleanup,
		wtBranch:   wtBranch,
		baseCommit: baseCommit,
	}
	a.sessionsMu.Lock()
	a.sessions[id] = s
	snap := s.Session
	a.sessionsMu.Unlock()

	entry := map[string]any{
		"event":          evSessionStart,
		"id":             id,
		"name":           name,
		"folder":         opts.folder,
		"spawnMode":      mode,
		"branch":         strPtr(branch),
		"permissionMode": permissionMode,
	}
	if opts.actor != "" {
		entry["actor"] = opts.actor
	}
	a.journal(entry)
	a.announce(evSessionStart, snap, false)

	go a.readLoop(s)

	return snap, nil
}

func (a *app) readLoop(s *liveSession) {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			var readySnap *Session
			var readyURL string
			a.sessionsMu.Lock()
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
					s.State = stateReady
					readyURL = url
					snap := s.Session
					readySnap = &snap
				}
			}
			id := s.ID
			a.sessionsMu.Unlock()
			if readySnap != nil {
				a.journal(map[string]any{"event": evSessionReady, "id": id, "pairingUrl": readyURL})
				a.announce(evSessionReady, *readySnap, false)
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

	a.sessionsMu.Lock()
	if s.State == stateStarting {
		s.State = stateError
	} else {
		s.State = stateExited
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
	a.sessionsMu.Unlock()

	if wtPath != "" {
		a.removeWorktree(cleanupRoot, wtPath, wtBranch, baseCommit)
	}
	a.journal(map[string]any{"event": evSessionExit, "id": id, "code": exitCode})
	a.announce(evSessionExit, snap, killed)
}

func (a *app) killSession(id, actor string) *Session {
	a.sessionsMu.Lock()
	s, ok := a.sessions[id]
	if !ok {
		a.sessionsMu.Unlock()
		return nil
	}
	s.killed = true // set before the kill so onExit never notifies for user-initiated stops
	if s.ptmx != nil && s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM) // pty close also HUPs the whole session tree; already gone is fine
	}
	snap := s.Session
	a.sessionsMu.Unlock()

	entry := map[string]any{"event": evSessionKill, "id": id}
	if actor != "" {
		entry["actor"] = actor
	}
	a.journal(entry)
	a.announce(evSessionKill, snap, true)
	return &snap
}

func (a *app) removeSession(id string) (bool, error) {
	a.sessionsMu.Lock()
	s, ok := a.sessions[id]
	if !ok {
		a.sessionsMu.Unlock()
		// lost-session headstones dismiss through the same endpoint
		a.lostMu.Lock()
		defer a.lostMu.Unlock()
		for i, l := range a.lostCache {
			if l.ID == id {
				a.lostCache = append(a.lostCache[:i], a.lostCache[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	}
	defer a.sessionsMu.Unlock()
	if s.State != stateExited && s.State != stateError {
		return false, errors.New("session is still live; kill it first")
	}
	delete(a.sessions, id)
	return true, nil
}
