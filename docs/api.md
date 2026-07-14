# groundcontrol HTTP API

Everything the PWA does goes through this API — the UI is just the first
client. Anything that can send HTTP on your tailnet (a script, n8n, another
agent) can browse folders, spawn Claude Code sessions, tail their logs, and
kill them.

Base URL: `http://<host>:<port>/api/v1`. The unversioned `/api/*` paths are a
deprecated alias kept for one release; new clients use `/api/v1`. The machine
contract lives in [`openapi.yaml`](openapi.yaml).

## Auth

Every `/api/*` route requires the bearer token from `config.json` when one is
set (set one):

```
authorization: Bearer <authToken>
```

`?token=<authToken>` is accepted as a fallback because `<img>` tags (the QR)
and `EventSource` (the SSE stream) can't send headers. Prefer the header
everywhere else — query strings leak into logs and proxies more readily.

### Scoped tokens

`authToken` has full access. Automations should get their own entries under
`tokens` in `config.json` instead — each carries only the scopes it needs:

```json
"tokens": [
  { "name": "n8n", "token": "<openssl rand -hex 16>", "scopes": ["read", "launch"] }
]
```

| scope | grants |
|---|---|
| `read` | browse folders and branches; list/inspect sessions, jobs, worktrees, config; the QR; the SSE stream |
| `launch` | spawn and kill sessions and jobs; dismiss finished records |
| `admin` | `PUT /config`, force-removing kept worktrees |

A known token missing the needed scope gets `403 insufficient_scope`; a
missing or unknown token gets `401 unauthorized`. The token's `name` travels
into the journal as `actor`, so the flight log shows who launched what.

`GET /healthz` is unauthenticated by design (uptime probes):

```json
{ "ok": true, "version": "0.4.0", "sessions": 1 }
```

## Errors

Every error is the same envelope, with a stable machine-readable `code` and a
human `message`:

```json
{ "error": { "code": "outside_roots", "message": "folder outside configured roots" } }
```

Key off `code`, never off `message` text. Adding a code is a compatible
change; renaming one is breaking.

| code | status | meaning |
|---|---|---|
| `unauthorized` | 401 | missing or wrong bearer token |
| `insufficient_scope` | 403 | token is valid but lacks the scope the route needs |
| `invalid_json` | 400 | request body didn't parse |
| `missing_param` | 400 | a required query/body field is absent |
| `invalid_param` | 400 | a field is present but malformed (e.g. non-http `callbackUrl`) |
| `invalid_path` | 400 | path outside roots, or not a directory |
| `outside_roots` | 400 | launch folder is outside configured roots |
| `invalid_config` | 400 | config write rejected (bad roots, bad webhooks block) |
| `not_found` | 404 | no such session or job |
| `not_ready` | 409 | session exists but has no pairing URL yet |
| `ready_timeout` | 504 | `?wait=ready` deadline passed — session still starting, keep polling |
| `launch_failed` | 409 | spawn rejected — duplicate live name, unknown branch, dirty checkout on branch switch, worktree creation failed |
| `session_live` | 409 | tried to dismiss a session that's still running |
| `job_live` | 409 | tried to dismiss a job that's still queued/running — cancel it first |
| `worktree_error` | 400 | kept-worktree removal failed or path isn't runner-managed |
| `not_implemented` | 501 | the field is real but the feature isn't (Docker isolation) — rejected rather than silently ignored |

## Endpoints

### Browse

| method + path | what |
|---|---|
| `GET /roots` | configured browse roots, with git badge + current branch |
| `GET /browse?path=` | folders inside `path`, plus the repo context of `path` itself |
| `GET /branches?path=` | branch names for the repo containing `path`, local + remote deduped, newest-commit first |

### Sessions (spawning agents)

