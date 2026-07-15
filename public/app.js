/* groundcontrol mobile UI */
const CLIENT_VERSION = "0.6.0"; // keep in step with main.go's version — healthz mismatch triggers a reload
const $ = (id) => document.getElementById(id);
// tokens travel by copy-paste, which loves to smuggle in zero-width and other
// non-ASCII characters — those make `Bearer <token>` an invalid header value
// and every fetch throws. Keep printable ASCII (minus space) only, applied on
// read too so an already-poisoned stored token heals itself.
const cleanToken = (v) => (v || "").replace(/[^\x21-\x7e]/g, "");
const authToken = () => cleanToken(localStorage.getItem("token"));
const tokenQS = () => (authToken() ? `?token=${encodeURIComponent(authToken())}` : "");
const api = async (path, opts = {}) => {
  const headers = { ...(opts.headers || {}) };
  if (authToken()) headers.authorization = `Bearer ${authToken()}`;
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 401) {
    showAuthGate();
    throw new Error("unauthorized");
  }
  const body = res.headers.get("content-type")?.includes("json") ? await res.json() : await res.text();
  if (!res.ok) throw new Error(body?.error?.message || res.statusText);
  return body;
};

function showAuthGate() {
  $("authGate").hidden = false;
  $("authInput").focus();
}

const state = {
  path: null,
  current: null, // BrowseResult
  sessions: [],
  lost: [],
  landed: [],
  orbit: null, // null = never fetched; [] = fetched, empty (or fetch failed → chip hidden)
  tab: "browse",
  opts: { spawnMode: "same-dir", permissionMode: "default" },
};

/* ---------- preferences & theme (light is the baseline) ---------- */
const prefs = JSON.parse(localStorage.getItem("prefs") || "{}");

function applyTheme() {
  const t = prefs.theme || "light";
  const resolved = t === "system" ? (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light") : t;
  document.documentElement.dataset.theme = resolved;
  document.querySelector('meta[name="theme-color"]').content = resolved === "dark" ? "#17150f" : "#f2eee3";
}
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  if ((prefs.theme || "light") === "system") applyTheme();
});
applyTheme();

function applyHand() {
  document.documentElement.dataset.hand = prefs.hand || "right";
}
applyHand();

function launchDefaults() {
  return { spawnMode: prefs.spawnMode || "same-dir", permissionMode: prefs.permissionMode || "default" };
}

/* ---------- toast ---------- */
let toastTimer;
function toast(msg, isError = false) {
  document.querySelector(".toast")?.remove();
  const el = document.createElement("div");
  el.className = `toast${isError ? " error" : ""}`;
  el.textContent = msg;
  document.body.appendChild(el);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.remove(), 3200);
}

const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

/* ---------- double-tap confirm ----------
   first tap arms the button with warning copy; a second tap within 4s runs
   fn; untouched, the arm dissolves back to the original label. Shared by
   YOLO launches, worktree cleans, and orbit branch sweeps. */
function armConfirm(btn, copy, fn) {
  if (btn.dataset.confirm === "1") {
    btn.dataset.confirm = "";
    btn.classList.remove("warn");
    if (btn._armLabel != null) btn.innerHTML = btn._armLabel;
    fn();
    return;
  }
  btn.dataset.confirm = "1";
  btn._armLabel = btn.innerHTML;
  btn.classList.add("warn");
  btn.innerHTML = copy;
  setTimeout(() => {
    if (btn.dataset.confirm !== "1") return;
    btn.dataset.confirm = "";
    btn.classList.remove("warn");
    btn.innerHTML = btn._armLabel;
  }, 4000);
}

function fmtDur(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d`;
}

/* ---------- browse ---------- */
async function loadFolder(path) {
  const data = path ? await api(`/api/v1/browse?path=${encodeURIComponent(path)}`) : null;
  if (!data) {
    const { roots } = await api("/api/v1/roots");
    state.current = { path: null, parent: null, isGit: false, repoRoot: null, repoName: null, branch: null, folders: roots };
  } else {
    state.current = data;
  }
  state.path = state.current.path;
  renderBrowse();
  if (!state.path) loadRecents().catch(() => {});
}

function crumbParts(path) {
  if (!path) return [{ label: "roots", path: null }];
  const segs = path.split("/").filter(Boolean);
  const parts = [{ label: "roots", path: null }];
  let acc = "";
  for (const seg of segs) {
    acc += "/" + seg;
    parts.push({ label: seg, path: acc });
  }
  if (parts.length <= 4) return parts;
  // keep the tail readable on phones, but never drop the way back to roots:
  // pin the root crumb and collapse hidden ancestors into a tappable ellipsis
  const hiddenParent = parts[parts.length - 4];
  return [parts[0], { label: "…", path: hiddenParent.path }, ...parts.slice(-3)];
}

function renderBrowse() {
  $("upBtn").disabled = !state.path; // stays visible at roots, just inert

  const crumbs = $("crumbs");
  crumbs.innerHTML = "";
  for (const part of crumbParts(state.path)) {
    const b = document.createElement("button");
    b.className = "crumb" + (part.path === state.path ? " current" : "");
    b.textContent = part.label;
    b.onclick = () => loadFolder(part.path).catch((e) => toast(e.message, true));
    crumbs.appendChild(b);
  }

  const list = $("folderList");
  list.innerHTML = "";
  state.current.folders.forEach((f, i) => {
    const li = document.createElement("li");
    const row = document.createElement("button");
    row.className = "folder-row";
    row.style.animationDelay = `${Math.min(i * 28, 280)}ms`;
    row.innerHTML = `
      <span class="folder-glyph${f.isGit ? " git" : ""}">${f.isGit ? gitGlyph() : "•"}</span>
      <span class="folder-name">${esc(f.name.split("/").pop() || f.name)}</span>
      ${f.branch ? `<span class="branch-chip">${esc(f.branch)}</span>` : ""}
      <svg class="folder-chevron" viewBox="0 0 24 24" width="15" height="15"><path d="M9 6l6 6-6 6" stroke="currentColor" stroke-width="1.8" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
    row.onclick = () => loadFolder(f.path).catch((e) => toast(e.message, true));
    li.appendChild(row);
    list.appendChild(li);
  });

  $("mission").hidden = !!state.path; // roots-level only, same guard as recents
  $("recents").hidden = !!state.path || !state.recentLoaded || !state.recent?.length;
  syncFab();
}

function syncFab() {
  const bar = $("launchBar");
  const show = state.tab === "browse" && !!state.path;
  bar.style.display = show ? "" : "none";
  if (state.path) {
    $("launchBarFolder").textContent = state.path.split("/").pop();
    $("launchBarBranch").textContent = state.current?.branch || "";
  }
}

function gitGlyph() {
  return `<svg viewBox="0 0 24 24" width="17" height="17"><path fill="currentColor" d="M21.6 11.2 12.8 2.4a1.4 1.4 0 0 0-2 0L9 4.2l2.3 2.3a1.7 1.7 0 0 1 2.1 2.1l2.2 2.2a1.7 1.7 0 1 1-1 1l-2-2v5.4a1.7 1.7 0 1 1-1.4 0V9.6a1.7 1.7 0 0 1-.7-2.8L8.2 4.9 2.4 10.8a1.4 1.4 0 0 0 0 2l8.8 8.8a1.4 1.4 0 0 0 2 0l8.4-8.4a1.4 1.4 0 0 0 0-2z"/></svg>`;
}

