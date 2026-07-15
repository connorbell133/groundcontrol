// Package jobs runs headless jobs: `claude -p` with JSON output — no PTY, no
// pairing, no human. A different resource from sessions on purpose: different
// lifecycle, different defaults (fresh worktree — an autonomous run should not
// dirty a checkout you might be standing in).
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

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
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateTimeout   State = "timeout"
	StateCanceled  State = "canceled"
)

// job lifecycle event names; evJobFailed is a derived match token (announce
// adds it alongside a failed/timeout exit), never an emitted event itself
const (
	evJobQueued = "job.queued"
	evJobStart  = "job.start"
	evJobExit   = "job.exit"
	evJobCancel = "job.cancel"
	evJobFailed = "job.failed"
)

type Job struct {
	ID             string              `json:"id"`
	Folder         string              `json:"folder"`
	Prompt         string              `json:"prompt"`
	SpawnMode      workspace.SpawnMode `json:"spawnMode"`
	Branch         *string             `json:"branch"`
	PermissionMode string              `json:"permissionMode"`
	TimeoutMs      int                 `json:"timeoutMs"`
	CallbackURL    *string             `json:"callbackUrl"`
	State          State               `json:"state"`
	CreatedAt      string              `json:"createdAt"`
	StartedAt      *string             `json:"startedAt"`
	FinishedAt     *string             `json:"finishedAt"`
	ExitCode       *int                `json:"exitCode"`
	Result         *string             `json:"result"` // claude's final text (or the launch error)
	CostUsd        *float64            `json:"costUsd"`
	DurationMs     *int                `json:"durationMs"`
	NumTurns       *int                `json:"numTurns"`
	WorktreePath   *string             `json:"worktreePath"`
}

type liveJob struct {
	Job
	proc            *exec.Cmd
	log             []string
	timer           *time.Timer
	timedOut        bool
	cancelRequested bool
	actor           string
}

// Manager owns the job table and the bounded-concurrency queue.
type Manager struct {
	mu       sync.Mutex
	jobs     map[string]*liveJob
	queue    []string
	running  int
	defaults struct{ concurrency, timeoutMs int }

	journal *journal.Journal
	bus     *events.Bus
	ws      *workspace.Manager
}

func NewManager(j *journal.Journal, bus *events.Bus, ws *workspace.Manager) *Manager {
	m := &Manager{
		jobs:    map[string]*liveJob{},
		journal: j,
		bus:     bus,
		ws:      ws,
	}
	m.defaults = struct{ concurrency, timeoutMs int }{concurrency: 2, timeoutMs: 15 * 60 * 1000}
	return m
}

// Configure overrides the queue defaults; zero values keep the current setting.
func (m *Manager) Configure(concurrency, timeoutMs int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if concurrency != 0 {
		m.defaults.concurrency = max(1, concurrency)
	}
	if timeoutMs != 0 {
		m.defaults.timeoutMs = max(1000, timeoutMs)
	}
}

func (m *Manager) announce(event string, j Job) {
	where := filepath.Base(j.Folder)
	if j.Branch != nil && *j.Branch != "" {
		where += " @ " + *j.Branch
	}
	failed := j.State == StateFailed || j.State == StateTimeout
	var title string
	if event == evJobStart {
		title = "job started: " + where
	} else {
		title = "job " + string(j.State) + ": " + where
	}
	result := ""
	if j.Result != nil {
		result = *j.Result
	}
	var message string
	switch {
	case event == evJobStart:
		message = util.FirstRunes(j.Prompt, 120)
	case j.State == StateSucceeded:
		message = util.FirstRunes(result, 200)
		if j.CostUsd != nil {
			message += fmt.Sprintf(" ($%.4f)", *j.CostUsd)
		}
	default:
		message = util.FirstRunes(result, 200)
	}
	var alsoMatch []string
	if failed {
		alsoMatch = []string{evJobFailed}
	}
	m.bus.Emit(event, map[string]any{"job": j}, events.EmitOpts{Title: title, Message: message, AlsoMatch: alsoMatch})
	if j.CallbackURL != nil && event == evJobExit {
		m.bus.DeliverWebhook(*j.CallbackURL, events.LifecycleEvent{Event: event, At: util.NowISO(), Title: title, Message: message, Data: map[string]any{"job": j}})
	}
}

