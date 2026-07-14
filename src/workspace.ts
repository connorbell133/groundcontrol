import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { basename, join } from "node:path";

/* ---------- journal: append-only flight log, shared by sessions and jobs ---------- */

const DATA_DIR = join(process.cwd(), "data");
const JOURNAL = join(DATA_DIR, "journal.json");

export function journal(entry: Record<string, unknown>) {
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

export function readJournal(): Record<string, unknown>[] {
  try {
    const all = JSON.parse(readFileSync(JOURNAL, "utf8"));
    return Array.isArray(all) ? all.slice(-2000) : [];
  } catch {
    return [];
  }
}

/* ---------- worktrees: one branch, one worktree, cleaned up honestly ---------- */

export const WT_BASE = join(homedir(), ".groundcontrol", "worktrees");

// resolve a picked branch name to a checkout-able ref: local branch as-is, else the
// remote-tracking ref (the picker offers remote branches a fresh clone never checked out)
export function resolveBranch(folder: string, branch: string): string | null {
  try {
    execFileSync("git", ["-C", folder, "rev-parse", "--verify", "--quiet", `refs/heads/${branch}`], {
      stdio: "ignore",
      timeout: 2000,
    });
    return branch;
  } catch {
    /* not a local branch */
  }
  try {
    const remote = execFileSync(
      "git",
      ["-C", folder, "for-each-ref", "--format=%(refname:short)", `refs/remotes/*/${branch}`],
      { encoding: "utf8", timeout: 2000 }
    )
      .trim()
      .split("\n")
      .filter(Boolean)[0];
    return remote || null;
  } catch {
    return null;
  }
}

export function branchExists(folder: string, branch: string): boolean {
  return resolveBranch(folder, branch) !== null;
}

export function addWorktree(folder: string, branch: string, id: string): { wtPath: string; wtBranch: string; baseCommit: string } {
  const wtPath = join(WT_BASE, basename(folder), id);
  mkdirSync(join(WT_BASE, basename(folder)), { recursive: true });
  const base = resolveBranch(folder, branch);
  if (!base) throw new Error(`branch ${branch} not found locally or on a remote`);
  const wtBranch = `gc/${id}`;
  try {
    // each run works on its own branch cut from the base: the base may be checked
    // out in the main folder (git refuses a second checkout) or exist only on a remote
    execFileSync("git", ["-C", folder, "worktree", "add", "-b", wtBranch, wtPath, base], {
      encoding: "utf8",
      timeout: 15000,
      stdio: ["ignore", "pipe", "pipe"],
    });
  } catch (err) {
    const stderr = (err as { stderr?: string }).stderr?.trim();
    // git's own message is the most useful thing to surface
    throw new Error(stderr?.split("\n").pop() || `could not create worktree for ${branch}`);
  }
  const baseCommit = execFileSync("git", ["-C", wtPath, "rev-parse", "HEAD"], { encoding: "utf8", timeout: 5000 }).trim();
  return { wtPath, wtBranch, baseCommit };
}

export function removeWorktree(folder: string, wtPath: string, wtBranch?: string | null, baseCommit?: string | null) {
  try {
    execFileSync("git", ["-C", folder, "worktree", "remove", wtPath], { timeout: 15000, stdio: "ignore" });
  } catch {
    // dirty or already gone — keep it rather than destroy work, but make it visible
    journal({ event: "worktree.kept", folder, wtPath, reason: "dirty or removal failed" });
    return;
  }
  // the run branch outlives the worktree only if it accumulated commits
  if (wtBranch && baseCommit) {
    try {
      const tip = execFileSync("git", ["-C", folder, "rev-parse", `refs/heads/${wtBranch}`], {
        encoding: "utf8",
        timeout: 5000,
      }).trim();
      if (tip === baseCommit) {
        execFileSync("git", ["-C", folder, "branch", "-D", wtBranch], { timeout: 5000, stdio: "ignore" });
      }
    } catch {
      /* branch already gone */
    }
  }
}

// current branch of the repo containing folder, or null when detached/not a repo
export function currentBranch(folder: string): string | null {
  try {
    return (
      execFileSync("git", ["-C", folder, "branch", "--show-current"], {
        encoding: "utf8",
        timeout: 5000,
        stdio: ["ignore", "pipe", "ignore"],
      }).trim() || null
    );
  } catch {
    return null;
  }
}