/* ---------- recents ---------- */
async function loadRecents() {
  // limit=20 (server max): the full feed doubles as the mission input's
  // suggestion pool — one fetch per arrival at roots covers both
  const { recent } = await api("/api/v1/journal/recent?limit=20");
  state.recent = recent;
  state.recentLoaded = true;
  const row = $("recentsRow");
  row.innerHTML = "";
  for (const r of recent.slice(0, 6)) {
    const card = document.createElement("button");
    card.className = "recent-card" + (r.stale ? " stale" : "");
    card.innerHTML = `
      <span class="recent-folder">${esc(r.folder.split("/").pop())}</span>
      ${r.stale ? `<span class="stale-chip">branch gone</span>` : r.branch ? `<span class="branch-chip">${esc(r.branch)}</span>` : ""}
      <span class="recent-mode">${esc(r.spawnMode)} · ${esc(r.permissionMode)}</span>`;
    card.onclick = () => relaunchFromRecent(r);
    row.appendChild(card);
  }
  $("recents").hidden = !!state.path || recent.length === 0;
}

async function relaunchFromRecent(cfg) {
  try {
    await loadFolder(cfg.folder);
    if (cfg.stale) {
      // the branch this config used no longer exists — degrade honestly
      toast(`branch ${cfg.branch} no longer exists — defaulting to in-folder`, true);
      openSheet({ spawnMode: "same-dir", permissionMode: cfg.permissionMode });
    } else {
      openSheet({ spawnMode: cfg.spawnMode, permissionMode: cfg.permissionMode, branch: cfg.branch ?? undefined });
    }
  } catch (e) {
    toast(e.message, true);
  }
}

/* ---------- repo search (browse) ---------- */
// the currently rendered matches — Enter activates when exactly one is visible
let missionMatches = [];
let missionTimer;

// every typed token must land somewhere in folder basename, mission name, or
// branch; newest-first journal order wins ties, one result per folder
function missionSuggestions(query) {
  const tokens = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (!tokens.length) return [];
  const seen = new Set();
  const out = [];
  for (const r of state.recent || []) {
    if (seen.has(r.folder)) continue;
    const hay = [r.folder.split("/").pop(), r.name, r.branch].filter(Boolean).map((s) => s.toLowerCase());
    if (tokens.every((t) => hay.some((h) => h.includes(t)))) {
      seen.add(r.folder);
      out.push(r);
      if (out.length === 3) break;
    }
  }
  return out;
}

function renderMissionChips() {
  const box = $("missionChips");
  box.innerHTML = "";
  // two repos named "api" need their parent dir to tell them apart
  const baseCounts = {};
  for (const r of missionMatches) {
    const b = r.folder.split("/").pop();
    baseCounts[b] = (baseCounts[b] || 0) + 1;
  }
  for (const r of missionMatches) {
    const base = r.folder.split("/").pop();
    const parent = r.folder.split("/").slice(-2, -1)[0];
    const chip = document.createElement("button");
    chip.className = "mission-chip" + (r.stale ? " stale" : "");
    chip.setAttribute("role", "option");
    chip.innerHTML = `
      <span class="chip-name">${esc(base)}</span>
      ${baseCounts[base] > 1 && parent ? `<span class="chip-dir">— ${esc(parent)}/</span>` : ""}
      ${r.stale ? `<span class="stale-chip">branch gone</span>` : r.branch ? `<span class="branch-chip">${esc(r.branch)}</span>` : ""}`;
    chip.onclick = () => launchFromMission(r);
    box.appendChild(chip);
  }
  const open = missionMatches.length > 0;
  box.hidden = !open;
  $("missionInput").setAttribute("aria-expanded", String(open));
}

// a search hit is just a recent launch — reuse its relaunch path; the thought
// belongs in the sheet's prompt field, not the search box
async function launchFromMission(r) {
  $("missionInput").value = "";
  missionMatches = [];
  renderMissionChips();
  await relaunchFromRecent(r);
}

$("missionInput").addEventListener("input", () => {
  clearTimeout(missionTimer);
  missionTimer = setTimeout(() => {
    missionMatches = missionSuggestions($("missionInput").value);
    renderMissionChips();
  }, 150);
});
$("missionInput").addEventListener("keydown", (e) => {
  if (e.key !== "Enter") return;
  // flush the debounce so Enter acts on what's typed, not what was rendered
  clearTimeout(missionTimer);
  missionMatches = missionSuggestions($("missionInput").value);
  renderMissionChips();
  if (missionMatches.length === 1) launchFromMission(missionMatches[0]);
});

/* ---------- launch sheet ---------- */
function syncBranchField() {
  // every control stays on screen in every mode — capability only changes copy + enabled state
  const git = !!state.current?.isGit;
  const noBranches = git && state.branchCount === 0; // repo with no commits yet
  const worktree = state.opts.spawnMode === "worktree";

  document.querySelectorAll("#optSpawn button").forEach((b) => (b.disabled = !git || noBranches));
  $("optBranch").disabled = !git || noBranches;

  $("spawnHint").textContent = !git
    ? "not a git folder — runs in place"
    : noBranches
      ? "no commits yet — worktrees need a branch"
      : worktree
        ? "isolated worktree — this folder stays untouched"
        : "runs directly in this folder";

  $("branchLabel").textContent = worktree ? "Base branch" : "Branch";
  if (!git) {
    $("branchHint").textContent = "no git here — nothing to branch";
    return;
  }
  if (noBranches) {
    $("branchHint").textContent = "no branches yet — make a first commit";
    return;
  }
  const note = worktree
    ? state.current?.repoRoot && state.current.repoRoot !== state.current.path
      ? `worktree of ${state.current.repoName}`
      : "worktree is created from this branch"
    : "checked out in the folder at launch";
  const n = state.branchCount;
  $("branchHint").textContent = n ? `${note} · ${n} branch${n === 1 ? "" : "es"}` : note;
}

async function loadBranches(folder) {
  const select = $("optBranch");
  select.innerHTML = "";
  try {
    const { branches } = await api(`/api/v1/branches?path=${encodeURIComponent(folder)}`);
    state.branchCount = branches.length;
    if (branches.length === 0) {
      // repo with no commits yet: no branches to pick and worktrees are impossible
      if (state.opts.spawnMode === "worktree") state.opts.spawnMode = "same-dir";
      syncSegment("optSpawn", state.opts.spawnMode);
      syncBranchField();
      return;
    }
    for (const b of branches) {
      const opt = document.createElement("option");
      opt.value = b;
      opt.textContent = b;
      select.appendChild(opt);
    }
    // saved choice if the branch still exists, else the repo's current branch
    const preferred = [state.opts.branch, state.current.branch].find((b) => b && branches.includes(b));
    if (preferred) select.value = preferred;
    syncBranchField();
  } catch {
    $("branchHint").textContent = "could not load branches";
  }
}

function openSheet(prefill) {
  const folder = state.path;
  if (!folder) return;
  $("sheetFolder").textContent = folder.split("/").pop();
  $("sheetPath").textContent = folder;

  const saved = JSON.parse(localStorage.getItem(`opts:${folder}`) || "null");
  state.opts = { ...(prefill || saved || launchDefaults()) };
  // name rides the prefill for one mission only — never into the per-repo opts
  delete state.opts.name;

  // every control stays on screen; a non-git folder gets them disabled with the reason in the hint
  const git = state.current.isGit;
  if (!git) {
    state.opts.spawnMode = "same-dir";
    // clear the select so a stale branch from the last git folder can't appear or ride the POST
    $("optBranch").innerHTML = "";
    $("optBranch").value = "";
    state.branchCount = 0;
  } else {
    state.branchCount = undefined; // unknown until this folder's branches load
  }
  syncSegment("optSpawn", state.opts.spawnMode);
  syncSegment("optPerm", state.opts.permissionMode);
  syncBranchField();
  if (git) loadBranches(folder);

  $("optName").value = prefill?.name || "";
  $("sheet").hidden = false;
  $("scrim").hidden = false;
  requestAnimationFrame(() => {
    $("sheet").classList.add("show");
    $("scrim").classList.add("show");
  });
}

