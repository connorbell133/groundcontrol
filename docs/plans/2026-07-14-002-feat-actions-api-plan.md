---
title: "feat: Promote the internal API to a first-class actions API"
type: feat
date: 2026-07-14
origin: docs/brainstorms/2026-07-13-agent-runner-requirements.md
---

# feat: Promote the internal API to a first-class actions API

## Summary

Every action in groundcontrol is already an HTTP endpoint — the PWA is just the
first client. What's missing is the contract that lets a *second* client exist:
stable versioned paths, a consistent error shape, written documentation, scoped
tokens so an automation can spawn agents without being able to rewrite `roots`,
push-based lifecycle signals instead of polling, and the one genuinely new
action — headless `claude -p` jobs with webhook callbacks (R13–R16 of the
agent-runner brainstorm). Five phases, each independently shippable, in
dependency order.

## Current surface (inventory)

What exists today, all behind the single bearer token:

| action | endpoint |
|---|---|
| health / version | `GET /healthz` (unauthenticated) |
| list browse roots | `GET /api/roots` |
| browse a folder | `GET /api/browse?path=` |
| list branches | `GET /api/branches?path=` |
| read / write config (roots, ntfy, showHidden) | `GET` / `PUT /api/config` |
| list / force-remove kept worktrees | `GET` / `DELETE /api/worktrees` |
| **spawn an agent session** (same-dir or worktree, branch, permission mode) | `POST /api/sessions` |
| list sessions + lost sessions | `GET /api/sessions` |
| inspect a session | `GET /api/sessions/:id` |
| pairing QR (SVG) | `GET /api/sessions/:id/qr` |
| runtime log tail | `GET /api/sessions/:id/log` |
| kill a session | `DELETE /api/sessions/:id` |
| dismiss a dead/lost session card | `DELETE /api/sessions/:id/record` |
| recent launches | `GET /api/journal/recent` |

Gaps that keep this from being a real API: no version prefix, error bodies are
ad-hoc `{error: string}` with inconsistent codes, nothing is documented, the
only lifecycle signal is polling `GET /api/sessions/:id` until `pairingUrl`
appears, one token grants everything including `PUT /api/config` (an API
consumer could widen `roots` — game over), and there is no headless run at all:
n8n can pair nothing, so "spawn an agent" currently always means a human with a
phone finishing the loop.

## Key Technical Decisions

- **Version by path, migrate the PWA in the same commit.** Routes move to
  `/api/v1/*`; `/api/*` stays as a thin alias for one release (the PWA
  self-reloads on version bump, so the alias is for pinned scripts, not for
  us). No content negotiation, no header versioning — this is a personal tool,
  the path prefix is the whole story.
- **No new dependencies for the contract.** Error shape, validation, and the
  OpenAPI document are hand-maintained, not generated via `@hono/zod-openapi` +
  zod. The badge says 4 runtime deps and the API is ~15 routes; a hand-written
  `docs/openapi.yaml` checked by a typecheck-adjacent CI step costs less than a
  schema framework. Revisit only if the route count doubles.
- **One error envelope everywhere:** `{ "error": { "code": "outside_roots",
  "message": "..." } }` with stable machine-readable `code` strings. The PWA
  keys off `code` where it currently string-matches messages.
- **Session creation gets a synchronous option.** `POST /api/v1/sessions?wait=ready`
  (bounded by a `timeoutMs` field, default 60s) blocks until the pairing URL is
  scraped or the process dies, so a script gets `pairingUrl` in one round-trip.
  Default remains async — the PWA keeps its optimistic card flow.
- **Push signals reuse the plumbing that already exists.** The journal +
  ntfy call sites already mark every lifecycle transition; fan the same events
  out to (a) a global `GET /api/v1/events` SSE stream and (b) an optional
  per-launch `callbackUrl` webhook POST. No new state machine — `notify()`
  grows a sibling `emit()`.
- **Headless jobs are a separate resource, not a session flag.** Sessions are
  PTY + pairing URL + a human; jobs are `claude -p --output-format json` + exit
  code + result payload. Different lifecycle, different fields, different
  defaults (`POST /api/v1/jobs` defaults to worktree mode — an autonomous agent
  should not dirty a checkout you're standing on). Shared machinery (worktree
  add/remove, branch resolution, journaling) extracts from `sessions.ts` into
  `src/workspace.ts` so both resources use one implementation.
- **Scoped tokens, additive to the existing one.** Config grows optional
  `tokens: [{ name, token, scopes }]` with scopes `read` (browse/list/inspect),
  `launch` (sessions + jobs), `admin` (config writes, worktree force-remove,
  record deletion). The legacy `authToken` keeps full scope so nothing breaks.
  The n8n token gets `read,launch` and can never widen `roots`.
- **Docker isolation stays out of scope.** The launch console's toggle is
  wired-for-v1 and the API mirrors that honestly: the field is accepted,
  validated, and rejected with `not_implemented` rather than silently ignored.
- **MCP wrapper is a client, not a server feature.** If/when we want Claude
  itself driving the tower, it's a thin stdio script speaking to the HTTP API
  with a `launch`-scoped token — nothing in the server changes. Deferred to
  last and optional.

## Implementation Units

