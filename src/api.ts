import { Hono, type Context } from "hono";
import type { ContentfulStatusCode } from "hono/utils/http-status";
import { existsSync, statSync } from "node:fs";
import QRCode from "qrcode";
import { browse, branches, listRoots, withinRoots } from "./folders.js";
import {
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
  type NtfyConfig,
} from "./sessions.js";

export interface Config {
  port: number;
  host?: string;
  roots: string[];
  showHidden: boolean;
  ntfy?: NtfyConfig;
  authToken?: string;
}

// Stable machine-readable codes — clients key off these, never off message text.
// Documented in docs/api.md; adding a code is fine, renaming one is a breaking change.
export type ApiErrorCode =
  | "unauthorized"
  | "invalid_json"
  | "missing_param"
  | "invalid_path"
  | "invalid_config"
  | "outside_roots"
  | "not_found"
  | "not_ready"
  | "launch_failed"
  | "session_live"
  | "worktree_error";

function err(c: Context, status: ContentfulStatusCode, code: ApiErrorCode, message: string) {
  return c.json({ error: { code, message } }, status);
}

// One sub-app, mounted at /api/v1 (canonical) and /api (deprecated alias for one release).
export function createApi(config: Config, persistConfig: () => void): Hono {
  const api = new Hono();

  // Every route requires the bearer token when one is configured.
  // ?token= is accepted too because <img> tags (the QR) can't send headers.
  api.use("*", async (c, next) => {
    if (!config.authToken) return next();
    const header = c.req.header("authorization");
    const token = header?.startsWith("Bearer ") ? header.slice(7) : c.req.query("token");
    if (token !== config.authToken) return err(c, 401, "unauthorized", "missing or invalid bearer token");
    return next();
  });

  api.get("/roots", (c) => c.json({ roots: listRoots() }));

  api.get("/browse", (c) => {
    const path = c.req.query("path");
    if (!path) return err(c, 400, "missing_param", "path required");
    try {
      return c.json(browse(path));
    } catch (e) {
      return err(c, 400, "invalid_path", (e as Error).message);
    }
  });

  api.get("/branches", (c) => {
    const path = c.req.query("path");
    if (!path) return err(c, 400, "missing_param", "path required");
    try {
      return c.json({ branches: branches(path) });
    } catch (e) {
      return err(c, 400, "invalid_path", (e as Error).message);
    }
  });

  api.get("/config", (c) =>
    c.json({
      roots: config.roots,
      showHidden: config.showHidden,
      ntfy: config.ntfy ?? { url: "https://ntfy.sh", topic: "", notifyReady: true, notifyExit: "errors" },
    })
  );

  api.put("/config", async (c) => {
    const body = await c.req.json().catch(() => null);
    if (!body) return err(c, 400, "invalid_json", "request body must be valid JSON");

    if (body.roots !== undefined) {
      if (!Array.isArray(body.roots) || body.roots.length === 0)
        return err(c, 400, "invalid_config", "roots must be a non-empty list");
      for (const r of body.roots) {
        if (typeof r !== "string" || !r.startsWith("/"))
          return err(c, 400, "invalid_config", `root must be an absolute path: ${r}`);
        if (!existsSync(r) || !statSync(r).isDirectory()) return err(c, 400, "invalid_config", `not a directory: ${r}`);
      }
      config.roots = body.roots;
    }
    if (body.showHidden !== undefined) config.showHidden = !!body.showHidden;
    if (body.ntfy !== undefined) {
      const n = body.ntfy;
      if (typeof n?.url !== "string" || typeof n?.topic !== "string" || !["errors", "all", "never"].includes(n?.notifyExit)) {
        return err(c, 400, "invalid_config", "invalid ntfy config");
      }
      config.ntfy = { url: n.url.replace(/\/+$/, "") || "https://ntfy.sh", topic: n.topic.trim(), notifyReady: !!n.notifyReady, notifyExit: n.notifyExit };
    }
    persistConfig();
    return c.json({ ok: true });
  });

  api.get("/worktrees", (c) => c.json({ worktrees: listKeptWorktrees() }));

  api.delete("/worktrees", (c) => {
    const path = c.req.query("path");
    if (!path) return err(c, 400, "missing_param", "path required");
    try {
      forceRemoveWorktree(path);
      return c.json({ ok: true });
    } catch (e) {
      return err(c, 400, "worktree_error", (e as Error).message);
    }
  });

  api.get("/sessions", (c) => c.json({ sessions: listSessions(), lost: listLostSessions() }));

  api.get("/journal/recent", (c) => {
    const limit = Math.min(20, Math.max(1, Number(c.req.query("limit")) || 5));
    return c.json({ recent: recentLaunches(limit) });
  });

  api.post("/sessions", async (c) => {
    const body = await c.req.json().catch(() => null);
    if (!body) return err(c, 400, "invalid_json", "request body must be valid JSON");
    if (!body.folder) return err(c, 400, "missing_param", "folder required");
    if (!withinRoots(body.folder)) return err(c, 400, "outside_roots", "folder outside configured roots");
    try {
      const session = createSession({
        folder: body.folder,
        name: body.name,
        spawnMode: body.spawnMode,
        branch: body.branch,
        permissionMode: body.permissionMode,
      });
      return c.json(session, 201);
    } catch (e) {
      return err(c, 409, "launch_failed", (e as Error).message);
    }
  });

  api.get("/sessions/:id", (c) => {
    const session = getSession(c.req.param("id"));
    return session ? c.json(session) : err(c, 404, "not_found", "no such session");
  });

  api.get("/sessions/:id/qr", async (c) => {
    const session = getSession(c.req.param("id"));
    if (!session) return err(c, 404, "not_found", "no such session");
    if (!session.pairingUrl) return err(c, 409, "not_ready", "no pairing url yet");
    const svg = await QRCode.toString(session.pairingUrl, {
      type: "svg",
      margin: 1,
      color: { dark: "#1c1a14", light: "#fffdf6" },
    });
    return c.body(svg, 200, { "content-type": "image/svg+xml", "cache-control": "no-store" });
  });

  api.get("/sessions/:id/log", (c) => {
    const log = getSessionLog(c.req.param("id"));
    return log === null ? err(c, 404, "not_found", "no such session") : c.text(log);
  });

  api.delete("/sessions/:id", (c) => {
    const session = killSession(c.req.param("id"));
    return session ? c.json(session) : err(c, 404, "not_found", "no such session");
  });

  api.delete("/sessions/:id/record", (c) => {
    try {
      return removeSession(c.req.param("id")) ? c.json({ ok: true }) : err(c, 404, "not_found", "no such session");
    } catch (e) {
      return err(c, 409, "session_live", (e as Error).message);
    }
  });

  return api;
}