function closeSheet() {
  $("sheet").classList.remove("show");
  $("scrim").classList.remove("show");
  setTimeout(() => {
    $("sheet").hidden = true;
    $("scrim").hidden = true;
  }, 340);
}

function syncSegment(id, value) {
  document.querySelectorAll(`#${id} button`).forEach((b) => b.classList.toggle("active", b.dataset.value === value));
}

function wireSegment(id, key, onChange) {
  document.querySelectorAll(`#${id} button`).forEach((b) => {
    b.onclick = () => {
      if (b.disabled) return;
      state.opts[key] = b.dataset.value;
      syncSegment(id, b.dataset.value);
      onChange?.();
    };
  });
}

function launch() {
  const btn = $("launchBtn");
  // YOLO in a non-git folder has no VCS undo — require a deliberate second tap
  if (state.opts.permissionMode === "bypassPermissions" && !state.current.isGit) {
    armConfirm(btn, `⚠ no git undo here — tap again`, doLaunch);
    return;
  }
  doLaunch();
}

async function doLaunch() {
  const btn = $("launchBtn");
  btn.disabled = true;
  btn.innerHTML = `<span class="cta-glyph">◴</span> Launching…`;
  try {
    const branch = state.current.isGit ? $("optBranch").value || undefined : undefined;
    state.opts.branch = branch;
    localStorage.setItem(`opts:${state.path}`, JSON.stringify(state.opts)); // name never saved — per mission, not per repo
    await api("/api/v1/sessions", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        folder: state.path,
        name: $("optName").value || undefined,
        spawnMode: state.opts.spawnMode,
        branch,
        permissionMode: state.opts.permissionMode,
      }),
    });
    closeSheet();
    switchTab("sessions");
    toast("Session starting");
  } catch (e) {
    toast(e.message, true);
  } finally {
    btn.disabled = false;
    btn.innerHTML = `<span class="cta-glyph">▶</span> Launch session`;
  }
}

/* ---------- sessions ---------- */
// Ids we've killed but whose PTY may not have exited yet. While an id is here,
// a stale "ready" from the server is held at "exited" to avoid a flicker.
const killing = new Set();
// Ids whose runtime-log <details> is currently open — preserved across innerHTML rewrites.
const openLogs = new Set();
// Same trick for the conversation <details>.
const openConvos = new Set();
// Ids whose pairing-QR <details> is expanded — same trick, so a tap survives re-renders.
const openQrs = new Set();
let lastSyncAt = Date.now();

// After a kill, poll a few times so the card lands on the real terminal
// state (exited vs error) rather than the optimistic guess.
function reconcile(id, tries = 6) {
  if (tries <= 0) {
    killing.delete(id);
    return;
  }
  setTimeout(async () => {
    await refreshSessions();
    const s = state.sessions.find((x) => x.id === id);
    if (s && (s.state === "exited" || s.state === "error")) killing.delete(id);
    else reconcile(id, tries - 1);
  }, 400);
}

async function refreshSessions() {
  try {
    const { sessions, lost, landed } = await api("/api/v1/sessions");
    state.sessions = sessions.map((s) =>
      killing.has(s.id) && s.state !== "exited" && s.state !== "error"
        ? { ...s, state: "exited", pairingUrl: null }
        : s
    );
    state.lost = lost || [];
    state.landed = landed || [];
    lastSyncAt = Date.now();
    renderSessions();
  } catch {
    /* runner unreachable; health dot covers it */
  }
}

/* ---------- attention triage ---------- */
// ready + output younger than this reads "working"; past it, whoever spoke
// last in the transcript decides between "your move" and "quiet"
const QUIET_MS = 30000;
// transcript fan-out cap: one fetch per ready session per cycle, first N only
const SNIPPET_POLL_CAP = 5;
let snippetCapWarned = false;
// id → {lastRole, lastAssistantText}, derived from the newest transcript
const tails = new Map();

// tool-call stubs ("→ ToolName" lines) are working signals, not turn-ends
function isToolStub(text) {
  const lines = String(text || "").split("\n").map((l) => l.trim()).filter(Boolean);
  return lines.length > 0 && lines.every((l) => l.startsWith("→ "));
}

// who spoke last (and what claude last said) in the newest transcript,
// skipping tool-stub-only messages — drives the status verb + card snippet
function deriveTail(transcripts) {
  const t = transcripts[transcripts.length - 1];
  if (!t) return null;
  const msgs = (t.messages || []).filter((m) => m.role !== "assistant" || !isToolStub(m.text));
  const last = msgs[msgs.length - 1];
  if (!last) return null;
  const lastAssistant = [...msgs].reverse().find((m) => m.role === "assistant");
  return { lastRole: last.role, lastAssistantText: lastAssistant?.text || "" };
}

// the card face's one-word answer to "does anything need me?"
function verbFor(s, now = Date.now()) {
  if (s.state === "starting") return "igniting";
  if (s.state === "error") return "scrubbed";
  if (s.state === "exited") return s.exitCode === 0 ? "landed" : "failed";
  const outAge = s.lastOutputAt ? now - Date.parse(s.lastOutputAt) : Infinity;
  if (outAge < QUIET_MS) return "working";
  return tails.get(s.id)?.lastRole === "assistant" ? "your move" : "quiet";
}

// ordering group ranks: 0 your-move · 1 starting · 2 working · 3 quiet ·
// 4 ended (exited/error/landed/lost); recency breaks ties within a group
function attentionRank(verb) {
  if (verb === "your move") return 0;
  if (verb === "igniting") return 1;
  if (verb === "working") return 2;
  if (verb === "quiet") return 3;
  return 4;
}

/* ---------- debrief face (ended sessions) ---------- */
const BRANCH_STATE_LABEL = { merged: "merged", "in-orbit": "in orbit", "worktree-kept": "worktree kept" };

function diffStatLine(d) {
  const parts = [`${d.filesChanged} file${d.filesChanged === 1 ? "" : "s"}`, `+${d.insertions} −${d.deletions}`];
  if (d.uncommitted) parts.push(`${d.uncommitted} uncommitted`);
  return parts.join(" · ");
}

// diff stat + branch-state chip; renders only when the data exists — no
// placeholders on same-dir runs
function debriefHTML(debrief) {
  return debrief
    ? `<div class="debrief-stat">
        <span class="debrief-diff">${diffStatLine(debrief)}</span>
        <span class="orbit-state ${esc(debrief.branchState)}">${esc(BRANCH_STATE_LABEL[debrief.branchState] || debrief.branchState)}</span>
      </div>`
    : "";
}

// contextual cleanup for a live exited/error card. Landed journal entries
// never get the worktree route (no worktreePath survives the restart) —
// Settings owns kept worktrees there.
function cleanupBtnHTML(s) {
  const bs = s.debrief?.branchState;
  if (bs === "in-orbit") return `<button class="btn" data-orbit-clean="${s.id}">Clean branch</button>`;
  if (bs === "worktree-kept" && s.worktreePath) return `<button class="btn" data-wt-clean="${s.id}">Clean worktree</button>`;
  return "";
}

