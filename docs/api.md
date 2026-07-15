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
| `admin` | `PUT /config`, force-removing kept worktrees, sweeping orbit branches |

A known token missing the needed scope gets `403 insufficient_scope`; a
missing or unknown token gets `401 unauthorized`. The token's `name` travels
into the journal as `actor`, so the flight log shows who launched what.

`GET /healthz` is unauthenticated by design (uptime probes):

```json
{ "ok": true, "version": "0.4.0", "sessions": 1 }
```

## Docs endpoint

`GET /docs` serves a [Scalar](https://scalar.com) API reference for this
API, and `GET /openapi.yaml` serves the raw spec it reads from — both
unauthenticated by design, same as `/healthz`. Visit
`http://<host>:<port>/docs` in a browser.

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
| `branch_held` | 409 | orbit sweep refused — the branch is checked out by a live session or a kept worktree (clean that up via `DELETE /worktrees` instead) |
| `branch_not_merged` | 409 | orbit sweep refused — git's safe delete found unmerged commits; merge the work first (there is no force delete) |
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
| `GET /sessions` | live sessions plus journal-derived `lost` headstones and `landed` debriefs |
| `POST /sessions` | spawn `claude remote-control` — see below |
| `GET /sessions/:id` | one session |
| `GET /sessions/:id/qr` | pairing QR as SVG (409 `not_ready` until the pairing URL is scraped) |
| `GET /sessions/:id/log` | plain-text runtime log tail (last ~400 chunks, ANSI stripped) |
| `GET /sessions/:id/transcript` | the actual conversations — user prompts and assistant replies, parsed from the JSONL transcripts Claude Code writes for the session's launch directory |
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
  "capacity": 8,
  "callbackUrl": "http://n8n.local:5678/webhook/gc",
  "timeoutMs": 60000
}
```

- `folder` (required) — must be inside a configured root.
- `name` — must not collide with a live session; omit to auto-generate.
- `spawnMode` — `same-dir` (default) or `worktree`. Worktree mode requires
  `branch`, cuts a private `gc/<name-slug>-<id>` branch off it under
  `~/.groundcontrol/worktrees/<repo>/<name-slug>-<id>/`, and cleans up on exit
  (dirty worktrees are kept, never force-deleted). The slug comes from the
  session name (jobs: the prompt), so `git branch` answers "what was this run
  for" at a glance. The worktree directory carries the slug too because
  claude.ai's environment picker labels environments by their folder basename
  (verified on CLI 2.1.210; `--name` reaches only the pre-created session) —
  worktree launches appear there under the session name, same-dir launches
  under the launch folder's name.
- `branch` — in `same-dir` mode this switches the checkout first; git refuses
  if that would clobber local changes.
- `permissionMode` — passed to `claude`: `default`, `acceptEdits`, `plan`,
  `auto`, `dontAsk`, or `bypassPermissions`; anything else is a 400
  `invalid_param`. Note that `dontAsk` and `bypassPermissions` skip Claude
  Code's confirmation prompts entirely — a launch-scoped token grants that
  power, so hand those tokens out accordingly.
- `capacity` — how many sessions claude.ai can create in this environment
  (`--capacity`). Normalized, never rejected: absent or < 1 falls back to 32
  (the CLI default), values above 256 clamp to 256. The flag is passed to
  `claude` only when the normalized value differs from the default, so
  launches keep working on CLIs that predate it. The session object always
  carries the resolved `capacity`.
- `presetName` — resolve a named [preset](#config) from config: the preset
  fills only the options the request left empty (`permissionMode`,
  `capacity`, `spawnMode` — an explicit request field always wins) and
  supplies the settings JSON for
  [injection](#preset-settings-injection). A name that no longer resolves
  is **not** an error: the launch proceeds without injection and the
  session carries `settingsSkipReason: "preset no longer exists"`, so
  relaunching a recent whose preset was deleted keeps working. The name is
  journaled either way.
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

#### Preset settings injection

A preset with `settingsJson` has it written to
`<launch cwd>/.claude/settings.local.json` before spawn — the project-scoped
settings file Claude Code merges natively — and removed when the session
exits. For worktree launches the launch cwd is the worktree, so the file
never touches your checkout. The mechanism never clobbers user files:

- The injected file carries a top-level `"_groundcontrol": true` marker key.
  Teardown and the boot sweep only ever remove files whose parse shows the
  marker; a file that lost its marker mid-session is left alone.
- Creation is an atomic create-if-absent, so two concurrent launches into
  the same folder can never interleave. When a file already exists: unmarked
  (or unparseable) means it's yours — injection refuses with
  `settingsSkipReason: "settings file already exists"`; marked and owned by
  another live session skips with `"in use by another session"`; marked with
  no live owner is a crash leftover and is replaced (journaled as
  `settingsNote: "replaced stale injection"`).
- At exit the file is removed only when no other live session shares the
  folder — otherwise removal defers to the last same-folder exit, and each
  exit re-checks. On boot the runner sweeps marked leftovers in folders
  whose recent `session.start` entries recorded an injection (crash
  recovery).
- While the file exists it affects **every** Claude session started in that
  folder, including ones you start by hand.

A skip never blocks the launch. The session object carries
`settingsSkipReason` only when injection was requested but skipped —
absence means either injected or never requested: the wire never claims
success, only explains absence. The outcome is flattened into the
`session.start` journal entry (`settingsInjected`, `settingsSkipReason`,
`settingsNote`), never a standalone event.

#### Debriefs

When a worktree session exits, the runner captures a debrief **before** the
worktree is cleaned up — the diff stat of the run's own work plus the fate of
its `gc/` branch. The stat is measured from the merge-base with the launch
base (not the launch commit itself), so upstream work pulled or merged into
the worktree never counts as the run's own; uncommitted edits do count, and
`uncommitted` is the number of paths `git status --porcelain` reported at
exit. It appears as `debrief` on the session object (and in the
`session.exit` event payload):

```json
"debrief": {
  "filesChanged": 3,
  "insertions": 42,
  "deletions": 7,
  "uncommitted": 1,
  "branchState": "in-orbit"
}
```

- `branchState` — `merged` (the `gc/` branch is gone, or its tip is reachable
  from the default branch), `in-orbit` (the branch survived with commits the
  default branch lacks), or `worktree-kept` (the worktree was dirty and kept
  on disk, branch intact).
- Same-dir sessions have no worktree to debrief: `debrief` is absent.
- Capture is best-effort: if git fails at exit time the debrief is absent and
  teardown proceeds regardless — it never blocks a session from exiting.

The same five fields are written flat into the `session.exit` journal entry
(`filesChanged`, `insertions`, `deletions`, `uncommitted`, `branchState`,
alongside `id`, `code`, and the `claudeSessionId` when one was captured),
which is what makes debriefs survive restarts: `GET /sessions` returns a
third list, `landed`, joining `session.start` entries with their
`session.exit` entries — id, launch config
(`name`/`folder`/`branch`/`spawnMode`/`permissionMode`),
`startedAt`/`exitedAt`, `exitCode`, `claudeSessionId` when the journal has
it, and the `debrief` when one was captured. Newest exits first, capped at
20, scoped to a 7-day window and the configured roots (folders that vanished
are dropped). Sessions the runner still lists under `sessions` — live, or
exited but not yet dismissed via `DELETE /sessions/:id/record` — are
excluded from `landed`.

#### Claude state enrichment

While sessions are live, a runner-owned poll of `claude agents --json` (the
CLI's documented scripting surface, 2.1.145+) enriches the session objects.
Every field degrades to absence — an unreachable registry, an older CLI, or
an exited session simply renders nothing — and none of it ever drives a state
transition: the PTY remains the sole exit authority.

- `claudeSessionId` — Claude Code's conversation UUID for the launch's
  primary session (the id `claude --resume` wants). Captured once, first
  capture wins; journaled as a `session.claude-id` entry and flattened into
  the `session.exit` entry, so both `lost` and `landed` objects expose
  `claudeSessionId` after a runner restart when the journal has it.
- `activity` — `"busy"` or `"idle"`, copied from the registry row. Absent
  when the registry hasn't confirmed it recently, reports an unknown value,
  or the session exited — never a stale value. Activity flips are never
  journaled and never fan out to webhooks.
- `environmentSessions` — the environment's own live sessions (registry rows
  descended from the spawned launcher, including sessions claude.ai creates
  on demand), as `[{ "name": "...", "status": "busy" }]`, primary session
  first, then by name. The primary row's status reads the same registry row
  as `activity`, so the two surfaces never disagree. Rows clear on the first
  successful poll that no longer lists them; the wall-clock grace window
  applies only across failed polls. When the process snapshot is unavailable,
  rows keep their last confident classification instead of demoting to
  `folderSessions`, and age out on the wall-clock window.
- `folderSessions` — live Claude sessions in the launch's directory that do
  *not* belong to the environment (a manual `claude` in the same folder, IDE
  sessions), sorted by name — "in this folder" is the literal contract. Rows
  age out shortly after the registry stops listing them. In both lists
  `status` is omitted when unknown, and a list with no rows is absent, never
  empty.
- `prLink` — `{ "number": 9, "url": "https://github.com/..." }`, the newest
  `pr-link` record from the session's transcript. Best-effort enrichment of
  an undocumented transcript detail; absence is the contract.

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
- `isolation` / `docker` — booleans; `true` is rejected with
  `501 not_implemented`. Jobs currently run as the runner's user, same as
  sessions.

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

### Orbit (leftover session branches)

Worktree runs leave their `gc/*` branch behind whenever it accumulated
commits (that's the point — the branch keeps the work reachable). Orbit is
the cross-repo view of those leftovers, and the guarded cleanup for them.

| method + path | what |
|---|---|
| `GET /orbit` | leftover `gc/*` branches across recently-used repos |
| `DELETE /orbit?repo=&branch=` | safe-delete one (`git branch -d` — never force) |

`GET /orbit` returns `{ "orbit": [...] }`:

```json
{
  "orbit": [
    {
      "repo": "/home/you/repos/checkout-service",
      "branch": "gc/fix-race-a1b2c3d4",
      "merged": false,
      "lastCommitAt": "2026-07-14T20:07:24+00:00",
      "heldBy": "/home/you/.groundcontrol/worktrees/checkout-service/a1b2c3d4"
    }
  ]
}
```

- Repos to scan come from the folders of recent `session.start` journal
  entries — resolved to their repo root, deduped, scoped to the configured
  roots. Repos absent from the journal's read window (last 2000 entries)
  aren't scanned: orbit is a recents-driven tool, not an archive.
- `merged` — the branch tip is reachable from the repo's default branch
  (origin's declared HEAD, else local `main`, else `master`; a repo with no
  default at all reads everything as unmerged).
- Branches checked out by a **live session** never appear.
- `heldBy` — present when some other worktree (a kept dirty one) still has
  the branch checked out; clean that up via `DELETE /worktrees?path=`
  instead of the branch sweep.

`DELETE /orbit?repo=<repo-root>&branch=<gc/...>` (admin scope) validates in
order: non-`gc/*` branch names are rejected (`400 invalid_param`) before
anything else runs; the repo must be inside the roots, still exist, and be a
git repo (`400 invalid_path`); a branch held by a live session or a worktree
is refused (`409 branch_held`). The delete itself is always `git branch -d`
— an unmerged branch comes back as `409 branch_not_merged`, and there is no
force parameter: merge the work (or clean up its worktree) first. A
successful sweep writes an `orbit.swept {repo, branch}` journal entry.

### Config

| method + path | what |
|---|---|
| `GET /config` | roots, showHidden, webhooks, presets |
| `PUT /config` | partial update of the same; persists to `config.json` |

PUT is a partial update: only keys present in the body are touched. A payload
carrying just `{"presets": [...]}` never disturbs roots or webhooks; an
explicit empty array clears the list. Validation runs before anything is
applied, so a rejected write leaves the config untouched.

`presets` are named launch configurations. Each preset carries:

- `name` (required) — unique and non-empty within the list.
- `permissionMode` — empty, or one of `default`, `acceptEdits`, `plan`,
  `auto`, `dontAsk`, `bypassPermissions`.
- `spawnMode` — empty, or `same-dir` / `worktree`.
- `capacity` — 0 (unset) or 1–256.
- `settingsJson` — optional settings-file content as a string: at most 64 KB
  and must parse as a JSON object. A top-level `hooks` key is rejected —
  hooks run shell commands and are a separately gated feature. An `env` key
  is allowed. Injected at launch as
  `<launch cwd>/.claude/settings.local.json` for the session's lifetime —
  see [Preset settings injection](#preset-settings-injection).

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
