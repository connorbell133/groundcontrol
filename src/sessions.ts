import { spawn as ptySpawn, type IPty } from "node-pty";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { basename, join, relative } from "node:path";
import { randomUUID } from "node:crypto";
import { gitRoot, withinRoots } from "./folders.js";

export type SessionState = "starting" | "ready" | "exited" | "error";

export interface Session {
  id: string;
  name: string;
  folder: string;
  spawnMode: "same-dir" | "worktree";
  branch: string | null;
  worktreePath: string | null;
  permissionMode: string;
  state: SessionState;
  pairingUrl: string | null;
  startedAt: string;
  exitedAt: string | null;
  exitCode: number | null;
  lastOutputAt: string | null;
  lastLine: string | null;
}

interface LiveSession extends Session {
  proc: IPty | null;
  log: string[];
  killed: boolean;
}

const sessions = new Map<string, LiveSession>();
const DATA_DIR = join(process.cwd(), "data");
const JOURNAL = join(DATA_DIR, "journal.json");
const ANSI = /\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07?/g;
const URL_RE = /https:\/\/[^\s"'\x1b]+/;
// box-drawing, blocks, braille spinners, and ASCII rule/spinner chars — lines of only these are visual noise
const JUNK = /[─-╿▀-▟⠀-⣿|\\/·•●◐◓◑◒~\-_=+*.\s]/g;

function lastMeaningfulLine(log: string[]): string | null {
  const tail = log.slice(-8).join("");
  const lines = tail.split(/[\r\n]+/);
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (line && /[A-Za-z0-9]/.test(line.replace(JUNK, ""))) return line.slice(0, 120).trim();
  }
  return null;
}

function journal(entry: Record<string, unknown>) {
  mkdirSync(DATA_DIR, { recursive: true });
  let all: unknown[] = [];
  try {
    all = JSON.parse(readFileSync(JOURNAL, "utf8"));
  } catch {
    /* first write */
  }
  all.push({ ...entry, at: new Date().toISOString() });
  writeFileSync(JOURNAL, JSON.stringify(all, null, 2));
}

function publicView(s: LiveSession): Session {
  const { proc: _proc, log: _log, killed: _killed, ...rest } = s;
  return rest;
}

/* ---------- ntfy ---------- */
export interface NtfyConfig {
  url: string;
  topic: string;
  notifyReady: boolean;
  notifyExit: "errors" | "all" | "never";
}

let ntfy: NtfyConfig | undefined;

export function configureNtfy(cfg?: NtfyConfig) {
  ntfy = cfg;
}

function notify(opts: { title: string; body: string; priority?: string; tags?: string; click?: string }) {
  if (!ntfy?.topic) return;
  try {
    const headers: Record<string, string> = { Title: opts.title.replace(/[^\x20-\x7e]/g, "?") };
    if (opts.priority) headers.Priority = opts.priority;
    if (opts.tags) headers.Tags = opts.tags;
    if (opts.click) headers.Click = opts.click;
    fetch(`${ntfy.url}/${ntfy.topic}`, {
      method: "POST",
      body: opts.body,
      headers,
      signal: AbortSignal.timeout(5000),
    }).catch(() => {});
  } catch {
    /* notification failure must never break the session lifecycle */
  }
}

/* ---------- journal queries ---------- */
export interface RecentLaunch {
  folder: string;
  name: string;
  branch: string | null;
  spawnMode: string;
  permissionMode: string;
  at: string;
  stale: boolean; // worktree config whose branch no longer exists
}

function branchExists(folder: string, branch: string): boolean {
  try {
    execFileSync("git", ["-C", folder, "rev-parse", "--verify", "--quiet", `refs/heads/${branch}`], {
      stdio: "ignore",
      timeout: 2000,
    });
    return true;
  } catch {
    return false;
  }
}

export interface LostSession extends RecentLaunch {
  id: string;
}

const LOST_WINDOW_MS = 7 * 24 * 60 * 60 * 1000;

function readJournal(): Record<string, unknown>[] {
  try {
    const all = JSON.parse(readFileSync(JOURNAL, "utf8"));
    return Array.isArray(all) ? all.slice(-2000) : [];
  } catch {
    return [];
  }
}

