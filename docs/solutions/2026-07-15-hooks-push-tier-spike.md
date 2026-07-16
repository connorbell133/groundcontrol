---
title: "U6 hooks push tier — verification spike findings"
date: 2026-07-15
type: spike
plan: docs/plans/2026-07-15-001-feat-claude-state-surfaces-plan.md
cli_version: "2.1.210 (Claude Code)"
machine: macOS arm64
status: findings-only (no production code)
---

# U6 — Hooks push tier verification spike

Bounded spike answering the four fork questions that decide whether and how the
documented hooks push tier can ship. All probes run on this machine against the
installed CLI, confirmed each session as **2.1.210 (Claude Code)** (`claude --version`).
Short-lived, self-killed local probes on the user's own account.

> Version-recording caveat: hook subprocesses inherit `CLAUDE_CODE_EXECPATH=.../versions/2.1.172`
> from the harness process that ran this spike — that env var reflects the *parent* runner,
> not the probed binary. The invoked `claude` is the `~/.local/bin/claude` symlink →
> `~/.local/share/claude/versions/2.1.210`, and MCP debug logs emit `claude-code/2.1.210 (cli)`.
> Every verdict below is against 2.1.210.

---

## Verdicts (brief)

| # | Question | Verdict |
|---|----------|---------|
| 1 | Injection path into server-mode sessions | **YES** — two working paths (flag-mode `--settings`, and project-scoped `.claude/settings.local.json`); both MERGE with user settings |
| 2 | Propagation to extra on-demand sessions the server spawns | **UNVERIFIED** — cannot spawn an on-demand session without a paired client |
| 3 | `Stop` hook fires on user interrupt (Escape) | **UNVERIFIED** — Stop fires and delivers a payload, but Escape-interrupt vs. completion could not be cleanly isolated headlessly; payload carries no stop-reason |
| 4 | `CLAUDE_CODE_BRIDGE_SESSION_ID` in hook subprocesses | **NO (without pairing)** — absent in SessionStart/Stop with the RC server up but no client; `CLAUDE_CODE_ENVIRONMENT_KIND=bridge` is present in subcommand mode; presence-with-pairing UNVERIFIED |

---

## Q1 — INJECTION PATH into server-mode sessions — **YES**

**Re-confirmed (cheap):** the `remote-control` subcommand rejects `--settings`.

```
$ claude remote-control --settings
Error: Unknown argument: --settings
Run 'claude remote-control --help' for usage.
```

`claude remote-control --help` on 2.1.210 lists no `--settings` flag (only `--name`,
`--continue`, `--session-id`, `--permission-mode`, `--debug-file`, `--verbose`,
`--spawn`, `--capacity`, `--[no-]create-session-in-dir`, and the name-prefix flag).

### Path A — top-level flag `claude --settings <json> --remote-control` — WORKS + MERGES

The **top-level** parser *does* accept `--settings <file-or-json>` and `--remote-control [name]`.
Help text: `--settings` "load **additional** settings from" (additive wording).

Probe (trusted scratch dir, pseudo-TTY via `script`, self-killed; chrome-extension
prompt answered by feeding `2\r`):

```
# marker settings (SessionStart hook writes a marker + dumps CLAUDE_*/bridge env)
$ cat /tmp/u6-settings.json
{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command",
 "command":"{ echo \"=== SessionStart fired $(date +%s) pid=$$ ===\";
   echo \"CLAUDE_SESSION_ID=$CLAUDE_SESSION_ID\";
   env | grep -i -E \"bridge|claude_\"; echo ---bridge-grep---; env | grep -i bridge;
 } >> /tmp/u6-hook-probe.log 2>&1"}]}]}}

$ { sleep 9; printf '2\r'; sleep 32; } | \
  script -q /tmp/u6-tui.log sh -c \
  "cd $SCRATCH && exec claude --settings /tmp/u6-settings.json --remote-control --debug-file /tmp/u6-rc-debug.log" &
  # ... sleep 44; pkill -f remote-control
```

Evidence the injected hook fired **in server mode**:

```
=== SessionStart fired 1784137061 pid=80321 ===
CLAUDE_SESSION_ID=
CLAUDE_CODE_ENTRYPOINT=cli
CLAUDE_EFFORT=high                     <-- user effortLevel:high survived (MERGE)
CLAUDE_PROJECT_DIR=/private/tmp/u6-scratch.TyK560
CLAUDE_CODE_CHILD_SESSION=1
CLAUDE_CODE_SESSION_ID=c003b8cd-de62-485f-b6dd-dd5d411045a1
```

Evidence the RC bridge server actually started (debug log + TUI):

```
[bridge:repl] ... session=cse_01Wi4o5n436vAnd7HkxfPhRr
TUI: /rc connecting…
TUI: https://claude.ai/code/session_01Wi4o5n436vAnd7HkxfPhRr
```

