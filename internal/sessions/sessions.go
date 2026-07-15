// Package sessions owns interactive remote-control sessions: a real PTY per
// launch, lifecycle events, and the journal-derived views (recent launches,
// lost sessions) that make a restarted runner honest about what it forgot.
package sessions

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

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/gitx"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/util"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

// State keeps string underneath: JSON marshals identically, and the untyped
// literals other packages compare against still compile.
type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateExited   State = "exited"
	StateError    State = "error"
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
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Folder         string              `json:"folder"`
	SpawnMode      workspace.SpawnMode `json:"spawnMode"`
	Branch         *string             `json:"branch"`
	WorktreePath   *string             `json:"worktreePath"`
	PermissionMode string              `json:"permissionMode"`
	InitialPrompt  *string             `json:"initialPrompt,omitempty"`
	State          State               `json:"state"`
	PairingURL     *string             `json:"pairingUrl"`
	CallbackURL    *string             `json:"callbackUrl"`
	StartedAt      string              `json:"startedAt"`
	ExitedAt       *string             `json:"exitedAt"`
	ExitCode       *int                `json:"exitCode"`
	LastOutputAt   *string             `json:"lastOutputAt"`
	LastLine       *string             `json:"lastLine"`
	Debrief        *Debrief            `json:"debrief,omitempty"`
}

// Debrief records what a worktree run left behind, captured at exit because
// the worktree (and any uncommitted work in it) is gone right after. Absent
// for same-dir sessions and whenever git failed at exit time.
type Debrief struct {
	gitx.DiffStats
	BranchState string `json:"branchState"`
}

// branchState values: what actually survived worktree cleanup
const (
	branchMerged       = "merged"        // branch gone, or its tip reachable from the default branch
	branchInOrbit      = "in-orbit"      // branch survived with commits the default branch lacks
	branchWorktreeKept = "worktree-kept" // dirty worktree kept on disk, branch intact
)

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

// Manager owns the live session table. Lost-session and recent-launch views
// derive from the journal; the browser scopes them to the configured roots.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*liveSession
	// names reserved by launches still between duplicate-check and insertion
	pendingNames map[string]bool

	lostMu       sync.Mutex
	lostCache    []LostSession
	lostComputed bool

	journal *journal.Journal
	bus     *events.Bus
	ws      *workspace.Manager
	browser *browse.Browser
}

func NewManager(j *journal.Journal, bus *events.Bus, ws *workspace.Manager, browser *browse.Browser) *Manager {
	return &Manager{
		sessions:     map[string]*liveSession{},
		pendingNames: map[string]bool{},
		journal:      j,
		bus:          bus,
		ws:           ws,
		browser:      browser,
	}
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
func (m *Manager) announce(event string, s Session, killed bool) {
	where := filepath.Base(s.Folder)
	if s.Branch != nil {
		where += " @ " + *s.Branch
	}
	failed := event == evSessionExit && !killed && (s.ExitCode == nil || *s.ExitCode != 0 || s.State == StateError)
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
		if s.State == StateError {
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
	m.bus.Emit(event, map[string]any{"session": s}, events.EmitOpts{Title: title, Message: message, AlsoMatch: alsoMatch})
	if s.CallbackURL != nil && event != evSessionStart {
		m.bus.DeliverWebhook(*s.CallbackURL, events.LifecycleEvent{
			Event:   event,
			At:      util.NowISO(),
			Title:   title,
			Message: message,
			Data:    map[string]any{"session": s},
		})
	}
}

func (m *Manager) List() []Session {
	m.mu.Lock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Session)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		snap := s.Session
		return &snap
	}
	return nil
}

func (m *Manager) GetLog(id string) *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		joined := strings.Join(s.log, "")
		return &joined
	}
	return nil
}

// LiveWorktreePaths snapshots the worktree paths owned by live sessions —
// the set workspace.ListKept subtracts from the on-disk state.
func (m *Manager) LiveWorktreePaths() map[string]bool {
	live := map[string]bool{}
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.WorktreePath != nil && *s.WorktreePath != "" {
			live[*s.WorktreePath] = true
		}
	}
	m.mu.Unlock()
	return live
}

// LiveWorktreeBranches snapshots the gc/ branches checked out by running
// sessions. Exited-but-undismissed sessions don't count: their surviving
// branches are exactly what the orbit view exists to surface.
func (m *Manager) LiveWorktreeBranches() map[string]bool {
	live := map[string]bool{}
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.wtBranch != "" && s.State != StateExited && s.State != StateError {
			live[s.wtBranch] = true
		}
	}
	m.mu.Unlock()
	return live
}

// WaitForReady blocks until the session pairs, dies, or the deadline passes —
// 300ms poll is plenty against a multi-second provision and immune to event races.
func (m *Manager) WaitForReady(id string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		s := m.Get(id)
		if s == nil || s.State == StateExited || s.State == StateError {
			return "dead"
		}
		if s.State == StateReady {
			return "ready"
		}
		if !time.Now().Before(deadline) {
			return "timeout"
		}
		time.Sleep(300 * time.Millisecond)
	}
}

