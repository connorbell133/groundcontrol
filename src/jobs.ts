import { spawn, type ChildProcess } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { basename, join, relative } from "node:path";
import { deliverWebhook, emit } from "./events.js";
import { gitRoot } from "./folders.js";
import { addWorktree, currentBranch, journal, removeWorktree } from "./workspace.js";

// Headless jobs: `claude -p` with JSON output — no PTY, no pairing, no human.
// A different resource from sessions on purpose: different lifecycle,
// different defaults (fresh worktree — an autonomous run should not dirty a
// checkout you might be standing in).

export type JobState = "queued" | "running" | "succeeded" | "failed" | "timeout" | "canceled";

export interface Job {
  id: string;
  folder: string;
  prompt: string;
  spawnMode: "same-dir" | "worktree";
  branch: string | null;
  permissionMode: string;
  timeoutMs: number;
  callbackUrl: string | null;
  state: JobState;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  exitCode: number | null;
  result: string | null; // claude's final text (or the launch error)
  costUsd: number | null;
  durationMs: number | null;
  numTurns: number | null;
  worktreePath: string | null;
}

interface LiveJob extends Job {
  proc: ChildProcess | null;
  log: string[];
  timer: ReturnType<typeof setTimeout> | null;
  timedOut: boolean;
  cancelRequested: boolean;
  actor?: string;
}

const jobs = new Map<string, LiveJob>();
const queue: string[] = [];
let running = 0;

const defaults = { concurrency: 2, timeoutMs: 15 * 60 * 1000 };

export interface JobsConfig {
  concurrency?: number;
  timeoutMs?: number;
}

export function configureJobs(cfg?: JobsConfig) {
  if (cfg?.concurrency) defaults.concurrency = Math.max(1, Math.floor(cfg.concurrency));
  if (cfg?.timeoutMs) defaults.timeoutMs = Math.max(1000, cfg.timeoutMs);
}

function publicView(j: LiveJob): Job {
  const { proc: _p, log: _l, timer: _t, timedOut: _to, cancelRequested: _c, actor: _a, ...rest } = j;
  return rest;
}

function announce(event: "job.start" | "job.exit", j: LiveJob) {
  const where = basename(j.folder) + (j.branch ? ` @ ${j.branch}` : "");
  const failed = j.state === "failed" || j.state === "timeout";
  const title =
    event === "job.start"
      ? `job started: ${where}`
      : `job ${j.state}: ${where}`;
  const message =
    event === "job.start"
      ? j.prompt.slice(0, 120)
      : j.state === "succeeded"
        ? `${(j.result ?? "").slice(0, 200)}${j.costUsd !== null ? ` ($${j.costUsd.toFixed(4)})` : ""}`
        : (j.result ?? "").slice(0, 200);
  emit(event, { job: publicView(j) }, { title, message, alsoMatch: failed ? ["job.failed"] : [] });
  if (j.callbackUrl && event === "job.exit") {
    deliverWebhook(j.callbackUrl, { event, at: new Date().toISOString(), title, message, data: { job: publicView(j) } });
  }
}

function promptDigest(prompt: string) {
  // the journal doubles as a UI surface — a hash plus a preview, never full prompts
  return { promptHash: createHash("sha256").update(prompt).digest("hex").slice(0, 12), promptPreview: prompt.slice(0, 80) };
}