**MERGE, not replace — proven three ways.** The injected `--settings` JSON contained
*only* a hook, yet in the same session:
- the TUI statusline rendered `Fable 5 with high effort · Claude Max · Connor`
  (user `model` + `statusLine` + `effortLevel` survived),
- the footer showed `⏵⏵ auto mode on` (user `permissions.defaultMode: "auto"` survived),
- `CLAUDE_EFFORT=high` appeared in the hook subprocess env.

**Cross-source hook aggregation — proven.** With a project `.claude/settings.local.json`
SessionStart hook present *and* a different SessionStart hook injected via `--settings`,
**both fired in the same run** (distinct marker files, same timestamp) — so `--settings`
does not replace the user/project `hooks` map; hooks aggregate across sources.

```
injected (--settings) SessionStart fired?  YES: === SessionStart fired 1784137353 ...
project (.claude/settings.local.json)  fired same run?  YES: === PROJSCOPE SessionStart fired 1784137353 ...
```

`CLAUDE_SESSION_ID` is empty in the hook env; the live UUID is in
**`CLAUDE_CODE_SESSION_ID`** (use this, not `CLAUDE_SESSION_ID`).

### Path B — project-scoped `.claude/settings.local.json` — WORKS under plain `remote-control`

Writing the marker hook to `<cwd>/.claude/settings.local.json` and running the plain
subcommand `claude remote-control` (no `--settings`) fired the hook for the pre-created
session:

```
=== PROJSCOPE SessionStart fired 1784137144 ===
sid=d7f66760-9e23-44af-b686-c5f0a49e0c1e
CLAUDE_CODE_ENVIRONMENT_KIND=bridge         <-- session knows it is a bridge session
```

This is the **cleanest server-mode path**: the subcommand reads project settings
natively, so no flag-injection is required. The tradeoff is the *same-dir settings-file
pollution* problem — a `.claude/settings.local.json` affects **every** session in that
directory (GroundControl launches, plus any manual/IDE `claude` a user starts there).
For GroundControl this is acceptable because it controls its own launch dirs / worktrees,
but the file must be scoped to the launch dir and cleaned up, and worktree-mode on-demand
sessions (isolated worktrees) will **not** inherit a cwd-scoped file (see Q2).

---

## Q2 — PROPAGATION to extra on-demand sessions — **UNVERIFIED**

Spawning an *extra* (on-demand) session behind the RC server requires a paired client
(phone or browser) to issue the spawn request; only the single pre-created session
(`--create-session-in-dir`, default on) exists headlessly. No client was paired in this
spike, so on-demand propagation was **not observed** and is marked unverified rather than
guessed.

Architectural inference (not verified):
- **Project-scoped file (Path B), `--spawn=same-dir`:** on-demand sessions run in the same
  cwd and would read the same `.claude/settings.local.json` → hooks would very likely
  propagate. High confidence, but unverified.
- **Project-scoped file, `--spawn=worktree`:** on-demand sessions get an isolated git
  worktree; a cwd-scoped settings file in the original dir would **not** be present there
  unless GroundControl also writes it into each worktree (or a WorktreeCreate hook does).
- **Flag-mode (Path A) `--settings`:** whether the flag value propagates from the RC
  server process to server-spawned child sessions is unverified.

**What would verify it:** pair a phone/browser client, spawn one on-demand session in each
`--spawn` mode, and check the marker log fires for the *second* session's UUID.

---

## Q3 — `Stop` hook on user interrupt (Escape) — **UNVERIFIED**

The `Stop` hook fires in interactive sessions and delivers a JSON payload on stdin:

```json
{"session_id":"b1382e5c-...","transcript_path":".../b1382e5c-....jsonl",
 "cwd":"/private/tmp/u6-scratch.TyK560","prompt_id":"959193f6-...",
 "permission_mode":"auto","effort":{"level":"high"},
 "hook_event_name":"Stop","stop_hook_active":false,
 "last_assistant_message":"...","background_tasks":[],"session_crons":[]}
```

(Note `permission_mode:"auto"` and `effort.level:"high"` in the payload — more merge
confirmation.) However:
- The payload carries **no interrupt / stop-reason discriminator** — nothing distinguishes
  an Escape interrupt from a normal completion.
- Headless timing of "submit long prompt → wait for streaming → send one ESC mid-generation"
  was non-deterministic in the `script`-fed PTY (the chrome-prompt answer and Enter timing
  interfered), so a Stop event could **not** be cleanly attributed to the Escape interrupt
  versus completion. Marked unverified.

**What would verify it:** a paired interactive client (phone/browser) where a human presses
Escape mid-generation and observes exactly one Stop event correlated to the interrupt; or an
`expect`/`tmux` PTY harness that confirms streaming has started, sends a single ESC, and
asserts a Stop fires with no subsequent completion event. **Implication for the follow-up:**
since the Stop payload has no stop-reason, "end-reason capture" (a deferred hooks-tier item)
likely needs the transcript JSONL, not the Stop hook payload.

