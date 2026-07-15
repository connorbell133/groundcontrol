// Package sessions owns interactive remote-control sessions: a real PTY per
// launch, lifecycle events, registry-sourced enrichment (Claude conversation
// ids, busy/idle activity, folder-mate sessions), and the journal-derived
// views (recent launches, lost sessions) that make a restarted runner honest
// about what it forgot.
package sessions

import (
	"context"
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
// (announce adds it alongside a failed exit), never an emitted event itself.
// evSessionClaudeID is journal-only: the registry poller appends it on first
// UUID capture but never announces it — enrichment facts don't fan out (R8).
const (
	evSessionStart        = "session.start"
	evSessionReady        = "session.ready"
	evSessionKill         = "session.kill"
	evSessionExit         = "session.exit"
	evSessionFailed       = "session.failed"
	evSessionClaudeID     = "session.claude-id"
	evSessionInjectIntent = "session.inject-intent"
	evSessionDismissed    = "session.dismissed"
)

type Session struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Folder         string              `json:"folder"`
	SpawnMode      workspace.SpawnMode `json:"spawnMode"`
	Branch         *string             `json:"branch"`
	WorktreePath   *string             `json:"worktreePath"`
	PermissionMode string              `json:"permissionMode"`
	// resolved --capacity: how many sessions claude.ai can create in this
	// environment. Always populated from the normalized launch opts (the CLI
	// default 32 when the launch didn't ask), so the card's "N of capacity"
	// reads the live wire, never the journal.
	Capacity     int     `json:"capacity"`
	State        State   `json:"state"`
	PairingURL   *string `json:"pairingUrl"`
	CallbackURL  *string `json:"callbackUrl"`
	StartedAt    string  `json:"startedAt"`
	ExitedAt     *string `json:"exitedAt"`
	ExitCode     *int    `json:"exitCode"`
	LastOutputAt *string `json:"lastOutputAt"`
	LastLine     *string `json:"lastLine"`
	// scraped from the CLI's own "Capacity: used/max" status line — the
	// environment's live session count, preferred over registry-derived
	// counts when present. Absent until the line first appears, cleared at
	// exit, never journaled. Max can disagree with Capacity if the CLI's
	// default drifts; the scrape wins on the card.
	CapacityUsed *int `json:"capacityUsed,omitempty"`
	CapacityMax  *int `json:"capacityMax,omitempty"`
	// why a requested settings injection didn't happen; absent whenever the
	// file was injected or no preset settings were in play — the wire only
	// ever explains absence, never claims success (R11)
	SettingsSkipReason *string `json:"settingsSkipReason,omitempty"`
	// registry-sourced enrichment: every field degrades to absence and none
	// of them ever drives a state transition — the PTY stays the sole exit
	// authority (R6)
	ClaudeSessionID *string `json:"claudeSessionId,omitempty"`
	Activity        *string `json:"activity,omitempty"`
	// the environment's own live sessions (registry rows descended from the
	// spawned pid, primary first) vs foreign sessions that merely share the
	// launch folder — split at join time, where ownership is still knowable
	EnvironmentSessions []ExtraSession `json:"environmentSessions,omitempty"`
	FolderSessions      []ExtraSession `json:"folderSessions,omitempty"`
	PRLink              *PRLink        `json:"prLink,omitempty"`
	Debrief             *Debrief       `json:"debrief,omitempty"`
}