export function listJobs(): Job[] {
  return [...jobs.values()].map(publicView).sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

export function getJob(id: string): Job | null {
  const j = jobs.get(id);
  return j ? publicView(j) : null;
}

export function getJobLog(id: string): string | null {
  const j = jobs.get(id);
  return j ? j.log.join("") : null;
}

export function createJob(opts: {
  folder: string;
  prompt: string;
  spawnMode?: "same-dir" | "worktree";
  branch?: string;
  permissionMode?: string;
  timeoutMs?: number;
  callbackUrl?: string;
  actor?: string;
}): Job {
  const root = gitRoot(opts.folder);
  // jobs default to a fresh worktree wherever git makes one possible
  const spawnMode: Job["spawnMode"] =
    opts.spawnMode === "same-dir" ? "same-dir" : opts.spawnMode === "worktree" || root ? "worktree" : "same-dir";
  let branch: string | null = null;
  if (spawnMode === "worktree") {
    if (!root) throw new Error("worktree mode requires a folder inside a git repository");
    branch = opts.branch ?? currentBranch(root);
    if (!branch) throw new Error("no branch given and the repository is detached — pass branch explicitly");
  }

  const job: LiveJob = {
    id: randomUUID().slice(0, 8),
    folder: opts.folder,
    prompt: opts.prompt,
    spawnMode,
    branch,
    permissionMode: opts.permissionMode || "acceptEdits",
    timeoutMs: Math.min(2 * 60 * 60 * 1000, Math.max(1000, opts.timeoutMs ?? defaults.timeoutMs)),
    callbackUrl: opts.callbackUrl ?? null,
    state: "queued",
    createdAt: new Date().toISOString(),
    startedAt: null,
    finishedAt: null,
    exitCode: null,
    result: null,
    costUsd: null,
    durationMs: null,
    numTurns: null,
    worktreePath: null,
    proc: null,
    log: [],
    timer: null,
    timedOut: false,
    cancelRequested: false,
    actor: opts.actor,
  };
  jobs.set(job.id, job);
  queue.push(job.id);
  journal({ event: "job.queued", id: job.id, folder: job.folder, spawnMode, branch, ...promptDigest(job.prompt), actor: opts.actor });
  startNext();
  return publicView(job);
}

function startNext() {
  while (running < defaults.concurrency && queue.length) {
    const id = queue.shift()!;
    const j = jobs.get(id);
    if (!j || j.state !== "queued") continue; // canceled while queued
    running++;
    start(j);
  }
}

function start(j: LiveJob) {
  j.state = "running";
  j.startedAt = new Date().toISOString();

  let cwd = j.folder;
  let wt: { wtPath: string; wtBranch: string; baseCommit: string } | null = null;
  let root: string | null = null;
  if (j.spawnMode === "worktree") {
    root = gitRoot(j.folder)!;
    try {
      wt = addWorktree(root, j.branch!, j.id);
    } catch (err) {
      finish(j, { state: "failed", result: (err as Error).message });
      return;
    }
    j.worktreePath = wt.wtPath;
    const rel = relative(root, j.folder);
    const sub = rel ? join(wt.wtPath, rel) : wt.wtPath;
    cwd = existsSync(sub) ? sub : wt.wtPath;
  }

  journal({ event: "job.start", id: j.id, folder: j.folder, spawnMode: j.spawnMode, branch: j.branch, ...promptDigest(j.prompt), actor: j.actor });
  announce("job.start", j);

  const args = ["-p", j.prompt, "--output-format", "json"];
  if (j.permissionMode !== "default") args.push("--permission-mode", j.permissionMode);

  // detached → own process group, so kills reach any children claude spawned;
  // an orphaned grandchild would otherwise hold the stdio pipes open and leave
  // the job stuck in "running" long after the kill
  const proc = spawn("claude", args, {
    cwd,
    env: { ...process.env, FORCE_COLOR: "0", NO_COLOR: "1" },
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  j.proc = proc;

  let stdout = "";
  let stderr = "";
  proc.stdout.on("data", (c: Buffer) => {
    stdout += c;
    pushLog(j, c.toString());
  });
  proc.stderr.on("data", (c: Buffer) => {
    stderr += c;
    pushLog(j, c.toString());
  });
  proc.on("error", (e) => {
    // spawn failure (claude not on PATH, cwd vanished) — no close event follows
    finish(j, { state: "failed", exitCode: 1, result: e.message, wt, root });
  });
  proc.on("close", (code) => {
    const exitCode = code ?? 1;
    let parsed: Record<string, unknown> | null = null;
    try {
      parsed = JSON.parse(stdout);
    } catch {
      /* killed mid-run or non-JSON failure — raw output stays in the log */
    }
    const state: JobState = j.cancelRequested
      ? "canceled"
      : j.timedOut
        ? "timeout"
        : exitCode === 0 && !parsed?.is_error
          ? "succeeded"
          : "failed";
    finish(j, {
      state,
      exitCode,
      result:
        (parsed?.result as string | undefined) ??
        (state === "timeout" ? `timed out after ${j.timeoutMs}ms` : state === "canceled" ? "canceled" : stderr.trim().split("\n").pop() || null),
      costUsd: (parsed?.total_cost_usd as number | undefined) ?? null,
      durationMs: (parsed?.duration_ms as number | undefined) ?? null,
      numTurns: (parsed?.num_turns as number | undefined) ?? null,
      wt,
      root,
    });
  });

  j.timer = setTimeout(() => {
    j.timedOut = true;
    killTree(j, "SIGKILL");
  }, j.timeoutMs);
}

function pushLog(j: LiveJob, text: string) {
  j.log.push(text);
  if (j.log.length > 400) j.log.splice(0, j.log.length - 400);
}

// signal the whole process group (detached spawn made the job its leader)
function killTree(j: LiveJob, sig: NodeJS.Signals) {
  const pid = j.proc?.pid;
  if (!pid) return;
  try {
    process.kill(-pid, sig);
  } catch {
    try {
      j.proc?.kill(sig);
    } catch {
      /* already gone */
    }
  }
}

function finish(
  j: LiveJob,
  outcome: {
    state: JobState;
    result: string | null;
    exitCode?: number | null;
    costUsd?: number | null;
    durationMs?: number | null;
    numTurns?: number | null;
    wt?: { wtPath: string; wtBranch: string; baseCommit: string } | null;
    root?: string | null;
  }
) {
  if (j.finishedAt) return; // error + close can both fire — first outcome wins
  j.state = outcome.state;
  j.finishedAt = new Date().toISOString();
  j.exitCode = outcome.exitCode ?? j.exitCode;
  j.result = outcome.result;
  j.costUsd = outcome.costUsd ?? null;
  j.durationMs = outcome.durationMs ?? null;
  j.numTurns = outcome.numTurns ?? null;
  j.proc = null;
  if (outcome.wt && outcome.root) {
    removeWorktree(outcome.root, outcome.wt.wtPath, outcome.wt.wtBranch, outcome.wt.baseCommit);
  }
  journal({ event: "job.exit", id: j.id, state: j.state, exitCode: j.exitCode, costUsd: j.costUsd, durationMs: j.durationMs });
  announce("job.exit", j);
  running = Math.max(0, running - 1);
  startNext();
}

export function cancelJob(id: string, actor?: string): Job | null {
  const j = jobs.get(id);
  if (!j) return null;
  if (j.state === "queued") {
    const idx = queue.indexOf(id);
    if (idx >= 0) queue.splice(idx, 1);
    j.cancelRequested = true;
    journal({ event: "job.cancel", id, actor });
    // finish() decrements the running counter it never claimed — compensate
    running++;
    finish(j, { state: "canceled", result: "canceled while queued" });
  } else if (j.state === "running") {
    j.cancelRequested = true;
    journal({ event: "job.cancel", id, actor });
    killTree(j, "SIGTERM");
    setTimeout(() => {
      if (!j.finishedAt) killTree(j, "SIGKILL");
    }, 5000).unref();
  }
  return publicView(j); // terminal states: cancel is a no-op, not an error
}

export function removeJob(id: string): boolean {
  const j = jobs.get(id);
  if (!j) return false;
  if (j.state === "queued" || j.state === "running") throw new Error("job is still live; cancel it first");
  return jobs.delete(id);
}