---

## Q4 — `CLAUDE_CODE_BRIDGE_SESSION_ID` in hook subprocesses — **NO without pairing**

With the RC server running but **no client paired**, `env | grep -i bridge` inside both the
SessionStart and Stop hook subprocesses returned **no `CLAUDE_CODE_BRIDGE_SESSION_ID`**.

What *was* observed:
- In **subcommand** RC mode (`claude remote-control`, Path B): `CLAUDE_CODE_ENVIRONMENT_KIND=bridge`
  is present in the hook env — the session knows it runs under the bridge, but the specific
  bridge session id is not exported.
- In **flag** RC mode (Path A): no bridge-family var was present in the SessionStart hook env
  at all.
- The RC bridge session id exists internally (debug log: `session=cse_01Wi4o5n436vAnd7HkxfPhRr`,
  teardown `session=cse_01Wi4o5n436vAnd7HkxfPhRr`), it is simply not injected into hook
  subprocesses absent an active paired connection.

This matches the documented nuance: the var is set **while the session has an active RC
connection** (a paired driving client). Absent pairing, it is legitimately absent — not a bug.

**What would verify presence:** pair a phone/browser client to the RC session, then trigger a
hook (e.g., a PreToolUse/Stop during active use) and grep the subprocess env — expect
`CLAUDE_CODE_BRIDGE_SESSION_ID` to carry the `cse_...` id for hook→session correlation.

---

## Exact probe commands used

1. `claude --version` → `2.1.210 (Claude Code)` (per session).
2. `claude remote-control --settings` → `Error: Unknown argument: --settings` (instant).
3. `claude remote-control --help` / `claude --help` → confirmed top-level `--settings`
   and `--remote-control` exist; subcommand has neither.
4. Trust setup: `cp ~/.claude.json /tmp/u6-claudejson.bak`; python3 surgically set
   `projects["/private/tmp/u6-scratch.XXXX"]={"hasTrustDialogAccepted":true}`.
5. Print-mode injected-hook firing: `claude --settings /tmp/u6-settings.json -p "..." --max-turns 1`.
6. Server-mode flag injection (Path A):
   `{ sleep 9; printf '2\r'; sleep 32; } | script -q /tmp/u6-tui.log sh -c "cd $SCR && exec claude --settings /tmp/u6-settings.json --remote-control --debug-file /tmp/u6-rc-debug.log" &` (self-killed via `pkill -f remote-control`).
7. Project-scope injection (Path B): wrote `<scratch>/.claude/settings.local.json` marker hook,
   ran plain `claude remote-control` under the same `script`/self-kill wrapper.
8. Cross-source aggregation: print-mode run with both the project file and `--settings` marker.
9. Stop-hook probe: `--settings` with a Stop hook that `cat`s stdin; fed prompt + ESC via timed stdin.
10. Cleanup (see below).

---

## Go / No-Go recommendation — **GO (design phase), gated follow-up spike before build**

A verified injection path into server-mode sessions **exists on 2.1.210**, which was the
open fork question the plan flagged. Recommendation:

- **GO** to design the hooks push tier around **project-scoped `.claude/settings.local.json`
  injection into GroundControl-controlled launch dirs** (Path B) — it works today under the
  plain `remote-control` subcommand, merges cleanly with user settings, and needs no
  unsupported flag. Path A (`--settings --remote-control`) is a viable alternative if the
  launch already shells the top-level `claude` binary.
- **Before committing to the full push implementation**, run a **second short spike with a
  paired phone/browser client** to close the three client-gated unknowns: Q2 (on-demand /
  worktree propagation), Q3 (Stop-on-Escape semantics + end-reason source), Q4
  (`CLAUDE_CODE_BRIDGE_SESSION_ID` presence for hook→session correlation).
- Keep the **poll baseline (U1–U5) as the shipping surface** regardless; hooks remain a
  latency *override*, exactly as the plan frames it. Nothing here changes that.
- Use **`CLAUDE_CODE_SESSION_ID`** (not `CLAUDE_SESSION_ID`, which is empty) as the hook-side
  session identity, and correlate to the registry `sessionId` from `agents --json`.
- Design for the same-dir settings-file pollution constraint: scope the injected hook file to
  the launch dir/worktree and remove it on teardown; account for worktree-mode on-demand
  sessions not inheriting a cwd-scoped file.

---

## Watch-item (recheck each CLI release)

On 2.1.210 the `claude agents` subcommand **has** `--settings <file-or-json>` (confirmed in
`claude agents --help`), and top-level `claude --settings` works — evidence the CLI is growing
per-subsystem settings injection. The `remote-control` **subcommand still lacks `--settings`**.
If a future release adds `--settings` to `remote-control` directly, injection becomes trivial
(no project-file pollution, no top-level-flag wrapper) — recheck `claude remote-control --help`
on every CLI bump.
