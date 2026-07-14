import { readdirSync, existsSync, statSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join, resolve, dirname } from "node:path";

export interface FolderEntry {
  name: string;
  path: string;
  isGit: boolean;
  branch: string | null;
}

export interface BrowseResult {
  path: string;
  parent: string | null;
  isGit: boolean;
  repoRoot: string | null;
  repoName: string | null;
  branch: string | null;
  folders: FolderEntry[];
}

const config = { roots: [] as string[], showHidden: false };

export function configureBrowser(roots: string[], showHidden: boolean) {
  config.roots = roots.map((r) => resolve(r));
  config.showHidden = showHidden;
}

export function withinRoots(path: string): boolean {
  const resolved = resolve(path);
  return config.roots.some((root) => resolved === root || resolved.startsWith(root + "/"));
}

export function gitRoot(path: string): string | null {
  try {
    return execFileSync("git", ["-C", path, "rev-parse", "--show-toplevel"], {
      encoding: "utf8",
      timeout: 2000,
      stdio: ["ignore", "pipe", "ignore"],
    }).trim() || null;
  } catch {
    return null;
  }
}

function gitBranch(path: string): string | null {
  try {
    return execFileSync("git", ["-C", path, "branch", "--show-current"], {
      encoding: "utf8",
      timeout: 2000,
    }).trim() || "(detached)";
  } catch {
    return null;
  }
}

function isGitFolder(path: string): boolean {
  return existsSync(join(path, ".git"));
}

export function listRoots(): FolderEntry[] {
  return config.roots.map((root) => ({
    name: root,
    path: root,
    isGit: isGitFolder(root),
    branch: isGitFolder(root) ? gitBranch(root) : null,
  }));
}

export function browse(path: string): BrowseResult {
  const resolved = resolve(path);
  if (!withinRoots(resolved)) throw new Error("path outside configured roots");
  if (!statSync(resolved).isDirectory()) throw new Error("not a directory");

  const entries = readdirSync(resolved, { withFileTypes: true })
    .filter((e) => e.isDirectory() || e.isSymbolicLink())
    .filter((e) => config.showHidden || !e.name.startsWith("."))
    .filter((e) => e.name !== "node_modules");

  const folders: FolderEntry[] = [];
  for (const entry of entries) {
    const full = join(resolved, entry.name);
    try {
      if (!statSync(full).isDirectory()) continue;
      // symlinks must still resolve inside the roots
      if (entry.isSymbolicLink() && !withinRoots(full)) continue;
      const git = isGitFolder(full);
      folders.push({ name: entry.name, path: full, isGit: git, branch: git ? gitBranch(full) : null });
    } catch {
      // unreadable entry: skip
    }
  }
  folders.sort((a, b) => (a.isGit === b.isGit ? a.name.localeCompare(b.name) : a.isGit ? -1 : 1));

  const atRoot = config.roots.includes(resolved);
  // repo context comes from the nearest parent with .git, not just this folder
  const repoRoot = gitRoot(resolved);
  return {
    path: resolved,
    parent: atRoot ? null : dirname(resolved),
    isGit: repoRoot !== null,
    repoRoot,
    repoName: repoRoot ? repoRoot.split("/").pop() ?? null : null,
    branch: repoRoot ? gitBranch(resolved) : null,
    folders,
  };
}

export function branches(path: string): string[] {
  if (!withinRoots(path)) throw new Error("path outside configured roots");
  try {
    const refs = execFileSync(
      "git",
      ["-C", path, "for-each-ref", "--format=%(refname)", "--sort=-committerdate", "refs/heads/", "refs/remotes/"],
      { encoding: "utf8", timeout: 3000 }
    )
      .trim()
      .split("\n")
      .filter(Boolean);
    // remote-tracking refs collapse to plain branch names so a fresh clone (one local
    // branch) still offers everything on origin; first occurrence wins the dedup
    const seen = new Set<string>();
    const names: string[] = [];
    for (const ref of refs) {
      const name = ref.startsWith("refs/heads/")
        ? ref.slice("refs/heads/".length)
        : ref.replace(/^refs\/remotes\/[^/]+\//, "");
      if (name === "HEAD" || seen.has(name)) continue;
      seen.add(name);
      names.push(name);
    }
    return names;
  } catch {
    return [];
  }
}