// wtBranch is gc/<id> or gc/<slug>-<id> — recover the owning session id
function idMatchesBranch(id, branch) {
  return branch === `gc/${id}` || branch.endsWith(`-${id}`);
}

// a swept branch makes the captured "in orbit" chip history — flip it in
// place (journal data won't; the exit entry is immutable)
function patchSweptDebrief(branch) {
  $("sessionList").querySelectorAll("[data-id]").forEach((card) => {
    if (!idMatchesBranch(card.dataset.id, branch)) return;
    const chip = card.querySelector(".orbit-state.in-orbit");
    if (chip) {
      chip.className = "orbit-state merged";
      chip.textContent = "merged";
    }
    card.querySelector("[data-orbit-clean]")?.remove();
  });
}

// CONTRACT: card.innerHTML is rewritten ONLY here, keyed on the card "face"
// (state/pairingUrl/exitCode/debrief — all step changes, never time-varying).
// Everything time-varying updates via data-* lookups in updateDynamic().
function renderShell(card, s) {
  const logWasOpen = openLogs.has(s.id);
  const convoWasOpen = openConvos.has(s.id);
  const qrWasOpen = openQrs.has(s.id);
  const shareBtn = navigator.share ? `<button class="log-tool" data-share-log="${s.id}">share</button>` : "";
  card.innerHTML = `
    <div class="session-head">
      <span class="session-name">${esc(s.name)}</span>
      <span class="state-pill ${s.state}" data-verb>${esc(verbFor(s))}</span>
    </div>
    <div class="session-path">${esc(s.folder)}</div>
    <div class="session-ticker" data-ticker></div>
    ${s.state === "ready" ? `<div class="session-snippet" data-snippet hidden></div>` : ""}
    <div class="session-meta">
      <span class="meta-chip">${s.spawnMode}</span>
      ${s.branch ? `<span class="meta-chip branch">⎇ ${esc(s.branch)}</span>` : ""}
      <span class="meta-chip">${s.permissionMode}</span>
      <span class="meta-chip age" data-age></span>
      <span class="meta-chip activity" data-activity hidden></span>
    </div>
    ${s.pairingUrl && s.state === "ready" ? `
      <a class="card-cta" href="${esc(s.pairingUrl)}" target="_blank" rel="noopener">enter cockpit <span class="cta-glyph">→</span></a>
      <details class="qr-details" data-qr="${s.id}">
        <summary class="qr-summary">
          <span class="qr-summary-label">
            <span class="qr-summary-title">pair another device</span>
            <span class="qr-summary-sub">tap for qr code</span>
          </span>
          <span class="qr-chevron">›</span>
        </summary>
        <div class="qr-wrap">
          <img class="qr-img" alt="Pairing QR code" src="/api/v1/sessions/${s.id}/qr${tokenQS()}" />
          <span class="qr-hint">Claude app · camera</span>
          <span class="bracket bl"></span><span class="bracket br"></span>
        </div>
      </details>
      <div class="session-actions">
        <button class="btn danger" data-kill="${s.id}">Kill</button>
      </div>` : s.state === "starting" ? `
      <div class="launch-seq">
        <div class="seq-stage done"><span class="seq-mark">✓</span><span class="seq-label">request accepted</span></div>
        <div class="seq-stage active"><span class="seq-mark"><span class="provision-dot"></span></span><span class="seq-label">agent igniting</span><span class="seq-elapsed" data-provision></span></div>
        <div class="seq-stage pending"><span class="seq-mark">·</span><span class="seq-label">telemetry acquired</span></div>
      </div>
      <div class="session-actions">
        <button class="btn danger" data-kill="${s.id}">Kill</button>
      </div>` : s.state === "error" ? `
      <div class="launch-seq failed">
        <div class="seq-stage done"><span class="seq-mark">✓</span><span class="seq-label">request accepted</span></div>
        <div class="seq-stage failed"><span class="seq-mark">✕</span><span class="seq-label">ignition failed — launch scrubbed</span></div>
      </div>
      ${debriefHTML(s.debrief)}
      <div class="session-actions">
        <button class="btn" data-relaunch="${s.id}">Relaunch</button>
        ${cleanupBtnHTML(s)}
        <button class="btn" data-remove="${s.id}">Dismiss</button>
      </div>` : `
      ${debriefHTML(s.debrief)}
      <div class="session-actions">
        <button class="btn" data-relaunch="${s.id}">Relaunch</button>
        ${cleanupBtnHTML(s)}
        <button class="btn" data-remove="${s.id}">Dismiss</button>
      </div>`}
    <details class="session-log session-convo"><summary>conversation</summary><div class="convo" data-convo="${s.id}">…</div></details>
    <details class="session-log"><summary>runtime log
      <span class="log-tools"><button class="log-tool" data-copy-log="${s.id}">copy</button>${shareBtn}</span>
    </summary><pre data-log="${s.id}">…</pre></details>`;
  if (logWasOpen) {
    const details = card.querySelector(".session-log:not(.session-convo)");
    if (details) details.open = true; // fires the toggle listener, which refills the log
  }
  if (convoWasOpen) {
    const details = card.querySelector(".session-convo");
    if (details) details.open = true;
  }
  if (qrWasOpen) {
    const qr = card.querySelector(".qr-details");
    if (qr) qr.open = true; // re-adds to openQrs via the toggle listener; harmless
  }
}

function renderLostShell(card, l) {
  card.innerHTML = `
    <div class="session-head">
      <span class="session-name">${esc(l.name)}</span>
      <span class="state-pill lost">lost</span>
    </div>
    <div class="lost-note">runner restarted — outcome unknown</div>
    <div class="session-path">${esc(l.folder)}</div>
    <div class="session-meta">
      <span class="meta-chip">${esc(l.spawnMode)}</span>
      ${l.branch ? `<span class="meta-chip branch">⎇ ${esc(l.branch)}</span>` : ""}
      <span class="meta-chip">${esc(l.permissionMode)}</span>
    </div>
    <div class="session-actions">
      <button class="btn" data-relaunch="${l.id}">Relaunch</button>
      <button class="btn" data-remove="${l.id}">Dismiss</button>
    </div>`;
}

// journal-derived debrief for an ended session the manager no longer lists.
// Static by nature (the journal never mutates) — rendered once, keyed on id.
function renderLandedShell(card, l) {
  const failed = l.exitCode !== 0; // null exit code (unknown) reads failed, not landed
  card.innerHTML = `
    <div class="session-head">
      <span class="session-name">${esc(l.name)}</span>
      <span class="state-pill ${failed ? "error" : "exited"}">${failed ? "failed" : "landed"}</span>
    </div>
    <div class="session-path">${esc(l.folder)}</div>
    <div class="session-meta">
      <span class="meta-chip">${esc(l.spawnMode)}</span>
      ${l.branch ? `<span class="meta-chip branch">⎇ ${esc(l.branch)}</span>` : ""}
      <span class="meta-chip">${esc(l.permissionMode)}</span>
      <span class="meta-chip age">ran ${fmtDur(Date.parse(l.exitedAt) - Date.parse(l.startedAt))}</span>
    </div>
    ${debriefHTML(l.debrief)}
    <div class="session-actions">
      <button class="btn" data-relaunch="${l.id}">Relaunch</button>
      ${l.debrief?.branchState === "in-orbit" ? `<button class="btn" data-orbit-clean="${l.id}">Clean branch</button>` : ""}
    </div>`;
}

