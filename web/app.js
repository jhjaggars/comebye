/* herdr phone app — vanilla JS, no build step. */
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
  sending: false,
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

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
  });
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

function esc(s) {
  const d = document.createElement("div");
  d.textContent = s ?? "";
  return d.innerHTML;
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
  return { repo: baseName(a.cwd) };
}

function subLine(a) {
  const { repo, branch } = repoLabel(a);
  const kind = a.agent ? esc(a.agent) : "";
  const hostEl = multiHost() && a.host ? `<span class="host-chip">${esc(a.host)}</span>` : "";
  const repoEl = `<span class="repo">${esc(repo)}</span>`;
  const chip = branch ? `<span class="wt-chip">⑂ ${esc(branch)}</span>` : "";
  const sep = kind ? '<span>·</span>' : "";
  return `<div class="sub">${hostEl}${repoEl}${chip}${sep}${kind}</div>`;
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
function setAgents(d) {
  state.agents = d.agents || [];
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
  return (mdLib = { m, P });
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
  const chip = (name, label, agents, down) => {
    const need = agents.some((a) => a.agent_status === "blocked");
    return `<button type="button" class="hf-chip${down ? " down" : ""}" data-host="${esc(name)}"` +
      ` aria-pressed="${active === name}">${need ? '<span class="need" aria-label="needs input"></span>' : ""}` +
      `${esc(label)} <span class="n">${down ? "down" : agents.length}</span></button>`;
  };
  nav.innerHTML = chip("all", "All", state.agents, false) + state.hosts.map((h) =>
    chip(h.name, h.name, state.agents.filter((a) => a.host === h.name), !h.local && h.status !== "ok")).join("");
  nav.hidden = false;
  nav.querySelectorAll(".hf-chip").forEach((el) =>
    el.addEventListener("click", () => setHostFilter(el.dataset.host)));
}

function renderList() {
  const wrap = $("agents");
  renderHostFilter();
  const filter = activeHostFilter();
  const agents = state.agents.filter((a) => filter === "all" || a.host === filter).sort((a, b) =>
    (STATUS_ORDER[a.agent_status] ?? 9) - (STATUS_ORDER[b.agent_status] ?? 9) ||
    // Within a status, most recently active first, so the agents that just
    // went idle sit above the ones that have been idle for days.
    activeMs(b) - activeMs(a) ||
    agentId(a).localeCompare(agentId(b)));
  const nBlocked = agents.filter((a) => a.agent_status === "blocked").length;
  const nWorking = agents.filter((a) => a.agent_status === "working").length;
  $("count").innerHTML = agents.length
    ? (nBlocked ? `<span class="need">${nBlocked} need${nBlocked === 1 ? "s" : ""} input</span> · ` : "") +
      (nWorking ? `<span class="run">${nWorking} working</span> · ` : "") +
      agents.length + " agent" + (agents.length === 1 ? "" : "s")
    : "";
  // Peers the hub cannot reach right now: one collapsed row each.
  const downRows = state.hosts.filter((h) => !h.local && h.status !== "ok" &&
    (filter === "all" || h.name === filter)).map((h) => {
    const seen = h.last_seen ? Date.parse(h.last_seen) : NaN;
    const what = h.status === "connecting" ? "connecting…" : "unreachable" +
      (Number.isNaN(seen) ? "" : ` · last seen ${fmtAgo(seen)} ago`);
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
  if (!agents.length) {
    wrap.innerHTML = `<div class="empty">${state.unauthorized ? esc(UNAUTHORIZED_MSG) : "No agents running"}</div>` + downRows;
    return;
  }
  wrap.innerHTML = agents.map((a) => `
    <div class="agent ${esc(a.agent_status)}" role="button" tabindex="0" data-id="${esc(agentId(a))}">
      <span class="dot ${esc(a.agent_status)}"></span>
      <div class="meta">
        <div class="name">${esc(a.name || a.terminal_title_stripped || a.display_agent || a.agent || a.pane_id)}</div>
        ${subLine(a)}
      </div>
      <span class="status ${esc(a.agent_status)}">${esc(a.agent_status)}${
        activeMs(a) && (a.agent_status === "idle" || a.agent_status === "done")
          ? `<br><span class="ago">${fmtAgo(activeMs(a))}</span>`
          : a.agent_status === "working" && sinceMs(a)
            ? `<br><span class="ago">for ${fmtAgo(sinceMs(a))}</span>` : ""}</span>
    </div>`).join("") + downRows;
  wrap.querySelectorAll(".agent[data-id]").forEach((el) => {
    el.addEventListener("click", () => openDetail(el.dataset.id));
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openDetail(el.dataset.id); }
    });
  });
}

