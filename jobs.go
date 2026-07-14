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

type Job struct {
	ID             string   `json:"id"`
	Folder         string   `json:"folder"`
	Prompt         string   `json:"prompt"`
	SpawnMode      string   `json:"spawnMode"` // "same-dir" | "worktree"
	Branch         *string  `json:"branch"`
	PermissionMode string   `json:"permissionMode"`
	TimeoutMs      int      `json:"timeoutMs"`
	CallbackURL    *string  `json:"callbackUrl"`
	State          string   `json:"state"` // queued|running|succeeded|failed|timeout|canceled
	CreatedAt      string   `json:"createdAt"`
	StartedAt      *string  `json:"startedAt"`
	FinishedAt     *string  `json:"finishedAt"`
	ExitCode       *int     `json:"exitCode"`
	Result         *string  `json:"result"` // claude's final text (or the launch error)
	CostUsd        *float64 `json:"costUsd"`
	DurationMs     *int     `json:"durationMs"`
	NumTurns       *int     `json:"numTurns"`
	WorktreePath   *string  `json:"worktreePath"`
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

var (
	jobsMu      sync.Mutex
	jobs        = map[string]*liveJob{}
	jobQueue    []string
	jobsRunning int
	jobDefaults = struct{ concurrency, timeoutMs int }{concurrency: 2, timeoutMs: 15 * 60 * 1000}
)

type JobsConfig struct {
	Concurrency int `json:"concurrency,omitempty"`
	TimeoutMs   int `json:"timeoutMs,omitempty"`
}

func configureJobs(cfg *JobsConfig) {
	if cfg == nil {
		return
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if cfg.Concurrency != 0 {
		jobDefaults.concurrency = max(1, cfg.Concurrency)
	}
	if cfg.TimeoutMs != 0 {
		jobDefaults.timeoutMs = max(1000, cfg.TimeoutMs)
	}
}

func announceJob(event string, j Job) {
	where := filepath.Base(j.Folder)
	if j.Branch != nil && *j.Branch != "" {
		where += " @ " + *j.Branch
	}
	failed := j.State == "failed" || j.State == "timeout"
	var title string
	if event == "job.start" {
		title = "job started: " + where
	} else {
		title = "job " + j.State + ": " + where
	}
	result := ""
	if j.Result != nil {
		result = *j.Result
	}
	var message string
	switch {
	case event == "job.start":
		message = firstRunes(j.Prompt, 120)
	case j.State == "succeeded":
		message = firstRunes(result, 200)
		if j.CostUsd != nil {
			message += fmt.Sprintf(" ($%.4f)", *j.CostUsd)
		}
	default:
		message = firstRunes(result, 200)
	}
	var alsoMatch []string
	if failed {
		alsoMatch = []string{"job.failed"}
	}
	emit(event, map[string]any{"job": j}, emitOpts{title: title, message: message, alsoMatch: alsoMatch})
	if j.CallbackURL != nil && event == "job.exit" {
		deliverWebhook(*j.CallbackURL, LifecycleEvent{Event: event, At: nowISO(), Title: title, Message: message, Data: map[string]any{"job": j}})
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

func listJobs() []Job {
	jobsMu.Lock()
	out := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Job)
	}
	jobsMu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt > out[b].CreatedAt })
	return out
}

func getJob(id string) *Job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, ok := jobs[id]
	if !ok {
		return nil
	}
	snap := j.Job
	return &snap
}