function updateDynamic(card, s) {
  const now = Date.now();
  const started = Date.parse(s.startedAt);
  const ticker = card.querySelector("[data-ticker]");
  if (ticker && ticker.textContent !== (s.lastLine || "")) ticker.textContent = s.lastLine || "";

  const pill = card.querySelector("[data-verb]");
  if (pill) {
    const v = verbFor(s, now);
    if (pill.textContent !== v) pill.textContent = v;
    pill.classList.toggle("your-move", v === "your move");
  }

  // last-assistant snippet (ready cards): textContent only — user-derived text
  const snip = card.querySelector("[data-snippet]");
  if (snip) {
    const text = tails.get(s.id)?.lastAssistantText || "";
    if (snip.textContent !== text) snip.textContent = text;
    snip.hidden = !text; // empty until the first transcript fetch — no placeholder
  }

  const age = card.querySelector("[data-age]");
  if (age) {
    age.textContent =
      s.state === "exited" || s.state === "error"
        ? `ran ${fmtDur((s.exitedAt ? Date.parse(s.exitedAt) : now) - started)}`
        : `up ${fmtDur(now - started)}`;
  }

  const activity = card.querySelector("[data-activity]");
  if (activity) {
    if (s.state === "ready" && s.lastOutputAt) {
      const diff = now - Date.parse(s.lastOutputAt);
      activity.hidden = false;
      activity.textContent = diff < 60000 ? `output ${Math.floor(diff / 1000)}s ago` : `quiet ${fmtDur(diff)}`;
      activity.classList.toggle("hot", diff < 10000);
    } else {
      activity.hidden = true;
    }
  }

  // elapsed ticker on the active launch-sequence stage (starting cards only)
  const provision = card.querySelector("[data-provision]");
  if (provision) provision.textContent = `t+${fmtDur(now - started)}`;
}

// re-slot cards only when the relative order actually changed; a re-inserted
// node restarts the card-in entrance animation, so moved nodes get it killed
// and only genuinely new cards (card._new) animate
function applyOrder(list, ids) {
  const current = [...list.children].map((c) => c.dataset.id);
  const changed = ids.length !== current.length || ids.some((id, i) => id !== current[i]);
  if (changed) {
    for (const id of ids) {
      const card = list.querySelector(`[data-id="${id}"]`);
      if (!card) continue;
      if (!card._new) card.style.animation = "none";
      list.appendChild(card);
    }
  }
  for (const card of list.children) card._new = false;
}

function renderSessions() {
  const live = state.sessions.filter((s) => s.state === "starting" || s.state === "ready");
  const badge = $("sessionCount");
  badge.hidden = live.length === 0;
  badge.textContent = live.length;

  // journal debriefs for ids the manager no longer lists (dismissed exits,
  // restarts); the server already excludes live ids — the filter is belt+braces
  const landed = (state.landed || []).filter((l) => !state.sessions.some((s) => s.id === l.id));

  $("sessionsEmpty").style.display = state.sessions.length || state.lost.length || landed.length ? "none" : "";
  const list = $("sessionList");

  for (const s of state.sessions) {
    let card = list.querySelector(`[data-id="${s.id}"]`);
    if (!card) {
      card = document.createElement("li");
      card.className = "session-card";
      card.dataset.id = s.id;
      card._new = true;
      list.prepend(card);
    }
    // exitCode/debrief join the key: a kill flips the card to exited
    // optimistically, before the server payload carries either — without
    // them the later real exit would never rewrite in the debrief face
    const face = `${s.state}|${s.pairingUrl}|${s.exitCode ?? ""}|${s.debrief?.branchState ?? ""}`;
    if (card.dataset.face !== face) {
      card.dataset.face = face;
      card.dataset.state = s.state;
      card.dataset.url = String(s.pairingUrl);
      renderShell(card, s);
    }
    updateDynamic(card, s);
  }

  for (const l of landed) {
    let card = list.querySelector(`[data-id="${l.id}"]`);
    if (!card) {
      card = document.createElement("li");
      card.className = "session-card landed";
      card.dataset.id = l.id;
      card.dataset.state = "landed";
      card._new = true;
      list.appendChild(card);
      renderLandedShell(card, l);
    }
  }

  for (const l of state.lost) {
    let card = list.querySelector(`[data-id="${l.id}"]`);
    if (!card) {
      card = document.createElement("li");
      card.className = "session-card lost";
      card.dataset.id = l.id;
      card.dataset.state = "lost";
      card._new = true;
      list.appendChild(card);
      renderLostShell(card, l);
    }
  }

  // drop cards for sessions that vanished
  list.querySelectorAll("[data-id]").forEach((card) => {
    const id = card.dataset.id;
    if (!state.sessions.some((s) => s.id === id) && !state.lost.some((l) => l.id === id) && !landed.some((l) => l.id === id)) {
      openLogs.delete(id);
      openConvos.delete(id);
      convoKeys.delete(id);
      openQrs.delete(id);
      tails.delete(id);
      card.remove();
    }
  });

  // attention-first ordering over the combined list
  const now = Date.now();
  const order = [
    ...state.sessions.map((s) => ({ id: s.id, rank: attentionRank(verbFor(s, now)), at: Date.parse(s.exitedAt || s.startedAt) || 0 })),
    ...landed.map((l) => ({ id: l.id, rank: 4, at: Date.parse(l.exitedAt) || 0 })),
    ...state.lost.map((l) => ({ id: l.id, rank: 4, at: Date.parse(l.at) || 0 })),
  ];
  order.sort((a, b) => a.rank - b.rank || b.at - a.at);
  applyOrder(list, order.map((o) => o.id));
}

/* ---------- log tail ---------- */
function fillLog(pre, text) {
  if (pre.textContent === text) return;
  const wasPlaceholder = pre.textContent === "…";
  const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
  pre.textContent = text;
  if (atBottom || wasPlaceholder) pre.scrollTop = pre.scrollHeight;
}

/* ---------- conversation view ---------- */
// last rendered payload per session — a re-render only happens when the
// transcript actually changed, so scroll position survives idle polls
const convoKeys = new Map();

function renderConvo(box, transcripts) {
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 4;
  const wasPlaceholder = box.textContent === "…";
  box.innerHTML = "";
  if (!transcripts.length) {
    const empty = document.createElement("div");
    empty.className = "convo-empty";
    empty.textContent = "no conversation yet — send a prompt from the Claude app";
    box.appendChild(empty);
    return;
  }
  transcripts.forEach((t, i) => {
    if (transcripts.length > 1) {
      const div = document.createElement("div");
      div.className = "convo-divider";
      div.textContent = `conversation ${i + 1}`;
      box.appendChild(div);
    }
    for (const m of t.messages) {
      const el = document.createElement("div");
      el.className = `convo-msg ${m.role === "user" ? "user" : "assistant"}`;
      el.textContent = m.text;
      box.appendChild(el);
    }
  });
  if (atBottom || wasPlaceholder) box.scrollTop = box.scrollHeight;
}

async function refreshConvo(id) {
  try {
    const { transcripts } = await api(`/api/v1/sessions/${id}/transcript`);
    const key = JSON.stringify(transcripts);
    if (convoKeys.get(id) === key) return; // unchanged payload — skip all repaints
    convoKeys.set(id, key);
    tails.set(id, deriveTail(transcripts));
    const box = document.querySelector(`.convo[data-convo="${id}"]`); // fresh lookup — survives rewrites
    if (box && openConvos.has(id)) renderConvo(box, transcripts);
    renderSessions(); // new tail can flip quiet↔your-move: verb, snippet, and order
  } catch {
    /* transient failure: keep the last good view */
  }
}