// ExtraSession is one live Claude session row, listed under
// EnvironmentSessions (the environment's own sessions — descendants of the
// spawned pid, phone- or claude.ai-created, primary first) or FolderSessions
// (unrelated sessions — manual, IDE — that happen to run in the folder;
// "in this folder" is the literal contract). Status carries only
// "busy"/"idle"; any other registry value reads as absent, mirroring the
// primary activity rule.
type ExtraSession struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
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
	// which source set PairingURL (bridge.go); the scrape keeps scanning while
	// the pointer holds it, so a drifted constructed URL can still be corrected
	pairingSource string
	// worktree cleanup context, captured at launch
	repoRoot   string
	wtBranch   string
	baseCommit string
	// settings.local.json this launch is responsible for (set whenever a
	// preset carried settings, even when injection skipped — the marker check
	// at removal protects user files); empty means nothing to tear down
	settingsPath string
	// registry enrichment bookkeeping, all guarded by m.mu
	extrasSeen             map[string]extraRecord // last sighting per extra row, for wall-clock aging
	activitySeenAt         time.Time              // last successful registry confirm of Activity
	claudeIDConflictLogged bool                   // a disagreeing later sessionId is logged once, not per tick
	prStatSize             int64                  // transcript size at the last pr-link scan (stat-gate)
}

// Manager owns the live session table. Lost-session and recent-launch views
// derive from the journal; the browser scopes them to the configured roots.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*liveSession
	// names reserved by launches still between duplicate-check and insertion
	pendingNames map[string]bool
	// same-dir launches reserve their normalized folder across the slow spawn
	// path, mirroring pendingNames: one live environment per folder (R-guard
	// below in Create)
	pendingFolders map[string]bool
	// watchers tracks bridge-pointer goroutines so tests can drain them before
	// restoring the package seams those goroutines read while alive
	watchers sync.WaitGroup

	lostMu       sync.Mutex
	lostCache    []LostSession
	lostComputed bool

	// registry poller lifecycle (guarded by mu): regCtx arms the loop,
	// regRunning guarantees at most one loop goroutine, observedAt feeds the
	// watched cadence tier
	regCtx     context.Context
	regRunning bool
	observedAt time.Time
	// regWake cuts a slow poll sleep short when a reader appears; buffered so
	// MarkObserved never blocks an API request
	regWake chan struct{}

	journal *journal.Journal
	bus     *events.Bus
	ws      *workspace.Manager
	browser *browse.Browser
}