// the journal doubles as a UI surface — a hash plus a preview, never full prompts
func promptDigest(prompt string) (promptHash, promptPreview string) {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])[:12], util.FirstRunes(prompt, 80)
}

func (m *Manager) List() []Job {
	m.mu.Lock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j.Job)
	}
	m.mu.Unlock()
	sort.Slice(out, func(x, y int) bool { return out[x].CreatedAt > out[y].CreatedAt })
	return out
}

func (m *Manager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	snap := j.Job
	return &snap
}

func (m *Manager) GetLog(id string) *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	joined := strings.Join(j.log, "")
	return &joined
}

type CreateOpts struct {
	Folder, Prompt, SpawnMode, Branch, PermissionMode, CallbackURL, Actor string
	TimeoutMs                                                             int // <0 = absent, use default
}

func (m *Manager) Create(opts CreateOpts) (Job, error) {
	root := gitx.Root(opts.Folder)
	// jobs default to a fresh worktree wherever git makes one possible;
	// opts carries the raw request string — everything past here is typed
	mode := workspace.SpawnSameDir
	if opts.SpawnMode != string(workspace.SpawnSameDir) && (opts.SpawnMode == string(workspace.SpawnWorktree) || root != "") {
		mode = workspace.SpawnWorktree
	}
	var branch *string
	if mode == workspace.SpawnWorktree {
		if root == "" {
			return Job{}, errors.New("worktree mode requires a folder inside a git repository")
		}
		b := opts.Branch
		if b == "" {
			b = gitx.CurrentBranch(root)
		}
		if b == "" {
			return Job{}, errors.New("no branch given and the repository is detached — pass branch explicitly")
		}
		branch = &b
	}

	permissionMode := opts.PermissionMode
	if permissionMode == "" {
		permissionMode = "acceptEdits"
	}
	m.mu.Lock()
	timeoutMs := opts.TimeoutMs
	if timeoutMs < 0 { // absent — an explicit 0 clamps to the 1s floor, like the TS
		timeoutMs = m.defaults.timeoutMs
	}
	timeoutMs = min(2*60*60*1000, max(1000, timeoutMs))

	j := &liveJob{
		Job: Job{
			ID:             util.RandomID(8),
			Folder:         opts.Folder,
			Prompt:         opts.Prompt,
			SpawnMode:      mode,
			Branch:         branch,
			PermissionMode: permissionMode,
			TimeoutMs:      timeoutMs,
			CallbackURL:    util.StrPtr(opts.CallbackURL),
			State:          StateQueued,
			CreatedAt:      util.NowISO(),
		},
		actor: opts.Actor,
	}
	m.jobs[j.ID] = j
	m.queue = append(m.queue, j.ID)
	m.mu.Unlock()

	promptHash, promptPreview := promptDigest(j.Prompt)
	entry := map[string]any{"event": evJobQueued, "id": j.ID, "folder": j.Folder, "spawnMode": mode, "branch": branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if opts.Actor != "" {
		entry["actor"] = opts.Actor
	}
	m.journal.Append(entry)
	m.startNext()

	m.mu.Lock()
	snap := j.Job
	m.mu.Unlock()
	return snap, nil
}

// caller holds mu; returns the jobs whose queue slot was just claimed —
// the caller launches them after releasing the lock
func (m *Manager) startNextLocked() []*liveJob {
	var toStart []*liveJob
	for m.running < m.defaults.concurrency && len(m.queue) > 0 {
		id := m.queue[0]
		m.queue = m.queue[1:]
		j, ok := m.jobs[id]
		if !ok || j.State != StateQueued {
			continue // canceled while queued
		}
		m.running++
		j.State = StateRunning
		j.StartedAt = util.StrPtr(util.NowISO())
		toStart = append(toStart, j)
	}
	return toStart
}

func (m *Manager) startNext() {
	m.mu.Lock()
	toStart := m.startNextLocked()
	m.mu.Unlock()
	for _, j := range toStart {
		go m.startJob(j)
	}
}

func (m *Manager) startJob(j *liveJob) {
	cwd := j.Folder
	var wt *workspace.Info
	root := ""
	if j.SpawnMode == workspace.SpawnWorktree {
		root = gitx.Root(j.Folder)
		w, err := m.ws.Add(root, *j.Branch, j.ID, j.Prompt)
		if err != nil {
			msg := err.Error()
			m.finishJob(j, jobOutcome{state: StateFailed, result: &msg})
			return
		}
		wt = &w
		m.mu.Lock()
		j.WorktreePath = util.StrPtr(w.Path)
		m.mu.Unlock()
		sub := w.Path
		if rel, err := filepath.Rel(root, j.Folder); err == nil && rel != "" && rel != "." {
			sub = filepath.Join(w.Path, rel)
		}
		cwd = w.Path
		if _, err := os.Stat(sub); err == nil {
			cwd = sub
		}
	}

	promptHash, promptPreview := promptDigest(j.Prompt)
	entry := map[string]any{"event": evJobStart, "id": j.ID, "folder": j.Folder, "spawnMode": j.SpawnMode, "branch": j.Branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if j.actor != "" {
		entry["actor"] = j.actor
	}
	m.journal.Append(entry)
	m.mu.Lock()
	snap := j.Job
	m.mu.Unlock()
	m.announce(evJobStart, snap)

	args := []string{"-p", j.Prompt, "--output-format", "json"}
	if j.PermissionMode != "default" {
		args = append(args, "--permission-mode", j.PermissionMode)
	}

	// own process group (TS detached: true), so kills reach any children claude
	// spawned; an orphaned grandchild would otherwise hold the stdio pipes open
	// and leave the job stuck in "running" long after the kill
	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, outErr := cmd.StdoutPipe()
	stderrPipe, errErr := cmd.StderrPipe()
	startErr := outErr
	if startErr == nil {
		startErr = errErr
	}
	if startErr == nil {
		startErr = cmd.Start()
	}
	if startErr != nil {
		// spawn failure (claude not on PATH, cwd vanished) — no exit follows
		ec := 1
		msg := startErr.Error()
		m.finishJob(j, jobOutcome{state: StateFailed, exitCode: &ec, result: &msg, wt: wt, root: root})
		return
	}

	m.mu.Lock()
	j.proc = cmd
	j.timer = time.AfterFunc(time.Duration(j.TimeoutMs)*time.Millisecond, func() {
		m.mu.Lock()
		j.timedOut = true
		m.mu.Unlock()
		m.killTree(j, syscall.SIGKILL)
	})
	canceledEarly := j.cancelRequested
	m.mu.Unlock()
	if canceledEarly {
		// cancel landed while the worktree/spawn was still in flight — deliver the
		// signals it had no process to hit, or the run would execute to completion
		// while reporting "canceled"
		m.killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			m.mu.Lock()
			finished := j.FinishedAt != nil
			m.mu.Unlock()
			if !finished {
				m.killTree(j, syscall.SIGKILL)
			}
		})
	}

	var stdout, stderr strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	read := func(r io.Reader, buf *strings.Builder) {
		defer wg.Done()
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				s := string(b[:n])
				buf.WriteString(s)
				m.pushLog(j, s)
			}
			if err != nil {
				return
			}
		}
	}
	go read(stdoutPipe, &stdout)
	go read(stderrPipe, &stderr)
	wg.Wait()

	werr := cmd.Wait()
	exitCode := 0
	if werr != nil {
		// killed by a signal (no exit code) or an unexpected wait error → 1
		exitCode = 1
		if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() > 0 {
			exitCode = ee.ExitCode()
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &parsed); err != nil {
		parsed = nil // killed mid-run or non-JSON failure — raw output stays in the log
	}

	m.mu.Lock()
	canceled := j.cancelRequested
	timedOut := j.timedOut
	m.mu.Unlock()

	isError := false
	if v, ok := parsed["is_error"].(bool); ok {
		isError = v
	}
	state := StateFailed
	switch {
	case canceled:
		state = StateCanceled
	case timedOut:
		state = StateTimeout
	case exitCode == 0 && !isError:
		state = StateSucceeded
	}

	var result *string
	if v, ok := parsed["result"].(string); ok {
		result = &v
	} else if state == StateTimeout {
		msg := fmt.Sprintf("timed out after %dms", j.TimeoutMs)
		result = &msg
	} else if state == StateCanceled {
		msg := "canceled"
		result = &msg
	} else {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if last := lines[len(lines)-1]; last != "" {
			result = &last
		}
	}
	var costUsd *float64
	if v, ok := parsed["total_cost_usd"].(float64); ok {
		costUsd = &v
	}
	var durationMs, numTurns *int
	if v, ok := parsed["duration_ms"].(float64); ok {
		n := int(v)
		durationMs = &n
	}
	if v, ok := parsed["num_turns"].(float64); ok {
		n := int(v)
		numTurns = &n
	}
	m.finishJob(j, jobOutcome{
		state:      state,
		exitCode:   &exitCode,
		result:     result,
		costUsd:    costUsd,
		durationMs: durationMs,
		numTurns:   numTurns,
		wt:         wt,
		root:       root,
	})
}