// transcripts feed two consumers on the same 2.5s cadence: open conversation
// views, and the card-face snippet/verb of every visible ready session
async function tailConvos() {
  const ids = new Set();
  if (state.tab === "sessions" && !document.hidden) {
    const ready = state.sessions.filter((s) => s.state === "ready");
    for (const s of ready.slice(0, SNIPPET_POLL_CAP)) ids.add(s.id);
    if (ready.length > SNIPPET_POLL_CAP && !snippetCapWarned) {
      snippetCapWarned = true;
      console.warn(`snippet polling capped at the first ${SNIPPET_POLL_CAP} ready sessions`);
    }
  }
  for (const id of [...openConvos]) {
    const s = state.sessions.find((x) => x.id === id);
    if (!s) {
      openConvos.delete(id);
      continue;
    }
    if (s.state !== "starting" && s.state !== "ready") continue; // frozen but stays open
    ids.add(id);
  }
  for (const id of ids) await refreshConvo(id);
}

async function tailLogs() {
  for (const id of [...openLogs]) {
    const s = state.sessions.find((x) => x.id === id);
    if (!s) {
      openLogs.delete(id);
      continue;
    }
    if (s.state !== "starting" && s.state !== "ready") continue; // frozen but stays open
    try {
      const text = await api(`/api/v1/sessions/${id}/log`);
      const pre = document.querySelector(`pre[data-log="${id}"]`); // fresh lookup — survives rewrites
      if (pre && openLogs.has(id)) fillLog(pre, text);
    } catch {
      /* transient failure: keep the last good log */
    }
  }
}

document.addEventListener("toggle", async (e) => {
  const qrId = e.target.dataset?.qr;
  if (qrId) {
    if (e.target.open) openQrs.add(qrId);
    else openQrs.delete(qrId);
    return;
  }
  const convoBox = e.target.querySelector?.(".convo[data-convo]");
  if (convoBox) {
    const id = convoBox.dataset.convo;
    if (e.target.open) {
      openConvos.add(id);
      convoKeys.delete(id); // force a repaint even if the payload is unchanged
      refreshConvo(id);
    } else {
      openConvos.delete(id);
    }
    return;
  }
  const pre = e.target.querySelector?.("pre[data-log]");
  if (!pre) return;
  const id = pre.dataset.log;
  if (e.target.open) {
    openLogs.add(id);
    const text = await api(`/api/v1/sessions/${id}/log`).catch(() => "log unavailable");
    fillLog(pre, text);
  } else {
    openLogs.delete(id);
  }
}, true);

/* ---------- actions ---------- */
document.addEventListener("click", async (e) => {
  const copyBtn = e.target.closest?.("[data-copy-log]");
  const shareBtn = e.target.closest?.("[data-share-log]");
  const relaunch = e.target.closest?.("[data-relaunch]");
  const kill = e.target.closest?.("[data-kill]");
  const remove = e.target.closest?.("[data-remove]");
  const orbitClean = e.target.closest?.("[data-orbit-clean]");
  const wtClean = e.target.closest?.("[data-wt-clean]");

  if (orbitClean) {
    const id = orbitClean.dataset.orbitClean;
    // double-tap: the sweep is always a safe delete, and an unmerged branch
    // will 409 — arming makes that round-trip a deliberate act
    armConfirm(orbitClean, "Sweep branch?", async () => {
      const cfg = state.sessions.find((x) => x.id === id) || state.landed.find((l) => l.id === id);
      if (!cfg) return;
      try {
        if (!state.orbit) await loadOrbit(); // debrief cards can precede any orbit fetch
        const entry = (state.orbit || []).find(
          (o) => idMatchesBranch(id, o.branch) && (cfg.folder === o.repo || cfg.folder.startsWith(o.repo + "/"))
        );
        if (!entry) {
          toast("branch not in orbit — already swept?", true);
          loadOrbit();
          return;
        }
        await api(`/api/v1/orbit?repo=${encodeURIComponent(entry.repo)}&branch=${encodeURIComponent(entry.branch)}`, { method: "DELETE" });
        toast("Branch swept");
        state.orbit = state.orbit.filter((o) => o !== entry);
        renderOrbit();
        patchSweptDebrief(entry.branch);
        loadOrbit(); // re-sync with the server's view
        refreshSessions();
      } catch (err) {
        toast(err.message, true); // 409 branch_not_merged / branch_held speak here
      }
    });
    return;
  }

  if (wtClean) {
    const id = wtClean.dataset.wtClean;
    // kept worktrees are kept because they're dirty — discarding takes two taps
    armConfirm(wtClean, "Discard changes?", async () => {
      const s = state.sessions.find((x) => x.id === id);
      if (!s?.worktreePath) return;
      try {
        await api(`/api/v1/worktrees?path=${encodeURIComponent(s.worktreePath)}`, { method: "DELETE" });
        toast("Worktree cleaned");
        wtClean.remove(); // the captured debrief won't change; the action is spent
        loadOrbit();
        refreshSessions();
      } catch (err) {
        toast(err.message, true);
      }
    });
    return;
  }

  if (copyBtn || shareBtn) {
    e.preventDefault(); // don't toggle the details
    const id = (copyBtn || shareBtn).dataset.copyLog || (copyBtn || shareBtn).dataset.shareLog;
    const pre = document.querySelector(`pre[data-log="${id}"]`);
    // read synchronously — an await here would consume iOS's transient user activation
    const text = pre && pre.textContent !== "…" ? pre.textContent : "";
    const s = state.sessions.find((x) => x.id === id);
    if (!text) return toast("open the log first", true);
    if (copyBtn) {
      try {
        await navigator.clipboard.writeText(text);
        toast("Log copied");
      } catch {
        toast("Copy failed", true);
      }
    } else {
      try {
        await navigator.share({ title: `${s?.name ?? "session"} · runtime log`, text });
      } catch (err) {
        if (err.name !== "AbortError") toast("Share failed", true);
      }
    }
    return;
  }

  if (relaunch) {
    const id = relaunch.dataset.relaunch;
    const cfg = state.sessions.find((x) => x.id === id) || state.lost.find((l) => l.id === id) || state.landed.find((l) => l.id === id);
    if (!cfg) return;
    try {
      await api("/api/v1/sessions", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          folder: cfg.folder,
          spawnMode: cfg.spawnMode,
          branch: cfg.branch ?? undefined,
          permissionMode: cfg.permissionMode,
          // name omitted deliberately: reusing it risks a duplicate-name conflict
        }),
      });
      toast("Session starting");
      refreshSessions();
    } catch (err) {
      toast(err.message, true);
    }
  }
  if (kill) {
    const id = kill.dataset.kill;
    try {
      await api(`/api/v1/sessions/${id}`, { method: "DELETE" });
      toast("Session killed");
      // optimistic: the PTY takes a beat to exit, so show it right away
      killing.add(id);
      const s = state.sessions.find((x) => x.id === id);
      if (s) {
        s.state = "exited";
        s.pairingUrl = null;
      }
      renderSessions();
      reconcile(id); // then confirm against the server
    } catch (err) {
      toast(err.message, true);
    }
  }
  if (remove) {
    try {
      await api(`/api/v1/sessions/${remove.dataset.remove}/record`, { method: "DELETE" });
      state.lost = state.lost.filter((l) => l.id !== remove.dataset.remove);
      refreshSessions();
    } catch (err) {
      toast(err.message, true);
    }
  }
});

/* ---------- in-orbit chip (leftover gc/* branches) ---------- */
// fetched on sessions-tab activation only, never on the poll; a fetch error
// hides the chip entirely — same face as zero branches
let orbitOpen = false;

