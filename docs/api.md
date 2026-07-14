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
can't send headers. Prefer the header everywhere else — query strings leak
into logs and proxies more readily.

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
| `invalid_json` | 400 | request body didn't parse |
| `missing_param` | 400 | a required query/body field is absent |
| `invalid_path` | 400 | path outside roots, or not a directory |
| `outside_roots` | 400 | launch folder is outside configured roots |
| `invalid_config` | 400 | config write rejected (bad roots, bad ntfy block) |
| `not_found` | 404 | no such session |
| `not_ready` | 409 | session exists but has no pairing URL yet |
| `launch_failed` | 409 | spawn rejected — duplicate live name, unknown branch, dirty checkout on branch switch, worktree creation failed |
| `session_live` | 409 | tried to dismiss a session that's still running |
| `worktree_error` | 400 | kept-worktree removal failed or path isn't runner-managed |

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

`POST /sessions` body:

```json
{
  "folder": "/home/you/repos/checkout-service",
  "name": "fix-race",
  "spawnMode": "worktree",
  "branch": "main",
  "permissionMode": "acceptEdits"
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

Returns `201` with the session immediately (`state: "starting"`). Poll
`GET /sessions/:id` until `state` is `ready` and `pairingUrl` is set —
push-based signals (wait-for-ready, SSE, webhooks) are planned, see
`docs/plans/2026-07-14-002-feat-actions-api-plan.md`.

Session states: `starting` → `ready` (pairing URL scraped) → `exited`;
`error` means it died before ever becoming ready.

### Worktrees

| method + path | what |
|---|---|
| `GET /worktrees` | kept worktrees — dirty orphans the sweeps refused to delete |
| `DELETE /worktrees?path=` | force-remove one; merged `gc/*` branches are deleted, unmerged kept |

### Config

| method + path | what |
|---|---|
| `GET /config` | roots, showHidden, ntfy |
| `PUT /config` | partial update of the same; persists to `config.json` |

A token that can reach `PUT /config` can widen `roots` — treat the token as
root on the box. Scoped tokens are planned (same plan doc).

## Cookbook

```bash
GC=http://localhost:3020/api/v1
AUTH="authorization: Bearer $GC_TOKEN"
```

Spawn a worktree session off `main` and print the pairing URL:

```bash
id=$(curl -sf -H "$AUTH" -X POST $GC/sessions \
  -d '{"folder":"/home/you/repos/checkout-service","spawnMode":"worktree","branch":"main"}' \
  | jq -r .id)

until url=$(curl -sf -H "$AUTH" $GC/sessions/$id | jq -re .pairingUrl); do sleep 1; done
echo "$url"
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

Relaunch the most recent launch config:

```bash
curl -sf -H "$AUTH" "$GC/journal/recent?limit=1" \
  | jq '.recent[0] | {folder, spawnMode, branch, permissionMode}' \
  | curl -sf -H "$AUTH" -X POST $GC/sessions -d @-
```
