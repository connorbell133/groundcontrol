---
title: "feat: Seven DX nuances for daily phone use"
type: feat
date: 2026-07-14
origin: docs/ideation/2026-07-14-dx-nuances-ideation.html
---

# feat: Seven DX nuances for daily phone use

## Summary

Implement all seven survivors from the DX-nuances ideation: journal-powered run history (recents, relaunch, lost-session resurrection), live session cards (activity age + last-line ticker), ntfy notifications, safelight dark mode, the intent-line launch field, wake-sync on return-to-app, and log copy/share with a render-preserving live tail.

## Key Technical Decisions

- **One shared card-render architecture.** `renderSessions()` rewrites `card.innerHTML` only on state/pairingUrl change (`renderShell`); after every rewrite, `restoreCardDynamics` reopens a previously-open log disclosure (programmatic `open` fires the existing toggle listener, which refills the log — one fill path). All time-varying content (ticker, age chips, provision counter, sync label) mutates via fresh `data-*` lookups in `updateDynamic` (per-poll + 1s `secondTick`), never innerHTML. This one contract serves ideas 2, 6, and 7 without fighting.
- **Journal logic stays in `sessions.ts`** (it owns the journal file): `readJournal()` (tail-bounded `slice(-2000)`), `recentLaunches(limit)` (deduped by folder+branch+spawnMode, filtered by `existsSync` + `withinRoots`), `listLostSessions()` (memoized at boot; `session.start` within 7 days with no matching `exit`/`kill` and not in the live map). `removeSession` also dismisses lost entries.
- **ntfy is a `notify()` helper beside `journal()`, not inside it** — exit events don't carry enough context; call sites pass the LiveSession. Config block `{url, topic, notifyReady, notifyExit}`; empty topic disables. A `killed` flag set synchronously in `killSession` suppresses notifications for user-initiated kills (exit codes after SIGTERM are platform-dependent). Fire-and-forget fetch with `AbortSignal.timeout(5000)`, double-wrapped so it can never break the exit handler.
- **Dark mode is token overrides only**: first tokenize the remaining hardcoded colors (`--card`, `--panel`, scrim/shadow/toast tokens — zero visual diff), then one `@media (prefers-color-scheme: dark)` block. The QR panel is deliberately light-locked in dark mode (QRs must stay dark-on-light to scan; it reads as a "lit card"). `theme-color` gets media-variant meta tags; manifest colors stay light (splash-only, not media-responsive).
- **Relaunch omits the old session name** — avoids duplicate-name 409s on double relaunch; the server auto-generates.
- **Clipboard/share read from the DOM synchronously** before any await — iOS Safari revokes transient user activation across fetch awaits.

## Implementation Units

### U1. Server: session fields + ntfy + journal queries
**Files:** src/sessions.ts, src/server.ts, config.json
Add `lastOutputAt`/`lastLine` (junk-filtered last meaningful line, ≤120 chars) stamped in `onData`; `killed` flag (stripped from publicView); `NtfyConfig` + `configureNtfy` + `notify()` with ready/exit call sites and kill suppression; `readJournal`/`recentLaunches`/`listLostSessions` + `removeSession` lost-dismissal; routes `GET /api/journal/recent` and `lost` array on `GET /api/sessions`; ntfy block in config.json.
**Test:** curl /api/sessions shows new fields; /api/journal/recent dedupes and clamps limit; orphaned start appears in lost after runner restart; killed session sends no ntfy; unroutable ntfy URL doesn't delay or break lifecycle.

### U2. Client: card architecture + live cards + intent line
**Files:** public/app.js, public/index.html, public/styles.css
renderShell/updateDynamic/restoreCardDynamics/secondTick split; ticker line, age + activity chips, live provision counter; intent-line reframe of the name field ("What's this run for?" + auto-name hint).
**Test:** provision counter ticks; activity flips green on output then decays; open log survives state transitions; blank name still auto-names.

### U3. Client: run history surfaces
**Files:** public/app.js, public/index.html, public/styles.css
Recents strip at roots level (tap → navigate + pre-filled sheet); Relaunch on exited/error cards; lost-session headstone cards (dashed border, "runner restarted — outcome unknown") with Relaunch + Dismiss.
**Test:** recents render and pre-fill; relaunch from exited card starts fresh session; lost card dismisses and stays gone.

### U4. Client: wake-sync + log copy/share/tail
**Files:** public/app.js, public/index.html, public/styles.css
visibilitychange → immediate health+sessions(+browse) refresh; `sync Ns` label (hidden under 10s); copy/share micro-chips in log summary (preventDefault to stop toggle; share hidden when unsupported); `openLogs` set + `fillLog` scroll-preserving tail on the 2.5s tick.
**Test:** backgrounding >10s then foregrounding fires immediate refresh and hides label; copy toasts without toggling; tail requests only while open+live; scroll position preserved unless pinned to bottom.

### U5. Dark mode
**Files:** public/styles.css, public/index.html
Tokenize remaining hardcoded colors (no visual diff), add dark token block per the contrast-checked table (bg #17150f, text #e8e2d0, accent #ff6a3d, stamp #56b16b…), light-lock the QR card, media-variant theme-color metas, bg-layer opacity/blend adjustments.
**Test:** playwright emulate dark → body colors assert; QR panel stays light; toast/segments legible; light mode pixel-identical after tokenization.

## Verification (whole feature)

tsc clean; server restart; playwright pass at 402×874 in both color schemes covering: launch → provision counter → ready ticker/age → kill → relaunch; recents strip; log open across transitions with copy; zero console errors.
