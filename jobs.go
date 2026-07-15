package main

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
)

// Headless jobs: `claude -p` with JSON output — no PTY, no pairing, no human.
// A different resource from sessions on purpose: different lifecycle,
// different defaults (fresh worktree — an autonomous run should not dirty a
// checkout you might be standing in).

// jobState keeps string underneath: JSON marshals identically, and the untyped
// literals other files compare against still compile
type jobState string

const (
	jobStateQueued    jobState = "queued"
	jobStateRunning   jobState = "running"
	jobStateSucceeded jobState = "succeeded"
	jobStateFailed    jobState = "failed"
	jobStateTimeout   jobState = "timeout"
	jobStateCanceled  jobState = "canceled"
)

// job lifecycle event names; evJobFailed is a derived match token (announceJob
// adds it alongside a failed/timeout exit), never an emitted event itself
const (
	evJobQueued = "job.queued"
	evJobStart  = "job.start"
	evJobExit   = "job.exit"
	evJobCancel = "job.cancel"
	evJobFailed = "job.failed"
)

type Job struct {
	ID             string    `json:"id"`
	Folder         string    `json:"folder"`
	Prompt         string    `json:"prompt"`
	SpawnMode      spawnMode `json:"spawnMode"`
	Branch         *string   `json:"branch"`
	PermissionMode string    `json:"permissionMode"`
	TimeoutMs      int       `json:"timeoutMs"`
	CallbackURL    *string   `json:"callbackUrl"`
	State          jobState  `json:"state"`
	CreatedAt      string    `json:"createdAt"`
	StartedAt      *string   `json:"startedAt"`
	FinishedAt     *string   `json:"finishedAt"`
	ExitCode       *int      `json:"exitCode"`
	Result         *string   `json:"result"` // claude's final text (or the launch error)
	CostUsd        *float64  `json:"costUsd"`
	DurationMs     *int      `json:"durationMs"`
	NumTurns       *int      `json:"numTurns"`
	WorktreePath   *string   `json:"worktreePath"`
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

type JobsConfig struct {
	Concurrency int `json:"concurrency,omitempty"`
	TimeoutMs   int `json:"timeoutMs,omitempty"`
}

func (a *app) configureJobs(cfg *JobsConfig) {
	if cfg == nil {
		return
	}
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	if cfg.Concurrency != 0 {
		a.jobDefaults.concurrency = max(1, cfg.Concurrency)
	}
	if cfg.TimeoutMs != 0 {
		a.jobDefaults.timeoutMs = max(1000, cfg.TimeoutMs)
	}
}

func (a *app) announceJob(event string, j Job) {
	where := filepath.Base(j.Folder)
	if j.Branch != nil && *j.Branch != "" {
		where += " @ " + *j.Branch
	}
	failed := j.State == jobStateFailed || j.State == jobStateTimeout
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
		message = firstRunes(j.Prompt, 120)
	case j.State == jobStateSucceeded:
		message = firstRunes(result, 200)
		if j.CostUsd != nil {
			message += fmt.Sprintf(" ($%.4f)", *j.CostUsd)
		}
	default:
		message = firstRunes(result, 200)
	}
	var alsoMatch []string
	if failed {
		alsoMatch = []string{evJobFailed}
	}
	a.emit(event, map[string]any{"job": j}, emitOpts{title: title, message: message, alsoMatch: alsoMatch})
	if j.CallbackURL != nil && event == evJobExit {
		a.deliverWebhook(*j.CallbackURL, LifecycleEvent{Event: event, At: nowISO(), Title: title, Message: message, Data: map[string]any{"job": j}})
	}
}

// the journal doubles as a UI surface — a hash plus a preview, never full prompts
func promptDigest(prompt string) (promptHash, promptPreview string) {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])[:12], firstRunes(prompt, 80)
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (a *app) listJobs() []Job {
	a.jobsMu.Lock()
	out := make([]Job, 0, len(a.jobs))
	for _, j := range a.jobs {
		out = append(out, j.Job)
	}
	a.jobsMu.Unlock()
	sort.Slice(out, func(x, y int) bool { return out[x].CreatedAt > out[y].CreatedAt })
	return out
}

func (a *app) getJob(id string) *Job {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j, ok := a.jobs[id]
	if !ok {
		return nil
	}
	snap := j.Job
	return &snap
}

func (a *app) getJobLog(id string) *string {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j, ok := a.jobs[id]
	if !ok {
		return nil
	}
	joined := strings.Join(j.log, "")
	return &joined
}

type createJobOpts struct {
	folder, prompt, spawnMode, branch, permissionMode, callbackURL, actor string
	timeoutMs                                                             int // <0 = absent, use default
}