async function loadOrbit() {
  try {
    const { orbit } = await api("/api/v1/orbit");
    state.orbit = orbit || [];
  } catch {
    state.orbit = [];
  }
  renderOrbit();
}

function renderOrbit() {
  const wrap = $("orbit");
  const list = $("orbitList");
  const n = state.orbit?.length || 0;
  wrap.hidden = n === 0;
  if (!n) {
    orbitOpen = false;
    list.hidden = true;
    return;
  }
  $("orbitChip").textContent = `${n} in orbit`;
  $("orbitChip").setAttribute("aria-expanded", String(orbitOpen));
  list.hidden = !orbitOpen;
  list.innerHTML = "";
  for (const o of state.orbit) {
    const row = document.createElement("div");
    row.className = "orbit-row";
    const age = o.lastCommitAt ? `${fmtDur(Date.now() - Date.parse(o.lastCommitAt))} ago` : "";
    row.innerHTML = `
      <div class="orbit-info">
        <span class="orbit-branch">⎇ ${esc(o.branch)}</span>
        <span class="orbit-meta">${esc(o.repo.split("/").pop())}
          · <span class="orbit-badge ${o.merged ? "merged" : "unmerged"}">${o.merged ? "merged" : "unmerged"}</span>
          ${o.heldBy ? `· <span class="orbit-badge held">held by worktree</span>` : ""}
          ${age ? `· ${age}` : ""}</span>
      </div>
      ${o.heldBy ? "" : `<button class="wt-clean-btn orbit-del">Sweep</button>`}`;
    const btn = row.querySelector(".orbit-del");
    if (btn) {
      btn.onclick = () => {
        const del = async () => {
          try {
            await api(`/api/v1/orbit?repo=${encodeURIComponent(o.repo)}&branch=${encodeURIComponent(o.branch)}`, { method: "DELETE" });
            toast("Branch swept");
            // optimistic: drop the row, decrement the count, hide at zero
            state.orbit = state.orbit.filter((x) => x !== o);
            renderOrbit();
            patchSweptDebrief(o.branch);
            loadOrbit();
            refreshSessions();
          } catch (err) {
            toast(err.message, true); // 409/400 — the row stays
          }
        };
        if (o.merged) del();
        else armConfirm(btn, "Delete unmerged?", del);
      };
    }
    list.appendChild(row);
  }
}

$("orbitChip").onclick = () => {
  orbitOpen = !orbitOpen;
  renderOrbit();
};

/* ---------- tabs, health, boot ---------- */
function switchTab(tab) {
  state.tab = tab;
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === tab));
  $("view-browse").hidden = tab !== "browse";
  $("view-sessions").hidden = tab !== "sessions";
  $("view-settings").hidden = tab !== "settings";
  syncFab();
  if (tab === "sessions") {
    refreshSessions();
    loadOrbit();
    tailConvos(); // warm the verbs/snippets right away, not a poll-cycle later
  }
  if (tab === "settings") openSettings();
}

async function health() {
  const el = $("health");
  const pop = $("statusPop");
  try {
    const h = await api("/healthz");
    el.classList.remove("err");
    el.classList.add("ok");
    pop.classList.remove("err");
    pop.classList.add("ok");
    $("readoutState").textContent = "live";
    $("readoutSessions").textContent = String(h.sessions ?? 0);
    $("readoutVersion").textContent = h.version ? `v${h.version}` : "—";
    lastSyncAt = Date.now();
    // stale-client self-heal: a warm PWA can run old JS for days; reload once per server version
    if (h.version && h.version !== CLIENT_VERSION && sessionStorage.getItem("reloaded-for") !== h.version) {
      sessionStorage.setItem("reloaded-for", h.version);
      location.reload();
    }
  } catch {
    el.classList.remove("ok");
    el.classList.add("err");
    pop.classList.remove("ok");
    pop.classList.add("err");
    $("readoutState").textContent = "offline";
  }
}

// 1s tick: relative ages on cards + staleness readout. Text-node mutations only.
function secondTick() {
  const age = Math.round((Date.now() - lastSyncAt) / 1000);
  const stale = age > 10;
  // no text in the topbar anymore — staleness shows as an amber dot, details in the popup
  $("health").classList.toggle("stale", stale);
  $("syncRow").hidden = !stale;
  if (stale) $("readoutSync").textContent = `${age}s ago`;
  if (state.tab !== "sessions" || document.hidden) return;
  const list = $("sessionList");
  for (const s of state.sessions) {
    const card = list.querySelector(`[data-id="${s.id}"]`);
    if (card) updateDynamic(card, s);
  }
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState !== "visible") return;
  // the return-to-app moment is the stalest frame — sync immediately
  health();
  refreshSessions();
  if (state.tab === "browse") loadFolder(state.path).catch(() => {});
});

/* ---------- settings ---------- */
const setVals = {};
function wireSetSegment(id, key) {
  document.querySelectorAll(`#${id} button`).forEach((b) => {
    b.onclick = () => {
      setVals[key] = b.dataset.value;
      syncSegment(id, b.dataset.value);
    };
  });
}
for (const pair of ["setTheme:theme", "setHand:hand", "setSpawn:spawnMode", "setPerm:permissionMode", "setHookEvents:hookEvents", "setHidden:hidden"]) {
  const [id, key] = pair.split(":");
  wireSetSegment(id, key);
}

// the settings sheet edits the first webhook; extra subscribers set up via
// config.json or the API are preserved untouched on save
const HOOK_PRESETS = {
  failures: ["session.failed", "job.failed"],
  ready: ["session.ready", "session.failed", "job.failed"],
  all: ["*"],
};
let webhooksDraft = [];
function hookPresetFor(events) {
  if (!events || !events.length) return "all";
  const key = [...events].sort().join(",");
  for (const [name, set] of Object.entries(HOOK_PRESETS)) {
    if ([...set].sort().join(",") === key) return name;
  }
  return "custom"; // hand-edited filter — shown unhighlighted, preserved on save
}

let rootsDraft = [];

function renderRootChips() {
  const row = $("rootsChips");
  row.innerHTML = "";
  for (const r of rootsDraft) {
    const chip = document.createElement("span");
    chip.className = "root-chip";
    chip.append(r);
    const x = document.createElement("button");
    x.textContent = "×";
    x.setAttribute("aria-label", `remove ${r}`);
    x.onclick = () => {
      rootsDraft = rootsDraft.filter((p) => p !== r);
      renderRootChips();
    };
    chip.appendChild(x);
    row.appendChild(chip);
  }
}

async function renderWorktrees() {
  const box = $("worktreeList");
  try {
    const { worktrees } = await api("/api/v1/worktrees");
    box.innerHTML = "";
    if (!worktrees.length) {
      box.innerHTML = `<div class="wt-empty">none kept — all clean</div>`;
      return;
    }
    for (const wt of worktrees) {
      const row = document.createElement("div");
      row.className = "wt-row";
      row.innerHTML = `
        <div class="wt-info">
          <span class="wt-repo">${esc(wt.repo)}</span>
          <span class="wt-meta">${wt.branch ? `⎇ ${esc(wt.branch)} · ` : ""}${wt.dirty ? `<span class="wt-dirty">dirty</span> · ` : ""}${esc(wt.id)}</span>
        </div>
        <button class="wt-clean-btn">Clean</button>`;
      row.querySelector(".wt-clean-btn").onclick = (e) => {
        const clean = async () => {
          try {
            await api(`/api/v1/worktrees?path=${encodeURIComponent(wt.path)}`, { method: "DELETE" });
            toast("Worktree cleaned");
            renderWorktrees();
          } catch (err) {
            toast(err.message, true);
          }
        };
        if (wt.dirty) armConfirm(e.target, "Discard changes?", clean);
        else clean();
      };
      box.appendChild(row);
    }
  } catch {
    box.innerHTML = `<div class="wt-empty">could not load worktrees</div>`;
  }
}