type CreateOpts struct {
	Folder, Name, SpawnMode, Branch, PermissionMode, InitialPrompt, CallbackURL, Actor string
}

func (m *Manager) Create(opts CreateOpts) (Session, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = filepath.Base(opts.Folder) + "-" + util.RandomID(4)
	}
	m.mu.Lock()
	duplicate := m.pendingNames[name]
	if !duplicate {
		for _, s := range m.sessions {
			if s.Name == name && s.State != StateExited && s.State != StateError {
				duplicate = true
				break
			}
		}
	}
	if !duplicate {
		// reserve the name across the slow worktree/switch/spawn work below, so two
		// concurrent launches can't both pass the duplicate check
		m.pendingNames[name] = true
	}
	m.mu.Unlock()
	if duplicate {
		return Session{}, fmt.Errorf("a live session named \"%s\" already exists", name)
	}
	defer func() {
		m.mu.Lock()
		delete(m.pendingNames, name)
		m.mu.Unlock()
	}()

	// opts carries the raw request string; everything past here is typed
	mode := workspace.SpawnSameDir
	if opts.SpawnMode == string(workspace.SpawnWorktree) {
		mode = workspace.SpawnWorktree
	}
	permissionMode := opts.PermissionMode
	if permissionMode == "" {
		permissionMode = "default"
	}
	id := util.RandomID(8)

	worktreePath := ""
	branch := ""
	wtBranch := ""
	baseCommit := ""
	cwd := opts.Folder
	repoRootForCleanup := ""
	if mode == workspace.SpawnWorktree {
		if opts.Branch == "" {
			return Session{}, errors.New("worktree mode requires a branch")
		}
		root := gitx.Root(opts.Folder)
		if root == "" {
			return Session{}, errors.New("folder is not inside a git repository")
		}
		branch = opts.Branch
		repoRootForCleanup = root
		wt, err := m.ws.Add(root, branch, id, name) // worktree of the nearest repo root, cleaned up on exit
		if err != nil {
			return Session{}, err
		}
		worktreePath = wt.Path
		wtBranch = wt.Branch
		baseCommit = wt.BaseCommit
		// land in the equivalent subfolder inside the worktree when it exists on that branch
		rel, relErr := filepath.Rel(root, opts.Folder)
		cwd = worktreePath
		if relErr == nil && rel != "" && rel != "." {
			sub := filepath.Join(worktreePath, rel)
			if util.PathExists(sub) {
				cwd = sub
			}
		}
	} else if opts.Branch != "" {
		// in-folder launch on a chosen branch: switch the checkout first — git refuses if
		// that would clobber local changes, and DWIMs a local branch for remote-only picks
		root := gitx.Root(opts.Folder)
		if root != "" {
			if gitx.CurrentBranch(root) != opts.Branch {
				if err := gitx.Run(root, 15*time.Second, "switch", opts.Branch); err != nil {
					msg := strings.TrimSpace(err.Error())
					// no stderr to surface — fall back to a generic message, like the TS
					if msg == "" || strings.HasPrefix(msg, "exit status") || strings.HasPrefix(msg, "signal:") {
						msg = "could not switch to branch " + opts.Branch
					}
					return Session{}, errors.New(msg)
				}
			}
			branch = opts.Branch
		}
	}

	// always --spawn same-dir: the runner already handled worktree creation itself
	args := []string{"remote-control", "--name", name, "--spawn", string(workspace.SpawnSameDir), "--permission-mode", permissionMode}
	// real PTY: the CLI prints its pairing URL and stays alive as it would in a terminal
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1", "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if worktreePath != "" {
			m.ws.Remove(repoRootForCleanup, worktreePath, wtBranch, baseCommit)
		}
		return Session{}, err
	}

	s := &liveSession{
		Session: Session{
			ID:             id,
			Name:           name,
			Folder:         opts.Folder,
			SpawnMode:      mode,
			Branch:         util.StrPtr(branch),
			WorktreePath:   util.StrPtr(worktreePath),
			PermissionMode: permissionMode,
			InitialPrompt:  util.StrPtr(opts.InitialPrompt),
			State:          StateStarting,
			PairingURL:     nil,
			CallbackURL:    util.StrPtr(opts.CallbackURL),
			StartedAt:      util.NowISO(),
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
	m.mu.Lock()
	m.sessions[id] = s
	snap := s.Session
	m.mu.Unlock()

	entry := map[string]any{
		"event":          evSessionStart,
		"id":             id,
		"name":           name,
		"folder":         opts.Folder,
		"spawnMode":      mode,
		"branch":         util.StrPtr(branch),
		"permissionMode": permissionMode,
	}
	if opts.Actor != "" {
		entry["actor"] = opts.Actor
	}
	if opts.InitialPrompt != "" {
		entry["initialPrompt"] = opts.InitialPrompt
	}
	m.journal.Append(entry)
	m.announce(evSessionStart, snap, false)

	go m.readLoop(s)

	return snap, nil
}

func (m *Manager) readLoop(s *liveSession) {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			var readySnap *Session
			var readyURL string
			m.mu.Lock()
			s.LastOutputAt = util.StrPtr(util.NowISO())
			text := ansiRE.ReplaceAllString(chunk, "")
			s.log = append(s.log, text)
			if len(s.log) > 400 {
				s.log = s.log[len(s.log)-400:]
			}
			if line := lastMeaningfulLine(s.log); line != "" {
				s.LastLine = util.StrPtr(line)
			}
			if s.PairingURL == nil {
				if found := urlRE.FindString(strings.Join(s.log, "")); found != "" {
					url := trailingRE.ReplaceAllString(found, "")
					s.PairingURL = &url
					s.State = StateReady
					readyURL = url
					snap := s.Session
					readySnap = &snap
				}
			}
			id := s.ID
			m.mu.Unlock()
			if readySnap != nil {
				m.journal.Append(map[string]any{"event": evSessionReady, "id": id, "pairingUrl": readyURL})
				m.announce(evSessionReady, *readySnap, false)
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

	m.mu.Lock()
	if s.State == StateStarting {
		s.State = StateError
	} else {
		s.State = StateExited
	}
	s.ExitedAt = util.StrPtr(util.NowISO())
	s.ExitCode = util.IntPtr(exitCode)
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
	m.mu.Unlock()

	var debrief *Debrief
	if wtPath != "" {
		// stats must precede Remove — the worktree and its uncommitted work are
		// about to be deleted. Best-effort throughout: any git failure means an
		// absent debrief, never a blocked teardown.
		if st, err := gitx.DiffStat(wtPath, baseCommit); err == nil {
			debrief = &Debrief{DiffStats: st}
		}
		m.ws.Remove(cleanupRoot, wtPath, wtBranch, baseCommit)
		if debrief != nil {
			// after Remove: disk and refs are reality, not Remove's intent
			debrief.BranchState = branchStateAfterRemove(cleanupRoot, wtPath, wtBranch)
		}
	}
	if debrief != nil {
		m.mu.Lock()
		s.Debrief = debrief
		snap = s.Session
		m.mu.Unlock()
	}
	exitEntry := map[string]any{"event": evSessionExit, "id": id, "code": exitCode}
	if debrief != nil {
		exitEntry["filesChanged"] = debrief.FilesChanged
		exitEntry["insertions"] = debrief.Insertions
		exitEntry["deletions"] = debrief.Deletions
		exitEntry["uncommitted"] = debrief.Uncommitted
		exitEntry["branchState"] = debrief.BranchState
	}
	m.journal.Append(exitEntry)
	m.announce(evSessionExit, snap, killed)
}

// branchStateAfterRemove inspects what workspace.Remove actually left behind
// rather than plumbing its decisions back out: the answer stays honest even
// when Remove's internals change.
func branchStateAfterRemove(root, wtPath, wtBranch string) string {
	if util.PathExists(wtPath) {
		return branchWorktreeKept // removal refused (dirty) — the work is still on disk
	}
	tip, err := gitx.Out(root, 5*time.Second, "rev-parse", "--verify", "--quiet", "refs/heads/"+wtBranch)
	if err != nil || tip == "" {
		return branchMerged // Remove deleted the no-commit branch: nothing beyond the base survives
	}
	if def := gitx.DefaultRef(root); def != "" {
		if gitx.Run(root, 5*time.Second, "merge-base", "--is-ancestor", tip, def) == nil {
			return branchMerged
		}
	}
	return branchInOrbit
}

func (m *Manager) Kill(id, actor string) *Session {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	s.killed = true // set before the kill so onExit never notifies for user-initiated stops
	if s.ptmx != nil && s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM) // pty close also HUPs the whole session tree; already gone is fine
	}
	snap := s.Session
	m.mu.Unlock()

	entry := map[string]any{"event": evSessionKill, "id": id}
	if actor != "" {
		entry["actor"] = actor
	}
	m.journal.Append(entry)
	m.announce(evSessionKill, snap, true)
	return &snap
}

func (m *Manager) Remove(id string) (bool, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		// lost-session headstones dismiss through the same endpoint
		m.lostMu.Lock()
		defer m.lostMu.Unlock()
		for i, l := range m.lostCache {
			if l.ID == id {
				m.lostCache = append(m.lostCache[:i], m.lostCache[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	}
	defer m.mu.Unlock()
	if s.State != StateExited && s.State != StateError {
		return false, errors.New("session is still live; kill it first")
	}
	delete(m.sessions, id)
	return true, nil
}