| method + path | what |
|---|---|
| `GET /sessions` | live sessions plus `lost` headstones from the journal |
| `POST /sessions` | spawn `claude remote-control` — see below |
| `GET /sessions/:id` | one session |
| `GET /sessions/:id/qr` | pairing QR as SVG (409 `not_ready` until the pairing URL is scraped) |
| `GET /sessions/:id/log` | plain-text runtime log tail (last ~400 chunks, ANSI stripped) |
| `DELETE /sessions/:id` | kill (SIGTERM to the PTY; worktree cleaned on exit) |
| `DELETE /sessions/:id/record` | dismiss an exited/error/lost session card |
| `GET /journal/recent?limit=` | recent distinct launch configs (max 20), with staleness flag |
| `GET /events` | SSE stream of lifecycle events — see [Events & notifications](#events--notifications) |

`POST /sessions` body:

```json
{
  "folder": "/home/you/repos/checkout-service",
  "name": "fix-race",
  "spawnMode": "worktree",
  "branch": "main",
  "permissionMode": "acceptEdits",
  "callbackUrl": "http://n8n.local:5678/webhook/gc",
  "timeoutMs": 60000
}
```

- `folder` (required) — must be inside a configured root.
- `name` — must not collide with a live session; omit to auto-generate.
- `spawnMode` — `same-dir` (default) or `worktree`. Worktree mode requires
  `branch`, cuts a private `gc/<id>` branch off it under
  `~/.groundcontrol/worktrees/`, and cleans up on exit (dirty worktrees are
  kept, never force-deleted).
- `branch` — in `same-dir` mode this switches the checkout first; git refuses
  if that would clobber local changes.
- `permissionMode` — passed to `claude`: `default`, `acceptEdits`, `plan`, or
  `bypassPermissions`.
- `callbackUrl` — this launch's own webhook: every lifecycle event except
  `session.start` (`session.ready`, `session.kill`, `session.exit`) is POSTed
  to it (same payload shape as [events](#events--notifications)).
- `timeoutMs` — only meaningful with `?wait=ready` (below); clamped 1s–5min,
  default 60s.

Returns `201` with the session immediately (`state: "starting"`). Add
**`?wait=ready`** to block until the pairing URL exists and get it in one
round-trip: `201` with `pairingUrl` set, `409 launch_failed` if the process
died first (the message carries the last output line), or `504 ready_timeout`
if the deadline passed while still provisioning.

Session states: `starting` → `ready` (pairing URL scraped) → `exited`;
`error` means it died before ever becoming ready.

### Jobs (headless agents)

Sessions need a phone to finish the loop; jobs don't. `POST /jobs` runs
`claude -p <prompt> --output-format json` to completion — the resource n8n
and cron-shaped automations talk to.

| method + path | what |
|---|---|
| `GET /jobs` | all jobs this runner knows, newest first |
| `POST /jobs` | queue a headless run — see below; returns `202` immediately |
| `GET /jobs/:id` | status, result, cost, duration, turns |
| `GET /jobs/:id/log` | raw stdout+stderr |
| `DELETE /jobs/:id` | cancel — dequeues a queued job, kills a running one (whole process group); a no-op on finished jobs |
| `DELETE /jobs/:id/record` | drop a finished job from the list |

`POST /jobs` body:

```json
{
  "folder": "/home/you/repos/checkout-service",
  "prompt": "run the test suite and fix the first failing test; commit the fix",
  "branch": "main",
  "spawnMode": "worktree",
  "permissionMode": "acceptEdits",
  "timeoutMs": 900000,
  "callbackUrl": "http://n8n.local:5678/webhook/gc-jobs"
}
```

- `prompt` (required) — what the agent should do.
- `spawnMode` — **defaults to `worktree`** for git folders (an autonomous run
  should not dirty a checkout you might be standing in); `branch` defaults to
  the repo's current branch. Non-git folders run in place.
- `permissionMode` — defaults to `acceptEdits`: file edits flow, anything
  else a non-interactive run can't approve is denied. Real autonomy (running
  commands, git pushes) usually wants an explicit `bypassPermissions` — say
  it, mean it.
- `timeoutMs` — kill deadline, default 15 min (configurable via the `jobs`
  config key), max 2h. A timed-out job ends in state `timeout`.
- `callbackUrl` — POSTed the `job.exit` event when the run finishes.
- `isolation` / `docker` — accepted and rejected with `501 not_implemented`;
  jobs currently run as the runner's user, same as sessions.

Job states: `queued` → `running` → `succeeded` | `failed` | `timeout` |
`canceled`. Concurrency is bounded (default 2, FIFO queue) by the `jobs`
config key: `{ "concurrency": 2, "timeoutMs": 900000 }`. On completion,
`result` carries claude's final text, plus `costUsd`, `durationMs`, and
`numTurns` parsed from the JSON output. The journal stores a prompt hash and
an 80-char preview, never the full prompt — the full text lives on the job
record until dismissed.

## Events & notifications

One mechanism, one payload. Every lifecycle transition becomes an event:

```json
{
  "event": "session.exit",
  "at": "2026-07-14T20:07:24.764Z",
  "title": "session failed: waittest",
  "message": "repo exited with code 1 (died before ready)",
  "data": { "session": { "...": "full session object" } }
}
```

`event`/`data` are for machines; `title`/`message` are ready-made human
strings so notification receivers don't have to compose their own.

Events: `session.start`, `session.ready`, `session.exit`, `session.kill`,
`job.start`, `job.exit` (with `data.job` instead of `data.session`).

Three ways to consume them:

**1. SSE stream** — `GET /events` holds the connection and pushes every event
as it happens (plus a `ping` every 25s). No replay: reconnecting clients
should re-list `GET /sessions` first; the journal is the history.

```bash
curl -N "$GC/events?token=$GC_TOKEN"
```

**2. Global webhook subscribers** — the `webhooks` config key. Each entry is a
URL plus an optional event filter; every matching event is POSTed as JSON
(5s timeout, fire-and-forget, failures journaled as `webhook.failed`):

```json
"webhooks": [
  { "url": "http://n8n.local:5678/webhook/groundcontrol" },
  { "url": "https://ntfy.sh/my-topic?template=yes&title={{.title}}&message={{.message}}",
    "events": ["session.ready", "session.failed"] }
]
```

Filter tokens: exact event names, prefix wildcards (`session.*`, `job.*`),
`*` (or omit `events`) for everything, and the derived tokens
`session.failed` / `job.failed` — an exit whose run failed (nonzero exit,
died before ready, or timed out; not killed on purpose) also matches them, so
"failures only" needs no client-side logic. No receiver is special: n8n
consumes the JSON raw; ntfy renders it via its `?template=yes` query params
as shown above.

**3. Per-launch `callbackUrl`** — scoped to one session, delivered regardless
of the global subscriber list. Right for job-style automations that only care
about their own launch.

### Worktrees

| method + path | what |
|---|---|
| `GET /worktrees` | kept worktrees — dirty orphans the sweeps refused to delete |
| `DELETE /worktrees?path=` | force-remove one; merged `gc/*` branches are deleted, unmerged kept |

### Config

| method + path | what |
|---|---|
| `GET /config` | roots, showHidden, webhooks |
| `PUT /config` | partial update of the same; persists to `config.json` |

A token that can reach `PUT /config` can widen `roots` — treat `authToken`
and any `admin`-scoped token as root on the box. Give automations
`read`/`launch` tokens instead; they can never widen what's reachable.

## Cookbook

```bash
GC=http://localhost:3020/api/v1
AUTH="authorization: Bearer $GC_TOKEN"
```

Spawn a worktree session off `main` and print the pairing URL — one call:

```bash
curl -sf -H "$AUTH" -X POST "$GC/sessions?wait=ready" \
  -d '{"folder":"/home/you/repos/checkout-service","spawnMode":"worktree","branch":"main"}' \
  | jq -r .pairingUrl
```

Watch everything happen live:

```bash
curl -N "$GC/events?token=$GC_TOKEN"
```

Tail a session's log:

```bash
watch -n 2 "curl -sf -H '$AUTH' $GC/sessions/$id/log | tail -20"
```

Kill it:

```bash
curl -sf -H "$AUTH" -X DELETE $GC/sessions/$id
```

List everything in flight:

```bash
curl -sf -H "$AUTH" $GC/sessions | jq '.sessions[] | {id, name, state, folder, branch}'
```

Fire a headless job from n8n (HTTP node) and get the result on your webhook:

```bash
curl -sf -H "$AUTH" -X POST $GC/jobs -d '{
  "folder": "/home/you/repos/checkout-service",
  "prompt": "update CHANGELOG.md from the last 5 commits and commit it",
  "permissionMode": "bypassPermissions",
  "callbackUrl": "http://n8n.local:5678/webhook/gc-jobs"
}'
```

Relaunch the most recent launch config:

```bash
curl -sf -H "$AUTH" "$GC/journal/recent?limit=1" \
  | jq '.recent[0] | {folder, spawnMode, branch, permissionMode}' \
  | curl -sf -H "$AUTH" -X POST $GC/sessions -d @-
```
