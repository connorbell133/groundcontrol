import { Hono, type Context } from "hono";
import { streamSSE } from "hono/streaming";
import type { ContentfulStatusCode } from "hono/utils/http-status";
import { existsSync, statSync } from "node:fs";
import QRCode from "qrcode";
import { onEvent, type WebhookConfig } from "./events.js";
import { browse, branches, listRoots, withinRoots } from "./folders.js";
import { cancelJob, createJob, getJob, getJobLog, listJobs, removeJob, type JobsConfig } from "./jobs.js";
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
} from "./sessions.js";

export interface Config {
  port: number;
  host?: string;
  roots: string[];
  showHidden: boolean;
  webhooks?: WebhookConfig[];
  jobs?: JobsConfig;
  authToken?: string;
}

// Stable machine-readable codes — clients key off these, never off message text.
// Documented in docs/api.md; adding a code is fine, renaming one is a breaking change.
export type ApiErrorCode =
  | "unauthorized"
  | "invalid_json"
  | "missing_param"
  | "invalid_param"
  | "invalid_path"
  | "invalid_config"
  | "outside_roots"
  | "not_found"
  | "not_ready"
  | "ready_timeout"
  | "launch_failed"
  | "session_live"
  | "job_live"
  | "worktree_error"
  | "not_implemented";

function err(c: Context, status: ContentfulStatusCode, code: ApiErrorCode, message: string) {
  return c.json({ error: { code, message } }, status);
}

const HTTP_URL = /^https?:\/\//;

// block until the session pairs, dies, or the deadline passes — 300ms poll is
// plenty against a multi-second provision and immune to event races
async function waitForReady(id: string, timeoutMs: number): Promise<"ready" | "dead" | "timeout"> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const s = getSession(id);
    if (!s || s.state === "exited" || s.state === "error") return "dead";
    if (s.state === "ready") return "ready";
    if (Date.now() >= deadline) return "timeout";
    await new Promise((r) => setTimeout(r, 300));
  }
}