func NewManager(j *journal.Journal, bus *events.Bus, ws *workspace.Manager, browser *browse.Browser) *Manager {
	return &Manager{
		sessions:       map[string]*liveSession{},
		pendingNames:   map[string]bool{},
		pendingFolders: map[string]bool{},
		regWake:        make(chan struct{}, 1),
		journal:        j,
		bus:            bus,
		ws:             ws,
		browser:        browser,
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
	capacityRE  = regexp.MustCompile(`Capacity:\s*(\d+)\s*/\s*(\d+)`)
)

// scanCapacity finds the newest "Capacity: used/max" pair in text — the TUI
// redraws its status area, so later occurrences supersede earlier ones.
func scanCapacity(text string) (used, max int, ok bool) {
	ms := capacityRE.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return 0, 0, false
	}
	last := ms[len(ms)-1]
	u, err1 := strconv.Atoi(last[1])
	x, err2 := strconv.Atoi(last[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return u, x, true
}

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
	Folder, Name, SpawnMode, Branch, PermissionMode, CallbackURL, Actor string
	// PresetName is the launch preset (if any) these opts came from — a
	// durable launch fact journaled for recents/relaunch, never interpreted here.
	PresetName string
	// Capacity is the requested --capacity; zero means "not asked" and
	// normalizes to the CLI default.
	Capacity int
	// SettingsJSON is the resolved preset's settings payload; non-empty asks
	// Create to inject it as <launch cwd>/.claude/settings.local.json before
	// spawn (inject.go owns the mechanism).
	SettingsJSON string
	// SettingsSkipReason pre-decides the injection outcome — the API layer
	// sets it when the named preset no longer exists, so the relaunch still
	// proceeds and the reason still reaches the journal and the wire (R8).
	SettingsSkipReason string
}

// --capacity bounds, matching the CLI (default probed on 2.1.210). Requests
// normalize instead of rejecting — absent or < 1 falls back to the default,
// oversized clamps to 256 — so a pre-capacity recent replayed through
// POST /sessions keeps launching.
const (
	defaultCapacity = 32
	maxCapacity     = 256
)

func normalizeCapacity(n int) int {
	switch {
	case n < 1:
		return defaultCapacity
	case n > maxCapacity:
		return maxCapacity
	}
	return n
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

	// one live environment per folder for same-dir launches: a second one would
	// register a second claude.ai environment with an identical picker label
	// (environments are named by folder basename — verified 2.1.210), and one
	// environment already holds up to --capacity sessions. Worktree launches
	// get their own uniquely named folders and skip this.
	if mode == workspace.SpawnSameDir {
		folderKey := normalizePath(opts.Folder)
		m.mu.Lock()
		folderBusy := m.pendingFolders[folderKey]
		if !folderBusy {
			m.pendingFolders[folderKey] = true
		}
		m.mu.Unlock()
		if folderBusy {
			return Session{}, errors.New("an environment is already launching in this folder — one live environment per folder; use worktree mode for a second")
		}
		defer func() {
			m.mu.Lock()
			delete(m.pendingFolders, folderKey)
			m.mu.Unlock()
		}()
		if m.liveSameFolderExists(opts.Folder) {
			return Session{}, errors.New("an environment is already live in this folder — open that one in claude.ai, kill it, or launch in worktree mode (claude.ai labels same-folder environments identically)")
		}
	}
	permissionMode := opts.PermissionMode
	if permissionMode == "" {
		permissionMode = "default"
	}
	capacity := normalizeCapacity(opts.Capacity)
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

	// preset settings injection — before spawn, into the launch cwd (the
	// worktree in worktree mode), so the pre-created session reads the file
	// natively; a skip never blocks the launch
	settingsInjected := false
	settingsReplaced := false
	settingsSkipReason := opts.SettingsSkipReason
	settingsPath := ""
	if settingsSkipReason == "" && opts.SettingsJSON != "" {
		settingsPath = settingsFilePath(cwd)
		// journal the intent before writing the file so a crash between the
		// write and the session.start entry still leaves a folder the boot
		// sweep can find and reclaim (R12); a crash before the write leaves an
		// intent with no file, which the marker-gated sweep no-ops on.
		m.journal.Append(map[string]any{"event": evSessionInjectIntent, "id": id, "folder": opts.Folder})
		settingsInjected, settingsReplaced, settingsSkipReason = m.injectSettings(settingsPath, cwd, opts.SettingsJSON, id)
	}

	// always --spawn same-dir: the runner already handled worktree creation itself
	args := []string{"remote-control", "--name", name, "--spawn", string(workspace.SpawnSameDir), "--permission-mode", permissionMode}
	if capacity != defaultCapacity {
		// omitted at the default so launches keep working on CLIs that predate --capacity
		args = append(args, "--capacity", strconv.Itoa(capacity))
	}
	// real PTY: the CLI prints its pairing URL and stays alive as it would in a terminal
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1", "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		if settingsInjected {
			// no session will ever tear this down — reclaim it here
			removeMarkedSettings(settingsPath)
		}
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
			Capacity:       capacity,
			State:          StateStarting,
			PairingURL:     nil,
			CallbackURL:    util.StrPtr(opts.CallbackURL),
			StartedAt:      util.NowISO(),
			ExitedAt:       nil,
			ExitCode:       nil,
			LastOutputAt:   nil,
			LastLine:       nil,
			// present only when injection was requested but skipped (R11)
			SettingsSkipReason: util.StrPtr(settingsSkipReason),
		},
		ptmx:         ptmx,
		cmd:          cmd,
		cwd:          cwd,
		repoRoot:     repoRootForCleanup,
		wtBranch:     wtBranch,
		baseCommit:   baseCommit,
		settingsPath: settingsPath,
	}
	m.mu.Lock()
	m.sessions[id] = s
	snap := s.Session
	// arm the registry poller on the 0→1 live-session transition: regRunning
	// flips under the same lock the loop uses when it observes zero live
	// sessions, so exactly one loop goroutine can exist at a time
	startPoll := m.regCtx != nil && !m.regRunning
	if startPoll {
		m.regRunning = true
	}
	regCtx := m.regCtx
	m.mu.Unlock()
	if startPoll {
		go m.registryLoop(regCtx)
	}

	entry := map[string]any{
		"event":          evSessionStart,
		"id":             id,
		"name":           name,
		"folder":         opts.Folder,
		"spawnMode":      mode,
		"branch":         util.StrPtr(branch),
		"permissionMode": permissionMode,
	}
	if capacity != defaultCapacity {
		// journaled exactly when the flag is spawned, so entry presence mirrors
		// the args and a defaulted (or pre-capacity) entry reads back as 32
		entry["capacity"] = capacity
	}
	if opts.PresetName != "" {
		entry["presetName"] = opts.PresetName
	}
	// injection outcome flattens into session.start — never a standalone
	// event: it's a launch fact, and the startup sweep reads it back from here
	if settingsInjected {
		entry["settingsInjected"] = true
		if settingsReplaced {
			entry["settingsNote"] = noteReplacedStale
		}
	} else if settingsSkipReason != "" {
		entry["settingsSkipReason"] = settingsSkipReason
	}
	if opts.Actor != "" {
		entry["actor"] = opts.Actor
	}
	m.journal.Append(entry)
	m.announce(evSessionStart, snap, false)

	go m.readLoop(s)
	// primary pairing-URL source; short-lived, ends at first resolution or timeout
	m.watchers.Add(1)
	go func() {
		defer m.watchers.Done()
		m.watchBridgePointer(s, cmd.Process.Pid)
	}()

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
			// a chunk boundary can split the capacity line, so scan a short log
			// tail rather than the chunk; no match keeps the last known value
			capTail := s.log
			if len(capTail) > 4 {
				capTail = capTail[len(capTail)-4:]
			}
			if used, max, ok := scanCapacity(strings.Join(capTail, "")); ok {
				s.CapacityUsed = util.IntPtr(used)
				s.CapacityMax = util.IntPtr(max)
			}
			// keep scanning past ready while the bridge pointer holds the URL:
			// the CLI's own output must be able to overrule a constructed URL
			if s.PairingURL == nil || s.pairingSource == pairingSourcePointer {
				if found := urlRE.FindString(strings.Join(s.log, "")); found != "" {
					url := trailingRE.ReplaceAllString(found, "")
					if snap := s.setReadyLocked(url, pairingSourceScrape); snap != nil {
						readyURL = url
						readySnap = snap
					} else {
						s.reconcileScrapedURLLocked(url)
					}
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
	// exit is authoritative for registry enrichment too: a "busy" chip must
	// die with the session (the poller's state guard keeps a stale snapshot
	// from resurrecting it), while the extras list freezes as-is
	s.Activity = nil
	// capacity describes a live environment; a dead card must not claim one
	s.CapacityUsed, s.CapacityMax = nil, nil
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
	settingsPath, cwd := s.settingsPath, s.cwd
	snap := s.Session
	m.mu.Unlock()

	if settingsPath != "" {
		// before the debrief and worktree cleanup: the injected file must not
		// count as the run's own uncommitted work or keep a worktree dirty.
		// Removal defers to the last same-folder exit; the marker check keeps
		// user files untouchable either way (inject.go).
		m.removeSettingsIfLast(settingsPath, cwd)
	}

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
	if snap.ClaudeSessionID != nil {
		// flattened like the debrief fields so ListLanded (and a restarted
		// runner) can read it off the exit entry even after the standalone
		// session.claude-id event scrolls off the journal's read window
		exitEntry["claudeSessionId"] = *snap.ClaudeSessionID
	}
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
