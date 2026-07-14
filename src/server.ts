import { serve } from "@hono/node-server";
import { serveStatic } from "@hono/node-server/serve-static";
import { Hono } from "hono";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createApi, type Config } from "./api.js";
import { configureWebhooks } from "./events.js";
import { configureBrowser } from "./folders.js";
import { configureJobs } from "./jobs.js";
import { listSessions, sweepOrphanWorktrees } from "./sessions.js";

const CONFIG_PATH = join(process.cwd(), "config.json");
const config: Config = JSON.parse(readFileSync(CONFIG_PATH, "utf8"));
if ((config as unknown as Record<string, unknown>).ntfy !== undefined) {
  console.warn("config.ntfy is no longer used — notifications are generic webhooks now; see the webhooks key in README");
}
configureBrowser(config.roots, config.showHidden);
configureWebhooks(config.webhooks);
configureJobs(config.jobs);
sweepOrphanWorktrees();

function applyAndPersistConfig() {
  configureBrowser(config.roots, config.showHidden);
  configureWebhooks(config.webhooks);
  writeFileSync(CONFIG_PATH, JSON.stringify(config, null, 2) + "\n");
}

const app = new Hono();

const VERSION: string = JSON.parse(readFileSync(join(process.cwd(), "package.json"), "utf8")).version;

app.get("/healthz", (c) =>
  c.json({ ok: true, version: VERSION, sessions: listSessions().filter((s) => s.state === "ready").length })
);

const api = createApi(config, applyAndPersistConfig);
app.route("/api/v1", api); // canonical, documented in docs/api.md
app.route("/api", api); // deprecated alias for pinned clients — kept for one release

app.use("/*", serveStatic({ root: "./public" }));

const host = config.host ?? "127.0.0.1";
serve({ fetch: app.fetch, port: config.port, hostname: host }, (info) => {
  console.log(`groundcontrol listening on http://${host}:${info.port}`);
});