// One sub-app, mounted at /api/v1 (canonical) and /api (deprecated alias for one release).
export function createApi(config: Config, persistConfig: () => void): Hono {
  const api = new Hono();

  // Every route requires the bearer token when one is configured.
  // ?token= is accepted too because <img> tags (the QR) and EventSource can't send headers.
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
      webhooks: config.webhooks ?? [],
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
    if (body.webhooks !== undefined) {
      if (!Array.isArray(body.webhooks)) return err(c, 400, "invalid_config", "webhooks must be a list");
      const hooks: WebhookConfig[] = [];
      for (const w of body.webhooks) {
        if (typeof w?.url !== "string" || !HTTP_URL.test(w.url))
          return err(c, 400, "invalid_config", `webhook url must be http(s): ${w?.url}`);
        if (w.events !== undefined && (!Array.isArray(w.events) || w.events.some((t: unknown) => typeof t !== "string" || !t.trim())))
          return err(c, 400, "invalid_config", "webhook events must be a list of non-empty strings");
        hooks.push({ url: w.url, ...(w.events ? { events: w.events } : {}) });
      }
      config.webhooks = hooks;
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

  // Live lifecycle stream (SSE): session.start/ready/exit/kill as they happen.
  // No replay — reconnecting clients should re-list first; the journal is the history.
  api.get("/events", (c) =>
    streamSSE(c, async (stream) => {
      let live = true;
      const unsub = onEvent((e) => {
        if (live) stream.writeSSE({ event: e.event, data: JSON.stringify(e) }).catch(() => {});
      });
      stream.onAbort(() => {
        live = false;
        unsub();
      });
      await stream.writeSSE({ event: "hello", data: JSON.stringify({ at: new Date().toISOString() }) });
      while (live) {
        await stream.sleep(25000);
        if (live) await stream.writeSSE({ event: "ping", data: "" }).catch(() => {});
      }
    })
  );

  api.post("/sessions", async (c) => {
    const body = await c.req.json().catch(() => null);
    if (!body) return err(c, 400, "invalid_json", "request body must be valid JSON");
    if (!body.folder) return err(c, 400, "missing_param", "folder required");
    if (!withinRoots(body.folder)) return err(c, 400, "outside_roots", "folder outside configured roots");
    if (body.callbackUrl !== undefined && (typeof body.callbackUrl !== "string" || !HTTP_URL.test(body.callbackUrl)))
      return err(c, 400, "invalid_param", "callbackUrl must be an http(s) URL");

    let session;
    try {
      session = createSession({
        folder: body.folder,
        name: body.name,
        spawnMode: body.spawnMode,
        branch: body.branch,
        permissionMode: body.permissionMode,
        callbackUrl: body.callbackUrl,
      });
    } catch (e) {
      return err(c, 409, "launch_failed", (e as Error).message);
    }

    if (c.req.query("wait") !== "ready") return c.json(session, 201);

    // ?wait=ready: one round-trip to a pairing URL for scripts and automations
    const timeoutMs = Math.min(300000, Math.max(1000, Number(body.timeoutMs) || 60000));
    const outcome = await waitForReady(session.id, timeoutMs);
    const latest = getSession(session.id) ?? session;
    if (outcome === "ready") return c.json(latest, 201);
    if (outcome === "dead")
      return err(
        c,
        409,
        "launch_failed",
        `session ${session.id} exited before pairing${latest.exitCode !== null ? ` (code ${latest.exitCode})` : ""}${latest.lastLine ? ` — ${latest.lastLine}` : ""}`
      );
    return err(c, 504, "ready_timeout", `session ${session.id} not ready after ${timeoutMs}ms — still starting; poll GET /sessions/${session.id}`);
  });

  /* ---------- headless jobs: claude -p, no phone in the loop ---------- */

  api.post("/jobs", async (c) => {
    const body = await c.req.json().catch(() => null);
    if (!body) return err(c, 400, "invalid_json", "request body must be valid JSON");
    if (!body.folder) return err(c, 400, "missing_param", "folder required");
    if (typeof body.prompt !== "string" || !body.prompt.trim()) return err(c, 400, "missing_param", "prompt required");
    if (!withinRoots(body.folder)) return err(c, 400, "outside_roots", "folder outside configured roots");
    if (body.callbackUrl !== undefined && (typeof body.callbackUrl !== "string" || !HTTP_URL.test(body.callbackUrl)))
      return err(c, 400, "invalid_param", "callbackUrl must be an http(s) URL");
    // honest about the launch console's wired-for-v1 toggle: accepted, never ignored
    if (body.isolation || body.docker)
      return err(c, 501, "not_implemented", "Docker isolation is not implemented yet — jobs run as the runner's user");
    try {
      const job = createJob({
        folder: body.folder,
        prompt: body.prompt,
        spawnMode: body.spawnMode,
        branch: body.branch,
        permissionMode: body.permissionMode,
        timeoutMs: body.timeoutMs,
        callbackUrl: body.callbackUrl,
      });
      return c.json(job, 202);
    } catch (e) {
      return err(c, 409, "launch_failed", (e as Error).message);
    }
  });

  api.get("/jobs", (c) => c.json({ jobs: listJobs() }));

  api.get("/jobs/:id", (c) => {
    const job = getJob(c.req.param("id"));
    return job ? c.json(job) : err(c, 404, "not_found", "no such job");
  });

  api.get("/jobs/:id/log", (c) => {
    const log = getJobLog(c.req.param("id"));
    return log === null ? err(c, 404, "not_found", "no such job") : c.text(log);
  });

  api.delete("/jobs/:id", (c) => {
    const job = cancelJob(c.req.param("id"));
    return job ? c.json(job) : err(c, 404, "not_found", "no such job");
  });

  api.delete("/jobs/:id/record", (c) => {
    try {
      return removeJob(c.req.param("id")) ? c.json({ ok: true }) : err(c, 404, "not_found", "no such job");
    } catch (e) {
      return err(c, 409, "job_live", (e as Error).message);
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
