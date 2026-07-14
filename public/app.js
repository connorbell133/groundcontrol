/* agent-runner mobile UI */
const CLIENT_VERSION = "0.2.0"; // keep in step with package.json — healthz mismatch triggers a reload
const $ = (id) => document.getElementById(id);
const authToken = () => localStorage.getItem("token") || "";
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
  if (!res.ok) throw new Error(body.error || res.statusText);
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
  const data = path ? await api(`/api/browse?path=${encodeURIComponent(path)}`) : null;
  if (!data) {
    const { roots } = await api("/api/roots");
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
  return parts.slice(-4); // keep the tail readable on phones
}

function renderBrowse() {
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
  const { recent } = await api("/api/journal/recent?limit=6");
  state.recent = recent;
  state.recentLoaded = true;
  const row = $("recentsRow");
  row.innerHTML = "";
  for (const r of recent) {
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

/* ---------- launch sheet ---------- */
function syncBranchField() {
  $("branchField").hidden = state.opts.spawnMode !== "worktree" || !state.current.isGit;
}

async function loadBranches(folder) {
  const select = $("optBranch");
  select.innerHTML = "";
  try {
    const { branches } = await api(`/api/branches?path=${encodeURIComponent(folder)}`);
    if (branches.length === 0) {
      // repo with no commits yet: worktrees are impossible, so remove the option
      if (state.opts.spawnMode === "worktree") state.opts.spawnMode = "same-dir";
      syncSegment("optSpawn", state.opts.spawnMode);
      $("spawnField").hidden = true;
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
    const repoNote = state.current.repoRoot && state.current.repoRoot !== state.current.path
      ? `worktree of ${state.current.repoName}` : "worktree is created from this branch";
    $("branchHint").textContent = `${repoNote} · ${branches.length} branches`;
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
  state.opts = prefill || saved || launchDefaults();

  // capability-conditional: a non-git folder gets no workspace/branch machinery at all,
  // not a disabled control — choices that can't be made shouldn't be on screen
  const git = state.current.isGit;
  $("spawnField").hidden = !git;
  if (!git && state.opts.spawnMode === "worktree") state.opts.spawnMode = "same-dir";
  syncSegment("optSpawn", state.opts.spawnMode);
  syncSegment("optPerm", state.opts.permissionMode);
  syncBranchField();
  if (git) loadBranches(folder);

  $("optName").value = "";
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

async function launch() {
  const btn = $("launchBtn");
  // YOLO in a non-git folder has no VCS undo — require a deliberate second tap
  if (state.opts.permissionMode === "bypassPermissions" && !state.current.isGit && btn.dataset.confirm !== "1") {
    btn.dataset.confirm = "1";
    btn.classList.add("warn");
    btn.innerHTML = `⚠ no git undo here — tap again`;
    setTimeout(() => {
      if (btn.dataset.confirm === "1") {
        btn.dataset.confirm = "";
        btn.classList.remove("warn");
        btn.innerHTML = `<span class="cta-glyph">▶</span> Launch session`;
      }
    }, 4000);
    return;
  }
  btn.dataset.confirm = "";
  btn.classList.remove("warn");
  btn.disabled = true;
  btn.innerHTML = `<span class="cta-glyph">◴</span> Launching…`;
  try {
    const branch = state.opts.spawnMode === "worktree" ? $("optBranch").value || undefined : undefined;
    state.opts.branch = branch;
    localStorage.setItem(`opts:${state.path}`, JSON.stringify(state.opts));
    await api("/api/sessions", {
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
    const { sessions, lost } = await api("/api/sessions");
    state.sessions = sessions.map((s) =>
      killing.has(s.id) && s.state !== "exited" && s.state !== "error"
        ? { ...s, state: "exited", pairingUrl: null }
        : s
    );
    state.lost = lost || [];
    lastSyncAt = Date.now();
    renderSessions();
  } catch {
    /* runner unreachable; health dot covers it */
  }
}

// CONTRACT: card.innerHTML is rewritten ONLY here, and only when state/pairingUrl
// change. Everything time-varying updates via data-* lookups in updateDynamic().
function renderShell(card, s) {
  const logWasOpen = openLogs.has(s.id);
  const shareBtn = navigator.share ? `<button class="log-tool" data-share-log="${s.id}">share</button>` : "";
  card.innerHTML = `
    <div class="session-head">
      <span class="session-name">${esc(s.name)}</span>
      <span class="state-pill ${s.state}">${s.state}</span>
    </div>
    <div class="session-path">${esc(s.folder)}</div>
    <div class="session-ticker" data-ticker></div>
    <div class="session-meta">
      <span class="meta-chip">${s.spawnMode}</span>
      ${s.branch ? `<span class="meta-chip branch">⎇ ${esc(s.branch)}</span>` : ""}
      <span class="meta-chip">${s.permissionMode}</span>
      <span class="meta-chip age" data-age></span>
      <span class="meta-chip activity" data-activity hidden></span>
    </div>
    ${s.pairingUrl && s.state === "ready" ? `
      <div class="qr-wrap">
        <img class="qr-img" alt="Pairing QR code" src="/api/sessions/${s.id}/qr${tokenQS()}" />
        <span class="qr-hint">Claude app · camera</span>
        <span class="bracket bl"></span><span class="bracket br"></span>
      </div>
      <div class="session-actions">
        <a class="btn primary" href="${esc(s.pairingUrl)}" target="_blank" rel="noopener">Open link</a>
        <button class="btn danger" data-kill="${s.id}">Kill</button>
      </div>` : s.state === "starting" ? `
      <div class="provision"><span class="provision-dot"></span><span data-provision>Provisioning…</span></div>
      <div class="session-actions">
        <button class="btn danger" data-kill="${s.id}">Kill</button>
      </div>` : `
      <div class="session-actions">
        <button class="btn" data-relaunch="${s.id}">Relaunch</button>
        <button class="btn" data-remove="${s.id}">Dismiss</button>
      </div>`}
    <details class="session-log"><summary>runtime log
      <span class="log-tools"><button class="log-tool" data-copy-log="${s.id}">copy</button>${shareBtn}</span>
    </summary><pre data-log="${s.id}">…</pre></details>`;
  if (logWasOpen) {
    const details = card.querySelector(".session-log");
    if (details) details.open = true; // fires the toggle listener, which refills the log
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

function updateDynamic(card, s) {
  const now = Date.now();
  const started = Date.parse(s.startedAt);
  const ticker = card.querySelector("[data-ticker]");
  if (ticker && ticker.textContent !== (s.lastLine || "")) ticker.textContent = s.lastLine || "";

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

  const provision = card.querySelector("[data-provision]");
  if (provision) provision.textContent = `Provisioning… ${fmtDur(now - started)}`;
}

function renderSessions() {
  const live = state.sessions.filter((s) => s.state === "starting" || s.state === "ready");
  const badge = $("sessionCount");
  badge.hidden = live.length === 0;
  badge.textContent = live.length;

  $("sessionsEmpty").style.display = state.sessions.length || state.lost.length ? "none" : "";
  const list = $("sessionList");

  for (const s of state.sessions) {
    let card = list.querySelector(`[data-id="${s.id}"]`);
    if (!card) {
      card = document.createElement("li");
      card.className = "session-card";
      card.dataset.id = s.id;
      list.prepend(card);
    }
    const stateChanged = card.dataset.state !== s.state || card.dataset.url !== String(s.pairingUrl);
    if (stateChanged) {
      card.dataset.state = s.state;
      card.dataset.url = String(s.pairingUrl);
      renderShell(card, s);
    }
    updateDynamic(card, s);
  }

  for (const l of state.lost) {
    let card = list.querySelector(`[data-id="${l.id}"]`);
    if (!card) {
      card = document.createElement("li");
      card.className = "session-card lost";
      card.dataset.id = l.id;
      card.dataset.state = "lost";
      list.appendChild(card);
      renderLostShell(card, l);
    }
  }

  // drop cards for sessions that vanished
  list.querySelectorAll("[data-id]").forEach((card) => {
    const id = card.dataset.id;
    if (!state.sessions.some((s) => s.id === id) && !state.lost.some((l) => l.id === id)) {
      openLogs.delete(id);
      card.remove();
    }
  });
}

/* ---------- log tail ---------- */
function fillLog(pre, text) {
  if (pre.textContent === text) return;
  const wasPlaceholder = pre.textContent === "…";
  const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
  pre.textContent = text;
  if (atBottom || wasPlaceholder) pre.scrollTop = pre.scrollHeight;
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
      const text = await api(`/api/sessions/${id}/log`);
      const pre = document.querySelector(`pre[data-log="${id}"]`); // fresh lookup — survives rewrites
      if (pre && openLogs.has(id)) fillLog(pre, text);
    } catch {
      /* transient failure: keep the last good log */
    }
  }
}

document.addEventListener("toggle", async (e) => {
  const pre = e.target.querySelector?.("pre[data-log]");
  if (!pre) return;
  const id = pre.dataset.log;
  if (e.target.open) {
    openLogs.add(id);
    const text = await api(`/api/sessions/${id}/log`).catch(() => "log unavailable");
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
    const cfg = state.sessions.find((x) => x.id === id) || state.lost.find((l) => l.id === id);
    if (!cfg) return;
    try {
      await api("/api/sessions", {
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
      await api(`/api/sessions/${id}`, { method: "DELETE" });
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
      await api(`/api/sessions/${remove.dataset.remove}/record`, { method: "DELETE" });
      state.lost = state.lost.filter((l) => l.id !== remove.dataset.remove);
      refreshSessions();
    } catch (err) {
      toast(err.message, true);
    }
  }
});

/* ---------- tabs, health, boot ---------- */
function switchTab(tab) {
  state.tab = tab;
  document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === tab));
  $("view-browse").hidden = tab !== "browse";
  $("view-sessions").hidden = tab !== "sessions";
  syncFab();
  if (tab === "sessions") refreshSessions();
}

async function health() {
  const el = $("health");
  try {
    const h = await api("/healthz");
    el.classList.remove("err");
    el.classList.add("ok");
    $("readoutState").textContent = "live";
    lastSyncAt = Date.now();
    // stale-client self-heal: a warm PWA can run old JS for days; reload once per server version
    if (h.version && h.version !== CLIENT_VERSION && sessionStorage.getItem("reloaded-for") !== h.version) {
      sessionStorage.setItem("reloaded-for", h.version);
      location.reload();
    }
  } catch {
    el.classList.remove("ok");
    el.classList.add("err");
    $("readoutState").textContent = "offline";
  }
}

// 1s tick: relative ages on cards + staleness readout. Text-node mutations only.
function secondTick() {
  const sync = $("readoutSync");
  const age = Math.round((Date.now() - lastSyncAt) / 1000);
  if (age > 10) {
    sync.hidden = false;
    sync.textContent = `sync ${age}s`;
  } else {
    sync.hidden = true;
  }
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
for (const pair of ["setTheme:theme", "setSpawn:spawnMode", "setPerm:permissionMode", "setNtfyReady:ntfyReady", "setNtfyExit:ntfyExit", "setHidden:hidden"]) {
  const [id, key] = pair.split(":");
  wireSetSegment(id, key);
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
    const { worktrees } = await api("/api/worktrees");
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
      row.querySelector(".wt-clean-btn").onclick = async (e) => {
        const b = e.target;
        if (b.dataset.confirm !== "1" && wt.dirty) {
          b.dataset.confirm = "1";
          b.textContent = "Discard changes?";
          setTimeout(() => { b.dataset.confirm = ""; b.textContent = "Clean"; }, 4000);
          return;
        }
        try {
          await api(`/api/worktrees?path=${encodeURIComponent(wt.path)}`, { method: "DELETE" });
          toast("Worktree cleaned");
          renderWorktrees();
        } catch (err) {
          toast(err.message, true);
        }
      };
      box.appendChild(row);
    }
  } catch {
    box.innerHTML = `<div class="wt-empty">could not load worktrees</div>`;
  }
}

async function openSettings() {
  setVals.theme = prefs.theme || "light";
  setVals.spawnMode = prefs.spawnMode || "same-dir";
  setVals.permissionMode = prefs.permissionMode || "default";
  syncSegment("setTheme", setVals.theme);
  syncSegment("setSpawn", setVals.spawnMode);
  syncSegment("setPerm", setVals.permissionMode);
  renderWorktrees();
  try {
    const cfg = await api("/api/config");
    setVals.ntfyReady = cfg.ntfy.notifyReady ? "on" : "off";
    setVals.ntfyExit = cfg.ntfy.notifyExit;
    setVals.hidden = cfg.showHidden ? "on" : "off";
    $("setNtfyTopic").value = cfg.ntfy.topic;
    $("setNtfyUrl").value = cfg.ntfy.url;
    rootsDraft = [...cfg.roots];
    renderRootChips();
    syncSegment("setNtfyReady", setVals.ntfyReady);
    syncSegment("setNtfyExit", setVals.ntfyExit);
    syncSegment("setHidden", setVals.hidden);
  } catch {
    toast("could not load server config", true);
  }
  $("settingsSheet").hidden = false;
  $("scrim").hidden = false;
  requestAnimationFrame(() => {
    $("settingsSheet").classList.add("show");
    $("scrim").classList.add("show");
  });
}

function closeSettings() {
  $("settingsSheet").classList.remove("show");
  if (!$("sheet").classList.contains("show")) $("scrim").classList.remove("show");
  setTimeout(() => {
    $("settingsSheet").hidden = true;
    if (!$("sheet").classList.contains("show")) $("scrim").hidden = true;
  }, 340);
}

async function saveSettings() {
  prefs.theme = setVals.theme;
  prefs.spawnMode = setVals.spawnMode;
  prefs.permissionMode = setVals.permissionMode;
  localStorage.setItem("prefs", JSON.stringify(prefs));
  applyTheme();
  try {
    await api("/api/config", {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        showHidden: setVals.hidden === "on",
        ntfy: {
          url: $("setNtfyUrl").value.trim() || "https://ntfy.sh",
          topic: $("setNtfyTopic").value.trim(),
          notifyReady: setVals.ntfyReady === "on",
          notifyExit: setVals.ntfyExit || "errors",
        },
        roots: rootsDraft,
      }),
    });
    toast("Settings saved");
    closeSettings();
    loadFolder(state.path).catch(() => loadFolder(null).catch(() => {}));
  } catch (e) {
    toast(e.message, true);
  }
}

$("gearBtn").onclick = openSettings;
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
$("authSubmit").onclick = () => {
  const v = $("authInput").value.trim();
  if (!v) return;
  localStorage.setItem("token", v);
  location.reload();
};
$("authInput").addEventListener("keydown", (e) => {
  if (e.key === "Enter") $("authSubmit").click();
});

document.querySelectorAll(".tab").forEach((t) => (t.onclick = () => switchTab(t.dataset.tab)));
$("launchBar").onclick = () => openSheet();
$("scrim").onclick = () => {
  closeSheet();
  closeSettings();
};
$("launchBtn").onclick = launch;
wireSegment("optSpawn", "spawnMode", syncBranchField);
wireSegment("optPerm", "permissionMode");
$("optBranch").onchange = () => (state.opts.branch = $("optBranch").value);

$("readoutHost").textContent = location.hostname.split(".")[0]; // first label only — full FQDN swamps a phone header
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(() => {});
loadFolder(null).catch((e) => toast(e.message, true));
health();
setInterval(health, 10000);
setInterval(() => {
  if (state.tab === "sessions" || state.sessions.some((s) => s.state === "starting")) refreshSessions();
  tailLogs();
}, 2500);
setInterval(secondTick, 1000);
refreshSessions();