func (m *Manager) pushLog(j *liveJob, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j.log = append(j.log, text)
	if len(j.log) > 400 {
		j.log = j.log[len(j.log)-400:]
	}
}

// signal the whole process group (the Setpgid spawn made the job its leader)
func (m *Manager) killTree(j *liveJob, sig syscall.Signal) {
	m.mu.Lock()
	proc := j.proc
	m.mu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	if err := syscall.Kill(-proc.Process.Pid, sig); err != nil {
		_ = proc.Process.Signal(sig) // already gone
	}
}

type jobOutcome struct {
	state      State
	result     *string
	exitCode   *int
	costUsd    *float64
	durationMs *int
	numTurns   *int
	wt         *workspace.Info
	root       string
}

func (m *Manager) finishJob(j *liveJob, outcome jobOutcome) {
	m.mu.Lock()
	if j.FinishedAt != nil { // error + close can both fire — first outcome wins
		m.mu.Unlock()
		return
	}
	j.State = outcome.state
	j.FinishedAt = util.StrPtr(util.NowISO())
	if outcome.exitCode != nil {
		j.ExitCode = outcome.exitCode
	}
	j.Result = outcome.result
	j.CostUsd = outcome.costUsd
	j.DurationMs = outcome.durationMs
	j.NumTurns = outcome.numTurns
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.proc = nil
	if outcome.wt != nil && outcome.root != "" {
		m.ws.Remove(outcome.root, outcome.wt.Path, outcome.wt.Branch, outcome.wt.BaseCommit)
	}
	m.journal.Append(map[string]any{"event": evJobExit, "id": j.ID, "state": j.State, "exitCode": j.ExitCode, "costUsd": j.CostUsd, "durationMs": j.DurationMs})
	snap := j.Job
	m.running = max(0, m.running-1)
	toStart := m.startNextLocked()
	m.mu.Unlock()
	m.announce(evJobExit, snap)
	for _, next := range toStart {
		go m.startJob(next)
	}
}

