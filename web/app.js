/* Comebye: a phone app for herdr. Vanilla JS, no build step. */
"use strict";

const $ = (id) => document.getElementById(id);
const state = {
  agents: [],          // current agent list from the server (every host's, on a hub)
  hostFilter: null,    // "all" or a host name (loaded lazily); only used with more than one host
  hosts: [],           // per-host status from the server; one entry unless this is a hub
  localHost: null,     // name of the server's own host
  selected: null,      // id ("<host>/<pane>") of the agent being viewed
  es: null,            // EventSource
  pollTimer: null,     // transcript/output auto-poll while detail view is open
  pollUntil: 0,        // poll regardless of agent status until this time (ms): set by a send
  sending: 0, // prompts on the wire
  chat: null,          // chat view state: { rendered: Set<id>, skip, more, terminal: bool }
  ask: null,           // pending ask_user_question from the transcript, if any
  lastStatus: null,    // status of the viewed agent at the previous SSE event
  lastEventAt: 0,      // Date.now() of the last SSE open/event (incl. server ping heartbeats)
  retryTimer: null,    // pending SSE reconnect
  retryDelay: 1000,    // reconnect backoff, doubles up to 30s, reset on success
  watchdog: null,      // stale-connection / liveness timer
  unauthorized: false, // last API call got 401 (Tailscale login not allowlisted)
  pendingRoute: null,  // {host, pane} from #/agent/... waiting for the agent list to arrive
  pushedRoute: false,  // openDetail pushed a history entry (back pops it)
  opened: false,       // the desktop's cold-start choice (openToSomething) has been made
  view: null,          // desktop only: the page filling the main pane ("status", "briefing", "todo"), or null for an agent / nothing
};

/* ---------- helpers ---------- */

const UNAUTHORIZED_MSG = "Not authorized: open this from a device logged into an allowed Tailscale account";

// The server authenticates by Tailscale identity (set by Tailscale Serve), so
// there is nothing to log in to: a 401 just means this device's account is
// not on the allowlist. Show that in the list's empty state.
function setUnauthorized(on) {
  if (state.unauthorized === on) return;
  state.unauthorized = on;
  renderList();
}

// A request that never answers (the phone changed networks or woke from
// sleep; the connection is half-dead) must fail rather than hang: a hung
// refreshOutput() pins the terminal view and a hung prompt POST leaves
// state.sending set, so the send button stays dead until the app is killed.
const API_TIMEOUT_MS = 30000;
const POLL_AFTER_SEND_MS = 120000; // keep polling this long after a send, whatever the status says

// anySignal aborts when any of the signals does (AbortSignal.any needs iOS 17.4).
function anySignal(signals) {
  if (AbortSignal.any) return AbortSignal.any(signals);
  const c = new AbortController();
  for (const s of signals) {
    if (s.aborted) { c.abort(s.reason); break; }
    s.addEventListener("abort", () => c.abort(s.reason), { once: true });
  }
  return c.signal;
}

async function api(path, opts = {}) {
  const { timeout = API_TIMEOUT_MS, ...init } = opts;
  let res;
  try {
    res = await fetch(path, {
      ...init,
      signal: init.signal ? anySignal([init.signal, AbortSignal.timeout(timeout)]) : AbortSignal.timeout(timeout),
      headers: { "Content-Type": "application/json", ...(init.headers || {}) },
    });
  } catch (e) {
    if (e && e.name === "TimeoutError") throw new Error("timed out — check the connection");
    if (e && e.name === "AbortError") { e.aborted = true; }
    throw e;
  }
  noteVersion(res.headers.get("X-App-Version"));
  if (res.status === 401) {
    setUnauthorized(true);
    const err = new Error(UNAUTHORIZED_MSG);
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    let detail = res.statusText, code = "";
    try {
      const body = await res.json();
      detail = body.message || body.code || detail;
      code = body.code || "";
    } catch {}
    const err = new Error(detail);
    err.status = res.status;
    err.code = code;
    throw err;
  }
  return res.json();
}

let toastTimer = null;
function toast(msg, isError = false) {
  const el = $("toast");
  el.textContent = msg;
  el.className = "show" + (isError ? " error" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (el.className = ""), 2600);
}

// toastAction: a toast with one button (Undo), on screen for ms.
function toastAction(msg, label, onClick, ms) {
  const el = $("toast");
  el.textContent = "";
  const b = document.createElement("button");
  b.type = "button"; b.className = "toast-act"; b.textContent = label;
  b.onclick = () => { el.className = ""; onClick(); };
  el.append(msg + " · ", b);
  el.className = "show act";
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (el.className = ""), ms);
}

// ---- optimistic actions ----
//
// A tap must look done within a frame, whatever the network does: the app
// changes its own state first, sends the request behind it, and rolls back
// with an error if the request fails. Refreshes (polls, SSE) re-fetch the
// server's view, which has not heard of the tap yet, so the pending change
// is an Overlay: it is re-applied to every refresh until the server shows it
// (or SETTLE_MS after the request was confirmed), so nothing flickers back.
const SETTLE_MS = 6000;

class Overlay {
  constructor() { this.m = new Map(); }
  // busy: the request is still on the wire (a double submit is refused);
  // has also covers the settle window, where only refreshes need covering.
  busy(key) { const e = this.m.get(key); return !!e && e.until === Infinity; }
  busyKeys() { return [...this.m].filter(([, e]) => e.until === Infinity).map(([k]) => k); }
  has(key) { return this.m.has(key); }
  // add registers a pending change; reapply(fn) runs fn(entry, key) for each.
  add(key, entry) { this.m.set(key, { ...entry, until: Infinity }); }
  // settle: the request was confirmed; keep covering refreshes that were
  // already on the wire for a while longer.
  settle(key) { const e = this.m.get(key); if (e) e.until = Date.now() + SETTLE_MS; }
  drop(key) { this.m.delete(key); }
  each(fn) {
    const now = Date.now();
    for (const [k, e] of this.m) {
      if (e.until < now) this.m.delete(k);
      else fn(e, k);
    }
  }
}

// flash gives a pressed button an instant, quiet "taken" cue (it needs no
// network): used where the change the tap causes is not drawn at once.
function flash(el) {
  if (!el) return;
  el.classList.remove("flash");
  void el.offsetWidth; // restart the animation
  el.classList.add("flash");
  setTimeout(() => el.classList.remove("flash"), 500);
}

// esc escapes text for HTML, quotes included, so it is also safe inside a
// quoted attribute (agent data from a hub peer is untrusted).
function esc(s) {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

function baseName(p) {
  if (!p) return "";
  return p.split("/").filter(Boolean).pop() || p;
}

// repoLabel returns the agent's repository context: a worktree chip
// (repo + branch) when the workspace is a linked checkout, else the repo
// name or cwd basename.
function repoLabel(a) {
  if (a.worktree && a.worktree.repo) {
    if (a.worktree.linked && a.worktree.branch) {
      return { repo: a.worktree.repo, branch: a.worktree.branch };
    }
    return { repo: a.worktree.repo };
  }
  // No checkout info from herdr: a handed-off worker's ledger entry, or the
  // ~/.worktrees/<repo>/<branch> convention.
  const e = ledgerEntry(a);
  if (e && e.repo_name) return { repo: e.repo_name, branch: e.branch };
  const m = WT_PATH_RE.exec(a.cwd || "");
  if (m) return { repo: m[1], branch: m[2] };
  return { repo: baseName(a.cwd) };
}

function subLine(a) {
  const { repo, branch } = repoLabel(a);
  const kindEl = a.agent ? `<span class="kind">· ${esc(a.agent)}</span>` : "";
  const hostEl = multiHost() && a.host ? `<span class="host-chip">${esc(a.host)}</span>` : "";
  const repoEl = `<span class="repo">${esc(repo)}</span>`;
  const chip = branch ? `<span class="wt-chip">⑂ ${esc(branch)}</span>` : "";
  const d = repoDamageOf(a);
  const dmg = d ? `<span class="lg-chip failed rd-chip" title="${escAttr(`${d.repo}: ${d.reason}. ${d.hint}`)}">repo damaged</span>` : "";
  return `<div class="sub">${hostEl}${repoEl}${chip}${kindEl}${dmg}${ledgerChip(a)}</div>`;
}

/* ---------- damaged repositories ---------- */
//
// Each host reports the repositories whose object store git found damaged (a
// failed fetch, another git command, or the daily fsck), with the checkouts that
// share them and how to repair them by hand. Nothing repairs them by itself.

function repoDamageList() {
  const out = [];
  for (const h of state.hosts || []) for (const d of h.repo_damage || []) out.push({ host: h.name, local: !!h.local, d });
  return out;
}

// repoDamageOf is the damaged repository an agent works in, if any.
function repoDamageOf(a) {
  const h = (a.host && hostStatus(a.host)) || (state.hosts || []).find((x) => x.local);
  if (!h || !h.repo_damage || !h.repo_damage.length) return null;
  const dir = (a.worktree && a.worktree.checkout) || a.cwd || "";
  if (!dir) return null;
  for (const d of h.repo_damage) {
    for (const w of (d.worktrees && d.worktrees.length ? d.worktrees : [d.path])) {
      if (w && (dir === w || dir.startsWith(w.replace(/\/+$/, "") + "/"))) return d;
    }
  }
  return null;
}

/* ---------- hosts and agent identity ---------- */
//
// Pane IDs are only unique per host, so agents are keyed by their
// host-qualified `id` ("<host>/<pane>"). The server's own agents use the
// original /api/agents/<pane> routes; a hub's peer agents go through
// /api/hosts/<host>/agents/<pane>, which the hub proxies to that peer.

function agentId(a) { return a.id || a.pane_id; }

function isLocal(a) {
  return !a.host || !state.localHost || a.host === state.localHost;
}

function multiHost() { return state.hosts.length > 1; }

// agentPath is the API base for one agent.
function agentPath(a) {
  const pane = encodeURIComponent(a.pane_id);
  return isLocal(a) ? `/api/agents/${pane}` : `/api/hosts/${encodeURIComponent(a.host)}/agents/${pane}`;
}

// agentHash is the deep link: #/agent/<pane> for the server's own agents
// (as before), #/agent/<host>/<pane> for a peer's.
function agentHash(a) {
  const pane = encodeURIComponent(a.pane_id);
  return "#/agent/" + (isLocal(a) ? pane : encodeURIComponent(a.host) + "/" + pane);
}

// pathFor is agentPath for an agent id; it throws (inside the callers'
// try blocks) when the agent has gone away.
function pathFor(id) {
  const a = agentById(id);
  if (!a) {
    const err = new Error("agent is gone");
    err.status = 410;
    throw err;
  }
  return agentPath(a);
}

function hostOf(id) {
  const i = (id || "").indexOf("/");
  return i > 0 ? id.slice(0, i) : null;
}

function hostStatus(name) { return state.hosts.find((h) => h.name === name); }

// setAgents applies an /api/agents answer or an SSE agents event.
// Statuses are used as class names and compared everywhere: anything else
// (a misbehaving hub peer) is shown as "unknown".
const AGENT_STATUSES = new Set(["idle", "working", "blocked", "done", "unknown"]);

function setAgents(d) {
  state.agents = Array.isArray(d.agents) ? d.agents : [];
  for (const a of state.agents) {
    if (!AGENT_STATUSES.has(a.agent_status)) a.agent_status = "unknown";
  }
  if (Array.isArray(d.hosts)) {
    state.hosts = d.hosts;
    const local = d.hosts.find((h) => h.local);
    state.localHost = local ? local.name : null;
  }
}

function fmtTime(iso) {
  if (!iso) return "";
  try {
    return new Date(iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  } catch { return ""; }
}

function fmtMs(ms) {
  if (!ms) return "";
  if (ms < 1000) return ms + "ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
  return Math.round(ms / 60000) + "m";
}

/* ---------- markdown (vendored marked + DOMPurify) ---------- */
//
// Agent text is untrusted. marked (CommonMark + GFM) turns it into HTML and
// DOMPurify then cleans that HTML against a small allowlist, so whatever
// the parser lets through (raw HTML, odd URLs) is still only inert markup.
// Every caller gets a sanitized string; if either library failed to load,
// the text is shown escaped instead (never unsanitized HTML).

// Longer texts are shown as plain text: parsing is fast, but a megabyte of
// markup in one bubble is not worth the layout cost on a phone.
const MD_MAX_CHARS = 100000;

// Raw HTML an agent writes inline is kept only for these tags (DOMPurify
// still strips their attributes); any other tag, e.g. the `<String>` in
// "Vec<String>" or a <script>, is shown as literal text rather than eaten.
const MD_RAW_TAGS = new Set(("b i em strong u s del ins mark sub sup small kbd code pre br hr p " +
  "ul ol li blockquote details summary dl dt dd h1 h2 h3 h4 h5 h6 table thead tbody tr th td span div").split(" "));

const MD_PURIFY = {
  ALLOWED_TAGS: ["a", "b", "blockquote", "br", "code", "dd", "del", "details", "div", "dl", "dt", "em",
    "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "input", "ins", "kbd", "li", "mark", "ol", "p", "pre",
    "s", "small", "span", "strong", "sub", "summary", "sup", "table", "tbody", "td", "th", "thead", "tr", "u", "ul"],
  ALLOWED_ATTR: ["href", "title", "class", "align", "start", "type", "checked", "disabled"],
  // Only absolute web and mail links; anything else (javascript:, data:,
  // relative paths that would land on this app) loses its href.
  ALLOWED_URI_REGEXP: /^(?:https?:|mailto:)/i,
  // DOMPurify checks every attribute value not listed as URI-safe against
  // that regexp; these never hold URLs, so exempt them.
  ADD_URI_SAFE_ATTR: ["align", "start", "type", "checked", "disabled"],
  ALLOW_DATA_ATTR: false,
  ALLOW_ARIA_ATTR: false,
};

let mdLib = null; // { marked instance } once set up; false if unavailable

function mdSetup() {
  if (mdLib !== null) return mdLib;
  const M = window.marked, P = window.DOMPurify;
  if (!M || !M.Marked || !P || !P.sanitize || !P.isSupported) return (mdLib = false);

  P.addHook("afterSanitizeAttributes", (node) => {
    const tag = node.nodeName;
    if (node.hasAttribute("class")) {
      // Keep only the classes this code emits (code languages, the table
      // wrapper); agent HTML must not borrow the app's own UI classes.
      const keep = node.getAttribute("class").split(/\s+/).filter((c) => /^(language-[\w+#.-]+|md-table)$/.test(c));
      if (keep.length) node.setAttribute("class", keep.join(" "));
      else node.removeAttribute("class");
    }
    if (tag === "A") {
      if (node.hasAttribute("href") && !/^(?:https?:|mailto:)/i.test(node.getAttribute("href"))) {
        node.removeAttribute("href"); // belt and braces with ALLOWED_URI_REGEXP
      }
      if (node.hasAttribute("href")) {
        node.setAttribute("target", "_blank");
        node.setAttribute("rel", "noopener noreferrer");
      }
    } else if (tag === "INPUT") {
      // Only GFM task-list boxes: read-only checkboxes.
      if (node.getAttribute("type") !== "checkbox") { node.remove(); return; }
      node.setAttribute("disabled", "");
    }
    for (const a of ["type", "checked", "disabled"]) {
      if (tag !== "INPUT" && node.hasAttribute(a)) node.removeAttribute(a);
    }
    if (tag !== "OL" && node.hasAttribute("start")) node.removeAttribute("start");
  });

  const m = new M.Marked({ gfm: true, breaks: true, async: false });
  const base = new M.Renderer();
  m.use({
    renderer: {
      html(token) {
        const t = token.text;
        if (/^\s*<!--[\s\S]*-->\s*$/.test(t)) return ""; // comments stay hidden
        const tags = [...t.matchAll(/<\/?([A-Za-z][\w-]*)/g)].map((x) => x[1].toLowerCase());
        if (!/<!--|<!|<\?/.test(t) && tags.length && tags.every((n) => MD_RAW_TAGS.has(n))) return t;
        return esc(t).replace(/\n/g, token.block ? "<br>" : "\n");
      },
      // Remote images are blocked by the CSP anyway: show a link instead.
      image(token) {
        const label = "🖼 " + (token.text || token.href || "image");
        if (!/^https?:/i.test(token.href || "")) return esc(label);
        return `<a href="${escAttr(token.href)}">${esc(label)}</a>`;
      },
      // Tables scroll sideways inside their own box, never the page.
      table(token) {
        return `<div class="md-table">${base.table.call(this, token)}</div>`;
      },
    },
  });
  // Titles and answers: raw HTML is shown as text, never interpreted.
  const inline = new M.Marked({ gfm: true, breaks: true, async: false });
  inline.use({ renderer: { html: (token) => esc(token.text) } });
  return (mdLib = { m, P, inline });
}

function escAttr(s) {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// mdCache memoizes rendered HTML by message so reopening a chat or loading
// earlier pages does not parse the same long messages again.
const mdCache = new Map();
const MD_CACHE_MAX = 400;

// renderMarkdown returns sanitized HTML for untrusted markdown text.
function renderMarkdown(src, key) {
  src = String(src ?? "");
  const hit = key && mdCache.get(key);
  if (hit && hit.src === src) return hit.html;
  let html;
  const lib = src.length <= MD_MAX_CHARS ? mdSetup() : false;
  if (!lib) {
    html = `<div class="md-plain">${esc(src)}</div>`;
  } else {
    try {
      html = lib.P.sanitize(lib.m.parse(src), MD_PURIFY);
    } catch {
      html = `<div class="md-plain">${esc(src)}</div>`;
    }
  }
  if (key) {
    mdCache.delete(key);
    if (mdCache.size >= MD_CACHE_MAX) mdCache.delete(mdCache.keys().next().value);
    mdCache.set(key, { src, html });
  }
  return html;
}

// renderInline returns sanitized HTML for a short text shown inside a line
// or a box (a to-do title, an answer): code spans, bold/italic, strikethrough
// and links, no block elements. Single newlines stay line breaks. Escaped
// text when the libraries are missing.
// links=false drops the anchors but keeps their text (a title sits inside a
// <button>, where a link must not be).
const MD_INLINE_TAGS = ["b", "br", "code", "del", "em", "i", "s", "strong"];
function renderInline(src, links = true) {
  src = String(src ?? "");
  const lib = src.length <= MD_MAX_CHARS ? mdSetup() : false;
  if (!lib) return esc(src);
  try {
    return lib.P.sanitize(lib.inline.parseInline(src), { ...MD_PURIFY, ALLOWED_TAGS: links ? ["a", ...MD_INLINE_TAGS] : MD_INLINE_TAGS });
  } catch {
    return esc(src);
  }
}

// mdOnOpen fills a <details> body with rendered markdown the first time it
// is opened: thinking and compaction summaries are usually never opened.
function mdOnOpen(details, target, text, key) {
  details.addEventListener("toggle", () => {
    if (!details.open || target.dataset.md) return;
    target.dataset.md = "1";
    target.innerHTML = renderMarkdown(text, key);
  });
}

/* ---------- agent list ---------- */

function activeMs(a) {
  const t = a.last_active ? Date.parse(a.last_active) : NaN;
  return Number.isNaN(t) ? 0 : t;
}

// sinceMs is when the agent entered its current status (0 if unknown).
function sinceMs(a) {
  const t = a && a.status_since ? Date.parse(a.status_since) : NaN;
  return Number.isNaN(t) ? 0 : t;
}

// fmtClock renders an elapsed time as m:ss (h:mm:ss past an hour).
function fmtClock(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const two = (n) => String(n).padStart(2, "0");
  if (s >= 3600) return `${Math.floor(s / 3600)}:${two(Math.floor(s / 60) % 60)}:${two(s % 60)}`;
  return `${Math.floor(s / 60)}:${two(s % 60)}`;
}

// fmtAgo renders a compact elapsed time: 45s, 12m, 3h, 2d.
function fmtAgo(ms) {
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

const STATUS_ORDER = { blocked: 0, working: 1, done: 2, idle: 3, unknown: 4 };

/* ---------- host filter (hub with more than one host) ---------- */

const HOST_FILTER_KEY = "herdr-host-filter";
function loadHostFilter() {
  try { return localStorage.getItem(HOST_FILTER_KEY) || "all"; } catch { return "all"; }
}
function setHostFilter(name) {
  state.hostFilter = name;
  try { localStorage.setItem(HOST_FILTER_KEY, name); } catch {}
  renderList();
}
// activeHostFilter falls back to "all" when the remembered host is no
// longer configured (or this is a single-host instance).
function activeHostFilter() {
  if (state.hostFilter === null) state.hostFilter = loadHostFilter();
  const f = state.hostFilter || "all";
  if (f === "all" || !multiHost() || !state.hosts.some((h) => h.name === f)) return "all";
  return f;
}

function renderHostFilter() {
  const nav = $("host-filter");
  if (!multiHost()) { nav.hidden = true; nav.innerHTML = ""; return; }
  const active = activeHostFilter();
  const chip = (name, label, agents, down, host) => {
    const need = agents.some((a) => a.agent_status === "blocked");
    // A peer on another version than the hub (the hub reports the
    // mismatch): a small badge naming the version it runs.
    const local = state.hosts.find((x) => x.local);
    const ver = host && host.version_mismatch && host.version
      ? `<span class="ver" title="${esc(host.name)} runs ${esc(host.version)}${local && local.version ? `; this hub runs ${esc(local.version)}` : ""}">${esc(host.version)}</span>` : "";
    return `<button type="button" class="hf-chip${down ? " down" : ""}" data-host="${esc(name)}"` +
      ` aria-pressed="${active === name}">${need ? '<span class="need" aria-label="needs input"></span>' : ""}` +
      `${esc(label)} <span class="n">${down ? "down" : agents.length}</span>${ver}</button>`;
  };
  nav.innerHTML = chip("all", "All", state.agents, false) + state.hosts.map((h) =>
    chip(h.name, h.name, state.agents.filter((a) => a.host === h.name), !h.local && h.status !== "ok", h)).join("");
  nav.hidden = false;
  nav.querySelectorAll(".hf-chip").forEach((el) =>
    el.addEventListener("click", () => setHostFilter(el.dataset.host)));
}

function renderList() {
  // Re-rendering replaces the rows: keep the keyboard focus on the same one
  // (the desktop arrow-key browsing would otherwise lose it on every poll).
  const fid = document.activeElement && document.activeElement.closest ? document.activeElement.closest("#agents .agent[data-id]") : null;
  renderListRows();
  markSelectedRow();
  if (fid) { const r = rowFor(fid.dataset.id); if (r) r.focus({ preventScroll: true }); }
}

// emptyHtml is the one empty state: a plain line saying why nothing is shown,
// what will appear there (hint, trusted HTML) and, where it helps, the one
// action to take (act = [label, key]; a click on .empty-act[data-act=key]).
function emptyHtml(title, hint, act) {
  return `<div class="empty"><b>${esc(title)}</b>${hint ? `<span>${hint}</span>` : ""}${act ? `<button type="button" class="empty-act" data-act="${escAttr(act[1])}">${esc(act[0])}</button>` : ""}</div>`;
}

function renderListRows() {
  const wrap = $("agents");
  renderHostFilter();
  const filter = activeHostFilter();
  // A host filter never hides an agent that is waiting on you.
  const agents = state.agents.filter((a) => filter === "all" || a.host === filter || a.agent_status === "blocked").sort((a, b) =>
    (STATUS_ORDER[a.agent_status] ?? 9) - (STATUS_ORDER[b.agent_status] ?? 9) ||
    // Within a status, most recently active first, so the agents that just
    // went idle sit above the ones that have been idle for days.
    activeMs(b) - activeMs(a) ||
    agentId(a).localeCompare(agentId(b)));
  const nBlocked = agents.filter((a) => a.agent_status === "blocked").length;
  const nWorking = agents.filter((a) => a.agent_status === "working").length;
  // The tab title carries the count, so a background tab shows it.
  document.title = nBlocked ? `(${nBlocked}) Comebye` : "Comebye";
  // The long form on wide screens; a short one (no "agents" word, "need")
  // on a phone, where the header also holds +, the bell and the ⋯.
  $("count").innerHTML = agents.length
    ? `<span class="long">${nBlocked ? `<span class="need">${nBlocked} need${nBlocked === 1 ? "s" : ""} input</span> · ` : ""}` +
      `${nWorking ? `<span class="run">${nWorking} working</span> · ` : ""}${agents.length} agent${agents.length === 1 ? "" : "s"}</span>` +
      `<span class="short">${nBlocked ? `<span class="need">${nBlocked} need</span> · ` : ""}` +
      `${nWorking ? `<span class="run">${nWorking} working</span> · ` : ""}${agents.length}</span>`
    : "";
  // Peers the hub cannot reach right now: one collapsed row each, saying
  // what broke (dns, tcp, tls, http) and since when - the hub's health
  // probe keeps the diagnosis fresh - instead of a bare error.
  const downRows = state.hosts.filter((h) => !h.local && h.status !== "ok" &&
    (filter === "all" || h.name === filter)).map((h) => {
    const since = h.since ? Date.parse(h.since) : NaN;
    let what;
    if (h.status === "connecting") {
      what = "connecting…";
    } else {
      what = "unreachable" + (h.class ? `: ${h.class}` : "") +
        (Number.isNaN(since) ? "" : ` (since ${fmtAgo(since)} ago)`);
    }
    return `
    <div class="agent host-down" role="status" title="${esc(h.error || "")}">
      <span class="dot unknown"></span>
      <div class="meta">
        <div class="name">${esc(h.name)}</div>
        <div class="sub">${esc(what)}</div>
      </div>
      <span class="status">${h.status === "connecting" ? "…" : "down"}</span>
    </div>`;
  }).join("");
  renderRemovalBanner();
  renderBrief();
  renderPlans();
  renderNav();
  renderStatus();
  if (!agents.length) {
    const f = activeHostFilter();
    wrap.innerHTML = (state.unauthorized ? `<div class="empty"><b>${esc(UNAUTHORIZED_MSG)}</b></div>`
      : f !== "all" ? emptyHtml(`No agents on ${f}`, "", ["Show all hosts", "all-hosts"])
      : emptyHtml("No agents running", "Start one with the + button, and it shows up here.")) + downRows;
    const act = wrap.querySelector(".empty-act");
    if (act) act.onclick = () => setHostFilter("all");
    return;
  }
  // Coordinators are pinned above the projects.
  // Desktop: the agents waiting on you sit in a group of their own at the top.
  const needs = isDesktop() ? agents.filter((a) => a.agent_status === "blocked") : [];
  const needsSec = needs.length ? `<section class="proj needs-sec" aria-label="Needs you">
    <div class="proj-head static"><span class="proj-name">Needs you</span><span class="proj-counts"><span class="need">${needs.length}</span></span></div>
    <div class="proj-body">${needs.map((a) => agentRow(a, false)).join("")}</div></section>` : "";
  const coords = agents.filter((a) => isCoordinator(a) && !needs.includes(a));
  const pinned = coords.length ? `<section class="proj coord-sec" aria-label="Coordinators">
    <div class="proj-head static"><span class="proj-name">Coordinators</span></div>
    <div class="proj-body">${coords.map((a) => agentRow(a, false)).join("")}</div></section>` : "";
  const rest = agents.filter((a) => !isCoordinator(a) && !needs.includes(a));
  wrap.innerHTML = needsSec + pinned + projectGroups(rest).map(([name, list]) => projectHTML(name, list)).join("") + downRows;
  wrap.querySelectorAll(".proj-head").forEach((el) =>
    el.addEventListener("click", () => toggleProject(el.dataset.proj)));
  wrap.querySelectorAll(".agent[data-id]").forEach((el) => {
    el.addEventListener("click", (e) => {
      const more = e.target.closest(".row-more");
      if (more) { e.stopPropagation(); openRowMenu(more); return; }
      openDetail(el.dataset.id);
    });
    el.addEventListener("keydown", (e) => {
      if (e.target.closest(".row-more")) return;
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openDetail(el.dataset.id); }
    });
  });
}

/* ---------- desktop view (>= 1100px, see the CSS): list + agent side by side ---------- */

const desktopMQ = window.matchMedia("(min-width: 1100px)");
const isDesktop = () => desktopMQ.matches;
const rowFor = (id) => [...document.querySelectorAll("#agents .agent[data-id]")].find((r) => r.dataset.id === id);

function markSelectedRow() {
  document.querySelectorAll("#agents .agent.selected").forEach((r) => {
    r.classList.remove("selected");
    r.removeAttribute("aria-current");
  });
  const r = state.selected && rowFor(state.selected);
  if (r) { r.classList.add("selected"); r.setAttribute("aria-current", "true"); }
}

// "n" jumps to the next agent that is waiting on you (desktop).
function onNextWaiting(ev) {
  if (!isDesktop() || ev.key !== "n" || ev.altKey || ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.repeat) return;
  if (ev.target.closest && ev.target.closest("input, textarea, select, [contenteditable], [role=dialog]")) return;
  const rows = [...document.querySelectorAll("#agents .agent.blocked[data-id]")];
  if (!rows.length) return;
  const i = rows.findIndex((r) => r.dataset.id === state.selected);
  ev.preventDefault();
  const r = rows[(i + 1) % rows.length];
  openDetail(r.dataset.id);
  r.scrollIntoView?.({ block: "nearest" });
}

// Up/Down on a row moves to the previous/next one and opens it.
function onListArrow(ev) {
  if (!isDesktop() || (ev.key !== "ArrowDown" && ev.key !== "ArrowUp")) return;
  if (ev.altKey || ev.ctrlKey || ev.metaKey || ev.shiftKey) return;
  const cur = ev.target.closest && ev.target.closest("#agents .agent[data-id]");
  if (!cur || ev.target.closest(".row-more")) return;
  const rows = [...document.querySelectorAll("#agents .agent[data-id]")].filter((r) => r.offsetParent);
  const i = rows.indexOf(cur) + (ev.key === "ArrowDown" ? 1 : -1);
  if (i < 0 || i >= rows.length) { ev.preventDefault(); return; }
  ev.preventDefault();
  rows[i].focus();
  rows[i].scrollIntoView?.({ block: "nearest" });
  openDetail(rows[i].dataset.id);
}

function agentRow(a, worker) {
  const id = agentId(a);
  const marked = ledger.on === true && ledger.marked.has(id);
  const nm = a.name || a.pane_id || id;
  return `
    <div class="agent ${esc(a.agent_status)}${worker ? " worker" : ""}${marked ? " marked" : ""}" role="button" tabindex="0" data-id="${esc(id)}">
      <span class="dot ${esc(a.agent_status)}"></span>
      <div class="meta">
        <div class="name">${esc(a.name || a.terminal_title_stripped || a.display_agent || a.agent || a.pane_id)}${
          isCoordinator(a) ? '<span class="role-chip">coordinator</span>' : ""}</div>
        ${subLine(a)}
      </div>
      <span class="status ${esc(a.agent_status)}">${esc(a.agent_status)}${
        activeMs(a) && (a.agent_status === "idle" || a.agent_status === "done")
          ? `<br><span class="ago">${fmtAgo(activeMs(a))}</span>`
          : a.agent_status === "working" && sinceMs(a)
            ? `<br><span class="ago">for ${fmtAgo(sinceMs(a))}</span>` : ""}</span>${
      ledger.on === true ? `<button type="button" class="row-more" data-menu="${esc(id)}" aria-haspopup="menu" aria-label="${esc((marked ? "Unmark " : "Mark ready to remove: ") + nm)}">&#8942;</button>` : ""}
    </div>`;
}

/* ---------- projects, workers and the handoff ledger ---------- */
//
// A project is a repository. Its home agents work in the primary checkout;
// workers each have their own git worktree (~/.worktrees/<repo>/<branch>)
// and branch. The hub's ledger records the workers it handed work to
// (running -> finished, with the worker's final report) and the sessions
// marked ready to remove. The list groups agents by project: home agents
// first, then the workers, indented, each with its ledger status.

const ledgerOpt = new Overlay(); // marks and roles changed here, on the wire or just confirmed
const ledger = {
  on: null,            // null until loaded; false when the server keeps no ledger
  entries: [],
  marks: [],           // marks on agents that are not ledger workers
  byAgent: new Map(),  // agent id -> its active entry
  marked: new Map(),   // agent id -> mark ({by, reason, at})
  roles: new Map(),    // agent id -> role ({role: "coordinator", name, by, at})
  loadedAt: 0,
  timer: null,
  inflight: false,
};
const ACTIVE_ENTRY = new Set(["running", "finished", "landing", "landed", "ready_to_remove"]);
// ~/.worktrees/<repo>/<branch>: a worker's checkout by convention.
const WT_PATH_RE = /\/\.worktrees\/([^/]+)\/([^/]+)/;

function ledgerEntry(a) { return ledger.byAgent.get(agentId(a)); }

function setLedger(d) {
  ledger.on = true;
  ledger.plans = Array.isArray(d.plans) ? d.plans : [];
  ledger.entries = Array.isArray(d.entries) ? d.entries : [];
  ledger.marks = Array.isArray(d.marks) ? d.marks : [];
  ledger.byAgent = new Map();
  ledger.marked = new Map();
  for (const e of ledger.entries) {
    if (!e || !e.agent_id || !ACTIVE_ENTRY.has(e.status)) continue;
    ledger.byAgent.set(e.agent_id, e);
    if (e.status === "ready_to_remove") ledger.marked.set(e.agent_id, e.mark || {});
  }
  for (const m of ledger.marks) if (m && m.agent_id) ledger.marked.set(m.agent_id, m);
  ledger.roles = new Map(d.roles && typeof d.roles === "object" ? Object.entries(d.roles) : []);
  ledgerOpt.each((e) => e.apply());
}

// redrawMarks repaints what a mark or role change shows.
function redrawMarks() {
  renderList();
  if (state.selected) renderDetailMenu();
}

// isCoordinator: the hub records the agent as a coordinator (and the pane
// still runs the same agent: herdr reuses pane ids).
function isCoordinator(a) {
  const r = a && ledger.roles.get(agentId(a));
  return !!r && r.role === "coordinator" && (!r.name || !a.name || r.name === a.name);
}

async function loadLedger() {
  if (ledger.inflight || ledger.on === false) return;
  ledger.inflight = true;
  try {
    setLedger(await api("/api/ledger"));
    // The briefing rides along, at most every 20 seconds.
    if (Date.now() - briefing.loadedAt > 20000) loadBrief();
    if (Date.now() - todos.loadedAt > 8000) loadTodos();
    // The server checks finished workers' worktrees in the background
    // (for the "merged" hint): look again shortly until they are in.
    const waiting = ledger.entries.some((e) => (e.status === "finished" || e.status === "landed") && e.worktree && !e.checks);
    ledger.checkPolls = waiting ? (ledger.checkPolls || 0) + 1 : 0;
    if (waiting && ledger.checkPolls <= 10 && !document.hidden) {
      clearTimeout(ledger.timer);
      ledger.timer = setTimeout(() => { ledger.timer = null; loadLedger(); }, 3000);
    }
  } catch (e) {
    if (e.status === 503) { ledger.on = false; briefing.data = null; } // no state directory: no handoffs or marks here
  } finally {
    ledger.inflight = false;
    ledger.loadedAt = Date.now();
  }
  renderList();
  if (state.selected) renderDetailMenu();
  if (launch.open) renderLaunchForm();
}

// scheduleLedger refreshes the ledger after agent updates (a worker going
// idle finishes it), at most every few seconds.
function scheduleLedger() {
  if (ledger.on === false || ledger.timer || document.hidden) return;
  const wait = Math.max(0, ledger.loadedAt + 5000 - Date.now());
  ledger.timer = setTimeout(() => { ledger.timer = null; loadLedger(); }, wait);
}

function ledgerChip(a) {
  const id = agentId(a);
  if (ledger.marked.has(id)) return '<span class="lg-chip rtr">ready to remove</span>';
  const e = ledger.byAgent.get(id);
  if (!e) return "";
  const lc = e.land_check;
  if (e.status === "landing") {
    const why = e.approval ? `Landing, approved by ${e.approval.by || "?"}: ${e.approval.note || ""}` : "Landing";
    return `<span class="lg-chip landing" title="${esc(why)}">landing</span>`;
  }
  if (e.status === "landed") {
    const as = lc && lc.landed && lc.landed.length ? ` as ${lc.landed.map((s) => s.slice(0, 7)).join(", ")}` : "";
    return `<span class="lg-chip candidate" title="${esc(`Landed on ${(lc && lc.base) || "its base"}${as}`)}">landed</span>`;
  }
  if (e.status === "finished" && lc && !lc.ok && e.approval && Date.parse(lc.at) >= Date.parse(e.finished_at || 0)) {
    return `<span class="lg-chip failed" title="${esc("The landing check failed: " + (lc.verdict || ""))}">land failed</span>`;
  }
  if (e.status === "finished") {
    return e.candidate
      ? '<span class="lg-chip candidate" title="Finished; its worktree is clean and merged">finished · merged</span>'
      : '<span class="lg-chip finished">finished</span>';
  }
  // A running worker whose brief was never delivered looks broken, not
  // busy: the error says why (and how) it can be resent.
  if (e.status === "running" && e.error)
    return `<span class="lg-chip failed" title="${esc(e.error)}">no brief</span>`;
  return '<span class="lg-chip running">running</span>';
}

function projectOf(a) {
  return repoLabel(a).repo || "";
}

function isWorker(a) {
  return ledger.byAgent.has(agentId(a)) || !!(a.worktree && a.worktree.linked) || WT_PATH_RE.test(a.cwd || "");
}

// projectGroups splits the (sorted) agents by project: the group with the
// most urgent agent first, then the most recently active; "Other" (no
// directory) last.
function projectGroups(agents) {
  const groups = new Map();
  for (const a of agents) {
    const p = projectOf(a);
    if (!groups.has(p)) groups.set(p, []);
    groups.get(p).push(a);
  }
  const rank = (list) => Math.min(...list.map((a) => STATUS_ORDER[a.agent_status] ?? 9));
  const recent = (list) => Math.max(...list.map(activeMs));
  return [...groups.entries()].sort(([na, la], [nb, lb]) =>
    (na === "") - (nb === "") || rank(la) - rank(lb) || recent(lb) - recent(la) || na.localeCompare(nb));
}

const COLLAPSED_KEY = "comebye-collapsed-projects";
let collapsedProjects = null;
function collapsed() {
  if (!collapsedProjects) {
    try { collapsedProjects = new Set(JSON.parse(localStorage.getItem(COLLAPSED_KEY) || "[]")); }
    catch { collapsedProjects = new Set(); }
  }
  return collapsedProjects;
}
function toggleProject(name) {
  const c = collapsed();
  if (c.has(name)) c.delete(name); else c.add(name);
  try { localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...c].slice(-200))); } catch {}
  renderList();
}

function projectHTML(name, list) {
  const open = !collapsed().has(name);
  const nBlocked = list.filter((a) => a.agent_status === "blocked").length;
  const nWorking = list.filter((a) => a.agent_status === "working").length;
  const counts = (nBlocked ? `<span class="need">${nBlocked} need${nBlocked === 1 ? "s" : ""} input</span>` : "") +
    (nWorking ? `<span class="run">${nWorking} working</span>` : "") + `<span>${list.length}</span>`;
  const home = list.filter((a) => !isWorker(a));
  const workers = list.filter(isWorker);
  return `<section class="proj">
    <button type="button" class="proj-head" data-proj="${esc(name)}" aria-expanded="${open}">
      <span class="proj-chev" aria-hidden="true"></span><span class="proj-name">${esc(name || "Other")}</span>
      <span class="proj-counts">${counts}</span>
    </button>
    <div class="proj-body"${open ? "" : " hidden"}>${home.map((a) => agentRow(a, false)).join("")}${workers.map((a) => agentRow(a, true)).join("")}</div>
  </section>`;
}

function renderRemovalBanner() {
  const b = $("rtr-banner");
  const n = ledger.on ? ledger.marked.size : 0;
  b.hidden = !n;
  if (n) b.innerHTML = `Ready to remove (${n})<span class="ln-chev" aria-hidden="true">›</span>`;
  renderFleetMenu();
}

/* ---------- the briefing (published by a coordinator) ---------- */
//
// A coordinator publishes a short markdown briefing to the hub
// (`comebye brief set`); it shows as a card at the top of the list. The
// card is collapsed by default, and a new briefing does not expand it: the
// collapsed header shows the title, the age and the first line of the
// briefing (the coordinator's status line) as a preview, so a new one is
// still noticeable. Tapping expands it; that choice is a persistent
// per-browser preference (not tied to one briefing), so an expanded card
// stays expanded across new briefings until it is collapsed again. The
// card can also be dismissed: a dismissed briefing stays hidden until a
// newer one is published (remembered per browser by its time). The stale
// note sits outside the foldable body, so it shows either way.
//
// The hub can also ask a coordinator for one on a schedule (off by
// default; set here or by a coordinator, `comebye brief schedule`). When
// scheduled runs are missed (no coordinator running, or it never
// published), the card says the briefing is stale and why, with a way to
// start a coordinator. Dismissing that note is stored on the hub, for this
// streak of misses only; the next briefing ends the streak.

const briefing = { data: null, schedule: null, loadedAt: 0, inflight: false, acking: null };
const BRIEF_DISMISSED_KEY = "comebye-brief-dismissed";
const BRIEF_EXPANDED_KEY = "comebye-brief-expanded"; // "1" once the user expanded the card

function lsGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch {} }

async function loadBrief() {
  if (briefing.inflight) return;
  if (ledger.on !== true) { briefing.data = null; briefing.schedule = null; renderBrief(); return; }
  briefing.inflight = true;
  try {
    const d = await api("/api/brief");
    briefing.data = d && d.brief && typeof d.brief.markdown === "string" ? d.brief : null;
    briefing.schedule = d && d.schedule && typeof d.schedule === "object" ? d.schedule : null;
  } catch {
    // Keep what we had: a failed poll should not make the card blink.
  } finally {
    briefing.inflight = false;
    briefing.loadedAt = Date.now();
  }
  renderBrief();
  renderStatus();
  if (briefs.open && Date.now() - briefs.histAt > 15000) loadBriefHistory();
}

const MISS_REASONS = {
  no_coordinator: "no coordinator is running",
  busy: "the coordinator stayed busy",
  no_briefing: "the coordinator was asked but published nothing",
};

// briefChecked is the note "checked 14:05, no change" the hub keeps with the
// latest briefing when it skipped a scheduled run because nothing a briefing
// reports had changed ("" when there is none, or a newer briefing replaced it).
function briefChecked() {
  const sc = briefing.schedule;
  const t = sc && sc.enabled && sc.checked_at ? Date.parse(sc.checked_at) : NaN;
  if (Number.isNaN(t)) return "";
  return `checked ${new Date(t).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })}, no change`;
}

// briefMiss is the streak of missed scheduled briefings to show, if any.
function briefMiss() {
  const m = briefing.schedule && briefing.schedule.enabled ? briefing.schedule.missed : null;
  return m && !m.dismissed && m.count > 0 && m.id !== briefing.acking ? m : null;
}

function fmtWhen(iso) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  return new Date(t).toLocaleString(undefined, { weekday: "short", hour: "numeric", minute: "2-digit" });
}

function missNote(m, hasBrief) {
  const runs = m.count === 1 ? "1 scheduled run" : `${m.count} scheduled runs`;
  const why = MISS_REASONS[m.reason] || m.reason;
  const text = `${hasBrief ? "Stale: " : ""}${runs} missed since ${fmtWhen(m.since)}; ${why}.`;
  return `<div class="bf-stale" role="status">
      <p>${esc(text)}</p>
      <div class="bf-stale-acts">
        ${m.reason === "no_coordinator" ? `<button type="button" class="bf-start">Start coordinator</button>` : ""}
        <button type="button" class="bf-sched">Schedule…</button>
        <button type="button" class="bf-ack">Dismiss</button>
      </div>
    </div>`;
}

// renderPlans shows the plans (several workers, one change) that are open
// or settled within the last day: how many workers finished and where each
// stands. The hub pushes when a plan settles; this is for looking.
const PLAN_SHOWN_MS = 24 * 3600 * 1000;
const planOpen = new Set(); // plans whose card is expanded (the page's own state)

// planMemberTarget says where a member leads: its agent while that is still
// running, else the archived session in Search (the worker was removed).
function planMemberTarget(m) {
  const a = m.agent_id ? agentById(m.agent_id) : null;
  if (a) return { hash: agentHash(a), live: a, tip: `Open ${m.repo_name || "?"}/${m.branch}` };
  return { hash: rcSearchHash(m.branch || m.repo_name || ""), live: null, tip: "This agent is gone: search its archived session" };
}

function planMemberHtml(m, p, open) {
  const t = planMemberTarget(m);
  // The outcome is what the member reported; the live status is what it does now.
  let live = "";
  if (m.followup === "landing") live = `<span class="pm-live landing">landing</span>`;
  else if (t.live && t.live.agent_status === "working" && m.outcome !== "pending") live = `<span class="pm-live working">working</span>`;
  const e = open ? ledger.entries.find((x) => x.id === m.id) : null;
  const when = e && Date.parse(e.created_at) ? ` · started ${fmtAgo(Date.parse(e.created_at))} ago` : "";
  const detail = open ? `<div class="pm-detail">${esc(m.host || "")}${m.host ? " · " : ""}${esc(m.status)}${esc(when)}${e && e.final_report ? `<details class="pm-rep"><summary>Report</summary><div class="md">${renderMarkdown(e.final_report, "pm" + e.id)}</div></details>` : ""}</div>` : "";
  return `<li><a class="pm${t.live ? "" : " gone"}" href="${esc(t.hash)}" title="${esc(t.tip)}"><span class="pm-name">${esc(m.repo_name || "?")}/${esc(m.branch)}</span><span class="pm-${esc(m.outcome)}">${esc(m.outcome === "pending" ? m.status : m.outcome)}</span>${live}</a>${detail}</li>`;
}

function renderPlans() {
  const el = $("plans");
  if (!el) return;
  const now = Date.now();
  const shown = (ledger.on === true && Array.isArray(ledger.plans) ? ledger.plans : []).filter((p) => {
    if (!p || !Array.isArray(p.members)) return false;
    if (p.state === "open") return true;
    const at = Date.parse(p.settled_at);
    return !Number.isNaN(at) && now - at < PLAN_SHOWN_MS;
  });
  const sig = JSON.stringify([shown.map((p) => [p.id, p.state, p.done, p.landing, p.working, planOpen.has(p.id), p.members.map((m) => [m.id, m.outcome, m.status, m.followup, m.agent_id, m.reported, !!(m.agent_id && agentById(m.agent_id)), m.agent_id && agentById(m.agent_id)?.agent_status])]), ledger.entries.length]);
  if (el.dataset.sig === sig) return;
  el.dataset.sig = sig;
  el.hidden = !shown.length;
  const label = { open: "in progress", done: "done", partial: "needs a look" };
  // The plan card keeps keyboard focus across a re-render. After a mouse
  // click nothing is restored: a script's focus() would draw the ring.
  const ae = document.activeElement;
  const focus = el.contains(ae) && ae.matches(":focus-visible") ? document.activeElement.dataset.plan || document.activeElement.getAttribute("href") : null;
  el.innerHTML = shown.map((p) => {
    const open = planOpen.has(p.id);
    const follow = [p.landing ? `${p.landing} landing` : "", p.working ? `${p.working} working` : ""].filter(Boolean).join(", ");
    const created = Date.parse(p.created_at);
    return `<div class="plan ${esc(p.state)}">
      <div class="plan-head"><button type="button" class="plan-title" data-plan="${esc(p.id)}" aria-expanded="${open}" title="${open ? "Collapse" : "Show members, branches and reports"}">${esc(p.title)}</button><span class="plan-state">${esc(label[p.state] || p.state)}${follow ? ` · ${esc(follow)}` : ` · ${p.done} of ${p.size}`}</span></div>
      ${open ? `<div class="plan-meta">${p.done} of ${p.size} finished${created ? ` · started ${fmtAgo(created)} ago` : ""}${p.by ? ` · by ${esc(p.by)}` : ""}</div>` : ""}
      <ul class="plan-members">${p.members.map((m) => planMemberHtml(m, p, open)).join("")}</ul>
    </div>`;
  }).join("");
  if (focus) {
    const again = [...el.querySelectorAll("[data-plan], a.pm")].find((x) => x.dataset.plan === focus || x.getAttribute("href") === focus);
    if (again) again.focus({ preventScroll: true });
  }
}

function initPlans() {
  $("plans").addEventListener("click", (ev) => {
    const b = ev.target.closest("button.plan-title");
    if (!b) return;
    const id = b.dataset.plan;
    if (planOpen.has(id)) planOpen.delete(id); else planOpen.add(id);
    renderPlans();
  });
}


/* ---------- the to-do panel ---------- */
//
// Questions and decisions the user has to deal with, and follow-ups, that
// coordinators and workers put on the hub's to-do list (`comebye todo`).
// The panel sits under the briefing on the phone; on a wide screen it is the
// To do page, in the main pane. Answering is the user's
// action here: the hub then tells the coordinator, who relays the words to
// the agent that waits. Every string comes from an agent (the question, the
// context, the links) and is escaped or sanitized; links must be http(s).

// One item is open at a time (an accordion). The open item, the answered
// group's fold and the drafts are the page's own state; drafts are kept in
// localStorage so an unsent answer survives a reload, and outlive an item
// that is closed or answered elsewhere until the user discards them.
const TODO_FOLDED_KEY = "comebye-todo-folded";
const TODO_DRAFTS_KEY = "comebye.todo.drafts";
const TODO_KIND = { decision: "decision", action: "for you", followup: "follow-up", release: "release" };
const todos = { on: null, items: [], open: 0, answered: 0, loadedAt: 0, inflight: false,
  openId: null, lastId: null, nxt: null, ansOpen: false, seen: new Map(), order: [], follow: false,
  drafts: loadTodoDrafts(), pend: new Overlay() };

function todoLinkHref(u) { return /^https?:\/\//i.test(u || "") || /^\/#\/agent\/[^\s]+$/.test(u || "") ? u : ""; }
// The provenance chip: items the hub filed itself say so (and why).
const TODO_SOURCE = { "hub-blocked": "auto · blocked", "hub-report": "from report" };
function todoSrcChip(t) {
  const label = TODO_SOURCE[t.source];
  return label ? `<span class="td-chip td-src" title="Filed by the hub, not by an agent or a person">${esc(label)}</span>` : "";
}

// drafts: item id -> { text, q }; q is the question, for a draft whose item is gone.
function loadTodoDrafts() {
  const m = new Map();
  try {
    const d = JSON.parse(lsGet(TODO_DRAFTS_KEY) || "{}");
    for (const [id, v] of Object.entries(d || {})) {
      if (v && typeof v.text === "string" && v.text.trim()) m.set(id, { text: v.text, q: String(v.q || "") });
    }
  } catch {}
  return m;
}
function saveTodoDrafts() { lsSet(TODO_DRAFTS_KEY, JSON.stringify(Object.fromEntries(todos.drafts))); }
function todoDraft(id) { const d = todos.drafts.get(id); return d ? d.text : ""; }

async function loadTodos() {
  if (todos.inflight || ledger.on !== true) { if (ledger.on !== true) { todos.on = false; renderTodos(); } return; }
  todos.inflight = true;
  try {
    const d = await api("/api/todos");
    todos.on = true;
    todos.items = Array.isArray(d.todos) ? d.todos : [];
    todos.open = d.open | 0;
    todos.answered = d.answered | 0;
    todoReapply();
  } catch (e) {
    if (e.status === 503 || e.status === 404) todos.on = false;
    // Otherwise keep what we had: a failed poll should not make the panel blink.
  } finally {
    todos.inflight = false;
    todos.loadedAt = Date.now();
  }
  renderTodos();
}

function todoCountText() {
  const p = [];
  if (todos.open) p.push(`${todos.open} open`);
  if (todos.answered) p.push(`${todos.answered} answered`);
  return p.join(" · ");
}

// Open items first; the answered ones wait in their own group at the bottom.
function todoOrder() {
  return todos.items.filter((t) => t.state === "open").concat(todos.items.filter((t) => t.state !== "open"));
}

// The next open item after id, in the order the list showed (wrapping to
// the top), or null when nothing else is open.
function todoNextOpen(id, order) {
  const open = todoOrder().filter((t) => t.state === "open" && t.id !== id).map((t) => t.id);
  const at = order.indexOf(id);
  for (let k = 1; k <= order.length; k++) {
    const x = order[(at + k + order.length) % order.length];
    if (open.includes(x)) return x;
  }
  return open[0] || null;
}

function todoOrphans() {
  if (todos.on !== true) return [];
  const ids = new Set(todos.items.map((t) => t.id));
  return [...todos.drafts].filter(([id, d]) => !ids.has(id) && d.text.trim());
}

function todoKeptHtml(id, text, label) {
  return `<div class="td-kept" data-kept="${escAttr(id)}"><div class="td-kept-h">${esc(label)}</div>
      <textarea class="td-kept-t" rows="2" readonly aria-label="Your unsent answer">${esc(text)}</textarea>
      <div class="td-acts"><button type="button" class="td-copy">Copy</button><button type="button" class="td-discard">Discard</button></div></div>`;
}

// todoRecommended finds the recommendation line ("I recommend ok 2",
// "Recommendation: **1**", "recommend 1+3", "recommend option 2": bold markers
// are fine) and returns the option numbers it names that exist.
function todoRecommended(lines, n) {
  for (const l0 of lines) {
    const l = String(l0).replace(/[*_`]/g, "");
    const NUMS = "(\\d+(?:\\s*(?:\\+|,|and|&)\\s*\\d+)*)";
    // "ok 1+2" anywhere after "recommend", or the number right after it.
    const m = new RegExp("recommend[^\\n]*?\\bok\\s+" + NUMS, "i").exec(l) ||
      new RegExp("recommend\\w*\\s*(?:[:=\\-\u2013\\u2014]|is)?\\s*(?:options?\\s+|choice\\s+|#\\s*)?" + NUMS, "i").exec(l);
    if (!m) continue;
    const nums = m[1].split(/\s*(?:\+|,|and|&)\s*/).map(Number).filter((x) => x >= 1 && x <= n);
    if (nums.length) return nums;
  }
  return [];
}
// Choices a question offers: the numbered list in its context ("1. **Mint a key** ..."),
// 2 to 9 items counted from 1. rec holds the numbers a "recommend... ok 1+2" line names.
function todoOptions(ctx) {
  const lines = String(ctx || "").split("\n");
  let opts = [];
  let inCode = false;
  for (const l of lines) {
    if (/^\s*```/.test(l)) { inCode = !inCode; continue; }
    const m = !inCode && /^ {0,3}(\d{1,2})[.)]\s+(.+)$/.exec(l);
    if (!m) continue;
    const n = Number(m[1]);
    if (n === 1) opts = [];
    if (n !== opts.length + 1) continue;
    const b = /\*\*([^*]+)\*\*/.exec(m[2]);
    let label = (b ? b[1] : m[2]).replace(/[*_`~]/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").trim().replace(/[.:;,]+$/, "");
    if (label.length > 42) label = label.slice(0, 40).trimEnd() + "…";
    opts.push({ n, label });
  }
  if (opts.length < 2 || opts.length > 9) return { opts: [], rec: [] };
  const rec = todoRecommended(lines, opts.length);
  return { opts, rec };
}
// The choices of an item: its options and recommend fields when it has them
// (2 to 9), else whatever the context's Markdown list yields.
function todoItemOptions(t) {
  const o = t && t.options;
  if (Array.isArray(o) && o.length >= 2 && o.length <= 9) {
    const opts = o.map((x, i) => {
      let label = String((x && x.label) || "").replace(/[*_`~]/g, "").trim();
      if (label.length > 42) label = label.slice(0, 40).trimEnd() + "…";
      return { n: i + 1, label, detail: String((x && x.detail) || "") };
    });
    const rec = [...new Set((Array.isArray(t.recommend) ? t.recommend : []).map(Number))].filter((n) => n >= 1 && n <= opts.length).sort((a, b) => a - b);
    return { opts, rec };
  }
  return todoOptions(t && t.context);
}
// The numbers picked in an answer box that holds "ok 1+2".
function todoPicked(text) {
  const m = /^\s*ok\s+(\d+(?:\+\d+)*)\s*$/i.exec(text || "");
  return m ? m[1].split("+").map(Number) : [];
}
function todoOptsHtml(t) {
  const { opts, rec } = todoItemOptions(t);
  if (!opts.length) return "";
  const picked = todoPicked(todoDraft(t.id));
  return `<div class="td-opts" role="group" aria-label="Choices">` + opts.map((o) =>
    `<button type="button" class="td-opt${rec.includes(o.n) ? " rec" : ""}${picked.includes(o.n) ? " on" : ""}" data-n="${o.n}" aria-pressed="${picked.includes(o.n)}" title="${escAttr(o.detail ? `${o.detail} (sends “ok ${o.n}” now; Shift-click to combine choices first)` : `Sends “ok ${o.n}” now (Shift-click to combine choices first)`)}"><kbd>${o.n}</kbd><span>${esc(o.label)}</span></button>`).join("") +
    (rec.length ? `<button type="button" class="td-opt td-rec" data-fill="ok ${rec.join("+")}" title="Sends “ok ${rec.join("+")}” now"><span>Send the recommendation: ok ${rec.join("+")}</span></button>` : "") + `<span class="td-opts-hint">Click a choice to send it · Shift-click to combine, then Answer</span></div>`;
}
function todoSyncOpts(item, text) {
  const picked = todoPicked(text);
  item.querySelectorAll(".td-opt[data-n]").forEach((b) => {
    const on = picked.includes(Number(b.dataset.n));
    b.classList.toggle("on", on);
    b.setAttribute("aria-pressed", String(on));
  });
}
// True while a Shift+digit key adds a pick: the keys stay with the list, not the box.
let todoKeyPick = false;
// Puts text in an item's answer box as if typed, and leaves the cursor there.
function todoFill(item, text, focus = !todoKeyPick) {
  const box = item.querySelector(".td-text");
  if (!box) return;
  box.value = text;
  box.dispatchEvent(new Event("input", { bubbles: true }));
  if (focus) box.focus({ preventScroll: true });
  box.setSelectionRange(box.value.length, box.value.length);
}

// pfx keeps element ids apart: the same item is drawn in the phone card
// and on the desktop page.
function todoItemHtml(t, pfx) {
  const at = Date.parse(t.created_at);
  const age = Number.isNaN(at) ? "" : fmtAgo(at);
  const who = t.waiting ? t.waiting.name || t.waiting.id : "";
  const open = todos.openId === t.id;
  const ans = t.state !== "open";
  const draft = todoDraft(t.id).trim() !== "";
  const rid = `${pfx}-row-${t.id}`, pid = `${pfx}-panel-${t.id}`;
  const chips = `<span class="td-chip td-draft"${draft ? "" : " hidden"}>draft</span>` + todoSrcChip(t) +
    (ans ? `<span class="td-chip st-answered">answered</span>` : `<span class="td-chip st-open">open</span>`) +
    (todos.pend.busy(t.id) ? `<span class="td-chip st-sending">sending…</span>` : "");
  let panel = "";
  if (open) {
    const meta = [who && `waiting: ${who}`, age, t.id].filter(Boolean).join(" · ");
    const links = (t.links || []).map((l) => {
      const href = todoLinkHref(l.url);
      const label = l.label || l.url || `handoff ${l.handoff}`;
      return href
        ? (href.startsWith("/#/") ? `<a href="${escAttr(href)}">${esc(label)}</a>` : `<a href="${escAttr(href)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`)
        : `<span>${esc(l.handoff ? `handoff ${l.handoff}` : label)}</span>`;
    });
    const a = t.answer;
    const aAt = a ? Date.parse(a.at) : NaN;
    const answered = a
      ? `<div class="td-answer"><div class="td-answer-h">${esc(a.via === "relay" ? "Answer, relayed by " : "Your answer, ")}${a.via === "relay" ? esc(a.by) + " " : ""}${Number.isNaN(aAt) ? "" : esc(fmtAgo(aAt)) + " ago"}</div>
          <div class="td-answer-t">${renderInline(a.text)}</div>
          <div class="td-wait">${t.state === "answered" && a.via === "app" ? "Waiting for the coordinator to pass it on." : ""}</div></div>` : "";
    const nx = todos.nxt && todos.nxt !== t.id ? todos.items.find((x) => x.id === todos.nxt) : null;
    const agentId = t.waiting && t.waiting.id;
    const strip = `<div class="td-strip">
        ${who ? `<div><span class="td-sl">Waiting</span><span class="td-sv"><span class="td-adot"></span>${agentId && agentById(agentId) ? `<button type="button" class="td-agent" data-agent="${escAttr(agentId)}" title="Open ${escAttr(who)}">${esc(who)} ↗</button>` : esc(who)}</span></div>` : ""}
        ${age ? `<div><span class="td-sl">Asked</span><span class="td-sv">${esc(age)} ago</span></div>` : ""}
        <div><span class="td-sl">Item</span><span class="td-sv"><code>${esc(t.id)}</code></span></div>
        ${links.length ? `<div><span class="td-sl">Links</span><span class="td-sv td-dlinks">${links.join("")}</span></div>` : ""}</div>`;
    const dhead = `<div class="td-dh"><div class="td-dtop"><span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span>${todoSrcChip(t)}${ans ? `<span class="td-chip st-answered">answered</span>` : `<span class="td-chip st-open">open</span>`}</div>
        <h3 class="td-dtitle">${renderInline(t.question.replace(/\s*\n\s*/g, " "), false)}</h3>${strip}</div>`;
    const isRel = t.kind === "release" && !!t.release;
    const after = ans || isRel ? "" : `<div class="td-after">${who ? `Goes to ${esc(who)} through the coordinator. ` : ""}${nx ? `Next: ${esc(nx.question.replace(/\s*\n\s*/g, " ").slice(0, 70))}` : "That clears the queue."}</div>`;
    const kept = ans && draft ? todoKeptHtml(t.id, todoDraft(t.id), "Not sent: this was answered before you sent yours.") : "";
    const busy = "";
    const form = ans || isRel ? ""
      : `<textarea class="td-text" id="${pfx}-answer-${escAttr(t.id)}" rows="2" maxlength="4000" placeholder="Your answer" aria-label="Your answer">${esc(todoDraft(t.id))}</textarea>`;
    panel = `<div class="td-panel" id="${escAttr(pid)}" role="region" aria-labelledby="${escAttr(rid)}">
        ${dhead}
        <div class="td-scroll">
          ${meta ? `<div class="td-meta">${esc(meta)}</div>` : ""}
          ${isRel ? relHtml(t) : ""}
          ${t.context ? `<div class="md td-ctx">${renderMarkdown(t.context, "todo:" + t.id + ":" + t.updated_at)}</div>` : ""}
          ${links.length ? `<div class="td-links">${links.join("")}</div>` : ""}
          ${answered}${kept}
        </div>
        <div class="td-form">${ans || isRel ? "" : todoOptsHtml(t)}${form}
          ${ans || isRel ? "" : sttStatusHtml("todo:" + t.id)}
          <div class="td-acts">
            ${ans || isRel ? "" : sttMicHtml("todo:" + t.id)}
            ${ans || isRel ? "" : `<button type="button" class="td-answer-btn"${busy}>Answer</button>`}
            <button type="button" class="td-done"${busy}>Done</button>
            <button type="button" class="td-drop"${busy}>Drop</button>
          </div>${after}
        </div>
      </div>`;
  }
  return `<div class="td-item td-${esc(t.kind)}${ans ? " answered" : ""}${open ? " open" : ""}${todos.pend.busy(t.id) ? " sending" : ""}" data-id="${escAttr(t.id)}">
      <button type="button" class="td-row" id="${escAttr(rid)}" aria-expanded="${open}"${open ? ` aria-controls="${escAttr(pid)}"` : ""}>
        <span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span><span class="td-q">${renderInline(t.question.replace(/\s*\n\s*/g, " "), false)}</span>${!open && !ans && todos.nxt === t.id ? `<span class="td-nx">Next up</span>` : ""}
        <span class="td-rm"><span class="td-who">${esc(who)}</span><span class="td-age">${esc(age)}</span><span class="td-chips">${chips}</span></span>
        <span class="td-chev" aria-hidden="true"></span>
      </button>${panel}
    </div>`;
}

function todoListHtml(pfx) {
  todos.nxt = todoNextOpen(todos.openId, todoOrder().map((t) => t.id));
  const opens = todos.items.filter((t) => t.state === "open");
  const done = todos.items.filter((t) => t.state !== "open");
  let h = todoOrphans().map(([id, d]) => todoKeptHtml(id, d.text, `Not sent: “${d.q || id}” was closed before you answered.`)).join("");
  h += opens.map((t) => todoItemHtml(t, pfx)).join("");
  if (!opens.length) h += `<div class="td-none">${done.length ? "Nothing else is waiting on you." : "Nothing is waiting on you."}</div>`;
  if (done.length) {
    const drafts = done.some((t) => todoDraft(t.id).trim());
    h += `<button type="button" class="td-g" aria-expanded="${todos.ansOpen}"${todos.ansOpen ? ` aria-controls="${pfx}-ans"` : ""}>Answered, being passed on <span class="td-gn">${done.length}</span>${
      drafts ? `<span class="td-chip td-draft">draft</span>` : ""}<span class="td-chev" aria-hidden="true"></span></button>`;
    if (todos.ansOpen) h += `<div class="td-ans" id="${pfx}-ans">${done.map((t) => todoItemHtml(t, pfx)).join("")}</div>`;
  }
  return h;
}

function todoHasFocus() {
  const a = document.activeElement;
  return !!a && [$("todo"), $("pt-body")].some((el) => el && el.contains(a));
}
function todoRowIn(el, id) { return id ? el.querySelector(`.td-item[data-id="${CSS.escape(id)}"] > .td-row`) : null; }

// A refresh closed the open item, or someone else answered it: open the
// next one, as after the user's own answer. Its draft stays in
// todos.drafts, shown on the answered item or as a kept answer.
function todoFollowRefresh() {
  let moved = false;
  const id = todos.openId;
  if (id && todos.on === true && !todos.pend.busy(id)) {
    const t = todos.items.find((x) => x.id === id);
    if (!t || (todos.seen.get(id) === "open" && t.state !== "open")) {
      const kept = todoDraft(id).trim() !== "";
      todos.openId = todoNextOpen(id, todos.order);
      if (todos.openId) todos.lastId = todos.openId;
      todos.follow = todoHasFocus();
      moved = true;
      if (todos.follow || (isDesktop() && state.view === "todo")) {
        toast((t ? "That item was answered elsewhere." : "That item was closed elsewhere.") + (kept ? " Your unsent answer is kept." : ""));
      }
    }
  }
  todos.seen = new Map(todos.items.map((x) => [x.id, x.state]));
  todos.order = todoOrder().map((x) => x.id);
  return moved;
}

// Redraws a list and puts back what a redraw loses: the scroll positions and
// the keyboard focus (the same control of the same item, or the open row).
function todoPaint(el, html) {
  const a = document.activeElement;
  const had = !!a && a !== el && el.contains(a);
  let sel = null;
  if (had && a.classList[0]) {
    const h = a.closest("[data-kept], [data-id]");
    sel = (h ? (h.dataset.kept ? `[data-kept="${CSS.escape(h.dataset.kept)}"] ` : `[data-id="${CSS.escape(h.dataset.id)}"] `) : "") + "." + a.classList[0];
  }
  const top = el.scrollTop;
  const inner = el.querySelector(".td-item.open .td-scroll");
  const innerId = inner && inner.closest(".td-item").dataset.id, innerTop = inner ? inner.scrollTop : 0;
  el.innerHTML = html;
  if (typeof sttSync === "function") sttSync(); // the mic buttons just drawn show the current state
  el.scrollTop = top;
  const ni = el.querySelector(".td-item.open .td-scroll");
  if (ni && ni.closest(".td-item").dataset.id === innerId) ni.scrollTop = innerTop;
  // Follow the open item only from inside the list (or from nowhere): never take focus from elsewhere.
  const follow = todos.follow && (had || document.activeElement === document.body);
  if (!had && !follow) return;
  const target = follow ? todoRowIn(el, todos.openId) || el.querySelector(".td-g") : (sel && el.querySelector(sel)) || todoRowIn(el, todos.openId);
  if (target) {
    target.focus({ preventScroll: true });
    if (follow) target.scrollIntoView?.({ block: "nearest" });
  }
}

function renderTodos(force) {
  const main = $("todo"), page = $("pt-body");
  const pill = $("pager-todo");
  if (pill) { pill.hidden = !todos.open; pill.textContent = String(todos.open); }
  const moved = todoFollowRefresh();
  // A forced redraw, or one that moves to another item, goes ahead even while the user types.
  const again = !!force || moved || todos.follow;
  const show = todos.on === true && (todos.items.length > 0 || todoOrphans().length > 0);
  if (main) main.hidden = !show;
  if (!show && main) { main.innerHTML = ""; delete main.dataset.sig; }
  const folded = lsGet(TODO_FOLDED_KEY) === "1";
  const sig = JSON.stringify([folded, todos.openId, todos.ansOpen, todos.pend.busyKeys(), [...todos.drafts.keys()],
    todos.items.map((t) => [t.id, t.state, t.updated_at, t.question]), rel.rev]);
  // The age moves between renders; a typed answer must not be lost to one.
  const typing = (el) => el && el.contains(document.activeElement) && document.activeElement.classList.contains("td-text");
  if (show && main && (again || (main.dataset.sig !== sig && !typing(main)))) {
    main.dataset.sig = sig;
    todoPaint(main, `<div class="td-head"><button type="button" class="td-toggle" aria-expanded="${!folded}">
        <span class="proj-chev" aria-hidden="true"></span><span class="td-title">To do</span><span class="td-count">${esc(todoCountText())}</span></button></div>
      <div class="td-list"${folded ? " hidden" : ""}>${todoListHtml("tdc")}</div>`);
  }
  renderNav();
  renderStatus();
  // The desktop page: the same items without the fold, and an empty note.
  if (page) {
    $("pt-count").textContent = show ? todoCountText() : "";
    const keys = $("pt-keys");
    if (keys) keys.hidden = !show;
    const psig = show ? "p" + sig : "empty";
    if (again || (page.dataset.sig !== psig && !typing(page))) {
      page.dataset.sig = psig;
      todoPaint(page, show ? `<div class="td-list">${todoListHtml("tdp")}</div><div class="tds-none"><b>${todos.open ? `${todos.open} waiting on you` : "Nothing is waiting on you"}</b><span>Pick a question on the left, or press <kbd>j</kbd>.</span>${todos.nxt ? `<button type="button" class="td-start">Start with the first</button>` : ""}</div>` : emptyHtml("Nothing is waiting on you", "When a coordinator needs an answer, the question appears here."));
    }
  }
  todos.follow = false;
}

// The list the user sees: the To do page on desktop, the card on a phone.
function todoListEl() { return isDesktop() ? $("pt-body") : $("todo"); }

// Opens one item (closing the one that was open) and focuses its row, or
// its answer box with to = "box".
function todoExpand(id, to) {
  todos.openId = id;
  if (id) todos.lastId = id;
  renderTodos(true);
  if (id && todos.items.some((x) => x.id === id && x.kind === "release")) loadRelease(id, false);
  const el = todoListEl();
  const row = el && todoRowIn(el, id);
  if (!row) return;
  const box = to === "box" && row.parentElement.querySelector(".td-text");
  if (box) {
    box.focus({ preventScroll: true });
    box.setSelectionRange(box.value.length, box.value.length);
    box.scrollIntoView?.({ block: "nearest" });
    return;
  }
  row.focus({ preventScroll: true });
  row.scrollIntoView?.({ block: isDesktop() ? "nearest" : "start" });
}
function todoCollapse() {
  if (todos.openId) todos.lastId = todos.openId;
  todos.openId = null;
  renderTodos(true);
  const el = todoListEl();
  const row = el && todoRowIn(el, todos.lastId);
  if (row) row.focus({ preventScroll: true });
}

// todoApply makes an action true in the local list: done and drop take the
// item off it, an answer moves it to the answered group.
function todoApply(id, what, body) {
  const t = todos.items.find((x) => x.id === id);
  if (!t || t.state !== "open") return;
  if (what === "answer") {
    t.state = "answered";
    t.answer = { text: body.text, via: "app", by: "", at: new Date().toISOString() };
    t.updated_at = t.answer.at;
    todos.open = Math.max(0, todos.open - 1);
    todos.answered++;
  } else {
    todos.items = todos.items.filter((x) => x.id !== id);
    todos.open = Math.max(0, todos.open - 1);
  }
}

// A refresh brings the server's list, which has not heard of an action
// still on the wire (or just confirmed): lay those actions over it again.
function todoReapply() {
  todos.pend.each((e, id) => todoApply(id, e.what, e.body));
}

// todoAct: the item leaves the open list in this frame, the POST goes out
// behind it. On failure it comes back, open, with the draft in its box.
async function todoAct(id, what, body, opts = {}) {
  if (todos.pend.busy(id)) return; // already on its way
  const t = todos.items.find((x) => x.id === id);
  if (!t) return;
  if (opts.undoMs) flushChoiceUndo(); // one undo window at a time: an earlier one ends now
  const next = todoNextOpen(id, todoOrder().map((x) => x.id));
  const hadFocus = todoHasFocus();
  const prev = { item: JSON.parse(JSON.stringify(t)), draft: todos.drafts.get(id), open: todos.open, answered: todos.answered, at: todos.items.indexOf(t) };
  todos.pend.add(id, { what, body });
  todoApply(id, what, body);
  todos.drafts.delete(id);
  saveTodoDrafts();
  // Focus moves on to the next open item, as the list closes this one.
  if (todos.openId === id) {
    todos.openId = next;
    if (next) todos.lastId = next;
    todos.follow = hadFocus;
  }
  renderTodos(true);
  // Puts the item back as it was. keepText: the typed answer comes back in its box
  // (a failed send), not just the draft it had before (an undo).
  const restore = (keepText) => {
    todos.pend.drop(id);
    todos.items = todos.items.filter((x) => x.id !== id);
    todos.items.splice(Math.min(prev.at, todos.items.length), 0, prev.item);
    todos.open = prev.open; todos.answered = prev.answered;
    const keep = prev.draft || (keepText && what === "answer" && body && body.text ? { text: body.text, q: prev.item.question } : null);
    if (keep) { todos.drafts.set(id, keep); saveTodoDrafts(); }
    todos.openId = id; todos.lastId = id;
    todos.seen.set(id, "open");
    renderTodos(true);
  };
  let started = false;
  const send = async (keepalive) => {
    if (started) return;
    started = true;
    try {
      await api(`/api/todos/${encodeURIComponent(id)}/${what}`, { method: "POST", body: JSON.stringify(body || {}), keepalive: !!keepalive });
      todos.pend.settle(id);
      toast(what === "answer" ? "Answered. The coordinator will pass it on." : what === "done" ? "Marked done" : "Dropped");
    } catch (e) {
      // Put it back as it was, with the typed answer, so a second try is one tap.
      restore(true);
      toast((what === "answer" ? "Not sent: " : "Not done: ") + e.message, true);
      return;
    }
    todos.loadedAt = 0;
    await loadTodos();
  };
  if (opts.undoMs) {
    // The item is answered on screen now; the request goes out when the window ends,
    // on Undo it never does, and leaving the page inside the window sends it at once.
    const u = {
      id,
      timer: setTimeout(() => u.flush(), opts.undoMs),
      flush(keepalive) { clearTimeout(u.timer); if (todos.undo === u) todos.undo = null; return send(keepalive); },
      undo() { clearTimeout(u.timer); if (todos.undo === u) todos.undo = null; started = true; restore(false); },
    };
    todos.undo = u;
    toastAction(`Sent: ${body.text}`, "Undo", () => u.undo(), opts.undoMs);
    return;
  }
  await send(false);
}

// flushChoiceUndo ends the undo window now, sending what is waiting in it.
function flushChoiceUndo(keepalive) {
  const u = todos.undo;
  if (!u) return;
  const el = $("toast");
  if (el.classList.contains("act")) el.className = "";
  return u.flush(keepalive);
}
for (const ev of ["pagehide", "beforeunload"]) window.addEventListener(ev, () => flushChoiceUndo(true));
document.addEventListener("visibilitychange", () => { if (document.hidden) flushChoiceUndo(true); });

// A choice (a button, the recommendation, a digit key) sends that answer at once,
// with an undo window. Shift adds to a pending combination instead of sending (the
// Answer button then sends it). Typed free text is never replaced or sent by a
// choice: the choice goes in front of it, and Answer sends the lot.
let CHOICE_UNDO_MS = 5000; // let: the uitest shortens it
function todoChoose(item, text, shift) {
  const box = item.querySelector(".td-text");
  if (!box) return;
  const id = item.dataset.id;
  const typed = box.value.trim();
  const pending = todoPicked(box.value);
  if (typed && !pending.length) {
    todoFill(item, text + " " + typed);
    toast(`Added “${text}” to the front of your answer. Press Answer to send it.`);
    return;
  }
  const picks = todoPicked(text);
  let combo = pending;
  if (picks.length > 1 && !pending.length) combo = picks; // the recommendation: ok 1+3
  else for (const n of picks) if (!combo.includes(n)) combo = combo.concat(n);
  combo = combo.slice().sort((a, b) => a - b);
  if (shift) { todoFill(item, "ok " + combo.join("+")); return; }
  todoAct(id, "answer", { text: "ok " + combo.join("+") }, { undoMs: CHOICE_UNDO_MS });
}

function todoAnswer(id, box) {
  const text = box ? box.value.trim() : "";
  if (!text) { toast("Write your answer first", true); return; }
  todoAct(id, "answer", { text });
}

function todoCopy(ta) {
  const fallback = () => { ta.focus(); ta.select(); toast("Selected: copy it with your keyboard"); };
  if (!navigator.clipboard || !navigator.clipboard.writeText) { fallback(); return; }
  navigator.clipboard.writeText(ta.value).then(() => toast("Copied"), fallback);
}

// ---- releases ----
// A "release" to-do carries a version and a full commit id. Everything the
// page shows about it that matters (the commit, CI, the changes) comes from
// the server's own check of the hub's checkout and GitHub, never from the
// item's text. Approving asks for a confirm step that names the version and
// the commit; the server runs the release script and each host's Upgrade runs
// `comebye upgrade` there. Agents cannot do either: only this page can.
const rel = { map: new Map(), rev: 0 };

function relOf(id) {
  let r = rel.map.get(id);
  if (!r) { r = { data: null, err: "", loadedAt: 0, inflight: false, confirm: null, busy: false }; rel.map.set(id, r); }
  return r;
}

async function loadRelease(id, refresh) {
  const r = relOf(id);
  if (r.inflight) return;
  r.inflight = true;
  try {
    r.data = await api(`/api/release/${encodeURIComponent(id)}${refresh ? "?refresh=1" : ""}`, { timeout: 120000 });
    r.err = "";
  } catch (e) {
    r.err = e.message || "failed";
  } finally {
    r.inflight = false;
    r.loadedAt = Date.now();
  }
  rel.rev++;
  renderTodos(true);
}

// While a release item is open: refresh it as the tag, the build or an
// upgrade moves on, and now and then otherwise.
function relActive(t, d) {
  if (!t.release) return false;
  if (t.release.state === "tagging") return true;
  if (t.release.state !== "tagged") return false;
  if (!d) return true;
  if (d.build && d.build.state !== "success" && d.build.state !== "failure") return true;
  return (d.hosts || []).some((h) => h.record && h.record.state === "running");
}

function relTick() {
  if (document.hidden || todos.on !== true) return;
  const t = todos.items.find((x) => x.id === todos.openId && x.kind === "release");
  if (!t) return;
  const r = relOf(t.id);
  const age = Date.now() - r.loadedAt;
  if (!r.data && !r.err ? age > 1500 : age > (relActive(t, r.data) ? 4000 : 30000)) {
    if (relActive(t, r.data)) loadTodos();
    loadRelease(t.id, false);
  }
}

function relShort(sha) { return (sha || "").slice(0, 7); }

function relChip(kind, text, href) {
  const inner = esc(text);
  return `<span class="rel-chip rel-${kind}">${href && todoLinkHref(href) ? `<a href="${escAttr(href)}" target="_blank" rel="noopener noreferrer">${inner}</a>` : inner}</span>`;
}

function relCiChip(ck) {
  if (!ck.ci_status) return relChip("none", "no CI run yet");
  if (ck.ci_status !== "completed") return relChip("wait", "CI " + ck.ci_status.replace("_", " "), ck.ci_url);
  return ck.ci_conclusion === "success" ? relChip("ok", "CI passed", ck.ci_url) : relChip("bad", "CI " + (ck.ci_conclusion || "failed"), ck.ci_url);
}

function relBuildChip(d) {
  if (!d || !d.build) return relChip("none", d && d.build_error ? "build unknown" : "…");
  const b = d.build;
  switch (b.state) {
    case "success": return relChip("ok", "build passed", b.url);
    case "failure": return relChip("bad", "build failed", b.url);
    case "none": return relChip("wait", "build not started yet");
    default: return relChip("wait", "building", b.url);
  }
}

function relRecordText(h) {
  const rc = h.record;
  if (h.unknown) return "not reachable";
  if (!h.enabled) return "upgrading from the app is off on this host";
  if (!rc) return "";
  const when = Date.parse(rc.finished || rc.started);
  const ago = Number.isNaN(when) ? "" : " " + fmtAgo(when) + " ago";
  const verb = rc.rollback ? "roll back" : "upgrade";
  switch (rc.state) {
    case "running": return `${verb} to ${rc.version} running…`;
    case "ok": return `${rc.rollback ? "rolled back" : "upgraded"} to ${rc.to || rc.version} by ${rc.by || "?"}${ago}`;
    case "rolled_back": return `${verb} failed its checks and was rolled back${ago}: ${rc.error || ""}`;
    case "dry_run": return `dry run ${rc.version}: nothing installed${ago}`;
    default: return `${verb} failed${ago}: ${rc.error || ""}`;
  }
}

function relHtml(t) {
  const rl = t.release, r = relOf(t.id), d = r.data;
  const ck = d && d.check;
  const STATE = { pending: "waiting for you", tagging: "tagging…", tagged: "tagged", failed: "failed" };
  let h = `<div class="rel" data-rel-id="${escAttr(t.id)}">`;
  h += `<div class="rel-h">Release <b>${esc(rl.version)}</b><span class="rel-state rel-s-${esc(rl.state)}">${esc(STATE[rl.state] || rl.state)}</span>
    <button type="button" class="rel-refresh" data-rel="refresh" aria-label="Check again"${r.inflight ? " disabled" : ""}>↻</button></div>`;
  if (d && d.simulated) h += `<div class="rel-block" role="alert">SIMULATED: this server runs no git, no GitHub and no upgrade. Nothing below is real.</div>`;
  if (d && d.dry_run) h += `<div class="rel-note rel-dry">Dry run: the checks run for real, but nothing is tagged or installed.</div>`;
  h += `<dl class="rel-facts"><dt>Commit</dt><dd><code class="rel-sha">${esc(rl.commit)}</code></dd>`;
  if (ck && ck.subject) h += `<dt>Subject</dt><dd>${esc(ck.subject)}</dd>`;
  if (ck) h += `<dt>Last tag</dt><dd>${esc(ck.previous || "none yet")}</dd><dt>CI</dt><dd>${relCiChip(ck)}</dd>`;
  if (rl.state === "tagged") h += `<dt>Build</dt><dd>${relBuildChip(d)}${rl.run_url ? ` ${relChip("none", "workflow run", rl.run_url)}` : ""}</dd>`;
  h += `</dl>`;
  if (!d && !r.err) h += `<div class="rel-note">Checking the commit with GitHub…</div>`;
  if (r.err) h += `<div class="rel-block" role="alert">Could not load the release page: ${esc(r.err)}</div>`;
  if (d && d.check_error) h += `<div class="rel-block" role="alert">Could not check the commit: ${esc(d.check_error)}</div>`;
  if (ck && ck.problem) h += `<div class="rel-block" role="alert">Blocked: ${esc(ck.problem)}</div>`;
  if (ck && ck.changes && ck.changes.length) {
    h += `<details class="rel-changes"><summary>Changes since ${esc(ck.previous || "the start")} (${ck.changes.length})</summary><ul>${ck.changes.map((c) => `<li>${esc(c)}</li>`).join("")}</ul></details>`;
  }
  if (rl.state === "failed" && rl.error) h += `<div class="rel-block" role="alert">The last try failed: ${esc(rl.error)}</div>`;
  if (rl.dry_run) h += `<details class="rel-changes"><summary>Last dry run</summary><pre>${esc(rl.dry_run)}</pre></details>`;

  // Approve.
  if (rl.state === "pending" || rl.state === "failed") {
    if (d && !d.can_tag) {
      h += `<div class="rel-note">This host is not set up to tag releases (it needs <code>-release-repo</code>). Run <code>tools/release/tag ${esc(rl.version)} ${esc(rl.commit)}</code> yourself.</div>`;
    } else if (r.confirm && r.confirm.k === "tag") {
      h += `<div class="rel-confirm" role="group" aria-label="Confirm the release">
        <div>Tag <b>${esc(rl.version)}</b> at <code>${esc(rl.commit)}</code> and push the tag${d && d.dry_run ? " (dry run: nothing will be pushed)" : ""}? The Release workflow then builds and publishes it.</div>
        <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-tag"${r.busy ? " disabled" : ""}>Confirm: release ${esc(rl.version)}</button>
        <button type="button" data-rel="cancel"${r.busy ? " disabled" : ""}>Cancel</button></div></div>`;
    } else {
      const ok = !!(ck && ck.ok);
      h += `<div class="td-acts"><button type="button" class="rel-approve" data-rel="approve"${ok && !r.busy ? "" : " disabled"}>Approve release</button></div>`;
    }
  }
  if (rl.state === "tagging") h += `<div class="rel-note">The tag is being pushed…</div>`;

  // Hosts.
  if (rl.state === "tagged") {
    const hosts = (d && d.hosts) || [];
    const green = !!(d && d.build && d.build.state === "success");
    h += `<div class="rel-sub">Hosts</div>`;
    if (!green) h += `<div class="rel-note">Hosts can be upgraded once the release build has passed.</div>`;
    h += `<div class="rel-hosts">`;
    for (const x of hosts) {
      const running = x.record && x.record.state === "running";
      const there = x.version === rl.version;
      let btn = "";
      if (x.enabled && green && !running && x.status === "ok") {
        if (!there) btn = `<button type="button" class="rel-up" data-rel="up" data-host="${escAttr(x.name)}"${r.busy ? " disabled" : ""}>Upgrade</button>`;
        else if (x.record && x.record.state === "ok" && !x.record.rollback) btn = `<button type="button" class="rel-rb" data-rel="rollback" data-host="${escAttr(x.name)}"${r.busy ? " disabled" : ""}>Roll back</button>`;
      }
      h += `<div class="rel-host" data-host="${escAttr(x.name)}"><span class="rel-hn">${esc(x.name)}</span>
        <span class="rel-hv">${esc(x.version || "?")}${there ? " ✓" : ""}</span><span class="rel-hs">${esc(relRecordText(x))}</span>${btn ? `<span class="rel-ha">${btn}</span>` : ""}</div>`;
    }
    h += `</div>`;
    const c = r.confirm;
    if (c && c.k === "up") {
      const x = hosts.find((y) => y.name === c.host);
      h += `<div class="rel-confirm" role="group" aria-label="Confirm the upgrade"><div>${c.rollback
        ? `Roll back <b>${esc(c.host)}</b> from ${esc(rl.version)} to the binary it had before? comebye restarts on that host.`
        : `Upgrade <b>${esc(c.host)}</b> from ${esc((x && x.version) || "?")} to <b>${esc(rl.version)}</b>? It checks the download, restarts comebye there, and rolls back by itself if the new one fails its checks.`}</div>
        <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-up"${r.busy ? " disabled" : ""}>Confirm: ${c.rollback ? "roll back" : "upgrade"} ${esc(c.host)}</button>
        <button type="button" data-rel="cancel"${r.busy ? " disabled" : ""}>Cancel</button></div></div>`;
    }
  }

  // Who did what.
  const au = (d && d.audit) || [];
  if (au.length) {
    h += `<details class="rel-changes rel-audit"><summary>Record (${au.length})</summary><ul>${au.slice(-12).reverse().map((e) => {
      const at = Date.parse(e.at);
      return `<li>${Number.isNaN(at) ? "" : esc(fmtAgo(at)) + " ago · "}${esc(e.by)} · ${esc(e.action)}${e.host ? " · " + esc(e.host) : ""}${e.ok ? "" : " (not done)"} · ${esc(e.detail || "")}</li>`;
    }).join("")}</ul></details>`;
  }
  return h + `</div>`;
}

async function relPost(id, path, body) {
  const r = relOf(id);
  r.busy = true;
  rel.rev++;
  renderTodos(true);
  try {
    await api(path, { method: "POST", headers: { "X-Comebye-Csrf": (r.data && r.data.csrf) || "" }, body: JSON.stringify(body), timeout: 60000 });
    r.confirm = null;
    return true;
  } catch (e) {
    if (e.code === "csrf") { await loadRelease(id, false); toast("The page was out of date. Check the details and try again.", true); }
    else toast(e.message, true);
    return false;
  } finally {
    r.busy = false;
  }
}

async function relClick(t, act, host) {
  const r = relOf(t.id);
  switch (act) {
    case "refresh": loadRelease(t.id, true); return;
    case "approve": r.confirm = { k: "tag" }; break;
    case "up": r.confirm = { k: "up", host, rollback: false }; break;
    case "rollback": r.confirm = { k: "up", host, rollback: true }; break;
    case "cancel": r.confirm = null; break;
    case "confirm-tag":
      if (await relPost(t.id, `/api/release/${encodeURIComponent(t.id)}/approve`, { version: t.release.version, commit: t.release.commit })) toast("Approved. Tagging…");
      todos.loadedAt = 0;
      await loadTodos();
      await loadRelease(t.id, true);
      return;
    case "confirm-up": {
      const c = r.confirm;
      if (!c) return;
      if (await relPost(t.id, `/api/hosts/${encodeURIComponent(c.host)}/release/upgrade`, { todo: t.id, version: t.release.version, rollback: !!c.rollback })) toast(`${c.rollback ? "Rolling back" : "Upgrading"} ${c.host}…`);
      await loadRelease(t.id, true);
      return;
    }
    default: return;
  }
  rel.rev++;
  renderTodos(true);
}

function onTodoClick(ev) {
  if (ev.target.closest(".td-toggle")) {
    lsSet(TODO_FOLDED_KEY, lsGet(TODO_FOLDED_KEY) === "1" ? "0" : "1");
    renderTodos(true);
    return;
  }
  if (ev.target.closest(".td-g")) {
    todos.ansOpen = !todos.ansOpen;
    const cur = todos.items.find((t) => t.id === todos.openId);
    if (!todos.ansOpen && cur && cur.state !== "open") todos.openId = null;
    renderTodos(true);
    return;
  }
  const kept = ev.target.closest("[data-kept]");
  if (kept) {
    if (ev.target.closest(".td-copy")) todoCopy(kept.querySelector(".td-kept-t"));
    else if (ev.target.closest(".td-discard")) { todos.drafts.delete(kept.dataset.kept); saveTodoDrafts(); renderTodos(true); }
    return;
  }
  if (ev.target.closest(".td-start")) {
    const first = todoOrder().find((t) => t.state === "open");
    if (first) todoExpand(first.id);
    return;
  }
  const item = ev.target.closest(".td-item");
  if (!item) return;
  const id = item.dataset.id;
  const ag = ev.target.closest(".td-agent");
  if (ag) { openDetail(ag.dataset.agent); return; }
  const opt = ev.target.closest(".td-opt");
  if (opt) {
    if (!item.querySelector(".td-text")) return;
    if (opt.dataset.fill) { todoChoose(item, opt.dataset.fill, ev.shiftKey); return; }
    const box = item.querySelector(".td-text"), n = Number(opt.dataset.n);
    const picked = todoPicked(box.value);
    // A button already in the pending combination, shift-clicked, leaves it.
    if (ev.shiftKey && picked.includes(n)) {
      const rest = picked.filter((x) => x !== n);
      todoFill(item, rest.length ? "ok " + rest.join("+") : "");
      return;
    }
    todoChoose(item, "ok " + n, ev.shiftKey);
    return;
  }
  if (ev.target.closest(".td-row")) {
    if (todos.openId === id) todoCollapse(); else todoExpand(id);
    return;
  }
  const t = todos.items.find((x) => x.id === id);
  if (!t) return;
  const rb = ev.target.closest("[data-rel]");
  if (rb && t.release) { relClick(t, rb.dataset.rel, rb.dataset.host); return; }
  if (ev.target.closest(".td-answer-btn")) {
    todoAnswer(id, item.querySelector(".td-text"));
  } else if (ev.target.closest(".td-done")) {
    todoAct(id, "done");
  } else if (ev.target.closest(".td-drop")) {
    if (!confirm(`Drop this item?\n\n${t.question}\n\nIt leaves the list and nobody is told.`)) return;
    todoAct(id, "drop");
  }
}

// A typed answer is kept per item (and across reloads), and marks the row.
function onTodoInput(ev) {
  const box = ev.target.closest && ev.target.closest(".td-text");
  const item = box && box.closest(".td-item");
  if (!item) return;
  const id = item.dataset.id;
  const t = todos.items.find((x) => x.id === id);
  if (box.value.trim()) todos.drafts.set(id, { text: box.value, q: t ? t.question : "" });
  else todos.drafts.delete(id);
  saveTodoDrafts();
  todoSyncOpts(item, box.value);
  const chip = item.querySelector(".td-row .td-draft");
  if (chip) chip.hidden = !box.value.trim();
}

// Keys. Ctrl/Cmd+Enter sends from any answer box. On the desktop To do page:
// ↑/↓ or j/k open the next or previous item, Enter goes to its answer box,
// Esc leaves the box and then closes the item.
function onTodoKey(ev) {
  if (ev.defaultPrevented || ev.isComposing) return;
  const el = ev.target;
  const box = el.closest && el.closest(".td-text");
  if (box && ev.key === "Enter" && (ev.ctrlKey || ev.metaKey) && !ev.altKey && !ev.shiftKey) {
    ev.preventDefault();
    todoAnswer(box.closest(".td-item").dataset.id, box);
    return;
  }
  if (!isDesktop() || state.view !== "todo" || anotherLayerOpen()) return;
  if (ev.altKey || ev.ctrlKey || ev.metaKey || (ev.shiftKey && !/^Digit[1-9]$/.test(ev.code))) return;
  const page = $("pt-body");
  if (!page || !(el === document.body || page.contains(el))) return;
  const k = ev.key;
  if (box) {
    if (k === "Escape") {
      ev.preventDefault();
      const row = todoRowIn(page, box.closest(".td-item").dataset.id);
      if (row) row.focus();
    }
    return;
  }
  if (el.closest("input, textarea, select, a, [contenteditable]")) return;
  if (k === "ArrowDown" || k === "j" || k === "ArrowUp" || k === "k") {
    ev.preventDefault();
    const rows = [...page.querySelectorAll(".td-row")];
    const ids = rows.map((r) => r.closest(".td-item").dataset.id);
    if (!rows.length) return;
    const down = k === "ArrowDown" || k === "j";
    let i = rows.indexOf(el);
    if (i < 0) i = ids.indexOf(todos.openId || todos.lastId);
    i = i < 0 ? (down ? 0 : rows.length - 1) : i + (down ? 1 : -1);
    if (i >= 0 && i < rows.length) todoExpand(ids[i]);
  } else if ((/^[1-9]$/.test(k) || (ev.shiftKey && /^Digit[1-9]$/.test(ev.code))) && todos.openId) {
    // 1-9 sends that choice; Shift+1-9 adds it to a pending combination.
    const d = ev.shiftKey ? ev.code.slice(5) : k;
    const b = page.querySelector(`.td-item[data-id="${CSS.escape(todos.openId)}"] .td-opt[data-n="${d}"]`);
    if (b) { ev.preventDefault(); todoKeyPick = true; try { b.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, shiftKey: ev.shiftKey })); } finally { todoKeyPick = false; } }
  } else if (k === "Enter") {
    if (el.closest(".td-acts, .td-g, .td-kept")) return; // a button does what it says
    const row = el.closest(".td-row");
    const id = row ? row.closest(".td-item").dataset.id : todos.openId || todos.lastId;
    if (!id) return;
    ev.preventDefault();
    todoExpand(id, todos.openId === id ? "box" : undefined);
  } else if (k === "Escape" && todos.openId) {
    ev.preventDefault();
    todoCollapse();
  }
}

// briefPreview is the first line of the briefing with the markdown marks
// taken off (the coordinator's status line), for the collapsed card.
function briefPreview(md) {
  for (const raw of md.split("\n")) {
    const t = raw.replace(/^\s*(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)*/, "").replace(/[*_`~]/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").trim();
    if (t && !/^[-=_*]{3,}$/.test(t)) return t.length > 200 ? t.slice(0, 200) + "…" : t;
  }
  return "";
}

function renderBrief() {
  const el = $("briefing");
  if (!el) return;
  renderFleetMenu();
  renderPager();
  renderBriefs();
  let b = briefing.data;
  if (b && (!b.markdown.trim() || lsGet(BRIEF_DISMISSED_KEY) === b.at)) b = null;
  const miss = briefMiss();
  if (!b && !miss) {
    el.hidden = true;
    el.innerHTML = "";
    delete el.dataset.sig;
    return;
  }
  const folded = !!b && !isDesktop() && lsGet(BRIEF_EXPANDED_KEY) !== "1"; // the desktop has the Briefing page instead of this card
  const preview = b && folded ? briefPreview(b.markdown) : "";
  const sched = briefing.schedule;
  const every = sched && sched.enabled ? ` · every ${sched.every}${sched.at ? " at " + sched.at : ""}` : "";
  const metaText = () => {
    if (!b) return `none to show${every}`;
    const at = Date.parse(b.at);
    // The age first: it is what you look for, and the rest may be cut.
    const chk = briefChecked();
    return `${Number.isNaN(at) ? "" : `${fmtAgo(at)} ago · `}from ${b.by || "a coordinator"}${chk ? " · " + chk : ""}${every}`;
  };
  const sig = [b ? b.at : "", folded, preview, every, briefChecked(), miss ? `${miss.id}|${miss.count}|${miss.reason}` : ""].join("|");
  el.hidden = false;
  el.classList.toggle("stale", !!miss);
  if (el.dataset.sig === sig) {
    // Same briefing, same state: only the age moves.
    const meta = el.querySelector(".bf-meta");
    if (meta) meta.textContent = metaText();
    return;
  }
  el.dataset.sig = sig;
  const head = b
    ? `<button type="button" class="bf-toggle" aria-expanded="${!folded}" aria-controls="bf-body">
        <span class="proj-chev" aria-hidden="true"></span>
        <span class="bf-title">Briefing</span>
        <span class="bf-meta">${esc(metaText())}</span>
      </button>
      <button type="button" class="bf-dismiss" aria-label="Dismiss this briefing">&#10005;</button>`
    : `<div class="bf-toggle static"><span class="bf-title">Briefing</span><span class="bf-meta">${esc(metaText())}</span></div>`;
  el.innerHTML = `<div class="bf-head">${head}</div>
    ${preview ? `<div class="bf-preview">${esc(preview)}</div>` : ""}
    ${miss ? missNote(miss, !!b) : ""}
    ${b ? `<div class="bf-body md" id="bf-body"${folded ? " hidden" : ""}>${renderMarkdown(b.markdown, "brief:" + b.at)}</div>` : ""}`;
}

async function onBriefClick(ev) {
  const b = briefing.data;
  if (ev.target.closest(".bf-start")) {
    openLaunch();
    setLaunchMode("coordinator");
  } else if (ev.target.closest(".bf-sched")) {
    editBriefSchedule();
  } else if (ev.target.closest(".bf-ack")) {
    const m = briefMiss();
    if (!m) return;
    briefing.acking = m.id; // gone now; back if the request fails
    renderBrief();
    try {
      const d = await api("/api/brief/schedule/dismiss", { method: "POST", body: JSON.stringify({ id: m.id }) });
      if (d && d.schedule) briefing.schedule = d.schedule;
    } catch (e) {
      toast("Could not dismiss: " + e.message, true);
    }
    briefing.acking = null;
    renderBrief();
  } else if (b && ev.target.closest(".bf-dismiss")) {
    lsSet(BRIEF_DISMISSED_KEY, b.at);
    renderBrief();
    toast("Briefing dismissed until the next one");
  } else if (b && ev.target.closest(".bf-toggle")) {
    lsSet(BRIEF_EXPANDED_KEY, lsGet(BRIEF_EXPANDED_KEY) === "1" ? "0" : "1");
    renderBrief();
  }
}

// editBriefSchedule takes you to the Briefings card on Status (on the phone,
// the top of the briefings screen), where the schedule is edited in place.
function editBriefSchedule() {
  toggleDetailMenu(false);
  toggleFleetMenu(false);
  if (isDesktop()) openPage("status"); else openBriefs(true);
  setTimeout(() => {
    renderStatus();
    const c = [...document.querySelectorAll(".bcard")].find((x) => x.getClientRects().length);
    if (!c) return;
    c.scrollIntoView?.({ block: "nearest" });
    c.querySelector("[data-bk]")?.focus();
  }, 80);
}

/* ---------- the briefings screen (swipe left from the agent list) ---------- */
//
// A page to the right of the list: the latest briefing in full, the
// schedule and any missed-briefing note, and the earlier briefings the hub
// kept (GET /api/brief/history, the last 48). Reached by swiping left on the
// list (the panel follows the finger, then snaps on distance or velocity),
// by the pill at the bottom of the list, or by the ⋯ menu. Swiping right,
// the back button, or the browser's back returns. The URL is #/briefings, so
// back and reload behave. The gesture stays out of the way of vertical
// scrolling, of the screen edges (the OS back gestures) and of anything that
// scrolls sideways itself.

const briefs = { open: false, pushed: false, history: [], histAt: 0, histLoaded: false, inflight: false, openAts: new Set(), sig: "" };
const BRIEFS_HASH = "#/briefings";
const BRIEF_SEEN_KEY = "comebye-brief-seen"; // the `at` of the latest briefing seen on the screen
const SWIPE = { lock: 10, edge: 20, fraction: 0.33, velocity: 0.45 }; // px, px, of the width, px/ms

const reducedMotion = () => !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

function briefsAvailable() { return ledger.on !== false; }

async function loadBriefHistory() {
  if (briefs.inflight || ledger.on !== true) return;
  briefs.inflight = true;
  try {
    const d = await api("/api/brief/history");
    briefs.history = d && Array.isArray(d.briefs) ? d.briefs.filter((b) => b && typeof b.markdown === "string") : [];
    briefs.histLoaded = true;
  } catch {
    // Keep what we had.
  } finally {
    briefs.inflight = false;
    briefs.histAt = Date.now();
  }
  renderBriefs();
}

// renderPager shows the pill that opens the screen, with a dot while the
// latest briefing has not been seen there.
function renderPager() {
  const p = $("pager");
  if (!p) return;
  p.hidden = !briefsAvailable();
  syncDesktopBriefs();
  const b = briefing.data;
  $("pager-new").hidden = !(b && b.markdown.trim() && lsGet(BRIEF_SEEN_KEY) !== b.at);
  renderNav();
}

function briefItem(h) {
  const at = Date.parse(h.at);
  const open = briefs.openAts.has(h.at);
  return `<details class="br-item" data-at="${esc(h.at)}"${open ? " open" : ""}>
      <summary><span class="br-when">${esc(Number.isNaN(at) ? "" : fmtAgo(at) + " ago · " + fmtWhen(h.at))}</span><span class="br-pre">${esc(briefPreview(h.markdown))}</span></summary>
      <div class="br-md md">${open ? renderMarkdown(h.markdown, "brief:" + h.at) : ""}</div>
    </details>`;
}

function renderBriefs() {
  const el = $("br-body");
  if (!el || !briefs.open) return;
  const b = briefing.data && briefing.data.markdown.trim() ? briefing.data : null;
  const miss = briefMiss();
  const earlier = briefs.history.filter((h) => !b || h.at !== b.at);
  const sig = [b ? b.at : "", miss ? `${miss.id}|${miss.count}|${miss.reason}` : "", briefChecked(), earlier.map((h) => h.at).join(","), Math.floor(Date.now() / 60000)].join("|");
  if (sig === briefs.sig) return;
  briefs.sig = sig;
  const top = el.scrollTop;
  const at = b ? Date.parse(b.at) : NaN;
  const key = el.contains(document.activeElement) ? document.activeElement.dataset.bk : ""; // the Briefings card's control in use
  el.innerHTML = `<div id="br-status"></div><section class="br-card${miss ? " stale" : ""}" aria-label="Latest briefing">
      <div class="br-head"><span class="bf-title">Latest</span><span class="bf-meta">${b ? esc((Number.isNaN(at) ? "" : fmtAgo(at) + " ago · ") + "from " + (b.by || "a coordinator") + (briefChecked() ? " · " + briefChecked() : "")) : "none yet"}</span></div>
      ${miss ? missNote(miss, !!b) : ""}
      ${b ? `<div class="br-md md">${renderMarkdown(b.markdown, "brief:" + b.at)}</div>`
          : `<p class="br-empty">No briefing has been published. A coordinator publishes one with <code>comebye brief set</code>.</p>`}
    </section>
    <h3 class="br-h">Earlier</h3>
    ${earlier.length ? earlier.map(briefItem).join("")
      : `<p class="br-note">${briefs.histLoaded ? "Earlier briefings appear here as new ones are published. The hub keeps the last 48." : "Loading…"}</p>`}`;
  el.scrollTop = top;
  renderStatus();
  if (key) el.querySelector(`[data-bk="${key}"]`)?.focus({ preventScroll: true });
}

// setBriefsOpen slides the screen in or out (animate: from wherever the
// drag left it) and keeps the focus and the inert state right.
function setBriefsOpen(open, animate) {
  const p = $("briefs");
  const was = briefs.open;
  briefs.open = open;
  p.classList.toggle("anim", !!animate && !reducedMotion());
  p.classList.remove("dragging");
  $("pager").classList.remove("dragging");
  p.style.transform = "";
  p.classList.toggle("open", open);
  p.inert = !open;
  if (open && !was) {
    const b = briefing.data;
    if (b) { lsSet(BRIEF_SEEN_KEY, b.at); renderPager(); }
    briefs.sig = "";
    renderBriefs();
    loadBrief();
    loadBriefHistory();
    // Focus the screen itself (no ring), so a keyboard user tabs on from its top.
    if (!isDesktop()) p.focus?.({ preventScroll: true });
  } else if (!open && was) {
    // Hand the focus back to where the user came from.
    if (p.contains(document.activeElement)) $("pager").focus?.({ preventScroll: true });
  }
}

// On desktop the screen is the Briefing page in the main pane: open while it
// is the page shown (and the hub keeps briefings), no sliding, no gesture;
// its history entry is the page's own hash.
function syncDesktopBriefs() {
  if (isDesktop() && state.view === "briefing" && !briefsAvailable()) { setView(null); return; } // the hub keeps none
  const desk = isDesktop(), want = desk && state.view === "briefing";
  if (want === briefs.open) { briefs.desk = want; return; }
  if (!want && !briefs.desk) return; // phone: the swipe state is not ours to touch
  briefs.pushed = false;
  briefs.desk = want;
  setBriefsOpen(want, false);
}

function openBriefs(animate = true) {
  if (isDesktop()) return;
  if (briefs.open || state.selected != null || !briefsAvailable()) return;
  setBriefsOpen(true, animate);
  if (location.hash !== BRIEFS_HASH) {
    // A history entry, so the phone's back gesture returns to the list.
    history.pushState(null, "", BRIEFS_HASH);
    briefs.pushed = true;
  }
}

function closeBriefs() {
  if (!briefs.open || isDesktop()) return;
  setBriefsOpen(false, true);
  if (briefs.pushed) { briefs.pushed = false; history.back(); }
  else if (location.hash === BRIEFS_HASH) history.replaceState(null, "", location.pathname + location.search);
}

// syncBriefs follows the URL (back, forward, a cold start on #/briefings).
function syncBriefs() {
  if (isDesktop()) return;
  const want = location.hash === BRIEFS_HASH && state.selected == null && briefsAvailable();
  if (want === briefs.open) return;
  if (!want) briefs.pushed = false;
  setBriefsOpen(want, !!briefs.inited);
}

// ---- the gesture ----

// scrollsSideways says whether something between the touch and the page
// would scroll in that direction (dir < 0: the finger moves left, so the
// content moves right), so the swipe leaves it alone.
function scrollsSideways(el, root, dir) {
  for (; el && el !== root && el.nodeType === 1; el = el.parentElement) {
    if (el.scrollWidth <= el.clientWidth + 1) continue;
    const ox = getComputedStyle(el).overflowX;
    if (ox !== "auto" && ox !== "scroll") continue;
    const max = el.scrollWidth - el.clientWidth;
    if (dir < 0 ? el.scrollLeft < max - 1 : el.scrollLeft > 1) return true;
  }
  return false;
}

function anotherLayerOpen() {
  if (openPopName()) return true;
  return !!document.querySelector('[role="dialog"]:not([hidden])');
}

function installBriefsGesture() {
  let g = null; // the touch in progress
  const W = () => window.innerWidth;

  const finish = (commit) => {
    const opening = g.opening;
    g = null;
    if (opening) { if (commit) openBriefs(true); else setBriefsOpen(false, true); }
    else if (commit) closeBriefs();
    else setBriefsOpen(true, true);
  };

  document.addEventListener("pointerdown", (ev) => {
    if (g || ev.pointerType !== "touch" || !ev.isPrimary || isDesktop()) return;
    if (state.selected != null || !briefsAvailable()) return;
    const opening = !briefs.open;
    if (opening && (anotherLayerOpen() || $("list").style.display === "none")) return;
    if (!opening && anotherLayerOpen()) return;
    // The screen edges belong to the OS (iOS back swipe on the left, Android's on both).
    if (ev.clientX < SWIPE.edge || ev.clientX > W() - SWIPE.edge) return;
    if (ev.target.closest?.("input, textarea, select, [contenteditable]")) return;
    g = { id: ev.pointerId, x0: ev.clientX, y0: ev.clientY, opening, state: "pending", target: ev.target, samples: [{ t: ev.timeStamp, x: ev.clientX }], dx: 0 };
  }, { passive: true });

  document.addEventListener("pointermove", (ev) => {
    if (!g || ev.pointerId !== g.id) return;
    const dx = ev.clientX - g.x0, dy = ev.clientY - g.y0;
    if (g.state === "pending") {
      if (Math.abs(dx) < SWIPE.lock && Math.abs(dy) < SWIPE.lock) return;
      const root = g.opening ? $("list") : $("briefs");
      const right = g.opening ? dx < 0 : dx > 0; // the direction that moves the screen
      // Mostly sideways, the right way, and not something that scrolls itself.
      if (Math.abs(dx) < Math.abs(dy) * 1.6 || !right || scrollsSideways(g.target, root, Math.sign(dx))) { g = null; return; }
      g.state = "drag";
      g.x0 = ev.clientX; // start from here: no jump when the lock engages
      $("briefs").classList.add("dragging");
      $("briefs").classList.remove("anim");
      $("pager").classList.add("dragging");
      try { document.body.setPointerCapture?.(ev.pointerId); } catch {}
      window.getSelection?.()?.removeAllRanges?.();
    }
    if (g.state !== "drag") return;
    const w = W();
    // Opening: the screen is at w + dx (dx <= 0). Closing: at dx (dx >= 0).
    const off = g.opening ? Math.min(w, Math.max(0, w + (ev.clientX - g.x0))) : Math.min(w, Math.max(0, ev.clientX - g.x0));
    g.dx = g.opening ? off - w : off;
    $("briefs").style.transform = `translateX(${off}px)`;
    g.samples.push({ t: ev.timeStamp, x: ev.clientX });
    while (g.samples.length > 2 && ev.timeStamp - g.samples[0].t > 100) g.samples.shift();
    ev.preventDefault?.();
  }, { passive: false });

  const end = (ev, cancelled) => {
    if (!g || ev.pointerId !== g.id) return;
    if (g.state !== "drag") { g = null; return; }
    const s = g.samples, a = s[0], z = s[s.length - 1];
    const v = z.t > a.t ? (z.x - a.x) / (z.t - a.t) : 0; // px/ms, negative = leftwards
    const moved = Math.abs(g.dx);
    const toward = g.opening ? v < -SWIPE.velocity : v > SWIPE.velocity;
    const away = g.opening ? v > SWIPE.velocity : v < -SWIPE.velocity;
    const commit = !cancelled && !away && (moved > W() * SWIPE.fraction || (toward && moved > 24));
    // A drag must not also be a tap on whatever is under the finger.
    const swallow = (e) => { e.stopPropagation(); e.preventDefault(); };
    document.addEventListener("click", swallow, { capture: true, once: true });
    setTimeout(() => document.removeEventListener("click", swallow, { capture: true }), 60);
    finish(commit);
  };
  document.addEventListener("pointerup", (ev) => end(ev, false));
  document.addEventListener("pointercancel", (ev) => end(ev, true));
}

function initBriefs() {
  briefs.inited = true;
  $("pager").addEventListener("click", () => openBriefs(true));
  $("br-back").addEventListener("click", closeBriefs);
  $("briefs").addEventListener("click", onBriefClick);
  $("briefs").addEventListener("keydown", (ev) => { if (ev.key === "Escape" && !anotherLayerOpen()) { ev.stopPropagation(); closeBriefs(); } });
  // An earlier briefing renders when it is first opened.
  $("br-body").addEventListener("toggle", (ev) => {
    const d = ev.target;
    if (!d.matches?.("details.br-item")) return;
    const at = d.dataset.at;
    if (d.open) {
      briefs.openAts.add(at);
      const h = briefs.history.find((x) => x.at === at);
      const md = d.querySelector(".br-md");
      if (h && md && !md.firstChild) md.innerHTML = renderMarkdown(h.markdown, "brief:" + at);
    } else briefs.openAts.delete(at);
  }, true);
  installBriefsGesture();
  syncBriefs();
  syncDesktopBriefs();
}

/* ---------- marking a session ready to remove (detail view menu) ---------- */

function renderDetailMenu() {
  // The "⋯" is always there: it is the only way to mark, switch the model
  // or change the role. Items that need the ledger wait for it (disabled)
  // or, when this hub keeps none, give way to a note.
  const can = !!state.selected && switchable(agentById(state.selected));
  const lon = ledger.on === true && !!state.selected;
  const off = ledger.on === false;
  $("d-model").hidden = !can;
  for (const id of ["d-mark", "d-role"]) { $(id).hidden = off; $(id).disabled = !lon; }
  const st = $("d-mark-state");
  if (!lon) {
    $("d-sched").hidden = true;
    renderMarkBar(null);
    st.hidden = !off;
    st.textContent = off ? "Marking and roles are not available on this hub" : "";
    if (!off) { $("d-mark").textContent = "Mark ready to remove… (loading)"; $("d-role").textContent = "Make coordinator (loading)"; }
    return;
  }
  const m = ledger.marked.get(state.selected);
  $("d-mark").textContent = m ? "Unmark (keep this session)" : "Mark ready to remove…";
  renderMarkBar(m);
  st.hidden = !m;
  if (m) st.textContent = "Marked ready to remove" + (m.reason ? ": " + m.reason : "");
  const coord = isCoordinator(agentById(state.selected));
  $("d-role").textContent = coord ? "Not coordinator" : "Make coordinator";
  $("d-sched").hidden = !coord;
}

// toggleRole makes the viewed agent a coordinator (pinned in the list,
// its removal asks twice, agents cannot mark it), or not.
async function toggleRole() {
  const id = state.selected;
  toggleDetailMenu(false);
  if (!id || ledgerOpt.busy("role:" + id)) return;
  const a = agentById(id);
  const coord = isCoordinator(a);
  const was = ledger.roles.get(id);
  const want = coord ? "" : "coordinator";
  const apply = () => { if (want) ledger.roles.set(id, { role: want, name: a ? a.name || "" : "", by: "app", at: new Date().toISOString() }); else ledger.roles.delete(id); };
  ledgerOpt.add("role:" + id, { apply });
  apply();
  redrawMarks();
  try {
    const d = await api("/api/roles", { method: "POST", body: JSON.stringify({ agent_id: id, role: want }) });
    ledger.roles = new Map(d.roles && typeof d.roles === "object" ? Object.entries(d.roles) : []);
    ledgerOpt.settle("role:" + id);
    ledgerOpt.each((e) => e.apply());
    toast(coord ? "No longer a coordinator" : "Now a coordinator");
  } catch (e) {
    ledgerOpt.drop("role:" + id);
    if (was) ledger.roles.set(id, was); else ledger.roles.delete(id);
    toast("Could not change the role: " + e.message, true);
  }
  redrawMarks();
}

function toggleDetailMenu(show) {
  const menu = $("d-menu");
  const open = show ?? menu.hidden;
  if (open === !menu.hidden) return;
  menu.hidden = !open;
  if (open) { renderDetailMenu(); if (ledger.on !== true && ledger.on !== false) loadLedger(); popOpened("detail"); popItems(menu)[0]?.focus(); }
  else popClosed("detail");
  $("d-more").setAttribute("aria-expanded", String(open));
}

// Marking: the detail view's bar and menu, and each row's menu, all end in
// markAgent (ask for an optional reason in a sheet) or unmarkAgent. Marking
// only proposes: the session is asked, and the user confirms in the review.

function renderMarkBar(m) {
  const bar = $("d-markbar");
  const show = ledger.on === true && !!state.selected && !!m;
  bar.hidden = !show;
  if (!show) { bar.innerHTML = ""; return; }
  // Marking lives in the "⋯" menu; the bar only appears once marked.
  bar.innerHTML = `<span class="lg-chip rtr">ready to remove</span><span class="mb-why">${esc(m.reason || "no reason given")}</span><button type="button" class="btn" data-mb="unmark">Unmark</button>`;
}

function toggleMark() {
  const id = state.selected;
  toggleDetailMenu(false);
  if (!id) return;
  if (ledger.marked.has(id)) unmarkAgent(id);
  else openMark(id);
}

async function unmarkAgent(id) {
  const key = "mark:" + id;
  if (ledgerOpt.busy(key)) return;
  const was = ledger.marked.get(id);
  const apply = () => ledger.marked.delete(id);
  ledgerOpt.add(key, { apply });
  apply();
  redrawMarks();
  try {
    await api("/api/marks/clear", { method: "POST", body: JSON.stringify({ agent_id: id }) });
    ledgerOpt.settle(key);
    toast("Unmarked");
  } catch (e) {
    ledgerOpt.drop(key);
    if (was) ledger.marked.set(id, was);
    redrawMarks();
    toast("Could not unmark: " + e.message, true);
    return;
  }
  ledger.loadedAt = 0;
  await loadLedger();
}

const mk = { open: false, id: "", busy: false };

function openMark(id) {
  if (mk.open) return;
  const a = agentById(id);
  Object.assign(mk, { open: true, id, busy: false });
  history.pushState({ mark: true }, "", location.href);
  $("mk-name").textContent = a ? (a.name || a.pane_id || id) : id;
  $("mk-reason").value = "";
  $("mk-status").textContent = "";
  $("mk-go").disabled = false;
  $("mk").hidden = false;
  document.body.classList.add("sheet-open");
  $("mk-reason").focus();
}

function hideMark() {
  mk.open = false;
  $("mk").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}

function closeMark() {
  if (mk.busy) return;
  if (history.state && history.state.mark) history.back();
  else hideMark();
}

function syncMark() {
  if (mk.open && !(history.state && history.state.mark)) hideMark();
}

async function submitMark() {
  if (mk.busy) return;
  const id = mk.id;
  const reason = $("mk-reason").value.trim();
  const key = "mark:" + id;
  // The sheet closes and the agent shows as marked now; the request follows.
  const apply = () => ledger.marked.set(id, { agent_id: id, reason, by: "app", at: new Date().toISOString(), pending: true });
  ledgerOpt.add(key, { apply });
  apply();
  closeMark();
  redrawMarks();
  toast("Marked ready to remove");
  let d;
  try {
    d = await api("/api/marks", { method: "POST", body: JSON.stringify({ agent_id: id, reason }) });
    ledgerOpt.settle(key);
  } catch (e) {
    ledgerOpt.drop(key);
    ledger.marked.delete(id);
    redrawMarks();
    toast("Could not mark: " + e.message, true);
    // Reopen the sheet with the reason, so a second try is one tap.
    setTimeout(() => {
      if (mk.open) return;
      openMark(id);
      $("mk-reason").value = reason;
      $("mk-status").textContent = e.message;
    }, 200);
    return;
  }
  if (d && d.mark && d.mark.ask && d.mark.ask.state === "asking") toast("Marked ready to remove; asking the session whether it is safe");
  ledger.loadedAt = 0;
  await loadLedger();
}

// The row menu: one popover, outside the list so a re-render keeps it.
function openRowMenu(btn) {
  const id = btn.dataset.menu;
  const menu = $("row-menu");
  if (!menu.hidden && menu.dataset.id === id) { closeRowMenu(); return; }
  menu.dataset.id = id;
  $("row-mark").textContent = ledger.marked.has(id) ? "Unmark (keep this session)" : "Mark ready to remove…";
  menu.hidden = false;
  popOpened("row");
  const r = btn.getBoundingClientRect();
  const w = menu.offsetWidth;
  menu.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.right - w)) + "px";
  const below = r.bottom + 4;
  menu.style.top = (below + menu.offsetHeight > innerHeight ? Math.max(8, r.top - menu.offsetHeight - 4) : below) + "px";
  $("row-mark").focus();
}

function closeRowMenu() { const m = $("row-menu"); if (m.hidden) return; m.hidden = true; popClosed("row"); }

function rowMenuMark() {
  const id = $("row-menu").dataset.id;
  closeRowMenu();
  if (!id) return;
  if (ledger.marked.has(id)) unmarkAgent(id);
  else openMark(id);
}

/* ---------- the header's fleet menu ("⋯" at the top right of the list) ---------- */
//
// Fleet-wide actions, always one tap away: the sweep, the removals review,
// the briefing (show / expand, show again after a dismissal, its schedule),
// the notifications panel and a refresh. It reuses the agent view's menu
// styles; the items that need the ledger wait for it ("(loading)") or, on a
// hub that keeps none, give way to a note.

function briefingUiState() {
  const b = briefing.data;
  if (!b || !b.markdown.trim()) return "none";
  if (lsGet(BRIEF_DISMISSED_KEY) === b.at) return "dismissed";
  if (isDesktop()) return "panel";
  return lsGet(BRIEF_EXPANDED_KEY) === "1" ? "expanded" : "folded";
}

function renderFleetMenu() {
  const lon = ledger.on === true;
  const off = ledger.on === false;
  const loading = !lon && !off;
  const sfx = loading ? " (loading)" : "";
  const n = lon ? ledger.marked.size : 0;
  const note = $("f-note");
  note.hidden = !off;
  note.textContent = off ? "Sweeping, removals and briefings are not available on this hub" : "";
  for (const id of ["f-sweep", "f-review", "f-brief", "f-briefs"]) { $(id).hidden = off; $(id).disabled = !lon; }
  $("f-sweep").textContent = "Sweep for finished agents…" + sfx;
  $("f-review").textContent = (lon ? `Review removals (${n})` : "Review removals") + sfx;
  $("f-review").disabled = !lon || !n;
  const st = briefingUiState();
  $("f-brief").textContent = ({ none: "Briefing (none yet)", dismissed: "Show the dismissed briefing", panel: "Briefing (see the Briefing page)", folded: "Expand the briefing", expanded: "Collapse the briefing" })[st] + (loading ? " (loading)" : "");
  $("f-brief").disabled = !lon || st === "none" || st === "panel";
  // Nothing to expand or show: the item would only be a dead row.
  $("f-brief").hidden = off || (lon && (st === "none" || st === "panel"));
  $("f-notify-val").textContent = ({ on: "On", off: "Off" })[pushState()] || "";
  applyTheme();
}

/* ---------- popovers: one set of rules for every ⋯ menu ---------- */
//
// The fleet menu, the agent menu, a row's menu, the theme picker and the
// notifications are popovers, and on a phone (<= 600px) bottom sheets. Each
// opens and closes through its own function; these rules are shared:
//   - one is open at a time (opening one closes the rest);
//   - Escape closes it and puts the focus back on the button that opened it;
//   - a click outside it (on a sheet, its backdrop), or its ×, closes it;
//   - ↑ ↓ Home End move between its items;
//   - a sheet also closes on a swipe down from its handle or title.

const POPS = {
  fleet: { id: "f-menu", close: () => toggleFleetMenu(false), trigger: () => $("btn-fleet") },
  detail: { id: "d-menu", close: () => toggleDetailMenu(false), trigger: () => $("d-more") },
  row: { id: "row-menu", close: () => closeRowMenu(), trigger: () => [...document.querySelectorAll(".row-more")].find((b) => b.dataset.menu === $("row-menu").dataset.id) },
  theme: { id: "theme-panel", close: () => toggleThemePanel(false), trigger: () => $("btn-fleet") },
  notify: { id: "notify-panel", close: () => toggleNotifyPanel(false), trigger: () => $("btn-notify") },
};
const sheetMQ = window.matchMedia("(max-width: 600px)");
const isSheet = () => sheetMQ.matches;
let clickNow = null; // the click being dispatched: a popover it opens must not be closed by that same click
let popOpenedBy = null;
document.addEventListener("click", (ev) => { clickNow = ev; }, true);

const openPopName = () => Object.keys(POPS).find((n) => !$(POPS[n].id).hidden) || "";
// popItems are the things the arrow keys move between (visible, enabled; the × is not one).
const popItems = (el) => [...el.querySelectorAll("button:not(.pop-x), select")].filter((b) => !b.disabled && !b.hidden && b.getClientRects().length);
function syncSheet() { $("sheet-bk").hidden = !(isSheet() && Object.values(POPS).some((p) => !$(p.id).hidden)); }
function popOpened(name) {
  popOpenedBy = clickNow;
  for (const n of Object.keys(POPS)) if (n !== name) POPS[n].close();
  syncSheet();
}
function popClosed(name) {
  if (name === "theme") { CT.apply(); themeHint(""); } // a preview ends with the picker
  syncSheet();
}
function closePop(name, focus) {
  const t = POPS[name].trigger();
  POPS[name].close();
  if (focus && t && t.isConnected) t.focus();
}
// placePop puts a popover under its button (right edges aligned); a sheet needs no place.
function placePop(name, anchor) {
  const el = $(POPS[name].id);
  el.style.left = el.style.top = el.style.maxHeight = "";
  if (isSheet() || !anchor) return;
  const r = anchor.getBoundingClientRect(), w = el.offsetWidth, top = r.bottom + 4;
  el.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.right - w)) + "px";
  el.style.top = top + "px";
  el.style.maxHeight = Math.max(160, innerHeight - top - 8) + "px";
}

function initPopovers() {
  document.addEventListener("keydown", (ev) => {
    const name = openPopName();
    if (!name) return;
    const el = $(POPS[name].id);
    if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); closePop(name, true); return; }
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(ev.key) || !el.contains(document.activeElement)) return;
    if (document.activeElement.tagName === "SELECT") return;
    const items = popItems(el);
    if (!items.length) return;
    ev.preventDefault();
    const i = items.indexOf(document.activeElement);
    const next = ev.key === "Home" ? 0 : ev.key === "End" ? items.length - 1
      : ev.key === "ArrowDown" ? (i + 1) % items.length : (i < 0 ? items.length - 1 : (i - 1 + items.length) % items.length);
    items[next].focus();
  }, true);
  document.addEventListener("click", (ev) => {
    const name = openPopName();
    if (!name || ev === popOpenedBy) return;
    const t = ev.target;
    if (!t.isConnected) return; // the click re-rendered its own target
    if ($(POPS[name].id).contains(t)) return;
    const trig = POPS[name].trigger();
    if (trig && trig.contains(t)) return;
    if (name === "row" && t.closest(".row-more")) return;
    POPS[name].close();
  });
  for (const p of Object.values(POPS)) {
    $(p.id).addEventListener("click", (ev) => {
      if (!ev.target.closest(".pop-x")) return;
      ev.stopPropagation();
      closePop(Object.keys(POPS).find((n) => POPS[n] === p), true);
    });
  }
  sheetMQ.addEventListener?.("change", syncSheet);

  // Swipe down on a sheet's handle or title: it follows the finger, then
  // closes on distance or speed, or springs back.
  let g = null;
  const reset = (pop) => { pop.style.transform = ""; pop.style.transition = ""; pop.style.animation = ""; };
  document.addEventListener("pointerdown", (ev) => {
    if (!isSheet() || ev.button > 0) return;
    const h = ev.target.closest(".pop .grab, .pop .pop-head");
    if (!h || ev.target.closest("button, select")) return;
    const pop = h.closest(".pop");
    g = { pop, y0: ev.clientY, t0: ev.timeStamp, dy: 0, id: ev.pointerId };
    h.setPointerCapture?.(ev.pointerId);
    pop.style.transition = "none";
    pop.style.animation = "none";
  });
  document.addEventListener("pointermove", (ev) => {
    if (!g || ev.pointerId !== g.id) return;
    g.dy = Math.max(0, ev.clientY - g.y0);
    g.pop.style.transform = "translateY(" + g.dy + "px)";
  });
  const end = (ev) => {
    if (!g || ev.pointerId !== g.id) return;
    const { pop, dy, t0 } = g;
    g = null;
    const fast = dy > 24 && dy / Math.max(1, ev.timeStamp - t0) > 0.5;
    const ms = reducedMotion() ? 0 : 180;
    pop.style.transition = ms ? "transform 0.18s ease-out" : "none";
    if (dy > 70 || fast) {
      pop.style.transform = "translateY(105%)";
      const name = Object.keys(POPS).find((n) => $(POPS[n].id) === pop);
      setTimeout(() => { if (name) closePop(name, true); reset(pop); }, ms);
    } else {
      pop.style.transform = "";
      setTimeout(() => reset(pop), ms);
    }
  };
  document.addEventListener("pointerup", end);
  document.addEventListener("pointercancel", end);
}

/* ---------- theme: System or one of the themes in themes.js ---------- */
//
// themes.js (loaded in <head>) owns the data, the saved choice and the
// pre-paint apply; this is the picker, a popover under the ⋯, and the ☀/☾
// button in the header. Hovering or arrowing over a theme previews it
// (CT.preview); Enter or a click keeps it, Escape (or leaving) reverts.
// "System" follows the OS light/dark, each with its own chosen theme, set in
// a second step.

const CT = window.ComebyeThemes;
const tp = { step: 1 };

function themeLabel() {
  const c = CT.choice();
  return c === "system" ? "System (" + CT.byId[CT.resolve()].name + ")" : CT.byId[c].name;
}

function swatch(id) {
  return '<span class="tp-sw" data-theme="' + id + '" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></span>';
}

const SUN_SVG = '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><circle cx="12" cy="12" r="4.2" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M12 2.5v2.4M12 19.1v2.4M2.5 12h2.4M19.1 12h2.4M5.3 5.3l1.7 1.7M17 17l1.7 1.7M5.3 18.7L7 17M17 7l1.7-1.7" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
const MOON_SVG = '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"/></svg>';

function themeHint(id) {
  const h = $("tp-hint");
  if (!h) return;
  h.textContent = id ? "Previewing " + CT.byId[CT.resolve(id)].name + " · Enter keeps · Esc reverts"
    : (window.matchMedia?.("(hover: none)").matches ? "Tap a theme to use it." : "Hover or arrow to preview · Enter keeps · Esc reverts");
}

function renderThemePanel() {
  const choice = CT.choice();
  const row = (id, name, sub, on) =>
    '<button type="button" class="tp-opt" role="menuitemradio" aria-checked="' + on + '" data-theme-id="' + id + '">' +
    swatch(id) + '<span class="tp-name">' + esc(name) + '</span><span class="tp-mode">' + esc(sub) + '</span></button>';
  $("tp-list").innerHTML =
    '<div class="tp-row">' + row("system", "System", "follows device", choice === "system") +
    '<button type="button" class="tp-more" id="tp-more" aria-label="Choose the light and dark themes System uses">light/dark ›</button></div>' +
    CT.THEMES.map((t) => row(t.id, t.name, t.mode, choice === t.id)).join("");
  const sys = CT.sysPair();
  const sel = (mode) => '<select data-sys="' + mode + '" aria-label="Theme when the device is ' + mode + '">' +
    CT.THEMES.filter((t) => t.mode === mode).map((t) => '<option value="' + t.id + '"' + (sys[mode] === t.id ? " selected" : "") + ">" + esc(t.name) + "</option>").join("") + "</select>";
  $("tp-sys").innerHTML = '<label>When the device is light' + sel("light") + "</label><label>When the device is dark" + sel("dark") + "</label>";
  const two = tp.step === 2;
  $("tp-list").hidden = two;
  $("tp-hint").hidden = two;
  $("tp-sys").hidden = !two;
  $("tp-back").hidden = !two;
  $("tp-title").textContent = two ? "System" : "Theme";
  if (!two) themeHint("");
}

function applyTheme() {
  CT.apply();
  const id = CT.resolve();
  const sw = $("f-theme-sw");
  if (sw) sw.innerHTML = swatch(id);
  const v = $("f-theme-val");
  if (v) v.textContent = themeLabel();
  const q = $("btn-theme-q");
  if (q) {
    const t = CT.byId[id], cp = CT.counterpart(id), other = t.mode === "dark" ? "light" : "dark";
    q.innerHTML = t.mode === "dark" ? SUN_SVG : MOON_SVG;
    const label = cp ? "Switch to " + CT.byId[cp].name : t.name + " has no " + other + " version";
    q.setAttribute("aria-label", label);
    q.title = label;
    q.setAttribute("aria-disabled", String(!cp));
  }
}

// quickTheme flips between the light and dark of the chosen theme's family.
function quickTheme() {
  const id = CT.resolve(), t = CT.byId[id], cp = CT.counterpart(id);
  if (!cp) { toast(t.name + " has no " + (t.mode === "dark" ? "light" : "dark") + " version"); return; }
  CT.set(cp);
  applyTheme();
  toast("Theme: " + CT.byId[cp].name);
}

function toggleThemePanel(show) {
  const p = $("theme-panel");
  const open = show ?? p.hidden;
  if (open === !p.hidden) return;
  p.hidden = !open;
  if (open) {
    tp.step = 1;
    renderThemePanel();
    placePop("theme", $("btn-fleet"));
    popOpened("theme");
    (p.querySelector('[aria-checked="true"]') || popItems(p)[0])?.focus();
  } else {
    popClosed("theme");
  }
}

function initThemePanel() {
  const list = $("tp-list");
  const previewable = (ev) => ev.target.closest && ev.target.closest("[data-theme-id]");
  list.addEventListener("click", (ev) => {
    if (ev.target.closest("#tp-more")) { tp.step = 2; renderThemePanel(); $("tp-sys").querySelector("select")?.focus(); return; }
    const b = previewable(ev);
    if (!b) return;
    CT.set(b.dataset.themeId);
    applyTheme();
    toast("Theme: " + themeLabel());
    closePop("theme", true);
  });
  $("tp-back").addEventListener("click", () => { tp.step = 1; renderThemePanel(); $("tp-more")?.focus(); });
  list.addEventListener("pointerover", (ev) => {
    const b = previewable(ev);
    if (b && ev.pointerType === "mouse") { CT.preview(CT.resolve(b.dataset.themeId)); themeHint(b.dataset.themeId); }
  });
  list.addEventListener("pointerleave", (ev) => { if (ev.pointerType === "mouse") { CT.apply(); themeHint(""); } });
  list.addEventListener("focusin", (ev) => {
    const b = previewable(ev);
    if (b) { CT.preview(CT.resolve(b.dataset.themeId)); themeHint(b.dataset.themeId); }
  });
  list.addEventListener("focusout", (ev) => {
    if (!ev.relatedTarget || !list.contains(ev.relatedTarget)) { CT.apply(); themeHint(""); }
  });
  $("tp-sys").addEventListener("change", (ev) => {
    const s = ev.target.closest("select[data-sys]");
    if (!s) return;
    CT.setSystem(s.dataset.sys, s.value);
    applyTheme();
    renderThemePanel();
    $("tp-sys").querySelector('select[data-sys="' + s.dataset.sys + '"]')?.focus();
  });
  $("btn-theme-q").addEventListener("click", quickTheme);
  // Another tab or the OS changed the theme: keep the label and swatches true.
  const sync = () => { applyTheme(); if (!$("theme-panel").hidden) renderThemePanel(); };
  window.addEventListener("storage", sync);
  window.matchMedia?.("(prefers-color-scheme: light)").addEventListener?.("change", sync);
}

function toggleFleetMenu(show, focus) {
  const menu = $("f-menu");
  const open = show ?? menu.hidden;
  if (open === !menu.hidden) return;
  menu.hidden = !open;
  if (open) {
    renderFleetMenu();
    if (ledger.on !== true && ledger.on !== false) loadLedger();
    popOpened("fleet");
    if (focus !== false) (popItems(menu)[0] || menu).focus?.();
  } else {
    popClosed("fleet");
  }
  $("btn-fleet").setAttribute("aria-expanded", String(open));
}

function fleetAction(fn) {
  return () => { toggleFleetMenu(false); fn(); };
}

function fleetBrief() {
  const st = briefingUiState();
  if (st === "dismissed") lsSet(BRIEF_DISMISSED_KEY, "");
  else if (st === "folded") lsSet(BRIEF_EXPANDED_KEY, "1");
  else if (st === "expanded") lsSet(BRIEF_EXPANDED_KEY, "0");
  else return;
  renderBrief();
  $("briefing").scrollIntoView?.({ block: "nearest" });
}

function fleetRefresh() {
  ledger.loadedAt = 0;
  briefing.loadedAt = 0;
  refreshAgents().catch(() => setConn(false));
  loadLedger();
  toast("Refreshing");
}

/* ---------- sweep for finished agents ---------- */
//
// Asks the hub for a dry run, shows what it would mark (clean, merged,
// nothing unpushed) and what needs a look, and marks the ready ones only on
// the button. Removal stays the review's.

const sweep = { open: false, busy: false, seq: 0, data: null, marked: false };

function openSweep() {
  if (sweep.open) return;
  Object.assign(sweep, { open: true, busy: false, data: null, marked: false });
  history.pushState({ sweep: true }, "", location.href);
  $("sweep").hidden = false;
  document.body.classList.add("sheet-open");
  $("sweep-close").focus();
  runSweep(true);
}

function hideSweep() {
  sweep.open = false;
  sweep.seq++;
  $("sweep").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}

function closeSweep() {
  if (sweep.busy) return;
  if (history.state && history.state.sweep) history.back();
  else hideSweep();
}

function syncSweep() {
  if (sweep.open && !(history.state && history.state.sweep)) hideSweep();
}

async function runSweep(dry) {
  const seq = ++sweep.seq;
  sweep.busy = true;
  $("sweep").classList.add("busy");
  $("sweep-go").disabled = true;
  $("sweep-status").textContent = dry ? "Checking each finished worker on its host…" : "Marking…";
  if (dry) $("sweep-list").innerHTML = "";
  try {
    const d = await api("/api/sweep", { method: "POST", body: JSON.stringify({ dry_run: dry }) });
    if (seq !== sweep.seq) return;
    sweep.data = d;
    sweep.marked = !dry;
    $("sweep-status").textContent = "";
  } catch (e) {
    if (seq !== sweep.seq) return;
    $("sweep-status").textContent = "";
    $("sweep-list").innerHTML = `<div class="ln-empty">${esc(e.message)}</div>`;
    sweep.data = null;
  }
  sweep.busy = false;
  $("sweep").classList.remove("busy");
  renderSweep();
  if (!dry) { ledger.loadedAt = 0; loadLedger(); }
}

function renderSweep() {
  const d = sweep.data;
  const go = $("sweep-go");
  if (!d) { go.disabled = true; return; }
  const item = (it) => `<div class="rv-item" data-sweep="${esc(it.id)}">
      <div class="rv-top"><span class="rv-name">${esc(it.name || it.id)}</span>${it.host ? `<span class="status">${esc(it.host)}</span>` : ""}</div>
      ${it.branch ? `<div class="rv-sub">${esc(it.branch)}</div>` : ""}
      <div class="rv-reason">${esc(it.reason)}</div></div>`;
  const ready = d.ready || [], look = d.needs_look || [], skipped = d.skipped || [];
  let h = "";
  if (!ready.length && !look.length) h += `<div class="ln-empty">No finished workers to sweep.</div>`;
  if (ready.length) h += `<div class="sw-h">${sweep.marked ? "Marked ready to remove" : "Ready to remove"} (${ready.length})</div>` + ready.map(item).join("");
  if (look.length) h += `<div class="sw-h">Needs a look, not marked (${look.length})</div>` + look.map(item).join("");
  if (skipped.length) h += `<div class="ln-note">${skipped.length} skipped (already marked, coordinators, primary checkouts).</div>`;
  $("sweep-list").innerHTML = h;
  if (sweep.marked) {
    go.textContent = ready.length ? "Review removals" : "Done";
    go.disabled = false;
  } else {
    go.textContent = ready.length ? `Mark ${ready.length} ready to remove` : "Nothing to mark";
    go.disabled = !ready.length;
  }
}

function sweepGo() {
  if (sweep.busy) return;
  if (sweep.marked) {
    const wantReview = ((sweep.data && sweep.data.ready) || []).length > 0;
    closeSweep();
    if (wantReview) setTimeout(openReview, 80);
    return;
  }
  runSweep(false);
}

/* ---------- review: remove the sessions marked ready to remove ---------- */
//
// The banner opens a sheet listing each marked session with safety checks
// run on its host just now: uncommitted changes, commits its base lacks, an
// agent that went back to work. Removing closes the session (its workspace,
// or only its pane when others share it) and removes the worktree only when
// it is clean and merged; the branch is kept. A session whose worktree
// has work a removal would keep (uncommitted changes, unmerged commits, an
// unfinished rebase) can instead be discarded: the sheet lists what would
// be lost, and only an explicit confirmation removes the worktree anyway
// and deletes its branch. A coordinator may propose that (mark --discard);
// only this sheet does it. Like the launch sheet, it is a history entry.

const review = { open: false, items: [], results: new Map(), busy: false, seq: 0, askTimer: 0 };

function openReview() {
  if (review.open) return;
  review.open = true;
  review.results = new Map();
  history.pushState({ review: true }, "", location.href);
  $("review").hidden = false;
  document.body.classList.add("sheet-open");
  $("rv-close").focus();
  loadReview();
}

function hideReview() {
  review.open = false;
  review.seq++;
  $("review").hidden = true;
  document.body.classList.remove("sheet-open");
  if (!state.selected) $("rtr-banner").focus();
  maybeReload();
}

function closeReview() {
  if (review.busy) return;
  if (history.state && history.state.review) history.back();
  else hideReview();
}

function syncReview() {
  if (review.open && !(history.state && history.state.review)) hideReview();
}

function setReviewBusy(on, text) {
  review.busy = on;
  $("review").classList.toggle("busy", on);
  $("rv-status").textContent = text || "";
  renderReviewFoot();
}

// loadReview checks the marked sessions; quiet (a refresh while a
// session is being asked whether it is safe to remove) keeps the list as
// it is meanwhile.
async function loadReview(quiet) {
  if (quiet && (review.busy || !review.open)) return;
  const seq = ++review.seq;
  if (!quiet) setReviewBusy(true, "Checking each session on its host…");
  if (!review.items.length) $("rv-list").innerHTML = "";
  let d;
  try {
    d = await api("/api/removals");
  } catch (e) {
    if (seq !== review.seq || quiet) return;
    setReviewBusy(false);
    $("rv-list").innerHTML = `<div class="ln-empty">${esc(e.message)}</div>`;
    return;
  }
  if (seq !== review.seq) return;
  review.items = Array.isArray(d.removals) ? d.removals : [];
  if (!quiet) setReviewBusy(false);
  renderReview();
  if (review.items.some((it) => it.mark && it.mark.ask && it.mark.ask.state === "asking")) {
    clearTimeout(review.askTimer);
    review.askTimer = setTimeout(() => loadReview(true), 6000);
  }
}

function reviewItemHTML(it) {
  const res = review.results.get(it.id);
  const done = res && res.ok;
  const st = it.live && it.live.present ? (AGENT_STATUSES.has(it.live.agent_status) ? it.live.agent_status : "unknown") : "";
  const host = multiHost() && it.host ? `<span class="host-chip">${esc(it.host)}</span>` : "";
  const branch = it.branch ? `<span class="wt-chip">⑂ ${esc(it.branch)}</span>` : "";
  const status = st ? `<span class="status ${esc(st)}">${esc(st)}</span>` : '<span class="status">gone</span>';
  const where = [it.repo, it.worktree].filter(Boolean).map(esc).join(" · ");
  const m = it.mark || {};
  const marked = `Marked${m.by ? " by " + esc(m.by) : ""}${m.at ? " · " + esc(fmtAgo(Date.parse(m.at))) + " ago" : ""}${m.reason ? ": " + esc(m.reason) : ""}`;
  const c = it.checks;
  const ok = c && c.exists && !c.primary && !c.dirty && c.unmerged_commits === 0 && !c.error
    ? `<li>Clean, and merged into ${esc(c.base_used || "its base")}: the worktree will be removed (the branch is kept)</li>` : "";
  const ask = m.ask || {};
  const said = ask.state === "safe"
    ? `<li>The session says SAFE${ask.reason ? ": " + esc(ask.reason) : ""}</li>` : "";
  const warnings = (it.warnings || []).map((w) => `<li>${esc(w)}</li>`).join("");
  const notes = (it.notes || []).map((w) => `<li>${esc(w)}</li>`).join("");
  const report = it.final_report
    ? `<details class="rv-report" data-id="${esc(it.id)}"><summary>Final report</summary><div class="md"></div></details>` : "";
  const loss = it.discardable && !done ? lossHTML(it) : "";
  let result = "";
  if (res && res.ok) {
    const r = res.result || {};
    const closed = r.closed === "workspace" ? "Closed its workspace" : r.closed === "pane" ? "Closed its pane (the workspace has other agents or is the repository's home)" : "Nothing was open to close";
    let wt = ".";
    if (r.discarded) {
      wt = ". Discarded the worktree" + (r.branch_deleted ? ` and deleted the branch ${esc(r.branch_deleted)}.` : ".") +
        (r.kept ? `<span class="kept">${esc(r.kept)}</span>` : "");
    } else if (r.worktree_removed) wt = ". Removed the worktree; the branch is kept.";
    else if (r.kept) wt = `.<span class="kept">Kept the worktree: ${esc(r.kept)}</span>`;
    result = `<div class="rv-result ok">${closed}${wt}</div>`;
  } else if (res) {
    result = `<div class="rv-result err">Not removed: ${esc(res.message || res.code || "failed")}</div>`;
  }
  const actions = done ? "" : `<div class="rv-actions">
      <button type="button" class="btn go" data-act="remove" data-id="${esc(it.id)}"${review.busy ? " disabled" : ""}>${it.safe ? "Remove" : "Remove anyway…"}</button>
      ${it.discardable ? `<button type="button" class="btn danger" data-act="discard" data-id="${esc(it.id)}"${review.busy ? " disabled" : ""}>Discard work…</button>` : ""}
      <button type="button" class="btn" data-act="unmark" data-id="${esc(it.id)}"${review.busy ? " disabled" : ""}>Unmark</button>
    </div>`;
  const role = it.coordinator ? '<span class="role-chip">coordinator</span>' : "";
  return `<div class="rv-item${it.safe ? "" : " warn"}${done ? " done" : ""}">
    <div class="rv-top"><span class="rv-name">${esc(it.name || it.pane_id || it.id)}</span>${role}${host}${branch}${status}</div>
    ${where ? `<div class="rv-sub">${where}</div>` : ""}
    <div class="rv-reason">${marked}</div>
    <ul class="rv-list warn">${done ? "" : warnings}</ul>
    <ul class="rv-list ok">${done ? "" : ok + said}</ul>
    <ul class="rv-list info">${done ? "" : notes}</ul>
    ${loss}${report}${actions}${result}
  </div>`;
}

// lossHTML lists what discarding a session's worktree would throw away:
// its unmerged commits (and how many exist nowhere else), its uncommitted
// files, an unfinished rebase or merge, and the branch.
function lossHTML(it) {
  const c = it.checks || {};
  const d = c.detail || {};
  const commits = d.commits || [];
  const files = d.status || [];
  const rows = [];
  if (c.in_progress) rows.push(`<li>the unfinished ${esc(c.in_progress)} (aborted)</li>`);
  if (commits.length) {
    const only = d.unpushed > 0 ? `, ${d.unpushed} on no remote` : "";
    rows.push(`<li>${commits.length} unmerged commit${commits.length === 1 ? "" : "s"}${only}:<ul class="rv-commits">` +
      commits.slice(0, 8).map((cm) => `<li><code>${esc(cm.sha.slice(0, 9))}</code> ${esc(cm.subject)}</li>`).join("") +
      (commits.length > 8 ? `<li>… and ${commits.length - 8} more</li>` : "") + "</ul></li>");
  }
  if (files.length) {
    rows.push(`<li>${c.dirty_files || files.length} uncommitted file${(c.dirty_files || files.length) === 1 ? "" : "s"}:<ul class="rv-commits">` +
      files.slice(0, 8).map((f) => `<li><code>${esc(f)}</code></li>`).join("") +
      (files.length > 8 ? `<li>… and more</li>` : "") + "</ul></li>");
  }
  if (c.branch) rows.push(`<li>the branch <code>${esc(c.branch)}</code></li>`);
  const proposed = it.mark && it.mark.discard ? '<div class="rv-loss-h">Proposed: discard this work</div>' : '<div class="rv-loss-h">Discarding would delete</div>';
  return `<div class="rv-loss">${proposed}<ul>${rows.join("")}</ul></div>`;
}

function renderReview() {
  const list = $("rv-list");
  list.innerHTML = review.items.length ? review.items.map(reviewItemHTML).join("")
    : '<div class="ln-empty">Nothing is marked ready to remove. Mark a session from its ⋮ menu.</div>';
  list.querySelectorAll(".rv-report").forEach((d) => {
    const it = review.items.find((x) => x.id === d.dataset.id);
    if (it) mdOnOpen(d, d.querySelector(".md"), it.final_report, "report:" + it.id);
  });
  renderReviewFoot();
}

function safeIds() {
  return review.items.filter((it) => it.safe && !(review.results.get(it.id) || {}).ok).map((it) => it.id);
}

function renderReviewFoot() {
  const n = safeIds().length;
  $("rv-all").disabled = review.busy || !n;
  $("rv-all").textContent = n ? `Remove all safe (${n})` : "Remove all safe";
}

async function confirmRemovals(ids, allowWorking, coordinators = [], discard = {}) {
  if (review.busy || !ids.length) return;
  setReviewBusy(true, ids.length === 1 ? "Removing…" : `Removing ${ids.length} sessions…`);
  renderReview();
  try {
    const d = await api("/api/removals/confirm", { method: "POST", body: JSON.stringify({ ids, allow_working: !!allowWorking, coordinators, discard }) });
    for (const r of d.results || []) review.results.set(r.id, r);
  } catch (e) {
    toast(e.message, true);
  }
  setReviewBusy(false);
  renderReview();
  ledger.loadedAt = 0;
  loadLedger();
  refreshAgents().catch(() => {});
}

async function reviewAction(act, id) {
  const it = review.items.find((x) => x.id === id);
  if (!it || review.busy) return;
  if (act === "unmark") {
    // The row leaves the review now; it comes back if the request fails.
    const was = review.items.slice();
    review.items = review.items.filter((x) => x.id !== id);
    renderReview();
    try {
      await api("/api/marks/clear", { method: "POST", body: JSON.stringify({ agent_id: id }) });
    } catch (e) {
      if (review.open && !review.items.some((x) => x.id === id)) { review.items = was; renderReview(); }
      toast("Could not unmark: " + e.message, true);
      return;
    }
    ledger.loadedAt = 0;
    loadLedger();
    return;
  }
  const working = it.live && (it.live.agent_status === "working" || it.live.agent_status === "blocked");
  if (act === "discard") {
    const c = it.checks || {};
    const d = c.detail || {};
    const lost = [];
    if (c.in_progress) lost.push(`abort the unfinished ${c.in_progress}`);
    if ((d.commits || []).length) lost.push(`${d.commits.length} unmerged commit(s)` + (d.unpushed > 0 ? `, ${d.unpushed} of them on no remote` : ""));
    if (c.dirty) lost.push(`${c.dirty_files} uncommitted file(s)`);
    if (c.branch) lost.push(`delete the branch ${c.branch}`);
    if (!c.tip || !confirm(`Discard ${it.name || id}?\n\nThis closes the session, removes ${c.path || "its worktree"} and throws away:\n\n• ${lost.join("\n• ")}\n\nThis cannot be undone.`)) return;
    if (it.coordinator && !confirm(`${it.name || id} is a coordinator. Remove this coordinator too?`)) return;
    confirmRemovals([id], working, it.coordinator ? [id] : [], { [id]: c.tip });
    return;
  }
  const others = (it.warnings || []).filter((w) => !it.coordinator || !/coordinator/.test(w));
  if (others.length && !confirm(`${it.name || id}:\n\n• ${others.join("\n• ")}\n\n` +
    "Remove it anyway? The worktree is still kept unless it is clean and merged.")) return;
  // A coordinator is removed only after its own, explicit confirmation.
  if (it.coordinator && !confirm(`${it.name || id} is a coordinator.\n\n` +
    "Removing it closes the agent that hands off and follows your work (the ledger and briefing stay on the hub). Remove this coordinator?")) return;
  confirmRemovals([id], working, it.coordinator ? [id] : []);
}

/* ---------- detail view ---------- */

function agentById(id) {
  return state.agents.find((a) => agentId(a) === id);
}

/* ---------- transcripts: search and read archived sessions ---------- */

// #/transcripts?q=...           the search screen: the newest sessions until there is a query
// #/session/<host>/<kind>/<id>?seq=N&sub=S&q=...   one session, at a match
// The screen covers the app. At desktop width both panes show side by side.
// All text comes from agents and tools: snippets and output go in as text nodes,
// messages through renderMarkdown. Filters may be remembered in localStorage;
// queries and transcript text never are.
const RC_FILTERS_KEY = "comebye.recall.filters";
const RC_PAGE = 100;
const rc = {
  q: "", filters: {}, res: null, loading: false, facets: null, status: null, err: "",
  ctl: null, timer: null, more: false, route: null,
  sess: null, sessKey: "", msgs: [], before: false, after: false, sessErr: "", sessLoading: false,
  hits: [], hitAt: -1, open: new Set(),
};

function rcLoadFilters() {
  try { return JSON.parse(localStorage.getItem(RC_FILTERS_KEY)) || {}; } catch { return {}; }
}
function rcSaveFilters() {
  try { localStorage.setItem(RC_FILTERS_KEY, JSON.stringify(rc.filters)); } catch {}
}

function parseRecallRoute() {
  const h = location.hash;
  const [path, qs] = h.slice(1).split("?");
  const p = new URLSearchParams(qs || "");
  if (path === "/transcripts") return { q: p.get("q") || "" };
  const m = path.match(/^\/session\/([^/]+)\/([^/]+)\/([^/]+)$/);
  if (!m) return null;
  try {
    const seq = p.get("seq");
    return { q: p.get("q") || "", session: { host: decodeURIComponent(m[1]), kind: decodeURIComponent(m[2]), id: decodeURIComponent(m[3]) },
      seq: seq !== null && /^-?\d+$/.test(seq) ? Number(seq) : null, sub: p.get("sub") || "" };
  } catch { return null; }
}

function rcSearchHash(q) { return "#/transcripts" + (q ? "?q=" + encodeURIComponent(q) : ""); }
function rcSessionHash(s, o = {}) {
  const p = new URLSearchParams();
  if (o.seq !== undefined && o.seq !== null) p.set("seq", o.seq);
  if (o.sub) p.set("sub", o.sub);
  if (o.q) p.set("q", o.q);
  const qs = p.toString();
  return `#/session/${encodeURIComponent(s.host)}/${encodeURIComponent(s.kind)}/${encodeURIComponent(s.id)}${qs ? "?" + qs : ""}`;
}

// syncRecall follows the hash: route() calls it.
function syncRecall() {
  const r = parseRecallRoute();
  const el = $("recall");
  const was = !el.hidden;
  rc.route = r;
  el.hidden = !r;
  document.body.classList.toggle("has-recall", !!r);
  if (!r) {
    if (rc.ctl) rc.ctl.abort();
    rc.sess = null; rc.sessKey = ""; rc.msgs = []; rc.hits = [];
    return;
  }
  el.classList.toggle("in-session", !!r.session);
  if (!was) {
    rc.filters = rcLoadFilters();
    rcLoadMeta();
    renderRecallFilters();
  }
  const input = $("rc-q");
  if (document.activeElement !== input || !was) input.value = r.q;
  // Opened afresh with no query, the list of recent sessions may have moved on.
  if (r.q !== rc.q || !rc.res || (!was && !r.q)) {
    rc.q = r.q;
    rcSearch();
  }
  if (r.session) rcOpenSession(r);
  else { $("rc-session").replaceChildren(rcEmptySession()); rc.sessKey = ""; }
  if (!was && !r.session && isDesktop()) input.focus();
}

function rcEmptySession() {
  const d = document.createElement("div");
  d.className = "rc-empty rc-pick";
  d.textContent = "Choose a result to read that session.";
  return d;
}

async function rcLoadMeta() {
  try {
    rc.status = await api("/api/recall/status");
  } catch (e) {
    rc.status = { state: "unavailable" };
  }
  renderRecallFresh();
  renderRecallResults();
  if (rc.status.state !== "ok" && rc.status.state !== "stale") return;
  try {
    rc.facets = await api("/api/recall/facets");
    renderRecallFilters();
  } catch {}
}

// rcLastSeen is when the index was last known current. Its content can be hours
// old with the timer still looking every few minutes.
function rcLastSeen(st) {
  const built = Date.parse(st.built_at || ""), checked = Date.parse(st.checked_at || "");
  return Number.isNaN(checked) || checked < built ? built : checked;
}

function renderRecallFresh() {
  const st = rc.status;
  const el = $("rc-fresh");
  if (!st) { el.textContent = ""; return; }
  const at = rcLastSeen(st);
  el.textContent = Number.isNaN(at) ? "" : `indexed ${fmtAgo(at)} ago`;
  el.classList.toggle("warn", st.state === "stale");
}

const RC_WHEN = { "": "Any time", "7": "Last 7 days", "30": "Last 30 days", "90": "Last 90 days" };
const RC_IN = { "": "Anywhere", said: "Said", commands: "Commands", output: "Output" };
const RC_SORT = { "": "Best match", new: "Newest" };

function rcSelect(key, label, options) {
  const s = document.createElement("select");
  s.className = "rc-chip" + (rc.filters[key] ? " on" : "");
  s.setAttribute("aria-label", label);
  s.dataset.key = key;
  for (const [value, text] of options) {
    const o = document.createElement("option");
    o.value = value; o.textContent = text;
    s.append(o);
  }
  // A remembered value the index no longer has.
  if (rc.filters[key] && !options.some(([v]) => v === rc.filters[key])) {
    const o = document.createElement("option");
    o.value = rc.filters[key]; o.textContent = rc.filters[key];
    s.append(o);
  }
  s.value = rc.filters[key] || "";
  return s;
}

function renderRecallFilters() {
  const f = rc.facets || { hosts: [], kinds: [], projects: [] };
  const opt = (all, list) => [["", all], ...list.map((x) => [x.value, `${x.value} (${x.n})`])];
  $("rc-filters").replaceChildren(
    rcSelect("role", "Where the words are", Object.entries(RC_IN)),
    rcSelect("when", "When", Object.entries(RC_WHEN)),
    rcSelect("host", "Host", opt("Any host", f.hosts)),
    rcSelect("kind", "Agent", opt("Any agent", f.kinds)),
    rcSelect("project", "Project", opt("Any project", f.projects)),
    rcSelect("sort", "Order", Object.entries(RC_SORT)),
  );
  rcSyncFilters();
}

// Where the words are, and best match, mean nothing until there are words.
function rcSyncFilters() {
  for (const s of $("rc-filters").querySelectorAll('[data-key="role"], [data-key="sort"]')) s.hidden = !rc.q;
}

function rcParams(skip) {
  const p = new URLSearchParams({ limit: "20" });
  if (rc.q) p.set("q", rc.q);
  const f = rc.filters;
  for (const k of rc.q ? ["role", "host", "kind", "project", "sort"] : ["host", "kind", "project"]) if (f[k]) p.set(k, f[k]);
  if (f.when) p.set("since", new Date(Date.now() - Number(f.when) * 86400000).toISOString().slice(0, 10));
  if (skip) p.set("skip", String(skip));
  return p;
}

async function rcSearch(more = false) {
  if (rc.ctl) rc.ctl.abort();
  rcSyncFilters();
  const ctl = new AbortController();
  rc.ctl = ctl;
  rc.loading = !more; rc.more = more; rc.err = "";
  if (!more) renderRecallResults();
  try {
    const d = await api((rc.q ? "/api/recall/search?" : "/api/recall/recent?") + rcParams(more ? rc.res.hits.length : 0), { signal: ctl.signal });
    if (rc.ctl !== ctl) return;
    d.recent = !rc.q;
    if (more) {
      rc.res.hits.push(...d.hits);
      rc.res.total = d.total;
    } else rc.res = d;
  } catch (e) {
    if (e.aborted || rc.ctl !== ctl) return;
    rc.err = e.message || "the search failed";
    if (!more) rc.res = null;
  }
  rc.loading = false; rc.more = false;
  renderRecallResults();
}

// rcMarked turns text with \x02 and \x03 around matches into text nodes and <mark>s.
function rcMarked(text) {
  const out = document.createDocumentFragment();
  for (const [i, part] of String(text ?? "").split(/[\x02\x03]/).entries()) {
    if (!part) continue;
    if (i % 2) {
      const m = document.createElement("mark");
      m.textContent = part;
      out.append(m);
    } else out.append(document.createTextNode(part));
  }
  return out;
}

function rcTag(text, cls) {
  const t = document.createElement("span");
  t.className = "rc-tag " + cls;
  t.textContent = text;
  return t;
}

const RC_VERDICT = { landed: "t-green", open: "t-blue", reported: "t-blue", unlanded: "t-orange", abandoned: "t-muted", failed: "t-red" };

function rcDate(iso) {
  const t = Date.parse(iso || "");
  if (Number.isNaN(t)) return "";
  const d = new Date(t);
  const opts = { month: "short", day: "numeric" };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
  return d.toLocaleDateString(undefined, opts);
}

// rcWhen is a date with the time, for a list where the newest are hours apart.
function rcWhen(iso) {
  const t = Date.parse(iso || "");
  if (Number.isNaN(t)) return "";
  return `${rcDate(iso)}, ${new Date(t).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" })}`;
}

function rcMetaLine(s, when = rcDate) {
  return [s.host, s.kind, s.repo, s.branch, when(s.started || s.ended)].filter(Boolean).join(" · ");
}

function rcVerdictTag(v) {
  return v && v !== "unknown" && RC_VERDICT[v] ? rcTag(v, RC_VERDICT[v]) : null;
}

function rcCard(h) {
  const a = document.createElement("a");
  a.className = "rc-card";
  a.href = rcSessionHash(h, { seq: h.seq, sub: h.sub, q: rc.q });
  a.dataset.key = `${h.host}/${h.kind}/${h.id}`;
  const title = document.createElement("div");
  title.className = "rc-title";
  title.textContent = h.title || h.id;
  const meta = document.createElement("div");
  meta.className = "rc-meta";
  // A recent session was not matched by anything, so it has no match to place.
  const recent = h.matches === undefined;
  meta.textContent = rcMetaLine(h, recent ? rcWhen : rcDate);
  a.append(title, meta);
  if (h.snippet) {
    const sn = document.createElement("div");
    sn.className = "rc-snip";
    sn.append(rcMarked(h.snippet));
    a.append(sn);
  }
  const foot = document.createElement("div");
  foot.className = "rc-meta rc-foot";
  const where = { result: " in output", tool: " in commands" }[h.role] || "";
  foot.append(document.createTextNode(recent
    ? `${h.messages} ${h.messages === 1 ? "message" : "messages"}`
    : `${h.matches} ${h.matches === 1 ? "match" : "matches"}${where}`));
  const v = rcVerdictTag(h.verdict);
  if (v) foot.append(v);
  if (h.secrets) foot.append(rcTag("possible secret", "t-orange"));
  a.append(foot);
  return a;
}

function rcCommitCard(c) {
  const a = document.createElement("a");
  a.className = "rc-card";
  a.href = rcSessionHash(c);
  const title = document.createElement("div");
  title.className = "rc-title";
  title.textContent = c.title || c.id;
  const meta = document.createElement("div");
  meta.className = "rc-meta";
  meta.textContent = rcMetaLine(c);
  const sn = document.createElement("div");
  sn.className = "rc-snip";
  sn.textContent = `${c.rel} ${c.sha.slice(0, 12)}${c.subject ? " " + c.subject : ""}`;
  a.append(title, meta, sn);
  return a;
}

function rcNotice(text, code) {
  const d = document.createElement("div");
  d.className = "rc-empty";
  d.textContent = text;
  if (code) {
    const c = document.createElement("code");
    c.textContent = code;
    d.append(document.createTextNode(" "), c);
  }
  return d;
}

function renderRecallResults() {
  const list = $("rc-results");
  const st = rc.status;
  const kids = [];
  if (st && st.state === "stale") {
    const at = rcLastSeen(st);
    const d = rcNotice(`The index was last built ${Number.isNaN(at) ? "a while" : fmtAgo(at)} ago, so recent sessions are missing.`);
    d.classList.add("warn");
    kids.push(d);
  }
  if (st && st.state === "missing") kids.push(rcNotice("This host has no transcript index yet. Build it with", "comebye archive index"));
  else if (st && st.state === "outdated") kids.push(rcNotice("The index is from an older version. The next hourly build replaces it, or run", "comebye archive index"));
  else if (st && st.state === "unavailable") kids.push(rcNotice("The index could not be read. Is sqlite3 installed on this host?"));
  else if (rc.err) { const d = rcNotice(rc.err); d.classList.add("warn"); kids.push(d); }
  else if (rc.loading) kids.push(rcNotice(rc.q ? "Searching…" : "Loading…"));
  else if (rc.res) {
    const count = document.createElement("div");
    count.className = "rc-count";
    const sessions = rc.res.total === 1 ? "1 session" : `${rc.res.total} sessions`;
    count.textContent = rc.res.recent ? `${sessions}, newest first` : sessions;
    if (rc.res.recent) {
      const tip = document.createElement("div");
      tip.className = "rc-count";
      tip.textContent = `Search what agents said, ran and got back. Put a phrase in "quotes"; a commit's SHA also works.`;
      kids.push(tip);
    }
    kids.push(count);
    for (const c of rc.res.commits || []) kids.push(rcCommitCard(c));
    for (const h of rc.res.hits) kids.push(rcCard(h));
    if (!rc.res.total && !(rc.res.commits || []).length) kids.push(rcNotice(rc.res.recent ? "No sessions match these filters." : "Nothing matches. Filters narrow this too, and only the start and end of long output is indexed."));
    if (rc.res.hits.length < rc.res.total) {
      const b = document.createElement("button");
      b.type = "button"; b.className = "btn rc-more"; b.id = "rc-more";
      b.textContent = rc.more ? "Loading…" : "More";
      b.disabled = rc.more;
      kids.push(b);
    }
  }
  list.replaceChildren(...kids);
  rcMarkCurrent();
}

function rcMarkCurrent() {
  const r = rc.route;
  const key = r && r.session ? `${r.session.host}/${r.session.kind}/${r.session.id}` : "";
  for (const c of document.querySelectorAll("#rc-results .rc-card")) c.classList.toggle("current", !!key && c.dataset.key === key);
}

/* session view */

function rcSessionURL(r) {
  const s = r.session;
  return `/api/recall/sessions/${encodeURIComponent(s.host)}/${encodeURIComponent(s.kind)}/${encodeURIComponent(s.id)}`;
}

async function rcFetchPage(r, opts) {
  const p = new URLSearchParams({ limit: String(RC_PAGE) });
  if (r.sub) p.set("sub", r.sub);
  if (r.q) p.set("q", r.q);
  for (const [k, v] of Object.entries(opts)) p.set(k, v);
  return api(rcSessionURL(r) + "?" + p);
}

async function rcOpenSession(r) {
  rcMarkCurrent();
  const key = `${r.session.host}/${r.session.kind}/${r.session.id}|${r.sub}|${r.q}`;
  if (key === rc.sessKey) {
    if (r.seq !== null) rcShowSeq(r.seq);
    return;
  }
  rc.sessKey = key;
  rc.sess = null; rc.msgs = []; rc.hits = []; rc.hitAt = -1; rc.sessErr = ""; rc.open = new Set();
  rc.sessLoading = true;
  renderRecallSession();
  try {
    const d = await rcFetchPage(r, r.seq !== null ? { seq: r.seq, dir: "around" } : {});
    if (rc.sessKey !== key) return;
    rc.sess = d; rc.msgs = d.messages; rc.before = d.before; rc.after = d.after;
  } catch (e) {
    if (rc.sessKey !== key) return;
    rc.sessErr = e.code === "no_session" ? "This session isn't in the index. It was deleted, or it hasn't been indexed yet." : (e.message || "the session could not be read");
  }
  rc.sessLoading = false;
  renderRecallSession();
  if (r.q && !rc.sessErr) {
    try {
      const d = await api(rcSessionURL(r) + "/hits?q=" + encodeURIComponent(r.q));
      if (rc.sessKey !== key) return;
      rc.hits = d.hits.filter((h) => h.sub === r.sub).map((h) => h.seq);
      rc.otherHits = d.hits.length - rc.hits.length;
      rc.hitAt = r.seq !== null ? rc.hits.indexOf(r.seq) : -1;
    } catch {}
    renderRecallFind();
  }
  if (r.seq !== null) rcShowSeq(r.seq);
}

function rcMerge(list, older) {
  return older ? [...list, ...rc.msgs] : [...rc.msgs, ...list];
}

async function rcLoadSide(older) {
  const r = rc.route;
  if (!r || !r.session || !rc.msgs.length) return;
  const key = rc.sessKey;
  const edge = older ? rc.msgs[0].seq : rc.msgs[rc.msgs.length - 1].seq;
  const box = $("rc-session");
  const before = box.scrollHeight - box.scrollTop;
  try {
    const d = await rcFetchPage(r, { seq: edge, dir: older ? "before" : "after" });
    if (rc.sessKey !== key) return;
    rc.msgs = rcMerge(d.messages, older);
    if (older) rc.before = d.before; else rc.after = d.after;
  } catch (e) { toast(e.message || "could not load more", true); return; }
  renderRecallMessages();
  if (older) box.scrollTop = box.scrollHeight - before;
}

// rcShowSeq scrolls to a message, loading the page around it if needed, and
// opens the tool card that holds it.
async function rcShowSeq(seq) {
  const r = rc.route;
  if (!r || !r.session) return;
  const want = (rc.showing = (rc.showing || 0) + 1); // only the latest tap wins
  if (!rc.msgs.some((m) => m.seq === seq)) {
    const key = rc.sessKey;
    try {
      const d = await rcFetchPage(r, { seq, dir: "around" });
      if (rc.sessKey !== key || rc.showing !== want) return;
      rc.msgs = d.messages; rc.before = d.before; rc.after = d.after;
    } catch (e) { toast(e.message || "could not load that message", true); return; }
  }
  rc.current = seq;
  const m = rc.msgs.find((x) => x.seq === seq);
  if (m && m.role === "result" && m.tool_id) rc.open.add(m.tool_id);
  if (m && m.role === "tool") rc.open.add(m.tool_id || "seq" + m.seq);
  if (m && m.role === "result" && !m.tool_id) rc.open.add("seq" + m.seq);
  renderRecallMessages();
  const el = document.querySelector(`#rc-session [data-seq="${seq}"]`);
  if (el) el.scrollIntoView({ block: "center" });
}

function renderRecallFind() {
  const bar = $("rc-find");
  const r = rc.route;
  if (!bar || !r || !r.q) return;
  const count = bar.querySelector(".rc-find-n");
  const n = rc.hits.length;
  let text = n ? `${rc.hitAt >= 0 ? rc.hitAt + 1 : "–"} of ${n}${n >= 500 ? "+" : ""}` : "no matches here";
  if (rc.otherHits) text += ` · ${rc.otherHits} elsewhere`;
  count.textContent = text;
  for (const b of bar.querySelectorAll("button")) b.disabled = !n;
}

function rcStep(d) {
  const n = rc.hits.length;
  if (!n) return;
  // From no current match, down goes to the first and up to the last.
  rc.hitAt = rc.hitAt < 0 ? (d > 0 ? 0 : n - 1) : (rc.hitAt + d + n) % n;
  renderRecallFind();
  rcShowSeq(rc.hits[rc.hitAt]);
}

function renderRecallSession() {
  const box = $("rc-session");
  const r = rc.route;
  if (!r || !r.session) return;
  if (rc.sessErr) { box.replaceChildren(rcNotice(rc.sessErr)); return; }
  if (!rc.sess) { box.replaceChildren(rcNotice(rc.sessLoading ? "Loading…" : "")); return; }
  const s = rc.sess.session;
  const kids = [];
  if (r.q) {
    const bar = document.createElement("div");
    bar.id = "rc-find"; bar.className = "rc-find";
    const term = document.createElement("span");
    term.className = "rc-find-q"; term.textContent = r.q;
    const n = document.createElement("span");
    n.className = "rc-find-n"; n.setAttribute("aria-live", "polite");
    const up = document.createElement("button");
    up.type = "button"; up.className = "rc-step"; up.id = "rc-prev"; up.textContent = "↑"; up.setAttribute("aria-label", "Previous match");
    const down = document.createElement("button");
    down.type = "button"; down.className = "rc-step"; down.id = "rc-next"; down.textContent = "↓"; down.setAttribute("aria-label", "Next match");
    bar.append(term, n, up, down);
    kids.push(bar);
  }
  const body = document.createElement("div");
  body.className = "rc-sbody";
  const title = document.createElement("h2");
  title.className = "rc-stitle"; title.textContent = s.title || s.id;
  const meta = document.createElement("div");
  meta.className = "rc-meta";
  meta.textContent = `${rcMetaLine(s)} · ${s.messages} messages`;
  const v = rcVerdictTag(s.verdict);
  if (v) { meta.append(" "); meta.append(v); }
  body.append(title, meta);
  if (rc.sess.subs.length) {
    const subs = document.createElement("nav");
    subs.className = "rc-subs"; subs.setAttribute("aria-label", "Subagents");
    const link = (label, sub) => {
      const a = document.createElement("a");
      a.href = rcSessionHash(s, { sub, q: r.q });
      a.textContent = label;
      a.className = "rc-chip" + (sub === r.sub ? " on" : "");
      return a;
    };
    subs.append(link("Main thread", ""));
    for (const x of rc.sess.subs) subs.append(link(`Subagent ${x.id.slice(0, 8)} (${x.messages})`, x.id));
    body.append(subs);
  }
  if (s.secrets) {
    const w = document.createElement("div");
    w.className = "rc-secret";
    w.textContent = `${s.secrets === 1 ? "A possible secret was" : s.secrets + " possible secrets were"} masked in this session. Masking can miss things, so treat what's here as sensitive.`;
    body.append(w);
  }
  const clipped = document.createElement("div");
  clipped.className = "rc-meta rc-clipnote";
  clipped.textContent = "This view shows the indexed text: long messages and tool output are clipped. The archive holds them in full.";
  body.append(clipped);
  const list = document.createElement("div");
  list.id = "rc-msgs"; list.className = "rc-msgs";
  body.append(list);
  kids.push(body);
  box.replaceChildren(...kids);
  box.scrollTop = 0;
  renderRecallMessages();
  renderRecallFind();
}

// rcHighlightText wraps the matched words in a rendered, sanitized message: it
// only touches text nodes, so no markup comes from the transcript's text.
function rcHighlightText(root, terms) {
  if (!terms.size) return;
  const list = [...terms].slice(0, 20).map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const re = new RegExp(list.join("|"), "gi");
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes = [];
  while (walker.nextNode()) nodes.push(walker.currentNode);
  for (const n of nodes) {
    const t = n.nodeValue;
    re.lastIndex = 0;
    if (!re.test(t)) continue;
    re.lastIndex = 0;
    const f = document.createDocumentFragment();
    let at = 0, m;
    while ((m = re.exec(t))) {
      if (!m[0]) break;
      f.append(document.createTextNode(t.slice(at, m.index)));
      const mk = document.createElement("mark");
      mk.textContent = m[0];
      f.append(mk);
      at = m.index + m[0].length;
    }
    f.append(document.createTextNode(t.slice(at)));
    n.replaceWith(f);
  }
}

function rcTerms(text) {
  const out = new Set();
  String(text).split(/[\x02\x03]/).forEach((p, i) => { if (i % 2 && p) out.add(p.toLowerCase()); });
  return out;
}

function rcPlain(text) { return String(text ?? "").replace(/[\x02\x03]/g, ""); }

function rcClipNote(m, kept) {
  if (!m.full_len) return null;
  const d = document.createElement("div");
  d.className = "rc-note";
  d.textContent = `Clipped: ${kept} of ${m.full_len.toLocaleString("en-US")} bytes kept.`;
  return d;
}

function rcSecretNote(m) {
  if (!m.secret) return null;
  const d = document.createElement("div");
  d.className = "rc-secret";
  d.textContent = "A possible secret was masked here.";
  return d;
}

function rcMessage(m) {
  const d = document.createElement("div");
  d.dataset.seq = m.seq;
  if (m.role === "handoff") {
    d.className = "rc-handoff";
    const det = document.createElement("details");
    det.open = m.seq === rc.current;
    const sum = document.createElement("summary");
    sum.textContent = "Handoff brief · from the ledger";
    const body = document.createElement("div");
    body.className = "md";
    body.innerHTML = renderMarkdown(rcPlain(m.text));
    rcHighlightText(body, rcTerms(m.text));
    det.append(sum, body);
    d.append(det);
  } else {
    d.className = "rc-msg " + (m.role === "user" ? "rc-user" : "rc-asst");
    const body = document.createElement("div");
    body.className = "md";
    body.innerHTML = renderMarkdown(rcPlain(m.text));
    rcHighlightText(body, rcTerms(m.text));
    d.append(body);
    const n = rcClipNote(m, "the first 4,000");
    if (n) d.append(n);
  }
  const s = rcSecretNote(m);
  if (s) d.append(s);
  if (m.seq === rc.current) d.classList.add("cur");
  return d;
}

// rcToolCard shows a tool call on one line; opening it shows the output as text.
function rcToolCard(call, result) {
  const id = (call && call.tool_id) || (result && result.tool_id) || "seq" + (call || result).seq;
  const det = document.createElement("details");
  det.className = "rc-tool";
  det.open = rc.open.has(id);
  det.dataset.tool = id;
  const sum = document.createElement("summary");
  const text = rcPlain(call ? call.text : "");
  const sp = text.indexOf(" ");
  const nm = document.createElement("span");
  nm.className = "rc-tname";
  nm.textContent = call ? (sp < 0 ? text : text.slice(0, sp)) : "Output";
  const ar = document.createElement("span");
  ar.className = "rc-targs";
  ar.textContent = call && sp >= 0 ? text.slice(sp + 1) : "";
  const st = document.createElement("span");
  st.className = "rc-tstate " + (result ? (result.is_err ? "t-red" : "t-green") : "t-muted");
  st.textContent = result ? (result.is_err ? "error" : "ok") : "";
  sum.append(nm, ar, st);
  det.append(sum);
  if (call) {
    const args = document.createElement("pre");
    args.className = "rc-args";
    args.dataset.seq = call.seq;
    const rawSp = call.text.indexOf(" ");
    args.append(rcMarked(rawSp < 0 ? call.text : call.text.slice(rawSp + 1)));
    det.append(args);
    if (call.seq === rc.current) { args.classList.add("cur"); }
    const s = rcSecretNote(call);
    if (s) det.append(s);
  }
  if (result) {
    const out = document.createElement("pre");
    out.className = "rc-out";
    out.dataset.seq = result.seq;
    out.append(rcMarked(result.text));
    if (result.seq === rc.current) out.classList.add("cur");
    det.append(out);
    const n = rcClipNote(result, "the first and last 1,500");
    if (n) det.append(n);
    const s = rcSecretNote(result);
    if (s) det.append(s);
  }
  det.addEventListener("toggle", () => { if (det.open) rc.open.add(id); else rc.open.delete(id); });
  return det;
}

function renderRecallMessages() {
  const list = $("rc-msgs");
  if (!list) return;
  const results = new Map();
  for (const m of rc.msgs) if (m.role === "result" && m.tool_id) results.set(m.tool_id, m);
  const calls = new Set();
  for (const m of rc.msgs) if (m.role === "tool" && m.tool_id) calls.add(m.tool_id);
  const kids = [];
  if (rc.before) {
    const b = document.createElement("button");
    b.type = "button"; b.className = "btn rc-more"; b.id = "rc-earlier"; b.textContent = "Earlier messages";
    kids.push(b);
  }
  for (const m of rc.msgs) {
    if (m.role === "tool") kids.push(rcToolCard(m, m.tool_id ? results.get(m.tool_id) : null));
    else if (m.role === "result") {
      if (!m.tool_id || !calls.has(m.tool_id)) kids.push(rcToolCard(null, m)); // its call is on another page
    } else kids.push(rcMessage(m));
  }
  if (rc.after) {
    const b = document.createElement("button");
    b.type = "button"; b.className = "btn rc-more"; b.id = "rc-later"; b.textContent = "Later messages";
    kids.push(b);
  }
  list.replaceChildren(...kids);
}

// openRecall opens the Transcripts screen, at the last search if there was one.
function openRecall() { location.hash = rcSearchHash(rc.q); }

function closeRecall() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

function rcBack() {
  const r = rc.route;
  if (r && r.session && !isDesktop()) location.hash = rcSearchHash(r.q);
  else closeRecall();
}

function initRecall() {
  $("btn-recall").addEventListener("click", openRecall);
  $("rc-back").addEventListener("click", rcBack);
  $("rc-q").addEventListener("input", (ev) => {
    clearTimeout(rc.timer);
    const v = ev.target.value;
    rc.timer = setTimeout(() => {
      const r = rc.route;
      if (!r || v.trim() === rc.q) return;
      if (v.length > 500) return;
      rc.q = v.trim();
      // Replaces the entry, so back leaves the search rather than stepping through each word.
      if (!r.session) history.replaceState(null, "", location.pathname + location.search + rcSearchHash(rc.q));
      rcSearch();
    }, 300);
  });
  $("rc-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearTimeout(rc.timer);
    const v = $("rc-q").value.trim();
    if (rc.route && rc.route.session && !isDesktop()) { location.hash = rcSearchHash(v); return; }
    if (v !== rc.q) { rc.q = v; history.replaceState(null, "", location.pathname + location.search + rcSearchHash(rc.q)); rcSearch(); }
    $("rc-q").blur();
  });
  $("rc-filters").addEventListener("change", (ev) => {
    const k = ev.target.dataset.key;
    if (!k) return;
    if (ev.target.value) rc.filters[k] = ev.target.value; else delete rc.filters[k];
    ev.target.classList.toggle("on", !!ev.target.value);
    rcSaveFilters();
    rcSearch();
  });
  $("rc-results").addEventListener("click", (ev) => {
    if (ev.target.closest("#rc-more") && !rc.more) { rc.more = true; renderRecallResults(); rcSearch(true); }
  });
  $("rc-session").addEventListener("click", (ev) => {
    if (ev.target.closest("#rc-earlier")) rcLoadSide(true);
    else if (ev.target.closest("#rc-later")) rcLoadSide(false);
    else if (ev.target.closest("#rc-prev")) rcStep(-1);
    else if (ev.target.closest("#rc-next")) rcStep(1);
  });
  $("recall").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") { ev.stopPropagation(); rcBack(); }
  });
}

/* ---------- routing: #/agent/[<host>/]<pane> deep links (notification taps) ---------- */

// parseRoute reads #/agent/<pane> (the server's own host, as in older
// links) or #/agent/<host>/<pane>; each part is URI-encoded.
function parseRoute() {
  const m = location.hash.match(/^#\/agent\/(?:([^/]+)\/)?([^/]+)$/);
  if (!m) return null;
  try {
    return { host: m[1] ? decodeURIComponent(m[1]) : null, pane: decodeURIComponent(m[2]) };
  } catch { return null; }
}

function routedAgent(r) {
  return state.agents.find((a) => a.pane_id === r.pane &&
    (r.host && r.host !== state.localHost ? a.host === r.host : isLocal(a)));
}

// route syncs the view to the URL hash. An agent that is not in the list
// yet (cold start from a notification) is opened once the list arrives.
function route() {
  syncRecall();
  syncBriefs();
  syncSubview();
  syncLaunch();
  syncReview();
  syncMark();
  syncSweep();
  syncSwitch();
  const r = parseRoute();
  if (!r) {
    state.pendingRoute = null;
    if (state.selected) closeDetail();
    if (isDesktop()) setView(viewFromHash()); // the hash names the page
    return;
  }
  const a = routedAgent(r);
  if (a && state.selected === agentId(a)) return;
  if (a) { state.pendingRoute = null; openDetail(agentId(a)); }
  else state.pendingRoute = r;
}

// applyPendingRoute runs after each authoritative agent list.
function applyPendingRoute() {
  const r = state.pendingRoute;
  if (!r) { openToSomething(); return; }
  const a = routedAgent(r);
  if (a) { state.pendingRoute = null; openDetail(agentId(a)); return; }
  const h = r.host && hostStatus(r.host);
  if (h && h.status === "connecting") return; // the hub has not reached that peer yet; wait
  state.pendingRoute = null;
  toast(h && h.status === "down" ? `${r.host} is unreachable${h.class ? `: ${h.class}` : ""}` : "that agent is gone", true);
  history.replaceState(null, "", location.pathname + location.search);
}

/* ---------- pages (desktop): Briefing, To do, Search fill the main pane ---------- */
//
// One selection fills one pane: an agent (state.selected) or a page
// (state.view), never both. The pages are only a desktop thing; below
// 1100px the hash routes do nothing new and the phone flow is unchanged.

const PAGE_HASH = { status: "#/status", briefing: "#/briefings", todo: "#/todo" }; // briefing: the old hand-written one, reached from Status
const viewFromHash = () => (isDesktop() ? Object.keys(PAGE_HASH).find((k) => PAGE_HASH[k] === location.hash) || null : null);

// setView shows a page (or none) without touching the history.
function setView(v) {
  state.view = isDesktop() ? v : null;
  for (const k of ["status", "briefing", "todo"]) document.body.classList.toggle("view-" + k, state.view === k);
  document.body.classList.toggle("has-page", !!state.view);
  syncDesktopBriefs();
  renderNav();
  renderStatus();
  if (state.view === "todo") renderTodos();
}

// openPage replaces the agent in the main pane with a page. Leaving the
// agent goes through closeDetail, which stops the polling and clears
// presence: otherwise the server keeps holding notifications for an agent
// that is no longer on screen.
function openPage(v) {
  if (!isDesktop()) return;
  if (state.selected != null) closeDetail();
  const hash = PAGE_HASH[v] || "";
  if (location.hash !== hash && (hash || location.hash)) history.pushState(null, "", hash || location.pathname + location.search);
  setView(v);
}

// On a cold start the desktop opens to something: an agent that is waiting
// on you if there is one, otherwise the Status page. A deep link or a
// page's own hash wins, and it happens once.
function openToSomething() {
  if (state.opened || !isDesktop() || state.pendingRoute) return;
  state.opened = true;
  if (state.selected || state.view || location.hash) return;
  const w = document.querySelector("#agents .agent.blocked[data-id]");
  if (w) { openDetail(w.dataset.id); return; }
  if (ledger.on !== false) {
    history.replaceState(null, "", PAGE_HASH.status);
    setView("status");
  }
}

/* ---------- Status: a page generated from the ledger, the to-do list and the agents ---------- */
//
// Nothing here is written by anyone: it is the same data the list and the
// To do page show, grouped by project, so it is as fresh as they are.
//   in progress     running handoffs (worker, branch, age)
//   blocked         a handoff queued behind earlier steps (ledger "waiting"),
//                   or a running one whose agent is stuck on a dialog
//   waiting on you  open to-do items, linked to the To do page
//   finished        reported / landed / ready to remove / failed in the last
//                   2 days, each with its report
// There is no "Needs you" section: the To do page is that.

const STATUS_RECENT_MS = 2 * 24 * 3600 * 1000;
const statusUi = { open: new Set() };

function statusModel() {
  const now = Date.now();
  const projects = new Map();
  const P = (name) => { if (!projects.has(name)) projects.set(name, { name, progress: [], blocked: [], todos: [], done: [] }); return projects.get(name); };
  for (const e of ledger.entries) {
    if (!e) continue;
    const proj = P(e.repo_name || "Other");
    const ag = e.agent_id ? agentById(e.agent_id) : null;
    const at = Date.parse(e.finished_at || e.created_at);
    if (e.status === "waiting") proj.blocked.push({ e, why: `queued behind ${(e.after || []).length || "earlier"} step${(e.after || []).length === 1 ? "" : "s"}` });
    else if (e.status === "running" || e.status === "landing") {
      if (ag && ag.agent_status === "blocked") proj.blocked.push({ e, ag, why: "waiting on a dialog" });
      else proj.progress.push({ e, ag });
    } else if (["finished", "landed", "ready_to_remove", "failed"].includes(e.status) && !Number.isNaN(at) && now - at < STATUS_RECENT_MS) proj.done.push({ e, at, ag });
  }
  if (todos.on === true) {
    for (const t of todos.items) {
      if (t.state !== "open") continue;
      const wa = t.waiting && agentById(t.waiting.id);
      P(wa ? repoLabel(wa).repo || "Other" : "Not tied to a project").todos.push(t);
    }
  }
  for (const p of projects.values()) p.done.sort((a, b) => b.at - a.at);
  return [...projects.values()].filter((p) => p.progress.length + p.blocked.length + p.todos.length + p.done.length)
    .sort((a, b) => (a.name === "Not tied to a project") - (b.name === "Not tied to a project") ||
      (b.blocked.length + b.todos.length > 0) - (a.blocked.length + a.todos.length > 0) || a.name.localeCompare(b.name));
}

function statusWho(e) { return e.agent_name || e.branch || e.id; }

function statusHTML(desktop) {
  if (ledger.on === false) return emptyHtml("Status is not available here", "It needs the hub's ledger, which this host does not keep.");
  if (ledger.on !== true) return emptyHtml("Loading…");
  const ps = statusModel();
  const nProg = ps.reduce((n, p) => n + p.progress.length, 0), nBlk = ps.reduce((n, p) => n + p.blocked.length, 0), nYou = ps.reduce((n, p) => n + p.todos.length, 0);
  const dmg = repoDamageList();
  const sum = [dmg.length ? `${dmg.length} ${dmg.length === 1 ? "repository needs" : "repositories need"} repair` : "", nProg ? `${nProg} in progress` : "", nBlk ? `${nBlk} blocked` : "", nYou ? `${nYou} waiting on you` : ""].filter(Boolean).join(" · ") || "Nothing in progress, and nothing is waiting on you.";
  const open = (a) => desktop && a ? ` <button type="button" class="st-link" data-agent="${escAttr(agentId(a))}">open</button>` : "";
  const card = (p) => {
    let h = "";
    if (p.blocked.length) h += `<div class="st-g blocked">Blocked</div>` + p.blocked.map(({ e, ag, why }) =>
      `<div class="st-row"><span class="st-n">${esc(statusWho(e))}</span><span class="st-m warn">${esc(why)}</span><span class="st-r">${esc(e.branch || "")}${open(ag)}</span></div>`).join("");
    if (p.todos.length) h += `<div class="st-g you">Waiting on you</div>` + p.todos.map((t) =>
      `<div class="st-row"><span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span><span class="st-n">${esc(t.question)}</span>${desktop ? `<span class="st-r"><button type="button" class="st-link" data-go="todo">To do</button></span>` : ""}</div>`).join("");
    if (p.progress.length) h += `<div class="st-g prog">In progress</div>` + p.progress.map(({ e, ag }) => {
      const at = Date.parse(e.created_at);
      return `<div class="st-row"><span class="st-n">${esc(statusWho(e))}</span>${e.branch ? `<span class="wt-chip">⑂ ${esc(e.branch)}</span>` : ""}<span class="st-m">${e.status === "landing" ? "landing" : "running"}${Number.isNaN(at) ? "" : " " + esc(fmtAgo(at))}</span><span class="st-r">${open(ag)}</span></div>`;
    }).join("");
    if (p.done.length) h += `<div class="st-g">Finished recently</div>` + p.done.map(({ e, at, ag }) => {
      const chip = { finished: ["", "reported"], landed: ["landed", "landed"], ready_to_remove: ["rtr", "ready to remove"], failed: ["failed", "failed"] }[e.status];
      const body = e.final_report ? `<div class="md st-rep">${renderMarkdown(e.final_report, "status:" + e.id)}</div>` : `<div class="st-m st-rep">${esc(e.error || "No report.")}</div>`;
      return `<details class="st-row" data-id="${escAttr(e.id)}"${statusUi.open.has(e.id) ? " open" : ""}><summary><span class="st-n">${esc(statusWho(e))}</span><span class="lg-chip ${chip[0]}">${chip[1]}</span><span class="st-r">${esc(fmtAgo(at))} ago</span></summary>${statusUi.open.has(e.id) ? body : ""}</details>`;
    }).join("");
    return `<section class="st-proj${p.blocked.length || p.todos.length ? " hot" : ""}" aria-label="${escAttr(p.name)}"><div class="st-ph">${esc(p.name)}</div>${h}</section>`;
  };
  const repair = dmg.length ? `<section class="st-proj hot st-repair" aria-label="Repositories need repair"><div class="st-ph">Repositories need repair</div>${dmg.map(({ host, d }) => {
    const n = state.agents.filter((a) => repoDamageOf(a) === d).length;
    const since = Date.parse(d.since);
    return `<div class="st-row"><span class="st-n">${esc(multiHost() ? `${host} · ${d.repo}` : d.repo)}</span><span class="st-m warn">${esc(d.reason)}${d.ref ? ` at <code>${esc(d.ref)}</code>` : ""}</span><span class="st-r">${Number.isNaN(since) ? "" : "since " + esc(fmtAgo(since)) + " ago"}</span>` +
      `<div class="st-hint">${esc(d.hint)}</div><div class="st-m">${esc(d.path)} · found by ${esc(d.source)}${n ? ` · ${n} ${n === 1 ? "agent works" : "agents work"} in it` : ""}</div></div>`;
  }).join("")}</section>` : "";
  return `<div class="st"><p class="st-sum">${esc(sum)}</p>${repair}${briefCardHtml()}${ps.map(card).join("")}${
    desktop ? `<p class="st-more"><button type="button" class="st-link" data-go="briefing">Earlier hand-written briefings</button></p>` : ""}</div>`;
}

// The Briefings card: when the last briefing was, when the next is due, and
// the schedule, edited in place through the hub's brief schedule API (what
// `comebye brief schedule` calls). Anything else a schedule can carry (a time
// of day, a long interval) shows as a pressed "Every …" chip and is kept.
const BRIEF_INTERVALS = [["", "Off"], ["1h", "1 h"], ["3h", "3 h"], ["6h", "6 h"]];
const BRIEF_QUIET = ["6h", "12h", "24h", "48h"];
const briefCard = { busy: false };
const hoursLabel = (d) => { const m = /^(\d+)h$/.exec(d || ""); return m ? m[1] + " h" : d || ""; };

function briefCardHtml() {
  const sc = briefing.schedule;
  if (!sc) return "";
  const on = !!sc.enabled;
  const cur = on ? sc.every : "";
  const b = briefing.data && briefing.data.markdown.trim() ? briefing.data : null;
  const checked = briefChecked();
  const next = !on ? "off" : sc.next && fmtWhen(sc.next) ? fmtWhen(sc.next) : "soon";
  const state = on ? { waiting: " · waiting for a busy coordinator", prompted: " · a coordinator was asked" }[sc.state] || "" : "";
  const quiet = sc.max_quiet || "6h";
  const quietOpts = BRIEF_QUIET.includes(quiet) ? BRIEF_QUIET : [...BRIEF_QUIET, quiet];
  const seg = BRIEF_INTERVALS.map(([v, l]) => `<button type="button" data-bk="int-${v || "off"}" data-bi="${v}" aria-pressed="${v === cur}">${l}</button>`).join("") +
    (on && !BRIEF_INTERVALS.some(([v]) => v === cur) ? `<button type="button" data-bk="int-custom" aria-pressed="true" disabled>Every ${esc(sc.every)}${sc.at ? " at " + esc(sc.at) : ""}</button>` : "");
  return `<section class="bcard" aria-label="Briefings">
    <div class="bc-h">Briefings</div>
    <div class="bc-kv"><span>Last briefing</span><b>${esc(b ? fmtWhen(b.at) + (checked ? " · " + checked : "") : "none yet")}</b></div>
    <div class="bc-kv"><span>Next briefing</span><b>${esc(next + state)}</b></div>
    <div class="bc-seg" role="group" aria-label="Ask a coordinator for a briefing every">${seg}</div>
    <label class="bc-chk"><input type="checkbox" data-bk="skip" data-bskip${sc.always ? "" : " checked"}${on ? "" : " disabled"}> Skip when nothing has changed</label>
    <label class="bc-fld">Max quiet<select data-bk="quiet" data-bquiet${on && !sc.always ? "" : " disabled"}>${quietOpts.map((v) => `<option value="${v}"${v === quiet ? " selected" : ""}>${hoursLabel(v)}</option>`).join("")}</select>
      <small>With skipping on, ask anyway once the last briefing is this old.</small></label>
    <div class="bc-acts"><button type="button" class="btn" data-bk="pub" data-bpub>Publish now</button><button type="button" class="btn" data-bk="hist" data-bhist>History</button></div>
  </section>`;
}

async function saveSchedule(patch, say) {
  if (briefCard.busy) return;
  const c = briefing.schedule || {};
  const body = { enabled: !!c.enabled, every: c.every || "", at: c.at || "", tz: c.tz || "", always: !!c.always, max_quiet: c.max_quiet || "", ...patch };
  briefCard.busy = true;
  try {
    const d = await api("/api/brief/schedule", { method: "POST", body: JSON.stringify(body) });
    if (d && d.schedule) briefing.schedule = d.schedule;
    toast(say(briefing.schedule || {}));
  } catch (e) {
    toast(e.message, true);
  } finally {
    briefCard.busy = false;
  }
  for (const el of [$("ps-body"), $("br-status")]) if (el) el._html = "";
  renderStatus();
  renderBrief();
}

function setBriefInterval(v) {
  // Off keeps the interval for next time (the hub holds on to it); a picked interval drops a time of day.
  if (!v) saveSchedule({ enabled: false, every: "", at: "", tz: "" }, () => "Scheduled briefings off");
  else saveSchedule({ enabled: true, every: v, at: "", tz: "" }, (sc) => `Briefing every ${hoursLabel(sc.every)}`);
}

// briefNow asks the coordinator for a briefing right away, as a scheduled
// run does (the hub has no endpoint for it; this is a prompt to its pane).
async function briefNow() {
  if (briefCard.busy) return;
  const a = state.agents.find((x) => isCoordinator(x));
  if (!a) { toast("No coordinator is running. Start one first.", true); return; }
  briefCard.busy = true;
  try {
    await api(`${pathFor(agentId(a))}/prompt`, { method: "POST", body: JSON.stringify({ text: "Publish a briefing now (asked from the Comebye Status page). Run `comebye todo list` first and relay answered items, catch up from the ledger (`comebye ledger`) and the workers' reports since the last briefing, then publish with `comebye brief set` in the playbook's format. If nothing changed, publish a short one that says so." }) });
    toast("Asked " + (a.name || "the coordinator") + " for a briefing");
  } catch (e) {
    toast(e.status === 409 ? "The coordinator is waiting for input; answer it first" : "Could not ask: " + e.message, true);
  } finally {
    briefCard.busy = false;
  }
}

function briefHistory() {
  if (isDesktop()) openPage("briefing");
  else document.querySelector("#br-body .br-h")?.scrollIntoView?.({ block: "start" });
}

// renderStatus fills the desktop page and, on the phone, the top of the briefings screen.
function renderStatus() {
  for (const [el, desktop] of [[$("ps-body"), true], [$("br-status"), false]]) {
    if (!el) continue;
    if (desktop && state.view !== "status") continue; // only the page on screen
    if (!desktop && !briefs.open) continue;
    const html = statusHTML(desktop);
    if (el._html === html) continue;
    el._html = html;
    const sc = el.closest(".page-body, .br-body");
    const y = sc ? sc.scrollTop : 0;
    const key = el.contains(document.activeElement) ? document.activeElement.dataset.bk : "";
    el.innerHTML = html;
    if (sc) sc.scrollTop = y;
    if (key) el.querySelector(`[data-bk="${key}"]`)?.focus({ preventScroll: true });
  }
}

function onStatusClick(ev) {
  const g = ev.target.closest("[data-go]"), a = ev.target.closest("[data-agent]"), bi = ev.target.closest("[data-bi]");
  if (bi) setBriefInterval(bi.dataset.bi);
  else if (ev.target.closest("[data-bpub]")) briefNow();
  else if (ev.target.closest("[data-bhist]")) briefHistory();
  else if (g) openPage(g.dataset.go);
  else if (a) openDetail(a.dataset.agent);
}

function onStatusChange(ev) {
  const t = ev.target;
  if (t.matches?.("[data-bskip]")) saveSchedule({ always: !t.checked }, (sc) => sc.always ? "A briefing every slot" : "Skipping when nothing has changed");
  else if (t.matches?.("[data-bquiet]")) saveSchedule({ max_quiet: t.value }, (sc) => "Max quiet " + hoursLabel(sc.max_quiet));
}

// An opened report is kept across re-renders, and rendered when first opened.
function onStatusToggle(ev) {
  const d = ev.target;
  if (!d.matches || !d.matches("details.st-row")) return;
  if (d.open === statusUi.open.has(d.dataset.id)) return; // a re-rendered <details open> fires toggle too: not a change
  if (d.open) statusUi.open.add(d.dataset.id); else statusUi.open.delete(d.dataset.id);
  for (const el of [$("ps-body"), $("br-status")]) if (el) el._html = "";
  renderStatus();
}

/* ---------- the page rows above the agent list (desktop) ---------- */
//
// One table; a row is a page (openPage) or an action (a dialog). The
// Briefing row is the only one that depends on the briefing, so it is the
// one to delete if the briefing goes: with its page (#briefs) and its fleet
// menu items.

const NAV_ICON = {
  status: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16M4 12h10M4 18h13"/><circle cx="19" cy="12" r="1.5"/></svg>',
  todo: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="16" rx="3"/><path d="m8 12 3 3 5-6"/></svg>',
  search: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></svg>',
  removals: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/></svg>',
};
const NAV_ROWS = [
  { key: "status", label: "Status", show: () => ledger.on === true },
  { key: "todo", label: "To do", show: () => todos.on === true, count: () => todos.open },
  { key: "search", label: "Search", show: () => true, open: () => openRecall(), noPage: true }, // Transcripts is a screen of its own
  { key: "removals", label: "Ready to remove", show: () => ledger.on === true && ledger.marked.size > 0, count: () => ledger.marked.size, open: () => openReview(), noPage: true },
];

function renderNav() {
  const el = $("nav");
  if (!el) return;
  const rows = NAV_ROWS.filter((r) => r.show());
  const html = rows.map((r) => {
    const n = r.count ? r.count() : 0;
    const on = !r.noPage && state.view === r.key;
    return `<button type="button" class="nv${on ? " on" : ""}" data-page="${r.key}"${on ? ' aria-current="page"' : ""}>${NAV_ICON[r.key]}<span>${esc(r.label)}</span>${
      n ? `<span class="badge" aria-label="${n} open">${n}</span>` : r.dot && r.dot() ? '<span class="bdot" aria-label="new"></span>' : ""}</button>`;
  }).join("");
  if (el.dataset.sig !== html) { el.dataset.sig = html; el.innerHTML = html; }
}

function onNavClick(ev) {
  const b = ev.target.closest("button[data-page]");
  const r = b && NAV_ROWS.find((x) => x.key === b.dataset.page);
  if (!r) return;
  if (r.open) r.open(); else openPage(r.key);
}

function openDetail(id) {
  const a = agentById(id);
  if (!a) return;
  setView(null);
  resetTerminalView();
  const hash = agentHash(a);
  if (location.hash !== hash) {
    // A history entry, so the phone's back gesture returns to the list.
    history.pushState(null, "", hash);
    state.pushedRoute = true;
  }
  state.selected = id;
  state.chat = null;
  state.ask = null;
  state.lastStatus = a.agent_status || null;
  $("list").style.display = "none";
  $("detail").style.display = "flex";
  document.body.classList.add("has-detail");
  markSelectedRow();
  renderDetail();
  loadChat().then(refreshStatusLine);
  startPolling();
  reportPresence();
}

function closeDetail() {
  toggleKeypad(false);
  toggleDetailMenu(false);
  closeActivityPanel();
  hideSubview();
  clearInterval(activity.timer);
  activity.timer = null;
  activity.seenAt = 0;
  const header = document.querySelector("#detail header");
  if (header) header.classList.remove("busy");
  state.selected = null;
  state.chat = null;
  state.ask = null;
  state.pushedRoute = false;
  resetTerminalView();
  if (parseRoute()) history.replaceState(null, "", location.pathname + location.search);
  stopPolling();
  updateAnswerCard();
  $("detail").style.display = "none";
  $("list").style.display = "block";
  document.body.classList.remove("has-detail");
  markSelectedRow();
  reportPresence();
  maybeReload(); // a deploy noticed while on the detail view
}

function renderDetail() {
  const a = agentById(state.selected);
  if (!a) {
    const h = hostStatus(hostOf(state.selected));
    if (h && !h.local && h.status !== "ok") toast(`${h.name} is unreachable${h.class ? `: ${h.class}` : ""}`, true);
    closeDetail();
    return;
  }
  $("d-title").textContent = a.name || a.terminal_title_stripped || a.display_agent || a.agent || a.pane_id;
  let sub;
  if (a.worktree && a.worktree.repo) {
    sub = a.worktree.repo +
      (a.worktree.branch ? " ⑂ " + a.worktree.branch : "") +
      " — " + a.worktree.checkout;
  } else {
    sub = (a.cwd || a.pane_id) + (a.agent ? " · " + a.agent : "");
  }
  if (multiHost() && a.host) sub = a.host + " · " + sub;
  $("d-subtitle").textContent = sub;
  const st = $("d-status");
  st.textContent = a.agent_status;
  st.className = "status " + a.agent_status;
  updateAnswerCard();
  updateSendButton();
  renderActivity();
  renderDetailMenu();
  if (sw.open) renderSwitch(); // the agent may have gone idle (or busy) meanwhile
  refreshQueued();
}

/* ---------- queued prompts ---------- */
//
// Prompts an agent (or `comebye prompt --queue`) parked on the hub for this
// agent: they are sent once it is idle with no question open. Shown above
// the composer with a way to withdraw each one.

const queued = { id: null, at: 0, items: [], busy: false, gone: new Set() };

function renderQueued() {
  const el = $("queued");
  const items = queued.id === state.selected ? queued.items : [];
  el.hidden = items.length === 0;
  if (!items.length) { el.textContent = ""; return; }
  // The head of the queue is "held" when the agent is free but someone has
  // text in its input box: it goes out once that box has been empty for a
  // few seconds, and the prompts behind it wait for it.
  const held = items.some((q) => q.held);
  el.innerHTML = `<div class="q-head">${items.length} queued — ` +
    (held ? "held: someone is typing in this agent's input box; sent once it is empty" : "sent when the agent is free") + `</div>` +
    items.map((q) => `<div class="q-item${held ? " q-held" : ""}"><span class="q-text" title="${escAttr(q.text)}">${esc(q.from)}: ${esc(q.text)}</span>` +
      `<button type="button" class="btn q-cancel" data-qid="${escAttr(q.id)}">Cancel</button></div>`).join("");
}

async function refreshQueued(force = false) {
  const id = state.selected;
  if (!id || queued.busy) return;
  if (!force && queued.id === id && Date.now() - queued.at < 4000) return;
  queued.busy = true;
  try {
    const r = await api(`/api/queue?agent=${encodeURIComponent(id)}`);
    if (id === state.selected) { queued.id = id; queued.items = (r.queue || []).filter((q) => !queued.gone.has(q.id)); }
  } catch {
    if (id === state.selected) { queued.id = id; queued.items = []; } // no hub or no queue: nothing to show
  } finally {
    queued.busy = false;
    queued.at = Date.now();
    renderQueued();
  }
}

document.addEventListener("click", async (ev) => {
  const b = ev.target.closest && ev.target.closest("#queued .q-cancel");
  if (!b) return;
  const qid = b.dataset.qid;
  if (queued.gone.has(qid)) return;
  // The prompt leaves the list now; a refresh that still has it (it was
  // fetched before the DELETE) must not bring it back.
  queued.gone.add(qid);
  queued.items = queued.items.filter((q) => q.id !== qid);
  renderQueued();
  try {
    await api(`/api/queue/${encodeURIComponent(qid)}`, { method: "DELETE" });
    setTimeout(() => queued.gone.delete(qid), SETTLE_MS);
  } catch (e) {
    queued.gone.delete(qid);
    toast("could not cancel: " + e.message, true);
  }
  refreshQueued(true);
});

/* ---------- chat view (Pi session transcript) ---------- */

async function loadChat(reset = true) {
  if (!state.selected) return;
  const pane = state.selected;
  if (reset) {
    state.chat = { rendered: new Set(), skip: 0, more: false, terminal: false, pending: [], from: "", running: [] };
    $("chat").innerHTML = "";
  }
  const chat = state.chat;
  try {
    // Polls ask only for what can have changed (?from=, see renderTranscript),
    // and the browser revalidates by ETag, so an unchanged transcript costs
    // a bodiless 304. The newest window is always skip=0: "Load earlier"
    // pages are fetched separately.
    const from = !reset && chat.from ? `&from=${encodeURIComponent(chat.from)}` : "";
    const t = await api(`${pathFor(pane)}/transcript?limit=100${from}`);
    if (state.selected !== pane || !state.chat) return;
    renderTranscript(t, reset);
  } catch (e) {
    if (e.status === 404) {
      // No Pi session (or it is gone): fall back to raw pane output.
      chat.terminal = true;
      showTerminal();
      refreshOutput();
    } else {
      toast("transcript failed: " + e.message, true);
    }
  }
}

function showTerminal() {
  $("chat").style.display = "none";
  $("btn-terminal").style.display = "none";
  $("output").style.display = "block";
}

function renderTranscript(t, reset) {
  const chatEl = $("chat");
  const chat = state.chat;
  const atBottom = chatEl.scrollHeight - chatEl.scrollTop - chatEl.clientHeight < 80;

  if (reset) chatEl.innerHTML = "";
  // More describes the newest window; once earlier pages are loaded it is
  // loadEarlier's to track. A partial response carries no new information.
  if (!t.partial && chat.skip === 0) chat.more = t.more;
  state.ask = t.ask || null;
  updateAnswerCard();
  updateSendButton(); // an open question turns Stop back into Send

  // "Load earlier" affordance sits at the top of the transcript.
  let earlier = chatEl.querySelector(".load-earlier");
  if (chat.more && !earlier) {
    earlier = document.createElement("button");
    earlier.className = "load-earlier";
    earlier.textContent = "Load earlier";
    earlier.addEventListener("click", loadEarlier);
    chatEl.prepend(earlier);
  }
  if (!chat.more && earlier) earlier.remove();

  let appended = false;
  for (const m of t.messages) {
    if (chat.rendered.has(m.id)) {
      // A tool card may have gained its result since it was rendered.
      if (m.kind === "tool") patchToolCard(m);
      continue;
    }
    chat.rendered.add(m.id);
    chatEl.appendChild(renderMessage(m));
    appended = true;
    if (m.kind === "user") settlePending(m.text);
  }
  renderPending();

  if (t.messages.length) {
    chat.last = t.messages[t.messages.length - 1];
    // The next poll starts at the oldest message that can still change (a
    // tool call waiting for its result, a subagent still running), else at
    // the newest one.
    chat.from = (t.messages.find(stillChanging) || chat.last).id;
  }
  // Running subagents are counted over the whole session, so even a
  // partial response carries the current list.
  chat.running = t.running_subagents || [];
  chat.thinking = t.thinking || null;
  renderActivity();
  ensureSubagentTicker();

  chatEl.appendChild(jumpAnchor()); // keep the jump button last
  if (appended && (atBottom || reset)) chatEl.scrollTop = chatEl.scrollHeight;
  updateJump();
}

let jumpEl = null;
function jumpAnchor() {
  if (!jumpEl) {
    jumpEl = document.createElement("div");
    jumpEl.className = "jump-anchor hidden";
    jumpEl.innerHTML = '<button class="jump-btn" type="button" aria-label="Jump to latest">&#8595; Latest</button>';
    jumpEl.firstChild.addEventListener("click", () => {
      const c = $("chat");
      c.scrollTo({ top: c.scrollHeight, behavior: "smooth" });
    });
    $("chat").addEventListener("scroll", updateJump, { passive: true });
  }
  return jumpEl;
}
function updateJump() {
  if (!jumpEl) return;
  const c = $("chat");
  jumpEl.classList.toggle("hidden", c.scrollHeight - c.scrollTop - c.clientHeight <= 120);
}

/* ---------- activity (the agent is working) ---------- */
//
// While the agent works: a row at the bottom of the chat with a pulsing
// dot, how long it has been working and what it is doing now (the tool it
// is running, else thinking), and a moving bar under the header that stays
// visible when the chat is scrolled up. Blocked agents get the answer
// card instead.

const activity = { timer: null, seenAt: 0, pane: null, open: false, panelTimer: null, panelTicks: 0, panelSig: "", subSteps: new Map(), status: null, statusPane: null };

function isWorking(a) { return !!a && a.agent_status === "working" && !state.ask; }

// activityStep describes what the agent is doing: the tool it is running
// (from the newest message), else what its own status line says
// ("Compacting conversation", the current todo), else thinking. The status
// line's placeholder verbs ("Shimmying…") count as thinking.
function activityStep() {
  const m = state.chat && state.chat.last;
  if (m && m.kind === "tool" && toolRunning(m)) {
    return { verb: "Running", what: m.tool || "a tool", detail: m.args || "" };
  }
  const st = agentStatusLine();
  if (st && !st.generic) return { verb: st.text, what: "", detail: "" };
  return { verb: "Thinking", what: "", detail: "" };
}

function agentStatusLine() {
  return activity.statusPane === state.selected ? activity.status : null;
}

// refreshStatusLine fetches the selected agent's status line (GET
// /activity) while it works. The poll calls it alongside the transcript.
async function refreshStatusLine() {
  const pane = state.selected;
  if (!isWorking(agentById(pane)) || !state.chat || state.chat.terminal) return;
  let st = null;
  try {
    st = (await api(`${pathFor(pane)}/activity`)).activity || null;
  } catch { /* an older host without /activity: keep the transcript's view */ }
  if (state.selected !== pane) return;
  activity.status = st;
  activity.statusPane = pane;
  renderActivity();
}

function workingSince(a) {
  const t = sinceMs(a);
  if (t) return t;
  // An older server without status_since: time from when we saw it.
  if (activity.pane !== state.selected || !activity.seenAt) {
    activity.pane = state.selected;
    activity.seenAt = Date.now();
  }
  return activity.seenAt;
}

// runningSubagents is how many of the session's subagents are still running.
function runningSubagents() {
  const chat = state.chat;
  return chat && !chat.terminal && chat.running ? chat.running.length : 0;
}

function subagentCount(n) { return n + " subagent" + (n === 1 ? "" : "s"); }

// renderActivity also shows background subagents: a line in the row while
// the agent works, and a compact row of its own while the agent is idle
// (or blocked) and they are still running.
function renderActivity() {
  const a = agentById(state.selected);
  const working = isWorking(a);
  const subs = runningSubagents();
  const header = document.querySelector("#detail header");
  if (header) header.classList.toggle("busy", working);
  const chatEl = $("chat");
  let row = chatEl.querySelector(".activity");
  if (!working) { activity.seenAt = 0; activity.status = null; clearInterval(activity.timer); activity.timer = null; }
  if (!state.chat || state.chat.terminal || (!working && !subs)) {
    if (row) row.remove();
    closeActivityPanel();
    return;
  }
  if (!row) {
    row = document.createElement("div");
    row.className = "activity";
    // Tapping the row opens a panel with the full current step, the
    // latest thinking and what each running subagent is doing.
    row.setAttribute("role", "button");
    row.tabIndex = 0;
    row.innerHTML = '<span class="dot working" aria-hidden="true"></span>' +
      '<span class="act-verb"></span><span class="act-what"></span>' +
      '<span class="act-time"></span><span class="act-chev" aria-hidden="true"></span>' +
      '<span class="act-detail"></span><span class="act-subs"></span>' +
      '<div class="act-panel" hidden></div>';
    row.addEventListener("click", (ev) => {
      if (ev.target.closest(".act-panel")) return;
      toggleActivityPanel();
    });
    row.addEventListener("keydown", (ev) => {
      if (ev.target !== row || (ev.key !== "Enter" && ev.key !== " ")) return;
      ev.preventDefault();
      toggleActivityPanel();
    });
  }
  const anchor = chatEl.querySelector(".jump-anchor");
  if (row.nextSibling !== anchor || row.parentNode !== chatEl) chatEl.insertBefore(row, anchor);
  row.classList.toggle("idle", !working);
  const step = working ? activityStep() : { verb: subagentCount(subs) + " running", what: "", detail: "" };
  row.querySelector(".act-verb").textContent = step.verb;
  row.querySelector(".act-what").textContent = step.what;
  row.querySelector(".act-detail").textContent = step.detail;
  row.querySelector(".act-detail").hidden = !step.detail;
  const subsEl = row.querySelector(".act-subs");
  subsEl.textContent = working && subs ? "Running " + subagentCount(subs) : "";
  subsEl.hidden = !(working && subs);
  row.setAttribute("aria-expanded", String(activity.open));
  row.setAttribute("aria-label", (working ? "Agent activity" : subagentCount(subs) + " running") + (activity.open ? ", tap to collapse" : ", tap for details"));
  row.classList.toggle("open", activity.open);
  renderActivityPanel(row, working);
  if (!working) { row.querySelector(".act-time").textContent = ""; return; }
  tickActivity();
  if (!activity.timer) activity.timer = setInterval(tickActivity, 1000);
}

/* The activity panel */

function toggleActivityPanel() {
  activity.open = !activity.open;
  activity.panelSig = "";
  if (activity.open) {
    activity.panelTicks = 0;
    clearInterval(activity.panelTimer);
    activity.panelTimer = setInterval(panelTick, 1000);
    fetchSubagentSteps();
  } else {
    clearInterval(activity.panelTimer);
    activity.panelTimer = null;
  }
  renderActivity();
}

function closeActivityPanel() {
  activity.open = false;
  activity.panelSig = "";
  activity.subSteps.clear();
  clearInterval(activity.panelTimer);
  activity.panelTimer = null;
}

// panelTick keeps the panel's subagent timers live and refreshes what each
// subagent is doing every few seconds (ETag-revalidated, so cheap).
function panelTick() {
  const row = $("chat").querySelector(".activity");
  if (!activity.open || !row) { closeActivityPanel(); return; }
  if (document.hidden) return;
  row.querySelectorAll(".ap-sub-time[data-since]").forEach((el) => {
    const t = Date.parse(el.dataset.since);
    el.textContent = Number.isNaN(t) ? "" : fmtClock(Date.now() - t);
  });
  if (++activity.panelTicks % 5 === 0) fetchSubagentSteps();
}

// describeStep is a one-line account of a transcript message.
function describeStep(m) {
  if (!m) return "";
  if (m.kind === "tool") {
    const verb = m.subagent ? (m.subagent.status === "running" ? "Running subagent" : "Ran subagent")
      : toolRunning(m) ? "Running" : m.err ? "Failed" : "Ran";
    return `${verb} ${m.tool || "tool"}${m.args ? " · " + m.args : ""}`;
  }
  if (m.kind === "assistant") return "Said: " + String(m.text || "").replace(/\s+/g, " ").trim();
  if (m.kind === "user") return "Started: " + String(m.text || "").replace(/\s+/g, " ").trim();
  return "";
}

async function fetchSubagentSteps() {
  const chat = state.chat;
  const pane = state.selected;
  if (!activity.open || !chat || !pane) return;
  await Promise.all((chat.running || []).filter((s) => s.agent_id).map(async (s) => {
    try {
      const t = await api(`${pathFor(pane)}/subagents/${encodeURIComponent(s.agent_id)}/transcript?limit=4`);
      const last = t.messages[t.messages.length - 1];
      const step = describeStep(last);
      const thinking = t.thinking && t.thinking.text ? t.thinking.text : "";
      activity.subSteps.set(s.agent_id, { step, thinking });
    } catch {} // the row keeps its last known step
  }));
  if (state.selected === pane && activity.open) renderActivity();
}

function renderActivityPanel(row, working) {
  const panel = row.querySelector(".act-panel");
  panel.hidden = !activity.open;
  if (!activity.open) return;
  const chat = state.chat;
  const subs = chat.running || [];
  const step = working ? activityStep() : null;
  const th = working ? chat.thinking : null;
  const sig = JSON.stringify([step, th, subs, [...activity.subSteps]]);
  if (sig === activity.panelSig) return;
  activity.panelSig = sig;
  let html = "";
  if (step) {
    html += `<div class="ap-sec"><div class="ap-h">Now</div><div class="ap-now"><b>${esc(step.verb)}</b>${step.what ? " " + esc(step.what) : ""}</div>` +
      (step.detail ? `<pre class="ap-args">${esc(step.detail)}</pre>` : "") + "</div>";
    html += '<div class="ap-sec"><div class="ap-h">Thinking' +
      (th && th.time ? ` <span class="ap-ago">${esc(fmtAgo(Date.parse(th.time)))} ago</span>` : "") + "</div>" +
      (th ? `<div class="ap-think md">${renderMarkdown(th.text, "live-thinking")}</div>`
        : '<div class="ap-empty">No thinking recorded since your last message.</div>') + "</div>";
  }
  if (subs.length) {
    html += `<div class="ap-sec"><div class="ap-h">${esc(subagentCount(subs.length))} running</div>` + subs.map((s) => {
      const info = activity.subSteps.get(s.agent_id) || {};
      return '<div class="ap-sub">' +
        `<div class="ap-sub-top"><span class="ap-sub-name">${esc(s.description || "Subagent")}</span>` +
        `<span class="ap-sub-time" data-since="${esc(s.since || "")}"></span></div>` +
        `<div class="ap-sub-step">${esc(info.step || (s.agent_id ? "…" : "steps unavailable"))}</div>` +
        (info.thinking ? `<div class="ap-sub-think">${esc(info.thinking.replace(/\s+/g, " ").slice(-240))}</div>` : "") +
        (s.agent_id ? `<button type="button" class="btn ap-steps" data-agent="${esc(s.agent_id)}">View steps</button>` : "") +
        "</div>";
    }).join("") + "</div>";
  }
  panel.innerHTML = html;
  panel.querySelectorAll(".ap-steps").forEach((b) => b.addEventListener("click", () => {
    const s = subs.find((x) => x.agent_id === b.dataset.agent);
    if (s) openSubviewInfo({ agent_id: s.agent_id, description: s.description, status: "running" });
  }));
  panelTick();
}

function tickActivity() {
  const a = agentById(state.selected);
  const row = $("chat").querySelector(".activity");
  if (!row || !isWorking(a)) return;
  const st = agentStatusLine();
  row.querySelector(".act-time").textContent = fmtClock(Date.now() - workingSince(a)) + (st && st.detail ? " · " + st.detail : "");
}

// toolMeta is a tool card's status: duration, "failed" for a call whose
// result is an error (red card), or still running.
function toolMeta(m) {
  if (m.err) return "failed" + (m.ms ? " · " + fmtMs(m.ms) : "");
  return m.ms ? fmtMs(m.ms) : (m.result ? "ran" : "running…");
}

// toolRunning: a call still waiting for its result.
function toolRunning(m) { return !m.result && !m.err && !m.ms; }

// toolResultText is a tool card's body: empty (hidden) while the call runs,
// so a result that arrives later is not mistaken for a final "(no output)".
function toolResultText(m) {
  return m.result || (toolRunning(m) ? "" : "(no output)");
}

// stillChanging: a message a later poll may update.
function stillChanging(m) {
  if (m.kind !== "tool") return false;
  return m.subagent ? m.subagent.status === "running" : toolRunning(m);
}

function patchToolCard(m, root = $("chat")) {
  const el = root.querySelector(`[data-id="${CSS.escape(m.id)}"]`);
  if (!el) return;
  if (el.classList.contains("subagent")) { patchSubagentCard(el, m); return; }
  // !!: the server omits err when false, and toggle(name, undefined) flips
  // the class instead of removing it, so cards blinked red on every poll.
  el.classList.toggle("err", !!m.err);
  const meta = el.querySelector(".toolmeta");
  if (meta) meta.textContent = toolMeta(m);
  const body = el.querySelector(".toolresult");
  const text = toolResultText(m);
  if (body && body.textContent !== text) body.textContent = text;
}

/* ---------- subagent cards (Claude's Agent/Task calls) ---------- */
//
// A subagent call gets its own card: what it is for (its description), its
// type, whether it runs in the background or its own worktree, and its
// status, ticking while it runs. Opened, it shows the agent's report as
// markdown and the prompt it was given, and "View steps" opens the
// subagent's own transcript.

function isSubagentCard(m) {
  return m.kind === "tool" && (!!m.subagent || m.tool === "Agent" || m.tool === "Task");
}

// subagentInfo is the server's description, or one made up from a plain
// tool card (an agent the server does not parse as one, e.g. Pi's).
function subagentInfo(m) {
  if (m.subagent) return m.subagent;
  return {
    description: m.args,
    status: toolRunning(m) ? "running" : m.err ? "failed" : "completed",
    report: m.err ? "" : m.result,
  };
}

function subagentMeta(m) {
  const s = subagentInfo(m);
  if (s.status === "running") {
    const t = m.time ? Date.parse(m.time) : NaN;
    return "running" + (Number.isNaN(t) ? "…" : " · " + fmtClock(Date.now() - t));
  }
  const parts = [s.status === "completed" ? "done" : s.status];
  if (s.dur_ms) parts.push(fmtMs(s.dur_ms));
  if (s.tool_uses) parts.push(s.tool_uses + " tool" + (s.tool_uses === 1 ? "" : "s"));
  return parts.join(" · ");
}

function renderSubagentCard(m) {
  const d = document.createElement("details");
  d.className = "toolcard subagent";
  d.dataset.id = m.id;
  d.innerHTML =
    '<summary><span class="toolname"></span><span class="sa-flags"></span>' +
    '<span class="toolargs"></span><span class="toolmeta"></span></summary>' +
    '<div class="sa-body"><div class="sa-report"></div>' +
    '<button type="button" class="sa-steps" hidden>View steps</button>' +
    '<details class="sa-prompt"><summary>Prompt</summary><div class="sa-prompt-text"></div></details>' +
    '<div class="toolresult"></div></div>';
  // The report is rendered when the card is first opened (most never are).
  d.addEventListener("toggle", () => { if (d.open) fillSubagentReport(d); });
  patchSubagentCard(d, m);
  return d;
}

function patchSubagentCard(el, m) {
  const s = subagentInfo(m);
  el._m = m; // for the ticker and "View steps"
  const running = s.status === "running";
  el.classList.toggle("running", running);
  el.classList.toggle("err", !!m.err || s.status === "failed" || s.status === "killed");
  el.querySelector(".toolname").textContent = s.type || m.tool || "agent";
  el.querySelector(".toolargs").textContent = s.description || m.args || "";
  const flags = [];
  if (s.background) flags.push("bg");
  if (s.worktree) flags.push("⑂ worktree");
  el.querySelector(".sa-flags").textContent = flags.join(" · ");
  el.querySelector(".toolmeta").textContent = subagentMeta(m);
  el.querySelector(".sa-steps").hidden = !s.agent_id;
  const prompt = el.querySelector(".sa-prompt");
  prompt.hidden = !s.prompt;
  const promptText = prompt.querySelector(".sa-prompt-text");
  if (promptText.textContent !== (s.prompt || "")) promptText.textContent = s.prompt || "";
  // An errored call has no report, just the error.
  const result = el.querySelector(".toolresult");
  const errText = m.err ? m.result || "" : "";
  if (result.textContent !== errText) result.textContent = errText;
  const report = s.report || "";
  if (el._report !== report || el._running !== running) {
    el._report = report;
    el._running = running;
    el.querySelector(".sa-report").dataset.md = "";
    if (el.open) fillSubagentReport(el);
  }
  for (const e of subview.stack) {
    if (e.id && e.id === s.agent_id) { e.info = s; if (e === subviewTop()) renderSubviewHeader(); }
  }
}

function fillSubagentReport(el) {
  const target = el.querySelector(".sa-report");
  if (target.dataset.md) return;
  target.dataset.md = "1";
  const report = el._report;
  target.classList.toggle("md", !!report);
  target.classList.toggle("sa-none", !report);
  if (report) target.innerHTML = renderMarkdown(report, "sa:" + el.dataset.id);
  else target.textContent = el._running ? "Working… its report shows here when it finishes." : el._m && el._m.err ? "" : "No report.";
}

// A running subagent's clock ticks once a second while any is on screen.
let subagentTimer = null;
function ensureSubagentTicker() {
  if (subagentTimer || !document.querySelector(".toolcard.subagent.running")) return;
  subagentTimer = setInterval(() => {
    if (document.hidden) return;
    const cards = document.querySelectorAll(".toolcard.subagent.running");
    if (!cards.length) { clearInterval(subagentTimer); subagentTimer = null; return; }
    for (const el of cards) if (el._m) el.querySelector(".toolmeta").textContent = subagentMeta(el._m);
  }, 1000);
}

/* ---------- subagent steps (a subagent's own transcript) ---------- */
//
// "View steps" covers the chat with the subagent's transcript, polled while
// the subagent runs. Each one opened pushes a history entry, so Back (the
// button or the phone's gesture) returns to the chat or the parent agent.

const subview = {
  stack: [],     // [{ id: agentId, info: SubagentInfo }], innermost last
  rendered: null,
  from: "",
  timer: null,
  seq: 0,        // bumped per view, so a slow answer for an old one is dropped
};

function subviewTop() { return subview.stack[subview.stack.length - 1] || null; }

function openSubview(m) { openSubviewInfo(subagentInfo(m)); }

function openSubviewInfo(s) {
  if (!s.agent_id || !state.selected) return;
  subview.stack.push({ id: s.agent_id, info: s });
  history.pushState({ subagent: s.agent_id, depth: subview.stack.length }, "", location.href);
  showSubview();
}

function showSubview() {
  const top = subviewTop();
  if (!top) { hideSubview(); return; }
  subview.seq++;
  subview.rendered = new Set();
  subview.from = "";
  $("sv-chat").innerHTML = "";
  $("subview").hidden = false;
  renderSubviewHeader();
  loadSubview(true);
  clearInterval(subview.timer);
  subview.timer = setInterval(() => {
    const t = subviewTop();
    if (!document.hidden && t && t.info.status === "running") loadSubview(false);
  }, 4000);
}

function hideSubview() {
  subview.stack = [];
  subview.seq++;
  clearInterval(subview.timer);
  subview.timer = null;
  $("subview").hidden = true;
  $("sv-chat").innerHTML = "";
}

// syncSubview follows the history: popping an entry closes its view.
function syncSubview() {
  const st = history.state;
  const depth = st && st.subagent ? st.depth || 0 : 0;
  if (subview.stack.length <= depth) return;
  subview.stack.length = depth;
  if (depth) showSubview(); else hideSubview();
}

function renderSubviewHeader() {
  const top = subviewTop();
  if (!top) return;
  const s = top.info;
  $("sv-title").textContent = s.description || "Subagent";
  const status = s.status === "completed" ? "done" : s.status || "";
  $("sv-subtitle").textContent = [s.type, status].filter(Boolean).join(" · ");
}

async function loadSubview(reset) {
  const top = subviewTop();
  const pane = state.selected;
  if (!top || !pane) return;
  const seq = subview.seq;
  const el = $("sv-chat");
  try {
    const from = !reset && subview.from ? `&from=${encodeURIComponent(subview.from)}` : "";
    const t = await api(`${pathFor(pane)}/subagents/${encodeURIComponent(top.id)}/transcript?limit=100${from}`);
    if (seq !== subview.seq) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (reset && t.more) {
      const n = document.createElement("div");
      n.className = "note";
      n.textContent = "Earlier steps are not shown";
      el.appendChild(n);
    }
    let appended = false;
    for (const m of t.messages) {
      if (subview.rendered.has(m.id)) {
        if (m.kind === "tool") patchToolCard(m, el);
        continue;
      }
      subview.rendered.add(m.id);
      el.appendChild(renderMessage(m));
      appended = true;
    }
    if (t.messages.length) subview.from = (t.messages.find(stillChanging) || t.messages[t.messages.length - 1]).id;
    if (appended && (atBottom || reset)) el.scrollTop = el.scrollHeight;
    ensureSubagentTicker();
  } catch (e) {
    if (seq !== subview.seq || !reset) return;
    const n = document.createElement("div");
    n.className = "note";
    n.textContent = e.status === 404 ? "This subagent's steps are not available." : "Could not load the steps: " + e.message;
    el.appendChild(n);
  }
}

/* ---------- sent-but-not-yet-seen prompts ---------- */
//
// A prompt sent while the agent works is queued by the agent and reaches
// the transcript only when it is taken up (Claude: at the next step; Pi:
// as a steering message). Until then it is shown as a "queued" bubble, so
// it never looks lost; it settles when a user message with the same text
// appears.

const normText = (s) => String(s || "").replace(/\s+/g, " ").trim();

function addPending(text) {
  if (!state.chat || state.chat.terminal) return null;
  const p = { text, at: Date.now() };
  state.chat.pending.push(p);
  renderPending();
  const c = $("chat");
  c.scrollTop = c.scrollHeight;
  return p;
}

function dropPending(p) {
  if (!state.chat || !p) return;
  state.chat.pending = state.chat.pending.filter((x) => x !== p);
  renderPending();
}

function settlePending(text) {
  const chat = state.chat;
  if (!chat || !chat.pending.length) return;
  const t = normText(text);
  const i = chat.pending.findIndex((p) => normText(p.text) === t);
  if (i >= 0) chat.pending.splice(i, 1);
}

function renderPending() {
  const chatEl = $("chat");
  chatEl.querySelectorAll(".msg.pending").forEach((el) => el.remove());
  const chat = state.chat;
  if (!chat || chat.terminal) return;
  const a = agentById(state.selected);
  const working = a && (a.agent_status === "working" || a.agent_status === "blocked");
  // Once the agent is idle, anything still unmatched after a minute was
  // rendered differently (a slash command, say) or not recorded: drop it.
  if (!working) chat.pending = chat.pending.filter((p) => Date.now() - p.at < 60000);
  // Keep them above the activity row and the jump button.
  const before = chatEl.querySelector(".activity") || chatEl.querySelector(".jump-anchor");
  for (const p of chat.pending) {
    const div = document.createElement("div");
    div.className = "msg user pending";
    div.innerHTML = `<div class="bubble">${esc(p.text)}</div>` +
      `<div class="msgtime">${p.sending ? "sending…" : working ? "queued — the agent picks it up at its next step" : "sent"}</div>`;
    chatEl.insertBefore(div, before);
  }
}

function renderMessage(m) {
  switch (m.kind) {
    case "user": {
      const div = document.createElement("div");
      div.className = "msg user";
      div.innerHTML = `<div class="bubble">${esc(m.text)}</div>` +
        (m.time ? `<div class="msgtime">${esc(fmtTime(m.time))}</div>` : "");
      return div;
    }
    case "assistant": {
      const wrap = document.createDocumentFragment();
      if (m.think) {
        const d = document.createElement("details");
        d.className = "thinking";
        d.innerHTML = `<summary>thought process</summary><div class="think-body md"></div>`;
        mdOnOpen(d, d.querySelector(".think-body"), m.think, "t:" + m.id);
        wrap.appendChild(d);
      }
      const div = document.createElement("div");
      div.className = "msg assistant";
      div.innerHTML = `<div class="bubble md">${renderMarkdown(m.text, "a:" + m.id)}</div>` +
        (m.time ? `<div class="msgtime">${esc(fmtTime(m.time))}${m.model ? " · " + esc(m.model) : ""}</div>` : "");
      wrap.appendChild(div);
      return wrap;
    }
    case "tool": {
      if (isSubagentCard(m)) return renderSubagentCard(m);
      const d = document.createElement("details");
      d.className = "toolcard" + (m.err ? " err" : "");
      d.dataset.id = m.id;
      const meta = toolMeta(m);
      d.innerHTML =
        `<summary><span class="toolname">${esc(m.tool || "tool")}</span>` +
        `<span class="toolargs">${esc(m.args || "")}</span>` +
        `<span class="toolmeta">${esc(meta)}</span></summary>` +
        `<div class="toolresult">${esc(toolResultText(m))}</div>`;
      return d;
    }
    case "note": {
      const div = document.createElement("div");
      div.className = "note";
      div.textContent = m.note;
      return div;
    }
    case "compaction": {
      const d = document.createElement("details");
      d.className = "compaction";
      d.innerHTML = `<summary class="note">✂ context compacted — tap for summary</summary>` +
        `<div class="note md"></div>`;
      mdOnOpen(d, d.querySelector("div.note"), m.note, "c:" + m.id);
      return d;
    }
    default:
      return document.createTextNode("");
  }
}

async function loadEarlier() {
  const chat = state.chat;
  if (!chat || !chat.more) return;
  chat.skip += 100;
  const chatEl = $("chat");
  const prevHeight = chatEl.scrollHeight;
  try {
    const t = await api(`${pathFor(state.selected)}/transcript?limit=100&skip=${chat.skip}`);
    // Prepend older messages without disturbing what is already rendered.
    const frag = document.createDocumentFragment();
    for (const m of t.messages) {
      if (chat.rendered.has(m.id)) continue;
      chat.rendered.add(m.id);
      frag.appendChild(renderMessage(m));
    }
    const earlier = chatEl.querySelector(".load-earlier");
    chatEl.insertBefore(frag, earlier || jumpEl);
    chat.more = t.more;
    if (!t.more && earlier) earlier.remove();
    chatEl.scrollTop += chatEl.scrollHeight - prevHeight; // keep viewport stable
  } catch (e) {
    chat.skip -= 100;
    toast("could not load earlier messages", true);
  }
}

/* ---------- raw terminal fallback ---------- */

let outputBusy = false;
let outputAbort = null; // cancels the in-flight read when the view changes agent

// resetTerminalView puts the detail pane back to its initial look and drops
// any read in flight. Without it, an agent opened after one that fell back
// to the raw view inherited the hidden chat and the previous agent's text.
function resetTerminalView() {
  if (outputAbort) outputAbort.abort();
  outputAbort = null;
  outputBusy = false;
  $("output").textContent = "";
  $("output").style.display = "none";
  $("chat").style.display = "";
  $("btn-terminal").style.display = "none";
}

async function refreshOutput(force) {
  if (!state.selected || !(state.chat && state.chat.terminal)) return;
  if (outputBusy && force === true && outputAbort) outputAbort.abort(), outputBusy = false; // a post-send read must not be skipped behind a hung one
  if (outputBusy) return; // the 4s poll must not stack reads behind a slow one
  outputBusy = true;
  const pane = state.selected;
  const ctl = outputAbort = new AbortController();
  try {
    const read = await api(`${pathFor(pane)}/output?lines=400`, { signal: ctl.signal, cache: "no-store" });
    // The user may have switched agents while this was in flight.
    if (state.selected !== pane || !state.chat || !state.chat.terminal) return;
    const el = $("output");
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    el.textContent = read.text || "(no output)";
    setAutoKeys(screenWantsKeys(read.text));
    if (atBottom) el.scrollTop = el.scrollHeight;
  } catch (e) {
    if (e.aborted || state.selected !== pane) return;
    if (e.status === 404) { toast("agent is gone", true); closeDetail(); }
    else toast("read failed: " + e.message, true);
  } finally {
    if (outputAbort === ctl) { outputAbort = null; outputBusy = false; }
  }
}

function startPolling() {
  stopPolling();
  // Poll while the agent is active. Status arrives over SSE; transcript
  // content and pane output have no push, so refresh on a timer.
  state.pollTimer = setInterval(() => {
    if (document.hidden) return; // resume() refreshes when visible again
    const a = agentById(state.selected);
    if (!a) return;
    // The status snapshot can lag a send (or SSE can be down), so also poll
    // for a while after anything is sent; the reply must not need a re-select.
    if (a.agent_status === "working" || a.agent_status === "blocked" || Date.now() < state.pollUntil) {
      if (state.chat && state.chat.terminal) refreshOutput();
      else { loadChat(false); refreshStatusLine(); } // append-only; new messages diff in by id
    } else if (runningSubagents()) {
      loadChat(false); // an idle agent's background subagents finishing
    }
  }, 4000);
}

function stopPolling() {
  clearInterval(state.pollTimer);
  state.pollTimer = null;
}

/* ---------- "needs your answer" card ---------- */
//
// Shown while the agent waits on a dialog: herdr reports `blocked` (Pi's
// permission gate, Claude Code's permission prompt, other hook- or
// screen-reported prompts), or the Pi transcript has an unanswered
// ask_user_question call (that dialog does not flip herdr to `blocked`, so
// the status alone would miss it).
//
// The server recognizes the dialog (GET /dialog: question, options, which
// row is highlighted, a content fingerprint) and the card shows one big
// button per option. A tap POSTs /answer with the fingerprint; the server
// re-reads the screen and refuses with 409 if the dialog changed, else
// sends the keys computed from the current highlight. "More keys…" keeps
// the raw key pad and a live screen snapshot as the fallback for dialogs
// it does not recognize. Nothing here ever sends a key without a tap.

// Pad layout: [label, key]. Keys must be in the server whitelist.
const PAD_KEYS = [
  ["↑", "up"], ["↓", "down"], ["←", "left"], ["→", "right"],
  ["Tab", "tab"], ["Space", "space"], ["Esc", "esc"], ["Enter", "enter"],
  ["1", "1"], ["2", "2"], ["3", "3"], ["4", "4"],
];

const answer = {
  timer: null,     // dialog/snapshot poll while the card is shown
  shown: false,
  sig: "",         // last rendered card signature
  pane: null,      // pane the dialog below belongs to
  dialog: null,    // last GET /dialog result (null: none recognized)
  loaded: false,   // a /dialog response has arrived for this pane
  inflight: false, // taps are being sent (answer.queue is not empty)
  queue: [],       // taps already shown on the card, sent one at a time: {action, index, label, want}
  server: null,    // the last dialog the server reported (what a failed tap rolls back to)
  hold: null,      // after a sent tap: {fp, until}: refreshes still showing that dialog are stale
  err: "",         // why the last tap failed (shown on the card until the next tap)
  moreOpen: false, // "More keys…" disclosure state (kept across re-renders)
  snapText: "loading screen…",
};

function needsAnswer(a) {
  if (!a) return false;
  if (a.agent_status === "blocked") return true;
  // Claude's AskUserQuestion does not reliably flip herdr to "blocked" (a
  // finished-looking coordinator sat on a multi-select), so an open question
  // counts whatever the status. A stale one (nothing on screen) is hidden by
  // staleQuestion() once the dialog read comes back empty.
  return !!state.ask;
}

// staleQuestion: the transcript says a question is open, the agent is not
// working or blocked, and no dialog is on its screen (the session was
// killed mid-question). Nothing to answer; the card stays hidden.
function staleQuestion(a) {
  return answer.loaded && !(answer.dialog && answer.dialog.options && answer.dialog.options.length) &&
    a.agent_status !== "blocked" && a.agent_status !== "working";
}

function updateAnswerCard() {
  const el = $("answer");
  const a = agentById(state.selected);
  if (!needsAnswer(a)) {
    if (answer.shown) { el.style.display = "none"; el.innerHTML = ""; }
    answer.shown = false;
    answer.sig = "";
    answer.dialog = null;
    answer.loaded = false;
    answer.pane = null;
    answer.queue = []; answer.inflight = false; answer.hold = null; answer.err = ""; answer.server = null;
    stopAnswerPoll();
    return;
  }
  if (answer.pane !== state.selected) {
    answer.pane = state.selected;
    answer.dialog = null;
    answer.loaded = false;
    answer.queue = []; answer.inflight = false; answer.hold = null; answer.err = ""; answer.server = null;
    answer.moreOpen = false;
    answer.snapText = "loading screen…";
  }
  renderAnswerCard(a);
  el.style.display = staleQuestion(a) ? "none" : "block";
  answer.shown = true;
  startAnswerPoll();
}

// renderAnswerCard re-renders only when what it shows changed, so focus and
// the disclosure state survive SSE updates and polls.
function renderAnswerCard(a) {
  const el = $("answer");
  const d = answer.dialog;
  const sig = JSON.stringify([answer.loaded, d && d.fingerprint, d && d.selected, d && d.insert, d && d.options && d.options.map((o) => (o.checked ? 1 : 0)).join(""),
    answer.queue.map((q) => q.action + q.index), answer.hold && answer.hold.index, answer.err, a.agent_status, a.state_labels, state.ask && state.ask.tool_call_id]);
  if (answer.shown && sig === answer.sig) return;
  answer.sig = sig;
  el.innerHTML = answerCardHTML(a, d);
  el.querySelectorAll(".pad .btn").forEach((b) =>
    b.addEventListener("click", () => padPress(b.dataset.key, b.textContent, b)));
  el.querySelectorAll("button.opt").forEach((b) =>
    b.addEventListener("click", () => tapAnswer(b.dataset.action, Number(b.dataset.i))));
  const more = el.querySelector("details.ans-more");
  if (more) more.addEventListener("toggle", () => {
    answer.moreOpen = more.open;
    if (more.open) refreshSnapshot();
  });
}

function answerCardHTML(a, d) {
  let html = '<div class="ans-head">Needs your answer</div>';
  const usable = d && d.options && d.options.length;
  if (usable) {
    if (d.header) html += `<span class="ans-chip">${esc(d.header)}</span>`;
    if (d.title) html += `<div class="ans-title">${esc(d.title)}</div>`;
    if (d.detail) html += `<pre class="ans-detail">${esc(d.detail)}</pre>`;
    if (d.question) html += `<div class="ans-q">${esc(d.question)}</div>`;
    const h = dialogHints(d);
    if (h.set) html += `<div class="ans-hint">${esc(h.set)}</div>`;
    html += '<div class="ans-opts">';
    // Taps already made show as chosen at once (pend: the first one still in
    // flight); while a single-shot answer is out, the other options hold still.
    const oneShot = answer.queue.some((q) => q.action !== "toggle") || (answer.hold && answer.hold.action !== "toggle" && answer.hold.fp === d.fingerprint);
    const stateOf = (i, action) => {
      const k = answer.queue.findIndex((q) => q.index === i && (q.action === "toggle") === (action === "toggle"));
      const held = answer.hold && answer.hold.fp === d.fingerprint && answer.hold.index === i && action !== "toggle";
      return (k >= 0 || held ? " chosen" : "") + (k === 0 ? " pend" : "") + (oneShot ? " locked" : "");
    };
    d.options.forEach((o, i) => {
      const cur = i === d.selected ? " cur" : "";
      const desc = o.description ? `<span class="opt-desc">${esc(o.description)}</span>` : "";
      if (o.role === "other") {
        html += `<div class="opt-note">✎ ${esc(o.label)} <span class="opt-desc">${esc(h.other)}</span></div>`;
        return;
      }
      if (o.role === "chat") {
        html += `<div class="opt-note">💬 ${esc(o.label)} <span class="opt-desc">${esc(h.chat)}</span></div>`;
        return;
      }
      if (!d.answerable || d.insert) {
        html += `<div class="opt ro${cur}"><span class="opt-label">${esc(o.label)}</span>${desc}</div>`;
        return;
      }
      if (o.role === "next") {
        // Zero ticks is a valid answer: say so, so "Next" is never a mystery.
        const none = !d.options.some((x) => x.checked);
        const word = o.label === "Submit" ? "Submit" : "Next";
        html += `<button type="button" class="opt submit${cur}${stateOf(i, "submit")}" data-action="submit" data-i="${i}"><span class="opt-label">${word}${none ? " with none selected" : ""}</span></button>`;
        return;
      }
      const box = d.multi ? `<span class="opt-box">${o.checked ? "☑" : "☐"}</span>` : "";
      html += `<button type="button" class="opt${cur}${o.checked ? " checked" : ""}${stateOf(i, d.multi ? "toggle" : "select")}" data-action="${d.multi ? "toggle" : "select"}" data-i="${i}"` +
        `${i === d.selected ? ' aria-current="true"' : ""}>${box}<span class="opt-label">${esc(o.label)}</span>${desc}</button>`;
    });
    html += "</div>";
    if (d.insert) {
      // A text field in vim INSERT mode swallows arrows and Enter. One Esc
      // leaves it; a second would cancel the whole question, so only this
      // button sends it, and only while the field is focused.
      html += '<div class="ans-hint">A text field is focused (INSERT mode), so the options cannot be used yet.</div>' +
        `<button type="button" class="opt submit${stateOf(-1, "reset")}" data-action="reset" data-i="-1"><span class="opt-label">Leave the text field (one Esc)</span></button>`;
    } else if (d.cancel_label) {
      html += `<button type="button" class="opt cancel${stateOf(-1, "cancel")}" data-action="cancel" data-i="-1">${esc(h.cancel)}</button>`;
    }
    if (answer.err) html += `<div class="ans-err" role="alert">${esc(answer.err)}</div>`;
    if (h.after) html += `<div class="ans-hint">${esc(h.after)}</div>`;
  } else if (!answer.loaded) {
    html += '<div class="ans-hint">Reading the dialog…</div>';
  } else {
    const label = (a.state_labels && a.state_labels.blocked) || "";
    html += `<div class="ans-hint">${label ? esc(label) + " — " : ""}the agent is waiting on a dialog this app doesn't recognize. Use the keys below; the screen shows the current highlight.</div>`;
  }
  // The key pad and snapshot: primary when nothing usable was recognized,
  // otherwise tucked under "More keys…".
  const open = !usable ? answer.loaded : (answer.moreOpen || !d.answerable);
  html += `<details class="ans-more"${open ? " open" : ""}><summary>More keys…</summary>`;
  html += `<div class="ans-hint">${esc(dialogHints(usable ? d : null).pad)}</div>`;
  html += '<div class="pad">' + PAD_KEYS.map(([label, key]) =>
    `<button type="button" class="btn${key === "esc" ? " warn" : ""}" data-key="${esc(key)}">${esc(label)}</button>`).join("") + "</div>";
  html += `<pre class="snap" id="answer-snap">${esc(answer.snapText)}</pre></details>`;
  return html;
}

// dialogHints words the card's help text for the dialog at hand: Pi's
// questionnaire, permission gate and selector, Claude Code's permission
// prompt and AskUserQuestion, or anything else. Where a dialog's keys are
// not pinned down (Claude's multi-question navigation, unknown agents) the
// wording stays neutral rather than guessing.
function dialogHints(d) {
  const kind = d ? d.kind : "";
  const agent = d ? d.agent || "" : "";
  const pi = agent === "pi" || kind.startsWith("pi_");
  const claude = agent === "claude" || kind.startsWith("claude_");
  const h = {
    set: "",
    other: "Free-text answer: type it at the desk, or use More keys….",
    cancel: "Esc · Cancel",
    chat: "",
    after: "",
    pad: "Use the keys below and check the screen: it shows the current highlight.",
  };
  if (!d) return h;
  if (d.questions > 1) {
    h.set = pi
      ? `One of ${d.questions} questions: answering moves to the next one, then a review to submit.`
      : `One of ${d.questions} questions: tap an option here; use More keys… for anything else.`;
  }
  switch (kind) {
    case "pi_gate":
      h.cancel = "Esc · Block";
      h.pad = "↑/↓ choose, Enter selects, Esc blocks.";
      break;
    case "pi_select":
      h.pad = "↑/↓ choose, Enter selects, Esc cancels.";
      break;
    case "pi_question":
      h.cancel = "Esc · Cancel the question";
      h.pad = "↑/↓ move (wraps), Enter picks, Space ticks in multi-select, Tab switches questions, Esc cancels.";
      break;
    case "claude_permission":
      h.cancel = "Esc · Tell Claude what to do instead";
      h.pad = "Press an option's number, or ↑/↓ then Enter; Esc cancels.";
      break;
    case "claude_question":
      h.cancel = d.multi ? "Esc · Cancel the question (discards answers on every tab)" : "Esc · Cancel";
      h.chat = "ends the question to talk it over; not an answer";
      h.pad = "↑/↓ move, Space ticks, Enter on Next continues; Esc cancels. Check the screen below.";
      break;
    case "confirm":
      h.pad = "Answer with the buttons above; the keys below move or interrupt.";
      break;
    case "ask":
      h.pad = pi
        ? "↑/↓ move (wraps), Enter picks, Space ticks in multi-select, Tab switches questions, Esc cancels."
        : "Check the screen below for the highlight, then use ↑/↓ and Enter; Esc cancels.";
      break;
  }
  if (!d.answerable) {
    h.after = "Tap-to-answer isn't available for this dialog: use More keys… and check the screen.";
  } else if (d.multi) {
    h.after = "Tap options to tick them, then Submit selection.";
  } else if (claude && kind === "claude_question") {
    h.after = "Tap an option; use More keys… for anything else.";
  }
  return h;
}

const KEY_GLYPH = { up: "↑", down: "↓", enter: "Enter", esc: "Esc", space: "Space" };
const ANSWER_HOLD_MS = 3500; // how long a refresh that still shows the old dialog is taken as stale

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// tapAnswer: the tap shows on the card in this frame (a toggled box flips, a
// chosen option takes the chosen look); the request goes out behind it. Taps
// are sent one at a time, each quoting the dialog as the server last showed it,
// so ticking several boxes quickly all land. A single-shot answer (select,
// submit, cancel) locks the card until it is confirmed or rolled back.
function tapAnswer(action, index) {
  const d = answer.dialog;
  const pane = state.selected;
  if (!d || !pane) return;
  if (answer.queue.some((q) => q.action !== "toggle")) return; // an answer is already on its way
  const h = answer.hold;
  if (h && h.action !== "toggle" && h.fp === d.fingerprint && Date.now() < h.until) return; // sent; waiting for the screen
  const o = d.options && d.options[index];
  const op = { action, index, label: o ? o.label : "", want: null };
  if (action === "toggle") {
    if (!o) return;
    o.checked = !o.checked;
    op.want = !!o.checked;
  }
  answer.err = "";
  answer.hold = null;
  answer.queue.push(op);
  answer.inflight = true;
  const a = agentById(pane);
  if (a) renderAnswerCard(a);
  pumpAnswers(pane);
}

async function postAnswer(pane, op, fingerprint) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), API_TIMEOUT_MS);
  try {
    const res = await fetch(`${pathFor(pane)}/answer`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: op.action, index: op.index, fingerprint }),
      signal: ctl.signal,
    });
    let body = {};
    try { body = await res.json(); } catch {}
    return { res, body };
  } finally {
    clearTimeout(timer);
  }
}

// freshDialog waits (a few tries) for the screen to show a dialog other than
// the one with this fingerprint: the previous tap has to land first.
async function freshDialog(pane, fp) {
  for (let i = 0; i < 6; i++) {
    await sleep(i ? 350 : 200);
    if (state.selected !== pane) return null;
    try {
      const r = await api(`${pathFor(pane)}/dialog`);
      if (r.dialog && r.dialog.fingerprint !== fp) return r.dialog;
      if (!r.dialog) return null;
    } catch { return null; }
  }
  return null;
}

async function pumpAnswers(pane) {
  if (answer.pumping) return;
  answer.pumping = true;
  let fail = "";
  let sentLast = null;
  try {
    while (answer.queue.length && state.selected === pane && answer.pane === pane) {
      const op = answer.queue[0];
      const base = answer.server || answer.dialog;
      let fp = base && base.fingerprint;
      let attempt = 0;
      for (;;) {
        let r;
        try { r = await postAnswer(pane, op, fp); }
        catch (e) { fail = e && e.name === "AbortError" ? "timed out — check the connection" : (e && e.message) || "network error"; break; }
        if (state.selected !== pane) return;
        const { res, body } = r;
        if (res.status === 401) { setUnauthorized(true); fail = UNAUTHORIZED_MSG; break; }
        if (res.ok) { sentLast = { op, fp, keys: body.keys || [] }; break; }
        // A toggle can be sent again against the dialog as it is now, when it is
        // the same question and the box still needs flipping (the previous tap
        // had not landed yet, or two taps raced).
        const nd = body.dialog;
        const retryable = op.action === "toggle" && attempt < 3 && (
          (res.status === 409 && body.code === "dialog_changed" && nd && nd.options && nd.options[op.index] &&
            nd.options[op.index].label === op.label && !!nd.options[op.index].checked !== op.want) ||
          (res.status === 409 && body.code === "duplicate"));
        if (retryable) {
          attempt++;
          if (body.code === "duplicate") { const f = await freshDialog(pane, fp); if (!f) { fail = "already answered"; break; } fp = f.fingerprint; answer.server = f; }
          else { fp = nd.fingerprint; answer.server = nd; }
          continue;
        }
        if (res.status === 409 && body.code === "dialog_changed") { fail = "the question changed — check it again"; if (nd) answer.server = nd; }
        else if (res.status === 409 && body.code === "duplicate") { fail = "already answered"; }
        else if (res.status === 409 && body.code === "no_dialog") { fail = "the dialog is gone"; answer.server = null; answer.gone = true; }
        else { fail = "answer failed: " + (body.message || body.code || res.statusText); if (body.dialog !== undefined) answer.server = body.dialog; }
        break;
      }
      if (fail) break;
      answer.queue.shift();
      if (answer.queue.length) {
        // Next tap: quote the dialog as it is once this one has landed.
        const f = await freshDialog(pane, fp);
        if (!f) { fail = "the question changed — check it again"; break; }
        answer.server = f;
      }
    }
  } finally {
    answer.pumping = false;
  }
  if (state.selected !== pane || answer.pane !== pane) return;
  const a = agentById(pane);
  if (fail) {
    // Roll back to what the server showed, with the reason on the card.
    const gone = answer.gone; answer.gone = false;
    answer.queue = [];
    answer.inflight = false;
    answer.hold = null;
    answer.err = fail;
    toast(fail, true);
    answer.dialog = gone ? null : answer.server ? JSON.parse(JSON.stringify(answer.server)) : answer.dialog;
    if (a) { renderAnswerCard(a); if (gone) { $("answer").style.display = staleQuestion(a) ? "none" : "block"; } }
    setTimeout(refreshDialog, 300);
    return;
  }
  answer.inflight = false;
  if (sentLast) {
    toast("sent " + sentLast.keys.map((k) => KEY_GLYPH[k] || k).join(" "));
    // Until the screen shows something else, a refresh that still has this
    // dialog is the tap not having landed yet, not a rollback.
    answer.hold = { fp: sentLast.fp, index: sentLast.op.index, action: sentLast.op.action, until: Date.now() + ANSWER_HOLD_MS };
    if (a) renderAnswerCard(a); // the spinner stops; the option stays chosen
    for (const ms of [400, 1500, ANSWER_HOLD_MS + 100]) setTimeout(refreshDialog, ms);
    if (answer.moreOpen) setTimeout(refreshSnapshot, 450);
    state.pollUntil = Date.now() + POLL_AFTER_SEND_MS;
    if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
  }
}

const cloneDialog = (d) => (d ? JSON.parse(JSON.stringify(d)) : d);

function setDialog(pane, d) {
  if (state.selected !== pane || answer.pane !== pane) return;
  answer.server = cloneDialog(d);
  // Taps in flight own what the card shows; a refresh that still carries the
  // dialog a tap was sent against has not heard of it yet: neither may undo it.
  if (answer.queue.length) return;
  const h = answer.hold;
  if (h) {
    if (d && d.fingerprint === h.fp && Date.now() < h.until) return;
    answer.hold = null;
  }
  answer.dialog = d;
  answer.loaded = true;
  const a = agentById(pane);
  if (a && needsAnswer(a)) {
    renderAnswerCard(a);
    $("answer").style.display = staleQuestion(a) ? "none" : "block";
  }
}

async function refreshDialog() {
  const pane = state.selected;
  if (!pane || !answer.shown || answer.inflight) return;
  try {
    const res = await api(`${pathFor(pane)}/dialog`);
    if (answer.inflight) return; // an answer started meanwhile; its result wins
    setDialog(pane, res.dialog || null);
  } catch (e) {
    if (e.status === 404) return; // agent gone; the SSE list update closes the view
    setDialog(pane, null); // fall back to the pad
  }
}

async function padPress(key, label, btn) {
  await sendKeys([key], label, btn);
  setTimeout(refreshSnapshot, 350);
  setTimeout(refreshDialog, 400);
}

async function refreshSnapshot() {
  const pane = state.selected;
  if (!pane || !$("answer-snap")) return;
  try {
    const read = await api(`${pathFor(pane)}/output?source=visible`);
    if (state.selected !== pane) return;
    answer.snapText = (read.text || "").replace(/\s+$/, "") || "(blank screen)";
  } catch (e) {
    answer.snapText = "screen read failed: " + e.message;
  }
  const el = $("answer-snap");
  if (!el) return;
  el.textContent = answer.snapText;
  el.scrollTop = el.scrollHeight; // dialogs render at the bottom of the screen
}

function startAnswerPoll() {
  if (answer.timer) return;
  refreshDialog();
  refreshSnapshot();
  answer.timer = setInterval(() => {
    if (document.hidden) return;
    refreshDialog();
    const more = document.querySelector("#answer details.ans-more");
    if (more && more.open) refreshSnapshot();
  }, 2500);
}

function stopAnswerPoll() {
  clearInterval(answer.timer);
  answer.timer = null;
}

/* ---------- actions ---------- */

// While the agent works and nothing is typed, Send becomes Stop (Esc,
// which interrupts both Pi and Claude Code). Typed text keeps it Send, so
// a prompt can still be queued mid-turn. An open questionnaire has its
// own cancel button in the answer card.
function stopMode() {
  const a = agentById(state.selected);
  return !!a && a.agent_status === "working" && !state.ask && !state.sending &&
    !promptValue().trim();
}

const SEND_ICON = '<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">' +
  '<path d="M12 19V5M5.5 11.5 12 5l6.5 6.5" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg>';

function updateSendButton() {
  const btn = $("btn-send");
  const stop = stopMode();
  // The stop glyph is a CSS square (an emoji ⏹ renders inconsistently).
  btn.innerHTML = stop ? '<span class="stop-glyph" aria-hidden="true"></span>' : SEND_ICON;
  btn.classList.toggle("stop", stop);
  btn.setAttribute("aria-label", stop ? "Stop the agent (Esc)" : "Send prompt");
  // While the agent works, a sent prompt waits for its next step.
  const a = agentById(state.selected);
  $("prompt-text").dataset.placeholder = a && a.agent_status === "working" && !state.ask
    ? "Queue a message for its next step…" : "Send a prompt…";
}

// echoInOutput shows a sent prompt in the raw view at once; the next read
// replaces it with what the pane really shows.
function echoInOutput(text) {
  if (!state.chat || !state.chat.terminal) return;
  const el = $("output");
  el.textContent += "\n> " + text;
  el.scrollTop = el.scrollHeight;
}

// refreshView re-fetches the current agent's view from scratch: drops any
// read in flight, reconnects the live stream and reloads chat or output.
function refreshView() {
  if (!state.selected) return;
  const terminal = !!(state.chat && state.chat.terminal);
  if (outputAbort) outputAbort.abort();
  outputAbort = null;
  outputBusy = false;
  state.sending = 0;
  updateSendButton();
  state.retryDelay = 1000;
  connectEvents();
  refreshAgents().catch(() => setConn(false));
  if (terminal) {
    $("output").textContent = "";
    refreshOutput();
  } else {
    loadChat(true).then(refreshStatusLine);
  }
  toast("refreshing…");
}

// sendPrompt: the message leaves the box and shows in the transcript (as
// "sending…") in this frame; the POST goes out behind it, in order, and a
// failure puts the text back in the box with the error. Several messages can
// be typed and sent while earlier ones are still on the wire.
let sendChain = Promise.resolve();

function sendPrompt() {
  const text = promptValue().trim();
  if (!text) return;
  const cur = agentById(state.selected);
  if (state.ask && cur && cur.agent_status === "working") {
    // herdr only refuses prompts while `blocked`; an open questionnaire
    // reports `working`, and pasted text + Enter would land in the dialog.
    // (The server refuses with 409 dialog_open for any other status when the
    // question is really on screen.)
    toast("answer the question first (or Esc to cancel it)", true);
    return;
  }
  const pane = state.selected;
  let path;
  try { path = pathFor(pane); } catch (e) { toast("prompt failed: " + e.message, true); return; }
  state.sending++;
  setPromptValue("");
  // Shown before the POST returns: a fast transcript poll may already
  // carry the message, and settling needs the bubble to exist.
  const pending = addPending(text);
  if (pending) pending.sending = true;
  renderPending();
  const terminal = !!(state.chat && state.chat.terminal);
  if (terminal) echoInOutput(text);
  updateSendButton();
  sendChain = sendChain.then(() => postPrompt({ text, pane, path, pending, terminal }));
}

async function postPrompt(job) {
  const { text, pane, path, pending, terminal } = job;
  try {
    await api(`${path}/prompt`, { method: "POST", body: JSON.stringify({ text }) });
    if (pending) { pending.sending = false; if (state.selected === pane) renderPending(); }
    state.pollUntil = Date.now() + POLL_AFTER_SEND_MS;
    if (state.selected !== pane) return;
    // The transcript poll picks the message up too.
    if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
    else setTimeout(() => refreshOutput(true), 600);
  } catch (e) {
    if (state.selected === pane) {
      dropPending(pending);
      if (terminal) refreshOutput(true); // replaces the echo with what the pane shows
      // Back in the box, ready to send again (ahead of anything typed since).
      const now = promptValue();
      setPromptValue(now.trim() ? text + "\n" + now : text);
    }
    if (e.status === 409) {
      toast("agent is waiting for input — answer the dialog or press Esc first", true);
    } else {
      toast("prompt failed: " + e.message + (state.selected === pane ? " — your message is back in the box" : ""), true);
    }
  } finally {
    state.sending = Math.max(0, state.sending - 1);
    updateSendButton();
  }
}

/* ---------- images ---------- */
//
// A pasted or picked image is uploaded to the agent's host, which returns
// a file path; the path goes into the prompt, and Claude Code and Pi read
// the image from it. Large images are downscaled first.

const IMAGE_MAX_SIDE = 2000;
const IMAGE_KEEP_BYTES = 2 << 20; // smaller images are sent as they are

async function imageDataURL(file) {
  let blob = file;
  try {
    const bmp = await createImageBitmap(file);
    const scale = Math.min(1, IMAGE_MAX_SIDE / Math.max(bmp.width, bmp.height));
    if (scale < 1 || file.size > IMAGE_KEEP_BYTES || !/^image\/(png|jpeg|gif|webp)$/.test(file.type)) {
      const c = document.createElement("canvas");
      c.width = Math.round(bmp.width * scale);
      c.height = Math.round(bmp.height * scale);
      c.getContext("2d").drawImage(bmp, 0, 0, c.width, c.height);
      blob = await new Promise((res) => c.toBlob(res, "image/jpeg", 0.9)) || file;
    }
    bmp.close && bmp.close();
  } catch {} // undecodable here: let the server judge the original
  return new Promise((res, rej) => {
    const r = new FileReader();
    r.onload = () => res(r.result);
    r.onerror = () => rej(r.error || new Error("could not read image"));
    r.readAsDataURL(blob);
  });
}

/* ---------- composer ---------- */
//
// The prompt box is a contenteditable <div>: iOS offers Paste for an
// image-only clipboard (a screenshot) only on rich editors, never on a
// <textarea>. Its content is kept plain text: pastes and drops insert
// text only, and images become uploads.

function promptValue() {
  // innerText maps <div>/<br> line breaks to "\n"; NBSPs from editing
  // become plain spaces.
  return $("prompt-text").innerText.replace(/\u00a0/g, " ").replace(/\n$/, "");
}

function setPromptValue(s) {
  $("prompt-text").textContent = s;
  updateSendButton();
}

// selectionInComposer reports whether the caret is in the composer.
function selectionInComposer() {
  const sel = window.getSelection();
  return !!sel && sel.rangeCount > 0 && $("prompt-text").contains(sel.anchorNode);
}

// insertPlain inserts text at the caret (as typing would, so undo works),
// or appends it when the caret is elsewhere.
function insertPlain(text) {
  const el = $("prompt-text");
  if (!selectionInComposer()) {
    el.focus();
    const r = document.createRange();
    r.selectNodeContents(el);
    r.collapse(false);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
  }
  if (!document.execCommand("insertText", false, text)) {
    const sel = window.getSelection();
    const r = sel.getRangeAt(0);
    r.deleteContents();
    const node = document.createTextNode(text);
    r.insertNode(node);
    r.setStartAfter(node);
    r.collapse(true);
    sel.removeAllRanges();
    sel.addRange(r);
  }
  normalizeComposer();
  updateSendButton();
}

// normalizeComposer keeps the composer plain: no stray markup (an image
// a browser inserted on its own), and truly empty when blank so the
// placeholder shows.
function normalizeComposer() {
  const el = $("prompt-text");
  if (el.querySelector("img, picture, video, iframe, object, embed, svg, a, table")) {
    el.textContent = promptValue();
  }
  if (!el.textContent && el.innerHTML) el.innerHTML = "";
}

// textBeforeCaret is the composer text up to the caret (all of it when
// the caret is elsewhere, since insertPlain then appends).
function textBeforeCaret() {
  if (!selectionInComposer()) return promptValue();
  const el = $("prompt-text");
  const at = window.getSelection().getRangeAt(0);
  const r = document.createRange();
  r.selectNodeContents(el);
  r.setEnd(at.startContainer, at.startOffset);
  return r.toString();
}

// insertPromptText inserts a word (an image path) with spaces around it.
function insertPromptText(text) {
  const before = textBeforeCaret();
  insertPlain((before && !/\s$/.test(before) ? " " : "") + text + " ");
}

// imageFiles collects image files from a paste/drop DataTransfer.
function imageFiles(dt) {
  if (!dt) return [];
  const files = [...(dt.files || [])].filter((f) => f.type.startsWith("image/"));
  if (!files.length) {
    for (const it of dt.items || []) {
      if (it.kind === "file" && it.type.startsWith("image/")) {
        const f = it.getAsFile();
        if (f) files.push(f);
      }
    }
  }
  return files;
}

function onComposerPaste(ev) {
  ev.preventDefault(); // never let rich content in
  const images = imageFiles(ev.clipboardData);
  if (images.length) { attachImages(images); return; }
  const text = ev.clipboardData ? ev.clipboardData.getData("text/plain") : "";
  if (text) insertPlain(text);
}

function onComposerDrop(ev) {
  ev.preventDefault();
  const images = imageFiles(ev.dataTransfer);
  if (images.length) { attachImages(images); return; }
  const text = ev.dataTransfer ? ev.dataTransfer.getData("text/plain") : "";
  if (text) insertPlain(text);
}

// The + menu: "Paste image" reads the clipboard (iOS then shows its own
// Paste bubble), "Choose photo…" opens the picker. Both run straight from
// a tap, which both APIs require.
function toggleAttachMenu(show) {
  const m = $("attach-menu");
  m.hidden = show === undefined ? !m.hidden : !show;
  $("btn-attach").setAttribute("aria-expanded", String(!m.hidden));
}

// The Keys… pad: raw keys for screens the app does not recognize. Ctrl+C
// is here, not in plain sight: Stop (Esc) is the way to interrupt, and two
// Ctrl+C presses exit Claude Code.
const KEYPAD_KEYS = [...PAD_KEYS.slice(0, 8), ["Ctrl+C", "ctrl+c"], ...PAD_KEYS.slice(8, 11)];

function toggleKeypad(show) {
  const pad = $("keypad");
  if (!pad.firstChild) {
    pad.innerHTML = KEYPAD_KEYS.map(([label, key]) =>
      `<button type="button" class="btn${key === "esc" ? " warn" : key === "ctrl+c" ? " danger" : ""}" data-key="${esc(key)}">${esc(label)}</button>`).join("");
  }
  if (show === undefined && !pad.hidden && pad.classList.contains("auto")) {
    // The automatic Esc/Enter strip is showing: Keys… widens it to the full pad.
    pad.classList.remove("auto");
    pad.dataset.manual = "1";
    return;
  }
  pad.hidden = show === undefined ? !pad.hidden : !show;
  pad.classList.remove("auto");
  if (pad.hidden) delete pad.dataset.manual; else pad.dataset.manual = "1";
}

// A screen that ends in a prompt-like footer ("enter to select · esc to
// cancel", "(y/n)", "Trust project folder?") is waiting for a key even when
// no dialog was recognised and the agent reads idle: show a compact
// ↑ ↓ Esc Enter strip without anyone having to find Keys….
const KEY_FOOTER = /(\benter\b[^\n]{0,24}\b(select|confirm|accept|submit|continue)|\besc(ape)?\b[^\n]{0,24}\b(cancel|close|back|dismiss|exit)|\(y\/n\)|\[y\/n\]|\btrust\b[^\n]{0,40}\?|press (enter|any key)|↑\/↓|↑↓)/i;
function screenWantsKeys(text) {
  const tail = String(text || "").split("\n").filter((l) => l.trim()).slice(-8).join("\n");
  return KEY_FOOTER.test(tail);
}
function setAutoKeys(on) {
  const pad = $("keypad");
  if (pad.dataset.manual) return; // the user opened the full pad themselves
  if (on) toggleKeypad(true), delete pad.dataset.manual, pad.classList.add("auto");
  else if (pad.classList.contains("auto")) { pad.hidden = true; pad.classList.remove("auto"); }
}

function pasteImageFromClipboard() {
  toggleAttachMenu(false);
  if (!navigator.clipboard || !navigator.clipboard.read) {
    toast("this browser cannot read images from the clipboard — use Choose photo", true);
    return;
  }
  navigator.clipboard.read().then(async (items) => {
    for (const item of items) {
      const type = item.types.find((t) => t.startsWith("image/"));
      if (type) {
        const blob = await item.getType(type);
        await attachImages([new File([blob], "pasted." + type.split("/")[1], { type })]);
        return;
      }
    }
    toast("no image on the clipboard", true);
  }).catch((e) => {
    if (e && e.name === "NotAllowedError") return; // the Paste bubble was dismissed
    toast("could not read the clipboard: " + (e && e.message || e), true);
  });
}

async function attachImages(files) {
  const images = [...files].filter((f) => f && f.type && f.type.startsWith("image/"));
  if (!images.length || !state.selected) return false;
  const pane = state.selected;
  for (const f of images) {
    toast("uploading image…");
    try {
      const data = await imageDataURL(f);
      const r = await api(`${pathFor(pane)}/image`, { method: "POST", body: JSON.stringify({ data }) });
      if (state.selected !== pane) return true;
      insertPromptText(r.path);
      toast("image attached");
    } catch (e) {
      toast("image upload failed: " + e.message, true);
    }
  }
  return true;
}

async function sendKeys(keys, label, btn) {
  flash(btn);
  toast(label + "…"); // the tap is taken now; the confirmation replaces this
  try {
    await api(`${pathFor(state.selected)}/keys`, {
      method: "POST",
      body: JSON.stringify({ keys }),
    });
    toast(label + " sent");
    state.pollUntil = Date.now() + POLL_AFTER_SEND_MS;
    if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
    else setTimeout(() => refreshOutput(true), 500);
  } catch (e) {
    toast(label + " failed: " + e.message, true);
  }
}

/* ---------- live updates (SSE) ---------- */

const STALE_MS = 45000;

function setConn(ok) {
  $("conn-list").classList.toggle("ok", ok);
}

function scheduleReconnect() {
  if (state.retryTimer) return; // no duplicate timers
  const delay = state.retryDelay;
  state.retryDelay = Math.min(delay * 2, 30000);
  state.retryTimer = setTimeout(() => { state.retryTimer = null; connectEvents(); }, delay);
}

function refreshAgents() {
  return api("/api/agents")
    .then((d) => {
      state.unauthorized = false;
      setAgents(d);
      renderList();
      if (state.selected) renderDetail();
      applyPendingRoute();
      scheduleLedger();
    });
}

function connectEvents() {
  clearTimeout(state.retryTimer);
  state.retryTimer = null;
  if (state.es) { state.es.onopen = state.es.onerror = null; state.es.close(); }
  const url = "/api/events";
  const es = new EventSource(url);
  state.es = es;
  state.lastEventAt = Date.now();

  const alive = () => { state.lastEventAt = Date.now(); state.retryDelay = 1000; setConn(true); };
  es.addEventListener("agents", (ev) => {
    alive();
    const data = JSON.parse(ev.data);
    noteVersion(data.version);
    state.unauthorized = false;
    setAgents(data);
    renderList();
    scheduleLedger();
    if (state.selected) {
      const a = agentById(state.selected);
      const prev = state.lastStatus;
      state.lastStatus = a ? a.agent_status : null;
      // When the viewed agent settles (working/blocked -> anything else),
      // the 4s transcript poll stops — fetch once more so the final
      // messages are not left behind.
      const wasActive = prev === "working" || prev === "blocked";
      const isActive = a && (a.agent_status === "working" || a.agent_status === "blocked");
      if (a && wasActive && !isActive && state.chat) {
        if (state.chat.terminal) refreshOutput();
        else loadChat(false);
      }
      renderDetail();
    }
    applyPendingRoute();
  });
  es.addEventListener("ping", alive); // server heartbeat, every 15s
  es.addEventListener("resync", () => {
    // The server's herdr stream was interrupted; refetch authoritative state.
    state.lastEventAt = Date.now();
    refreshAgents().catch(() => setConn(false));
  });
  es.onopen = alive;
  es.onerror = () => {
    // Take over reconnecting so we control backoff and never stack streams.
    setConn(false);
    es.onopen = es.onerror = null;
    es.close();
    if (state.es === es) scheduleReconnect();
  };
}

// Heartbeats are SSE comments (invisible to JS), so liveness is judged by the
// last open/agents/resync event. A silent stream is recycled (the server sends
// a fresh snapshot on connect), and a cheap /api/agents fetch doubles as a
// network probe so the dot reflects reality.
function startWatchdog() {
  if (state.watchdog) return;
  state.watchdog = setInterval(() => {
    if (document.hidden) return;
    if (Date.now() - state.lastEventAt > STALE_MS) { setConn(false); connectEvents(); }
    fetch("/api/agents").then((r) => {
      if (r.status === 401) setUnauthorized(true);
      if (!r.ok) throw new Error(r.status);
      if (state.es && state.es.readyState === EventSource.OPEN) setConn(true);
    }).catch(() => setConn(false));
  }, 15000);
}

// suspendEvents closes the stream when the page is hidden for good or put
// in the back/forward cache. Chrome keeps a cached page's EventSource open,
// and each one pins one of the browser's six HTTP/1.1 connections to this
// origin: a few cached copies of the app starve every later request (the
// chat stays empty, keys never send). pageshow -> resume() reconnects.
function suspendEvents() {
  clearTimeout(state.retryTimer);
  state.retryTimer = null;
  if (state.es) { state.es.onopen = state.es.onerror = null; state.es.close(); state.es = null; }
}

let resumeAt = 0;
function resume() {
  if (document.hidden) return;
  const now = Date.now();
  if (now - resumeAt < 500) return; // visibilitychange + pageshow + online often fire together
  resumeAt = now;
  state.retryDelay = 1000;
  connectEvents();
  refreshAgents().catch(() => setConn(false));
  if (state.selected) {
    // A read begun before the app was backgrounded may never settle (iOS
    // drops its socket without failing the fetch); do not wait for it.
    if (outputAbort) outputAbort.abort();
    outputAbort = null;
    outputBusy = false;
    if (state.chat && state.chat.terminal) refreshOutput();
    else loadChat(!state.chat);
  }
}

/* ---------- Web Push notifications ---------- */
//
// The server sends an encrypted push when an agent becomes blocked or
// done. Subscribing needs a user gesture (the "Enable" button). On iOS it
// only works in the Home Screen app (16.4+), never in a Safari tab.

const push = {
  key: null,        // server VAPID public key (base64url); null until fetched
  serverOn: null,   // server has Web Push enabled
  sub: null,        // current PushSubscription
  busy: false,
};

function isIOS() {
  return /iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1); // iPadOS reports as a Mac
}

function isStandalone() {
  return navigator.standalone === true || window.matchMedia("(display-mode: standalone)").matches;
}

function pushSupported() {
  return "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
}

function b64ToBytes(s) {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

function sameKey(sub, key) {
  const k = sub.options && sub.options.applicationServerKey;
  if (!k || !key) return true; // cannot tell; assume it matches
  const a = new Uint8Array(k), b = b64ToBytes(key);
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

// pushState names what the panel should show.
function pushState() {
  if (isIOS() && !isStandalone()) return "ios-install";
  if (!pushSupported()) return isIOS() ? "ios-old" : "unsupported";
  if (push.serverOn === false) return "server-off";
  if (push.serverOn === null) return "checking";
  if (Notification.permission === "denied") return "denied";
  return push.sub ? "on" : "off";
}

const PUSH_TEXT = {
  "ios-install": "On iPhone and iPad, notifications only work in the Home Screen app. In Safari, tap Share, then “Add to Home Screen”, and open Comebye from your Home Screen.",
  "ios-old": "Notifications need iOS 16.4 or later, in the Home Screen app.",
  unsupported: "This browser does not support Web Push notifications.",
  "server-off": "Web Push is turned off on the server.",
  checking: "Checking…",
  denied: "Notifications are blocked for this app. Allow them in the system or browser settings, then reload.",
  off: "Off. Get a notification when an agent needs input or finishes.",
  on: "On for this device. You are notified when an agent needs input or finishes, unless you are looking at it.",
};

function renderPush() {
  const st = pushState();
  $("notify-status").textContent = PUSH_TEXT[st] || st;
  $("notify-enable").hidden = st !== "off";
  $("notify-test").hidden = st !== "on";
  $("notify-disable").hidden = st !== "on";
  for (const id of ["notify-enable", "notify-test", "notify-disable"]) $(id).disabled = push.busy;
  $("btn-notify").classList.toggle("on", st === "on");
}

async function postSubscription(sub) {
  await api("/api/push/subscribe", { method: "POST", body: JSON.stringify(sub.toJSON()) });
}

// initPush learns the server key and the current subscription, and
// re-registers an existing subscription so the server's copy stays fresh
// (e.g. after its state dir was reset).
async function initPush() {
  if (!pushSupported() || (isIOS() && !isStandalone())) { renderPush(); return; }
  try {
    const k = await api("/api/push/key");
    push.serverOn = !!k.enabled;
    push.key = k.key || null;
  } catch (e) {
    renderPush();
    return;
  }
  try {
    const reg = await navigator.serviceWorker.ready;
    let sub = await reg.pushManager.getSubscription();
    if (sub && push.serverOn && !sameKey(sub, push.key)) {
      // The server's VAPID key changed: the old subscription is useless.
      await sub.unsubscribe().catch(() => {});
      sub = null;
      if (Notification.permission === "granted") {
        sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(push.key) }).catch(() => null);
      }
    }
    push.sub = sub;
    if (sub && push.serverOn) await postSubscription(sub);
  } catch (e) {
    // Leave the state as found; the panel offers Enable again.
  }
  renderPush();
}

async function enablePush() {
  if (push.busy) return;
  push.busy = true;
  renderPush();
  try {
    // Called first, straight from the tap: iOS only shows the permission
    // prompt from a user gesture.
    const perm = await Notification.requestPermission();
    if (perm !== "granted") {
      toast(perm === "denied" ? "notifications blocked" : "notifications not allowed", true);
      return;
    }
    if (!push.key) throw new Error("server key unavailable");
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(push.key) });
    await postSubscription(sub);
    push.sub = sub;
    toast("notifications on");
  } catch (e) {
    toast("could not enable notifications: " + e.message, true);
  } finally {
    push.busy = false;
    renderPush();
  }
}

async function disablePush() {
  if (push.busy || !push.sub) return;
  push.busy = true;
  renderPush();
  const sub = push.sub;
  try {
    await api("/api/push/unsubscribe", { method: "POST", body: JSON.stringify({ endpoint: sub.endpoint }) }).catch(() => {});
    await sub.unsubscribe();
    push.sub = null;
    toast("notifications off");
  } catch (e) {
    toast("could not turn off: " + e.message, true);
  } finally {
    push.busy = false;
    renderPush();
  }
}

async function testPush() {
  if (push.busy || !push.sub) return;
  push.busy = true;
  renderPush();
  try {
    const r = await api("/api/push/test", { method: "POST", body: JSON.stringify({ endpoint: push.sub.endpoint }) });
    if (r.sent > 0) toast("test sent");
    else toast("test failed: " + ((r.results && r.results[0] && r.results[0].error) || "unknown error"), true);
  } catch (e) {
    if (e.code === "not_subscribed") {
      // The server lost this subscription; register it again.
      await postSubscription(push.sub).then(() => toast("re-registered, try again"), () => toast("test failed", true));
    } else {
      toast("test failed: " + e.message, true);
    }
  } finally {
    push.busy = false;
    renderPush();
  }
}

function toggleNotifyPanel(show) {
  const panel = $("notify-panel");
  const open = show ?? panel.hidden;
  if (open === !panel.hidden) return;
  panel.hidden = !open;
  $("btn-notify").setAttribute("aria-expanded", String(open));
  if (open) {
    renderPush();
    if (push.serverOn === null) initPush();
    placePop("notify", $("btn-notify"));
    popOpened("notify");
    (popItems(panel)[0] || panel.querySelector(".pop-x")).focus();
  } else {
    popClosed("notify");
  }
}

/* ---------- presence (foreground suppression) ---------- */
//
// Tell the server which agent is on screen so it does not push a
// notification for it. Reports expire on the server (45s), so they are
// refreshed while visible; a hidden page or closed chat clears them.

const presence = {
  id: (crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2)),
  last: "",
  timer: null,
};

function reportPresence(force = false, gone = false) {
  const pane = gone ? "" : (state.selected || "");
  const visible = !!pane && !document.hidden;
  const sig = visible ? pane : "";
  if (!force && sig === presence.last) return;
  presence.last = sig;
  fetch("/api/push/presence", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ client: presence.id, pane, visible }),
    keepalive: true, // survive the page being hidden or closed
  }).catch(() => { presence.last = "?"; }); // retry on the next tick
  clearInterval(presence.timer);
  presence.timer = visible ? setInterval(() => reportPresence(true), 15000) : null;
  if (visible) clearNotifications(pane);
}

// clearNotifications closes an agent's notification once its chat is
// open: the server holds further ones until then, so the tray then shows
// only agents still waiting on you. Notifications are tagged with the
// same agent ID the presence report carries.
function clearNotifications(tag) {
  if (!("serviceWorker" in navigator)) return;
  navigator.serviceWorker.getRegistration()
    .then((reg) => reg && reg.getNotifications({ tag }))
    .then((list) => { for (const n of list || []) n.close(); })
    .catch(() => {});
}

/* ---------- launching a new agent ---------- */
//
// "+" in the list header opens a sheet: pick a host (on a hub), browse to
// a directory inside that host's launch roots, pick an agent kind, and
// optionally name it and give it a first prompt. The server opens a
// workspace there and starts the agent; the app then opens its chat once
// it shows up in the agent list. Like "View steps", the sheet is a
// history entry, so Back (or Escape) closes it.

const launch = {
  open: false,
  host: null,   // host the sheet launches on
  opts: null,   // /launch/options answer for that host
  path: "",     // directory being browsed ("" = the roots)
  pathLabel: "",
  dir: null,    // chosen directory (dirLabel: as shown, with ~)
  dirLabel: "",
  kind: null,
  busy: false,
  seq: 0,       // bumped per host/listing, so a slow answer for an old one is dropped
  mode: null,   // "agent" (in a directory) or "worker" (a handoff: new branch and worktree)
  repos: null,  // the host's git repositories, for worker mode
  repoSeq: 0,
  model: "",    // chosen model ("" = the agent's own default: no flag is passed)
  thinking: "", // chosen pi thinking level ("" = pi's default)
  cat: null,    // the host's model catalog for the kind ({loading} or {error} while not known)
  catSeq: 0,
  picking: false, // the model list is open
  q: "",        // its search text
};

const LAUNCH_MODE_KEY = "comebye-launch-mode";

// workerMode: the sheet hands work to a new worker (needs the ledger).
function workerMode() { return ledger.on === true && launch.mode === "worker"; }
// coordMode: the sheet starts a coordinator (the hub records its role).
function coordMode() { return ledger.on === true && launch.mode === "coordinator"; }
const LAUNCH_MODES = new Set(["agent", "worker", "coordinator"]);

// harnessFor mirrors the server's playbook flavor for an agent kind.
function harnessFor(kind) { return kind === "pi" || kind === "claude" ? kind : "generic"; }

// validBranch mirrors the server's conservative branch-name rule.
function validBranch(b) {
  if (!/^[A-Za-z0-9][A-Za-z0-9._\/-]{0,99}$/.test(b) || b === "HEAD" || b.includes("..") || b.includes("//") || /[/.]$/.test(b)) return false;
  return b.split("/").every((p) => !p.startsWith(".") && !p.startsWith("-") && !p.endsWith(".lock"));
}

/* ---- the model picker ---- */
//
// After the agent kind, the sheet offers the models that kind can start
// with on the chosen host (GET .../models?kind=K). "Default" is the
// default: no flag is passed and the agent uses its own configured model.
// The last choice per kind is remembered on this device.

const MODEL_KEY = "comebye-model-";
const MODEL_RE = /^[A-Za-z0-9][A-Za-z0-9._:\/@+-]{0,127}$/;
const THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

// supportsModel: the host's server says this kind takes a model.
function supportsModel() {
  const o = launch.opts;
  return !!(o && o.enabled && launch.kind && (o.model_kinds || []).includes(launch.kind));
}

function modelsBase(host) {
  return !host || host === state.localHost ? "/api/models" : `/api/hosts/${encodeURIComponent(host)}/models`;
}

function rememberModel() {
  if (!launch.kind) return;
  try {
    if (!launch.model && !launch.thinking) localStorage.removeItem(MODEL_KEY + launch.kind);
    else localStorage.setItem(MODEL_KEY + launch.kind, JSON.stringify({ model: launch.model, thinking: launch.thinking }));
  } catch {}
}

// kindChosen restores the kind's remembered model and fetches its catalog.
async function kindChosen() {
  launch.model = "";
  launch.thinking = "";
  launch.picking = false;
  launch.q = "";
  launch.cat = null;
  const seq = ++launch.catSeq;
  if (!supportsModel()) { renderLaunchForm(); return; }
  try {
    const m = JSON.parse(lsGet(MODEL_KEY + launch.kind) || "null");
    if (m && typeof m.model === "string" && MODEL_RE.test(m.model)) launch.model = m.model;
    if (m && THINKING_LEVELS.includes(m.thinking) && launch.kind === "pi") launch.thinking = m.thinking;
  } catch {}
  launch.cat = { loading: true };
  renderLaunchForm();
  const kind = launch.kind;
  let cat;
  try {
    const d = await api(modelsBase(launch.host) + "?kind=" + encodeURIComponent(kind));
    cat = (d.catalogs || [])[0] || { error: "the host sent no model list" };
  } catch (e) {
    cat = { error: e.message };
  }
  if (seq !== launch.catSeq) return;
  launch.cat = cat;
  // A remembered model this host does not have (a different host, or
  // removed since): back to the default rather than failing the launch.
  if (launch.model && cat.exact && !(cat.models || []).some((m) => m.id === launch.model)) {
    launch.model = "";
    toast(`That model is not available on ${launch.host || "this host"}: using the default`);
  }
  renderLaunchForm();
}

function pickModel(id) {
  launch.model = id;
  launch.picking = false;
  launch.q = "";
  rememberModel();
  renderLaunchForm();
  $("ln-model-btn").focus();
}

function togglePicker() {
  launch.picking = !launch.picking;
  launch.q = "";
  $("ln-model-q").value = "";
  renderLaunchForm();
  if (launch.picking) {
    // Bring the list up under the header so it has room above the keyboard.
    $("ln-model-label").scrollIntoView({ block: "start" });
    $("ln-model-q").focus({ preventScroll: true });
  }
}

// modelRow is one row of the list; note is a second line.
function modelRow(id, label, selected, { note = "", badge = "", plain = false } = {}) {
  return `<button type="button" class="ln-model${plain ? " plain" : ""}" role="option" aria-selected="${id === selected}" data-model="${esc(id)}">` +
    `<span class="ln-mname">${esc(label)}${note ? `<span class="ln-mnote">${esc(note)}</span>` : ""}</span>${badge}</button>`;
}

// fillModelList draws a catalog into list: grouped by provider, filtered by
// the search text, the host's default marked. withDefault adds the
// "Default: pass no flag" row (a launch has one, a running agent does not).
function fillModelList(list, cat, query, selected, withDefault) {
  cat = cat || {};
  if (cat.loading) { list.innerHTML = '<div class="ln-empty">Loading models…</div>'; return; }
  const q = query.trim().toLowerCase();
  const terms = q.split(/\s+/).filter(Boolean);
  const models = (cat.models || []).filter((m) => {
    const hay = `${m.provider || ""} ${m.id} ${m.name || ""}`.toLowerCase();
    return terms.every((t) => hay.includes(t));
  });
  let html = "";
  if (withDefault && !q) {
    const dn = cat.default ? `currently ${cat.default}` : "the agent's own setting";
    html += modelRow("", "Default", selected, { note: `No flag is passed: ${dn}`, plain: true });
  }
  let prov = null;
  for (const m of models) {
    const p = m.provider || "Models";
    if (p !== prov) { html += `<div class="ln-head">${esc(p)}</div>`; prov = p; }
    const badges = (m.id === cat.default ? '<span class="ln-badge dflt">default</span>' : "") +
      (m.thinking ? '<span class="ln-badge" title="reasoning model">thinking</span>' : "");
    html += modelRow(m.id, m.provider && m.id.startsWith(m.provider + "/") ? m.id.slice(m.provider.length + 1) : m.id, selected, { note: m.note, badge: badges });
  }
  // A name that is not listed can still be typed unless the list is exact.
  const typed = query.trim();
  const canType = typed && !cat.exact && MODEL_RE.test(typed) && !(cat.models || []).some((m) => m.id === typed);
  if (canType) html += `<div class="ln-head">Other</div>` + modelRow(typed, `Use “${typed}”`, selected, { plain: true });
  if (!html || (q && !models.length && !canType)) html = `<div class="ln-empty">No model matches “${esc(typed)}”</div>`;
  list.innerHTML = html;
}

function renderModelList() { fillModelList($("ln-mlist"), launch.cat, launch.q, launch.model, true); }

function renderModelBlock() {
  const block = $("ln-model-block");
  const show = supportsModel();
  block.hidden = !show;
  if (!show) return;
  const cat = launch.cat || {};
  const val = $("ln-model-val");
  if (launch.model) {
    val.textContent = launch.model;
    val.classList.add("set");
  } else {
    val.textContent = cat.default ? `Default (${cat.default})` : "Default";
    val.classList.remove("set");
  }
  // Like a model pill with its effort beside it: pi's thinking level, muted.
  $("ln-model-think").textContent = launch.kind === "pi" ? (launch.thinking || cat.default_thinking || "") : "";
  $("ln-model-btn").setAttribute("aria-expanded", String(launch.picking));
  $("ln-picker").hidden = !launch.picking;
  if (launch.picking) renderModelList();
  let note = "";
  if (cat.error) note = `Could not list models (${cat.error}). ${launch.kind === "pi" ? "" : "You can still type a name. "}Default still works.`;
  else if (!launch.model) note = "Default passes no flag: the agent uses its own setting.";
  else {
    const m = (cat.models || []).find((x) => x.id === launch.model);
    if (m && launch.kind === "pi" && !m.thinking && launch.thinking && launch.thinking !== "off") note = "This model does not reason: the thinking level has no effect.";
  }
  $("ln-model-note").textContent = note;
  const think = launch.kind === "pi";
  $("ln-thinking").hidden = !think;
  if (think) {
    const dflt = cat.default_thinking ? `Default (${cat.default_thinking})` : "Default";
    $("ln-think-chips").innerHTML = [["", dflt], ...(cat.thinking_levels || THINKING_LEVELS).map((l) => [l, l])].map(([v, label]) =>
      `<button type="button" class="hf-chip" data-think="${esc(v)}" aria-pressed="${v === launch.thinking}">${esc(label)}</button>`).join("");
  }
}

// withModel adds the chosen model (and pi's thinking level) to a request.
function withModel(body) {
  if (!supportsModel()) return body;
  if (launch.model) body.model = launch.model;
  if (launch.kind === "pi" && launch.thinking) body.thinking = launch.thinking;
  return body;
}

function setLaunchMode(mode) {
  if (!LAUNCH_MODES.has(mode)) return;
  launch.mode = mode;
  lsSet(LAUNCH_MODE_KEY, mode);
  if (mode === "worker" && !launch.repos) loadRepos(launch.host);
  renderLaunchForm();
}

async function loadRepos(host) {
  const seq = ++launch.repoSeq;
  launch.repos = null;
  renderLaunchForm();
  const base = !host || host === state.localHost ? "/api/repos" : `/api/hosts/${encodeURIComponent(host)}/repos`;
  let d;
  try {
    d = await api(base);
  } catch (e) {
    if (seq !== launch.repoSeq) return;
    launch.repos = [];
    $("hw-repo-note").textContent = e.message;
    renderLaunchForm();
    return;
  }
  if (seq !== launch.repoSeq) return;
  launch.repos = Array.isArray(d.repos) ? d.repos : [];
  $("hw-repo-note").textContent = launch.repos.length ? "" : "No git repositories in this host's launch roots";
  renderLaunchForm();
}

function chosenRepo() {
  const p = $("hw-repo").value;
  return (launch.repos || []).find((r) => r.path === p) || null;
}

function renderWorkerFields() {
  const sel = $("hw-repo");
  const repos = launch.repos;
  const keep = sel.value;
  const sig = repos ? repos.map((r) => r.path).join("\n") : "loading";
  if (sel.dataset.sig !== sig) {
    sel.dataset.sig = sig;
    sel.innerHTML = !repos ? "<option value=\"\">Loading…</option>"
      : '<option value="">Choose a repository</option>' + repos.map((r) =>
        `<option value="${esc(r.path)}">${esc(r.name)} — ${esc(r.label || r.path)}</option>`).join("");
    if (repos && repos.some((r) => r.path === keep)) sel.value = keep;
  }
  const repo = chosenRepo();
  $("hw-base").placeholder = repo && repo.default_branch ? `Default: ${repo.default_branch}` : "Default: the default branch";
  const branch = $("hw-branch").value.trim();
  const branchOk = !branch || validBranch(branch);
  $("hw-branch").setAttribute("aria-invalid", String(!branchOk));
  $("hw-branch-hint").hidden = branchOk;
  const base = $("hw-base").value.trim();
  const baseOk = !base || validBranch(base);
  $("hw-base").setAttribute("aria-invalid", String(!baseOk));
  $("hw-base-hint").hidden = baseOk;
  $("hw-path").textContent = repo && branch && branchOk
    ? `New worktree ~/.worktrees/${repo.name}/${branch.replace(/\//g, "-")}, branch ${branch} from ${base || repo.default_branch || "the default branch"}`
    : "";
  return !!repo && !!branch && branchOk && baseOk && !!$("hw-brief").value.trim();
}

const AGENT_NAME_RE = /^[a-z][a-z0-9_-]{0,31}$/;

// launchBase is the API base for launching on host.
function launchBase(host) {
  return !host || host === state.localHost ? "/api/launch" : `/api/hosts/${encodeURIComponent(host)}/launch`;
}

// defaultLaunchName mirrors the server's default name (before any -2
// suffix for a name that is taken).
function defaultLaunchName(dir) {
  let n = baseName(dir).toLowerCase().replace(/[^a-z0-9_]+/g, "-").replace(/^[-_]+|[-_]+$/g, "");
  if (!/^[a-z]/.test(n)) n = ("agent-" + n).replace(/-+$/, "");
  return n.length > 28 ? n.slice(0, 28).replace(/[-_]+$/, "") : n;
}

function openLaunch() {
  if (launch.open) return;
  launch.open = true;
  history.pushState({ launch: true }, "", location.href);
  const f = activeHostFilter();
  launch.host = f !== "all" ? f : state.localHost;
  if (!launch.mode) {
    const m = lsGet(LAUNCH_MODE_KEY);
    launch.mode = LAUNCH_MODES.has(m) ? m : "agent";
  }
  $("ln-name").value = "";
  $("ln-prompt").value = "";
  $("hw-branch").value = "";
  $("hw-base").value = "";
  $("hw-brief").value = "";
  $("launch").hidden = false;
  document.body.classList.add("sheet-open");
  setLaunchHost(launch.host);
  $("ln-close").focus();
}

function hideLaunch() {
  launch.open = false;
  launch.seq++;
  launch.catSeq++;
  launch.picking = false;
  $("launch").hidden = true;
  document.body.classList.remove("sheet-open");
  if (!state.selected) $("btn-launch").focus();
  maybeReload(); // a deploy noticed while the sheet was open
}

// closeLaunch pops the sheet's history entry (popstate -> syncLaunch).
function closeLaunch() {
  if (launch.busy) return;
  if (history.state && history.state.launch) history.back();
  else hideLaunch();
}

function syncLaunch() {
  if (launch.open && !(history.state && history.state.launch)) hideLaunch();
}

function renderLaunchHosts() {
  const nav = $("ln-hosts");
  if (!multiHost()) { nav.hidden = true; nav.innerHTML = ""; return; }
  nav.innerHTML = state.hosts.map((h) => {
    const down = !h.local && h.status !== "ok";
    return `<button type="button" class="hf-chip${down ? " down" : ""}" data-host="${esc(h.name)}"` +
      ` aria-pressed="${launch.host === h.name}"${down ? " disabled" : ""}>${esc(h.name)}</button>`;
  }).join("");
  nav.hidden = false;
}

async function setLaunchHost(host) {
  launch.host = host;
  launch.opts = null;
  launch.dir = null;
  launch.kind = null;
  launch.path = "";
  launch.repos = null;
  launch.cat = null;
  launch.catSeq++;
  if (workerMode()) loadRepos(host);
  const seq = ++launch.seq;
  renderLaunchHosts();
  renderLaunchForm();
  $("ln-list").innerHTML = '<div class="ln-empty">Loading…</div>';
  $("ln-crumb").textContent = "";
  let o;
  try {
    o = await api(launchBase(host) + "/options");
  } catch (e) {
    if (seq !== launch.seq) return;
    $("ln-list").innerHTML = `<div class="ln-empty">${esc(e.message)}</div>`;
    return;
  }
  if (seq !== launch.seq) return;
  launch.opts = o;
  launch.kind = o.default_kind || (o.kinds || [])[0] || null;
  kindChosen();
  if (!o.enabled) {
    $("ln-list").innerHTML = `<div class="ln-empty">${esc(o.reason || "Launching is not available on this host")}</div>`;
    return;
  }
  // One root: start inside it rather than on a list of one.
  browseLaunch(o.roots.length === 1 ? o.roots[0].path : "");
}

function dirRow(d, recent) {
  const git = d.git ? '<span class="ln-git" title="git repository">git</span>' : "";
  const name = recent ? (d.label || d.path) : (d.name || d.label || d.path);
  return `<button type="button" class="ln-dir${recent ? " recent" : ""}" data-path="${esc(d.path)}" data-label="${esc(d.label || d.path)}">` +
    `<span class="ln-dname">${esc(name)}</span>${git}<span class="ln-chev" aria-hidden="true">›</span></button>`;
}

async function browseLaunch(path) {
  const o = launch.opts;
  if (!o || !o.enabled) return;
  const seq = ++launch.seq;
  launch.path = path;
  const startPath = o.roots.length === 1 ? o.roots[0].path : "";
  const recent = path === startPath && o.recent && o.recent.length
    ? '<div class="ln-head">Recent</div>' + o.recent.map((d) => dirRow(d, true)).join("") +
      `<div class="ln-head">${path ? esc(o.roots[0].label) : "Roots"}</div>`
    : "";
  let d;
  if (!path) {
    d = { path: "", entries: o.roots };
  } else {
    try {
      d = await api(launchBase(launch.host) + "/dirs?path=" + encodeURIComponent(path));
    } catch (e) {
      if (seq !== launch.seq) return;
      toast(e.message, true);
      return;
    }
  }
  if (seq !== launch.seq) return;
  launch.path = d.path;
  const atRoot = !d.parent;
  const canUp = !!d.parent || (d.path && o.roots.length > 1);
  $("ln-up").disabled = !canUp;
  $("ln-up").dataset.path = d.parent || "";
  launch.pathLabel = d.label || d.path;
  // Right-to-left so a long path is cut at its start, with the text itself
  // isolated left-to-right so "~/code" is not reordered.
  $("ln-crumb").innerHTML = `<bdi dir="ltr">${esc(d.path ? launch.pathLabel : "Launch roots")}</bdi>`;
  $("ln-crumb").title = d.path || "";
  $("ln-use").hidden = !d.path;
  const rows = (d.entries || []).map((e) => dirRow(e, false)).join("");
  $("ln-list").innerHTML = recent + (rows ||
    `<div class="ln-empty">${atRoot && !d.path ? "No launch roots" : "No subdirectories"}</div>`) +
    (d.truncated ? '<div class="ln-empty">Only the first entries are shown</div>' : "");
  $("ln-list").scrollTop = 0;
  renderLaunchForm();
}

function chooseLaunchDir(path, label) {
  launch.dir = path;
  launch.dirLabel = label || path;
  renderLaunchForm();
}

function renderLaunchForm() {
  const o = launch.opts;
  const kinds = (o && o.enabled && o.kinds) || [];
  $("ln-kinds").innerHTML = kinds.map((k) =>
    `<button type="button" class="hf-chip" data-kind="${esc(k)}" aria-pressed="${k === launch.kind}">${esc(k)}</button>`).join("") ||
    '<span class="ln-muted">—</span>';
  const chosen = $("ln-chosen");
  if (launch.dir) {
    chosen.textContent = launch.dirLabel;
    chosen.title = launch.dir;
    chosen.classList.add("set");
  } else {
    chosen.textContent = "No directory chosen yet";
    chosen.title = "";
    chosen.classList.remove("set");
  }
  $("ln-use").setAttribute("aria-pressed", String(!!launch.path && launch.dir === launch.path));
  $("ln-use").textContent = launch.path && launch.dir === launch.path ? "✓ Using this directory" : "Use this directory";
  $("ln-name").placeholder = launch.dir ? defaultLaunchName(launch.dir) : "Default: the directory's name";
  const name = $("ln-name").value.trim();
  const nameOk = !name || AGENT_NAME_RE.test(name);
  $("ln-name").setAttribute("aria-invalid", String(!nameOk));
  $("ln-name-hint").hidden = nameOk;
  renderModelBlock();
  const worker = workerMode();
  const coord = coordMode();
  $("ln-mode").hidden = ledger.on !== true;
  $("ln-mode").querySelectorAll("button[data-mode]").forEach((b) =>
    b.setAttribute("aria-pressed", String(b.dataset.mode === (worker ? "worker" : coord ? "coordinator" : "agent"))));
  $("ln-dir-block").hidden = worker || coord;
  $("ln-agent-extra").hidden = worker || coord;
  $("hw-top").hidden = !worker;
  $("hw-bottom").hidden = !worker;
  $("co-block").hidden = !coord;
  $("ln-title").textContent = worker ? "Hand off to a worker" : coord ? "Start a coordinator" : "New agent";
  $("ln-go").textContent = worker ? "Hand off" : coord ? "Start coordinator" : "Launch";
  if (coord) {
    const host = launch.host || state.localHost || "this host";
    const dir = (o && o.coordinator_dir) || "~";
    $("co-note").innerHTML = `Starts an agent named <b>coordinator</b> in <code>${esc(dir)}</code> on <b>${esc(host)}</b> ` +
      `and asks it to run <code>comebye playbook --harness ${esc(harnessFor(launch.kind))}</code>. ` +
      "It hands work to workers, follows them in the ledger and publishes briefings here; " +
      "removing anything stays with you.";
    $("ln-go").disabled = launch.busy || !launch.kind || !(o && o.enabled);
    return;
  }
  if (worker) {
    const ready = renderWorkerFields();
    $("ln-go").disabled = launch.busy || !ready || !launch.kind || !(o && o.enabled);
    return;
  }
  $("ln-go").disabled = launch.busy || !launch.dir || !launch.kind || !nameOk;
}

async function submitLaunch() {
  if (workerMode()) return submitHandoff();
  if (coordMode()) return submitCoordinator();
  if (launch.busy || !launch.dir || !launch.kind) return;
  const host = launch.host;
  const kind = launch.kind;
  const body = withModel({ dir: launch.dir, kind });
  const name = $("ln-name").value.trim();
  if (name) body.name = name;
  const prompt = $("ln-prompt").value;
  if (prompt.trim()) body.prompt = prompt;
  launch.busy = true;
  $("launch").classList.add("busy");
  $("ln-status").textContent = `Creating workspace and starting ${kind}…`;
  renderLaunchForm();
  let r;
  try {
    r = await api(launchBase(host), { method: "POST", timeout: 180000, body: JSON.stringify(body) });
  } catch (e) {
    toast(e.message, true);
    $("ln-status").textContent = "";
    return;
  } finally {
    launch.busy = false;
    $("launch").classList.remove("busy");
    renderLaunchForm();
  }
  $("ln-status").textContent = "";
  if (r.prompt_error) toast(r.prompt_error, true);
  else if (r.prompt_note) toast(r.prompt_note);
  // Leave the sheet in place of its history entry, then open the chat.
  if (history.state && history.state.launch) history.replaceState(null, "", location.pathname + location.search);
  hideLaunch();
  const pane = r.agent && r.agent.pane_id;
  if (pane) openLaunched(host, pane, r.agent.name || kind);
}

// submitHandoff hands the brief to a new worker on the chosen host: the hub
// creates (or reopens) the branch's worktree there, starts the agent in it,
// sends the brief in the handoff template and records it in the ledger.
async function submitHandoff() {
  const repo = chosenRepo();
  const branch = $("hw-branch").value.trim();
  const brief = $("hw-brief").value;
  if (launch.busy || !repo || !launch.kind || !validBranch(branch) || !brief.trim()) return;
  const host = launch.host || state.localHost;
  const kind = launch.kind;
  const body = withModel({ host, repo: repo.path, branch, kind, brief });
  const base = $("hw-base").value.trim();
  if (base) body.base = base;
  launch.busy = true;
  $("launch").classList.add("busy");
  $("ln-status").textContent = `Creating the worktree and starting ${kind}…`;
  renderLaunchForm();
  let r;
  try {
    r = await api("/api/handoffs", { method: "POST", body: JSON.stringify(body) });
  } catch (e) {
    toast(e.message, true);
    $("ln-status").textContent = "";
    return;
  } finally {
    launch.busy = false;
    $("launch").classList.remove("busy");
    renderLaunchForm();
  }
  $("ln-status").textContent = "";
  if (r.entry && r.entry.error) toast(r.entry.error, true);
  if (history.state && history.state.launch) history.replaceState(null, "", location.pathname + location.search);
  hideLaunch();
  ledger.loadedAt = 0;
  loadLedger();
  const a = r.worker && r.worker.agent;
  if (a && a.pane_id) openLaunched(host, a.pane_id, a.name || branch);
}

// submitCoordinator starts a coordinator on the chosen host: the server
// picks the directory (-coordinator-dir), the name, the harness's
// permission arguments and a first prompt that points at the playbook,
// and the hub records the coordinator role.
async function submitCoordinator() {
  if (launch.busy || !launch.kind) return;
  const host = launch.host || state.localHost;
  const kind = launch.kind;
  launch.busy = true;
  $("launch").classList.add("busy");
  $("ln-status").textContent = `Starting a ${kind} coordinator…`;
  renderLaunchForm();
  let r;
  try {
    r = await api(launchBase(host), { method: "POST", timeout: 180000, body: JSON.stringify(withModel({ coordinator: true, kind })) });
  } catch (e) {
    toast(e.message, true);
    $("ln-status").textContent = "";
    return;
  } finally {
    launch.busy = false;
    $("launch").classList.remove("busy");
    renderLaunchForm();
  }
  $("ln-status").textContent = "";
  if (r.prompt_error) toast(r.prompt_error, true);
  else if (r.prompt_note) toast(r.prompt_note);
  if (history.state && history.state.launch) history.replaceState(null, "", location.pathname + location.search);
  hideLaunch();
  ledger.loadedAt = 0;
  loadLedger(); // the new role
  const pane = r.agent && r.agent.pane_id;
  if (pane) openLaunched(host, pane, r.agent.name || "coordinator");
}

// openLaunched opens the new agent's chat as soon as the agent list (SSE)
// has it; herdr reports it within a second or two.
function openLaunched(host, pane, label) {
  const r = { host: host && host !== state.localHost ? host : null, pane };
  const until = Date.now() + 15000;
  const tick = () => {
    const a = routedAgent(r);
    if (a) {
      if (!state.selected && !launch.open) openDetail(agentId(a));
      return;
    }
    if (Date.now() > until) { toast(`${label} started, but has not appeared in the list yet`, true); return; }
    if (Date.now() > until - 10000) refreshAgents().catch(() => {});
    setTimeout(tick, 500);
  };
  toast(`Started ${label}`);
  tick();
}

/* ---------- switching a running agent's model ---------- */
//
// "Switch model…" in the agent's menu opens the same list. The server types
// the agent's own slash command (claude: /model, pi: /model and /thinking),
// so the agent must be idle; for a worker the ledger then shows the new model.

const SWITCH_KINDS = ["pi", "claude"];
const sw = { open: false, id: null, kind: null, host: null, cat: null, model: "", thinking: "", q: "", busy: false, seq: 0 };

function switchable(a) { return !!a && SWITCH_KINDS.includes(a.agent); }

function openSwitch() {
  const a = agentById(state.selected);
  toggleDetailMenu(false);
  if (!switchable(a) || sw.open) return;
  Object.assign(sw, { open: true, id: state.selected, kind: a.agent, host: a.host || state.localHost, cat: { loading: true }, model: "", thinking: "", q: "", busy: false });
  const seq = ++sw.seq;
  history.pushState({ switch: true }, "", location.href);
  $("sw-q").value = "";
  $("switch").hidden = false;
  document.body.classList.add("sheet-open");
  renderSwitch();
  $("sw-close").focus();
  api(modelsBase(sw.host) + "?kind=" + encodeURIComponent(sw.kind))
    .then((d) => (d.catalogs || [])[0] || { error: "the host sent no model list" }, (e) => ({ error: e.message }))
    .then((cat) => { if (seq === sw.seq) { sw.cat = cat; renderSwitch(); } });
}

function hideSwitch() {
  sw.open = false;
  sw.seq++;
  $("switch").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}

function closeSwitch() {
  if (sw.busy) return;
  if (history.state && history.state.switch) history.back();
  else hideSwitch();
}

function syncSwitch() {
  if (sw.open && !(history.state && history.state.switch)) hideSwitch();
}

function renderSwitch() {
  const a = agentById(sw.id);
  const idle = !!a && a.agent_status === "idle";
  const name = a ? (a.name || a.pane_id) : "this agent";
  $("sw-title").textContent = "Switch model";
  $("sw-note").textContent = !a ? "The agent is gone."
    : idle ? `${name} is idle. It switches by running its own /model command.`
    : `${name} is ${a.agent_status}: switch its model when it is idle.`;
  if (sw.cat && sw.cat.error) $("sw-note").textContent += ` Could not list models (${sw.cat.error}); you can still type a name.`;
  fillModelList($("sw-list"), sw.cat, sw.q, sw.model, false);
  const pi = sw.kind === "pi";
  $("sw-thinking").hidden = !pi;
  if (pi) {
    $("sw-think-chips").innerHTML = ((sw.cat && sw.cat.thinking_levels) || THINKING_LEVELS).map((l) =>
      `<button type="button" class="hf-chip" data-think="${esc(l)}" aria-pressed="${l === sw.thinking}">${esc(l)}</button>`).join("");
  }
  const what = [sw.model, pi && sw.thinking ? `thinking ${sw.thinking}` : ""].filter(Boolean).join(", ");
  $("sw-go").textContent = what ? `Switch to ${what}` : "Switch";
  $("sw-go").disabled = sw.busy || !idle || !(sw.model || sw.thinking);
}

async function submitSwitch() {
  if (sw.busy || !(sw.model || sw.thinking)) return;
  const id = sw.id;
  const body = {};
  if (sw.model) body.model = sw.model;
  if (sw.kind === "pi" && sw.thinking) body.thinking = sw.thinking;
  sw.busy = true;
  $("switch").classList.add("busy");
  $("sw-status").textContent = "Sending the switch to the agent…";
  renderSwitch();
  try {
    await api(`${pathFor(id)}/model`, { method: "POST", body: JSON.stringify(body) });
  } catch (e) {
    toast(e.message, true);
    return;
  } finally {
    sw.busy = false;
    $("switch").classList.remove("busy");
    $("sw-status").textContent = "";
    renderSwitch();
  }
  toast(`Switched to ${[body.model, body.thinking && "thinking " + body.thinking].filter(Boolean).join(", ")}`);
  closeSwitch(); // pops the sheet's history entry; the chat stays open under it
  ledger.loadedAt = 0;
  loadLedger();
}

/* ---------- wiring ---------- */

/* ---------- new deploys ---------- */
//
// An installed iOS app stays in memory across server deploys, so an old
// app.js would keep talking to a newer API. The server reports its build
// version (a hash of these assets) in X-App-Version on /api responses and
// in each "agents" event. The first version seen is the one this page was
// loaded with; a different one later means a deploy, and the page reloads:
// right away when nothing is in progress, otherwise via a banner (or on
// the next return to the list). Each version gets at most one automatic
// reload per tab session, so a server that keeps changing cannot loop us.

const ver = { loaded: null, pending: null };
const RELOADED_KEY = "herdr-reloaded-for";

function noteVersion(v) {
  if (!v) return;
  if (!ver.loaded) { ver.loaded = v; return; }
  if (v === ver.loaded) return; // e.g. rolled back before we reloaded
  if (v === ver.pending) return;
  ver.pending = v;
  maybeReload();
}

// busy: the user is reading an agent or something would be lost.
function busy() {
  return !!state.selected || state.sending || answer.inflight || !!promptValue().trim() || launch.open || review.open || sw.open;
}

function maybeReload() {
  if (!ver.pending) return;
  if (!busy() && reloadOnce(ver.pending)) return;
  const b = $("update-banner");
  if (b) b.hidden = false;
}

function reloadOnce(v) {
  try {
    const done = JSON.parse(sessionStorage.getItem(RELOADED_KEY) || "[]");
    if (done.includes(v)) return false; // already reloaded for it once: banner only
    done.push(v);
    sessionStorage.setItem(RELOADED_KEY, JSON.stringify(done.slice(-10)));
  } catch {
    return false; // no way to remember: never reload unasked
  }
  location.reload();
  return true;
}

// checkVersion runs on resume: iOS can wake a page that slept for hours,
// and the event stream may take a moment to come back.
function checkVersion() {
  if (document.hidden) return;
  api("/api/version").then((d) => noteVersion(d.version)).catch(() => {});
}

window.addEventListener("DOMContentLoaded", () => {
  $("update-banner").addEventListener("click", () => location.reload());
  document.addEventListener("visibilitychange", checkVersion);
  window.addEventListener("pageshow", (ev) => { if (ev.persisted) checkVersion(); });
});

window.addEventListener("DOMContentLoaded", () => {
  renderList();
  // Probe first so a 401 is reported to the user (EventSource alone would
  // just fail and retry silently).
  api("/api/agents")
    .catch((e) => { if (e.status === 401) toast(UNAUTHORIZED_MSG, true); })
    .finally(connectEvents);
  startWatchdog();
  setInterval(() => { if (!document.hidden && (!state.selected || isDesktop())) renderList(); }, 60000);
  document.addEventListener("visibilitychange", resume);
  // In the background nothing is shown: close the live connection and the
  // timers so the radio and CPU can sleep (notifications arrive by push
  // regardless). resume() reconnects and catches up when visible again.
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) return;
    suspendEvents();
    clearInterval(activity.timer);
    activity.timer = null;
    clearInterval(subagentTimer);
    subagentTimer = null;
  });
  window.addEventListener("pageshow", (ev) => { if (ev.persisted) resume(); }); // skip the initial load
  window.addEventListener("online", resume);
  document.addEventListener("keydown", onListArrow);
  document.addEventListener("keydown", onNextWaiting);
  document.addEventListener("keydown", onTodoKey);
  desktopMQ.addEventListener("change", () => { setView(viewFromHash()); renderList(); renderBrief(); markSelectedRow(); syncDesktopBriefs(); });
  window.addEventListener("offline", () => setConn(false));

  $("back").addEventListener("click", () => {
    // Pop our own history entry (popstate then closes the view); a deep
    // link has no list entry behind it, so just close in place.
    if (state.pushedRoute) history.back();
    else closeDetail();
  });
  $("sv-back").addEventListener("click", () => {
    if (history.state && history.state.subagent) history.back(); // popstate -> syncSubview
    else hideSubview();
  });
  document.addEventListener("click", (ev) => {
    const b = ev.target.closest(".sa-steps");
    const card = b && b.closest(".toolcard.subagent");
    if (card && card._m) openSubview(card._m);
  });
  window.addEventListener("popstate", route);
  window.addEventListener("hashchange", route);
  initBriefs();
  initRecall();
  initPlans();
  route(); // cold start on #/agent/[<host>/]<pane>
  $("btn-terminal").addEventListener("click", () => {
    // One-way escape hatch for debugging: show the raw pane.
    if (state.chat) state.chat.terminal = true;
    showTerminal();
    refreshOutput();
  });
  $("attach-keys").addEventListener("click", () => { toggleAttachMenu(false); toggleKeypad(); });
  $("keypad").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-key]");
    if (b) sendKeys([b.dataset.key], b.textContent, b);
  });
  $("btn-send").addEventListener("click", () => {
    if (stopMode()) sendKeys(["esc"], "Stop");
    else sendPrompt();
  });
  $("prompt-text").addEventListener("input", () => { normalizeComposer(); updateSendButton(); });
  $("prompt-text").addEventListener("paste", onComposerPaste);
  $("prompt-text").addEventListener("drop", onComposerDrop);
  $("btn-attach").addEventListener("click", (ev) => { ev.stopPropagation(); toggleAttachMenu(); });
  $("attach-paste").addEventListener("click", pasteImageFromClipboard);
  $("attach-pick").addEventListener("click", () => { toggleAttachMenu(false); $("attach-file").click(); });
  document.addEventListener("click", (ev) => {
    if (!$("attach-menu").hidden && !$("attach-menu").contains(ev.target)) toggleAttachMenu(false);
  });
  $("attach-file").addEventListener("change", (ev) => {
    attachImages(ev.target.files);
    ev.target.value = ""; // picking the same file again still fires change
  });
  $("prompt-text").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) { ev.preventDefault(); sendPrompt(); }
  });

  // The service worker makes the app installable and receives Web Push.
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("sw.js").catch(() => {});
    // A notification tap while the app is open: route in place.
    navigator.serviceWorker.addEventListener("message", (ev) => {
      if (!ev.data || ev.data.type !== "navigate") return;
      let u;
      try { u = new URL(ev.data.url, location.origin); } catch { return; }
      if (u.origin !== location.origin) return;
      if (u.hash.startsWith("#/agent/")) {
        if (location.hash !== u.hash) location.hash = u.hash; // hashchange -> route()
      } else if (state.selected) {
        closeDetail();
      }
    });
  }

  $("btn-launch").addEventListener("click", openLaunch);
  $("ln-close").addEventListener("click", closeLaunch);
  $("launch").addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape") return;
    ev.preventDefault();
    // Escape closes the open model list first, then the sheet.
    if (launch.picking) { launch.picking = false; renderLaunchForm(); $("ln-model-btn").focus(); } else closeLaunch();
  });
  $("ln-hosts").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-host]");
    if (b && !launch.busy && b.dataset.host !== launch.host) setLaunchHost(b.dataset.host);
  });
  $("ln-kinds").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-kind]");
    if (b && !launch.busy && b.dataset.kind !== launch.kind) { launch.kind = b.dataset.kind; kindChosen(); }
  });
  $("ln-model-btn").addEventListener("click", () => { if (!launch.busy) togglePicker(); });
  $("ln-model-q").addEventListener("input", () => { launch.q = $("ln-model-q").value; renderModelList(); });
  $("ln-mlist").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-model]");
    if (b && !launch.busy) pickModel(b.dataset.model);
  });
  $("ln-think-chips").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-think]");
    if (b && !launch.busy) { launch.thinking = b.dataset.think; rememberModel(); renderLaunchForm(); }
  });
  $("ln-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("button.ln-dir");
    if (!b) return;
    // A recent directory is usually the one you want: choose it as well.
    if (b.classList.contains("recent")) chooseLaunchDir(b.dataset.path, b.dataset.label);
    browseLaunch(b.dataset.path);
  });
  $("ln-up").addEventListener("click", () => browseLaunch($("ln-up").dataset.path || ""));
  $("ln-use").addEventListener("click", () => chooseLaunchDir(launch.path, launch.pathLabel));
  $("ln-name").addEventListener("input", renderLaunchForm);
  $("ln-go").addEventListener("click", submitLaunch);
  $("ln-mode").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-mode]");
    if (b && !launch.busy) setLaunchMode(b.dataset.mode);
  });
  for (const id of ["hw-repo", "hw-branch", "hw-base", "hw-brief"]) {
    $(id).addEventListener(id === "hw-repo" ? "change" : "input", renderLaunchForm);
  }

  $("rtr-banner").addEventListener("click", openReview);
  $("rv-close").addEventListener("click", closeReview);
  $("review").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") { ev.preventDefault(); closeReview(); }
  });
  $("rv-all").addEventListener("click", () => confirmRemovals(safeIds(), false));
  $("rv-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-act]");
    if (b) reviewAction(b.dataset.act, b.dataset.id);
  });
  $("d-refresh").addEventListener("click", refreshView);
  $("d-more").addEventListener("click", (ev) => { ev.stopPropagation(); toggleDetailMenu(); });
  $("d-mark").addEventListener("click", toggleMark);
  $("d-markbar").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-mb]");
    if (!b || !state.selected) return;
    if (b.dataset.mb === "unmark") unmarkAgent(state.selected);
  });
  $("row-mark").addEventListener("click", rowMenuMark);
  initPopovers();
  $("mk-close").addEventListener("click", closeMark);
  $("mk-go").addEventListener("click", submitMark);
  $("mk").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeMark(); } });
  $("btn-fleet").addEventListener("click", (ev) => { ev.stopPropagation(); toggleFleetMenu(); });
  $("f-sweep").addEventListener("click", fleetAction(openSweep));
  $("f-review").addEventListener("click", fleetAction(openReview));
  $("nav").addEventListener("click", onNavClick);
  for (const id of ["ps-body", "br-body"]) {
    $(id).addEventListener("click", onStatusClick);
    $(id).addEventListener("change", onStatusChange);
    $(id).addEventListener("toggle", onStatusToggle, true);
  }
  $("f-brief").addEventListener("click", fleetAction(fleetBrief));
  $("f-briefs").addEventListener("click", fleetAction(() => openBriefs(true)));
  $("f-notify").addEventListener("click", fleetAction(() => toggleNotifyPanel(true)));
  $("f-refresh").addEventListener("click", fleetAction(fleetRefresh));
  $("f-theme").addEventListener("click", fleetAction(() => toggleThemePanel(true)));
  initThemePanel();
  applyTheme();
  $("btn-fleet").addEventListener("keydown", (ev) => { if (ev.key === "ArrowDown" && $("f-menu").hidden) { ev.preventDefault(); toggleFleetMenu(true); } });
  $("sweep-close").addEventListener("click", closeSweep);
  $("sweep-go").addEventListener("click", sweepGo);
  $("sweep").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeSweep(); } });
  $("d-model").addEventListener("click", openSwitch);
  $("sw-close").addEventListener("click", closeSwitch);
  $("switch").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeSwitch(); } });
  $("sw-q").addEventListener("input", () => { sw.q = $("sw-q").value; renderSwitch(); });
  $("sw-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-model]");
    if (b && !sw.busy) { sw.model = b.dataset.model; renderSwitch(); }
  });
  $("sw-think-chips").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-think]");
    if (b && !sw.busy) { sw.thinking = sw.thinking === b.dataset.think ? "" : b.dataset.think; renderSwitch(); }
  });
  $("sw-go").addEventListener("click", submitSwitch);
  setInterval(relTick, 2000);
  $("d-role").addEventListener("click", toggleRole);
  $("d-sched").addEventListener("click", editBriefSchedule);
  $("briefing").addEventListener("click", onBriefClick);
  for (const id of ["todo", "pt-body"]) {
    const el = $(id);
    if (!el) continue;
    el.addEventListener("click", onTodoClick);
    el.addEventListener("input", onTodoInput);
  }
  loadLedger();

  $("btn-notify").addEventListener("click", () => toggleNotifyPanel());
  $("notify-enable").addEventListener("click", enablePush);
  $("notify-disable").addEventListener("click", disablePush);
  $("notify-test").addEventListener("click", testPush);
  initPush();

  document.addEventListener("visibilitychange", () => reportPresence());
  window.addEventListener("pagehide", () => { reportPresence(false, true); suspendEvents(); });
});
