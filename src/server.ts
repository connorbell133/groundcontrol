import { serve } from "@hono/node-server";
import { serveStatic } from "@hono/node-server/serve-static";
import { Hono } from "hono";
import { existsSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import QRCode from "qrcode";
import { browse, branches, configureBrowser, listRoots, withinRoots } from "./folders.js";
import {
  configureNtfy,
  createSession,
  forceRemoveWorktree,
  getSession,
  getSessionLog,
  killSession,
  listKeptWorktrees,
  listLostSessions,
  listSessions,
  recentLaunches,
  removeSession,
  sweepOrphanWorktrees,
  type NtfyConfig,
} from "./sessions.js";

interface Config {
  port: number;
  host?: string;
  roots: string[];
  showHidden: boolean;
  ntfy?: NtfyConfig;
  authToken?: string;
}

const CONFIG_PATH = join(process.cwd(), "config.json");
const config: Config = JSON.parse(readFileSync(CONFIG_PATH, "utf8"));
configureBrowser(config.roots, config.showHidden);
configureNtfy(config.ntfy);
sweepOrphanWorktrees();

function applyAndPersistConfig() {
  configureBrowser(config.roots, config.showHidden);
  configureNtfy(config.ntfy);
  writeFileSync(CONFIG_PATH, JSON.stringify(config, null, 2) + "\n");
}

const app = new Hono();

const VERSION: string = JSON.parse(readFileSync(join(process.cwd(), "package.json"), "utf8")).version;

app.get("/healthz", (c) =>
  c.json({ ok: true, version: VERSION, sessions: listSessions().filter((s) => s.state === "ready").length })
);

// Every /api/* route requires the bearer token when one is configured.
// ?token= is accepted too because <img> tags (the QR) can't send headers.
app.use("/api/*", async (c, next) => {
  if (!config.authToken) return next();
  const header = c.req.header("authorization");
  const token = header?.startsWith("Bearer ") ? header.slice(7) : c.req.query("token");
  if (token !== config.authToken) return c.json({ error: "unauthorized" }, 401);
  return next();
});

app.get("/api/roots", (c) => c.json({ roots: listRoots() }));

app.get("/api/browse", (c) => {
  const path = c.req.query("path");
  if (!path) return c.json({ error: "path required" }, 400);
  try {
    return c.json(browse(path));
  } catch (err) {
    return c.json({ error: (err as Error).message }, 400);
  }
});

app.get("/api/branches", (c) => {
  const path = c.req.query("path");
  if (!path) return c.json({ error: "path required" }, 400);
  try {
    return c.json({ branches: branches(path) });
  } catch (err) {
    return c.json({ error: (err as Error).message }, 400);
  }
});

app.get("/api/config", (c) =>
  c.json({ roots: config.roots, showHidden: config.showHidden, ntfy: config.ntfy ?? { url: "https://ntfy.sh", topic: "", notifyReady: true, notifyExit: "errors" } })
);

app.put("/api/config", async (c) => {
  const body = await c.req.json().catch(() => null);
  if (!body) return c.json({ error: "invalid json" }, 400);

  if (body.roots !== undefined) {
    if (!Array.isArray(body.roots) || body.roots.length === 0) return c.json({ error: "roots must be a non-empty list" }, 400);
    for (const r of body.roots) {
      if (typeof r !== "string" || !r.startsWith("/")) return c.json({ error: `root must be an absolute path: ${r}` }, 400);
      if (!existsSync(r) || !statSync(r).isDirectory()) return c.json({ error: `not a directory: ${r}` }, 400);
    }
    config.roots = body.roots;
  }
  if (body.showHidden !== undefined) config.showHidden = !!body.showHidden;
  if (body.ntfy !== undefined) {
    const n = body.ntfy;
    if (typeof n?.url !== "string" || typeof n?.topic !== "string" || !["errors", "all", "never"].includes(n?.notifyExit)) {
      return c.json({ error: "invalid ntfy config" }, 400);
    }
    config.ntfy = { url: n.url.replace(/\/+$/, "") || "https://ntfy.sh", topic: n.topic.trim(), notifyReady: !!n.notifyReady, notifyExit: n.notifyExit };
  }
  applyAndPersistConfig();
  return c.json({ ok: true });
});

app.get("/api/worktrees", (c) => c.json({ worktrees: listKeptWorktrees() }));

app.delete("/api/worktrees", (c) => {
  const path = c.req.query("path");
  if (!path) return c.json({ error: "path required" }, 400);
  try {
    forceRemoveWorktree(path);
    return c.json({ ok: true });
  } catch (err) {
    return c.json({ error: (err as Error).message }, 400);
  }
});

app.get("/api/sessions", (c) => c.json({ sessions: listSessions(), lost: listLostSessions() }));

app.get("/api/journal/recent", (c) => {
  const limit = Math.min(20, Math.max(1, Number(c.req.query("limit")) || 5));
  return c.json({ recent: recentLaunches(limit) });
});

app.post("/api/sessions", async (c) => {
  const body = await c.req.json().catch(() => null);
  if (!body?.folder) return c.json({ error: "folder required" }, 400);
  if (!withinRoots(body.folder)) return c.json({ error: "folder outside configured roots" }, 400);
  try {
    const session = createSession({
      folder: body.folder,
      name: body.name,
      spawnMode: body.spawnMode,
      branch: body.branch,
      permissionMode: body.permissionMode,
    });
    return c.json(session, 201);
  } catch (err) {
    return c.json({ error: (err as Error).message }, 409);
  }
});

app.get("/api/sessions/:id", (c) => {
  const session = getSession(c.req.param("id"));
  return session ? c.json(session) : c.json({ error: "not found" }, 404);
});

app.get("/api/sessions/:id/qr", async (c) => {
  const session = getSession(c.req.param("id"));
  if (!session?.pairingUrl) return c.json({ error: "no pairing url yet" }, 404);
  const svg = await QRCode.toString(session.pairingUrl, {
    type: "svg",
    margin: 1,
    color: { dark: "#1c1a14", light: "#fffdf6" },
  });
  return c.body(svg, 200, { "content-type": "image/svg+xml", "cache-control": "no-store" });
});

app.get("/api/sessions/:id/log", (c) => {
  const log = getSessionLog(c.req.param("id"));
  return log === null ? c.json({ error: "not found" }, 404) : c.text(log);
});

app.delete("/api/sessions/:id", (c) => {
  const session = killSession(c.req.param("id"));
  return session ? c.json(session) : c.json({ error: "not found" }, 404);
});

app.delete("/api/sessions/:id/record", (c) => {
  try {
    return removeSession(c.req.param("id")) ? c.json({ ok: true }) : c.json({ error: "not found" }, 404);
  } catch (err) {
    return c.json({ error: (err as Error).message }, 409);
  }
});

app.use("/*", serveStatic({ root: "./public" }));

const host = config.host ?? "127.0.0.1";
serve({ fetch: app.fetch, port: config.port, hostname: host }, (info) => {
  console.log(`agent-runner listening on http://${host}:${info.port}`);
});