export function recentLaunches(limit: number): RecentLaunch[] {
  const seen = new Set<string>();
  const out: RecentLaunch[] = [];
  const entries = readJournal();
  for (let i = entries.length - 1; i >= 0 && out.length < limit; i--) {
    const e = entries[i] as Partial<RecentLaunch> & { event?: string };
    if (e.event !== "session.start" || !e.folder) continue;
    if (!existsSync(e.folder) || !withinRoots(e.folder)) continue;
    const key = `${e.folder}\x00${e.branch ?? ""}\x00${e.spawnMode}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const branch = e.branch ?? null;
    const spawnMode = e.spawnMode ?? "same-dir";
    out.push({
      folder: e.folder,
      name: e.name ?? "",
      branch,
      spawnMode,
      permissionMode: e.permissionMode ?? "default",
      at: e.at ?? "",
      stale: spawnMode === "worktree" && !!branch && !branchExists(e.folder, branch),
    });
  }
  return out;
}

let lostCache: LostSession[] | null = null;

export function listLostSessions(): LostSession[] {
  if (lostCache) return lostCache;
  const entries = readJournal();
  const terminated = new Set<string>();
  for (const e of entries as { event?: string; id?: string }[]) {
    if ((e.event === "session.exit" || e.event === "session.kill") && e.id) terminated.add(e.id);
  }
  const cutoff = Date.now() - LOST_WINDOW_MS;
  const out: LostSession[] = [];
  for (const e of entries as (Partial<LostSession> & { event?: string })[]) {
    if (e.event !== "session.start" || !e.id || !e.folder) continue;
    if (terminated.has(e.id) || sessions.has(e.id)) continue;
    if (!e.at || Date.parse(e.at) < cutoff) continue;
    if (!existsSync(e.folder) || !withinRoots(e.folder)) continue;
    const branch = e.branch ?? null;
    const spawnMode = e.spawnMode ?? "same-dir";
    out.push({
      id: e.id,
      folder: e.folder,
      name: e.name ?? "",
      branch,
      spawnMode,
      permissionMode: e.permissionMode ?? "default",
      at: e.at,
      stale: spawnMode === "worktree" && !!branch && !branchExists(e.folder, branch),
    });
  }
  lostCache = out;
  return out;
}

/* ---------- kept worktrees (dirty orphans the sweeps refused to delete) ---------- */
export interface KeptWorktree {
  path: string;
  repo: string;
  id: string;
  branch: string | null;
  dirty: boolean;
}

const WT_BASE = join(homedir(), ".agent-runner", "worktrees");

export function listKeptWorktrees(): KeptWorktree[] {
  if (!existsSync(WT_BASE)) return [];
  const live = new Set([...sessions.values()].map((s) => s.worktreePath).filter(Boolean));
  const out: KeptWorktree[] = [];
  for (const repo of readdirSync(WT_BASE)) {
    let ids: string[] = [];
    try {
      ids = readdirSync(join(WT_BASE, repo));
    } catch {
      continue;
    }
    for (const id of ids) {
      const wtPath = join(WT_BASE, repo, id);
      if (live.has(wtPath)) continue; // belongs to a running session
      let branch: string | null = null;
      let dirty = false;
      try {
        branch = execFileSync("git", ["-C", wtPath, "branch", "--show-current"], { encoding: "utf8", timeout: 3000 }).trim() || null;
        dirty = execFileSync("git", ["-C", wtPath, "status", "--porcelain"], { encoding: "utf8", timeout: 5000 }).trim().length > 0;
      } catch {
        /* unreadable — still listed so it can be cleaned */
      }
      out.push({ path: wtPath, repo, id, branch, dirty });
    }
  }
  return out;
}

export function forceRemoveWorktree(wtPath: string): void {
  const resolved = join(wtPath); // normalize
  if (!resolved.startsWith(WT_BASE + "/")) throw new Error("not a runner-managed worktree");
  const commonDir = execFileSync("git", ["-C", resolved, "rev-parse", "--path-format=absolute", "--git-common-dir"], {
    encoding: "utf8",
    timeout: 5000,
    stdio: ["ignore", "pipe", "ignore"],
  }).trim();
  const mainRoot = commonDir.endsWith("/.git") ? commonDir.slice(0, -5) : commonDir;
  execFileSync("git", ["-C", mainRoot, "worktree", "remove", "--force", resolved], { timeout: 15000, stdio: "ignore" });
  execFileSync("git", ["-C", mainRoot, "worktree", "prune"], { timeout: 15000, stdio: "ignore" });
  journal({ event: "worktree.force-removed", wtPath: resolved });
}

export function listSessions(): Session[] {
  return [...sessions.values()]
    .map(publicView)
    .sort((a, b) => b.startedAt.localeCompare(a.startedAt));
}

export function getSession(id: string): Session | null {
  const s = sessions.get(id);
  return s ? publicView(s) : null;
}

export function getSessionLog(id: string): string | null {
  const s = sessions.get(id);
  return s ? s.log.join("") : null;
}

function addWorktree(folder: string, branch: string, id: string): string {
  const wtPath = join(homedir(), ".agent-runner", "worktrees", basename(folder), id);
  mkdirSync(join(homedir(), ".agent-runner", "worktrees", basename(folder)), { recursive: true });
  try {
    execFileSync("git", ["-C", folder, "worktree", "add", wtPath, branch], {
      encoding: "utf8",
      timeout: 15000,
      stdio: ["ignore", "pipe", "pipe"],
    });
  } catch (err) {
    const stderr = (err as { stderr?: string }).stderr?.trim();
    // git's own message is the most useful thing to surface (e.g. "already checked out")
    throw new Error(stderr?.split("\n").pop() || `could not create worktree for ${branch}`);
  }
  return wtPath;
}

function removeWorktree(folder: string, wtPath: string) {
  try {
    execFileSync("git", ["-C", folder, "worktree", "remove", wtPath], { timeout: 15000, stdio: "ignore" });
  } catch {
    // dirty or already gone — keep it rather than destroy work, but make it visible
    journal({ event: "worktree.kept", folder, wtPath, reason: "dirty or removal failed" });
  }
}

// Boot sweep: sessions are in-memory, so any worktree on disk at startup is an
// orphan from a previous runner. Remove the clean ones; keep dirty ones and journal.
export function sweepOrphanWorktrees() {
  const base = join(homedir(), ".agent-runner", "worktrees");
  if (!existsSync(base)) return;
  for (const repo of readdirSync(base)) {
    const repoDir = join(base, repo);
    let ids: string[] = [];
    try {
      ids = readdirSync(repoDir);
    } catch {
      continue;
    }
    for (const wt of ids) {
      const wtPath = join(repoDir, wt);
      try {
        const commonDir = execFileSync("git", ["-C", wtPath, "rev-parse", "--path-format=absolute", "--git-common-dir"], {
          encoding: "utf8",
          timeout: 5000,
          stdio: ["ignore", "pipe", "ignore"],
        }).trim();
        const mainRoot = commonDir.endsWith("/.git") ? commonDir.slice(0, -5) : commonDir;
        execFileSync("git", ["-C", mainRoot, "worktree", "remove", wtPath], { timeout: 15000, stdio: "ignore" });
        journal({ event: "worktree.swept", wtPath });
      } catch {
        journal({ event: "worktree.kept", wtPath, reason: "orphan is dirty or unresolvable" });
      }
    }
  }
}

export function createSession(opts: {
  folder: string;
  name?: string;
  spawnMode?: "same-dir" | "worktree";
  branch?: string;
  permissionMode?: string;
}): Session {
  const name = opts.name?.trim() || `${basename(opts.folder)}-${randomUUID().slice(0, 4)}`;
  const duplicate = [...sessions.values()].some((s) => s.name === name && s.state !== "exited" && s.state !== "error");
  if (duplicate) throw new Error(`a live session named "${name}" already exists`);

  const spawnMode = opts.spawnMode === "worktree" ? "worktree" : "same-dir";
  const permissionMode = opts.permissionMode || "default";
  const id = randomUUID().slice(0, 8);

  let worktreePath: string | null = null;
  let branch: string | null = null;
  let cwd = opts.folder;
  let repoRootForCleanup: string | null = null;
  if (spawnMode === "worktree") {
    if (!opts.branch) throw new Error("worktree mode requires a branch");
    const root = gitRoot(opts.folder);
    if (!root) throw new Error("folder is not inside a git repository");
    branch = opts.branch;
    repoRootForCleanup = root;
    worktreePath = addWorktree(root, branch, id); // worktree of the nearest repo root, cleaned up on exit
    // land in the equivalent subfolder inside the worktree when it exists on that branch
    const rel = relative(root, opts.folder);
    const sub = rel ? join(worktreePath, rel) : worktreePath;
    cwd = existsSync(sub) ? sub : worktreePath;
  }

  const args = ["remote-control", "--name", name, "--spawn", "same-dir", "--permission-mode", permissionMode];
  // real PTY: the CLI prints its pairing URL and stays alive as it would in a terminal
  const proc = ptySpawn("claude", args, {
    name: "xterm-256color",
    cols: 120,
    rows: 40,
    cwd,
    env: { ...process.env, FORCE_COLOR: "0", NO_COLOR: "1" } as Record<string, string>,
  });

  const session: LiveSession = {
    id,
    name,
    folder: opts.folder,
    spawnMode,
    branch,
    worktreePath,
    permissionMode,
    state: "starting",
    pairingUrl: null,
    startedAt: new Date().toISOString(),
    exitedAt: null,
    exitCode: null,
    lastOutputAt: null,
    lastLine: null,
    proc,
    log: [],
    killed: false,
  };
  sessions.set(id, session);
  journal({ event: "session.start", id, name, folder: opts.folder, spawnMode, branch, permissionMode });

  const onData = (chunk: string) => {
    session.lastOutputAt = new Date().toISOString();
    const text = chunk.replace(ANSI, "");
    session.log.push(text);
    if (session.log.length > 400) session.log.splice(0, session.log.length - 400);
    session.lastLine = lastMeaningfulLine(session.log) ?? session.lastLine;
    if (!session.pairingUrl) {
      const match = session.log.join("").match(URL_RE);
      if (match) {
        session.pairingUrl = match[0].replace(/[).,]+$/, "");
        session.state = "ready";
        journal({ event: "session.ready", id, pairingUrl: session.pairingUrl });
        if (ntfy?.notifyReady) {
          notify({
            title: `session ready: ${session.name}`,
            body: basename(session.folder) + (session.branch ? ` @ ${session.branch}` : ""),
            click: session.pairingUrl,
          });
        }
      }
    }
  };
  proc.onData(onData);
  proc.onExit(({ exitCode }) => {
    session.state = session.state === "starting" ? "error" : "exited";
    session.exitedAt = new Date().toISOString();
    session.exitCode = exitCode;
    session.proc = null;
    if (session.worktreePath) removeWorktree(repoRootForCleanup ?? session.folder, session.worktreePath);
    journal({ event: "session.exit", id, code: exitCode });
    if (!session.killed && ntfy && ntfy.notifyExit !== "never") {
      const failed = exitCode !== 0 || session.state === "error";
      if (failed) {
        notify({
          title: `session failed: ${session.name}`,
          body: `${basename(session.folder)} exited with code ${exitCode}${session.state === "error" ? " (died before ready)" : ""}`,
          priority: "high",
          tags: "warning",
        });
      } else if (ntfy.notifyExit === "all") {
        notify({
          title: `session exited: ${session.name}`,
          body: `${basename(session.folder)} exited cleanly`,
          tags: "checkered_flag",
        });
      }
    }
  });

  return publicView(session);
}

export function killSession(id: string): Session | null {
  const s = sessions.get(id);
  if (!s) return null;
  s.killed = true; // set before the kill so onExit never notifies for user-initiated stops
  if (s.proc) {
    try {
      s.proc.kill("SIGTERM"); // pty close also HUPs the whole session tree
    } catch {
      /* already gone */
    }
  }
  journal({ event: "session.kill", id });
  return publicView(s);
}

export function removeSession(id: string): boolean {
  const s = sessions.get(id);
  if (!s) {
    // lost-session headstones dismiss through the same endpoint
    const idx = lostCache?.findIndex((l) => l.id === id) ?? -1;
    if (idx >= 0) {
      lostCache!.splice(idx, 1);
      return true;
    }
    return false;
  }
  if (s.state !== "exited" && s.state !== "error") throw new Error("session is still live; kill it first");
  return sessions.delete(id);
}