func (m *Manager) Cancel(id, actor string) *Job {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	switch j.State {
	case StateQueued:
		for i, qid := range m.queue {
			if qid == id {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
		j.cancelRequested = true
		entry := map[string]any{"event": evJobCancel, "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		m.journal.Append(entry)
		// finish() decrements the running counter it never claimed — compensate
		m.running++
		m.mu.Unlock()
		msg := "canceled while queued"
		m.finishJob(j, jobOutcome{state: StateCanceled, result: &msg})
	case StateRunning:
		j.cancelRequested = true
		entry := map[string]any{"event": evJobCancel, "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		m.journal.Append(entry)
		m.mu.Unlock()
		m.killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			m.mu.Lock()
			finished := j.FinishedAt != nil
			m.mu.Unlock()
			if !finished {
				m.killTree(j, syscall.SIGKILL)
			}
		})
	default:
		m.mu.Unlock()
	}
	m.mu.Lock()
	snap := j.Job
	m.mu.Unlock()
	return &snap // terminal states: cancel is a no-op, not an error
}

func (m *Manager) Remove(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return false, nil
	}
	if j.State == StateQueued || j.State == StateRunning {
		return false, errors.New("job is still live; cancel it first")
	}
	delete(m.jobs, id)
	return true, nil
}