func getJobLog(id string) *string {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, ok := jobs[id]
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

func createJob(opts createJobOpts) (Job, error) {
	root := gitRoot(opts.folder)
	// jobs default to a fresh worktree wherever git makes one possible
	spawnMode := "same-dir"
	if opts.spawnMode != "same-dir" && (opts.spawnMode == "worktree" || root != "") {
		spawnMode = "worktree"
	}
	var branch *string
	if spawnMode == "worktree" {
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
	jobsMu.Lock()
	timeoutMs := opts.timeoutMs
	if timeoutMs < 0 { // absent — an explicit 0 clamps to the 1s floor, like the TS
		timeoutMs = jobDefaults.timeoutMs
	}
	timeoutMs = min(2*60*60*1000, max(1000, timeoutMs))

	j := &liveJob{
		Job: Job{
			ID:             randomID(8),
			Folder:         opts.folder,
			Prompt:         opts.prompt,
			SpawnMode:      spawnMode,
			Branch:         branch,
			PermissionMode: permissionMode,
			TimeoutMs:      timeoutMs,
			CallbackURL:    strPtr(opts.callbackURL),
			State:          "queued",
			CreatedAt:      nowISO(),
		},
		actor: opts.actor,
	}
	jobs[j.ID] = j
	jobQueue = append(jobQueue, j.ID)
	jobsMu.Unlock()

	promptHash, promptPreview := promptDigest(j.Prompt)
	entry := map[string]any{"event": "job.queued", "id": j.ID, "folder": j.Folder, "spawnMode": spawnMode, "branch": branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if opts.actor != "" {
		entry["actor"] = opts.actor
	}
	journal(entry)
	startNext()

	jobsMu.Lock()
	snap := j.Job
	jobsMu.Unlock()
	return snap, nil
}

// caller holds jobsMu; returns the jobs whose queue slot was just claimed —
// the caller launches them after releasing the lock
func startNextLocked() []*liveJob {
	var toStart []*liveJob
	for jobsRunning < jobDefaults.concurrency && len(jobQueue) > 0 {
		id := jobQueue[0]
		jobQueue = jobQueue[1:]
		j, ok := jobs[id]
		if !ok || j.State != "queued" {
			continue // canceled while queued
		}
		jobsRunning++
		j.State = "running"
		j.StartedAt = strPtr(nowISO())
		toStart = append(toStart, j)
	}
	return toStart
}

func startNext() {
	jobsMu.Lock()
	toStart := startNextLocked()
	jobsMu.Unlock()
	for _, j := range toStart {
		go startJob(j)
	}
}

func startJob(j *liveJob) {
	cwd := j.Folder
	var wt *worktreeInfo
	root := ""
	if j.SpawnMode == "worktree" {
		root = gitRoot(j.Folder)
		w, err := addWorktree(root, *j.Branch, j.ID)
		if err != nil {
			msg := err.Error()
			finishJob(j, jobOutcome{state: "failed", result: &msg})
			return
		}
		wt = &w
		jobsMu.Lock()
		j.WorktreePath = strPtr(w.wtPath)
		jobsMu.Unlock()
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
	entry := map[string]any{"event": "job.start", "id": j.ID, "folder": j.Folder, "spawnMode": j.SpawnMode, "branch": j.Branch, "promptHash": promptHash, "promptPreview": promptPreview}
	if j.actor != "" {
		entry["actor"] = j.actor
	}
	journal(entry)
	jobsMu.Lock()
	snap := j.Job
	jobsMu.Unlock()
	announceJob("job.start", snap)

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
		finishJob(j, jobOutcome{state: "failed", exitCode: &ec, result: &msg, wt: wt, root: root})
		return
	}

	jobsMu.Lock()
	j.proc = cmd
	j.timer = time.AfterFunc(time.Duration(j.TimeoutMs)*time.Millisecond, func() {
		jobsMu.Lock()
		j.timedOut = true
		jobsMu.Unlock()
		killTree(j, syscall.SIGKILL)
	})
	canceledEarly := j.cancelRequested
	jobsMu.Unlock()
	if canceledEarly {
		// cancel landed while the worktree/spawn was still in flight — deliver the
		// signals it had no process to hit, or the run would execute to completion
		// while reporting "canceled"
		killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			jobsMu.Lock()
			finished := j.FinishedAt != nil
			jobsMu.Unlock()
			if !finished {
				killTree(j, syscall.SIGKILL)
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
				pushJobLog(j, s)
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

	jobsMu.Lock()
	canceled := j.cancelRequested
	timedOut := j.timedOut
	jobsMu.Unlock()

	isError := false
	if v, ok := parsed["is_error"].(bool); ok {
		isError = v
	}
	state := "failed"
	switch {
	case canceled:
		state = "canceled"
	case timedOut:
		state = "timeout"
	case exitCode == 0 && !isError:
		state = "succeeded"
	}

	var result *string
	if v, ok := parsed["result"].(string); ok {
		result = &v
	} else if state == "timeout" {
		msg := fmt.Sprintf("timed out after %dms", j.TimeoutMs)
		result = &msg
	} else if state == "canceled" {
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
	finishJob(j, jobOutcome{
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

func pushJobLog(j *liveJob, text string) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j.log = append(j.log, text)
	if len(j.log) > 400 {
		j.log = j.log[len(j.log)-400:]
	}
}

// signal the whole process group (the Setpgid spawn made the job its leader)
func killTree(j *liveJob, sig syscall.Signal) {
	jobsMu.Lock()
	proc := j.proc
	jobsMu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	if err := syscall.Kill(-proc.Process.Pid, sig); err != nil {
		_ = proc.Process.Signal(sig) // already gone
	}
}

type jobOutcome struct {
	state      string
	result     *string
	exitCode   *int
	costUsd    *float64
	durationMs *int
	numTurns   *int
	wt         *worktreeInfo
	root       string
}

func finishJob(j *liveJob, outcome jobOutcome) {
	jobsMu.Lock()
	if j.FinishedAt != nil { // error + close can both fire — first outcome wins
		jobsMu.Unlock()
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
		removeWorktree(outcome.root, outcome.wt.wtPath, outcome.wt.wtBranch, outcome.wt.baseCommit)
	}
	journal(map[string]any{"event": "job.exit", "id": j.ID, "state": j.State, "exitCode": j.ExitCode, "costUsd": j.CostUsd, "durationMs": j.DurationMs})
	snap := j.Job
	jobsRunning = max(0, jobsRunning-1)
	toStart := startNextLocked()
	jobsMu.Unlock()
	announceJob("job.exit", snap)
	for _, next := range toStart {
		go startJob(next)
	}
}

func cancelJob(id, actor string) *Job {
	jobsMu.Lock()
	j, ok := jobs[id]
	if !ok {
		jobsMu.Unlock()
		return nil
	}
	switch j.State {
	case "queued":
		for i, qid := range jobQueue {
			if qid == id {
				jobQueue = append(jobQueue[:i], jobQueue[i+1:]...)
				break
			}
		}
		j.cancelRequested = true
		entry := map[string]any{"event": "job.cancel", "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		journal(entry)
		// finish() decrements the running counter it never claimed — compensate
		jobsRunning++
		jobsMu.Unlock()
		msg := "canceled while queued"
		finishJob(j, jobOutcome{state: "canceled", result: &msg})
	case "running":
		j.cancelRequested = true
		entry := map[string]any{"event": "job.cancel", "id": id}
		if actor != "" {
			entry["actor"] = actor
		}
		journal(entry)
		jobsMu.Unlock()
		killTree(j, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			jobsMu.Lock()
			finished := j.FinishedAt != nil
			jobsMu.Unlock()
			if !finished {
				killTree(j, syscall.SIGKILL)
			}
		})
	default:
		jobsMu.Unlock()
	}
	jobsMu.Lock()
	snap := j.Job
	jobsMu.Unlock()
	return &snap // terminal states: cancel is a no-op, not an error
}

func removeJob(id string) (bool, error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	j, ok := jobs[id]
	if !ok {
		return false, nil
	}
	if j.State == "queued" || j.State == "running" {
		return false, errors.New("job is still live; cancel it first")
	}
	delete(jobs, id)
	return true, nil
}