async function openSettings() {
  setVals.theme = prefs.theme || "light";
  setVals.hand = prefs.hand || "right";
  setVals.spawnMode = prefs.spawnMode || "same-dir";
  setVals.permissionMode = prefs.permissionMode || "default";
  syncSegment("setTheme", setVals.theme);
  syncSegment("setHand", setVals.hand);
  syncSegment("setSpawn", setVals.spawnMode);
  syncSegment("setPerm", setVals.permissionMode);
  renderWorktrees();
  try {
    const cfg = await api("/api/v1/config");
    webhooksDraft = Array.isArray(cfg.webhooks) ? [...cfg.webhooks] : [];
    setVals.hookEvents = hookPresetFor(webhooksDraft[0]?.events);
    setVals.hidden = cfg.showHidden ? "on" : "off";
    $("setHookUrl").value = webhooksDraft[0]?.url || "";
    rootsDraft = [...cfg.roots];
    renderRootChips();
    syncSegment("setHookEvents", setVals.hookEvents);
    syncSegment("setHidden", setVals.hidden);
  } catch {
    toast("could not load server config", true);
  }
}

async function saveSettings() {
  prefs.theme = setVals.theme;
  prefs.hand = setVals.hand;
  prefs.spawnMode = setVals.spawnMode;
  prefs.permissionMode = setVals.permissionMode;
  localStorage.setItem("prefs", JSON.stringify(prefs));
  applyTheme();
  applyHand();
  try {
    await api("/api/v1/config", {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        showHidden: setVals.hidden === "on",
        webhooks: (() => {
          const url = $("setHookUrl").value.trim();
          const rest = webhooksDraft.slice(1);
          if (!url) return rest;
          const events = setVals.hookEvents === "custom" ? webhooksDraft[0]?.events : HOOK_PRESETS[setVals.hookEvents || "failures"];
          return [{ url, ...(events && !events.includes("*") ? { events } : {}) }, ...rest];
        })(),
        roots: rootsDraft,
      }),
    });
    toast("Settings saved");
    loadFolder(state.path).catch(() => loadFolder(null).catch(() => {}));
  } catch (e) {
    toast(e.message, true);
  }
}

/* status popup — tap the dot for details, tap anywhere else to dismiss */
$("health").onclick = (e) => {
  e.stopPropagation();
  $("statusPop").hidden = !$("statusPop").hidden;
};
document.addEventListener("click", (e) => {
  const pop = $("statusPop");
  if (!pop.hidden && !pop.contains(e.target)) pop.hidden = true;
});
$("settingsSave").onclick = saveSettings;
$("rootAddBtn").onclick = (e) => {
  e.preventDefault();
  const v = $("rootAdd").value.trim();
  if (!v.startsWith("/")) return toast("absolute path required", true);
  if (!rootsDraft.includes(v)) rootsDraft.push(v);
  $("rootAdd").value = "";
  renderRootChips();
};

/* ---------- auth gate ---------- */
$("authSubmit").onclick = async () => {
  const v = cleanToken($("authInput").value);
  if (!v) return;
  const err = $("authError");
  err.hidden = true;
  // a wrong token used to save + reload straight back to this gate, which
  // reads as "nothing happened" — probe the API first and say so instead
  try {
    const res = await fetch("/api/v1/roots", { headers: { authorization: `Bearer ${v}` } });
    if (res.status === 401) {
      err.textContent = "token rejected — check authToken in the config.json the server was started from";
      err.hidden = false;
      return;
    }
  } catch {
    /* server unreachable — save anyway; the reload will surface it */
  }
  localStorage.setItem("token", v);
  location.reload();
};
$("authInput").addEventListener("keydown", (e) => {
  if (e.key === "Enter") $("authSubmit").click();
});

document.querySelectorAll(".tab[data-tab]").forEach((t) => (t.onclick = () => switchTab(t.dataset.tab)));
// at a configured root the API reports no parent — up goes to the roots list
$("upBtn").onclick = () => loadFolder(state.current?.parent ?? null).catch((e) => toast(e.message, true));
$("launchBar").onclick = () => openSheet();
$("scrim").onclick = closeSheet;
$("launchBtn").onclick = launch;
wireSegment("optSpawn", "spawnMode", syncBranchField);
wireSegment("optPerm", "permissionMode");
$("optBranch").onchange = () => (state.opts.branch = $("optBranch").value);

/* ---------- URL-parameter launch (manifest shortcuts, scripts) ---------- */
// Grammar: ?path=/abs/dir[&name=...] | ?mission=1 | ?relaunch=last
// Params only prefill UI — launching always takes a human tap.
const PARAM_TEXT_MAX = 4096;

// Land on Browse at roots with the mission input focused. The input ships in a
// sibling unit, so it may not exist yet — the focus simply no-ops.
async function missionEntry() {
  switchTab("browse");
  if (state.path !== null) await loadFolder(null);
  document.getElementById("missionInput")?.focus();
}

async function consumeLaunchParams() {
  const qs = new URLSearchParams(location.search);
  const trunc = (v) => (v ? String(v).slice(0, PARAM_TEXT_MAX) : undefined);
  const path = qs.get("path");
  const name = trunc(qs.get("name"));
  const mission = qs.get("mission") === "1";
  const relaunch = qs.get("relaunch") === "last";
  if (!path && !name && !mission && !relaunch) return; // nothing we understand — leave the URL alone
  try {
    history.replaceState(null, "", "/"); // consumed — reload/relaunch never replays
  } catch {
    /* cosmetic — some webviews refuse; params simply stay visible */
  }

  if (path) {
    // client-side root check is UX only — the server's WithinRoots is the real gate
    const { roots } = await api("/api/v1/roots");
    const inRoots = typeof path === "string" && path.startsWith("/") && roots.some((r) => path === r.path || path.startsWith(r.path + "/"));
    if (inRoots) {
      await loadFolder(path);
      const saved = JSON.parse(localStorage.getItem(`opts:${path}`) || "null");
      openSheet({ ...(saved || launchDefaults()), name });
      return;
    }
    toast("path is outside the configured roots", true);
    return;
  }
  if (relaunch) {
    const { recent } = await api("/api/v1/journal/recent?limit=1");
    const last = recent?.[0];
    if (!last) {
      toast("no previous launch");
      await missionEntry();
      return;
    }
    await relaunchFromRecent(last); // handles the stale-branch degrade + toast itself
    return;
  }
  if (mission) await missionEntry();
}

$("readoutHost").textContent = location.hostname.split(".")[0]; // first label only — full FQDN swamps a phone header
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(() => {});
loadFolder(null)
  .then(() => {
    // first authorized load succeeded — safe to consume launch params. A 401 boot
    // shows the auth gate instead, and unlocking reloads with the query intact.
    if ($("authGate").hidden) consumeLaunchParams().catch((e) => toast(e.message, true));
  })
  .catch((e) => toast(e.message, true));
health();
setInterval(health, 10000);
setInterval(() => {
  if (state.tab === "sessions" || state.sessions.some((s) => s.state === "starting")) refreshSessions();
  tailLogs();
  tailConvos();
}, 2500);
setInterval(secondTick, 1000);
refreshSessions();