func (a *app) createJob(opts createJobOpts) (Job, error) {
	root := gitRoot(opts.folder)
	// jobs default to a fresh worktree wherever git makes one possible;
	// opts carries the raw request string — everything past here is typed
	mode := spawnSameDir
	if opts.spawnMode != string(spawnSameDir) && (opts.spawnMode == string(spawnWorktree) || root != "") {
		mode = spawnWorktree
	}
	var branch *string
	if mode == spawnWorktree {
		if root == "" {
			return Job{}, errors.New("worktree mode requires a folder inside a git repository")
		}
		b := opts.branch
		if b == "" {
			b = currentBranch(root)
		}
		if b == "" {
			return Job{}, errors.New("no branch given and the repository is detached — pass branch explicitly")
		}
		branch = &b
	}

	permissionMode := opts.permissionMode
	if permissionMode == "" {
		permissionMode = "acceptEdits"
	}
	a.jobsMu.Lock()
	timeoutMs := opts.timeoutMs
	if timeoutMs < 0 { // absent — an explicit 0 clamps to the 1s floor, like the TS
		timeoutMs = a.jobDefaults.timeoutMs
	}
	timeoutMs = min(2*60*60*1000, max(1000, timeoutMs))

	j := &liveJob{
		Job: Job{
			ID:             randomID(8),
			Folder:         opts.folder,
			Prompt:         opts.prompt,
			SpawnMode:      mode,
			Branch:         branch,
			PermissionMode: permissionMode,
			TimeoutMs:      timeoutMs,
			CallbackURL:    strPtr(opts.callbackURL),
			State:          jobStateQueued,
			CreatedAt:      nowISO(),
		},
		actor: opts.actor,
	}
	a.jobs[j.ID] = j
	a.jobQueue = append(a.jobQueue, j.ID)
	a.jobsMu.Unlock()

	promptHash, promptPreview := promptDigest(j.Prompt)
	entry := map[string]any{"event": evJobQueued, "id": j.ID, "folder": j.Folder, "spawnMode": mode, "branch": branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if opts.actor != "" {
		entry["actor"] = opts.actor
	}
	a.journal(entry)
	a.startNext()

	a.jobsMu.Lock()
	snap := j.Job
	a.jobsMu.Unlock()
	return snap, nil
}

// caller holds jobsMu; returns the jobs whose queue slot was just claimed —
// the caller launches them after releasing the lock
func (a *app) startNextLocked() []*liveJob {
	var toStart []*liveJob
	for a.jobsRunning < a.jobDefaults.concurrency && len(a.jobQueue) > 0 {
		id := a.jobQueue[0]
		a.jobQueue = a.jobQueue[1:]
		j, ok := a.jobs[id]
		if !ok || j.State != jobStateQueued {
			continue // canceled while queued
		}
		a.jobsRunning++
		j.State = jobStateRunning
		j.StartedAt = strPtr(nowISO())
		toStart = append(toStart, j)
	}
	return toStart
}

func (a *app) startNext() {
	a.jobsMu.Lock()
	toStart := a.startNextLocked()
	a.jobsMu.Unlock()
	for _, j := range toStart {
		go a.startJob(j)
	}
}

func (a *app) startJob(j *liveJob) {
	cwd := j.Folder
	var wt *worktreeInfo
	root := ""
	if j.SpawnMode == spawnWorktree {
		root = gitRoot(j.Folder)
		w, err := a.addWorktree(root, *j.Branch, j.ID, j.Prompt)
		if err != nil {
			msg := err.Error()
			a.finishJob(j, jobOutcome{state: jobStateFailed, result: &msg})
			return
		}
		wt = &w
		a.jobsMu.Lock()
		j.WorktreePath = strPtr(w.wtPath)
		a.jobsMu.Unlock()
		sub := w.wtPath
		if rel, err := filepath.Rel(root, j.Folder); err == nil && rel != "" && rel != "." {
			sub = filepath.Join(w.wtPath, rel)
		}
		cwd = w.wtPath
		if _, err := os.Stat(sub); err == nil {
			cwd = sub
		}
	}

	promptHash, promptPreview := promptDigest(j.Prompt)
	entry := map[string]any{"event": evJobStart, "id": j.ID, "folder": j.Folder, "spawnMode": j.SpawnMode, "branch": j.Branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if j.actor != "" {
		entry["actor"] = j.actor
	}
	a.journal(entry)
	a.jobsMu.Lock()
	snap := j.Job
	a.jobsMu.Unlock()
	a.announceJob(evJobStart, snap)

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
		a.finishJob(j, jobOutcome{state: jobStateFailed, exitCode: &ec, result: &msg, wt: wt, root: root})
		return
	}

	a.jobsMu.Lock()
	j.proc = cmd
	j.timer = time.AfterFunc(time.Duration(j.TimeoutMs)*time.Millisecond, func() {
		a.jobsMu.Lock()
		j.timedOut = true
		a.jobsMu.Unlock()
		a.killTree(j, syscall.SIGKILL)
	})
	canceledEarly := j.cancelRequested
	a.jobsMu.Unlock()
	if canceledEarly {
		// cancel landed while the worktree/spawn was still in flight — deliver the
		// signals it had no process to hit, or the run would execute to completion
		// while reporting "canceled"
		a.killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			a.jobsMu.Lock()
			finished := j.FinishedAt != nil
			a.jobsMu.Unlock()
			if !finished {
				a.killTree(j, syscall.SIGKILL)
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
				a.pushJobLog(j, s)
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

	a.jobsMu.Lock()
	canceled := j.cancelRequested
	timedOut := j.timedOut
	a.jobsMu.Unlock()

	isError := false
	if v, ok := parsed["is_error"].(bool); ok {
		isError = v
	}
	state := jobStateFailed
	switch {
	case canceled:
		state = jobStateCanceled
	case timedOut:
		state = jobStateTimeout
	case exitCode == 0 && !isError:
		state = jobStateSucceeded
	}

	var result *string
	if v, ok := parsed["result"].(string); ok {
		result = &v
	} else if state == jobStateTimeout {
		msg := fmt.Sprintf("timed out after %dms", j.TimeoutMs)
		result = &msg
	} else if state == jobStateCanceled {
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
	a.finishJob(j, jobOutcome{
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

func (a *app) pushJobLog(j *liveJob, text string) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j.log = append(j.log, text)
	if len(j.log) > 400 {
		j.log = j.log[len(j.log)-400:]
	}
}

// signal the whole process group (the Setpgid spawn made the job its leader)
func (a *app) killTree(j *liveJob, sig syscall.Signal) {
	a.jobsMu.Lock()
	proc := j.proc
	a.jobsMu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	if err := syscall.Kill(-proc.Process.Pid, sig); err != nil {
		_ = proc.Process.Signal(sig) // already gone
	}
}

type jobOutcome struct {
	state      jobState
	result     *string
	exitCode   *int
	costUsd    *float64
	durationMs *int
	numTurns   *int
	wt         *worktreeInfo
	root       string
}

func (a *app) finishJob(j *liveJob, outcome jobOutcome) {
	a.jobsMu.Lock()
	if j.FinishedAt != nil { // error + close can both fire — first outcome wins
		a.jobsMu.Unlock()
		return
	}
	j.State = outcome.state
	j.FinishedAt = strPtr(nowISO())
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
		a.removeWorktree(outcome.root, outcome.wt.wtPath, outcome.wt.wtBranch, outcome.wt.baseCommit)
	}
	a.journal(map[string]any{"event": evJobExit, "id": j.ID, "state": j.State, "exitCode": j.ExitCode, "costUsd": j.CostUsd, "durationMs": j.DurationMs})
	snap := j.Job
	a.jobsRunning = max(0, a.jobsRunning-1)
	toStart := a.startNextLocked()
	a.jobsMu.Unlock()
	a.announceJob(evJobExit, snap)
	for _, next := range toStart {
		go a.startJob(next)
	}
}

func (a *app) cancelJob(id, actor string) *Job {
	a.jobsMu.Lock()
	j, ok := a.jobs[id]
	if !ok {
		a.jobsMu.Unlock()
		return nil
	}
	switch j.State {
	case jobStateQueued:
		for i, qid := range a.jobQueue {
			if qid == id {
				a.jobQueue = append(a.jobQueue[:i], a.jobQueue[i+1:]...)
				break
			}
		}
		j.cancelRequested = true
		entry := map[string]any{"event": evJobCancel, "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		a.journal(entry)
		// finish() decrements the running counter it never claimed — compensate
		a.jobsRunning++
		a.jobsMu.Unlock()
		msg := "canceled while queued"
		a.finishJob(j, jobOutcome{state: jobStateCanceled, result: &msg})
	case jobStateRunning:
		j.cancelRequested = true
		entry := map[string]any{"event": evJobCancel, "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		a.journal(entry)
		a.jobsMu.Unlock()
		a.killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			a.jobsMu.Lock()
			finished := j.FinishedAt != nil
			a.jobsMu.Unlock()
			if !finished {
				a.killTree(j, syscall.SIGKILL)
			}
		})
	default:
		a.jobsMu.Unlock()
	}
	a.jobsMu.Lock()
	snap := j.Job
	a.jobsMu.Unlock()
	return &snap // terminal states: cancel is a no-op, not an error
}

func (a *app) removeJob(id string) (bool, error) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j, ok := a.jobs[id]
	if !ok {
		return false, nil
	}
	if j.State == jobStateQueued || j.State == jobStateRunning {
		return false, errors.New("job is still live; cancel it first")
	}
	delete(a.jobs, id)
	return true, nil
}