### U1. API contract: v1 prefix, error envelope, route extraction
**Files:** src/server.ts, src/api.ts (new), public/app.js
Extract all `/api/*` handlers into `src/api.ts` as a Hono sub-app mounted at
`/api/v1` and aliased at `/api`; introduce `apiError(c, status, code, message)`
and sweep every handler onto it; fix drive-by status codes (404 vs 400 on the
QR route's "no pairing url yet" → 409 `not_ready`); PWA fetches move to
`/api/v1` and switch error handling to `code`.
**Test:** typecheck clean; PWA smoke (browse → launch → kill) unchanged; curl
each route and assert envelope shape + status; `/api/roots` alias still
answers.

### U2. Documentation: docs/api.md + OpenAPI
**Files:** docs/api.md (new), docs/openapi.yaml (new), README.md, .githooks/pre-commit
`docs/api.md` is the human page: auth, the QR `?token=` caveat, a curl
cookbook (spawn worktree session and print pairing URL as a one-liner, kill,
tail log). `docs/openapi.yaml` is the machine page covering every v1 route,
schema-first from the `Session`/`KeptWorktree`/`RecentLaunch` interfaces.
Pre-commit gains a YAML-parses check alongside the JSON one.
**Test:** `npx @redocly/cli lint docs/openapi.yaml` passes (dev-time only, not
a dependency); every route in `src/api.ts` appears in the spec (grep audit).

### U3. Lifecycle signals: wait=ready, SSE stream, per-launch webhook
**Files:** src/sessions.ts, src/api.ts, docs/api.md
`emit()` beside `notify()` publishing `{event, sessionId, state, pairingUrl?}`
for start/ready/exit/kill; `GET /api/v1/events` holds an SSE connection and
replays nothing (journal is the history — document that); `POST sessions`
accepts `wait=ready` + `timeoutMs` and `callbackUrl` (https-or-tailnet-http,
fire-and-forget with 5s abort like ntfy, journaled on delivery failure).
**Test:** curl `-N` the SSE stream, launch a session in another shell, observe
ready event; `wait=ready` returns pairingUrl in one call and 504
`ready_timeout` when claude is absent; webhook receives exit for a killed
session; a hanging webhook endpoint doesn't delay session exit handling.

### U4. Headless jobs: POST /api/v1/jobs
**Files:** src/jobs.ts (new), src/workspace.ts (new), src/sessions.ts, src/api.ts, docs/api.md, docs/openapi.yaml
Extract `resolveBranch`/`addWorktree`/`removeWorktree`/journal helpers into
`src/workspace.ts`; `jobs.ts` owns a FIFO queue (concurrency 2, per-job
timeout default 15m, both config-overridable) running
`claude -p <prompt> --output-format json` via `execFile` (no PTY needed);
routes: `POST /api/v1/jobs` → 202 `{id}`, `GET /jobs`, `GET /jobs/:id`
(status/result/cost/duration parsed from the JSON output), `GET /jobs/:id/log`,
`DELETE /jobs/:id` (cancel); webhook + SSE reuse U3's `emit()`; journal events
`job.start`/`job.exit`; ntfy on failure honoring `notifyExit`.
**Test:** post a job against a scratch repo, poll to completion, assert result
+ cost fields; timeout kills and reports `timeout` status; third concurrent
job queues; worktree pruned after completion, kept when dirty; cancel mid-run
delivers `canceled` to the webhook.

### U5. Scoped tokens
**Files:** src/server.ts, src/api.ts, config.example.json, docs/api.md, README.md
Auth middleware resolves the presented token to a scope set (`authToken` →
all scopes); route table annotates required scope; insufficient scope → 403
`insufficient_scope` naming the missing scope; journal entries gain the token
`name` as `actor` so the flight log says who launched what.
**Test:** a `read,launch` token can browse and spawn but gets 403 on
`PUT /api/v1/config` and `DELETE /api/v1/worktrees`; legacy single-token
config behaves exactly as today; journal rows carry actor.

### U6 (optional, later). MCP client wrapper
**Files:** scripts/mcp-tower.ts (new)
Stdio MCP server exposing `launch_session`, `launch_job`, `list_sessions`,
`kill_session`, `get_log` as tools, implemented purely as an HTTP client of
v1 with a scoped token from env. Ships as a script, not a dependency of the
server.
**Test:** register in a Claude Code project `.mcp.json`, launch and kill a
session by prompting.

## Sequencing & sizing

U1 → U2 land together (a contract isn't real until it's written down) — one
sitting. U3 next — small, mostly plumbing reuse. U4 is the meat (new resource,
queue, output parsing) — do it alone. U5 is independent of U3/U4 and can slot
anywhere after U1. U6 whenever the itch strikes.

## Verification (whole feature)

tsc clean; PWA smoke in both color schemes; the n8n scenario end-to-end with a
`read,launch` token: browse → post a headless job with a callbackUrl → receive
the webhook → fetch the log — no polling, no admin scope, no phone.

## Outstanding questions (defaults chosen, flag to revisit)

- Job prompts land in the journal as a hash + first 80 chars, not full text
  (journal doubles as the recents UI; full prompts belong in the job record).
- SSE has no replay/backfill; reconnecting clients re-list sessions first.
  Good enough for one owner; revisit if a dashboard ever needs gap-free events.
- `wait=ready` default timeout 60s matches the slowest observed cold start;
  tune from journal data once real numbers exist.