/* ---------- detail view ---------- */

function agentById(id) {
  return state.agents.find((a) => agentId(a) === id);
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
  const r = parseRoute();
  if (!r) {
    state.pendingRoute = null;
    if (state.selected) closeDetail();
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
  if (!r) return;
  const a = routedAgent(r);
  if (a) { state.pendingRoute = null; openDetail(agentId(a)); return; }
  const h = r.host && hostStatus(r.host);
  if (h && h.status === "connecting") return; // the hub has not reached that peer yet; wait
  state.pendingRoute = null;
  toast(h && h.status === "down" ? `${r.host} is unreachable` : "that agent is gone", true);
  history.replaceState(null, "", location.pathname + location.search);
}

function openDetail(id) {
  const a = agentById(id);
  if (!a) return;
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
  renderDetail();
  loadChat();
  startPolling();
  reportPresence();
}

function closeDetail() {
  toggleKeypad(false);
  clearInterval(activity.timer);
  activity.timer = null;
  activity.seenAt = 0;
  const header = document.querySelector("#detail header");
  if (header) header.classList.remove("busy");
  state.selected = null;
  state.chat = null;
  state.ask = null;
  state.pushedRoute = false;
  if (parseRoute()) history.replaceState(null, "", location.pathname + location.search);
  stopPolling();
  updateAnswerCard();
  $("detail").style.display = "none";
  $("list").style.display = "block";
  reportPresence();
  maybeReload(); // a deploy noticed while on the detail view
}

function renderDetail() {
  const a = agentById(state.selected);
  if (!a) {
    const h = hostStatus(hostOf(state.selected));
    if (h && !h.local && h.status !== "ok") toast(`${h.name} is unreachable`, true);
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
}

/* ---------- chat view (Pi session transcript) ---------- */

async function loadChat(reset = true) {
  if (!state.selected) return;
  const pane = state.selected;
  if (reset) {
    state.chat = { rendered: new Set(), skip: 0, more: false, terminal: false, pending: [] };
    $("chat").innerHTML = "";
  }
  const chat = state.chat;
  try {
    const t = await api(`${pathFor(pane)}/transcript?limit=100&skip=${chat.skip}`);
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
  chat.more = t.more;
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

  if (t.messages.length) chat.last = t.messages[t.messages.length - 1];
  renderActivity();

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

const activity = { timer: null, seenAt: 0, pane: null };

function isWorking(a) { return !!a && a.agent_status === "working" && !state.ask; }

// activityStep describes what the agent is doing, from the newest message.
function activityStep() {
  const m = state.chat && state.chat.last;
  if (m && m.kind === "tool" && !m.result && !m.err && !m.ms) {
    return { verb: "Running", what: m.tool || "a tool", detail: m.args || "" };
  }
  return { verb: "Thinking", what: "", detail: "" };
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

function renderActivity() {
  const a = agentById(state.selected);
  const working = isWorking(a);
  const header = document.querySelector("#detail header");
  if (header) header.classList.toggle("busy", working);
  const chatEl = $("chat");
  let row = chatEl.querySelector(".activity");
  if (!working || !state.chat || state.chat.terminal) {
    if (row) row.remove();
    if (!working) { activity.seenAt = 0; clearInterval(activity.timer); activity.timer = null; }
    return;
  }
  if (!row) {
    row = document.createElement("div");
    row.className = "activity";
    row.setAttribute("role", "status");
    row.innerHTML = '<span class="dot working" aria-hidden="true"></span>' +
      '<span class="act-verb"></span><span class="act-what"></span>' +
      '<span class="act-time"></span><span class="act-detail"></span>';
  }
  const anchor = chatEl.querySelector(".jump-anchor");
  if (row.nextSibling !== anchor || row.parentNode !== chatEl) chatEl.insertBefore(row, anchor);
  const step = activityStep();
  row.querySelector(".act-verb").textContent = step.verb;
  row.querySelector(".act-what").textContent = step.what;
  row.querySelector(".act-detail").textContent = step.detail;
  row.querySelector(".act-detail").hidden = !step.detail;
  tickActivity();
  if (!activity.timer) activity.timer = setInterval(tickActivity, 1000);
}

function tickActivity() {
  const a = agentById(state.selected);
  const row = $("chat").querySelector(".activity");
  if (!row || !isWorking(a)) return;
  row.querySelector(".act-time").textContent = fmtClock(Date.now() - workingSince(a));
}

// toolMeta is a tool card's status: duration, "failed" for a call whose
// result is an error (red card), or still running.
function toolMeta(m) {
  if (m.err) return "failed" + (m.ms ? " · " + fmtMs(m.ms) : "");
  return m.ms ? fmtMs(m.ms) : (m.result ? "ran" : "running…");
}

function patchToolCard(m) {
  const el = document.querySelector(`#chat [data-id="${CSS.escape(m.id)}"]`);
  if (!el) return;
  el.classList.toggle("err", m.err);
  const meta = el.querySelector(".toolmeta");
  if (meta) meta.textContent = toolMeta(m);
  const body = el.querySelector(".toolresult");
  if (body && !body.textContent && m.result) body.textContent = m.result;
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
      `<div class="msgtime">${working ? "queued — the agent picks it up at its next step" : "sent"}</div>`;
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
      const d = document.createElement("details");
      d.className = "toolcard" + (m.err ? " err" : "");
      d.dataset.id = m.id;
      const meta = toolMeta(m);
      d.innerHTML =
        `<summary><span class="toolname">${esc(m.tool || "tool")}</span>` +
        `<span class="toolargs">${esc(m.args || "")}</span>` +
        `<span class="toolmeta">${esc(meta)}</span></summary>` +
        `<div class="toolresult">${esc(m.result || "(no output)")}</div>`;
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

async function refreshOutput() {
  if (!state.selected || !(state.chat && state.chat.terminal)) return;
  try {
    const read = await api(`${pathFor(state.selected)}/output?lines=400`);
    const el = $("output");
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    el.textContent = read.text || "(no output)";
    if (atBottom) el.scrollTop = el.scrollHeight;
  } catch (e) {
    if (e.status === 404) { toast("agent is gone", true); closeDetail(); }
    else toast("read failed: " + e.message, true);
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
    if (a.agent_status === "working" || a.agent_status === "blocked") {
      if (state.chat && state.chat.terminal) refreshOutput();
      else loadChat(false); // append-only; new messages diff in by id
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
  inflight: false, // an answer POST is running
  moreOpen: false, // "More keys…" disclosure state (kept across re-renders)
  snapText: "loading screen…",
};

function needsAnswer(a) {
  if (!a) return false;
  if (a.agent_status === "blocked") return true;
  return !!state.ask && a.agent_status === "working";
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
    stopAnswerPoll();
    return;
  }
  if (answer.pane !== state.selected) {
    answer.pane = state.selected;
    answer.dialog = null;
    answer.loaded = false;
    answer.moreOpen = false;
    answer.snapText = "loading screen…";
  }
  renderAnswerCard(a);
  el.style.display = "block";
  answer.shown = true;
  startAnswerPoll();
}

// renderAnswerCard re-renders only when what it shows changed, so focus and
// the disclosure state survive SSE updates and polls.
function renderAnswerCard(a) {
  const el = $("answer");
  const d = answer.dialog;
  const sig = JSON.stringify([answer.loaded, d && d.fingerprint, d && d.selected, a.agent_status, a.state_labels, state.ask && state.ask.tool_call_id]);
  if (answer.shown && sig === answer.sig) return;
  answer.sig = sig;
  el.innerHTML = answerCardHTML(a, d);
  el.querySelectorAll(".pad .btn").forEach((b) =>
    b.addEventListener("click", () => padPress(b.dataset.key, b.textContent)));
  el.querySelectorAll("button.opt").forEach((b) =>
    b.addEventListener("click", () => tapAnswer(b.dataset.action, Number(b.dataset.i))));
  const more = el.querySelector("details.ans-more");
  if (more) more.addEventListener("toggle", () => {
    answer.moreOpen = more.open;
    if (more.open) refreshSnapshot();
  });
  setAnswerBusy(answer.inflight);
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
    d.options.forEach((o, i) => {
      const cur = i === d.selected ? " cur" : "";
      const desc = o.description ? `<span class="opt-desc">${esc(o.description)}</span>` : "";
      if (o.role === "other") {
        html += `<div class="opt-note">✎ ${esc(o.label)} <span class="opt-desc">${esc(h.other)}</span></div>`;
        return;
      }
      if (!d.answerable) {
        html += `<div class="opt ro${cur}"><span class="opt-label">${esc(o.label)}</span>${desc}</div>`;
        return;
      }
      if (o.role === "next") {
        html += `<button type="button" class="opt submit${cur}" data-action="submit" data-i="${i}"><span class="opt-label">Submit selection</span></button>`;
        return;
      }
      const box = d.multi ? `<span class="opt-box">${o.checked ? "☑" : "☐"}</span>` : "";
      html += `<button type="button" class="opt${cur}${o.checked ? " checked" : ""}" data-action="${d.multi ? "toggle" : "select"}" data-i="${i}"` +
        `${i === d.selected ? ' aria-current="true"' : ""}>${box}<span class="opt-label">${esc(o.label)}</span>${desc}</button>`;
    });
    html += "</div>";
    if (d.cancel_label) {
      html += `<button type="button" class="opt cancel" data-action="cancel" data-i="-1">${esc(h.cancel)}</button>`;
    }
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
      h.pad = "↑/↓ then Enter picks; Esc cancels. Check the screen below.";
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

function setAnswerBusy(busy) {
  document.querySelectorAll("#answer button.opt").forEach((b) => { b.disabled = busy; });
}

const KEY_GLYPH = { up: "↑", down: "↓", enter: "Enter", esc: "Esc", space: "Space" };

async function tapAnswer(action, index) {
  const d = answer.dialog;
  const pane = state.selected;
  if (!d || !pane || answer.inflight) return;
  answer.inflight = true;
  setAnswerBusy(true);
  try {
    const res = await fetch(`${pathFor(pane)}/answer`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, index, fingerprint: d.fingerprint }),
    });
    let body = {};
    try { body = await res.json(); } catch {}
    if (state.selected !== pane) return;
    if (res.status === 401) { setUnauthorized(true); toast(UNAUTHORIZED_MSG, true); return; }
    if (res.ok) {
      toast("sent " + (body.keys || []).map((k) => KEY_GLYPH[k] || k).join(" "));
      setTimeout(refreshDialog, 400);
      if (answer.moreOpen) setTimeout(refreshSnapshot, 450);
      if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
      return;
    }
    if (res.status === 409 && body.code === "dialog_changed") {
      toast("the question changed — check it again", true);
      setDialog(pane, body.dialog || null);
    } else if (res.status === 409 && body.code === "duplicate") {
      toast("already answered", true);
      setTimeout(refreshDialog, 400);
    } else if (res.status === 409 && body.code === "no_dialog") {
      toast("the dialog is gone", true);
      setDialog(pane, null);
    } else {
      toast("answer failed: " + (body.message || body.code || res.statusText), true);
      if (body.dialog !== undefined) setDialog(pane, body.dialog);
    }
  } catch (e) {
    toast("answer failed: " + e.message, true);
  } finally {
    answer.inflight = false;
    setAnswerBusy(false);
  }
}

function setDialog(pane, d) {
  if (state.selected !== pane || answer.pane !== pane) return;
  answer.dialog = d;
  answer.loaded = true;
  const a = agentById(pane);
  if (a && needsAnswer(a)) renderAnswerCard(a);
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

async function padPress(key, label) {
  await sendKeys([key], label);
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

async function sendPrompt() {
  const text = promptValue().trim();
  if (!text || state.sending) return;
  if (state.ask) {
    // herdr only refuses prompts while `blocked`; an open questionnaire
    // reports `working`, and pasted text + Enter would land in the dialog.
    toast("answer the question first (or Esc to cancel it)", true);
    return;
  }
  state.sending = true;
  $("btn-send").disabled = true;
  // Shown before the POST returns: a fast transcript poll may already
  // carry the message, and settling needs the bubble to exist.
  const pending = addPending(text);
  try {
    await api(`${pathFor(state.selected)}/prompt`, {
      method: "POST",
      body: JSON.stringify({ text }),
    });
    setPromptValue("");
    // Echo the sent prompt locally; the transcript poll will pick it up too.
    if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
    else setTimeout(refreshOutput, 600);
  } catch (e) {
    dropPending(pending);
    if (e.status === 409) {
      toast("agent is waiting for input — answer the dialog or press Esc first", true);
    } else {
      toast("prompt failed: " + e.message, true);
    }
  } finally {
    state.sending = false;
    $("btn-send").disabled = false;
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
  pad.hidden = show === undefined ? !pad.hidden : !show;
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

async function sendKeys(keys, label) {
  try {
    await api(`${pathFor(state.selected)}/keys`, {
      method: "POST",
      body: JSON.stringify({ keys }),
    });
    toast(label + " sent");
    if (state.chat && !state.chat.terminal) setTimeout(() => loadChat(false), 800);
    else setTimeout(refreshOutput, 500);
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
  "ios-install": "On iPhone and iPad, notifications only work in the Home Screen app. In Safari, tap Share, then “Add to Home Screen”, and open herdr from your Home Screen.",
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

function toggleNotifyPanel() {
  const panel = $("notify-panel");
  panel.hidden = !panel.hidden;
  $("btn-notify").setAttribute("aria-expanded", String(!panel.hidden));
  if (!panel.hidden) { renderPush(); if (push.serverOn === null) initPush(); }
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
  return !!state.selected || state.sending || answer.inflight || !!promptValue().trim();
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
  setInterval(() => { if (!document.hidden && !state.selected) renderList(); }, 60000);
  document.addEventListener("visibilitychange", resume);
  window.addEventListener("pageshow", (ev) => { if (ev.persisted) resume(); }); // skip the initial load
  window.addEventListener("online", resume);
  window.addEventListener("offline", () => setConn(false));

  $("back").addEventListener("click", () => {
    // Pop our own history entry (popstate then closes the view); a deep
    // link has no list entry behind it, so just close in place.
    if (state.pushedRoute) history.back();
    else closeDetail();
  });
  window.addEventListener("popstate", route);
  window.addEventListener("hashchange", route);
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
    if (b) sendKeys([b.dataset.key], b.textContent);
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

  $("btn-notify").addEventListener("click", toggleNotifyPanel);
  $("notify-enable").addEventListener("click", enablePush);
  $("notify-disable").addEventListener("click", disablePush);
  $("notify-test").addEventListener("click", testPush);
  initPush();

  document.addEventListener("visibilitychange", () => reportPresence());
  window.addEventListener("pagehide", () => { reportPresence(false, true); suspendEvents(); });
});
