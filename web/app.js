/* Comebye: a phone app for herdr. Vanilla JS, no build step. */
"use strict";

const $ = (id) => document.getElementById(id);
const state = {
  agents: [],          // current agent list from the server (every host's, on a hub)
  listTab: null,       // the status tab: all | needs | working | done (loaded lazily)
  listQ: null,         // the filter box's text (loaded lazily)
  listGroup: null,     // how the list is grouped: project | host | status (loaded lazily)
  hostFilter: null,    // the chosen host names, [] for all (loaded lazily); only used with more than one host
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
  view: null,          // desktop only: the page filling the main pane ("status", "todo"), or null for an agent / nothing
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
// Test mode (hub flag -test-mode, browser tests and the dev server): poll every 300 ms instead of
// every 3 to 20 s. A normal hub answers {} and nothing changes. pollMs(n) is n unless test mode says otherwise.
let TEST_POLL_MS = 0;
const testReady = fetch("/api/test-mode").then((r) => (r.ok ? r.json() : {})).then((j) => { TEST_POLL_MS = (j && j.poll_ms) | 0; }).catch(() => {});
// test is the interval to use in test mode when 300 ms is too fast for what the screen shows (the side panel's refusal message is cleared by the next poll).
const pollMs = (n, test) => (TEST_POLL_MS ? test || TEST_POLL_MS : n);
window.pollMs = pollMs;
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
  // An "immutable" response (a page's preview for one version) is kept by the browser for a year and
  // replayed with the X-App-Version of the build that first fetched it: not the server's version now.
  if (!/\bimmutable\b/.test(res.headers.get("Cache-Control") || "")) noteVersion(res.headers.get("X-App-Version"));
  if (res.status === 401) {
    setUnauthorized(true);
    const err = new Error(UNAUTHORIZED_MSG);
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    let detail = res.statusText, code = "", parkId = "";
    try {
      const body = await res.json();
      detail = body.message || body.code || detail;
      code = body.code || "";
      parkId = body.park_id || "";
    } catch {}
    const err = new Error(detail);
    err.status = res.status;
    err.code = code;
    err.parkId = parkId;
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
  // An error stays longer and carries a Copy button: it is the text one pastes
  // into a chat or an issue, and it is gone before it can be selected.
  if (isError && msg) {
    const b = document.createElement("button");
    b.type = "button"; b.className = "toast-act"; b.textContent = "Copy";
    b.onclick = () => copyText(msg).then((ok) => { b.textContent = ok ? "Copied" : "Not copied"; clearTimeout(toastTimer); toastTimer = setTimeout(() => (el.className = ""), 1500); });
    el.append(" ", b);
  }
  toastTimer = setTimeout(() => (el.className = ""), isError && msg ? 6000 : 2600);
}

// toastActions: a toast with several buttons, on screen for ms.
function toastActions(msg, actions, ms) {
  const el = $("toast");
  el.textContent = "";
  el.append(msg);
  for (const a of actions) {
    const b = document.createElement("button");
    b.type = "button"; b.className = "toast-act"; b.textContent = a.label;
    b.onclick = () => { el.className = ""; a.fn(); };
    el.append(" ", b);
  }
  el.className = "show act error refusal";
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (el.className = ""), ms);
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
  settle(key) { const e = this.m.get(key); if (e) e.until = Date.now() + SETTLE_MS; this.wake(key); }
  drop(key) { this.m.delete(key); this.wake(key); }
  // idle resolves once nothing for the key is on the wire (at once if nothing is).
  idle(key) {
    if (!this.busy(key)) return Promise.resolve();
    return new Promise((res) => { (this.w || (this.w = new Map())).set(key, [...((this.w.get(key)) || []), res]); });
  }
  wake(key) { const ws = this.w && this.w.get(key); if (ws) { this.w.delete(key); ws.forEach((r) => r()); } }
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

// whyBlocked is what a blocked agent waits on, as the hub read it from its
// screen ("blocked: permission prompt (git push origin x)"); "" until read.
function whyBlocked(a) {
  const b = a && a.agent_status === "blocked" && a.blocked_on;
  return b && b.label ? "blocked: " + b.label : "";
}
// noteOf: herdr said blocked but the screen shows no dialog and a running step.
function noteOf(a) {
  return a && a.agent_status === "working" && a.status_note ? "working (" + a.status_note + ")" : "";
}

function subLine(a) {
  const { repo, branch } = repoLabel(a);
  const kindEl = a.agent ? `<span class="kind">· ${esc(a.agent)}</span>` : "";
  const hostEl = multiHost() && a.host ? `<span class="host-chip">${esc(a.host)}</span>` : "";
  const repoEl = `<span class="repo">${esc(repo)}</span>`;
  const chip = branch ? `<span class="wt-chip">⑂ ${esc(branch)}</span>` : "";
  const d = repoDamageOf(a);
  const dmg = d ? `<span class="lg-chip failed rd-chip" title="${escAttr(`${d.repo}: ${d.reason}. ${d.hint}`)}">repo damaged</span>` : "";
  const ch = a.chat ? `<span class="chip purple chat-chip" title="${escAttr((a.chat.writable ? "A helper about " : "A read-only helper about ") + (a.chat.title ? "“" + a.chat.title + "”" : "a Page"))}">chat</span>${a.chat.writable ? '<span class="chip cp-rwchip" title="Started with the read-only check overridden: this helper can change files and run commands.">not read-only</span>' : ""}` : "";
  const pk = a.park_hint ? `<span class="chip teal can-park" title="${escAttr(parkHintTitle(a))}">can park</span>` : "";
  return `<div class="sub">${hostEl}${repoEl}${chip}${kindEl}${dmg}${ch}${pk}${ledgerChip(a)}${(ledgerEntry(a) && usageChipHtml(ledgerEntry(a).id, false)) || ""}</div>`;
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

// Each host reports how full its temporary directory is (on many Linux hosts
// /tmp is RAM, and agents' scratch files fill it). Shown on Status, red above
// the host's own warning threshold.
function tmpHosts() { return (state.hosts || []).filter((h) => h.tmp && h.tmp.total > 0); }

function fmtBytes(n) {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : String(n)) + " " + u[i];
}

// The transcript index's state, on the Status page too, so a timer that has
// stopped shows without opening Search (GET /api/recall/status, the same state
// the Search screen's "indexed N min ago" reads).
const idx = { st: null, at: 0, inflight: false };

async function loadIdx() {
  if (idx.inflight || Date.now() - idx.at < 30000) return;
  idx.inflight = true;
  try { idx.st = await api("/api/recall/status"); } catch { idx.st = null; } finally { idx.inflight = false; idx.at = Date.now(); }
  for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
  renderStatus();
}

function idxCardHtml() {
  const st = idx.st;
  if (!st || !st.state || st.state === "unavailable") return "";
  const at = typeof rcLastSeen === "function" ? rcLastSeen(st) : NaN;
  const ago = Number.isNaN(at) ? "" : fmtAgo(at) + " ago";
  let msg, bad = true, head = "Transcript index";
  if (st.state === "ok") {
    bad = false;
    const hosts = (st.hosts || []).length;
    msg = `indexed ${ago || "just now"} · ${st.sessions} ${st.sessions === 1 ? "session" : "sessions"}${hosts ? ` on ${hosts} ${hosts === 1 ? "host" : "hosts"}` : ""}`;
  } else if (st.state === "stale") { head = "Transcript index is not updating"; msg = `last indexed ${ago || "a long time ago"}: the timer may have stopped. Search shows older results.`; }
  else if (st.state === "missing") { head = "No transcript index"; msg = "This host has not built one yet. The timer (comebye archive index --all-hosts --if-changed) builds it."; }
  else { head = "Transcript index is out of date"; msg = "It is an older version: the next comebye archive index rebuilds it."; }
  return `<section class="st-proj${bad ? " hot" : ""}" aria-label="Transcript index"><div class="st-ph">${esc(head)}</div><div class="st-row idx-row"><span class="st-m${bad ? " warn" : ""}">${esc(msg)}</span></div></section>`;
}

function tmpCardHtml() {
  const hs = tmpHosts();
  if (!hs.length) return "";
  const hot = hs.some((h) => h.tmp.warn);
  const rows = hs.map((h) => {
    const t = h.tmp;
    const pct = Math.min(100, Math.max(0, t.used_pct));
    return `<div class="st-row tmp-row${t.warn ? " warn" : ""}"><span class="st-n">${esc(multiHost() ? h.name : "This host")}</span><span class="st-m">${esc(t.path)}${t.tmpfs ? " (RAM)" : ""}</span>` +
      `<span class="tmp-meter" role="img" aria-label="${pct}% used"><i style="width:${pct}%"></i></span>` +
      `<span class="st-r${t.warn ? " warn" : ""}">${pct}% of ${esc(fmtBytes(t.total))}</span></div>`;
  }).join("");
  const hint = hot ? `<div class="st-hint">Nearly full: agents' scratch files are filling it. <code>comebye scratch --dry-run</code> lists what can go.</div>` : "";
  return `<section class="st-proj${hot ? " hot tmp-hot" : ""}" aria-label="Temporary space"><div class="st-ph">${hot ? "Temporary space is nearly full" : "Temporary space"}</div>${rows}${hint}</section>`;
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

// pathForSend is pathFor for a message: a session on a worker that powered off is no longer listed
// (its host is down), but a message to it starts the worker: the hub does that (host_waking).
function pathForSend(id) {
  if (!agentById(id) && stoppedSession(id)) {
    const h = hostOf(id);
    return `/api/hosts/${encodeURIComponent(h)}/agents/${encodeURIComponent(id.slice(h.length + 1))}`;
  }
  return pathFor(id);
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
  if (state.selected) {
    // a host that came up (or went down) changes the banner; a stopped session that is listed again is live
    if (state.stoppedOpen && agentById(state.selected)) { state.stoppedOpen = false; openDetail(state.selected); }
    renderWakeBanner();
  }
  launchHostSync(); // the host picked in the launch sheet joined the hub: its folders load
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
  // The one exception is a link to a To do item on this same hub
  // (/#/todo/<id> or #/todo/<id>): it stays, and opens in the app.
  ALLOWED_URI_REGEXP: /^(?:(?:https?:|mailto:)|\/?#\/todo\/t[0-9a-f]{10}$)/i,
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
      const href = node.getAttribute("href");
      const inApp = href != null && /^\/?#\/todo\/t[0-9a-f]{10}$/.test(href);
      if (href != null && !inApp && !/^(?:https?:|mailto:)/i.test(href)) {
        node.removeAttribute("href"); // belt and braces with ALLOWED_URI_REGEXP
      }
      if (inApp) {
        node.removeAttribute("target"); // same tab: the hash change opens the item
        node.removeAttribute("rel");
      } else if (node.hasAttribute("href")) {
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

// ---- copy buttons on code blocks ----
//
// Every fenced block gets a strip above it: the language on the left, a Copy
// button on the right. The strip is added to the already sanitized HTML (so
// DOMPurify needs no <button>), and the cached HTML keeps it. The button
// copies the block's own text when it is pressed (never the label, never a
// line number): see codeBlockText.
const COPY_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 3.5v-.2A1.3 1.3 0 0 0 9.2 2H3.8A1.8 1.8 0 0 0 2 3.8v5.4A1.3 1.3 0 0 0 3.3 10.5h.2"/></svg>';
const COPY_SHELLS = /^(sh|bash|shell|console|zsh)$/;

// copyWorthy: does an inline code span hold a value worth a tap to copy? A SHA
// (7-40 hex with a digit and a letter), a to-do or handoff id (t + 10 hex), a
// path or branch (starts with / ~/ ./, or has a / and no space), or an error
// line. Anything else (a word in code font) stays plain text.
function copyWorthy(s) {
  if (!s || s.length > 300 || /\n/.test(s)) return false;
  if (/^[0-9a-f]{7,40}$/.test(s) && /\d/.test(s) && /[a-f]/.test(s)) return true;
  if (/^t[0-9a-f]{10}$/.test(s)) return true;
  if (/^(error|fatal|panic|warning)\b[: ]/i.test(s)) return true;
  if (/\s/.test(s)) return false;
  if (/^(\/|~\/|\.\/)[^/\s]/.test(s)) return true;
  return /^[\w.@+-]+(\/[\w.@+%=,:~-]+)+\/?$/.test(s) || /^https?:\/\/\S+$/.test(s);
}

function addCopyStrips(html) {
  if (!html.includes("<pre") && !html.includes("<code")) return html;
  const t = document.createElement("template");
  t.innerHTML = html;
  for (const c of t.content.querySelectorAll("code")) {
    if (c.closest("pre, a, .cp") || !copyWorthy(c.textContent)) continue;
    c.classList.add("cp");
    c.tabIndex = 0;
    c.setAttribute("role", "button");
    c.setAttribute("aria-label", "Copy " + c.textContent);
  }
  for (const pre of t.content.querySelectorAll("pre")) {
    const code = pre.firstElementChild;
    if (!code || code.tagName !== "CODE" || pre.parentElement && pre.parentElement.classList.contains("cb")) continue;
    const lang = ((/(?:^|\s)language-([\w+#.-]+)/.exec(code.className) || [])[1] || "").toLowerCase();
    const box = document.createElement("div");
    box.className = "cb";
    if (lang) box.dataset.lang = lang;
    box.innerHTML = `<div class="cb-bar"><span class="cb-lang">${esc(lang)}</span>` +
      `<button type="button" class="cb-copy" aria-label="Copy ${lang ? esc(lang) + " " : ""}code">${COPY_ICON}<span class="cb-t">Copy</span></button></div>`;
    pre.replaceWith(box);
    box.append(pre);
  }
  return t.innerHTML;
}

// codeBlockText is exactly what the block holds: its text with one trailing
// newline dropped. In a shell block, a leading "$ " is removed only when every
// non-blank line has one, so a block that mixes commands and output is copied whole.
function codeBlockText(box) {
  const code = box.querySelector("pre > code") || box.querySelector("pre");
  let t = code.textContent.replace(/\n$/, "");
  if (COPY_SHELLS.test(box.dataset.lang || "")) {
    const lines = t.split("\n"), real = lines.filter((l) => l.trim() !== "");
    if (real.length && real.every((l) => l.startsWith("$ "))) t = lines.map((l) => (l.startsWith("$ ") ? l.slice(2) : l)).join("\n");
  }
  return t;
}

// copyText writes text to the clipboard from inside a tap or key press:
// navigator.clipboard where it exists (https or localhost) and is allowed, then
// execCommand("copy") on an off-screen textarea (font-size 16px so iOS does not
// zoom; setSelectionRange because select() alone does nothing there).
// Resolves true or false; never rejects.
function copyText(text) {
  const legacy = () => {
    const ta = document.createElement("textarea");
    ta.value = text; ta.readOnly = true; ta.setAttribute("aria-hidden", "true");
    ta.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;opacity:0;font-size:16px";
    document.body.append(ta);
    const keep = document.activeElement;
    ta.focus(); ta.select(); ta.setSelectionRange(0, text.length);
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    if (keep && keep.focus) try { keep.focus({ preventScroll: true }); } catch {}
    return ok;
  };
  if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
    let p;
    try { p = navigator.clipboard.writeText(text); } catch { p = null; }
    if (p) return p.then(() => true, () => legacy());
  }
  return Promise.resolve(legacy());
}

let copyLive = null;
function copyAnnounce(msg) {
  if (!copyLive) {
    copyLive = document.createElement("div");
    copyLive.className = "sr-only";
    copyLive.setAttribute("role", "status");
    copyLive.setAttribute("aria-live", "polite");
    document.body.append(copyLive);
  }
  copyLive.textContent = "";
  setTimeout(() => { copyLive.textContent = msg; }, 30); // a change, so a repeat is read again
}

// A value that copies on a tap: inline code that looks like a SHA, a path or an
// id (copyWorthy), or any element with class "cp" (copies its data-copy, or its
// text). It shows a check for 1.5 s.
function copyTap(el) {
  const text = el.dataset.copy != null ? el.dataset.copy : el.textContent.trim();
  copyText(text).then((ok) => {
    clearTimeout(el._t);
    el.dataset.state = ok ? "ok" : "fail";
    if (ok) copyAnnounce("Copied " + (text.length > 60 ? "to clipboard" : text));
    else toast("Could not copy", true);
    el._t = setTimeout(() => { delete el.dataset.state; }, 1500);
  });
}
document.addEventListener("click", (ev) => {
  const cp = ev.target.closest && ev.target.closest(".cp");
  if (cp) { ev.preventDefault(); ev.stopPropagation(); copyTap(cp); }
}, true);
document.addEventListener("keydown", (ev) => {
  if (ev.key !== "Enter" && ev.key !== " ") return;
  const cp = ev.target.closest && ev.target.closest(".cp");
  if (cp && cp.getAttribute("role") === "button") { ev.preventDefault(); copyTap(cp); }
});

// A button that copies something bigger than its neighbourhood: a whole message,
// a report, a to-do's question and context. The Markdown source is kept here by
// key (the page renders the key, not the text), so a long report is not
// repeated in the DOM. copyBtnHtml registers the text and returns the button.
const copySrc = new Map();
function copyBtnHtml(key, text, label, aria, cls) {
  copySrc.delete(key); copySrc.set(key, text);
  if (copySrc.size > 3000) copySrc.delete(copySrc.keys().next().value);
  return `<button type="button" class="cpb${cls ? " " + cls : ""}" data-copy-key="${escAttr(key)}" aria-label="${escAttr(aria || label)}">${esc(label)}</button>`;
}
// cpOut: a Copy button for text that is already on the page (a tool call, a
// log, a snapshot). sel is a CSS selector, looked up from the button outwards:
// the nearest ancestor that is, or contains, a match. The text is what the user
// sees (innerText: line breaks and all), minus the button itself.
function cpOut(sel, label, aria) {
  return `<button type="button" class="cpb cpb-out" data-copy-target="${escAttr(sel)}" aria-label="${escAttr(aria || label)}">${esc(label)}</button>`;
}
function copyTargetText(b) {
  let el = null;
  for (let n = b.parentElement; n && !el; n = n.parentElement) el = n.matches(b.dataset.copyTarget) ? n : n.querySelector(b.dataset.copyTarget);
  if (!el) return null;
  const hid = [...el.querySelectorAll('.cpb, [aria-hidden="true"]')].filter((x) => !x.hidden);
  hid.forEach((x) => { x.hidden = true; });
  const t = el.innerText;
  hid.forEach((x) => { x.hidden = false; });
  return t.replace(/\n+$/, "");
}
document.addEventListener("click", (ev) => {
  const b = ev.target.closest && ev.target.closest(".cpb");
  if (!b) return;
  ev.preventDefault(); ev.stopPropagation(); // inside a <summary> this must not toggle it
  const text = b.dataset.copyTarget ? copyTargetText(b) : copySrc.get(b.dataset.copyKey);
  if (text == null) { toast("Nothing to copy: reload the page", true); return; }
  if (text === "") { toast("Nothing to copy yet"); return; }
  if (b.dataset.label == null) b.dataset.label = b.textContent;
  copyText(text).then((ok) => {
    clearTimeout(b._t);
    b.dataset.state = ok ? "ok" : "fail";
    b.textContent = ok ? "Copied" : "Not copied";
    if (ok) copyAnnounce("Copied to clipboard"); else toast("Could not copy", true);
    b._t = setTimeout(() => { delete b.dataset.state; b.textContent = b.dataset.label; }, 1500);
  });
}, true);

document.addEventListener("click", (ev) => {
  const btn = ev.target.closest && ev.target.closest(".cb-copy");
  if (!btn) return;
  const box = btn.closest(".cb");
  if (!box) return;
  const label = btn.querySelector(".cb-t");
  const reset = () => { delete btn.dataset.state; label.textContent = "Copy"; };
  copyText(codeBlockText(box)).then((ok) => {
    clearTimeout(btn._t);
    if (ok) {
      btn.dataset.state = "ok"; label.textContent = "Copied";
      copyAnnounce("Copied to clipboard");
      btn._t = setTimeout(reset, 1500);
    } else {
      // Select the block so the system Copy menu is one tap away.
      const r = document.createRange(); r.selectNodeContents(box.querySelector("pre"));
      const sel = getSelection(); sel.removeAllRanges(); sel.addRange(r);
      btn.dataset.state = "fail"; label.textContent = "Not copied";
      toast("Could not copy: the code is selected, use Copy from the menu", true);
      copyAnnounce("Could not copy. The code is selected.");
      btn._t = setTimeout(reset, 2500);
    }
  });
});

// renderMarkdown returns sanitized HTML for untrusted markdown text.
// max raises the size limit for a caller that shows a whole document (the
// Doc panel); chat text keeps MD_MAX_CHARS.
function renderMarkdown(src, key, max = MD_MAX_CHARS) {
  src = String(src ?? "");
  const hit = key && mdCache.get(key);
  if (hit && hit.src === src) return hit.html;
  let html;
  const lib = src.length <= max ? mdSetup() : false;
  if (!lib) {
    html = `<div class="md-plain">${esc(src)}</div>`;
  } else {
    try {
      html = addCopyStrips(lib.P.sanitize(lib.m.parse(src), MD_PURIFY));
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

// reportAgeText says how old a ledger entry's report is ("report 3 from 2h ago"), so a reader can tell an old
// report from a new one. Empty without a report.
function reportAgeText(e) {
  if (!e || !e.final_report) return "";
  const what = e.report_source !== "explicit" ? "last reply, taken as its report," : e.report_seq > 1 ? `report ${e.report_seq}` : "report";
  const at = Date.parse(e.report_at || "");
  return Number.isNaN(at) ? `${what} age unknown` : `${what} from ${fmtAgo(at)} ago`;
}

// newerReplyMs is when the worker's last reply kept beside its report (a loop pass, or a turn that ended without
// a report) was written, when that is newer than the report; NaN otherwise.
function newerReplyMs(e) {
  if (!e || !e.last_pass_report) return NaN;
  const p = Date.parse(e.last_pass_report_at || ""), r = Date.parse(e.report_at || "");
  return Number.isNaN(p) || (!Number.isNaN(r) && p <= r) ? NaN : p;
}

// reportAgeHtml is the report's age as a quiet chip for a report's summary line, with the newer reply's age.
function reportAgeHtml(e) {
  const t = reportAgeText(e);
  if (!t) return "";
  const n = newerReplyMs(e);
  return ` <span class="rep-age">${esc(t)}${Number.isNaN(n) ? "" : ` · newer reply ${esc(fmtAgo(n))} ago`}</span>`;
}

// newerReplyHtml shows the newer reply under the report, labelled as not being a report.
function newerReplyHtml(e, key) {
  const n = newerReplyMs(e);
  if (Number.isNaN(n)) return "";
  return `<div class="rep-newer"><div class="rep-newer-h">Newer: its last reply, ${esc(fmtAgo(n))} ago (a turn that ended without a report; not a report)</div><div class="md">${renderMarkdown(e.last_pass_report, "newer:" + key)}</div></div>`;
}

const STATUS_ORDER = { blocked: 0, working: 1, done: 2, idle: 3, unknown: 4 };

/* ---------- host filter (hub with more than one host) ---------- */

const HOST_FILTER_KEY = "herdr-host-filter";
// The selection is a list of host names; an empty list is "all hosts". It is stored as a JSON array; the old
// single string ("all", or one host's name) still reads, as no item or as one.
function normHostFilter(v) {
  if (Array.isArray(v)) return [...new Set(v.filter((x) => typeof x === "string" && x && x !== "all"))];
  if (typeof v === "string" && v && v !== "all") return [v];
  return [];
}
function loadHostFilter() {
  let raw = null;
  try { raw = localStorage.getItem(HOST_FILTER_KEY); } catch { return []; }
  if (raw && raw[0] === "[") { try { return normHostFilter(JSON.parse(raw)); } catch { return []; } }
  return normHostFilter(raw);
}
function setHostFilter(sel) {
  state.hostFilter = normHostFilter(sel);
  try { localStorage.setItem(HOST_FILTER_KEY, JSON.stringify(state.hostFilter)); } catch {}
  renderList();
  syncListHash();
}
// hostSel is the chosen hosts that are still configured; empty means every host (also on a single-host instance).
function hostSel() {
  if (state.hostFilter === null) state.hostFilter = loadHostFilter();
  state.hostFilter = normHostFilter(state.hostFilter);
  if (!multiHost()) return [];
  return state.hostFilter.filter((n) => state.hosts.some((h) => h.name === n));
}
const hostIn = (name) => { const s = hostSel(); return !s.length || s.includes(name); };
// A host filter never hides an agent that is waiting on you.
const agentShown = (a) => hostIn(a.host) || a.agent_status === "blocked";
// hostLabel: what the picker button says: "All hosts", "laharl", "laharl + mac", "3 hosts".
function hostLabel() {
  const s = hostSel();
  return !s.length ? "All hosts" : s.length <= 2 ? s.join(" + ") : s.length + " hosts";
}

/* ---------- status tabs and the filter box (left nav, slice 3) ---------- */

const LIST_TAB_KEY = "herdr-list-tab", LIST_Q_KEY = "herdr-list-q", LIST_HASH = "#/list";
// A tab is a predicate over agent_status. A blocked agent is in All, Needs you and Working (it is waiting mid-job);
// Done is every agent that has stopped without waiting on you (done or idle). An unknown one shows only in All.
const LIST_TABS = [
  { key: "all", label: "All", has: () => true },
  { key: "needs", label: "Needs you", has: (a) => a.agent_status === "blocked" },
  { key: "working", label: "Working", has: (a) => a.agent_status === "working" || a.agent_status === "blocked" },
  { key: "done", label: "Done", has: (a) => a.agent_status === "done" || a.agent_status === "idle" },
];
const normTab = (t) => (LIST_TABS.some((x) => x.key === t) ? t : "all");
function listTab() {
  if (state.listTab === null) state.listTab = normTab(lsGet(LIST_TAB_KEY));
  return state.listTab;
}
function listQ() {
  if (state.listQ === null) state.listQ = (lsGet(LIST_Q_KEY) || "").slice(0, 200);
  return state.listQ;
}
// textMatch: every word of the box is a part of the agent's name, repo or branch (case does not matter).
function textMatch(a, q) {
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const r = repoLabel(a);
  const hay = [a.name, a.terminal_title_stripped, a.display_agent, a.agent, a.pane_id, r.repo, r.branch, a.worktree && a.worktree.branch].filter(Boolean).join("\n").toLowerCase();
  return words.every((w) => hay.includes(w));
}
const filtersOn = () => listTab() !== "all" || !!listQ().trim() || hostSel().length > 0;

function setListTab(t) {
  state.listTab = normTab(t);
  lsSet(LIST_TAB_KEY, state.listTab);
  renderList();
  syncListHash();
}
function setListQ(q) {
  state.listQ = String(q).slice(0, 200);
  lsSet(LIST_Q_KEY, state.listQ);
  renderList();
  syncListHash();
}
// clearFilters resets the status tab, the box and the host selection.
function clearFilters() {
  state.listTab = "all"; state.listQ = "";
  lsSet(LIST_TAB_KEY, "all"); lsSet(LIST_Q_KEY, "");
  state.hostFilter = [];
  try { localStorage.setItem(HOST_FILTER_KEY, "[]"); } catch {}
  renderList();
  syncListHash();
}

/* ---------- group by (left nav, slice 4) ---------- */

// Each mode turns the sorted agents into [name, list] groups; a mode that does not apply (Host on one host) is not offered and the
// list falls back to Project. To add a mode, add an entry here (and its `groups`); the menu, the URL and the saved choice follow.
const LIST_GROUP_KEY = "herdr-list-group";
const GROUP_STATUS = [["Needs you", (a) => a.agent_status === "blocked"], ["Working", (a) => a.agent_status === "working"],
  ["Done", (a) => a.agent_status === "done" || a.agent_status === "idle"]];
const GROUP_MODES = [
  { key: "project", label: "Project", pins: true, groups: (list) => projectGroups(list) },
  { key: "host", label: "Host", pins: true, show: () => multiHost(), groups: (list) => hostGroups(list) },
  // Coordinator heads its own group, so the Coordinators section is not pinned above; a waiting agent is still pinned on a desktop.
  { key: "coordinator", label: "Coordinator", pins: true, pinCoords: false, show: () => hasCoordinatorView(), groups: (list) => coordinatorGroups(list) },
  { key: "status", label: "Status", pins: false, groups: (list) => statusGroups(list) },
];
const normGroup = (g) => (GROUP_MODES.some((x) => x.key === g) ? g : "project");
// listGroup is the saved choice; groupMode is the one in effect (a saved Host on a single host shows as Project, and comes back with the hosts).
function listGroup() {
  if (state.listGroup === null) state.listGroup = normGroup(lsGet(LIST_GROUP_KEY));
  return state.listGroup;
}
function groupMode() {
  const m = GROUP_MODES.find((x) => x.key === listGroup());
  return m && (!m.show || m.show()) ? m : GROUP_MODES[0];
}
function setListGroup(g) {
  state.listGroup = normGroup(g);
  lsSet(LIST_GROUP_KEY, state.listGroup);
  renderList();
  syncListHash();
}
// Collapse state is per mode: a Project group and a Host group of the same name are two keys. Project keeps the plain name it always had.
const groupKey = (mode, name) => (mode === "project" ? name : "\u001f" + mode + "\u001f" + name);
function urgentFirst(groups) {
  const rank = (list) => Math.min(...list.map(statusRank));
  const recent = (list) => Math.max(...list.map(activeMs));
  return groups.sort(([na, la], [nb, lb]) => rank(la) - rank(lb) || recent(lb) - recent(la) || na.localeCompare(nb));
}
function hostGroups(agents) {
  const groups = new Map();
  for (const a of agents) {
    const h = a.host || state.localHost || "This host";
    if (!groups.has(h)) groups.set(h, []);
    groups.get(h).push(a);
  }
  return urgentFirst([...groups.entries()]);
}
// Coordinator mode is offered when there is more than one coordinator, or any agent has an owner (the hub says so in `owner`).
// With one coordinator and nobody owned there is nothing to tell apart, as Host is not offered on one host.
const coordAgents = () => state.agents.filter((a) => isCoordinator(a));
const hasCoordinatorView = () => coordAgents().length > 1 || state.agents.some((a) => a.owner);
// coordinatorGroups: a group per coordinator (named for it, its row first, then the agents it started), then "Unowned" for the rest
// (no owner, or an owner that is not a coordinator any more). Two coordinators of one name are told apart by their host.
function coordinatorGroups(agents) {
  const coords = coordAgents();
  const label = new Map(coords.map((c) => [agentId(c), coords.filter((x) => x.name === c.name).length > 1 && c.host ? `${c.name || c.pane_id} · ${c.host}` : c.name || c.pane_id]));
  const groups = new Map(), unowned = [];
  for (const a of agents) {
    const key = isCoordinator(a) ? agentId(a) : label.has(a.owner) ? a.owner : "";
    if (!key) { unowned.push(a); continue; }
    if (!groups.has(key)) groups.set(key, []);
    const l = groups.get(key);
    if (isCoordinator(a)) l.unshift(a); else l.push(a);
  }
  const out = urgentFirst([...groups.entries()].map(([id, l]) => [label.get(id), l]));
  if (unowned.length) out.push(["Unowned", unowned]);
  return out;
}
// statusGroups: a group per status, in a fixed order; a blocked agent is in Needs you only (a group holds an agent once).
function statusGroups(agents) {
  const out = GROUP_STATUS.map(([name, has]) => [name, agents.filter(has)]);
  const other = agents.filter((a) => !GROUP_STATUS.some(([, has]) => has(a)));
  if (other.length) out.push(["Unknown", other]);
  return out.filter(([, l]) => l.length);
}

// The URL carries the view so it can be shared: #/list?tab=working&q=api&host=mac,laharl (only what differs from the default).
// A screen or an agent owns the hash while it is open; the list's own hash is written when none is.
function listHash() {
  const p = new URLSearchParams();
  if (listTab() !== "all") p.set("tab", listTab());
  if (listQ().trim()) p.set("q", listQ().trim());
  const h = normHostFilter(state.hostFilter);
  if (h.length) p.set("host", h.join(","));
  if (listGroup() !== "project") p.set("group", listGroup());
  const s = p.toString();
  return s ? `${LIST_HASH}?${s}` : "";
}
const onListHash = () => location.hash === "" || location.hash === LIST_HASH || location.hash.startsWith(LIST_HASH + "?");
function syncListHash() {
  if (!onListHash()) return;
  const want = listHash();
  if (location.hash === want || (!want && location.hash === "")) return;
  history.replaceState(history.state, "", want || location.pathname + location.search);
}
// applyListHash takes the view from a #/list?... link (a missing part is its default); false when the hash is not one.
function applyListHash() {
  const h = location.hash;
  if (h !== LIST_HASH && !h.startsWith(LIST_HASH + "?")) return false;
  const qi = h.indexOf("?");
  const p = new URLSearchParams(qi < 0 ? "" : h.slice(qi + 1));
  state.listTab = normTab(p.get("tab"));
  state.listQ = (p.get("q") || "").slice(0, 200);
  state.hostFilter = normHostFilter((p.get("host") || "").split(",").filter(Boolean));
  state.listGroup = normGroup(p.get("group"));
  lsSet(LIST_GROUP_KEY, state.listGroup);
  lsSet(LIST_TAB_KEY, state.listTab); lsSet(LIST_Q_KEY, state.listQ);
  try { localStorage.setItem(HOST_FILTER_KEY, JSON.stringify(state.hostFilter)); } catch {}
  return true;
}

// renderListFilter draws the tabs' counts. The box is static markup (a poll must not take the typing from it).
function renderListFilter(base) {
  const bar = $("list-filter");
  if (!bar) return;
  const show = state.agents.length > 0;
  bar.hidden = !show;
  if (!show) return;
  const tab = listTab(), q = listQ();
  const box = $("lf-q");
  if (box.value !== q) box.value = q;
  $("lf-x").hidden = !q;
  const gm = groupMode();
  $("gb-label").textContent = gm.label;
  $("gb-btn").setAttribute("aria-label", `Group by: ${gm.label}`);
  if (gm.key !== "project") $("gb-btn").setAttribute("data-set", ""); else $("gb-btn").removeAttribute("data-set");
  if (!$("gb-pop").hidden) renderGroupMenu();
  const had = document.activeElement && document.activeElement.closest ? document.activeElement.closest("#lf-tabs .lf-tab") : null;
  $("lf-tabs").innerHTML = LIST_TABS.map((t) => {
    const n = base.filter(t.has).length;
    return `<button type="button" class="lf-tab${t.key === "needs" && n ? " nd" : ""}" data-tab="${t.key}" aria-pressed="${t.key === tab}"` +
      ` aria-label="${esc(t.label)}, ${n}"><span class="l">${esc(t.label)}</span><span class="n">${n}</span></button>`;
  }).join("");
  if (had) { const b = $("lf-tabs").querySelector(`[data-tab="${had.dataset.tab}"]`); if (b) b.focus({ preventScroll: true }); }
}
function focusListFilter() {
  const bar = $("list-filter");
  if (!bar || bar.hidden || !bar.getClientRects().length) return false;
  if (document.body.classList.contains("list-collapsed")) setListCollapsed(false, false);
  $("lf-q").focus();
  $("lf-q").select();
  return true;
}
function renderGroupMenu() {
  const cur = groupMode().key, list = $("gb-list");
  const fid = document.activeElement && list.contains(document.activeElement) ? document.activeElement.dataset.group : null;
  list.innerHTML = GROUP_MODES.filter((m) => !m.show || m.show()).map((m) =>
    `<button type="button" class="hp-row" role="menuitemradio" aria-checked="${m.key === cur}" data-group="${m.key}"><span class="hp-box" aria-hidden="true"></span><span class="hp-name">${esc(m.label)}</span></button>`).join("");
  if (fid !== null) list.querySelector(`[data-group="${fid}"]`)?.focus({ preventScroll: true });
}
function toggleGroupMenu(show) {
  const pop = $("gb-pop");
  const open = show ?? pop.hidden;
  if (open === !pop.hidden) return;
  pop.hidden = !open;
  if (open) {
    renderGroupMenu();
    popOpened("group");
    placePop("group", $("gb-btn"));
    // Under the button on a desktop, left edge in line with the box and never off the screen.
    (pop.querySelector('[aria-checked="true"]') || popItems(pop)[0] || pop).focus?.();
  } else {
    popClosed("group");
  }
  $("gb-btn").setAttribute("aria-expanded", String(open));
}
function openGroupMenu() {
  const bar = $("list-filter");
  if (!bar || bar.hidden || !bar.getClientRects().length) return false;
  if (document.body.classList.contains("list-collapsed")) setListCollapsed(false, false);
  toggleGroupMenu(true);
  return true;
}
function initListFilter() {
  $("gb-btn").addEventListener("click", () => toggleGroupMenu());
  $("gb-list").addEventListener("click", (ev) => {
    const b = ev.target.closest(".hp-row");
    if (!b) return;
    closePop("group", true);
    setListGroup(b.dataset.group);
  });
  $("gb-pop").addEventListener("keydown", (ev) => { if (ev.key === "Tab") closePop("group", true); });
  $("lf-q").addEventListener("input", () => setListQ($("lf-q").value));
  $("lf-q").addEventListener("keydown", (ev) => {
    if (ev.isComposing) return;
    if (ev.key === "Escape") {
      ev.preventDefault();
      if ($("lf-q").value) setListQ(""); else $("lf-q").blur();
    } else if (ev.key === "Enter" || ev.key === "ArrowDown") {
      const r = document.querySelector("#agents .agent[data-id]");
      if (r) { ev.preventDefault(); r.focus({ preventScroll: false }); }
    }
  });
  $("lf-x").addEventListener("click", () => { setListQ(""); $("lf-q").focus(); });
  $("lf-tabs").addEventListener("click", (ev) => { const b = ev.target.closest(".lf-tab"); if (b) setListTab(b.dataset.tab); });
}

const hp = { open: false, q: "" };
// hostCard: one host's facts for the picker.
function hostFacts(h) {
  const agents = state.agents.filter((a) => a.host === h.name);
  return { h, n: agents.length, need: agents.some((a) => a.agent_status === "blocked"), down: !h.local && h.status !== "ok" };
}
function renderHostFilter() {
  const nav = $("host-filter");
  if (!multiHost()) { nav.hidden = true; nav.innerHTML = ""; if (hp.open) toggleHostPicker(false); return; }
  const sel = hostSel(), narrow = sel.length > 0;
  const need = state.agents.some((a) => a.agent_status === "blocked");
  const outside = narrow ? state.agents.filter((a) => a.agent_status === "blocked" && !hostIn(a.host)).length : 0;
  // innerHTML replaced on every poll: keep the keyboard focus on the button.
  const had = document.activeElement === $("hp-btn");
  nav.innerHTML = `<button type="button" id="hp-btn" class="hp-btn" aria-haspopup="dialog" aria-expanded="${hp.open}" aria-controls="hp-pop"` +
    `${narrow ? " data-narrow" : ""} title="Filter by host (h)">` +
    `${need ? '<span class="hp-need" aria-label="needs input"></span>' : ""}<span class="hp-label">${esc(hostLabel())}</span><span class="chev" aria-hidden="true">&#9662;</span></button>` +
    `<div class="hp-out" role="status"${outside ? "" : " hidden"}>${outside ? `<button type="button" class="hp-out-btn" id="hp-out-btn" title="Show every host"><span class="hp-need" aria-hidden="true"></span>+${outside} outside this filter</button>` : ""}</div>`;
  nav.hidden = false;
  $("hp-btn").addEventListener("click", () => toggleHostPicker());
  if ($("hp-out-btn")) $("hp-out-btn").addEventListener("click", () => { setHostFilter([]); $("hp-btn")?.focus({ preventScroll: true }); });
  if (had) $("hp-btn").focus({ preventScroll: true });
  if (hp.open) renderHostPicker();
}

// The picker's rows: an "All hosts" row, then a row per host with its need-you dot, version badge, down state and agent count.
function renderHostPicker() {
  const list = $("hp-list"), sel = hostSel(), q = hp.q.trim().toLowerCase();
  const fid = document.activeElement && list.contains(document.activeElement) ? document.activeElement.dataset.host : null;
  const local = state.hosts.find((x) => x.local);
  const rows = state.hosts.map((h) => {
    const f = hostFacts(h), on = sel.includes(h.name);
    const ver = h.version_mismatch && h.version
      ? `<span class="ver" title="${esc(h.name)} runs ${esc(h.version)}${local && local.version ? `; this hub runs ${esc(local.version)}` : ""}">${esc(h.version)}</span>` : "";
    const since = h.since ? Date.parse(h.since) : NaN;
    const right = f.down ? `down${Number.isNaN(since) ? "" : " " + fmtAgo(since)}` : h.no_agents ? "no agents here" : String(f.n);
    const hide = q && !h.name.toLowerCase().includes(q);
    // A down host is listed but cannot be picked (unless it is already picked, so it can be let go).
    const dis = f.down && !on;
    return `<button type="button" class="hp-row${f.down ? " down" : ""}" role="checkbox" aria-checked="${on}" data-host="${esc(h.name)}"` +
      `${dis ? " disabled" : ""}${hide ? " hidden" : ""}${f.down ? ` title="${esc(h.error || "This host is not reachable")}"` : ""}>` +
      `<span class="hp-box" aria-hidden="true"></span>${f.need ? '<span class="hp-need" aria-label="needs input"></span>' : '<span class="hp-need off" aria-hidden="true"></span>'}` +
      `<span class="hp-name">${esc(h.name)}</span>${ver}<span class="n">${esc(right)}</span></button>`;
  });
  const all = `<button type="button" class="hp-row hp-all" role="checkbox" aria-checked="${!sel.length}" data-host="">` +
    `<span class="hp-box" aria-hidden="true"></span><span class="hp-name">All hosts</span><span class="n">${state.agents.length}</span></button>`;
  const none = q && !state.hosts.some((h) => h.name.toLowerCase().includes(q));
  list.innerHTML = (q ? "" : all) + rows.join("") + (none ? '<div class="hp-none">No host matches</div>' : "");
  $("hp-q").hidden = state.hosts.length < 8;
  if (fid !== null) { const r = [...list.querySelectorAll(".hp-row")].find((x) => x.dataset.host === fid && !x.disabled && !x.hidden); if (r) r.focus({ preventScroll: true }); }
}
function toggleHostPick(name) {
  const sel = hostSel();
  if (!name) return setHostFilter([]);
  setHostFilter(sel.includes(name) ? sel.filter((x) => x !== name) : [...sel, name]);
}
function toggleHostPicker(show) {
  const pop = $("hp-pop");
  const open = show ?? pop.hidden;
  if (open === !pop.hidden) return;
  if (open && !multiHost()) return;
  hp.open = open;
  pop.hidden = !open;
  if (open) {
    hp.q = ""; $("hp-q").value = "";
    renderHostPicker();
    popOpened("hosts");
    placePop("hosts", $("hp-btn"));
    // On a desktop the popover sits under the left-aligned button.
    if (!isSheet()) { const r = $("hp-btn").getBoundingClientRect(); pop.style.left = Math.max(8, Math.min(innerWidth - pop.offsetWidth - 8, r.left)) + "px"; }
    const first = !$("hp-q").hidden ? $("hp-q") : (pop.querySelector('.hp-row[aria-checked="true"]:not(:disabled)') || popItems(pop)[0]);
    (first || pop).focus?.();
  } else {
    popClosed("hosts");
  }
  if ($("hp-btn")) $("hp-btn").setAttribute("aria-expanded", String(open));
}
function initHostPicker() {
  const pop = $("hp-pop");
  $("hp-list").addEventListener("click", (ev) => {
    const b = ev.target.closest(".hp-row");
    if (b && !b.disabled) toggleHostPick(b.dataset.host);
  });
  $("hp-q").addEventListener("input", () => { hp.q = $("hp-q").value; renderHostPicker(); });
  pop.addEventListener("keydown", (ev) => {
    // Enter applies (the list already follows every tick) and closes; Space toggles through the button itself.
    if (ev.key === "Enter" && !ev.isComposing && !ev.target.closest(".pop-x")) { ev.preventDefault(); closePop("hosts", true); return; }
    // A dialog traps Tab: it cycles through the search, the rows and the x.
    if (ev.key === "Tab") {
      const f = [...pop.querySelectorAll("button, input")].filter((x) => !x.disabled && !x.hidden && x.getClientRects().length);
      if (!f.length) { ev.preventDefault(); return; }
      const i = f.indexOf(document.activeElement);
      const next = ev.shiftKey ? (i <= 0 ? f.length - 1 : i - 1) : (i < 0 || i === f.length - 1 ? 0 : i + 1);
      ev.preventDefault(); f[next].focus();
    }
  });
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
  // base: what the host picker and the box leave; the tabs count it and narrow it.
  const base = state.agents.filter(agentShown).filter((a) => textMatch(a, listQ()));
  renderListFilter(base);
  const tabHas = LIST_TABS.find((t) => t.key === listTab()).has;
  const agents = base.filter(tabHas).sort((a, b) =>
    statusRank(a) - statusRank(b) ||
    // Within a status, most recently active first, so the agents that just
    // went idle sit above the ones that have been idle for days.
    activeMs(b) - activeMs(a) ||
    agentId(a).localeCompare(agentId(b)));
  const nBlocked = agents.filter((a) => a.agent_status === "blocked").length;
  const nWorking = agents.filter((a) => a.agent_status === "working").length;
  syncRail();
  // The tab title carries the count, so a background tab shows it.
  document.title = nBlocked ? `(${nBlocked}) Comebye` : "Comebye";
  // The long form on wide screens; a short one (no "agents" word, "need")
  // on a phone, where the header also holds +, the bell and the ⋯.
  $("count").innerHTML = agents.length
    ? `<span class="long">${nBlocked ? `<span class="need">${nBlocked} need${nBlocked === 1 ? "s" : ""} input</span> · ` : ""}` +
      `${nWorking ? `<span class="run">${nWorking} working</span> · ` : ""}${agents.length} agent${agents.length === 1 ? "" : "s"}</span>` +
      `<span class="short">${nBlocked ? `<span class="need">${nBlocked} need</span><span class="sep"> · </span>` : ""}` +
      `${nWorking ? `<span class="run">${nWorking} working</span><span class="sep"> · </span>` : ""}<span class="tot">${agents.length}</span></span>`
    : "";
  // Peers the hub cannot reach right now: one collapsed row each, saying
  // what broke (dns, tcp, tls, http) and since when - the hub's health
  // probe keeps the diagnosis fresh - instead of a bare error.
  const downRows = state.hosts.filter((h) => !h.local && h.status !== "ok" &&
    hostIn(h.name)).map((h) => {
    const since = h.since ? Date.parse(h.since) : NaN;
    let what;
    if (h.status === "connecting") {
      what = "connecting…";
    } else {
      what = "unreachable" + (h.class ? `: ${h.class}` : "") +
        (Number.isNaN(since) ? "" : ` (since ${fmtAgo(since)} ago)`);
    }
    const wx = wakeOf(h);
    if (wx) {
      return `
    <div class="agent host-down host-stopped ${esc(wx.kind)}" role="status" data-wk-host="${esc(h.name)}">
      <span class="dot unknown"></span>
      <div class="meta">
        <div class="name">${esc(h.name)}</div>
        <div class="sub">${esc(wakeWhat(wx))}</div>
        ${wakeAction(h, wx)}
      </div>
      <span class="status">${esc(wakeChip(wx))}</span>
    </div>`;
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
  renderFleetMenu();
  renderPlans();
  renderNav();
  renderStatus();
  if (!agents.length) {
    const f = hostSel();
    const hubOnly = state.hosts.some((h) => h.local && h.no_agents);
    // A hub-only hub (no herdr) says so, instead of telling you to start an agent here.
    const here = !f.length || state.hosts.some((h) => h.local && f.includes(h.name));
    const tabLabel = LIST_TABS.find((t) => t.key === listTab()).label, qq = listQ().trim();
    const why = [listTab() !== "all" ? `status ${esc(tabLabel)}` : "", qq ? `“${esc(qq)}”` : "", f.length ? `on ${esc(f.join(" or "))}` : ""].filter(Boolean).join(", ");
    wrap.innerHTML = (state.unauthorized ? `<div class="empty"><b>${esc(UNAUTHORIZED_MSG)}</b></div>`
      : hubOnly && here && !qq && listTab() === "all" ? emptyHtml("No agents here", "This hub hosts none. Agents on its peers appear as they connect.")
      : state.agents.length && filtersOn() ? emptyHtml("No agents match", `Nothing for ${why}.`, ["Clear filters", "clear-filters"])
      : emptyHtml("No agents running", "Start one with the + button, and it shows up here.")) + parkedSectionHTML() + downRows;
    wrap.querySelectorAll(".proj-head").forEach((el) => el.addEventListener("click", () => toggleProject(el.dataset.proj)));
    bindParkedRows(wrap);
    const act = wrap.querySelector(".empty-act");
    if (act) act.onclick = () => { clearFilters(); $("lf-q")?.focus({ preventScroll: true }); };
    return;
  }
  // Coordinators are pinned above the projects.
  // Desktop: the agents waiting on you sit in a group of their own at the top.
  const gm = groupMode();
  const needs = isDesktop() && gm.pins ? agents.filter((a) => a.agent_status === "blocked") : [];
  const needsSec = needs.length ? `<section class="proj needs-sec" aria-label="Needs you">
    <div class="proj-head static"><span class="proj-name">Needs you</span><span class="proj-counts"><span class="need">${needs.length}</span></span></div>
    <div class="proj-body">${needs.map((a) => agentRow(a, false)).join("")}</div></section>` : "";
  const coords = gm.pins && gm.pinCoords !== false ? agents.filter((a) => isCoordinator(a) && !needs.includes(a)) : [];
  const pinned = coords.length ? `<section class="proj coord-sec" aria-label="Coordinators">
    <div class="proj-head static"><span class="proj-name">Coordinators</span></div>
    <div class="proj-body">${coords.map((a) => agentRow(a, false)).join("")}</div></section>` : "";
  const rest = agents.filter((a) => !(gm.pins && gm.pinCoords !== false && isCoordinator(a)) && !needs.includes(a));
  wrap.innerHTML = parkBannerHTML() + needsSec + pinned + gm.groups(rest).map(([name, list]) => projectHTML(name, list, gm.key)).join("") + parkedSectionHTML() + downRows;
  wrap.querySelectorAll(".proj-head").forEach((el) =>
    el.addEventListener("click", () => toggleProject(el.dataset.proj)));
  bindParkedRows(wrap);
  $("park-banner") && $("park-banner").addEventListener("click", openParkAll);
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

/* Collapse the left column to a rail (desktop). Remembered per browser. The
   list is only hidden: the selected agent, a half-typed prompt and the list's
   scroll position (restored below) are kept. */
const LIST_COLLAPSED_KEY = "comebye-list-collapsed";
function syncRail() {
  const need = state.agents.filter((a) => a.agent_status === "blocked").length;
  const n = $("lr-need"), t = $("lr-todo");
  if (n) { n.hidden = !need; n.textContent = String(need); n.title = `${need} agent${need === 1 ? "" : "s"} need input`; }
  if (t) { const open = todos.on === true ? todos.open || 0 : 0; t.hidden = !open; t.textContent = String(open); t.title = `${open} to do`; }
  const r = $("list-rail");
  if (r) r.setAttribute("aria-label", "Expand the list" + (need ? `, ${need} need input` : "") + (todos.on === true && todos.open ? `, ${todos.open} to do` : ""));
}
function setListCollapsed(on, remember = true) {
  const ag = $("agents"), top = ag ? ag.scrollTop : 0;
  document.body.classList.toggle("list-collapsed", on);
  $("btn-collapse")?.setAttribute("aria-expanded", String(!on));
  $("list-rail")?.setAttribute("aria-expanded", String(!on));
  if (remember) lsSet(LIST_COLLAPSED_KEY, on ? "1" : "0");
  if (!on && ag) ag.scrollTop = top; // display:none loses it in some browsers
  if (on) syncRail();
  const f = on ? $("list-rail") : $("btn-collapse");
  if (isDesktop() && remember && f && (document.activeElement === document.body || !document.activeElement?.offsetParent)) f.focus({ preventScroll: true });
}
const toggleList = () => setListCollapsed(!document.body.classList.contains("list-collapsed"));

function markSelectedRow() {
  document.querySelectorAll("#agents .agent.selected").forEach((r) => {
    r.classList.remove("selected");
    r.removeAttribute("aria-current");
  });
  const r = state.selected && rowFor(state.selected);
  if (r) { r.classList.add("selected"); r.setAttribute("aria-current", "true"); }
}

// The single-key shortcuts (n, j, k, i, /, ?, g chords) and the Ctrl/⌘+K palette live in keys.js.

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
      <span class="dot ${esc(bgRunning(a) ? "waiting" : a.agent_status)}"></span>
      <div class="meta">
        <div class="name">${esc(a.name || a.terminal_title_stripped || a.display_agent || a.agent || a.pane_id)}${
          isCoordinator(a) ? '<span class="role-chip">coordinator</span>' : ""}</div>
        ${subLine(a)}${noteHtml(a)}${quietMs(a) && !whyBlocked(a) ? `<div class="why stuck" title="${escAttr(quietTitle(a))}">${esc(quietText(a))}</div>` : bgRunning(a) && !whyBlocked(a) && !noteOf(a) ? `<div class="why quiet" title="${escAttr(bgTitle(a))}">${esc(bgWhy(a))}</div>` : whyBlocked(a) ? `<div class="why" title="${escAttr(whyBlocked(a))}">${esc(whyBlocked(a))}</div>` : noteOf(a) ? `<div class="why quiet" title="${escAttr(noteOf(a))}">${esc(noteOf(a))}</div>` : ""}
      </div>
      <span class="status ${esc(agentStatusCls(a))}"${bgRunning(a) ? ` title="${esc(bgTitle(a))}"` : ""}>${esc(agentStatusText(a))}${
        bgRunning(a) ? (bgSinceMs(a) ? `<br><span class="ago">${fmtAgo(bgSinceMs(a))}</span>` : "")
        : activeMs(a) && (a.agent_status === "idle" || a.agent_status === "done")
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
    if (!arch.data && !arch.err) loadArchived(); // the board footer and Recently closed need it
    if (Date.now() - todos.loadedAt > 8000) loadTodos();
    loadUsageChips();
    // The server checks finished workers' worktrees in the background
    // (for the "merged" hint): look again shortly until they are in.
    const waiting = ledger.entries.some((e) => (e.status === "finished" || e.status === "landed") && e.worktree && !e.checks);
    ledger.checkPolls = waiting ? (ledger.checkPolls || 0) + 1 : 0;
    if (waiting && ledger.checkPolls <= 10 && !document.hidden) {
      clearTimeout(ledger.timer);
      ledger.timer = setTimeout(() => { ledger.timer = null; loadLedger(); }, 3000);
    }
  } catch (e) {
    if (e.status === 503) { ledger.on = false; } // no state directory: no handoffs or marks here
  } finally {
    ledger.inflight = false;
    ledger.loadedAt = Date.now();
  }
  peersHostsChanged();
  renderList();
  if (state.selected) { renderDetailMenu(); renderDetailNote(); }
  if (launch.open) renderLaunchForm();
}

// scheduleLedger refreshes the ledger after agent updates (a worker going
// idle finishes it), at most every few seconds.
function scheduleLedger() {
  if (ledger.on === false || ledger.timer || document.hidden) return;
  const wait = Math.max(0, ledger.loadedAt + 5000 - Date.now());
  ledger.timer = setTimeout(() => { ledger.timer = null; loadLedger(); }, wait);
}

// An idle or done agent's tasks started in the background (a test run, a CI
// watch, a Monitor): the hub reads them from its session and sends them with
// the agent (a.background: kind, a short label, start). The agent is not
// finished while one runs; it wakes when the task ends.
function bgTasks(a) {
  return (a && a.background) || [];
}
// quietMs is when a working worker last wrote anything (0 when it is not marked quiet): the hub marks a
// worker "working" that has produced nothing for a long time (a.quiet_since), see quiet.go.
function quietMs(a) {
  const t = a && a.agent_status === "working" && a.quiet_since ? Date.parse(a.quiet_since) : 0;
  return Number.isNaN(t) ? 0 : t;
}
function quietText(a) { return quietMs(a) ? "quiet " + fmtAgo(quietMs(a)) : ""; }
function quietTitle(a) {
  return `Marked working, but it has written nothing to its session for ${fmtAgo(quietMs(a))} and no background task is known. It may be stuck: open it to see.`;
}
function bgRunning(a) {
  return !!a && (a.agent_status === "idle" || a.agent_status === "done") && bgTasks(a).length > 0;
}
// bgLabel names what the agent waits on: "race + uitest", "gh run +1".
function bgLabel(a) {
  const t = bgTasks(a);
  const first = t.find((x) => x.label) || t[0];
  const name = (first && first.label) || (first && first.kind) || "background task";
  return t.length > 1 ? `${name} +${t.length - 1}` : name;
}
// bgSinceMs is when the oldest running task started (0 when unknown).
function bgSinceMs(a) {
  const ts = bgTasks(a).map((x) => Date.parse(x.since)).filter((n) => !Number.isNaN(n));
  return ts.length ? Math.min(...ts) : 0;
}
function bgTitle(a) {
  const t = bgTasks(a);
  return `Not finished: ${t.length} task${t.length === 1 ? "" : "s"} running in the background (the agent wakes when ${t.length === 1 ? "it ends" : "they end"}): ` +
    t.map((x) => `${x.kind}${x.label ? " " + x.label : ""}${x.since && !Number.isNaN(Date.parse(x.since)) ? " (" + fmtAgo(Date.parse(x.since)) + ")" : ""}`).join("; ");
}
// agentStatusText is the status word of an agent: "running" while only a
// task it started in the background goes on ("waiting" is kept for an agent
// that waits on the user).
function agentStatusText(a) {
  return bgRunning(a) ? "running" : a.agent_status;
}
// agentStatusCls styles it: the quiet blue "waiting" look (hollow dot), never the orange of an agent that needs you.
function agentStatusCls(a) {
  return bgRunning(a) ? "waiting" : a.agent_status;
}
// bgWhy is the line under the name: "Running: prelanding.sh +1".
function bgWhy(a) {
  return "Running: " + bgLabel(a);
}
// statusRank orders the list: waiting agents sit between working and done.
function statusRank(a) {
  return bgRunning(a) ? 1.5 : STATUS_ORDER[a.agent_status] ?? 9;
}

const LAND_PHASE = { rebase: "rebasing", tests: "tests running", push: "pushing", ci: "pushed, CI running" };
const LAND_NOTE_STALE_MS = 20 * 60 * 1000;

// landLabel says what a handoff's landing is doing, from what the hub saw
// and what the worker said, and nothing it did not: {cls, text, title}.
function landLabel(e, a) {
  const lc = e.land_check;
  const state = lc && (lc.state || (lc.ok ? "landed" : "failed"));
  if (e.status === "landed") {
    const as = lc && lc.landed && lc.landed.length ? ` as ${lc.landed.map((x) => x.slice(0, 7)).join(", ")}` : "";
    const where = `Landed on ${(lc && lc.base) || "its base"}${as}`;
    const ci = lc && lc.ci;
    if (ci && ci.state === "running") return { cls: "landing", text: "landed, CI running", title: `${where}. CI is running for the landed commit.` };
    if (ci && ci.state === "success") return { cls: "candidate", text: "landed, CI green", title: `${where}. CI passed (${ci.runs} run${ci.runs === 1 ? "" : "s"}).` };
    if (ci && ci.state === "failure") return { cls: "failed", text: "landed, CI failed", title: `${where}. CI failed${ci.url ? ": " + ci.url : ""}.` };
    return { cls: "candidate", text: "landed", title: where + (ci && ci.state === "none" ? ". No CI run was found for the landed commit." : ".") };
  }
  if (e.status === "finished" && lc && state === "failed" && e.approval && Date.parse(lc.at) >= Date.parse(e.finished_at || 0)) {
    const why = (lc.reason || lc.verdict || "").replace(/\s+/g, " ");
    return { cls: "failed", text: "land failed" + (why ? ": " + (why.length > 64 ? why.slice(0, 63) + "…" : why) : ""), title: "The landing failed: " + (lc.verdict || why) };
  }
  if (e.status !== "landing") return null;
  const approved = e.approval ? `Landing, approved by ${e.approval.by || "?"}: ${e.approval.note || ""}` : "Landing";
  const p = e.progress;
  const idle = a && (a.agent_status === "idle" || a.agent_status === "done");
  const checked = lc ? ` Last checked ${fmtAgo(Date.now() - Date.parse(lc.at))} ago${lc.stopped ? "; the hub stopped re-checking" : ""}.` : "";
  const why = lc && lc.reason ? ` ${lc.reason}.` : "";
  const q = e.queue;
  if (q && !q.turn)
    return { cls: "queued", text: `queued #${q.position}`, title: `${approved}. Waiting for its turn to land: ${q.ahead} landing${q.ahead === 1 ? "" : "s"} ahead of it in the queue (${q.total} in all). The hub tells the worker when it is up.` };
  const now = q && q.turn ? "landing now" : "landing";
  const turnNote = q && q.turn ? ` It holds the landing slot${q.total > 1 ? `; ${q.total - 1} more wait` : ""}${q.expires_at ? `, released at ${new Date(q.expires_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} at the latest` : ""}.` : "";
  if (a && bgRunning(a))
    return { cls: "landing", text: "landing: waiting on a background task", title: `${approved}. ${bgTitle(a)}.${checked}` };
  if (p && !(idle && Date.now() - Date.parse(p.at) > LAND_NOTE_STALE_MS)) {
    const t = LAND_PHASE[p.phase] || p.text.replace(/\s+/g, " ").slice(0, 60);
    return { cls: "landing", text: "landing: " + t, title: `${approved}. The worker's note ${fmtAgo(Date.now() - Date.parse(p.at))} ago: ${p.text}.${turnNote}${checked}` };
  }
  if (lc && state === "pending") {
    const t = lc.unpushed > 0 ? "not pushed yet" : lc.unmerged > 0 ? "not on main yet" : "checking";
    return { cls: "landing", text: q && q.turn && t !== "not pushed yet" && t !== "not on main yet" ? now : "landing: " + t, title: `${approved}.${turnNote}${why}${checked}` };
  }
  return { cls: "landing", text: now, title: approved + "." + turnNote };
}

/* ---------- progress notes ---------- */
//
// A handoff worker's latest `comebye report --progress` note shows under its
// name (list, opened agent, Status) with its age. It fades to muted when it is
// over 2 h old while the agent is still working. A working handoff worker that
// has run over 90 min with no note in the last 90 min gets a "no update" hint.
const NOTE_OLD_MS = 2 * 3600 * 1000, NOTE_QUIET_MS = 90 * 60 * 1000;

function progressNote(a) {
  const e = a && ledger.on === true ? ledgerEntry(a) : null;
  if (!e || (e.status !== "running" && e.status !== "landing")) return null;
  const working = a.agent_status === "working";
  const p = e.progress && e.progress.text ? e.progress : null;
  const at = p ? Date.parse(p.at) : NaN;
  const age = Number.isNaN(at) ? 0 : Math.max(0, Date.now() - at);
  const since = Date.parse(e.created_at);
  const last = p && !Number.isNaN(at) ? at : since;
  const noUpdate = e.status === "running" && working && !Number.isNaN(last) && Date.now() - last > NOTE_QUIET_MS && Date.now() - since > NOTE_QUIET_MS;
  if (!p && !noUpdate) return null;
  return { text: p ? p.text.replace(/\s+/g, " ") : "", at, age, old: !!p && working && age > NOTE_OLD_MS, noUpdate };
}
function noteHtml(a) {
  const n = progressNote(a);
  if (!n) return "";
  const hint = n.noUpdate ? `<span class="lg-chip nou" title="No progress note for over 90 minutes while it works.">no update</span>` : "";
  if (!n.text) return `<div class="pnote">${hint}</div>`;
  return `<div class="pnote${n.old ? " old" : ""}" title="${escAttr(n.text)}"><span class="pn-t">${esc(n.text)}</span><span class="pn-a">${esc(fmtAgo(n.at))} ago</span>${hint}</div>`;
}

function ledgerChip(a) {
  const id = agentId(a);
  if (ledger.marked.has(id)) return '<span class="lg-chip rtr">ready to remove</span>';
  const e = ledger.byAgent.get(id);
  if (!e) return "";
  const l = landLabel(e, a);
  if (l) return `<span class="lg-chip ${l.cls}" title="${esc(l.title)}">${esc(l.text)}</span>`;
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
  const rank = (list) => Math.min(...list.map(statusRank));
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
function toggleProject(name) { // name is a group's collapse key (groupKey), or "Recently closed"
  if (!name) return;
  const c = collapsed();
  if (c.has(name)) c.delete(name); else c.add(name);
  try { localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...c].slice(-200))); } catch {}
  renderList();
}

function projectHTML(name, list, mode = "project") {
  const key = groupKey(mode, name);
  const open = !collapsed().has(key);
  const nBlocked = list.filter((a) => a.agent_status === "blocked").length;
  const nWorking = list.filter((a) => a.agent_status === "working").length;
  const counts = (nBlocked ? `<span class="need">${nBlocked} need${nBlocked === 1 ? "s" : ""} input</span>` : "") +
    (nWorking ? `<span class="run">${nWorking} working</span>` : "") + `<span>${list.length}</span>`;
  // Project mode seats the worktree workers after the checkout's own agents; the other modes keep the list's order.
  const home = mode === "project" ? list.filter((a) => !isWorker(a)) : list;
  const workers = mode === "project" ? list.filter(isWorker) : [];
  // Needs you (Status mode) is never folded away, as the desktop's Needs you section is not: a waiting agent stays in sight.
  if (mode === "status" && name === "Needs you") return `<section class="proj needs-sec" data-group="status" aria-label="Needs you">
    <div class="proj-head static"><span class="proj-name">Needs you</span><span class="proj-counts"><span class="need">${list.length}</span></span></div>
    <div class="proj-body">${list.map((a) => agentRow(a, false)).join("")}</div></section>`;
  return `<section class="proj" data-group="${esc(mode)}">
    <button type="button" class="proj-head" data-proj="${esc(key)}" aria-expanded="${open}">
      <span class="proj-chev" aria-hidden="true"></span><span class="proj-name">${esc(name || "Other")}</span>
      <span class="proj-counts">${counts}</span>
    </button>
    <div class="proj-body"${open ? "" : " hidden"}>${home.map((a) => agentRow(a, false)).join("")}${workers.map((a) => agentRow(a, true)).join("")}</div>
  </section>`;
}

function renderRemovalBanner() {
  const b = $("rtr-banner");
  const n = ledger.on ? ledger.marked.size : 0;
  // On the phone the banner is the way into the page, so at zero it offers the sweep (the desktop has the sidebar entry and hides it).
  b.hidden = !ledger.on;
  b.classList.toggle("quiet", !n);
  if (ledger.on) b.innerHTML = `${n ? `Ready to remove (${n})` : "Look for finished agents"}<span class="ln-chev" aria-hidden="true">›</span>`;
  renderFleetMenu();
}

/* ---------- small helpers: browser storage, times ---------- */

function lsGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch {} }

function fmtWhen(iso) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  return new Date(t).toLocaleString(undefined, { weekday: "short", hour: "numeric", minute: "2-digit" });
}

// renderPlans shows the plans (several workers, one change) that are open
// or settled within the last day: how many workers finished and where each
// stands. The hub pushes when a plan settles; this is for looking.
const PLAN_SHOWN_MS = 24 * 3600 * 1000;
const planOpen = new Set(); // plans whose card is expanded (the page's own state)

// Dismissed plans are kept per browser (localStorage, by plan id): the hub
// has no per-user state, and a dismissal is a view preference.
const PLAN_DISMISSED_KEY = "comebye.plans.dismissed";
const planDismissed = new Set((() => {
  try { const a = JSON.parse(localStorage.getItem(PLAN_DISMISSED_KEY)); return Array.isArray(a) ? a.filter((x) => typeof x === "string") : []; } catch { return []; }
})());
function savePlanDismissed() {
  // Forget ids the ledger no longer has, so the list cannot grow for ever.
  const known = new Set((ledger.plans || []).map((p) => p && p.id));
  if (ledger.on === true && known.size) for (const id of [...planDismissed]) if (!known.has(id)) planDismissed.delete(id);
  try { localStorage.setItem(PLAN_DISMISSED_KEY, JSON.stringify([...planDismissed])); } catch {}
}

// planGone: a settled plan with every member landed or removed has nothing
// left to look at. A partial plan ("needs a look") is never hidden by itself.
function planAllGone(p) {
  return p.state === "done" && p.members.length > 0 && p.members.every((m) => m.status === "landed" || m.status === "removed");
}

// planMemberTarget says where a member leads: its agent while that is still
// running, else the archived session in Search (the worker was removed).
function planMemberTarget(m) {
  const a = m.agent_id ? agentById(m.agent_id) : null;
  if (a) return { hash: agentHash(a), live: a, tip: `Open ${m.repo_name || "?"}/${m.branch}` };
  return { hash: m.id ? "#/search/for?ledger=" + encodeURIComponent(m.id) : rcSearchHash(m.branch || m.repo_name || "", {}), live: null, tip: "This agent is gone: open its archived transcript" };
}

function planMemberHtml(m, p, open) {
  const t = planMemberTarget(m);
  // The outcome is what the member reported; the live status is what it does now.
  let live = "";
  if (m.followup === "landing") live = `<span class="pm-live landing">landing</span>`;
  else if (t.live && t.live.agent_status === "working" && m.outcome !== "pending") live = `<span class="pm-live working">working</span>`;
  const e = open ? ledger.entries.find((x) => x.id === m.id) : null;
  const when = e && Date.parse(e.created_at) ? ` · started ${fmtAgo(Date.parse(e.created_at))} ago` : "";
  const trans = m.id && t.live ? ` · <a class="pm-tr" href="#/search/for?ledger=${encodeURIComponent(m.id)}">Transcript</a>` : "";
  const detail = open ? `<div class="pm-detail">${esc(m.host || "")}${m.host ? " · " : ""}${esc(m.status)}${esc(when)}${trans}${e && e.final_report ? `<details class="pm-rep"><summary>Report${reportAgeHtml(e)}</summary>${copyBtnHtml("rep:" + e.id, e.final_report, "Copy report", "Copy report as Markdown", "cpb-rep")}<div class="md">${renderMarkdown(e.final_report, "pm" + e.id)}</div>${newerReplyHtml(e, "pm" + e.id)}</details>` : ""}</div>` : "";
  return `<li><a class="pm${t.live ? "" : " gone"}" href="${esc(t.hash)}" title="${esc(t.tip)}"><span class="pm-name">${esc(m.repo_name || "?")}/${esc(m.branch)}</span><span class="pm-${esc(m.outcome)}">${esc(m.outcome === "pending" ? m.status : m.outcome)}</span>${live}</a>${detail}</li>`;
}

function renderPlans() {
  const el = $("plans");
  if (!el) return;
  const now = Date.now();
  const shown = (ledger.on === true && Array.isArray(ledger.plans) ? ledger.plans : []).filter((p) => {
    if (!p || !Array.isArray(p.members)) return false;
    if (p.state === "open") return true;
    if (planDismissed.has(p.id)) return false;
    if (planAllGone(p)) return false;
    const at = Date.parse(p.settled_at);
    return !Number.isNaN(at) && now - at < PLAN_SHOWN_MS;
  });
  const sig = JSON.stringify([shown.map((p) => [p.id, p.state, p.done, p.landing, p.working, planOpen.has(p.id), planDismissed.size, p.members.map((m) => [m.id, m.outcome, m.status, m.followup, m.agent_id, m.reported, !!(m.agent_id && agentById(m.agent_id)), m.agent_id && agentById(m.agent_id)?.agent_status])]), ledger.entries.length]);
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
    const settled = p.state !== "open";
    const collapsed = p.state === "done" && !open; // done: one line until opened; partial never collapses
    const follow = [p.landing ? `${p.landing} landing` : "", p.working ? `${p.working} working` : ""].filter(Boolean).join(", ");
    const created = Date.parse(p.created_at);
    const state = `${label[p.state] || p.state}${follow ? ` · ${follow}` : ` · ${p.done} of ${p.size}`}`;
    const tip = `${p.title}\n${open ? "Click to collapse" : collapsed ? "Click to show the members" : "Click for branches and reports"}`;
    return `<div class="plan ${esc(p.state)}${collapsed ? " collapsed" : ""}${settled ? " settled" : ""}">
      <div class="plan-head"><button type="button" class="plan-title" data-plan="${esc(p.id)}" aria-expanded="${open}" title="${esc(tip)}">${p.state === "done" ? `<span class="plan-check" aria-hidden="true">✓</span> ` : ""}${esc(p.title)}</button><span class="plan-state">${esc(state)}</span></div>
      ${settled ? `<button type="button" class="plan-x" data-dismiss="${esc(p.id)}" aria-label="Dismiss this plan" title="Dismiss (hides it in this browser)">×</button>` : ""}
      ${open ? `<div class="plan-meta">${p.done} of ${p.size} finished${created ? ` · started ${fmtAgo(created)} ago` : ""}${p.by ? ` · by ${esc(p.by)}` : ""}</div>` : ""}
      ${collapsed ? "" : `<ul class="plan-members">${p.members.map((m) => planMemberHtml(m, p, open)).join("")}</ul>`}
    </div>`;
  }).join("");
  if (focus) {
    const again = [...el.querySelectorAll("[data-plan], a.pm")].find((x) => x.dataset.plan === focus || x.getAttribute("href") === focus);
    if (again) again.focus({ preventScroll: true });
  }
}

function initPlans() {
  $("plans").addEventListener("click", (ev) => {
    const x = ev.target.closest("button.plan-x");
    if (x) {
      planDismissed.add(x.dataset.dismiss);
      savePlanDismissed();
      renderPlans();
      return;
    }
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
// The panel sits at the top of the list on the phone; on a wide screen it is the
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
const TODO_KIND = { decision: "decision", action: "for you", followup: "follow-up", release: "release", review: "review", approval: "approve" };
const todos = { on: null, items: [], open: 0, answered: 0, loadedAt: 0, inflight: false,
  openId: null, lastId: null, nxt: null, ansOpen: false, seen: new Map(), order: [], follow: false,
  drafts: loadTodoDrafts(), pend: new Overlay(),
  deep: null, deepId: null, deepHash: null, pin: null, notice: null };

function todoLinkHref(u) {
  u = u || "";
  if (/^#\/todo\/t[0-9a-f]{10}$/.test(u)) return "/" + u; // same hub: opens in the app
  return /^https?:\/\//i.test(u) || /^\/#\/(agent\/[^\s]+|pages\/pg_[0-9a-f]{10}|todo\/t[0-9a-f]{10}|remove)$/.test(u) ? u : "";
}
const todoRepOpen = new Set(); // items whose "Full report" is expanded: a redraw keeps it
// The provenance chip: items the hub filed itself say so (and why).
const TODO_SOURCE = { "hub-quiet": "auto · quiet", "hub-blocked": "auto · blocked", "hub-question": "auto · question", "hub-report": "from report", "hub-sweep": "auto · sweep" };
function todoSrcChip(t) {
  const label = TODO_SOURCE[t.source];
  return (label ? `<span class="td-chip td-src" title="Filed by the hub, not by an agent or a person">${esc(label)}</span>` : "") +
    (t.suggested && t.state === "open" ? `<span class="td-chip td-sug" title="The hub guessed this from the agent's last message: keep it or dismiss it">suggested</span>` : "");
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
  if (todos.inflight || ledger.on !== true) { if (ledger.on !== true) { todos.on = false; renderTodos(); if (ledger.on === false) todoDeepResolve(); } return; }
  todos.inflight = true;
  try {
    const d = await api("/api/todos");
    todos.on = true;
    todos.items = Array.isArray(d.todos) ? d.todos : [];
    todos.open = d.open | 0;
    todos.answered = d.answered | 0;
    // A done or dropped item a deep link opened is not in the list: keep it while it is the open one.
    if (todos.pin && todos.openId === todos.pin.id && !todos.items.some((x) => x.id === todos.pin.id)) todos.items.push(todos.pin);
    todoReapply();
  } catch (e) {
    if (e.status === 503 || e.status === 404) todos.on = false;
    // Otherwise keep what we had: a failed poll should not make the panel blink.
  } finally {
    todos.inflight = false;
    todos.loadedAt = Date.now();
  }
  renderTodos();
  todoDeepResolve();
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
// ---- review cards (kind "review", internal/server/review.go) ----
// A review is about a Page version: the card shows the hub's thumbnail of it, what to try, and two buttons. Looks good
// closes the item at once and the hub holds the message to the worker for the Undo window (review.verdict.undo_until,
// the hub's clock, not a timer here); Needs changes takes a note and a kind and files a Fix item.
const REVIEW_KINDS = [["fix", "Fix: it is wrong"], ["idea", "Idea for later"], ["looks_off", "Looks off"]];
todos.rv = new Map();      // item id -> { kind } while the Needs changes note is open
todos.rvBusy = new Set();  // item ids whose verdict is on the wire
function todoIsReview(t) { return t.kind === "review" && !!t.page; }
function reviewThumbHtml(t) {
  const p = t.page, sc = pgsScheme(), have = p.thumbs || [];
  const use = have.includes(sc) ? sc : have[0];
  const src = use && p.thumb_url ? p.thumb_url.replace("{scheme}", use) : "";
  const href = todoLinkHref(p.url);
  const pic = src ? `<img class="rv-img" src="${escAttr(src)}" alt="" decoding="async" loading="lazy">` : `<span class="rv-nopic">No preview yet</span>`;
  const title = esc(p.title || p.id);
  const tag = href ? `a href="${escAttr(href)}" title="Open the demo"` : "div";
  return `<div class="rv-page"><${tag} class="rv-thumb${src ? " has-img" : ""}">${pic}</${href ? "a" : "div"}>
      <div class="rv-pinfo"><b class="rv-ptitle">${title}</b><span class="rv-pver">v${p.version}${p.by ? ` · by ${esc(p.by)}` : ""}</span>${href ? `<a class="rv-open" href="${escAttr(href)}">Open demo ↗</a>` : ""}</div></div>`;
}
// Where the verdict goes, in words (the hub's rule: the publisher if its pane is in the fleet, else a coordinator).
function reviewRouteLine(t) {
  const p = t.page, ag = p.agent && agentById(p.agent);
  const who = p.by || "the worker";
  return ag
    ? `Goes to ${esc(who)}, who is still running. If that worker is gone, it goes to the coordinator.`
    : `${esc(who)} is no longer running, so this goes to the coordinator.`;
}
function reviewDoneHtml(t) {
  const rv = t.review || {}, v = rv.verdict, r = rv.route;
  if (!v) return "";
  let line = "";
  if (r) {
    const to = r.name ? esc(r.name) : "the " + esc(r.via || "coordinator");
    line = r.state === "sent" ? `Told ${to}.` : r.state === "expired" ? "Nobody could be told. The verdict is on record." : "Telling " + (r.name ? to : "the worker or the coordinator") + " when the Undo time ends.";
  }
  const fix = rv.fix_id ? ` A Fix item (${esc(rv.fix_id)}) is filed for the coordinator.` : "";
  return `<div class="rv-done">${line}${fix}</div>`;
}
const RV_CHAT_TITLE = "Start a read-only chat with a helper that has read this Page";
function reviewBarHtml(t) {
  if (t.page && !pgsChatOff.has(t.page.id)) setTimeout(() => pgsChatGate(t.page.id), 0); // asked once per Page; a refused kind greys the button
  const st = todos.rv.get(t.id), busy = todos.rvBusy.has(t.id) ? " disabled" : "";
  const stale = t.review && t.review.stale ? `<div class="rv-stale">Waiting for 3 days or more.</div>` : "";
  const kinds = st ? `<div class="rv-note"><div class="seg sm rv-kinds" role="group" aria-label="What kind of note">${REVIEW_KINDS.map(([k, l]) => `<button type="button" class="rv-kind" data-k="${k}" aria-pressed="${st.kind === k}">${esc(l)}</button>`).join("")}</div>
      <textarea class="td-text rv-text" id="rv-note-${escAttr(t.id)}" rows="3" maxlength="4000" placeholder="What should change? (optional)" aria-label="Note for the worker">${esc(todoDraft(t.id))}</textarea>
      <div class="td-acts"><button type="button" class="rv-send"${busy}>Send: Needs changes</button><button type="button" class="rv-cancel">Cancel</button></div></div>` : "";
  return `<div class="td-form rv-bar">${stale}
      <div class="rv-btns"><button type="button" class="btn primary rv-good"${busy} title="Closes the item. You can undo for a few seconds. Key: 1"><kbd>1</kbd> ✓ Looks good</button><button type="button" class="btn warn rv-chg" aria-expanded="${!!st}"${busy} title="Add a note and file a Fix item. Key: 2"><kbd>2</kbd> ✎ Needs changes</button><span class="rv-chatrow"><button type="button" class="btn rv-chat" data-page="${escAttr(t.page.id)}"${busy}${pgs.chatBusy ? ' aria-busy="true"' : ""}${pgsChatOff.get(t.page.id) ? ` aria-disabled="true" title="${escAttr(pgsChatOff.get(t.page.id))}"` : ` title="${RV_CHAT_TITLE}"`}>💬 Chat about this</button><button type="button" class="btn rv-chatpick" data-page="${escAttr(t.page.id)}"${busy} aria-label="Chat with another agent" title="Pick the host, agent and model for the chat">▾</button></span></div>
      ${kinds}
      <div class="rv-foot"><div class="rv-route">${reviewRouteLine(t)}</div><div class="td-acts">${todoOpenBtn(t)}<button type="button" class="td-drop">Drop</button></div></div></div>`;
}

// reviewVerdict sends the verdict. The hub closes the item and holds the message to the worker until the Undo window
// ends, so Undo is a second request inside that window; the toast counts down to review.verdict.undo_until.
async function reviewVerdict(id, verdict, item) {
  const t = todos.items.find((x) => x.id === id);
  if (!t || t.state !== "open" || todos.rvBusy.has(id)) return;
  const box = item && item.querySelector(".rv-text");
  const st = todos.rv.get(id);
  const note = rsJoin(t, box ? box.value.trim() : todoDraft(id).trim(), RS_LIMIT.note);
  if (note === null) return;
  const body = { verdict, note };
  if (verdict === "changes") body.kind = (st && st.kind) || "fix";
  const next = todoNextOpen(id, todoOrder().map((x) => x.id));
  const hadFocus = todoHasFocus();
  todos.rvBusy.add(id);
  renderTodos(true);
  let r;
  try {
    r = await api(`/api/todos/${encodeURIComponent(id)}/verdict`, { method: "POST", body: JSON.stringify(body) });
  } catch (e) {
    todos.rvBusy.delete(id);
    renderTodos(true);
    toast("Not sent: " + e.message, true);
    return;
  }
  todos.rvBusy.delete(id);
  todos.rv.delete(id);
  todos.drafts.delete(id); saveTodoDrafts();
  rsNotesSent(t);
  if (todos.openId === id) { todos.openId = next; if (next) todos.lastId = next; todos.follow = hadFocus; }
  const i = todos.items.findIndex((x) => x.id === id);
  if (i >= 0 && r && r.todo) todos.items[i] = r.todo;
  todos.loadedAt = 0;
  const done = r && r.todo, until = done && done.review && done.review.verdict && done.review.verdict.undo_until ? Date.parse(done.review.verdict.undo_until) : NaN;
  if (verdict === "good" && !Number.isNaN(until) && until > Date.now()) {
    flushChoiceUndo();
    toastAction("Looks good. The worker is told in a moment", "Undo", () => reviewUndo(id), until - Date.now());
  } else {
    toast(verdict === "good" ? "Looks good" : "Needs changes sent. A Fix item is filed.");
  }
  await loadTodos();
}
async function reviewUndo(id) {
  try {
    await api(`/api/todos/${encodeURIComponent(id)}/undo`, { method: "POST", body: "{}" });
    toast("Taken back. The review is open again.");
    todos.openId = id; todos.lastId = id;
  } catch (e) {
    toast("Too late to undo: " + e.message, true);
  }
  todos.loadedAt = 0;
  await loadTodos();
}

// ---- split review mode (issue #17, slice 3b) ----
// An item about a Page (a review, or a decision set from a report) opens with the Page beside it: the Page in a frame on
// the left, the item's own panel (the verdict, the decisions or the answer box) on the right. On a phone the two are
// tabs of one full-screen card. The frame opens the version the item was filed with (t.page.version). The Pages
// comments strip and tray (pgc*) work on that frame, and what the user selected and noted goes with the answer: into
// the verdict's note, the answer's text or the set's submit. On the desktop the card covers the To do page; the open
// item is still todos.openId, so j and k step through the items and Escape closes it (see onTodoKey).
const rs = { el: null, key: "", tab: "page", sig: "", loading: false };
const RS_LIMIT = { note: 1900, answer: 3800 }; // bytes: the hub cuts a verdict note at 2 KiB and an answer at 4 KiB
function rsWants(t) { return !!t && !!t.page && todos.on === true && (!isDesktop() || state.view === "todo"); }
function rsItem() { const t = todos.openId && todos.items.find((x) => x.id === todos.openId); return rsWants(t) ? t : null; }
function rsActive() { return !!rs.el && !rs.el.hidden; }

// The comments the user selected on this item's Page version and has not sent yet.
function rsNotes(t) {
  if (!t || !t.page || pgc.id !== t.page.id || pgc.ver !== t.page.version) return [];
  return pgcItems();
}
function rsNotesText(t) {
  const n = rsNotes(t);
  if (!n.length) return "";
  const q = (x) => (x.length > 90 ? x.slice(0, 89) + "…" : x);
  return `On the Page (v${t.page.version}):\n` + n.map((c) => `- “${q(c.quote)}”: ${c.note}`).join("\n");
}
const rsBytes = (x) => new TextEncoder().encode(x).length;
// withNotes joins the typed text and the Page notes; null (and a toast) when they will not fit.
function rsJoin(t, typed, limit) {
  const n = rsNotesText(t);
  const all = typed && n ? typed + "\n\n" + n : typed || n;
  if (rsBytes(all) > limit) { toast("Too long to send with the answer: shorten the notes, or send the comments on their own from the Page.", true); return null; }
  return all;
}
function rsNotesSent(t) {
  if (!t || !t.page || pgc.id !== t.page.id) return;
  pgc.drafts.delete(pgcKey());
  pgcRender();
}
function rsNotesHtml(t) {
  const n = rsNotes(t).length;
  return `<div class="rs-notes" id="rs-notes"${n ? "" : " hidden"}>${n ? `${n} comment${n === 1 ? "" : "s"} on the Page ${n === 1 ? "goes" : "go"} with your answer.` : ""}</div>`;
}
// Called when the comments change: refresh the line in the side panel without a redraw.
function rsNotesRefresh() {
  const el = $("rs-notes"), t = rsItem();
  if (!el || !t) return;
  const n = rsNotes(t).length;
  el.hidden = !n;
  el.textContent = n ? `${n} comment${n === 1 ? "" : "s"} on the Page ${n === 1 ? "goes" : "go"} with your answer.` : "";
  const tab = rs.el && rs.el.querySelector('[data-rs-tab="decide"] .rs-n');
  if (tab) { tab.hidden = !n; tab.textContent = String(n); }
}

function rsEnsure() {
  if (rs.el) return rs.el;
  const el = document.createElement("div");
  el.id = "rs"; el.className = "rs"; el.hidden = true;
  el.setAttribute("role", "region"); el.setAttribute("aria-label", "Review with the Page");
  el.addEventListener("click", (ev) => {
    if (ev.target.closest(".rs-back")) { todoCollapse(); return; }
    const tb = ev.target.closest("[data-rs-tab]");
    if (tb) { rs.tab = tb.dataset.rsTab; el.dataset.tab = rs.tab; for (const b of el.querySelectorAll("[data-rs-tab]")) b.setAttribute("aria-pressed", String(b === tb)); return; }
    onTodoClick(ev);
  });
  el.addEventListener("input", onTodoInput);
  el.addEventListener("change", onTodoChange);
  rs.el = el;
  return el;
}

// leave: the app is going somewhere else (a page, an agent), so the history entry the split pushed becomes an ordinary one
// (replaceState) instead of being popped: a history.back() here would run after the navigation's own pushState and undo it.
function rsTeardown(leave) {
  if (!rs.el) return;
  if (!rs.el.hidden) {
    rs.el.hidden = true; rs.el.replaceChildren(); rs.key = ""; rs.sig = ""; pgc.split = false;
    if (history.state && history.state.rs) { // the entry the split pushed
      if (leave) history.replaceState(null, "", location.href); else history.back();
    }
  }
}
// rsLeave closes the split because something else is opening (an agent, another page). The item is closed too, so coming back to
// the list does not put the split up again; the unsent answer and notes stay (todoDraft, the Page comments).
function rsLeave() {
  if (!rsActive()) return;
  if (todos.openId) todos.lastId = todos.openId;
  todos.openId = null;
  rsTeardown(true);
}
// On a phone the split sits under the app's header, so the header's buttons (commands, search, the fleet menu) stay reachable.
function rsPlace() {
  if (!rs.el) return;
  const h = !isDesktop() && document.querySelector("#list > header");
  rs.el.style.top = h ? Math.max(0, Math.round(h.getBoundingClientRect().bottom)) + "px" : "";
}
// The split pushes one history entry when it opens, so the phone's Back closes it (route() calls this). Stepping to another
// item with j and k keeps the one entry.
function syncSplit() {
  if (rsActive() && !(history.state && history.state.rs)) todoCollapse();
}

function renderSplit(force) {
  const t = rsItem();
  if (!t) { rsTeardown(); return; }
  const el = rsEnsure(), p = t.page, ver = p.version, key = p.id + "/" + ver;
  const host = isDesktop() ? $("p-todo") : document.body;
  if (el.parentElement !== host) { host.append(el); rs.key = ""; }
  el.classList.toggle("on-phone", !isDesktop());
  rsPlace();
  if (!pgs.loaded) {
    // pgs.pagesUrl (a separate origin for pages, if any) comes with the Pages list.
    if (!rs.loading) { rs.loading = true; pagesLoad().finally(() => { rs.loading = false; renderSplit(true); }); }
    if (el.hidden) {
      if (!(history.state && history.state.rs)) history.pushState({ rs: true }, "", location.href); // Back closes it even while the Pages load
      el.hidden = false; el.innerHTML = `<div class="pgs-pick">Loading…</div>`; rs.key = "";
    }
    rsPlace();
    return;
  }
  const first = el.hidden || rs.key !== key;
  if (first) {
    if (!(history.state && history.state.rs)) history.pushState({ rs: true }, "", location.href);
    el.hidden = false;
    rs.key = key; rs.sig = "";
    el.dataset.tab = rs.tab;
    const href = todoLinkHref(p.url);
    const title = esc(p.title || p.id);
    el.innerHTML = `<div class="rs-bar">
        <button type="button" class="rs-back btn sm" aria-label="Back to the list" title="Back to the list (Esc)"><svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true"><path d="m15 5-7 7 7 7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg><span class="rs-backt">To do</span></button>
        <h2 class="rs-title">${title}</h2><span class="pgs-pill">v${ver}</span>
        ${href ? `<a class="rs-open" href="${escAttr(href)}" target="_blank" rel="noopener" title="Open the Page on its own">Open ↗</a>` : ""}
        <span class="seg sm rs-tabs" role="group" aria-label="Show"><button type="button" data-rs-tab="page" aria-pressed="${rs.tab === "page"}">Page</button><button type="button" data-rs-tab="decide" aria-pressed="${rs.tab === "decide"}">${esc(t.set ? "Decide" : t.kind === "review" ? "Verdict" : "Answer")}<span class="rs-n" hidden></span></button></span>
      </div>
      <div class="rs-cols"><section class="rs-page" aria-label="The Page"><div class="pgs-sel" id="pgs-sel" hidden></div><div class="rs-body"><iframe class="pgs-frame" style="color-scheme:${pgsScheme()}" sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox" allow="clipboard-write" referrerpolicy="no-referrer" title="${escAttr(p.title || p.id)}" src="${escAttr((pgs.pagesUrl || "") + "/p/" + p.id + "/" + ver)}"></iframe><aside class="pgs-tray" id="pgs-tray" aria-label="Comments" hidden></aside></div></section>
        <section class="rs-side" id="rs-side" aria-label="${escAttr(TODO_KIND[t.kind] || "item")}"></section></div>`;
    const fr = el.querySelector("iframe.pgs-frame");
    fr.addEventListener("load", sendPageTheme);
    fr.addEventListener("load", pgcMarks);
    pgcOpen(p, ver, true);
  }
  const side = $("rs-side");
  const typing = side.contains(document.activeElement) && /^(TEXTAREA|INPUT)$/.test(document.activeElement.tagName);
  const sig = JSON.stringify([t, todos.rv.get(t.id), todos.rvBusy.has(t.id), todoDraft(t.id), todos.pend.busy(t.id), [...(todos.setBusy.has(t.id) ? [1] : [])], todos.setStep.get(t.id), [...todos.setOther], pgsScheme(), rs.tab]);
  if (first || force === true || (sig !== rs.sig && !typing)) {
    rs.sig = sig;
    todoPaint(side, todoItemHtml(t, "rs", true));
    rsNotesRefresh();
  }
}

// A thumbnail that fails to load gives way to the placeholder.
document.addEventListener("error", (ev) => {
  const im = ev.target;
  if (!im || !im.classList || !im.classList.contains("rv-img")) return;
  const box = im.parentElement;
  im.replaceWith(Object.assign(document.createElement("span"), { className: "rv-nopic", textContent: "No preview yet" }));
  if (box) box.classList.remove("has-img");
}, true);

// The agent an item is about, for its "Open session" button: the one it is waiting on, else the one that filed it, else the one
// running the handoff it links to. null when the item names none. live is the agent in the fleet now (not one that has gone,
// or that a restarted host has replaced); why is the plain reason there is no session to open.
function todoSessionAgent(t) {
  const cands = [];
  const add = (id, name) => { if (id && !cands.some((c) => c.id === id)) cands.push({ id, name: name || "" }); };
  if (t.waiting && t.waiting.id && !t.waiting.gone) add(t.waiting.id, t.waiting.name);
  add(t.created_by_id, t.created_by);
  for (const l of t.links || []) {
    const e = l.handoff ? (ledger.entries || []).find((x) => x.id === l.handoff) : null;
    if (e && e.agent_id) add(e.agent_id, e.name || e.branch);
  }
  if (!cands.length) return t.waiting && t.waiting.id ? { id: t.waiting.id, name: t.waiting.name || "", live: false, why: `${t.waiting.name || "The agent"} is no longer running, so there is no session to open.` } : null;
  const c = cands.find((x) => agentById(x.id));
  if (c) return { id: c.id, name: c.name, live: true, why: "" };
  return { ...cands[0], live: false, why: `${cands[0].name || "The agent"} is no longer running, so there is no session to open.` };
}
// The button: it opens that agent's session with the prompt box focused, and leaves the item as it is. With no live agent it is
// greyed (still reachable by keyboard, so its reason can be read) and says why.
function todoOpenBtn(t) {
  const s = todoSessionAgent(t);
  if (!s) return "";
  return s.live
    ? `<button type="button" class="td-open" data-agent="${escAttr(s.id)}" title="${escAttr(`Open ${s.name || "the agent"}'s session to read it and type to it. This item stays open.`)}">Open session</button>`
    : `<button type="button" class="td-open" aria-disabled="true" data-why="${escAttr(s.why)}" title="${escAttr(s.why)}">Open session</button>`;
}
// todoOpenSession: the same view as opening the agent from the list, then the prompt box.
function todoOpenSession(btn) {
  if (btn.getAttribute("aria-disabled") === "true") { toast(btn.dataset.why || "That agent is no longer running.", true); return; }
  const id = btn.dataset.agent;
  if (!agentById(id)) { toast("That agent is no longer running, so there is no session to open.", true); return; }
  rsLeave(); // the split's history entry must not be popped after the agent route is pushed
  openDetail(id);
  requestAnimationFrame(() => { const p = $("prompt-text"); if (p && p.offsetParent) p.focus(); });
}

// mainCoordinatorRole: the hub's role for the main coordinator ({id, role}), or null with no coordinator at all. The hub marks
// one main (liveRoles falls back to the first live one when none is marked); a role can outlive its agent.
function mainCoordinatorRole() {
  const cs = [...ledger.roles].filter(([, r]) => r && r.role === "coordinator");
  const m = cs.find(([, r]) => r.main) || cs.find(([id]) => agentById(id)) || cs[0];
  return m ? { id: m[0], name: m[1].name || "" } : null;
}
// openMainCoordinator (`g c`): the main coordinator's session, as clicking its row does, with the prompt focused. A new history
// entry (openDetail pushes), so Back returns to where you were. With the main coordinator gone it says so and stays put.
function openMainCoordinator() {
  const m = mainCoordinatorRole();
  if (!m) { toast("No coordinator is running", true); return; }
  if (!agentById(m.id)) { toast(`The main coordinator${m.name ? " (" + m.name + ")" : ""} is no longer running`, true); return; }
  rsLeave();
  openDetail(m.id);
  requestAnimationFrame(() => { const p = $("prompt-text"); if (p && p.offsetParent) p.focus(); });
}

// The hub's own relay of an answer (relay.go): a closed item it relayed says where the answer went,
// so "done" does not read as an answer that was dropped.
function todoSentTo(t) {
  const r = t.relay;
  return r && r.state === "delivered" && t.state === "done" ? r.to || (t.waiting && (t.waiting.name || t.waiting.id)) || "the agent" : "";
}
function todoStateChip(t) {
  const to = todoSentTo(t);
  if (to) return `<span class="td-chip st-answered" title="The hub typed your answer into ${escAttr(to)}'s pane">${esc("Sent to " + to)}</span>`;
  return `<span class="td-chip st-answered">${esc(t.state === "open" ? "answered" : t.state)}</span>`;
}
function todoRelayNote(t) {
  const r = t.relay;
  if (!r) return "";
  const at = Date.parse(r.at), ago = Number.isNaN(at) ? "" : ` ${fmtAgo(at)} ago`;
  if (todoSentTo(t)) return `Sent to ${todoSentTo(t)} by the hub${ago}${r.notice ? `; it reached the agent's transcript (notice ${r.notice})` : ""}.`;
  if (r.state === "queued" && t.state === "answered") return "Queued by the hub: it is typed when the agent can take it.";
  if (r.state === "gaveup" && t.state === "answered") return "The hub could not pass it on; the coordinator relays it.";
  return "";
}

// asPanel: only the panel, for the split review mode's side (the Page is beside it, so no row and no thumbnail).
function todoItemHtml(t, pfx, asPanel) {
  const at = Date.parse(t.created_at);
  const age = Number.isNaN(at) ? "" : fmtAgo(at);
  const who = t.waiting ? t.waiting.name || t.waiting.id : "";
  const open = asPanel || todos.openId === t.id;
  const inSplit = !asPanel && rsWants(t); // the panel is in the split view, not under the row
  const ans = t.state !== "open";
  const draft = todoDraft(t.id).trim() !== "";
  const rid = `${pfx}-row-${t.id}`, pid = `${pfx}-panel-${t.id}`;
  const chips = `<span class="td-chip td-draft"${draft ? "" : " hidden"}>draft</span>` + todoSrcChip(t) +
    (ans ? todoStateChip(t) : `<span class="td-chip st-open">open</span>`) +
    (todos.pend.busy(t.id) ? `<span class="td-chip st-sending">sending…</span>` : "") +
    (t.kind === "review" && !ans && t.review && t.review.stale ? `<span class="td-chip st-warn" title="Open for 3 days or more">stale</span>` : "");
  let panel = "";
  if (open && !inSplit) {
    const meta = [who && `waiting: ${who}`, age].filter(Boolean).map(esc).join(" · ");
    const repEntry = (l) => (ledger.entries || []).find((x) => x.id === l.handoff);
    // "Full report" is the hub's stored final report, shown in place; the transcript is a link of its own.
    const isRep = (l) => l.handoff && !l.url && (l.kind === "report" || (!l.kind && /^Full report/.test(l.label || "")));
    let repLink = null;
    // An item's Page comes first, in every layout, unless links already has it.
    const pgHref = t.page ? todoLinkHref(t.page.url) : "";
    const pgLink = pgHref && !(t.links || []).some((l) => l.url === t.page.url || l.url === pgHref) ? `<a class="td-pglink" href="${escAttr(pgHref)}">${esc("Page: " + (t.page.title || t.page.id))}</a>` : "";
    const links = (pgLink ? [pgLink] : []).concat((t.links || []).map((l) => {
      if (isRep(l)) {
        const e = repEntry(l);
        if (e && e.final_report) { repLink = { l, e }; return `<a href="#" class="td-reptog" role="button">${esc(l.label.replace(/\s*\(handoff [^)]*\)/g, "") || "Full report")}</a>`; }
      }
      const href = todoLinkHref(l.url);
      const label = l.label || l.url || `handoff ${l.handoff}`;
      return href
        ? (href.startsWith("/#/") ? `<a href="${escAttr(href)}">${esc(label)}</a>` : `<a href="${escAttr(href)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`)
        : l.handoff ? `<a class="td-tr" href="#/search/for?ledger=${encodeURIComponent(l.handoff)}" title="Open that handoff's transcript">${esc(l.kind === "transcript" || isRep(l) ? (l.kind === "transcript" ? label : "Transcript") : label.includes(l.handoff) ? label : `${label} (handoff ${l.handoff})`)}</a>`
        : `<span>${esc(label)}</span>`;
    }));
    const repBody = repLink ? `<details class="td-rep"${todoRepOpen.has(t.id) ? " open" : ""}><summary>Full report${reportAgeHtml(repLink.e)}</summary>${copyBtnHtml("rep:" + repLink.e.id, repLink.e.final_report, "Copy report", "Copy report as Markdown", "cpb-rep")}<div class="md">${renderMarkdown(repLink.e.final_report, "todorep:" + t.id)}</div>${newerReplyHtml(repLink.e, "todorep:" + t.id)}</details>` : "";
    const a = t.answer;
    const aAt = a ? Date.parse(a.at) : NaN;
    const answered = a
      ? `<div class="td-answer"><div class="td-answer-h">${esc(a.via === "relay" ? "Answer, relayed by " : "Your answer, ")}${a.via === "relay" ? esc(a.by) + " " : ""}${Number.isNaN(aAt) ? "" : esc(fmtAgo(aAt)) + " ago"}</div>
          <div class="td-answer-t">${renderInline(a.text)}</div>
          <div class="td-wait">${esc(todoRelayNote(t) || (t.state === "answered" && a.via === "app" ? "Waiting for the coordinator to pass it on." : ""))}</div></div>` : "";
    const nx = todos.nxt && todos.nxt !== t.id ? todos.items.find((x) => x.id === todos.nxt) : null;
    const agentId = t.waiting && t.waiting.id;
    const strip = `<div class="td-strip">
        ${who ? `<div><span class="td-sl">Waiting</span><span class="td-sv"><span class="td-adot"></span>${agentId && agentById(agentId) ? `<button type="button" class="td-agent" data-agent="${escAttr(agentId)}" title="Open ${escAttr(who)}">${esc(who)} ↗</button>` : esc(who)}${t.waiting && t.waiting.parked ? ' <span class="chip teal" title="Its pane is closed. Whatever is sent to it waits and is delivered when it is resumed.">parked</span>' : ""}</span></div>` : ""}
        ${age ? `<div><span class="td-sl">Asked</span><span class="td-sv">${esc(age)} ago</span></div>` : ""}
        ${links.length ? `<div><span class="td-sl">Links</span><span class="td-sv td-dlinks">${links.join("")}</span></div>` : ""}</div>`;
    const dhead = `<div class="td-dh"><div class="td-dtop"><span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span>${todoSrcChip(t)}${ans ? todoStateChip(t) : `<span class="td-chip st-open">open</span>`}</div>
        <h3 class="td-dtitle">${renderInline(t.question.replace(/\s*\n\s*/g, " "), false)}</h3>${strip}</div>`;
    const isRel = t.kind === "release" && !!t.release;
    const after = ans || isRel || todoIsReview(t) || t.kind === "approval" ? "" : `<div class="td-after">${who ? `Goes to ${esc(who)} through the coordinator. ` : ""}${nx ? `Next: ${esc(nx.question.replace(/\s*\n\s*/g, " ").slice(0, 70))}` : "That clears the queue."}</div>`;
    const kept = ans && draft ? todoKeptHtml(t.id, todoDraft(t.id), "Not sent: this was answered before you sent yours.") : "";
    const busy = "";
    const isRev = todoIsReview(t);
    const isAp = t.kind === "approval" && !!t.approval;
    const form = ans || isRel || isRev || isAp ? ""
      : `<textarea class="td-text" id="${pfx}-answer-${escAttr(t.id)}" rows="2" maxlength="4000" placeholder="Your answer" aria-label="Your answer">${esc(todoDraft(t.id))}</textarea>`;
    panel = `<div class="td-panel" id="${escAttr(pid)}" role="region" aria-labelledby="${escAttr(rid)}">
        ${dhead}
        <div class="td-scroll">
          ${ans && todos.deepId === t.id ? `<div class="td-deepnote" role="note">${esc(t.state === "dropped" ? "Dropped" + (t.drop_reason ? ": " + t.drop_reason : "") + ". Read only." : t.state === "done" ? (todoSentTo(t) ? "Sent to " + todoSentTo(t) + ". Read only." : "Done. Read only.") : "Already answered. Read only.")}</div>` : ""}
          ${meta ? `<div class="td-meta">${meta}</div>` : ""}
          ${isRel ? relPingHtml(t) : ""}
          ${asPanel ? rsNotesHtml(t) : ""}
          ${isRev && !asPanel ? reviewThumbHtml(t) : ""}
          ${isRev && t.context ? `<div class="rv-tryh">What to try</div>` : ""}
          ${t.set ? setBodyHtml(t) : t.context ? `<div class="md td-ctx">${renderMarkdown(t.context, "todo:" + t.id + ":" + t.updated_at)}</div>` : ""}
          ${links.length ? `<div class="td-links">${links.join("")}</div>` : ""}
          ${repBody}
          <div class="td-cprow">${copyBtnHtml("todo:" + t.id, t.question + (t.context ? "\n\n" + t.context : ""), "Copy question and context", "Copy the question and its context as Markdown")}</div>
          ${answered}${kept}${isRev && ans ? reviewDoneHtml(t) : ""}
        </div>
        ${t.set ? setBarHtml(t) : isAp ? approvalBarHtml(t) : isRev && !ans ? reviewBarHtml(t) : `<div class="td-form">${ans || isRel ? "" : todoOptsHtml(t)}${form}
          ${ans || isRel ? "" : sttStatusHtml("todo:" + t.id)}
          <div class="td-acts">
            ${ans || isRel ? "" : sttMicHtml("todo:" + t.id)}
            ${t.suggested && !ans ? `<button type="button" class="td-keep" title="Make this a real item">Keep</button><button type="button" class="td-dismiss" data-why="not_a_question">Not a question</button><button type="button" class="td-dismiss" data-why="already_handled">Already handled</button>` : ""}
            ${ans || isRel ? "" : `<button type="button" class="td-answer-btn"${busy}>Answer</button>`}
            ${todoOpenBtn(t)}
            ${t.state === "done" || t.state === "dropped" ? "" : `<button type="button" class="td-done"${busy}>Done</button>`}
            ${t.state === "done" || t.state === "dropped" ? "" : `<button type="button" class="td-drop"${busy}>Drop</button>`}
          </div>${after}
        </div>`}
      </div>`;
  }
  if (asPanel) return `<div class="td-item td-${esc(t.kind)}${ans ? " answered" : ""} open" data-id="${escAttr(t.id)}">${panel}</div>`;
  return `<div class="td-item td-${esc(t.kind)}${ans ? " answered" : ""}${open ? " open" : ""}${todos.pend.busy(t.id) ? " sending" : ""}" data-id="${escAttr(t.id)}">
      <button type="button" class="td-row" id="${escAttr(rid)}" aria-expanded="${open}"${open && !inSplit ? ` aria-controls="${escAttr(pid)}"` : ""}>
        <span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span><span class="td-q">${renderInline(t.question.replace(/\s*\n\s*/g, " "), false)}</span>${!open && !ans && todos.nxt === t.id ? `<span class="td-nx">Next up</span>` : ""}
        <span class="td-rm"><span class="td-who">${esc(who)}</span><span class="td-age">${esc(age)}</span><span class="td-chips">${chips}</span></span>
        <span class="td-chev" aria-hidden="true"></span>
      </button>${panel}
    </div>`;
}

// ---- decision sets (todoset.go) ----
// One item with N decisions. The draft lives on the hub (t.set.draft) so the
// phone and the desktop agree; every change is saved at once, in order. The
// rules are evaluated here as the hub does (the hub has the last word on
// Submit). On a phone the set is a stepper with a review screen; on the
// desktop it is the whole list.
todos.setStep = new Map(); // item id -> index; D.length is the review screen
todos.setQ = new Map();    // item id -> promise chain of saves
todos.setBusy = new Set();
todos.setOther = new Set(); // "item:decision" whose "Other" box is open
todos.setText = new Map();  // "item:decision" or "item:#note" -> text typed and not yet saved

function setChoices(t) { return (t.set.draft && t.set.draft.choices) || {}; }
function setFacts(t) { return (t.set.draft && t.set.draft.facts) || {}; }
function setConflicts(t) {
  const ch = setChoices(t);
  return (t.set.rules || []).filter((r) => Object.entries(r.when).every(([d, o]) => (ch[d] || {}).option === o));
}
function setMissing(t) {
  const ch = setChoices(t);
  return t.set.decisions.filter((d) => !(ch[d.id] && (ch[d.id].option || ch[d.id].text) && ch[d.id].confirmed));
}
function setDone(t) { return t.set.decisions.length - setMissing(t).length; }
function setLocked(t) { return t.state !== "open" || !!t.set.submitted_at; }
function setBlockers(t) { return setConflicts(t).filter((r) => r.level === "block"); }

function setOptHtml(t, d, i) {
  const c = setChoices(t)[d.id], sel = c && c.option === i + 1, pend = sel && !c.confirmed, o = d.options[i];
  return `<button type="button" class="ds-opt${sel ? " on" : ""}${pend ? " pend" : ""}" role="radio" aria-checked="${!!sel}" data-d="${escAttr(d.id)}" data-o="${i + 1}"${setLocked(t) ? " disabled" : ""}>
      <span class="ds-dot" aria-hidden="true"></span><span class="ds-ot"><b>${esc(o.label)}</b>${o.detail ? `<small>${esc(o.detail)}</small>` : ""}</span>
      <span class="ds-oc">${i + 1 === d.recommend ? `<span class="td-chip st-answered">suggested</span>` : ""}${pend ? `<span class="td-chip td-draft">confirm</span>` : ""}</span></button>`;
}

// The "Other…" choice of a decision: the user's own words, which confirm the decision.
function setOtherHtml(t, d) {
  const c = setChoices(t)[d.id], key = t.id + ":" + d.id, txt = c && c.text, editing = todos.setOther.has(key), lock = setLocked(t);
  const val = todos.setText.has(key) ? todos.setText.get(key) : txt || "";
  return `<button type="button" class="ds-opt ds-other${txt ? " on" : ""}" role="radio" aria-checked="${!!txt}" data-d="${escAttr(d.id)}" data-other="1"${lock ? " disabled" : ""}>
      <span class="ds-dot" aria-hidden="true"></span><span class="ds-ot"><b>Other…</b><small>${txt ? esc(txt) : "Write your own answer"}</small></span><span class="ds-oc"></span></button>` +
    (editing && !lock ? `<div class="ds-obox"><textarea class="ds-text" rows="2" maxlength="500" data-d="${escAttr(d.id)}" placeholder="Your answer for ${escAttr(d.id)}" aria-label="Your own answer for ${escAttr(d.id)} ${escAttr(d.title)}">${esc(val)}</textarea>
      <button type="button" class="ds-use" data-d="${escAttr(d.id)}">${txt ? "Update answer" : "Use this answer"}</button></div>` : "");
}

function setDecHtml(t, d, compact) {
  const ch = setChoices(t)[d.id], rules = setConflicts(t).filter((r) => d.id in r.when);
  const bad = rules.some((r) => r.level === "block");
  const facts = (t.set.facts || []).filter((f) => (f.affects || []).includes(d.id) && !setFacts(t)[f.id]);
  const notes = rules.map((r) => `<div class="ds-note ${r.level}"><span><b>${r.level === "block" ? "Conflict" : "Heads up"}:</b> ${esc(r.say)}</span>${r.fix && !setLocked(t) ? `<button type="button" class="ds-fix" data-r="${escAttr(r.id)}">${esc(setFixLabel(t, r.fix))}</button>` : ""}</div>`).join("") +
    facts.map((f) => `<div class="ds-note fact"><span><b>Fact ${esc(f.id)} unknown.</b> ${esc(f.text)}</span></div>`).join("") +
    (ch && ch.option && !ch.confirmed && d.one_way ? `<div class="ds-note warn"><span>Suggested, not yet confirmed. A one-way door needs your explicit yes.</span><button type="button" class="ds-confirm" data-d="${escAttr(d.id)}"${setLocked(t) ? " disabled" : ""}>Confirm</button></div>` : "");
  return `<div class="ds-dec${bad ? " bad" : rules.length ? " warn" : ""}${ch && ch.confirmed ? " done" : ""}" data-dec="${escAttr(d.id)}">
      <h4><span class="ds-id">${esc(d.id)}</span>${esc(d.title)}</h4>
      <div class="ds-chips">${d.one_way ? `<span class="td-chip st-warn">one-way door</span>` : ""}${d.release_early ? `<span class="td-chip st-answered">unblocks now${(t.set.released || []).includes(d.id) ? " · sent" : ""}</span>` : ""}${(d.after || []).length ? `<span class="td-chip">after ${esc(d.after.join(", "))}</span>` : ""}${d.surface ? `<span class="td-chip st-surf">changes: ${esc(d.surface)}</span>` : ""}</div>
      ${!compact && d.question ? `<div class="ds-q">${esc(d.question)}</div>` : ""}
      <div class="ds-opts" role="radiogroup" aria-label="${escAttr(d.title)}">${d.options.map((_, i) => setOptHtml(t, d, i)).join("")}${setOtherHtml(t, d)}</div>${notes}</div>`;
}

function setFixLabel(t, fix) {
  const d = t.set.decisions.find((x) => x.id === fix.decision);
  return d ? `Set ${d.id} to “${d.options[fix.option - 1].short || d.options[fix.option - 1].label}”` : "Fix";
}

function setStatusHtml(t) {
  const n = setDone(t), N = t.set.decisions.length, bl = setBlockers(t).length, wn = setConflicts(t).length - bl;
  return `<span class="td-chip ${n === N ? "st-answered" : "st-open"}">${n}/${N} decided</span><span class="td-chip ${bl ? "st-err" : wn ? "st-warn" : "st-answered"}">${bl ? `${bl} blocking` : wn ? `${wn} warning${wn > 1 ? "s" : ""}` : "0 conflicts"}</span>` +
    `<span class="td-chip">${t.state !== "open" ? "submitted" : "draft saved on the hub"}</span>${t.set.plan && !t.set.submitted_at ? `<span class="td-chip st-surf" title="The plan's waiting handoffs start when you submit">plan waits on this</span>` : ""}`;
}

function setBarHtml(t) {
  const lock = setLocked(t), miss = setMissing(t), bl = setBlockers(t);
  const why = lock ? "" : miss.length ? `${miss.length} still to decide or confirm` : bl.length ? "Fix the blocking conflict first" : "";
  return `<div class="td-form ds-bar"><div class="ds-st">${setStatusHtml(t)}</div>
    <div class="td-acts"><button type="button" class="ds-all"${lock ? " disabled" : ""}>Accept all suggestions</button>
    <button type="button" class="ds-submit td-answer-btn"${lock || why ? " disabled" : ""}${why ? ` title="${escAttr(why)}"` : ""}>Submit to agent</button>
    ${todoOpenBtn(t)}<button type="button" class="td-done">Done</button><button type="button" class="td-drop">Drop</button></div>${why ? `<div class="td-after">${esc(why)}. Held-back one-way doors wait for you; warnings do not stop Submit.</div>` : ""}</div>`;
}

function setBodyHtml(t) {
  const S = t.set, risks = (S.risks || []).map((r) => `<div class="ds-risk"><b>${esc(r.id)}</b><span>${esc(r.text)}</span>${(r.decisions || []).map((x) => `<button type="button" class="ds-rk" data-go="${escAttr(x)}">${esc(x)}</button>`).join("")}</div>`).join("");
  const decided = (S.decided || []).length ? `<div class="ds-h">Already decided</div><ul class="ds-ul">${S.decided.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : "";
  const facts = (S.facts || []).length ? `<div class="ds-h">Facts nobody has yet</div>` + S.facts.map((f) => {
    const known = !!setFacts(t)[f.id];
    return `<div class="ds-fact${known ? " known" : ""}"><div><b>${esc(f.id)}</b> ${esc(f.text)} <span class="td-chip ${known ? "st-answered" : "st-warn"}">${known ? "known" : "unknown"}</span></div>
      ${f.test ? `<div class="ds-q">Cheapest test: ${esc(f.test)}</div>` : ""}<div class="ds-chips">${(f.affects || []).length ? `<span class="td-chip td-sug">affects ${esc(f.affects.join(", "))}</span>` : ""}<button type="button" class="ds-fk" data-f="${escAttr(f.id)}"${setLocked(t) ? " disabled" : ""}>${known ? "Mark unknown" : "Mark known"}</button></div></div>`;
  }).join("") : "";
  const step = todos.setStep.get(t.id) ?? 0, N = S.decisions.length;
  let decs;
  if (isDesktop()) decs = `<div class="ds-h">Decisions</div><div class="ds-list">${S.decisions.map((d) => setDecHtml(t, d, false)).join("")}</div>`;
  else if (step >= N) decs = `<div class="ds-dots">${setDotsHtml(t, step)}</div><div class="ds-h">Review</div><div class="ds-review">${S.decisions.map((d, i) => {
    const c = setChoices(t)[d.id];
    return `<button type="button" class="ds-rv" data-step="${i}"><b>${esc(d.id)}</b><span>${c && c.text ? "Other: " + esc(c.text) : c && c.option ? esc(d.options[c.option - 1].short || d.options[c.option - 1].label) + (c.confirmed ? "" : " (confirm)") : "<i>open</i>"}</span></button>`;
  }).join("")}</div>${setConflicts(t).map((r) => `<div class="ds-note ${r.level}"><span><b>${r.level === "block" ? "Conflict" : "Heads up"}:</b> ${esc(r.say)}</span></div>`).join("")}${setNoteHtml(t)}`;
  else decs = `<div class="ds-dots">${setDotsHtml(t, step)}</div>${setDecHtml(t, S.decisions[step], true)}
      <div class="ds-nav"><button type="button" class="ds-back" data-step="${step - 1}"${step ? "" : " disabled"}>Back</button><button type="button" class="ds-next" data-step="${step + 1}">${step === N - 1 ? "Review" : "Next"}</button></div>`;
  const wide = isDesktop() || step >= N;
  return `<div class="ds-set">${isDesktop() || step === 0 ? `${decided}${risks ? `<div class="ds-h">Biggest risks</div>${risks}` : ""}` : ""}${decs}${wide ? facts : ""}${isDesktop() ? setNoteHtml(t) : ""}</div>`;
}

// A free-text note for anything that is not one decision.
function setNoteHtml(t) {
  const key = t.id + ":#note", lock = setLocked(t), saved = (t.set.draft && t.set.draft.note) || "";
  const val = todos.setText.has(key) ? todos.setText.get(key) : saved;
  if (lock && !saved) return "";
  return `<div class="ds-h">Your note</div><textarea class="ds-text ds-notebox" rows="3" maxlength="2000" data-note="1" placeholder="Anything that is not one decision (optional)" aria-label="Your note on the whole set"${lock ? " disabled" : ""}>${esc(val)}</textarea>`;
}

function setDotsHtml(t, step) {
  const ch = setChoices(t);
  return t.set.decisions.map((d, i) => `<button type="button" aria-label="Go to ${escAttr(d.id)}" data-step="${i}" class="${i === step ? "cur" : ch[d.id] && ch[d.id].confirmed ? "ok" : ""}"></button>`).join("") +
    `<button type="button" aria-label="Review" data-step="${t.set.decisions.length}" class="${step >= t.set.decisions.length ? "cur" : ""}"></button>`;
}

// Saves the draft: the whole choices and facts, in order per item.
function setSave(t, body, quiet) {
  const q = (todos.setQ.get(t.id) || Promise.resolve()).then(async () => {
    try {
      const r = await api(`/api/todos/${encodeURIComponent(t.id)}/draft`, { method: "PUT", body: JSON.stringify(body) });
      const i = todos.items.findIndex((x) => x.id === t.id);
      if (i >= 0 && r.todo && !todos.setQ.get(t.id + ":next")) { todos.items[i] = r.todo; }
    } catch (e) {
      toast("Not saved: " + e.message, true);
      todos.loadedAt = 0; await loadTodos();
    }
    if (!quiet) renderTodos(true);
  });
  todos.setQ.set(t.id, q);
  return q;
}

// Applies fn to a copy of the draft, shows it now and saves it.
function setEdit(t, fn) {
  const ch = JSON.parse(JSON.stringify(setChoices(t))), fa = JSON.parse(JSON.stringify(setFacts(t)));
  fn(ch, fa);
  t.set.draft = { ...t.set.draft, choices: ch, facts: fa };
  renderTodos(true);
  return setSave(t, { choices: ch, facts: fa });
}

// Keeps the user's note at once (no redraw: they may be about to tap Submit).
function setSaveNote(t, text) {
  todos.setText.delete(t.id + ":#note");
  t.set.draft = { ...t.set.draft, note: text };
  return setSave(t, { note: text }, true);
}

function setAcceptAll(t) {
  setEdit(t, (ch) => { for (const d of t.set.decisions) if (!ch[d.id] && d.recommend) ch[d.id] = { option: d.recommend, confirmed: !d.one_way }; });
}

async function setSubmit(t) {
  if (todos.setBusy.has(t.id)) return;
  todos.setBusy.add(t.id);
  try {
    await (todos.setQ.get(t.id) || Promise.resolve());
    const note = rsJoin(t, "", RS_LIMIT.note);
    if (note === null) return;
    await api(`/api/todos/${encodeURIComponent(t.id)}/submit`, { method: "POST", body: JSON.stringify(note ? { note } : {}) });
    rsNotesSent(t);
    toast("Submitted. The coordinator will pass it on.");
  } catch (e) {
    toast("Not submitted: " + e.message, true);
  } finally {
    todos.setBusy.delete(t.id);
  }
  todos.loadedAt = 0; await loadTodos();
}

// Clicks inside a set. Returns true when it handled one.
function setClick(ev, t) {
  const el = ev.target;
  const opt = el.closest(".ds-opt");
  if (opt && opt.dataset.other) {
    const key = t.id + ":" + opt.dataset.d;
    if (!opt.disabled) {
      if (todos.setOther.has(key) && !(setChoices(t)[opt.dataset.d] || {}).text) todos.setOther.delete(key); else todos.setOther.add(key);
      renderTodos(true);
      const box = el.closest(".ds-dec") && el.closest(".ds-dec").querySelector(".ds-text");
      if (box) box.focus();
    }
    return true;
  }
  if (opt) {
    if (!opt.disabled) { todos.setOther.delete(t.id + ":" + opt.dataset.d); todos.setText.delete(t.id + ":" + opt.dataset.d); setEdit(t, (ch) => { ch[opt.dataset.d] = { option: Number(opt.dataset.o), confirmed: true }; }); }
    return true;
  }
  const use = el.closest(".ds-use");
  if (use) {
    const box = use.parentElement.querySelector(".ds-text"), text = box ? box.value.trim() : "";
    const key = t.id + ":" + use.dataset.d;
    if (text) { todos.setOther.delete(key); todos.setText.delete(key); setEdit(t, (ch) => { ch[use.dataset.d] = { text, confirmed: true }; }); }
    return true;
  }
  const cf = el.closest(".ds-confirm");
  if (cf) { setEdit(t, (ch) => { if (ch[cf.dataset.d]) ch[cf.dataset.d].confirmed = true; }); return true; }
  const fix = el.closest(".ds-fix");
  if (fix) {
    const r = t.set.rules.find((x) => x.id === fix.dataset.r);
    if (r && r.fix) setEdit(t, (ch) => { ch[r.fix.decision] = { option: r.fix.option, confirmed: true }; });
    return true;
  }
  const fk = el.closest(".ds-fk");
  if (fk) { setEdit(t, (ch, fa) => { if (fa[fk.dataset.f]) delete fa[fk.dataset.f]; else fa[fk.dataset.f] = true; }); return true; }
  if (el.closest(".ds-all")) { setAcceptAll(t); return true; }
  if (el.closest(".ds-submit")) { setSubmit(t); return true; }
  const st = el.closest("[data-step]");
  if (st) { todos.setStep.set(t.id, Math.max(0, Math.min(t.set.decisions.length, Number(st.dataset.step)))); renderTodos(true); return true; }
  const go = el.closest(".ds-rk");
  if (go) {
    const i = t.set.decisions.findIndex((d) => d.id === go.dataset.go);
    if (!isDesktop() && i >= 0) { todos.setStep.set(t.id, i); renderTodos(true); return true; }
    const card = el.closest(".td-item").querySelector(`.ds-dec[data-dec="${CSS.escape(go.dataset.go)}"]`);
    if (card) {
      card.classList.add("flash");
      card.scrollIntoView?.({ block: "nearest" });
      setTimeout(() => card.classList.remove("flash"), 1400);
    }
    return true;
  }
  return false;
}

function todoListHtml(pfx) {
  todos.nxt = todoNextOpen(todos.openId, todoOrder().map((t) => t.id));
  const opens = todos.items.filter((t) => t.state === "open");
  const done = todos.items.filter((t) => t.state !== "open");
  let h = todos.notice ? `<div class="td-notice" role="status"><span>${esc(todos.notice.text)}</span><button type="button" class="td-notice-x btn sm">Back to the list</button></div>` : "";
  h += todoOrphans().map(([id, d]) => todoKeptHtml(id, d.text, `Not sent: “${d.q || id}” was closed before you answered.`)).join("");
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
      const mine = guItems.has(id); // closed by this page's own Face ID: no "elsewhere" notice
      if (!mine && (todos.follow || (isDesktop() && state.view === "todo"))) {
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
  const moved = todoFollowRefresh();
  // A forced redraw, or one that moves to another item, goes ahead even while the user types.
  const again = !!force || moved || todos.follow;
  const show = todos.on === true && (todos.items.length > 0 || todoOrphans().length > 0 || !!todos.notice);
  if (main) main.hidden = !show;
  if (!show && main) { main.innerHTML = ""; delete main.dataset.sig; }
  const folded = lsGet(TODO_FOLDED_KEY) === "1";
  const sig = JSON.stringify([folded, todos.openId, todos.ansOpen, todos.pend.busyKeys(), [...todos.drafts.keys()], [...gu].map(([k, v]) => [k, v.st, v.msg, apBusy.has(k)]),
    todos.items.map((t) => [t.id, t.state, t.updated_at, t.question, t.approval && t.approval.state, t.approval && t.approval.id, t.review && t.review.stale, t.review && t.review.route && t.review.route.state, t.page && (t.page.thumbs || []).length]), [...todos.rv], [...todos.rvBusy], pgsScheme(), rls.loadedAt]);
  // The age moves between renders; a typed answer must not be lost to one.
  const typing = (el) => el && ((el.contains(document.activeElement) && (document.activeElement.classList.contains("td-text") || document.activeElement.classList.contains("ds-text"))) || [...el.querySelectorAll(".ap-pass")].some((p) => p.value));
  if (show && main && (again || (main.dataset.sig !== sig && !typing(main)))) {
    main.dataset.sig = sig;
    todoPaint(main, `<div class="td-head"><button type="button" class="td-toggle" aria-expanded="${!folded}">
        <span class="proj-chev" aria-hidden="true"></span><span class="td-title">To do</span><span class="td-count">${esc(todoCountText())}</span></button></div>
      <div class="td-list"${folded ? " hidden" : ""}>${todoListHtml("tdc")}</div>`);
  }
  renderNav();
  renderStatus();
  syncRail();
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
  renderSplit(!!force);
  todos.follow = false;
}

// The list the user sees: the To do page on desktop, the card on a phone.
function todoListEl() { return isDesktop() ? $("pt-body") : $("todo"); }

// Opens one item (closing the one that was open) and focuses its row, or
// its answer box with to = "box".
function todoExpand(id, to) {
  if (todos.deepId && todos.deepId !== id) todoDeepClean();
  todos.notice = null;
  todos.openId = id;
  if (id) todos.lastId = id;
  renderTodos(true);
  if (rsActive()) {
    // The item is in the split view: its answer box, or the Back button.
    const box = to === "box" && rs.el.querySelector(".td-text");
    const f = box || rs.el.querySelector(".rs-back");
    if (f) f.focus({ preventScroll: true });
    return;
  }
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
  if (rsActive() && history.state && history.state.rs) { history.back(); return; } // popstate closes it, see syncSplit
  if (todos.openId) todos.lastId = todos.openId;
  if (todos.deepId) todoDeepClean();
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
      toast(what === "answer" ? (prev.item.dialog ? "Sent to the agent's dialog." : "Answered. The coordinator will pass it on.") : what === "done" ? "Marked done" : what === "dismiss" ? "Dismissed" : "Dropped");
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

// todoKeep makes a suggestion a real item: it stays, the "suggested" marks go.
async function todoKeep(id) {
  const t = todos.items.find((x) => x.id === id);
  if (!t || !t.suggested || todos.pend.busy(id)) return;
  t.suggested = false;
  renderTodos(true);
  try {
    await api(`/api/todos/${encodeURIComponent(id)}/keep`, { method: "POST", body: "{}" });
  } catch (e) {
    t.suggested = true;
    renderTodos(true);
    toast("Not kept: " + e.message, true);
    return;
  }
  todos.loadedAt = 0;
  await loadTodos();
}

// reviewNote opens the Needs changes note (a second press closes it).
function reviewNote(id, item) {
  if (todos.rv.has(id)) { todos.rv.delete(id); renderTodos(true); return; }
  todos.rv.set(id, { kind: "fix" });
  renderTodos(true);
  const box = document.querySelector(`.td-item[data-id="${CSS.escape(id)}"] .rv-text`);
  if (box) box.focus();
}

function todoAnswer(id, box) {
  const it = box && box.closest(".td-item"), rt = it && todos.items.find((x) => x.id === it.dataset.id);
  if (rt && todoIsReview(rt)) { reviewVerdict(rt.id, "changes", it); return; }
  const t = todos.items.find((x) => x.id === id);
  const text = rsJoin(t, box ? box.value.trim() : "", RS_LIMIT.answer);
  if (text === null) return;
  if (!text) { toast("Write your answer first", true); return; }
  todoAct(id, "answer", { text }); // on a failed send the text comes back in the box, the notes in it
  rsNotesSent(t);
}

function todoCopy(ta) {
  copyText(ta.value).then((ok) => {
    if (ok) { toast("Copied"); return; }
    ta.focus(); ta.select(); toast("Selected: copy it with your keyboard");
  });
}

// ---- releases ----
// A "release" to-do carries a version and a full commit id, and is only a
// one-line ping now: its "Open in Peers" button goes to the Releases tab on
// the Peers screen (#/peers/~releases), which holds everything else: the
// release card, the fleet table with its Bump hub and Upgrade buttons, and the
// history. The tab is fed by GET /api/releases. It shows a tagged release
// until every host runs it, even if its item was closed, and the hub closes
// the item itself when they do. Everything the tab shows about a release
// (the commit, CI, the changes) comes from the server's own check of the hub's
// checkout and GitHub, never from the item's text. Approving asks for a
// confirm step that names the version and the commit; each host's Upgrade runs
// `comebye upgrade` there. Agents cannot do either: only this page can.
const RELEASES_HASH = "#/peers/~releases";
const PRICES_HASH = "#/peers/~prices";
const rls = { data: null, err: "", loadedAt: 0, inflight: false, confirm: null, busy: false };

async function loadReleases(refresh) {
  if (rls.inflight) return;
  rls.inflight = true;
  try {
    rls.data = await api(`/api/releases${refresh ? "?refresh=1" : ""}`, { timeout: 120000 });
    rls.err = "";
  } catch (e) {
    rls.err = e.message || "failed";
  } finally {
    rls.inflight = false;
    rls.loadedAt = Date.now();
  }
  // The hub may have closed the item itself: bring the To do list along.
  const d = rls.data, mine = d && d.todo && todos.items.find((x) => x.id === d.todo.id);
  if (mine && mine.state !== d.todo.state) { todos.loadedAt = 0; loadTodos(); }
  renderPeers();
  renderReleaseDots();
}

// Does a release need the user? Drives the dots on the Peers entries.
function relNeedsYou() { return !!(rls.data && rls.data.needs_you); }

// The orange dot on the Peers entry of the navigation and of the fleet menu,
// so a release that needs the user shows wherever they are.
function renderReleaseDots() {
  renderNav();
  const f = $("f-peers");
  if (!f) return;
  const need = relNeedsYou();
  let d = f.querySelector(".nv-dot");
  if (need && !d) { d = document.createElement("span"); d.className = "nv-dot"; f.append(d); }
  if (d) { d.hidden = !need; d.setAttribute("aria-label", need ? "a release needs you: " + (rls.data.needs_text || "") : ""); }
}

// Outside Peers nobody loads the releases, so look now and then (a hub with
// peers only: that is where releases are managed).
function relBackground() {
  if (document.hidden || ledger.on !== true || !multiHost() || (prs.route && prs.route.tab === "releases")) return;
  if (Date.now() - rls.loadedAt > 55000) loadReleases(false);
}

// While a tag, a build or an upgrade is moving, refresh the tab often; now
// and then otherwise.
function relActive(d) {
  if (!d || !d.current || !d.todo.release) return false;
  const st = d.todo.release.state;
  if (st === "tagging") return true;
  if (st !== "tagged") return false;
  if (d.build && d.build.state !== "success" && d.build.state !== "failure") return true;
  if (d.deploy && d.deploy.record && d.deploy.record.state === "running") return true;
  if (roState(d) === "running") return true;
  return (d.hosts || []).some((h) => h.record && h.record.state === "running");
}

function relTick() {
  if (document.hidden || !prs.route || prs.route.tab !== "releases") return;
  const age = Date.now() - rls.loadedAt;
  if (!rls.data && !rls.err ? age > pollMs(1500) : age > pollMs(relActive(rls.data) ? 4000 : 30000)) loadReleases(false);
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
    case "ok": return `${rc.rollback ? "rolled back" : "upgraded"} to ${rc.to || rc.version} by ${relBy(rc.by) || "?"}${ago}`;
    case "rolled_back": return `${verb} failed its checks and was rolled back${ago}: ${rc.error || ""}`;
    case "dry_run": return `dry run ${rc.version}: nothing installed${ago}`;
    default: return `${verb} failed${ago}: ${rc.error || ""}`;
  }
}

// "me (hub says: by me)" is one person said twice: say it once. A different
// name on the hub's side stays, as "me (hub: them)".
function relBy(by) {
  const m = /^(.*?) \(hub says: (?:by )?(.*)\)$/.exec(String(by || ""));
  if (!m) return String(by || "");
  const a = m[1].trim(), b = m[2].trim();
  return !b || b.toLowerCase() === a.toLowerCase() ? a : `${a} (hub: ${b})`;
}

function relDeployText(dp, version) {
  const rc = dp.record;
  if (!rc || rc.version !== version) return "";
  const when = Date.parse(rc.finished || rc.started);
  const ago = Number.isNaN(when) ? "" : " " + fmtAgo(when) + " ago";
  const by = rc.by ? " by " + relBy(rc.by) : "";
  switch (rc.state) {
    case "running": return `${rc.version} running${by}…`;
    case "ok": return `${rc.version} done${by}${ago}`;
    case "dry_run": return `dry run ${rc.version}: nothing was run${ago}`;
    default: return `${rc.version} failed${ago}`;
  }
}

// The deploy's own words: the headline (FAILED phase=… reason=…, else the
// error; the latest line while it runs; the ok line when it passed) and, for a
// failure, the last lines of output in a monospace block. Plain text only: the
// output is an operator command's, never HTML. A host that predates "headline"
// gets one made here from "output" and "error".
function relDeployOut(rc, label) {
  if (!rc || rc.state === "dry_run") return "";
  const lines = String(rc.output || "").split("\n").map((l) => l.trimEnd()).filter((l) => l.trim());
  let head = rc.headline || "";
  if (!head) {
    const find = (sub) => [...lines].reverse().find((l) => l.includes(sub)) || "";
    head = rc.state === "failed" ? find("FAILED") || rc.error || "" : rc.state === "ok" ? find("bump: ok") || lines[lines.length - 1] || "" : lines[lines.length - 1] || "";
  }
  head = head.slice(0, 300);
  if (!head && !lines.length) return "";
  const cls = rc.state === "failed" ? "bad" : rc.state === "ok" ? "ok" : "run";
  let h = `<div class="rel-depout rel-depout-${cls}" data-deploy-out>`;
  if (head) h += `<div class="rel-dephead" role="${rc.state === "failed" ? "alert" : "status"}">${esc(head)}</div>`;
  if (lines.length) {
    const nm = esc(label.toLowerCase());
    if (rc.state === "failed") {
      const tail = lines.slice(-15), more = lines.length > tail.length;
      h += cpOut(".rel-deptail", "Copy", "Copy the output") + `<pre class="rel-deptail">${esc(tail.join("\n"))}</pre>`;
      if (more) h += `<details class="rel-changes"><summary>Show all ${nm} output (${lines.length} lines)</summary>${cpOut("pre", "Copy", "Copy the output")}<pre>${esc(lines.join("\n"))}</pre></details>`;
    } else if (lines.length > 1 || lines[0] !== head) {
      h += `<details class="rel-changes"><summary>Show ${nm} output</summary>${cpOut("pre", "Copy", "Copy the output")}<pre>${esc(lines.join("\n"))}</pre></details>`;
    }
  }
  return h + `</div>`;
}

// "3 of 4, 1 offline": how far a rollout has got. An offline host that has
// not upgraded is counted apart and holds the release open.
function relCount(ro) {
  return ro ? `${ro.current} of ${ro.total}${ro.offline ? `, ${ro.offline} offline` : ""}` : "";
}

function relWhen(iso) {
  const at = Date.parse(iso);
  return Number.isNaN(at) ? "" : fmtAgo(at) + " ago";
}

// The to-do item: one line and a way to the real thing.
function relPingHtml(t) {
  const rl = t.release, v = esc(rl.version);
  const d = rls.data, live = d && d.todo && d.todo.id === t.id && d.needs_text ? d.needs_text : "";
  const TEXT = {
    pending: `Release ${v} is proposed: approve it in Peers.`,
    tagging: `Tagging ${v}…`,
    failed: `Tagging ${v} failed: see Peers.`,
    tagged: `${v} is tagged: roll it out from Peers. This item closes by itself once the hub and every host run it.`,
  };
  const txt = t.state === "done" || t.state === "dropped" ? `${v} (${esc(t.state)})` : live ? esc(live) : rl.rollout_note ? esc(rl.rollout_note) : TEXT[rl.state] || v;
  return `<div class="rel rel-ping" data-rel-id="${escAttr(t.id)}">
    <div class="rel-pingrow"><span class="rel-pingtxt">${txt}</span>
    <a class="rel-open" href="${RELEASES_HASH}">Open in Peers</a></div></div>`;
}

function relDeployConfirmHtml(d) {
  const c = rls.confirm, rl = d.todo.release, dp = d.deploy;
  if (!c || c.k !== "deploy" || !dp) return "";
  const label = dp.label || "Deploy";
  return `<div class="rel-confirm" role="group" aria-label="Confirm the deploy"><div>Run <code>${esc(dp.command || "the deploy command")}</code> on ${esc(d.release_host || "the release host")} for <b>${esc(rl.version)}</b>${dp.dry_run ? " (dry run: nothing will run)" : ""}? This is a separate step from the release: it changes what is running.</div>
    <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-deploy"${rls.busy ? " disabled" : ""}>Confirm: ${esc(label.toLowerCase())} ${esc(rl.version)}</button>
    <button type="button" data-rel="cancel"${rls.busy ? " disabled" : ""}>Cancel</button></div></div>`;
}


// ---- the rolling rollout (Roll out to all) ----
// The job is the hub's (release/rollout.json); the page only shows it and
// sends Pause, Resume and Cancel. d.rollout_job is this release's job;
// d.rollout_plan the order the confirm offers.
function roJob(d) { return d && d.todo && d.rollout_job && d.rollout_job.todo === d.todo.id ? d.rollout_job : null; }
function roLive(j) { return !!j && (j.state === "running" || j.state === "paused" || j.state === "stopped"); }
function roState(d) { const j = roJob(d); return j ? j.state : ""; }
const RO_WAVE = { canary: "Canary", peers: "Other peers", hub: "Hub" };
const roWaveName = (w, plan) => (w === "peers" && !(plan || []).some((st) => st.wave === "canary") ? "Peers" : RO_WAVE[w]);
const RO_PHASE = { pending: "waiting", waking: "starting", upgrading: "upgrading", verifying: "verifying", soaking: "soaking", ok: "done", skipped: "skipped", failed: "failed" };

// What a host's step says beside its version.
function roStepText(st, job) {
  switch (st.phase) {
    case "pending": return st.detail || (job.state === "stopped" ? "not touched" : "waiting for its turn");
    case "waking": return st.detail || "starting " + st.host + "…";
    case "upgrading": return (st.mode === "deploy" ? "running the deploy; it restarts the hub" : "upgrading") + (st.detail ? " · " + st.detail : "…");
    case "verifying": return (st.mode === "deploy" ? "deploy ok · " : "upgrade ok · ") + (st.detail || "waiting for it to be back");
    case "soaking": return st.detail || "soaking";
    case "ok": return (st.detail || "") + (st.woke ? (st.detail ? " · " : "") + "woke it" : "");
    case "skipped": return "skipped · " + (st.detail || "");
    default: return st.detail || "failed";
  }
}

// The three gates of a host: its own upgrade finished ok, the hub sees it
// back online, the version is the target (canary: and the soak passed).
function roGatesHtml(st, version) {
  const past = (...ph) => ph.includes(st.phase);
  const g = (label, done, now) => `<span class="rl-gate ${done ? "rl-gate-ok" : now ? "rl-gate-now" : ""}">${done ? "✓" : now ? "…" : "○"} ${esc(label)}</span>`;
  const dep = st.mode === "deploy";
  const gates = [
    g(dep ? "deploy ok" : "upgrade ok", past("verifying", "soaking", "ok"), st.phase === "upgrading"),
    g(dep ? "hub back" : "back online", past("soaking", "ok"), st.phase === "verifying"),
    g(version + " seen", past("soaking", "ok"), st.phase === "verifying"),
  ];
  if (st.woke || st.phase === "waking") gates.unshift(g("started", st.phase !== "waking" && st.phase !== "pending", st.phase === "waking"));
  if (st.wave === "canary") gates.push(g("soak", st.phase === "ok", st.phase === "soaking"));
  return `<div class="rl-gates" aria-label="Gates of ${escAttr(st.host)}">${gates.join("")}</div>`;
}

function roBandHtml(d, job) {
  const cur = d.todo.release, hosts = job.plan || [];
  const doneN = hosts.filter((x) => x.phase === "ok").length;
  const skipped = hosts.filter((x) => x.phase === "skipped");
  const busy = rls.busy ? " disabled" : "";
  const b = (act, label, cls = "") => `<button type="button" class="${cls}" data-rel="${act}"${busy}>${esc(label)}</button>`;
  let title = "", acts = "", cls = job.state, role = "status";
  switch (job.state) {
    case "running":
      title = `Rolling out ${cur.version} · ${doneN} of ${hosts.length} done`;
      acts = job.pause_requested ? b("ro-cancel", "Cancel") : b("ro-pause", "Pause") + b("ro-cancel", "Cancel");
      break;
    case "paused":
      title = `Paused · ${doneN} of ${hosts.length} done`;
      acts = b("ro-resume", "Resume", "rel-go") + b("ro-cancel", "Cancel");
      break;
    case "stopped": {
      const left = hosts.filter((x) => x.phase === "pending").map((x) => x.host);
      title = `Stopped at ${job.stop ? job.stop.host : "a host"}`;
      acts = b("ro-resume", "Retry", "rel-go") + b("ro-cancel", "Cancel");
      cls = "stopped"; role = "alert";
      return `<div class="rl-band rl-band-${cls}" role="${role}" data-rollout="${escAttr(job.state)}"><div class="rl-band-t">${esc(title)}</div>
        <div class="rl-band-s">${esc(job.stop ? job.stop.reason : "")}</div>
        <div class="rl-band-s">${left.length ? "Not touched: " + esc(left.join(", ")) + "." : "Nothing else was left."} Hosts already done stay on ${esc(cur.version)}.</div>
        <div class="td-acts">${acts}</div></div>`;
    }
    case "done":
      title = skipped.length ? `Rolled out · ${doneN} of ${hosts.length} hosts, ${skipped.length} skipped (${skipped.map((x) => x.host).join(", ")})` : `Rolled out · ${hosts.length} of ${hosts.length} hosts`;
      break;
    default:
      title = "Rollout cancelled · hosts keep the version they have";
  }
  return `<div class="rl-band rl-band-${cls}" role="${role}" data-rollout="${escAttr(job.state)}"><div class="rl-band-t">${esc(title)}</div>
    ${job.state === "running" || job.state === "paused" ? `<div class="rl-band-s">${esc(job.note || "")}</div>` : ""}
    ${acts ? `<div class="td-acts">${acts}</div>` : ""}</div>`;
}

// What the confirm says of a host that is not up: a stopped worker the rollout
// will start (and about how long it then runs), or "offline" with the reason
// when it is stopped but will not be started.
function rlPreviewTag(h) {
  if (h.stopped) return ` <span class="rl-tag rl-tag-stop" data-stopped>stopped, will be started (about ${h.wake_min || 4} min of runtime)</span>`;
  if (!h.offline) return "";
  return ` <span class="rl-tag rl-tag-off">offline</span>${h.wake_note ? ` <span class="rl-note">${esc(h.wake_note)}</span>` : ""}`;
}

// The confirm: version, commit, build, the ordered hosts with what they run,
// who goes first. The click repeats all of it to the server.
function roConfirmHtml(d) {
  const c = rls.confirm;
  if (!c || c.k !== "rollout") return "";
  const rl = d.todo.release, by = new Map((d.hosts || []).map((h) => [h.name, h]));
  const order = c.order || [], hubStep = c.hub ? (d.rollout_plan || []).find((st) => st.wave === "hub") : null, hubH = hubStep ? by.get(hubStep.host) || {} : null;
  // A canary is the opt-in (-release-canary): the server's plan says whether there is one.
  const canary = ((d.rollout_plan || []).find((st) => st.wave === "canary") || {}).host || "";
  const total = order.length + (hubStep ? 1 : 0), hubLabel = d.rollout_hub_label || "Bump hub";
  const li = order.map((n, i) => {
    const h = by.get(n) || {};
    const role = i === 0 && canary === n ? ' <span class="rl-tag">canary</span>' : "";
    const off = rlPreviewTag(h);
    return `<li data-host="${escAttr(n)}"><b>${esc(n)}</b> <span class="rl-ver rl-v-old">${esc(h.version || "?")}</span> → <span class="rl-to"><b>${esc(rl.version)}</b></span>${role}${off}</li>`;
  }).join("");
  const hubLi = hubStep ? `<li class="rl-hubli" data-host="${escAttr(hubStep.host)}" data-wave="hub"><b>${esc(hubStep.host)}</b> <span class="rl-tag">hub</span> <span class="rl-ver rl-v-old">${esc(hubH.version || "?")}</span> → <span class="rl-to"><b>${esc(rl.version)}</b></span><div class="rl-sub">${hubStep.mode === "deploy" ? `last: <b>${esc(hubLabel)}</b>${d.release_host && d.release_host !== hubStep.host ? ` runs on ${esc(d.release_host)}` : ""}. This changes what is running in the cluster and restarts the hub; it has no automatic undo.` : "last: the hub upgrades itself and restarts."}</div></li>` : "";
  const soak = canary ? ` ${esc(canary)} goes first, alone, and soaks for 2 minutes; the rest follow one at a time only if it stays healthy.` : order.length > 1 ? " The peers go one at a time." : "";
  return `<div class="rel-confirm" role="group" aria-label="Confirm the rollout"><div>Roll out <b>${esc(rl.version)}</b> (<code>${esc(relShort(rl.commit))}</code>, the build passed) to ${total} host${total === 1 ? "" : "s"}${hubStep ? ", the hub last" : ""}, in this order?${soak} The first failure stops everything and leaves the rest alone; a host that fails rolls itself back. Offline hosts are skipped and can be rolled out again later; a stopped worker is started first.</div>
    <ol class="rl-order">${li}${hubLi}</ol>
    <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-rollout"${rls.busy ? " disabled" : ""}>Confirm: roll out ${esc(rl.version)} to ${total} host${total === 1 ? "" : "s"}</button>
    <button type="button" data-rel="cancel"${rls.busy ? " disabled" : ""}>Cancel</button></div></div>`;
}

// Roll out to all / Roll out again: next to the card's other buttons.
function roButtonHtml(d, green) {
  const job = roJob(d);
  if (!d.rollout_enabled || !green || roLive(job) || !d.rollout_plan || !d.rollout_plan.length || (d.rollout && d.rollout.complete)) return "";
  const again = !!job && job.state === "done" && (job.plan || []).some((x) => x.phase === "skipped");
  const label = again ? "Roll out again" : "Roll out to all";
  return `<div class="rl-roll"><button type="button" class="rel-up rel-roll" data-rel="${again ? "rollout-again" : "rollout"}"${rls.busy || rls.confirm ? " disabled" : ""}>${label}</button></div>`;
}

// One row of the fleet table.
function relRowHtml(x, d, job, step) {
  const cur = d.current ? d.todo.release : null;
  const tagged = !!cur && cur.state === "tagged";
  const green = !!(tagged && d.build && d.build.state === "success");
  const dp = tagged && d.deploy && d.deploy.enabled ? d.deploy : null;
  // The deploy is the hub's upgrade, though its command runs on the release
  // host: it belongs on the hub's row. The release host's own row is an
  // ordinary host with its own Upgrade (unless it is the hub, as on a
  // single-host setup). Without a hub row, the release host's row carries it.
  const hubName = ((d.hosts || []).find((h) => h.hub) || {}).name || d.release_host;
  const isDeploy = !!dp && x.name === hubName;
  const deployRuns = isDeploy && d.release_host && d.release_host !== x.name ? `runs on ${d.release_host}` : "";
  const rc = x.record && cur && x.record.version === cur.version ? x.record : null;
  const drec = isDeploy && dp.record && dp.record.version === cur.version ? dp.record : null;
  const running = !!(x.record && x.record.state === "running") || !!(isDeploy && dp.record && dp.record.state === "running");
  const locked = roLive(job);
  const btn = (act, label, cls) => `<button type="button" class="${cls}" data-rel="${act}" data-host="${escAttr(x.name)}"${rls.busy || locked ? " disabled" : ""}>${esc(label)}</button>`;
  let act = "", sub = "", err = false, out = "", rowcls = "";
  if (tagged) {
    if (x.current) {
      rowcls = " rl-h-cur";
      act = `<span class="rl-ok">✓ ${esc(x.version)}</span>`;
      if (x.enabled && green && !running && !isDeploy && rc && rc.state === "ok" && !rc.rollback) act += " " + btn("rollback", "Roll back", "rel-rb");
      // An earlier failure for this version is history once the host runs it
      // (it stays in the Record): only a success or a running job is status.
      if (isDeploy && drec) { if (drec.state === "ok" || drec.state === "running") { sub = relDeployText(dp, cur.version); out = relDeployOut(drec, dp.label || "Deploy"); } }
      else if (rc && (rc.state === "ok" || rc.state === "running")) sub = relRecordText(x);
    } else if (x.offline) {
      rowcls = " rl-h-off";
      act = `<button type="button" class="rel-up" disabled>Upgrade</button>`;
      sub = `not reachable${x.since ? " since " + relWhen(x.since) : ""}; upgrade when it is back`;
    } else if (running) {
      sub = isDeploy && dp.record && dp.record.state === "running" ? relDeployText(dp, dp.record.version) : relRecordText(x);
    } else if (isDeploy) {
      const failed = !!drec && drec.state === "failed";
      if (failed) { rowcls = " rl-h-fail"; err = true; }
      if (green && !(drec && drec.state === "ok")) act = `<button type="button" class="rel-dep-btn" data-rel="deploy"${rls.busy || locked ? " disabled" : ""}>${esc(failed ? "Retry" : dp.label || "Deploy")}</button>`;
      sub = drec ? relDeployText(dp, cur.version) : dp.record && dp.record.state === "running" ? `another deploy (${dp.record.version}) is running…` : "";
      out = relDeployOut(drec, dp.label || "Deploy");
    } else if (!x.enabled) {
      sub = relRecordText(x);
    } else {
      const failed = !!rc && (rc.state === "failed" || rc.state === "rolled_back");
      if (failed) { rowcls = " rl-h-fail"; err = true; }
      if (green) act = btn("up", failed ? "Retry" : "Upgrade", "rel-up");
      sub = rc ? relRecordText(x) : "";
    }
  } else if (x.offline) {
    rowcls = " rl-h-off";
  }
  let gates = "";
  if (step) {
    sub = roStepText(step, job);
    err = step.phase === "failed";
    if (err) { rowcls = " rl-h-fail"; if (step.output) out = `<details class="rel-changes"><summary>Show the host's output</summary>${cpOut("pre", "Copy", "Copy the output")}<pre>${esc(step.output)}</pre></details>`; }
    rowcls += ` rl-st-${step.phase}`;
    gates = roGatesHtml(step, cur.version);
    if (step.phase !== "ok") act = act.replace(/<button[^>]*rel-(up|rb)[^>]*>[^<]*<\/button>/g, "");
  }
  const vcls = x.offline && !x.current ? "off" : !cur || x.current || !tagged ? "cur" : "old";
  const to = tagged && !x.current ? ` <span class="rl-to">→ <b>${esc(cur.version)}</b></span>` : "";
  return `<div class="rl-host rel-host${rowcls}" data-host="${escAttr(x.name)}"${isDeploy ? " data-deploy" : ""}>
    <div class="rl-hl"><span class="rel-hn">${esc(x.name)}</span>${x.hub ? '<span class="rl-tag">hub</span>' : ""}${x.stopped && !step ? '<span class="rl-tag rl-tag-stop">stopped</span>' : x.offline && !(step && step.phase === "waking") ? '<span class="rl-tag rl-tag-off">offline</span>' : ""}<span class="rl-ver rl-v-${vcls}">${esc(x.version || "?")}</span>${to}${step ? `<span class="rl-phase rl-ph-${escAttr(step.phase)}">${esc(RO_PHASE[step.phase] || step.phase)}</span>` : ""}</div>
    ${act ? `<div class="rel-ha">${act}</div>` : ""}
    ${sub ? `<div class="rl-sub rel-hs${err ? " rl-err" : ""}">${esc(sub)}</div>` : ""}${gates}${deployRuns && (act || drec) ? `<div class="rl-sub rel-runs">${esc(deployRuns)}</div>` : ""}${out}</div>`;
}

function relFleetHtml(d) {
  const cur = d.current ? d.todo.release : null;
  const tagged = !!cur && cur.state === "tagged";
  const ro = d.rollout, hosts = d.hosts || [];
  const green = !!(tagged && d.build && d.build.state === "success");
  let h = `<div class="rl-card" data-fleet><h3>Fleet${tagged && ro ? ` · <span class="rl-count">${esc(relCount(ro))}</span>` : ""}</h3>`;
  if (tagged && ro) {
    const pct = ro.total ? Math.round((ro.current * 100) / ro.total) : 0;
    h += `<div class="rl-prog" role="progressbar" aria-label="Hosts on ${escAttr(cur.version)}: ${escAttr(relCount(ro))}" aria-valuemin="0" aria-valuemax="${ro.total}" aria-valuenow="${ro.current}"><i style="width:${pct}%"></i></div>`;
    if (ro.complete) h += `<div class="rl-done" role="status"><b>All upgraded</b> · ${ro.total} of ${ro.total} on ${esc(cur.version)}</div>`;
    else if (!green) h += `<div class="rel-note">Hosts can be upgraded once the release build has passed.</div>`;
  }
  const job = tagged ? roJob(d) : null;
  const showJob = !!job && (roLive(job) || (job.state === "done" && !(ro && ro.complete)) || job.state === "cancelled");
  if (tagged) h += roButtonHtml(d, green);
  if (tagged && showJob) h += roBandHtml(d, job);
  if (tagged && d.rollout_plan_error && d.rollout_enabled && green && !roLive(job) && !(ro && ro.complete)) h += `<div class="rel-note" data-rollout-plan-error>Roll out to all is not available: ${esc(d.rollout_plan_error)}</div>`;
  if (!hosts.length) h += `<div class="rel-note">${rls.data ? "No hosts." : "Loading…"}</div>`;
  if (tagged && roLive(job)) {
    // Waves group the rows: canary, the other peers, the hub; what the rollout does not touch comes last.
    const byName = new Map(hosts.map((x) => [x.name, x]));
    const inPlan = new Set((job.plan || []).map((st) => st.host));
    for (const w of ["canary", "peers", "hub"]) {
      const steps = (job.plan || []).filter((st) => st.wave === w);
      if (!steps.length) continue;
      h += `<div class="rl-wave" data-wave="${w}"><div class="rl-wave-h">${roWaveName(w, job.plan)}</div><div class="rl-hosts">${steps.map((st) => byName.has(st.host) ? relRowHtml(byName.get(st.host), d, job, st) : "").join("")}</div></div>`;
    }
    const rest = hosts.filter((x) => !inPlan.has(x.name));
    if (rest.length) h += `<div class="rl-wave" data-wave="rest"><div class="rl-wave-h">Not in this rollout</div><div class="rl-hosts">${rest.map((x) => relRowHtml(x, d, job)).join("")}</div></div>`;
  } else {
    h += `<div class="rl-hosts">${hosts.map((x) => relRowHtml(x, d, job && job.state === "done" ? job : null, job && job.state === "done" ? (job.plan || []).find((st) => st.host === x.name && st.phase === "skipped") : null)).join("")}</div>`;
  }
  if (tagged) h += roConfirmHtml(d);
  const c = rls.confirm;
  if (c && c.k === "up" && tagged) {
    const x = hosts.find((y) => y.name === c.host);
    h += `<div class="rel-confirm" role="group" aria-label="Confirm the upgrade"><div>${c.rollback
      ? `Roll back <b>${esc(c.host)}</b> from ${esc(cur.version)} to the binary it had before? comebye restarts on that host.`
      : `Upgrade <b>${esc(c.host)}</b> from ${esc((x && x.version) || "?")} to <b>${esc(cur.version)}</b>? It checks the download, restarts comebye there, and rolls back by itself if the new one fails its checks.`}</div>
      <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-up"${rls.busy ? " disabled" : ""}>Confirm: ${c.rollback ? "roll back" : "upgrade"} ${esc(c.host)}</button>
      <button type="button" data-rel="cancel"${rls.busy ? " disabled" : ""}>Cancel</button></div></div>`;
  }
  if (tagged) h += relDeployConfirmHtml(d);
  return h + `</div>`;
}

function relCardHtml(d) {
  if (!d.current) {
    const hosts = d.hosts || [], vs = [...new Set(hosts.map((x) => x.version).filter(Boolean))];
    const runs = vs.length === 1 && hosts.length ? `Everything runs <b>${esc(vs[0])}</b>.` : vs.length ? `Hosts run ${vs.map(esc).join(", ")}.` : "";
    return `<div class="rl-card" data-card="none"><h3>Current release</h3><div class="rel-h"><span class="rl-big">No release pending</span>
      <button type="button" class="rel-refresh" data-rel="refresh" aria-label="Check again"${rls.inflight ? " disabled" : ""}>↻</button></div>
      <p class="rl-muted">${runs} A coordinator proposes the next one with <code>comebye todo add --kind release</code>.</p></div>`;
  }
  const t = d.todo, rl = t.release, ck = d.check, ro = d.rollout;
  const tagged = rl.state === "tagged";
  const allDone = tagged && ro && ro.complete;
  const failed = rl.state === "failed" || (ck && !ck.ok && ck.ci_conclusion && ck.ci_conclusion !== "success") || (d.build && d.build.state === "failure");
  const cls = allDone ? " rl-good" : failed ? " rl-bad" : d.needs_you ? " rl-attn" : "";
  const STATE = { pending: "waiting for you", tagging: "tagging…", tagged: "tagged", failed: "failed" };
  let h = `<div class="rl-card${cls}" data-card="release" data-rel-id="${escAttr(t.id)}"><h3>${tagged ? "Release" : "Proposed release"}</h3>
    <div class="rel-h"><span class="rl-big">${esc(rl.version)}</span><code class="rel-sha cp" role="button" tabindex="0" aria-label="Copy commit" data-copy="${escAttr(rl.commit)}" title="${escAttr(rl.commit)}">${esc(relShort(rl.commit))}</code>
    <span class="rel-state rel-s-${esc(rl.state)}">${esc(STATE[rl.state] || rl.state)}</span>
    <button type="button" class="rel-refresh" data-rel="refresh" aria-label="Check again"${rls.inflight ? " disabled" : ""}>↻</button></div>`;
  h += `<div class="rl-chips">`;
  if (ck) h += relCiChip(ck);
  if (tagged) h += relBuildChip(d) + (d.notes_url ? " " + relChip("none", "release notes", d.notes_url) : "");
  h += `</div>`;
  if (d.simulated) h += `<div class="rel-block" role="alert">SIMULATED: this server runs no git, no GitHub and no upgrade. Nothing below is real.</div>`;
  if (d.dry_run) h += `<div class="rel-note rel-dry">Dry run: the checks run for real, but nothing is tagged or installed.</div>`;
  h += `<dl class="rel-facts"><dt>Commit</dt><dd><code class="rel-sha cp" role="button" tabindex="0" aria-label="Copy commit">${esc(rl.commit)}</code></dd>`;
  if (ck && ck.subject) h += `<dt>Subject</dt><dd>${esc(ck.subject)}</dd>`;
  const rhost = rl.host || d.release_host;
  if (rhost) h += `<dt>${tagged ? "Tagged on" : "Tags from"}</dt><dd>${esc(rhost)}</dd>`;
  if (ck) h += `<dt>Last tag</dt><dd>${esc(ck.previous || "none yet")}</dd>`;
  h += `</dl>`;
  if (!d.check && !d.check_error && (rl.state === "pending" || rl.state === "failed")) h += `<div class="rel-note">Checking the commit with GitHub…</div>`;
  if (d.check_error) h += `<div class="rel-block" role="alert">Could not check the commit: ${esc(d.check_error)}</div>`;
  if (ck && ck.problem) h += `<div class="rel-block" role="alert">Blocked: ${esc(ck.problem)}${ck.ci_conclusion && ck.ci_conclusion !== "success" ? ". Fix CI first." : ""}</div>`;
  if (ck && ck.changes && ck.changes.length) {
    h += `<details class="rel-changes"><summary>Release notes: ${ck.changes.length} change${ck.changes.length === 1 ? "" : "s"} since ${esc(ck.previous || "the start")}</summary><ul>${ck.changes.map((c) => `<li>${esc(c)}</li>`).join("")}</ul></details>`;
  }
  if (rl.state === "failed" && rl.error) h += `<div class="rel-block" role="alert">The last try failed: ${esc(rl.error)}</div>`;
  if (rl.dry_run) h += `<details class="rel-changes"><summary>Last dry run</summary>${cpOut("pre", "Copy", "Copy the dry run output")}<pre>${esc(rl.dry_run)}</pre></details>`;
  if (d.build_error) h += `<div class="rel-note">Build status unknown: ${esc(d.build_error)}</div>`;
  if (d.deploy_error) h += `<div class="rel-note">Deploy status unknown: ${esc(d.deploy_error)}</div>`;

  // Approve.
  if (rl.state === "pending" || rl.state === "failed") {
    if (!d.can_tag) {
      h += `<div class="rel-note">This host is not set up to tag releases (it needs <code>-release-repo</code>). Run <code>tools/release/tag ${esc(rl.version)} ${esc(rl.commit)}</code> yourself.</div>`;
    } else if (rls.confirm && rls.confirm.k === "tag") {
      h += `<div class="rel-confirm" role="group" aria-label="Confirm the release">
        <div>Tag <b>${esc(rl.version)}</b> at <code>${esc(rl.commit)}</code> and push the tag${d.dry_run ? " (dry run: nothing will be pushed)" : ""}? The Release workflow then builds and publishes it.</div>
        <div class="td-acts"><button type="button" class="rel-go" data-rel="confirm-tag"${rls.busy ? " disabled" : ""}>Confirm: release ${esc(rl.version)}</button>
        <button type="button" data-rel="cancel"${rls.busy ? " disabled" : ""}>Cancel</button></div></div>`;
    } else {
      const ok = !!(ck && ck.ok);
      h += `<div class="td-acts"><button type="button" class="rel-approve" data-rel="approve"${ok && !rls.busy ? "" : " disabled"}>Approve release</button></div>`;
    }
  }
  if (rl.state === "tagging") h += `<div class="rel-note">The tag is being pushed…</div>`;
  if (tagged && ro && !ro.complete) {
    if (rls.confirm && rls.confirm.k === "dismiss") {
      h += `<div class="rel-confirm" role="group" aria-label="Confirm dismissing the release">
        <div>Dismiss <b>${esc(rl.version)}</b>? The rollout is abandoned: the release leaves this tab, the badge and the automatic close, and the hub stops watching it. Nothing is changed on any host and the tag stays. Recorded in the audit log.</div>
        <div class="td-acts"><button type="button" class="rel-go rel-dismiss-go" data-rel="confirm-dismiss"${rls.busy ? " disabled" : ""}>Confirm: dismiss ${esc(rl.version)}</button>
        <button type="button" data-rel="cancel"${rls.busy ? " disabled" : ""}>Cancel</button></div></div>`;
    } else {
      h += `<div class="td-acts"><button type="button" class="rel-dismiss" data-rel="dismiss"${rls.busy ? " disabled" : ""}>Dismiss release…</button></div>`;
    }
  }
  if (tagged && ro) {
    if (ro.complete) h += `<div class="rl-line"><span class="rel-chip rel-ok">all upgraded</span> <span class="rel-note">${ro.total} of ${ro.total} hosts run ${esc(rl.version)}</span></div>`;
    else h += `<div class="rl-line rel-note">${esc(relCount(ro))} on ${esc(rl.version)}${ro.offline ? ". The release is not done until the offline host is back and upgraded" : ". Buttons stay until each host matches"}.</div>`;
  }
  if (allDone && t.state === "done") h += `<div class="rel-note">The hub closed its to-do item when the last host upgraded.</div>`;
  else if (t.state === "dropped" || t.state === "done") h += `<div class="rel-note">Its to-do item is ${t.state === "dropped" ? "dropped" : "closed"}; the release stays here until every host runs it.</div>`;

  // Who did what.
  const au = d.audit || [];
  if (au.length) {
    h += `<details class="rel-changes rel-audit"><summary>Record (${au.length})</summary><ul>${au.slice(-12).reverse().map((e) => {
      const at = Date.parse(e.at);
      return `<li>${Number.isNaN(at) ? "" : esc(fmtAgo(at)) + " ago · "}${esc(e.by)} · ${esc(e.action)}${e.host ? " · " + esc(e.host) : ""}${e.ok ? "" : " (not done)"} · ${esc(e.detail || "")}</li>`;
    }).join("")}</ul></details>`;
  }
  return h + `</div>`;
}

function relHistoryHtml(d) {
  const hist = d.history || [];
  let h = `<div class="rl-card" data-history><h3>History</h3>`;
  if (!hist.length) return h + `<div class="rel-note">No releases yet.</div></div>`;
  h += `<table class="rl-hist"><tbody>`;
  for (const r of hist) {
    const at = r.tagged_at ? new Date(r.tagged_at) : null;
    const date = at && !Number.isNaN(at.getTime()) ? at.toLocaleDateString(undefined, { month: "short", day: "numeric" }) : "";
    const link = r.notes_url && todoLinkHref(r.notes_url) ? `<a href="${escAttr(r.notes_url)}" target="_blank" rel="noopener noreferrer">notes ↗</a>` : "";
    h += `<tr><td class="rl-hv">${esc(r.version)}</td><td class="rl-hd">${esc(date)}</td><td class="rl-hn2"><code>${esc(relShort(r.commit))}</code> ${link}</td></tr>`;
  }
  return h + `</tbody></table></div>`;
}

function relPaneHtml() {
  const d = rls.data;
  if (!d) return rls.err ? `<div class="rel-block" role="alert">Could not load the releases: ${esc(rls.err)}</div>` : '<div class="pr-empty">Loading…</div>';
  return `${rls.err ? `<div class="rel-block rl-err-top" role="alert">Could not refresh: ${esc(rls.err)}</div>` : ""}
    <div class="rl-wrap"><div class="rl-a-card">${relCardHtml(d)}</div><div class="rl-a-fleet">${relFleetHtml(d)}</div><div class="rl-a-hist">${relHistoryHtml(d)}</div></div>`;
}

function renderReleases() {
  const el = $("pr-releases");
  const html = relPaneHtml();
  if (el._html === html) return;
  // Keep the scroll and the focused button across a redraw.
  const ae = document.activeElement, keep = ae && el.contains(ae) && ae.dataset.rel ? { rel: ae.dataset.rel, host: ae.dataset.host || "" } : null;
  el._html = html;
  el.innerHTML = html;
  if (keep) {
    const b = [...el.querySelectorAll("[data-rel]")].find((x) => x.dataset.rel === keep.rel && (x.dataset.host || "") === keep.host);
    if (b && !b.disabled) b.focus({ preventScroll: true });
  }
}

async function relPost(path, body) {
  rls.busy = true;
  renderReleases();
  try {
    await api(path, { method: "POST", headers: { "X-Comebye-Csrf": (rls.data && rls.data.csrf) || "" }, body: JSON.stringify(body), timeout: 60000 });
    rls.confirm = null;
    return true;
  } catch (e) {
    if (e.code === "csrf") { await loadReleases(false); toast("The page was out of date. Check the details and try again.", true); }
    else toast(e.message, true);
    return false;
  } finally {
    rls.busy = false;
  }
}

async function relClick(act, host) {
  const d = rls.data;
  if (!d || !d.todo) { if (act === "refresh") loadReleases(true); return; }
  const t = d.todo, id = encodeURIComponent(t.id);
  switch (act) {
    case "refresh": loadReleases(true); return;
    case "approve": rls.confirm = { k: "tag" }; break;
    case "up": rls.confirm = { k: "up", host, rollback: false }; break;
    case "rollback": rls.confirm = { k: "up", host, rollback: true }; break;
    case "deploy": rls.confirm = { k: "deploy" }; break;
    case "dismiss": rls.confirm = { k: "dismiss" }; break;
    case "confirm-dismiss":
      if (await relPost(`/api/release/${id}/dismiss`, { version: t.release.version, commit: t.release.commit })) toast(`${t.release.version} dismissed.`);
      todos.loadedAt = 0;
      loadTodos();
      await loadReleases(true);
      return;
    case "cancel": rls.confirm = null; break;
    case "rollout": rls.confirm = { k: "rollout", order: (d.rollout_plan || []).filter((st) => st.wave !== "hub").map((st) => st.host), hub: (d.rollout_plan || []).some((st) => st.wave === "hub") }; break;
    case "rollout-again": {
      // Only the hosts the last rollout skipped, still behind.
      const job = roJob(d), behind = new Set((d.hosts || []).filter((x) => !x.current).map((x) => x.name));
      const order = (job ? job.plan : []).filter((st) => st.phase === "skipped" && behind.has(st.host)).map((st) => st.host);
      rls.confirm = { k: "rollout", order, hub: (d.rollout_plan || []).some((st) => st.wave === "hub") };
      break;
    }
    case "confirm-rollout": {
      const c = rls.confirm;
      if (!c || !c.order || (!c.order.length && !c.hub)) return;
      if (await relPost("/api/release/rollout", { todo: t.id, version: t.release.version, commit: t.release.commit, hosts: c.order, hub: !!c.hub })) toast("Rolling out " + t.release.version + "…");
      await loadReleases(true);
      return;
    }
    case "ro-pause": case "ro-resume": case "ro-cancel": {
      const job = roJob(d);
      if (!job) return;
      const verb = act.slice(3);
      if (await relPost(`/api/release/rollout/${verb}`, { id: job.id })) toast({ pause: "Pausing after the step in flight…", resume: "Resuming…", cancel: "Rollout cancelled. Nothing was reverted." }[verb]);
      await loadReleases(true);
      return;
    }
    case "confirm-deploy":
      if (await relPost(`/api/release/${id}/deploy`, { version: t.release.version, commit: t.release.commit })) toast("Started.");
      await loadReleases(true);
      return;
    case "confirm-tag":
      if (await relPost(`/api/release/${id}/approve`, { version: t.release.version, commit: t.release.commit })) toast("Approved. Tagging…");
      todos.loadedAt = 0;
      loadTodos();
      await loadReleases(true);
      return;
    case "confirm-up": {
      const c = rls.confirm;
      if (!c) return;
      if (await relPost(`/api/hosts/${encodeURIComponent(c.host)}/release/upgrade`, { todo: t.id, version: t.release.version, rollback: !!c.rollback })) toast(`${c.rollback ? "Rolling back" : "Upgrading"} ${c.host}…`);
      await loadReleases(true);
      return;
    }
    default: return;
  }
  renderReleases();
}

function onTodoClick(ev) {
  if (ev.target.closest(".td-toggle")) {
    lsSet(TODO_FOLDED_KEY, lsGet(TODO_FOLDED_KEY) === "1" ? "0" : "1");
    renderTodos(true);
    return;
  }
  if (ev.target.closest(".td-notice-x")) { todos.notice = null; renderTodos(true); return; }
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
  const ob = ev.target.closest(".td-open");
  if (ob) { todoOpenSession(ob); return; }
  const ag = ev.target.closest(".td-agent");
  if (ag) { rsLeave(); openDetail(ag.dataset.agent); return; }
  if (ev.target.closest(".td-reptog")) {
    ev.preventDefault();
    const d = item.querySelector("details.td-rep");
    if (d) { d.open = true; todoRepOpen.add(id); d.scrollIntoView({ block: "nearest" }); }
    return;
  }
  if (ev.target.closest(".td-rep > summary")) { const d = ev.target.closest(".td-rep"); if (d.open) todoRepOpen.delete(id); else todoRepOpen.add(id); }
  if (ev.target.closest(".ap-go")) { approvalGo(id, item); return; }
  if (ev.target.closest(".ap-no")) { todoAct(id, "drop"); return; } // "Not now": no confirm box, the hub denies the request
  if (ev.target.closest(".rv-good")) { reviewVerdict(id, "good", item); return; }
  if (ev.target.closest(".rv-chg")) { reviewNote(id, item); return; }
  if (ev.target.closest(".rv-chatpick")) { const tt = todos.items.find((x) => x.id === id); if (tt && tt.page) openChatPick(tt.page.id, false); return; }
  if (ev.target.closest(".rv-chat")) { const tt = todos.items.find((x) => x.id === id); if (tt && tt.page) pagesChat(false, tt.page.id); return; }
  const rk = ev.target.closest(".rv-kind");
  if (rk) { const st = todos.rv.get(id); if (st) { st.kind = rk.dataset.k; renderTodos(true); } return; }
  if (ev.target.closest(".rv-send")) { reviewVerdict(id, "changes", item); return; }
  if (ev.target.closest(".rv-cancel")) { todos.rv.delete(id); renderTodos(true); return; }
  const setT = todos.items.find((x) => x.id === id);
  if (setT && setT.set && !ev.target.closest(".td-row, .td-agent, .td-open, .td-done, .td-drop") && setClick(ev, setT)) return;
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
  if (ev.target.closest(".td-answer-btn")) {
    todoAnswer(id, item.querySelector(".td-text"));
  } else if (ev.target.closest(".td-keep")) {
    todoKeep(id);
  } else if (ev.target.closest(".td-dismiss")) {
    todoAct(id, "dismiss", { reason: ev.target.closest(".td-dismiss").dataset.why || "other" });
  } else if (ev.target.closest(".td-done")) {
    todoAct(id, "done");
  } else if (ev.target.closest(".td-drop")) {
    if (!confirm(`Drop this item?\n\n${t.question}\n\nIt leaves the list and nobody is told.`)) return;
    todoAct(id, "drop");
  }
}

// A typed answer is kept per item (and across reloads), and marks the row.
function onTodoInput(ev) {
  const sb = ev.target.closest && ev.target.closest(".ds-text");
  if (sb) { // a decision set's own words: kept in the page until they are used or saved
    const it = sb.closest(".td-item");
    if (it) todos.setText.set(it.dataset.id + ":" + (sb.dataset.note ? "#note" : sb.dataset.d), sb.value);
    return;
  }
  const ck = ev.target.closest && ev.target.closest(".ap-confirm");
  if (ck) { const it = ck.closest(".td-item"); const ti = it && todos.items.find((x) => x.id === it.dataset.id); if (ti && ti.approval) guState(ti.approval.id).ok = ck.checked; return; }
  if (ev.target.closest && ev.target.closest(".ap-pass")) return; // never a draft: the passphrase lives in its input and nowhere else
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

// The note on a decision set is kept when the box is left.
function onTodoChange(ev) {
  const nb = ev.target.closest && ev.target.closest(".ds-notebox");
  const item = nb && nb.closest(".td-item");
  const t = item && todos.items.find((x) => x.id === item.dataset.id);
  if (t && t.set && !setLocked(t) && nb.value.trim() !== ((t.set.draft && t.set.draft.note) || "")) setSaveNote(t, nb.value.trim());
}

// Keys. Ctrl/Cmd+Enter sends from any answer box. On the desktop To do page:
// ↑/↓ or j/k open the next or previous item, Enter goes to its answer box,
// Esc leaves the box and then closes the item.
function onTodoKey(ev) {
  if (ev.defaultPrevented || ev.isComposing) return;
  const el = ev.target;
  if (el.classList && el.classList.contains("ap-pass")) {
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) { ev.preventDefault(); const it = el.closest(".td-item"); if (it) approvalGo(it.dataset.id, it); }
    return;
  }
  const ds = el.closest && el.closest(".ds-text:not(.ds-notebox)");
  if (ds && ev.key === "Enter" && !ev.shiftKey && !ev.altKey && !ev.isComposing) { ev.preventDefault(); const u = ds.parentElement.querySelector(".ds-use"); if (u) u.click(); return; }
  const box = el.closest && el.closest(".td-text");
  if (box && ev.key === "Enter" && (ev.ctrlKey || ev.metaKey) && !ev.altKey && !ev.shiftKey) {
    ev.preventDefault();
    todoAnswer(box.closest(".td-item").dataset.id, box);
    return;
  }
  const sp = rsActive() ? rs.el : null; // the split review mode is open: its side panel is where the item is
  if (sp && ev.key === "Escape" && !ev.altKey && !ev.ctrlKey && !ev.metaKey && !anotherLayerOpen() && sp.contains(el)) {
    // Escape in order: the comment being written, then the answer box, then the card.
    if (el.id === "pgs-note") { ev.preventDefault(); $("pgs-notecancel")?.click(); sp.querySelector(".rs-back")?.focus(); return; }
    if (box) { ev.preventDefault(); sp.querySelector(".rs-back")?.focus(); return; }
  }
  if (!isDesktop() || state.view !== "todo" || anotherLayerOpen()) return;
  if (ev.altKey || ev.ctrlKey || ev.metaKey || (ev.shiftKey && !/^Digit[1-9]$/.test(ev.code))) return;
  const page = $("pt-body");
  if (!page || !(el === document.body || page.contains(el) || (sp && sp.contains(el)))) return;
  const k = ev.key;
  const cont = sp || page; // where the open item's buttons are
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
  } else if ((k === "1" || k === "2") && todos.openId && !ev.shiftKey && (() => { const t = todos.items.find((x) => x.id === todos.openId); return t && t.state === "open" && todoIsReview(t); })()) {
    // On a review, 1 is Looks good and 2 opens the Needs changes note.
    ev.preventDefault();
    const it = cont.querySelector(`.td-item[data-id="${CSS.escape(todos.openId)}"]`);
    if (k === "1") reviewVerdict(todos.openId, "good", it); else reviewNote(todos.openId, it);
  } else if ((/^[1-9]$/.test(k) || (ev.shiftKey && /^Digit[1-9]$/.test(ev.code))) && todos.openId) {
    // 1-9 sends that choice; Shift+1-9 adds it to a pending combination.
    const d = ev.shiftKey ? ev.code.slice(5) : k;
    const b = cont.querySelector(`.td-item[data-id="${CSS.escape(todos.openId)}"] .td-opt[data-n="${d}"]`);
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

/* ---------- the Status screen (phone) ---------- */
//
// On the phone, Status (the board and the project cards, statusHTML) is a
// screen to the right of the list, reached by the ⋯ menu's Status item; the
// back button or the browser's back returns. Its URL is #/status, the
// desktop Status page's own, so back and reload behave. There is no
// sideways gesture. On the desktop the Status page fills the main pane
// instead, and this screen stays closed.

const sts = { open: false, pushed: false, inited: false };
const STATUS_HASH = "#/status";

const reducedMotion = () => !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

// setStatusScreenOpen slides the screen in or out and keeps the focus and
// the inert state right.
function setStatusScreenOpen(open, animate) {
  const p = $("st-screen");
  const was = sts.open;
  sts.open = open;
  p.classList.toggle("anim", !!animate && !reducedMotion());
  p.style.transform = "";
  p.classList.toggle("open", open);
  p.inert = !open;
  if (open && !was) {
    const el = $("ss-body");
    if (el) el._html = "";
    renderStatus();
    // Focus the screen itself (no ring), so a keyboard user tabs on from its top.
    p.focus?.({ preventScroll: true });
  } else if (!open && was) {
    // Hand the focus back to where the user came from.
    if (p.contains(document.activeElement)) $("btn-fleet").focus?.({ preventScroll: true });
  }
}

function openStatusScreen(animate = true) {
  if (isDesktop()) { openPage("status"); return; }
  if (sts.open || state.selected != null || ledger.on === false) return;
  setStatusScreenOpen(true, animate);
  if (location.hash !== STATUS_HASH) {
    // A history entry, so the phone's back gesture returns to the list.
    history.pushState(null, "", STATUS_HASH);
    sts.pushed = true;
  }
}

function closeStatusScreen() {
  if (!sts.open || isDesktop()) return;
  setStatusScreenOpen(false, true);
  if (sts.pushed) { sts.pushed = false; history.back(); }
  else if (location.hash === STATUS_HASH) history.replaceState(null, "", location.pathname + location.search);
}

// syncStatusScreen follows the URL on the phone (back, forward, a cold start
// on #/status) and keeps the screen closed on the desktop.
function syncStatusScreen() {
  const want = !isDesktop() && location.hash === STATUS_HASH && state.selected == null && ledger.on !== false;
  if (want === sts.open) return;
  if (!want) sts.pushed = false;
  setStatusScreenOpen(want, sts.inited && !isDesktop());
}

function anotherLayerOpen() {
  if (openPopName()) return true;
  return !!document.querySelector('[role="dialog"]:not([hidden])');
}

function initStatusScreen() {
  $("ss-back").addEventListener("click", closeStatusScreen);
  $("st-screen").addEventListener("keydown", (ev) => { if (ev.key === "Escape" && !anotherLayerOpen()) { ev.stopPropagation(); closeStatusScreen(); } });
  syncStatusScreen();
  sts.inited = true;
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
  $("d-transcript").hidden = !lon;
  const st = $("d-mark-state");
  if (!lon) {
    $("d-fresh").hidden = true;
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
  $("d-fresh").hidden = !(coord && agentById(state.selected)?.agent === "claude");
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

function detailTranscript() {
  const id = state.selected;
  toggleDetailMenu(false);
  if (id) location.hash = rcWorkHash(id);
}

function toggleMark() {
  const id = state.selected;
  toggleDetailMenu(false);
  if (!id) return;
  if (ledger.marked.has(id)) unmarkAgent(id);
  else openMark(id);
}

const unmarkWaiting = new Set(); // agents whose Unmark is waiting for their mark request

async function unmarkAgent(id) {
  const key = "mark:" + id;
  // Unmark tapped while the mark's request is still on the wire: wait for it (the badge is already
  // showing) rather than dropping the tap, then unmark if the mark held. A second tap during the wait
  // is the same wait, so only the first goes on.
  if (ledgerOpt.busy(key)) {
    if (unmarkWaiting.has(id)) return;
    unmarkWaiting.add(id);
    await ledgerOpt.idle(key);
    unmarkWaiting.delete(id);
    if (!ledger.marked.has(id) || ledgerOpt.busy(key)) return;
  }
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
  if (d && d.mark && d.mark.ask && d.mark.ask.state === "asking") toast("Marked ready to remove; asking the session what is left to finish");
  ledger.loadedAt = 0;
  await loadLedger();
}

/* ---------- restart in place: end the agent's program and start it again in its pane ---------- */
//
// POST /api/hosts/{host}/restart (internal/server/restart.go). Only an idle or done agent of a kind the hub can
// start again. The hub refuses, and closes nothing, for a working agent, a background task, an open dialog
// and (claude) a scheduled loop or wakeup; those come back as plain sentences, shown in the sheet.
const RESTART_KINDS = ["claude", "pi", "opencode"];
// canRestart: like canPark, but an unnamed agent can be restarted too (the hub names it for the moment).
function canRestart(a) {
  return !!a && RESTART_KINDS.includes(a.agent) && (a.agent_status === "idle" || a.agent_status === "done") && !isCoordinator(a);
}

// The sentences for the refusals; any other code shows the hub's own message.
const RESTART_REFUSALS = {
  agent_working: "It is working; restart it when it is idle",
  background_task: "A background task is running, and a restart would stop it. Restart it when that has finished",
  dialog_open: "A dialog is open in its pane; answer it first",
  kind_not_restartable: "Only claude, pi and opencode agents can be restarted",
  restart_running: "A restart of this pane is already under way",
};
function restartRefusalText(err) {
  const known = RESTART_REFUSALS[err.code];
  if (known) return known;
  const m = String(err.message || "Could not restart");
  return m.charAt(0).toUpperCase() + m.slice(1);
}

const rt = { open: false, id: "", busy: false, force: false, done: false };

function openRestart(id) {
  if (rt.open) return;
  const a = agentById(id);
  if (!canRestart(a)) return;
  Object.assign(rt, { open: true, id, busy: false, force: false, done: false });
  history.pushState({ restart: true }, "", location.href);
  $("rt-name").textContent = a.name || a.pane_id || id;
  $("rt-result").textContent = "";
  $("rt-result").className = "ln-note";
  $("rt-status").textContent = "";
  $("rt-go").textContent = "Restart";
  $("rt-go").disabled = false;
  $("rt").classList.remove("busy");
  $("rt").hidden = false;
  document.body.classList.add("sheet-open");
  $("rt-go").focus();
}

function hideRestart() {
  rt.open = false;
  $("rt").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}

function closeRestart() {
  if (rt.busy) return;
  if (history.state && history.state.restart) history.back();
  else hideRestart();
}

function syncRestart() {
  if (rt.open && !(history.state && history.state.restart)) hideRestart();
}

async function submitRestart() {
  if (rt.busy) return;
  if (rt.done) { closeRestart(); return; }
  const a = agentById(rt.id);
  if (!a) { $("rt-status").textContent = "That agent is gone"; return; }
  rt.busy = true;
  $("rt").classList.add("busy");
  $("rt-go").disabled = true;
  $("rt-result").textContent = "";
  $("rt-status").textContent = "Ending it and starting it again… this takes up to half a minute";
  try {
    const body = { agent: a.pane_id, name: a.name || "" };
    if (rt.force) body.force_schedule = true;
    const d = await api(`/api/hosts/${encodeURIComponent(a.host)}/restart`, { method: "POST", body: JSON.stringify(body), timeout: 60000 });
    rt.busy = false; rt.done = true;
    $("rt").classList.remove("busy");
    $("rt-status").textContent = "";
    const parts = ["Restarted in the same pane, on the same conversation."];
    if (d && d.args_from === "session") parts.push("Its start flags could not be recovered, so it came back with the conversation alone: a model or setting it was started with may need setting again.");
    if (d && d.warning) parts.push(d.warning);
    $("rt-result").textContent = parts.join(" ");
    $("rt-result").className = "ln-note rt-ok" + (d && d.args_from === "session" ? " rt-warn" : "");
    $("rt-go").textContent = "Done";
    $("rt-go").disabled = false;
    $("rt-go").focus();
  } catch (e) {
    rt.busy = false;
    $("rt").classList.remove("busy");
    $("rt-status").textContent = "";
    $("rt-result").textContent = restartRefusalText(e);
    $("rt-result").className = "ln-note rt-err";
    // A loop or wakeup is the user's call: offer to restart anyway, which ends it.
    if (e.code === "schedule_pending" || e.code === "schedule_uncertain") {
      rt.force = true;
      $("rt-go").textContent = "Restart anyway";
    }
    $("rt-go").disabled = false;
  }
}

/* ---------- fresh start: clear the coordinator's context in place ---------- */
//
// The hub sends /clear into the coordinator's pane and a SessionStart hook
// loads a digest (see docs/coordinator-reset.md). The sheet shows how large
// the context is and whether the hook is there, then asks the hub. The hub
// refuses while the coordinator is working or blocked, or while it holds a
// message for it, and says why.

const fs = { open: false, id: "", busy: false };

function fmtTokens(n) { return n >= 1000 ? Math.round(n / 1000) + "k" : String(n); }

async function openFresh() {
  const id = state.selected;
  toggleDetailMenu(false);
  if (!id || fs.open) return;
  const a = agentById(id);
  Object.assign(fs, { open: true, id, busy: false });
  history.pushState({ fresh: true }, "", location.href);
  $("fs-name").textContent = a ? (a.name || a.pane_id || id) : id;
  $("fs-ctx").textContent = "Reading its context size…";
  $("fs-status").textContent = "";
  $("fs-go").disabled = false;
  $("fs").classList.remove("busy");
  $("fs").hidden = false;
  document.body.classList.add("sheet-open");
  $("fs-go").focus();
  try {
    const d = await api("/api/coordinator?refresh=1");
    if (!fs.open || fs.id !== id) return;
    const c = (d.coordinators || []).find((x) => x.id === id);
    const parts = [];
    if (c && c.context) {
      parts.push("Context: " + fmtTokens(c.context.tokens) + " tokens.");
      if (!c.context.hook_active) {
        parts.push("This session was not started with the reset hook, so the hub will refuse: run comebye coordinator install-hook, then start the coordinator again.");
        $("fs-go").disabled = true;
      }
    } else if (c && c.error) parts.push("Could not read its context: " + c.error);
    if (d.threshold > 0) parts.push("The hub clears it by itself, when it is idle, once its context passes " + fmtTokens(d.threshold) + " tokens.");
    else parts.push("The automatic fresh start is off.");
    if (d.last) parts.push("Last fresh start: " + new Date(d.last.at).toLocaleString() + " (" + d.last.trigger + ").");
    $("fs-ctx").textContent = parts.join(" ");
  } catch (e) {
    if (fs.open) $("fs-ctx").textContent = "Could not read its context: " + e.message;
  }
}

function hideFresh() {
  fs.open = false;
  $("fs").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}

function closeFresh() {
  if (fs.busy) return;
  if (history.state && history.state.fresh) history.back();
  else hideFresh();
}

function syncFresh() {
  if (fs.open && !(history.state && history.state.fresh)) hideFresh();
}

async function submitFresh() {
  if (fs.busy) return;
  const id = fs.id;
  fs.busy = true;
  $("fs-go").disabled = true;
  $("fs").classList.add("busy");
  $("fs-status").textContent = "Clearing the context…";
  try {
    const d = await api("/api/coordinator/reset", { method: "POST", body: JSON.stringify({ agent_id: id }) });
    fs.busy = false;
    $("fs").classList.remove("busy");
    closeFresh();
    if (d && d.digest_landed === false) toast("Cleared, but the digest was not seen in the new session: look at the pane", true);
    else toast("Fresh start done" + (d && d.tokens ? " (was " + fmtTokens(d.tokens) + " tokens)" : ""));
  } catch (e) {
    fs.busy = false;
    $("fs").classList.remove("busy");
    $("fs-go").disabled = false;
    $("fs-status").textContent = e.message;
  }
}

// The row menu: one popover, outside the list so a re-render keeps it.
function openRowMenu(btn) {
  const id = btn.dataset.menu;
  const menu = $("row-menu");
  if (!menu.hidden && menu.dataset.id === id) { closeRowMenu(); return; }
  menu.dataset.id = id;
  $("row-transcript").dataset.hash = rcWorkHash(id);
  $("row-park").hidden = !canPark(agentById(id));
  $("row-restart").hidden = !canRestart(agentById(id));
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

function rowMenuTranscript() {
  const hash = $("row-transcript").dataset.hash;
  closeRowMenu();
  if (hash) location.hash = hash;
}

function rowMenuMark() {
  const id = $("row-menu").dataset.id;
  closeRowMenu();
  if (!id) return;
  if (ledger.marked.has(id)) unmarkAgent(id);
  else openMark(id);
}

/* ---------- Park and resume: "Recently closed" on the Agents screen ---------- */
//
// Parking closes an idle agent's pane and keeps its worktree, branch and session; the host
// writes the record and the hub lists every host's (GET /api/parked/all), with a host that
// is down shown greyed from the hub's last copy. A row opens a sheet with Resume and Keep.
const parked = { hosts: [], loaded: false, sig: "[]", sheet: false, id: "", host: "", busy: false };
const PARK_KINDS = ["claude", "pi", "opencode"];

async function loadParked() {
  try {
    const d = await api("/api/parked/all");
    const hosts = d.hosts || [];
    // Only hosts with entries count: a hub with nothing parked must not redraw the list (and
    // replace the rows under the user's hand) on every answer.
    const sig = JSON.stringify(hosts.filter((h) => (h.parked || []).length));
    parked.loaded = true;
    if (sig === parked.sig) return;
    parked.hosts = hosts; parked.sig = sig;
    renderList();
    if (parked.sheet) renderParkSheet();
  } catch { /* the list keeps what it had */ }
}

// parkedEntries are the records the host filter shows, newest first. An archived session's record
// is left out: the Archived screen lists it (the Parked list folds into it).
function parkedEntries() {
  const out = [], inArchive = archParkIds();
  for (const h of parked.hosts) {
    if (!hostIn(h.host)) continue;
    for (const r of h.parked || []) if (!inArchive.has(r.id)) out.push({ host: h.host, online: h.online, rec: r });
  }
  return out.sort((a, b) => Date.parse(b.rec.parked_at) - Date.parse(a.rec.parked_at));
}

// parkLedgerEntry is the ledger entry of a park record: the newest handoff of that name on that host.
function parkLedgerEntry(host, rec) {
  let best = null;
  for (const x of ledger.entries) {
    if (!x || !x.agent_name || x.agent_name !== rec.name || (x.host && host && x.host !== host)) continue;
    if (rec.worktree && x.worktree && x.worktree !== rec.worktree) continue;
    if (!best || Date.parse(x.created_at) > Date.parse(best.created_at)) best = x;
  }
  return best;
}

// PAUSED_STATES: a handoff parked in one of these was parked mid-task, so it is "paused" (it comes back
// to the board when resumed). Parked finished work just has its "pane closed".
const PAUSED_STATES = new Set(["running", "waiting", "landing"]);
function parkLabel(host, rec) {
  if (rec.why === "auto_removed") return "removed";
  const e = parkLedgerEntry(host, rec);
  return e && PAUSED_STATES.has(e.status) ? "paused" : "pane closed";
}

// boardPark is the open park record of a board item's handoff, so the card can say "paused" or
// "pane closed" and offer Resume.
function boardPark(it) {
  if (it.type !== "handoff" || !parked.hosts.length) return null;
  const e = ledger.entries.find((x) => x && x.id === it.id);
  if (!e || !e.agent_name || (it.agent && state.agents.some((a) => agentId(a) === it.agent))) return null;
  for (const h of parked.hosts) {
    if (e.host && h.host !== e.host) continue;
    const rec = (h.parked || []).find((r) => r.name === e.agent_name && !r.resumed_at && r.why !== "auto_removed" && (!r.worktree || !e.worktree || r.worktree === e.worktree));
    if (rec) return { host: h.host, rec, label: PAUSED_STATES.has(e.status) ? "paused" : "pane closed" };
  }
  return null;
}

function parkedRowHTML(e) {
  const r = e.rec, kept = r.dirty ? `<span class="chip yellow">${r.dirty_files || "some"} file${r.dirty_files === 1 ? "" : "s"} edited</span>` : '<span class="chip green">clean</span>';
  const when = fmtAgo(Date.parse(r.parked_at));
  return `<div class="agent parked-row${e.online ? "" : " offline"}" role="button" tabindex="0" data-pk="${esc(r.id)}" data-host="${esc(e.host)}">
    <span class="dot park"></span>
    <div class="meta"><div class="name">${esc(r.name)}${r.pinned ? '<span class="role-chip" title="Kept: never suggested for removal">kept</span>' : ""}</div>
      <div class="sub">${esc(e.host)} · ${esc(r.kind)} · closed ${esc(when)} ago</div>
      <div class="sub">${kept}${e.online ? "" : ' <span class="chip muted">host offline</span>'}</div></div>
    <span class="status">${esc(parkLabel(e.host, r))}</span></div>`;
}

function parkedSectionHTML() {
  const list = parkedEntries(), nArch = archCount();
  if (!list.length && !nArch) return "";
  const toArch = nArch ? `<a class="pk-arch" href="${ARCHIVED_HASH}">${nArch} archived: finished work put away<span class="ln-chev" aria-hidden="true">›</span></a>` : "";
  const open = !collapsed().has("Recently closed");
  return `<section class="proj parked-sec" aria-label="Recently closed">
    <button type="button" class="proj-head" data-proj="Recently closed" aria-expanded="${open}">
      <span class="proj-chev" aria-hidden="true"></span><span class="proj-name">Recently closed</span>
      <span class="proj-counts"><span>${list.length}</span></span>
    </button>
    <div class="proj-body"${open ? "" : " hidden"}>${parkCostHTML(list)}${list.map(parkedRowHTML).join("")}${toArch}</div></section>`;
}

function fmtBytes(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + " GB";
  if (n >= 1e6) return Math.round(n / 1e6) + " MB";
  return Math.max(1, Math.round(n / 1e3)) + " KB";
}

// parkCostHTML is what the closed agents keep on disk, so keeping them is a number you can see.
function parkCostHTML(list) {
  const sum = (k) => list.reduce((n, e) => n + (e.rec[k] || 0), 0);
  const wt = sum("worktree_bytes"), ses = sum("session_bytes"), pad = sum("scratch_bytes");
  if (!wt && !ses && !pad) return "";
  const parts = [wt && `${fmtBytes(wt)} of worktrees`, ses && `${fmtBytes(ses)} of sessions`, pad && `${fmtBytes(pad)} of scratchpads moved off /tmp`].filter(Boolean);
  return `<div class="pk-cost" title="What these closed agents keep on disk. Removal is proposed after they have been closed for a while (14 days by default), and you confirm it under Ready to remove.">${esc(list.length)} kept: ${esc(parts.join(", "))}</div>`;
}

function bindParkedRows(wrap) {
  wrap.querySelectorAll(".agent[data-pk]").forEach((el) => {
    const open = () => openParkSheet(el.dataset.pk, el.dataset.host);
    el.addEventListener("click", open);
    el.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); } });
  });
}

function parkEntry(id, host) {
  const h = parked.hosts.find((x) => x.host === host);
  const rec = h && (h.parked || []).find((r) => r.id === id);
  return rec ? { host, online: h.online, rec } : null;
}

function openParkSheet(id, host) {
  if (parked.sheet) return;
  Object.assign(parked, { sheet: true, id, host, busy: false });
  history.pushState({ park: true }, "", location.href);
  $("pk-status").textContent = "";
  renderParkSheet();
  $("pk").hidden = false;
  document.body.classList.add("sheet-open");
  $("pk-go").focus();
}

function renderParkSheet() {
  const e = parkEntry(parked.id, parked.host);
  if (!e) { if (parked.sheet) closeParkSheet(); return; }
  const r = e.rec;
  $("pk-name").textContent = r.name;
  const when = new Date(r.parked_at).toLocaleString();
  const rows = [
    ["Host", e.host + (e.online ? "" : " (offline)")],
    ["Agent", r.kind],
    ["Worktree", r.worktree],
    ["Branch", r.branch || "—"],
    ["Session", r.kind === "pi" ? r.session.split("/").pop() : r.session],
    ["Closed", `${when} · ${r.why === "auto_removed" ? "removed automatically (landed, clean, SAFE); resume recreates the worktree" : r.why === "auto" ? "parked automatically" : r.why === "suggested" ? "suggested, then you parked it" : "you parked it"}`],
  ];
  if (r.dirty) rows.push(["Edits", `${r.dirty_files || "Some"} uncommitted file${r.dirty_files === 1 ? "" : "s"}, left in place`]);
  if (r.scratch_bytes) rows.push(["Scratchpad", `${(r.scratch_bytes / 1e6).toFixed(1)} MB, on disk until it resumes`]);
  $("pk-kv").innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join("");
  $("pk-note").textContent = e.online
    ? "Resume opens a new pane in the same worktree on the same session, and tells the agent it was parked."
    : `${e.host} is offline, so this cannot be resumed until it is back. This is the hub's last copy of its record.`;
  $("pk-go").disabled = !e.online || parked.busy;
  $("pk-pin").disabled = !e.online || parked.busy;
  $("pk-pin").textContent = r.pinned ? "Stop keeping" : "Keep (pin)";
  $("pk-rm").disabled = parked.busy;
}

function hideParkSheet() {
  parked.sheet = false;
  $("pk").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}
function closeParkSheet() {
  if (parked.busy) return;
  if (history.state && history.state.park) history.back();
  else hideParkSheet();
}
function syncParkSheet() {
  if (parked.sheet && !(history.state && history.state.park)) hideParkSheet();
}

async function submitResume() {
  if (parked.busy) return;
  const { id, host } = parked, e = parkEntry(id, host);
  if (!e || !e.online) return;
  parked.busy = true; renderParkSheet();
  $("pk").classList.add("busy");
  $("pk-status").textContent = "Resuming…";
  try {
    const d = await apiWake(host, `/api/hosts/${encodeURIComponent(host)}/resume`, { method: "POST", body: JSON.stringify({ id }), timeout: 100000 }, (h) => { $("pk-status").textContent = `Starting ${h}…`; });
    parked.busy = false;
    $("pk").classList.remove("busy");
    closeParkSheet();
    toast(`Resumed ${e.rec.name}` + (d.warning ? ` — ${d.warning}` : ""));
    loadParked();
  } catch (err) {
    parked.busy = false;
    $("pk").classList.remove("busy");
    $("pk-status").textContent = err.message;
    renderParkSheet();
  }
}

async function sendParkToRemoval() {
  if (parked.busy) return;
  const { id, host } = parked, e = parkEntry(id, host);
  if (!e) return;
  parked.busy = true; renderParkSheet();
  try {
    await api("/api/park/mark", { method: "POST", body: JSON.stringify({ host, id }) });
    parked.busy = false;
    closeParkSheet();
    toast(`${e.rec.name} is in Ready to remove: review it there. Nothing is deleted until you confirm.`);
    loadLedger();
  } catch (err) {
    parked.busy = false;
    $("pk-status").textContent = err.message;
    renderParkSheet();
  }
}

async function togglePin() {
  if (parked.busy) return;
  const { id, host } = parked, e = parkEntry(id, host);
  if (!e) return;
  parked.busy = true; renderParkSheet();
  try {
    await api(`/api/hosts/${encodeURIComponent(host)}/park/pin`, { method: "POST", body: JSON.stringify({ id, pinned: !e.rec.pinned }) });
    parked.sig = "";
    await loadParked();
  } catch (err) {
    $("pk-status").textContent = err.message;
  }
  parked.busy = false; renderParkSheet();
}

// canPark: an idle or done agent of a kind that can be resumed, not a coordinator.
function canPark(a) {
  return !!a && PARK_KINDS.includes(a.agent) && (a.agent_status === "idle" || a.agent_status === "done") && !isCoordinator(a) && !!a.name;
}

async function parkAgentUI(id) {
  const a = agentById(id);
  if (!canPark(a)) return;
  const url = `/api/hosts/${encodeURIComponent(a.host)}/park`;
  const body = { agent: a.pane_id, name: a.name };
  try {
    let d;
    try {
      d = await api(url, { method: "POST", body: JSON.stringify(body), timeout: 100000 });
    } catch (err) {
      if (err.code !== "dirty_worktree" || !confirm(`${a.name}: ${err.message}.\n\nPark it anyway? Its edits stay where they are.`)) throw err;
      d = await api(url, { method: "POST", body: JSON.stringify({ ...body, dirty: true }), timeout: 100000 });
    }
    toast(`Parked ${a.name}` + (d.scratch ? ` · ${d.scratch}` : ""));
    loadParked();
  } catch (err) {
    if (err.code === "dirty_worktree") return; // declined
    toast(err.message, true);
  }
}


/* ---------- suggest parking: "can park" chips and a one-tap review ---------- */
//
// The hub marks an agent that has been idle for a day with every guard passed (park_hint, see
// parksuggest.go). Nothing is parked by itself: the banner opens a list, and one tap parks the lot.
function parkHintTitle(a) {
  const since = a.park_hint && Date.parse(a.park_hint.since);
  return `Idle${since ? " for " + fmtAgo(since) : ""}, nothing running, no open question and a clean worktree: it can be parked. Park… is in its ⋯ menu.`;
}
function parkSuggestions() {
  return state.agents.filter((a) => a.park_hint && canPark(a) && hostIn(a.host))
    .sort((a, b) => Date.parse(a.park_hint.since) - Date.parse(b.park_hint.since));
}
function parkBannerHTML() {
  const n = parkSuggestions().length;
  return n ? `<button type="button" id="park-banner" class="rtr-banner quiet">${n} idle agent${n === 1 ? "" : "s"} can be parked<span class="ln-chev" aria-hidden="true">›</span></button>` : "";
}
const parkAll = { open: false, busy: false, list: [] };
function openParkAll() {
  if (parkAll.open) return;
  parkAll.open = true; parkAll.busy = false;
  // A snapshot of each agent: the list behind the sheet refreshes while it is open.
  parkAll.list = parkSuggestions().map((a) => ({ id: agentId(a), a: { host: a.host, pane_id: a.pane_id, name: a.name, agent: a.agent, since: a.park_hint.since }, on: true, state: "" }));
  history.pushState({ parkall: true }, "", location.href);
  $("ps-status").textContent = "";
  renderParkAll();
  $("ps").hidden = false;
  document.body.classList.add("sheet-open");
  $("ps-go").focus();
}
function renderParkAll() {
  $("ps-list").innerHTML = parkAll.list.map((it, i) => {
    const a = it.a;
    const nm = a.name || a.pane_id;
    const since = fmtAgo(Date.parse(a.since));
    return `<label class="ps-row"><input type="checkbox" data-i="${i}"${it.on ? " checked" : ""}${parkAll.busy || it.state === "ok" ? " disabled" : ""}>
      <span class="ps-name">${esc(nm)}</span><span class="ps-sub">${esc(a.host + " · " + a.agent + " · idle " + since)}</span>
      <span class="ps-state${it.state && it.state !== "ok" ? " bad" : ""}">${esc(it.state === "ok" ? "parked" : it.state)}</span></label>`;
  }).join("") || '<div class="ln-note">Nothing to park now.</div>';
  const n = parkAll.list.filter((x) => x.on && x.state !== "ok").length;
  $("ps-go").textContent = n ? `Park ${n} agent${n === 1 ? "" : "s"}` : "Park";
  $("ps-go").disabled = parkAll.busy || !n;
}
function closeParkAll() {
  if (parkAll.busy) return;
  if (history.state && history.state.parkall) history.back();
  else hideParkAll();
}
function hideParkAll() { parkAll.open = false; $("ps").hidden = true; document.body.classList.remove("sheet-open"); maybeReload(); }
function syncParkAll() { if (parkAll.open && !(history.state && history.state.parkall)) hideParkAll(); }
async function submitParkAll() {
  if (parkAll.busy) return;
  parkAll.busy = true; $("ps").classList.add("busy"); renderParkAll();
  let ok = 0;
  for (const it of parkAll.list) {
    if (!it.on || it.state === "ok") continue;
    const a = it.a;
    $("ps-status").textContent = `Parking ${a.name}…`;
    try {
      await api(`/api/hosts/${encodeURIComponent(a.host)}/park`, { method: "POST", body: JSON.stringify({ agent: a.pane_id, name: a.name, why: "suggested" }), timeout: 100000 });
      it.state = "ok"; ok++;
    } catch (err) { it.state = err.message; }
    renderParkAll();
  }
  parkAll.busy = false; $("ps").classList.remove("busy");
  const bad = parkAll.list.filter((x) => x.on && x.state && x.state !== "ok").length;
  $("ps-status").textContent = `Parked ${ok}${bad ? `, ${bad} could not be parked (see the list)` : ""}.`;
  renderParkAll();
  loadParked();
}

/* ---------- the header's fleet menu ("⋯" at the top right of the list) ---------- */
//
// Fleet-wide actions, always one tap away: the sweep, the removals review,
// Status (on the phone),
// the notifications panel and a refresh. It reuses the agent view's menu
// styles; the items that need the ledger wait for it ("(loading)") or, on a
// hub that keeps none, give way to a note.

function renderFleetMenu() {
  const lon = ledger.on === true;
  const off = ledger.on === false;
  const loading = !lon && !off;
  const sfx = loading ? " (loading)" : "";
  const note = $("f-note");
  note.hidden = !off;
  note.textContent = off ? "Removals and Status are not available on this hub" : "";
  for (const id of ["f-status", "f-pages", "f-usage", "f-peers", "f-settings"]) { $(id).hidden = off; $(id).disabled = !lon; }
  $("f-peers").hidden = off || !multiHost();
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
  // The palette and the shortcut sheet (keys.js) are popovers too: one manager closes the others, answers
  // Esc (the palette's esc goes back a step first) and puts the focus back. noSheet: no phone backdrop.
  palette: { id: "palette", noSheet: true, close: () => window.CMDK && CMDK.close(false), trigger: () => window.CMDK && CMDK.from(), esc: () => !!(window.CMDK && CMDK.back()) },
  cheats: { id: "cheats", noSheet: true, close: () => window.CMDK && CMDK.closeCheats(false), trigger: () => window.CMDK && CMDK.cheatFrom() },
  group: { id: "gb-pop", close: () => toggleGroupMenu(false), trigger: () => $("gb-btn") },
  hosts: { id: "hp-pop", close: () => toggleHostPicker(false), trigger: () => $("hp-btn") },
  theme: { id: "theme-panel", close: () => toggleThemePanel(false), trigger: () => sxRow("theme") || $("btn-fleet") },
  pgsmenu: { id: "pgs-menu", close: () => pgsPop("pgsmenu", false), trigger: () => $("pgs-menub") },
  pgsinfo: { id: "pgs-infopop", close: () => pgsPop("pgsinfo", false), trigger: () => $("pgs-infob") },
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
function syncSheet() {
  // A drag set animation:none on its sheet so the slide-in would not fight it; a hidden sheet gets it back for its next opening.
  for (const p of Object.values(POPS)) if ($(p.id).hidden) $(p.id).style.animation = "";
  $("sheet-bk").hidden = !(isSheet() && Object.values(POPS).some((p) => !p.noSheet && !$(p.id).hidden)); }
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
  const r = anchor.getBoundingClientRect(), w = el.offsetWidth;
  let top = r.bottom + 4, room = innerHeight - top - 8;
  // A button low on the screen (a row at the foot of Settings): open upward when that has more room.
  if (room < Math.min(el.scrollHeight, 280) && r.top - 12 > room) { room = Math.min(el.scrollHeight, r.top - 12); top = r.top - 4 - room; }
  el.style.left = Math.max(8, Math.min(innerWidth - w - 8, r.right - w)) + "px";
  el.style.top = top + "px";
  el.style.maxHeight = Math.max(160, room) + "px";
}

function initPopovers() {
  document.addEventListener("keydown", (ev) => {
    const name = openPopName();
    if (!name) return;
    const el = $(POPS[name].id);
    // A popover with steps (the palette) answers Esc itself first: it goes back one step.
    if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); if (POPS[name].esc && POPS[name].esc()) return; closePop(name, true); return; }
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
  // reset leaves animation alone: clearing it on a sheet that is still open replays the slide-in (syncSheet restores it once the sheet is hidden).
  const reset = (pop) => { pop.style.transform = ""; pop.style.transition = ""; };
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
  sxRowVals();
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

function toggleThemePanel(show, anchor) {
  const p = $("theme-panel");
  const open = show ?? p.hidden;
  if (open === !p.hidden) return;
  p.hidden = !open;
  if (open) {
    tp.step = 1;
    renderThemePanel();
    placePop("theme", anchor || $("btn-fleet"));
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

/* ---------- font: Sans or Serif (themes.js owns the saved choice; Settings > Appearance is the control) ---------- */

function fontLabel() { return CT.FONTS.find((f) => f.id === CT.fontChoice()).name; }

// Keep the fleet menu's value and the Settings radios true after a change here or in another tab.
function applyFont() {
  CT.fontApply();
  const v = $("f-font-val");
  if (v) v.textContent = fontLabel();
}

function initFontPanel() {
  // The fleet menu's Font entry toggles Sans and Serif; the choice itself lives in Settings > Appearance.
  $("f-font").addEventListener("click", fleetAction(() => {
    CT.fontSet(CT.fontChoice() ? "" : "serif");
    applyFont();
    toast("Font: " + fontLabel());
    if (sx.route) renderSettings();
  }));
  window.addEventListener("storage", (e) => { if (e.key === CT.KEY_FONT) applyFont(); });
  applyFont();
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

function fleetRefresh() {
  ledger.loadedAt = 0;
  refreshAgents().catch(() => setConn(false));
  loadLedger();
  toast("Refreshing");
}

/* ---------- Archived: a screen (#/archived) ---------- */
//
// Finished work nobody will merge or look at soon, put away by the hub's sweep
// loop (or by hand): GET /api/archived (archived.go). Each row says why it was
// archived, the commits main lacks, what is left (followup items as "Owes
// you"), and the build output its archiving cleared. Nothing here deletes
// anything: Unarchive brings it back to the board, Resume also reopens its
// parked pane, and Propose removal marks it, so it goes to Ready to remove,
// is asked what is left and is checked by git there. It is reached from the
// board's footer, from Ready to remove and from Recently closed, which leaves
// out the park records of archived sessions (they are listed here).

const ARCHIVED_HASH = "#/archived";
const ARCH_FILTERS = [
  { key: "all", label: "All", ok: () => true },
  { key: "unmerged", label: "Unmerged", ok: (r) => (r.archive.unmerged || 0) > 0 },
  { key: "reports", label: "Reports", ok: (r) => !!r.report },
  { key: "left", label: "Something left", ok: (r) => (r.archive.left || []).length > 0 },
];
const arch = { route: false, data: null, err: "", q: "", filter: "all", busy: new Set(), open: new Set(), inflight: false, sig: "" };

// archCount is the number archived: the list when it is in, else the board's count.
function archCount() {
  if (arch.data) return arch.data.count || 0;
  return (board.data && board.data.loose && board.data.loose.archived) || 0;
}

// archParkIds are the park records of archived sessions: Recently closed leaves them out.
function archParkIds() {
  return new Set(((arch.data && arch.data.archived) || []).map((r) => r.archive && r.archive.park_id).filter(Boolean));
}

async function loadArchived() {
  if (arch.inflight || ledger.on !== true) return;
  arch.inflight = true;
  try {
    const d = await api("/api/archived");
    d.archived = Array.isArray(d.archived) ? d.archived : [];
    for (const r of d.archived) r.archive = r.archive || {};
    arch.data = d; arch.err = "";
  } catch (e) {
    arch.err = e.message;
  } finally { arch.inflight = false; }
  const sig = JSON.stringify([...archParkIds()]) + ":" + archCount();
  if (sig !== arch.sig) { arch.sig = sig; renderList(); renderNav(); }
  renderArchived();
  if (review.open) renderReviewArch();
}

// archItemText is a leftover item as a line: its text, or what it names.
function archItemText(x) {
  const what = x.type === "pr" || x.type === "issue" ? `${x.repo || ""}#${x.number || ""}` : x.type === "branch" ? (x.remote ? x.remote + "/" : "") + (x.branch || "")
    : x.type === "todo" ? x.id || "" : x.type === "path" ? x.name || "" : "";
  return [what, x.text].filter(Boolean).join(": ") || "(no detail given)";
}

function archRowHTML(r) {
  const a = r.archive, id = esc(r.id), name = esc(r.name || r.id), busy = arch.busy.has(r.id) ? " disabled" : "";
  const left = a.left || [];
  const chips = (r.purpose ? `<span class="chip line">${esc(r.purpose)}</span>` : "") + (left.length ? `<span class="chip yellow">${left.length} left</span>` : "") +
    (multiHost() && r.host ? `<span class="host-chip">${esc(r.host)}</span>` : "");
  const where = [r.project, r.branch].filter(Boolean).map(esc).join(" · ");
  const when = a.at ? `archived ${esc(fmtAgo(Date.parse(a.at)))} ago` : "archived";
  const commits = a.unmerged > 0 ? `${a.unmerged} commit${a.unmerged === 1 ? "" : "s"} not merged${a.unpushed > 0 ? `, ${a.unpushed} on no remote` : a.unpushed === 0 ? ", pushed" : ""}.`
    : a.unmerged === 0 ? "Nothing that its base lacks." : "";
  const build = a.build_pending ? `Its build output is cleared when ${esc(r.host)} is next up.`
    : a.build_freed ? `Freed ${esc(fmtBytes(a.build_freed))} of build output${(a.build_dirs || []).length ? ` (${a.build_dirs.map(esc).join(", ")})` : ""}.` : "";
  const disk = a.worktree_bytes ? `${esc(fmtBytes(a.worktree_bytes))} on disk.` : "";
  const facts = [commits && esc(commits), build, disk].filter(Boolean).join(" ");
  const owes = left.filter((x) => x.type === "followup" || x.type === "decision"), cant = left.filter((x) => x.type === "cleanup" || x.type === "other");
  const waits = left.filter((x) => !owes.includes(x) && !cant.includes(x));
  const line = (label, xs, cls) => xs.length ? `<div class="ar-left ${cls}"><b>${label}:</b> ${xs.map((x) => esc(archItemText(x))).join("; ")}</div>` : "";
  const e = r.report ? ledger.entries.find((x) => x && x.id === r.id) : null;
  const rep = e && e.final_report ? `<details class="rv-report" data-id="${id}"><summary>Final report${reportAgeHtml(e)}</summary><div class="md"></div>${newerReplyHtml(e, "archived:" + e.id)}</details>` : "";
  const resume = a.park_id ? `<button type="button" class="btn" data-ar="resume" data-id="${id}" aria-label="Unarchive ${name} and resume its pane"${busy}>Resume</button>` : "";
  const propose = r.agent_id ? `<button type="button" class="btn" data-ar="propose" data-id="${id}" aria-label="Propose removing ${name}"${busy}>Propose removal</button>` : "";
  return `<div class="rv-item ar-item" data-id="${id}">
    <div class="rv-top"><span class="rv-name">${name}</span>${chips}</div>
    <div class="rv-sub">${where ? where + " · " : ""}${when} · <a class="ar-tr" href="#/search/for?ledger=${encodeURIComponent(r.id)}">transcript</a></div>
    <div class="rv-reason">${esc(a.why || "Archived.")}${facts ? " " + facts : ""}</div>
    ${line("Owes you", owes, "owes")}${line("Can’t check", cant, "cant")}${line("Waits on", waits, "waits")}${rep}
    <div class="rv-actions"><button type="button" class="btn" data-ar="restore" data-id="${id}" aria-label="Unarchive ${name}"${busy}>Unarchive</button>${resume}${propose}</div></div>`;
}

function archMatches(r, q) {
  if (!q) return true;
  const a = r.archive;
  return [r.name, r.project, r.branch, r.purpose, r.host, a.why, ...(a.left || []).map(archItemText)].some((s) => s && String(s).toLowerCase().includes(q));
}

function renderArchived() {
  if (!arch.route) return;
  const d = arch.data, rows = d ? d.archived : [];
  $("ar-sum").innerHTML = d ? `<b>${d.count} archived</b> · ${esc(fmtBytes(d.worktree_bytes || 0))} on disk · ${esc(fmtBytes(d.build_freed || 0))} of build output freed · no source is ever deleted` : "";
  const q = arch.q.trim().toLowerCase();
  const found = rows.filter((r) => archMatches(r, q));
  $("ar-seg").innerHTML = ARCH_FILTERS.map((f) => `<button type="button" data-ar-f="${f.key}" aria-pressed="${arch.filter === f.key}">${esc(f.label)} <span class="ar-n">${found.filter(f.ok).length}</span></button>`).join("");
  const f = ARCH_FILTERS.find((x) => x.key === arch.filter) || ARCH_FILTERS[0];
  const shown = found.filter(f.ok);
  let html;
  if (!d) html = arch.err ? `<div class="ln-empty">${esc(arch.err)}</div>` : `<div class="ln-empty">Loading…</div>`;
  else if (!rows.length) html = `<div class="ln-empty">Nothing is archived. The hub archives finished work after a week with no progress (3 days for research, plans, reviews and the like), and keeps its worktree and branch.</div>`;
  else if (!shown.length) html = `<div class="ln-empty">No archived session matches.</div>`;
  else html = shown.map(archRowHTML).join("");
  const list = $("ar-list");
  if (list._html === html) return;
  list._html = html;
  list.innerHTML = html;
  list.querySelectorAll(".rv-report").forEach((det) => {
    const e = ledger.entries.find((x) => x && x.id === det.dataset.id);
    if (e) mdOnOpen(det, det.querySelector(".md"), e.final_report, "archived:" + e.id);
    if (arch.open.has(det.dataset.id)) det.open = true;
    det.addEventListener("toggle", () => { if (det.open) arch.open.add(det.dataset.id); else arch.open.delete(det.dataset.id); });
  });
}

function archSay(msg, isErr) { $("ar-msg").textContent = msg || ""; $("ar-msg").classList.toggle("err", !!isErr); }

async function archAct(act, id) {
  const r = arch.data && arch.data.archived.find((x) => x.id === id);
  if (!r || arch.busy.has(id)) return;
  arch.busy.add(id); renderArchived();
  const name = r.name || r.id;
  try {
    if (act === "propose") {
      await api("/api/marks", { method: "POST", body: JSON.stringify({ agent_id: r.agent_id, reason: "Proposed from the Archived screen" }) });
      archSay(`${name} is under Ready to remove: it is asked what is left, and git decides before anything is deleted.`);
    } else {
      const d = await api("/api/archived/restore", { method: "POST", body: JSON.stringify({ ref: r.id, resume: act === "resume" }) });
      if (d.resume_error) archSay(`Unarchived ${name}, but it could not be resumed: ${d.resume_error}`, true);
      else archSay(act === "resume" ? `Unarchived ${name} and resumed its pane.` : `Unarchived ${name}: it is back on the board.`);
      if (act === "resume") { parked.sig = ""; loadParked(); }
    }
  } catch (e) {
    archSay(e.message, true);
  } finally { arch.busy.delete(id); }
  board.at = 0;
  loadLedger();
  await loadArchived();
  loadBoard();
}

function syncArchived() {
  const on = location.hash.split("?")[0] === ARCHIVED_HASH;
  const el = $("archived");
  const was = !el.hidden;
  arch.route = on;
  el.hidden = !on;
  if (!on) return;
  if (!was) {
    archSay("");
    $("ar-list")._html = "";
    loadArchived();
    ($("ar-back").offsetParent ? $("ar-back") : $("ar-body")).focus({ preventScroll: true });
  }
  renderArchived();
}

function closeArchived() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

function initArchived() {
  $("ar-back").addEventListener("click", closeArchived);
  $("archived").addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape") return;
    ev.stopPropagation();
    if (ev.target.id === "ar-q" && arch.q) { arch.q = ev.target.value = ""; renderArchived(); } else closeArchived();
  });
  $("ar-q").addEventListener("input", (ev) => { arch.q = ev.target.value; renderArchived(); });
  $("ar-seg").addEventListener("click", (ev) => {
    const b = ev.target.closest("[data-ar-f]");
    if (b) { arch.filter = b.dataset.arF; renderArchived(); }
  });
  $("ar-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-ar]");
    if (b && !b.disabled) archAct(b.dataset.ar, b.dataset.id);
  });
}

// renderReviewArch is the "Archived (N)" link on Ready to remove.
function renderReviewArch() {
  const el = $("rv-arch");
  if (!el) return;
  const n = archCount();
  el.hidden = !n;
  el.innerHTML = n ? `<a href="${ARCHIVED_HASH}">Archived (${n})</a><span> finished work put away, with its worktree and branch kept. Propose removal there to bring one here.</span>` : "";
}

/* ---------- Ready to remove: a page (#/remove) ---------- */
//
// The sessions marked ready to remove, each with safety checks run on its
// host just now: uncommitted changes, commits its base lacks, an agent that
// went back to work. Removing closes the session (its workspace, or only its
// pane when others share it) and removes the worktree only when it is clean
// and merged; its local branch goes with it. A session whose worktree has work a
// removal would keep can instead be discarded: the row lists what would be
// lost, and only an explicit confirmation removes the worktree anyway and
// deletes its branch. A coordinator may propose that (mark --discard); only
// this page does it. Every removal is confirmed in the row it belongs to.
//
// "Look for finished agents" (the sweep) asks the hub for a dry run, shows
// what it would mark (clean, merged, nothing unpushed) and what needs a
// look, and marks the ready ones only on its button. Marked rows appear in
// the list below it.

const REMOVE_HASH = "#/remove";
const review = { open: false, items: [], auto: [], results: new Map(), busy: false, seq: 0, askTimer: 0, sel: new Set(), confirm: null };
const sweep = { busy: false, seq: 0, data: null, marked: false, shown: false, err: "" };

// openReview goes to the page; the hash change shows it.
function openReview() {
  if (location.hash !== REMOVE_HASH) location.hash = REMOVE_HASH;
}

function showReview() {
  review.open = true;
  review.results = new Map();
  review.sel = new Set();
  review.confirm = null;
  Object.assign(sweep, { busy: false, data: null, marked: false, shown: false, err: "" });
  $("review").hidden = false;
  document.body.classList.add("has-remove");
  document.body.classList.toggle("sheet-open", !isDesktop());
  renderSweep();
  renderNav();
  loadReview();
  loadArchived();
}

function hideReview() {
  review.open = false;
  review.seq++;
  sweep.seq++;
  review.confirm = null;
  // Leaving is always possible: a check that is still running is dropped (its seq is stale), and a
  // removal that is in flight finishes on its own.
  review.busy = false;
  sweep.busy = false;
  $("review").classList.remove("busy");
  $("review").hidden = true;
  document.body.classList.remove("has-remove", "sheet-open");
  renderNav();
  maybeReload();
}

// closeReview is the phone's Back: it leaves for the list.
function closeReview() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

function syncReview() {
  const want = location.hash.split("?")[0] === REMOVE_HASH;
  if (want && !review.open) showReview();
  else if (!want && review.open) hideReview();
}

function setReviewBusy(on, text) {
  review.busy = on;
  $("review").classList.toggle("busy", on);
  $("rv-status").textContent = text || "";
  renderReview();
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
  review.auto = Array.isArray(d.auto_removed) ? d.auto_removed : [];
  review.branches = Array.isArray(d.sweep_branches) ? d.sweep_branches : [];
  const ids = new Set(review.items.map((it) => it.id));
  for (const id of [...review.sel]) if (!ids.has(id)) review.sel.delete(id);
  if (review.confirm && !review.confirm.ids.every((id) => ids.has(id))) review.confirm = null;
  if (!quiet) setReviewBusy(false);
  renderReview();
  if (review.items.some((it) => it.mark && it.mark.ask && (it.mark.ask.state === "asking" || it.mark.ask.reasked_at || it.mark.ask.cleaning_at))) {
    clearTimeout(review.askTimer);
    review.askTimer = setTimeout(() => loadReview(true), 6000);
  }
}

// askAge says how long ago an answer was given ("6 h ago"), as the hub's own notes do.
function askAge(iso) {
  const m = Math.floor((Date.now() - Date.parse(iso)) / 60000);
  if (!(m >= 0)) return "";
  if (m < 1) return "just now";
  if (m < 60) return m + " min ago";
  if (m < 48 * 60) return Math.floor(m / 60) + " h ago";
  return Math.floor(m / 1440) + " d ago";
}

// reviewAskHTML is a marked session's answer to "what is left to finish this work?", with the age it was given and,
// when something is left, each item it named as the hub re-checked it just now (resolved, still holding, or not
// checkable), who it waits on, and a Re-ask for the item. When only cleanup the session can do itself is left, a
// "Let it clean up" button tells it to do exactly that. Its text is the session's, escaped.
const ASK_ITEM_CHIP = { resolved: "resolved", unresolved: "holds", unchecked: "can’t check" };
// Who an unresolved item of a type waits on, when the type says so.
const ASK_WAITS = { decision: "waits on you", followup: "yours to do", cleanup: "agent can do it" };
// loopNote says what the sweep loop did about an answer, and when ("the sweep, 2 h ago: deleted origin/x …").
const loopNote = (l) => l ? `<div class="rv-ask-n rv-loop">Sweep · ${esc(askAge(l.at))}: ${esc(l.text)}</div>` : "";
function reviewAskHTML(it, dis) {
  const ar = it.ask_review;
  if (!ar || ar.state !== "unsafe") return "";
  const id = esc(it.id);
  const re = ar.cleaning ? " · cleaning up…" : ar.reasking ? " · asking it again…" : "";
  const busy = !!(ar.reasking || ar.cleaning);
  const hold = ar.resolved ? "all of it is done now" : ar.stale_only ? "the sweep can finish it" : "still left";
  const rows = (ar.items || []).map((x, i) => {
    const found = (x.stale || x.state === "resolved") && x.now ? ` <span class="rv-say">${esc(x.now)}</span>` : "";
    const text = x.type === "other" || x.type === "cleanup" ? esc(x.text || "(no detail given)") + found : `<b>${esc(x.label)}</b> ${esc(x.now)}${x.text ? ` <span class="rv-say">“${esc(x.text)}”</span>` : ""}`;
    const waits = x.state !== "resolved" && x.stale ? "sweep can do it" : x.state !== "resolved" && ASK_WAITS[x.type] ? ASK_WAITS[x.type] : "";
    const chip = waits || ASK_ITEM_CHIP[x.state] || x.state;
    return `<li class="rv-ai ${esc(x.state)} t-${esc(x.type)}"><span class="rv-chip">${esc(chip)}</span><span class="rv-ai-t">${text}</span>` +
      (ar.stale_only || (it.live && !it.live.present) ? "" : `<button type="button" class="btn rv-reask" data-act="reask" data-id="${id}" data-item="${i + 1}" aria-label="Re-ask the session about item ${i + 1}"${dis || busy ? " disabled" : ""}>Re-ask</button>`) + `</li>`;
  }).join("");
  const staleN = ar.stale_only ? `<div class="rv-ask-act"><span class="rv-ask-n">Git shows every commit on that branch is already in the default branch, so removing this session deletes it first (and the sweep does it by itself when auto-delete is on).</span></div>` : "";
  const goneN = ar.only_cleanup && !ar.resolved && !ar.stale_only && it.live && !it.live.present
    ? `<div class="rv-ask-act"><span class="rv-ask-n">The agent is gone, so nothing can do this for it. The sweep checks what it can on the host; the rest is left to you.</span></div>` : "";
  const clean = ar.only_cleanup && !ar.resolved && !ar.stale_only && !(it.live && !it.live.present)
    ? `<div class="rv-ask-act"><button type="button" class="btn rv-cleanup" data-act="cleanup" data-id="${id}"${dis || busy ? " disabled" : ""}>Let it clean up</button>` +
      `<span class="rv-ask-n">Tells the session to do exactly what is listed, then asks again what is left.</span></div>` : "";
  const note = ar.reask_note ? `<div class="rv-ask-n">${esc(ar.reask_note)}</div>` : "";
  return `<div class="rv-ask ${ar.resolved ? "resolved" : "holds"}"><div class="rv-ask-h"><b>LEFT TO FINISH</b> · answered <span class="rv-age">${esc(askAge(ar.answered_at))}</span>${re} · ${hold}</div><ul class="rv-items">${rows}</ul>${clean}${goneN}${staleN}${loopNote(ar.loop)}${note}</div>`;
}

const reviewDone = (it) => !!(review.results.get(it.id) || {}).ok;
// isWorking: the agent is mid-task (or waiting on you), so removing it needs the "allow working" flag.
const reviewWorking = (it) => !!(it.live && (it.live.agent_status === "working" || it.live.agent_status === "blocked"));

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
    ? `<li>Clean, and merged into ${esc(c.base_used || "its base")}: the worktree and its local branch will be removed</li>` : "";
  const ask = m.ask || {};
  const said = ask.state === "safe"
    ? `<li>Nothing is left to finish${ask.reason && !ask.loop ? ": " + esc(ask.reason) : ""} · answered <span class="rv-age">${esc(askAge(ask.at))}</span> ${it.live && it.live.present ? `<button type="button" class="btn rv-reask" data-act="reask" data-id="${esc(it.id)}"${review.busy || ask.reasked_at ? " disabled" : ""}>Re-ask</button>` : ""}${loopNote(ask.loop)}</li>` : "";
  // An UNSAFE answer's items are rows of their own below (reviewAskHTML): their warnings and per-item notes are not repeated.
  const askRows = !!(it.ask_review && it.ask_review.state === "unsafe");
  const warnings = (it.warnings || []).filter((w) => !(askRows && w.startsWith("the session says this is left"))).map((w) => `<li>${esc(w)}</li>`).join("");
  const notes = (it.notes || []).filter((w) => !(askRows && w.startsWith("the session listed ") && (w.includes(" as left, but it ") || w.startsWith("the session listed this as left:")))).map((w) => `<li>${esc(w)}</li>`).join("");
  const repE = it.entry_id ? ledger.entries.find((x) => x && x.id === it.entry_id) : null;
  const report = it.final_report
    ? `<details class="rv-report" data-id="${esc(it.id)}"><summary>Final report${reportAgeHtml(repE && repE.final_report === it.final_report ? repE : null)}</summary>${copyBtnHtml("rep:" + it.id, it.final_report, "Copy report", "Copy report as Markdown", "cpb-rep")}<div class="md"></div>${repE ? newerReplyHtml(repE, "rv:" + it.id) : ""}<div class="hubchk" data-id="${esc(it.id)}" hidden></div></details>` : "";
  const loss = it.discardable && !done ? lossHTML(it) : "";
  const dd = !!it.delivered_discard && !done;
  let result = "";
  if (res && res.ok) {
    const r = res.result || {};
    const closed = r.closed === "workspace" ? "Closed its workspace" : r.closed === "pane" ? "Closed its pane (the workspace has other agents or is the repository's home)" : "Nothing was open to close";
    let wt = ".";
    if (r.discarded) {
      wt = ". Discarded the worktree" + (r.branch_deleted ? ` and deleted the branch ${esc(r.branch_deleted)}.` : ".") +
        (r.kept ? `<span class="kept">${esc(r.kept)}</span>` : "");
    } else if (r.worktree_removed) wt = ". Removed the worktree" + (r.branch_deleted ? ` and its branch ${esc(r.branch_deleted)}.` : "; the branch is kept.");
    else if (r.kept) wt = `.<span class="kept">Kept the worktree: ${esc(r.kept)}</span>`;
    result = `<div class="rv-result ok">${closed}${wt}</div>`;
  } else if (res) {
    result = `<div class="rv-result err">Not removed: ${esc(res.message || res.code || "failed")}</div>`;
  }
  const dis = review.busy ? " disabled" : "";
  const cf = review.confirm && review.confirm.kind !== "bulk" && review.confirm.ids.length === 1 && review.confirm.ids[0] === it.id ? review.confirm : null;
  const name = esc(it.name || it.pane_id || it.id);
  let actions = "";
  if (done) actions = "";
  else if (cf) actions = reviewConfirmHTML(it, cf);
  else if (dd) actions = `<div class="rv-actions">
      <button type="button" class="btn danger" data-act="discard" data-id="${esc(it.id)}"${dis}>Discard work…</button>
      <button type="button" class="btn" data-act="remove" data-id="${esc(it.id)}"${dis}>Close session only…</button>
      <button type="button" class="btn" data-act="unmark" data-id="${esc(it.id)}"${dis}>Keep</button>
    </div>`;
  else actions = `<div class="rv-actions">
      <button type="button" class="btn go" data-act="remove" data-id="${esc(it.id)}"${dis}>${it.safe ? "Remove" : "Remove anyway…"}</button>
      ${it.discardable ? `<button type="button" class="btn danger" data-act="discard" data-id="${esc(it.id)}"${dis}>Discard work…</button>` : ""}
      <button type="button" class="btn" data-act="unmark" data-id="${esc(it.id)}"${dis}>Keep</button>
    </div>`;
  const role = it.coordinator ? '<span class="role-chip">coordinator</span>' : "";
  const chk = done ? "" : `<input type="checkbox" class="rv-chk" data-id="${esc(it.id)}" aria-label="Select ${name}"${review.sel.has(it.id) ? " checked" : ""}${dis}>`;
  return `<div class="rv-item${it.safe ? "" : " warn"}${done ? " done" : ""}" data-id="${esc(it.id)}">
    <div class="rv-top">${chk}<span class="rv-name">${name}</span>${role}${host}${branch}${status}</div>
    ${where ? `<div class="rv-sub">${where}</div>` : ""}
    <div class="rv-sub"><a class="rv-tr" href="${esc(rcWorkHash(it.id))}">Open the work: its transcript</a></div>
    <div class="rv-reason">${marked}</div>
    <ul class="rv-list warn">${done ? "" : warnings}</ul>
    <ul class="rv-list ok">${done ? "" : ok + said}</ul>
    <ul class="rv-list info">${done ? "" : notes}</ul>
    ${done ? "" : reviewAskHTML(it, review.busy ? " disabled" : "")}
    ${loss}${report}${actions}${result}
  </div>`;
}

// reviewConfirmHTML is the question a row asks before it removes anything.
function reviewConfirmHTML(it, cf) {
  const name = `<b>${esc(it.name || it.pane_id || it.id)}</b>`;
  let body, yes;
  if (cf.kind === "discard") {
    const c = it.checks || {};
    body = `<p>Discard ${name}? This closes the session, removes ${esc(c.path || "its worktree")} and throws away the work listed above. This cannot be undone.</p>`;
    yes = "Discard work";
  } else if (it.delivered_discard) {
    body = `<p>Close ${name} but keep its worktree? Its notes and commits stay on disk; only the session closes.</p>`;
    yes = "Close session";
  } else if (it.safe && !(it.warnings || []).length) {
    body = `<p>Remove the worktree for ${name}? The branch stays.</p>`;
    yes = "Remove";
  } else {
    const w = (it.warnings || []).map((x) => `<li>${esc(x)}</li>`).join("");
    body = `<p>${name} has work a removal keeps on disk:</p><ul>${w || "<li>its worktree is not clean and merged</li>"}</ul><p>The worktree stays; only the session closes.</p>`;
    yes = "Close session";
  }
  if (it.coordinator) body += `<p>${name} is a coordinator. Removing it closes the agent that hands off and follows your work (the ledger stays on the hub).</p>`;
  return `<div class="rv-confirm" role="group" aria-label="Confirm">${body}<div class="rv-actions">
    <button type="button" class="btn ${cf.kind === "discard" ? "danger" : "go"}" data-act="confirm-yes" data-id="${esc(it.id)}">${yes}</button>
    <button type="button" class="btn" data-act="confirm-no" data-id="${esc(it.id)}">Cancel</button></div></div>`;
}

// deliveredLinks names the Pages that capture an item's work, each a link to
// the Pages screen (page titles are agent text, escaped).
function deliveredLinks(it) {
  const pages = (it.delivered && it.delivered.pages) || [];
  return pages.map((p) => `<a class="rv-page" href="${pageHash(p.id)}">${esc(p.title || p.id)}</a>`).join(", ") || "a page";
}

// deliveredDrops is one line of what a delivered item's discard throws away.
function deliveredDrops(it) {
  const d = it.delivered || {};
  const bits = [];
  const some = (list, n) => list.slice(0, 4).map((x) => `<code>${esc(x)}</code>`).join(", ") + (n > 4 ? ", …" : "");
  if (d.files) bits.push(`${d.files} uncommitted file${d.files === 1 ? "" : "s"} (${some(d.file_list || [], d.files)})`);
  if (d.commits) bits.push(`${d.commits} unmerged commit${d.commits === 1 ? "" : "s"} (${some((d.commit_list || []).map((c) => c.replace(/^[0-9a-f]+ /, "")), d.commits)})`);
  return bits.join(" and ") || "nothing";
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
  let proposed = it.mark && it.mark.discard ? '<div class="rv-loss-h">Proposed: discard this work</div>' : '<div class="rv-loss-h">Discarding would delete</div>';
  if (it.delivered_discard) proposed = `<div class="rv-loss-h">Captured in ${deliveredLinks(it)}. Discarding drops</div>`;
  return `<div class="rv-loss">${proposed}<ul>${rows.join("")}</ul></div>`;
}

function renderReview() {
  const list = $("rv-list");
  list.innerHTML = review.items.length ? review.items.map(reviewItemHTML).join("")
    : '<div class="ln-empty">Nothing is marked ready to remove. Mark a session from its ⋮ menu, or look for finished agents.</div>';
  list.querySelectorAll(".rv-report").forEach((d) => {
    const it = review.items.find((x) => x.id === d.dataset.id);
    if (it) mdOnOpen(d, d.querySelector(".md"), it.final_report, "report:" + it.id);
    d.addEventListener("toggle", () => { if (d.open) fillHubChecks(d); });
  });
  renderReviewBulk();
  renderReviewAuto();
  renderReviewArch();
  $("rv-sweep").disabled = review.busy || sweep.busy;
}

// The workers the hub removed on its own in the last day (every check was clean), each with an Undo that starts a
// fresh worker on the kept branch.
function renderReviewAuto() {
  const box = $("rv-auto");
  const rows = review.auto || [];
  const branches = review.branches || [];
  box.hidden = !rows.length && !branches.length;
  if (box.hidden) { box.innerHTML = ""; return; }
  const dis = review.busy ? " disabled" : "";
  const sessions = rows.map((a) => {
    const name = esc(a.name || a.entry_id);
    const host = multiHost() && a.host ? `<span class="host-chip">${esc(a.host)}</span>` : "";
    const branch = a.branch ? `<span class="wt-chip">⑂ ${esc(a.branch)}</span>` : "";
    const when = a.at ? esc(askAge(a.at)) : "";
    const how = [a.reason, a.landed].filter(Boolean).map(esc).join(" · ");
    const kept = !a.worktree_removed && a.kept ? `<div class="rv-sub">Kept the worktree: ${esc(a.kept)}</div>` : "";
    let act;
    if (a.undone_at) act = `<div class="rv-sub">Undone ${esc(askAge(a.undone_at))}${a.undo_note ? ": " + esc(a.undo_note) : ""}</div>`;
    else if (a.can_undo) act = `<div class="rv-actions"><button type="button" class="btn" data-act="autoundo" data-id="${esc(a.entry_id)}" aria-label="Undo the removal of ${name}"${dis}>Undo</button></div>`;
    else act = `<div class="rv-sub">Undo is not possible any more.</div>`;
    return `<div class="rv-item rv-auto-item" data-id="${esc(a.entry_id)}">
      <div class="rv-top"><span class="rv-name">${name}</span>${host}${branch}<span class="status">${when}</span></div>
      <div class="rv-reason">Removed automatically: ${how}</div>${a.in_recent ? '<div class="rv-sub">Its session is in Recently closed: resume it from there any time.</div>' : ""}${kept}${act}</div>`;
  }).join("");
  // Remote branches the sweep deleted, each with the SHA it was at, so it can be pushed back.
  const gone = branches.map((b) => `<div class="rv-item rv-auto-item rv-branch">
      <div class="rv-top"><span class="rv-name">origin/${esc(b.branch)}</span>${b.name ? `<span class="wt-chip">${esc(b.name)}</span>` : ""}<span class="status">${esc(askAge(b.at))}</span></div>
      <div class="rv-reason">Deleted by the sweep: every commit was already in the default branch and no pull request was open. It was at <code>${esc(b.tip)}</code>.</div>
      <div class="rv-sub">To restore: <code>${esc(b.restore)}</code></div></div>`).join("");
  box.innerHTML = (rows.length ? `<h3 class="rv-autoh">Removed automatically · last day</h3>${sessions}` : "") +
    (branches.length ? `<h3 class="rv-autoh">Remote branches deleted · last day</h3>${gone}` : "");
}

// The select-all bar: it counts the selection and removes the safe rows in it.
function reviewOpenIds() { return review.items.filter((it) => !reviewDone(it)).map((it) => it.id); }
function reviewBulkIds() {
  return review.items.filter((it) => review.sel.has(it.id) && it.safe && !it.delivered_discard && !it.coordinator && !reviewDone(it)).map((it) => it.id);
}

// Every row a removal would fully clean up: landed or merged, clean, nothing unpushed (it.safe), and not a coordinator.
function reviewSafeIds() {
  return review.items.filter((it) => it.safe && !it.delivered_discard && !it.coordinator && !reviewDone(it)).map((it) => it.id);
}

// Every row whose work is captured in Pages and that has nothing else against it: one confirmation discards them
// all. A plain removal would keep their worktrees, so they are not in "Remove all safe".
function reviewDeliveredIds() {
  return review.items.filter((it) => it.safe && it.delivered_discard && !it.coordinator && !reviewDone(it) && it.checks && it.checks.tip).map((it) => it.id);
}

function renderReviewBulk() {
  const bar = $("rv-bulk");
  const open = reviewOpenIds();
  bar.hidden = !open.length;
  if (!open.length) return;
  const sel = open.filter((id) => review.sel.has(id)).length;
  const safe = reviewBulkIds().length;
  const dis = review.busy ? " disabled" : "";
  const cf = review.confirm && review.confirm.kind === "bulk" ? review.confirm : null;
  const skipped = sel - safe;
  const allSafe = reviewSafeIds();
  const deliv = reviewDeliveredIds();
  const left = open.length - allSafe.length - deliv.length;
  const nm = (id) => { const it = review.items.find((x) => x.id === id); return esc((it && (it.name || it.pane_id)) || id); };
  const names = cf ? cf.ids.map((id) => `<li>${nm(id)}</li>`).join("") : "";
  const why = cf && cf.all
    ? (left ? ` ${left} not safe (uncommitted or unmerged work, or a coordinator) left alone, still in the list.` : "")
    : (skipped ? ` ${skipped} selected with work to keep or a coordinator are left for Remove anyway… in their rows.` : "");
  const dcf = cf && cf.discard;
  const dlist = dcf ? cf.ids.map((id) => review.items.find((x) => x.id === id)).filter(Boolean).map((it) =>
    `<li><b>${esc(it.name || it.pane_id || it.id)}</b>${it.branch ? ` <code>${esc(it.branch)}</code>` : ""}<br>Captured in ${deliveredLinks(it)}<br>Drops ${deliveredDrops(it)}</li>`).join("") : "";
  bar.innerHTML = `<label><input type="checkbox" id="rv-selall"${sel === open.length ? " checked" : ""}${dis}> Select all</label>
    <span class="rv-grow">${sel ? `${sel} selected` : `${open.length} ready`}</span>
    ${cf || !deliv.length ? "" : `<button type="button" class="btn danger" id="rv-all-deliv" data-act="bulk-deliv"${dis}>Discard all delivered (${deliv.length})</button>`}
    ${cf ? "" : `<button type="button" class="btn go" id="rv-all-safe" data-act="bulk-safe"${dis}${allSafe.length ? "" : " disabled"}>Remove all safe (${allSafe.length})</button>
    <button type="button" class="btn go" id="rv-all" data-act="bulk"${dis}${safe ? "" : " disabled"}>${safe ? `Remove selected (${safe})` : "Remove selected"}</button>`}
    ${!cf && left && allSafe.length ? `<div class="rv-note ln-note" style="flex-basis:100%">${left} not safe: left alone (use Remove anyway… in their rows).</div>` : ""}
    ${dcf ? `<div class="rv-confirm rv-dconfirm" role="group" aria-label="Confirm discard" style="flex-basis:100%"><p>Discard ${cf.ids.length} delivered session${cf.ids.length === 1 ? "" : "s"}? Each is closed, its worktree removed and its branch deleted. What they found is in the Pages named below; the files and commits listed are dropped for good. This cannot be undone.</p><ul class="rv-dlist">${dlist}</ul>
      <div class="rv-actions"><button type="button" class="btn danger" data-act="bulk-yes">Discard ${cf.ids.length}</button><button type="button" class="btn" data-act="bulk-no">Cancel</button></div></div>` : ""}
    ${cf && !dcf ? `<div class="rv-confirm" role="group" aria-label="Confirm" style="flex-basis:100%"><p>Remove the worktree${cf.ids.length === 1 ? "" : "s"} of ${cf.ids.length} session${cf.ids.length === 1 ? "" : "s"}? The branches stay.${why}</p><ul>${names}</ul>
      <div class="rv-actions"><button type="button" class="btn go" data-act="bulk-yes">Remove ${cf.ids.length}</button><button type="button" class="btn" data-act="bulk-no">Cancel</button></div></div>` : ""}`;
}

async function confirmRemovals(ids, allowWorking, coordinators = [], discard = {}) {
  if (review.busy || !ids.length) return;
  review.confirm = null;
  setReviewBusy(true, ids.length === 1 ? "Removing…" : `Removing ${ids.length} sessions…`);
  try {
    const d = await api("/api/removals/confirm", { method: "POST", body: JSON.stringify({ ids, allow_working: !!allowWorking, coordinators, discard }) });
    for (const r of d.results || []) { review.results.set(r.id, r); review.sel.delete(r.id); }
  } catch (e) {
    toast(e.message, true);
  }
  setReviewBusy(false);
  ledger.loadedAt = 0;
  loadLedger();
  refreshAgents().catch(() => {});
}

async function reviewAction(act, id, item) {
  if (review.busy) return;
  if (act === "reask") {
    // The old answer stays on the row until the session replies; the list reloads while it is asked.
    try {
      await api("/api/marks/reask", { method: "POST", body: JSON.stringify({ agent_id: id, item: Number(item) || 0 }) });
      toast("Asked the session again");
    } catch (e) {
      toast("Could not ask it again: " + e.message, true);
      return;
    }
    loadReview(true);
    return;
  }
  if (act === "cleanup") {
    try {
      await api("/api/marks/cleanup", { method: "POST", body: JSON.stringify({ agent_id: id }) });
      toast("Told the session to clean up; it will be asked what is left when it reports");
    } catch (e) {
      toast("Could not tell it to clean up: " + e.message, true);
      return;
    }
    loadReview(true);
    return;
  }
  if (act === "autoundo") {
    setReviewBusy(true, "Starting a worker on the branch again…");
    try {
      const d = await api("/api/removals/undo", { method: "POST", body: JSON.stringify({ entry_id: id }) });
      toast(d.note || "Undone");
    } catch (e) {
      toast("Could not undo: " + e.message, true);
    }
    setReviewBusy(false);
    refreshAgents().catch(() => {});
    loadReview(true);
    return;
  }
  if (act === "bulk") {
    const ids = reviewBulkIds();
    if (ids.length) { review.confirm = { ids, kind: "bulk" }; renderReview(); }
    return;
  }
  if (act === "bulk-safe") {
    const ids = reviewSafeIds();
    if (ids.length) { review.confirm = { ids, kind: "bulk", all: true }; renderReview(); }
    return;
  }
  if (act === "bulk-deliv") {
    const ids = reviewDeliveredIds();
    if (ids.length) { review.confirm = { ids, kind: "bulk", discard: true }; renderReview(); }
    return;
  }
  if (act === "bulk-no") { review.confirm = null; renderReview(); return; }
  if (act === "bulk-yes") {
    const cf = review.confirm;
    if (cf && cf.kind === "bulk" && cf.discard && cf.ids.length) {
      // Each item's own reviewed tip: a branch that moved since is refused by the host.
      const tips = {};
      for (const id of cf.ids) { const it = review.items.find((x) => x.id === id); if (it && it.checks && it.checks.tip) tips[id] = it.checks.tip; }
      confirmRemovals(cf.ids.filter((id) => tips[id]), false, [], tips);
    } else if (cf && cf.kind === "bulk" && cf.ids.length) confirmRemovals(cf.ids, false);
    return;
  }
  const it = review.items.find((x) => x.id === id);
  if (!it) return;
  if (act === "confirm-no") { review.confirm = null; renderReview(); return; }
  if (act === "remove" || act === "discard") {
    if (act === "discard") {
      const c = it.checks || {};
      if (!c.tip) { toast("Cannot discard: the worktree's tip is unknown", true); return; }
    }
    review.confirm = { ids: [id], kind: act };
    renderReview();
    return;
  }
  if (act === "confirm-yes") {
    const cf = review.confirm;
    if (!cf || cf.ids.length !== 1 || cf.ids[0] !== id) return;
    const working = reviewWorking(it);
    if (cf.kind === "discard") confirmRemovals([id], working, it.coordinator ? [id] : [], { [id]: (it.checks || {}).tip });
    else confirmRemovals([id], working, it.coordinator ? [id] : []);
    return;
  }
  if (act === "unmark") {
    // The row leaves the page now; it comes back if the request fails.
    const was = review.items.slice();
    review.items = review.items.filter((x) => x.id !== id);
    review.sel.delete(id);
    renderReview();
    try {
      await api("/api/marks/clear", { method: "POST", body: JSON.stringify({ agent_id: id }) });
    } catch (e) {
      if (review.open && !review.items.some((x) => x.id === id)) { review.items = was; renderReview(); }
      toast("Could not keep it: " + e.message, true);
      return;
    }
    ledger.loadedAt = 0;
    loadLedger();
  }
}

/* ---- the sweep, on the page ---- */

async function runSweep(dry) {
  const seq = ++sweep.seq;
  sweep.busy = true;
  sweep.dry = dry;
  sweep.shown = true;
  sweep.err = "";
  if (dry) { sweep.data = null; sweep.marked = false; }
  $("review").classList.add("busy");
  renderSweep();
  renderReview();
  try {
    const d = await api("/api/sweep", { method: "POST", body: JSON.stringify({ dry_run: dry }) });
    if (seq !== sweep.seq) return;
    sweep.data = d;
    sweep.marked = !dry;
  } catch (e) {
    if (seq !== sweep.seq) return;
    sweep.err = e.message;
    sweep.data = null;
  }
  sweep.busy = false;
  $("review").classList.toggle("busy", review.busy);
  renderSweep();
  renderReview();
  if (!dry) { ledger.loadedAt = 0; loadLedger(); loadReview(true); }
}

function renderSweep() {
  const box = $("rv-sweepbox");
  box.hidden = !sweep.shown;
  if (!sweep.shown) { box.innerHTML = ""; return; }
  if (sweep.busy) {
    box.innerHTML = `<div class="sw-box"><div class="sw-row"><span class="ln-status" style="display:inline">${sweep.dry === false ? "Marking…" : "Checking each finished worker on its host…"}</span></div></div>`;
    return;
  }
  const d = sweep.data;
  if (!d) { box.innerHTML = `<div class="sw-box"><div class="ln-empty">${esc(sweep.err || "The sweep did not run.")}</div><div class="sw-row"><button type="button" class="btn" data-act="sw-dismiss">Dismiss</button></div></div>`; return; }
  const item = (it) => `<div class="rv-item" data-sweep="${esc(it.id)}">
      <div class="rv-top"><span class="rv-name">${esc(it.name || it.id)}</span>${it.host ? `<span class="status">${esc(it.host)}</span>` : ""}${it.ad_hoc ? `<span class="status" title="started by hand: no live handoff">no handoff</span>` : ""}</div>
      ${it.branch ? `<div class="rv-sub">${esc(it.branch)}</div>` : ""}
      <div class="rv-reason">${esc(it.reason)}</div></div>`;
  const ready = d.ready || [], look = d.needs_look || [], skipped = d.skipped || [];
  const delivered = d.delivered || [], gone = d.gone || [];
  // Gone rows have nothing to lose: a plain sweep marks them with the ready ones.
  const toMark = ready.length + gone.length;
  let h = `<div class="sw-box"><div class="sw-row"><b class="rv-grow">${toMark || look.length || delivered.length ? "Finished agents" : "Sweep"}</b><button type="button" class="btn" data-act="sw-dismiss">${sweep.marked ? "Dismiss" : "Not now"}</button></div>`;
  if (!toMark && !look.length && !delivered.length) h += `<div class="ln-empty">No finished workers to sweep.</div>`;
  if (ready.length) h += `<div class="sw-h">${sweep.marked ? "Marked ready to remove" : "Ready to remove"} (${ready.length})</div>` + ready.map(item).join("");
  if (gone.length) h += `<div class="sw-h">Gone: nothing left to lose or check, ${sweep.marked ? "marked" : "marked with the ready ones"} (${gone.length})</div>` + gone.map(item).join("");
  // Delivered rows hold unpushed commits: never marked by the sweep itself,
  // only by this button, after the user has read them.
  if (delivered.length) h += `<div class="sw-h">Work delivered in the report; branch has local commits (${delivered.length})</div>` + delivered.map(item).join("") +
    `<div class="sw-row"><button type="button" class="btn" data-act="sw-delivered">Mark for discard: ${delivered.length}</button></div>`;
  if (look.length) h += `<div class="sw-h">Needs a look, not marked (${look.length})</div>` + look.map(item).join("");
  if (skipped.length) h += `<div class="ln-note">${skipped.length} skipped (already marked, coordinators, primary checkouts).</div>`;
  if (sweep.marked) h += `<div class="sw-done">${toMark ? `Marked ${toMark} ready to remove. They are in the list below.` : "Nothing was marked."}</div>`;
  else if (toMark) h += `<div class="sw-row" style="margin-top:8px"><button type="button" class="btn go" id="sw-mark" data-act="sw-mark">Mark ${toMark} ready to remove</button></div>`;
  box.innerHTML = h + "</div>";
}

function sweepAction(act) {
  if (act === "sw-dismiss") { sweep.seq++; Object.assign(sweep, { shown: false, busy: false, data: null, marked: false }); renderSweep(); renderReview(); return; }
  if (act === "sw-mark" && !sweep.busy) runSweep(false);
  if (act === "sw-delivered") sweepMarkDelivered();
}

// sweepMarkDelivered marks every row of the delivered group for discard, one
// mark each (what `comebye mark --discard` does). Removal stays the list's:
// the row asks the user to confirm the tip.
async function sweepMarkDelivered() {
  const items = (sweep.data && sweep.data.delivered) || [];
  if (sweep.busy || !items.length) return;
  const seq = sweep.seq;
  sweep.busy = true;
  $("review").classList.add("busy");
  renderSweep();
  for (const it of items) {
    try {
      await api("/api/marks", { method: "POST", body: JSON.stringify({ agent_id: it.id, reason: "Sweep: " + it.reason, discard: true }) });
    } catch (e) { /* the refreshed sweep still lists it */ }
  }
  sweep.busy = false;
  $("review").classList.toggle("busy", review.busy);
  if (seq !== sweep.seq) return;
  ledger.loadedAt = 0; loadLedger(); loadReview(true);
  runSweep(true);
}

/* ---------- detail view ---------- */

function agentById(id) {
  return state.agents.find((a) => agentId(a) === id);
}

/* ---------- transcripts: search and read archived sessions ---------- */

// #/search?q=...               the Search page: the newest sessions until there is a query
// #/search/<host>/<kind>/<id>?seq=N&sub=S&q=...   one session, at a match
// (#/transcripts and #/session/... are the old forms and redirect.)
// On the desktop it is a page beside the agent list, results and session side by
// side (stacked in a narrow pane); on the phone it covers the app.
// All text comes from agents and tools: snippets and output go in as text nodes,
// messages through renderMarkdown. Filters may be remembered in localStorage;
// queries and transcript text never are.
const RC_FILTERS_KEY = "comebye.recall.filters";
const RC_PAGE = 100;
const rc = {
  q: "", filters: {}, res: null, loading: false, facets: null, status: null, err: "",
  ctl: null, timer: null, more: false, route: null,
  sess: null, sessKey: "", msgs: [], before: false, after: false, sessErr: "", sessLoading: false,
  hits: [], hitAt: -1, open: new Set(), prod: new Map(),
  scope: {}, scopeKey: "", thread: null, threadErr: "",
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
  // #/transcripts and #/session/... are the old forms of these two: saved links and
  // notifications still carry them, and syncRecall rewrites them.
  // agent= and ledger= narrow the list to one agent's or one handoff's sessions.
  if (path === "/search" || path === "/transcripts") return { q: p.get("q") || "", agent: p.get("agent") || "", ledger: p.get("ledger") || "", legacy: path === "/transcripts" };
  // #/search/for?ledger=<handoff> or ?agent=<id> is a link from another screen: it goes to the
  // one session that work made, or to its thread when there are several.
  if (path === "/search/for") {
    const link = { agent: p.get("agent") || "", ledger: p.get("ledger") || "" };
    return link.agent || link.ledger ? { q: "", link } : null;
  }
  const tm = path.match(/^\/search\/thread\/(t-[0-9a-f]{8})$/);
  if (tm) return { q: p.get("q") || "", thread: tm[1] };
  const m = path.match(/^\/(search|session)\/([^/]+)\/([^/]+)\/([^/]+)$/);
  if (!m) return null;
  try {
    const seq = p.get("seq");
    return { q: p.get("q") || "", legacy: m[1] === "session", session: { host: decodeURIComponent(m[2]), kind: decodeURIComponent(m[3]), id: decodeURIComponent(m[4]) },
      seq: seq !== null && /^-?\d+$/.test(seq) ? Number(seq) : null, sub: p.get("sub") || "" };
  } catch { return null; }
}

function rcSearchHash(q, scope = rc.scope) {
  const parts = [];
  if (q) parts.push("q=" + encodeURIComponent(q));
  if (scope && scope.agent) parts.push("agent=" + encodeURIComponent(scope.agent));
  if (scope && scope.ledger) parts.push("ledger=" + encodeURIComponent(scope.ledger));
  return "#/search" + (parts.length ? "?" + parts.join("&") : "");
}
function rcThreadHash(id, q) { return "#/search/thread/" + encodeURIComponent(id) + (q ? "?q=" + encodeURIComponent(q) : ""); }
// rcWorkHash is the link to an agent's transcript. An agent id is reused for later work, so the
// agent's latest handoff is the better key when the ledger knows it.
function rcWorkHash(agentId) {
  const mine = (ledger.entries || []).filter((e) => e.agent_id === agentId).sort((a, b) => String(b.created_at).localeCompare(String(a.created_at)));
  return "#/search/for?" + (mine.length ? "ledger=" + encodeURIComponent(mine[0].id) : "agent=" + encodeURIComponent(agentId));
}
const rcDetail = (r) => !!(r && (r.session || r.thread || r.link));
function rcSessionHash(s, o = {}) {
  const p = new URLSearchParams();
  if (o.seq !== undefined && o.seq !== null) p.set("seq", o.seq);
  if (o.sub) p.set("sub", o.sub);
  if (o.q) p.set("q", o.q);
  const qs = p.toString();
  return `#/search/${encodeURIComponent(s.host)}/${encodeURIComponent(s.kind)}/${encodeURIComponent(s.id)}${qs ? "?" + qs : ""}`;
}

// syncRecall follows the hash: route() calls it.
function syncRecall() {
  let r = parseRecallRoute();
  if (r && r.legacy) {
    const h = r.session ? rcSessionHash(r.session, { seq: r.seq, sub: r.sub, q: r.q }) : rcSearchHash(r.q, {});
    history.replaceState(history.state, "", location.pathname + location.search + h);
    r = parseRecallRoute();
  }
  const el = $("recall");
  const was = !el.hidden;
  rc.route = r;
  el.hidden = !r;
  document.body.classList.toggle("has-recall", !!r);
  if (was !== !!r) renderNav();
  if (!r) {
    if (rc.ctl) rc.ctl.abort();
    rc.sess = null; rc.sessKey = ""; rc.msgs = []; rc.hits = [];
    return;
  }
  const detail = rcDetail(r);
  el.classList.toggle("in-session", detail);
  // The list keeps the narrowing of the last list route while a session or thread is read.
  if (!detail) rc.scope = { agent: r.agent || "", ledger: r.ledger || "" };
  else if (!was) rc.scope = {};
  const scopeKey = (rc.scope.agent || "") + "|" + (rc.scope.ledger || "");
  if (!was) {
    rc.filters = rcLoadFilters();
    rcLoadMeta();
  }
  if (!was || scopeKey !== rc.scopeKey) renderRecallFilters();
  const input = $("rc-q");
  if (document.activeElement !== input || !was) input.value = r.q;
  // Opened afresh with no query, the list of recent sessions may have moved on.
  if (r.q !== rc.q || !rc.res || (!was && !r.q) || scopeKey !== rc.scopeKey) {
    rc.q = r.q;
    rc.scopeKey = scopeKey;
    rcSearch();
  }
  if (r.session) rcOpenSession(r);
  else if (r.thread) rcOpenThread(r);
  else if (r.link) rcOpenLink(r);
  else { $("rc-session").replaceChildren(rcEmptySession()); rc.sessKey = ""; }
  if (!was && !detail && isDesktop()) input.focus();
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
  const scope = [];
  if (rc.scope.agent || rc.scope.ledger) {
    const b = document.createElement("button");
    b.type = "button"; b.className = "rc-chip on"; b.id = "rc-unscope";
    b.textContent = (rc.scope.ledger ? "This handoff only" : "This agent only") + " ×";
    b.title = "Show every session again";
    scope.push(b);
  }
  $("rc-filters").replaceChildren(
    ...scope,
    rcSelect("role", "Where the words are", Object.entries(RC_IN)),
    rcSelect("when", "When", Object.entries(RC_WHEN)),
    rcSelect("host", "Host", opt("Any host", f.hosts)),
    rcSelect("kind", "Agent", opt("Any agent", f.kinds)),
    rcSelect("project", "Project", opt("Any project", f.projects)),
    rcSelect("sort", "Order", Object.entries(RC_SORT)),
  );
  rcSyncFilters();
}

// rcWork says whether the list shows threads (Work) rather than messages.
const rcWork = () => !!rc.q && rc.filters.mode === "work";

// Where the words are, and best match, mean nothing until there are words, and a thread has neither.
function rcSyncFilters() {
  for (const s of $("rc-filters").querySelectorAll('[data-key="role"], [data-key="sort"]')) s.hidden = !rc.q || rcWork();
  const tabs = $("rc-tabs");
  tabs.hidden = !rc.q;
  for (const b of tabs.querySelectorAll("button")) b.setAttribute("aria-selected", String((b.dataset.mode === "work") === rcWork()));
}

function rcParams(skip) {
  const p = new URLSearchParams({ limit: "20" });
  if (rc.q) p.set("q", rc.q);
  if (rcWork()) p.set("mode", "work");
  const f = rc.filters;
  for (const k of rc.q ? (rcWork() ? ["host", "kind", "project"] : ["role", "host", "kind", "project", "sort"]) : ["host", "kind", "project"]) if (f[k]) p.set(k, f[k]);
  if (rc.scope.agent) p.set("agent", rc.scope.agent);
  if (rc.scope.ledger) p.set("ledger", rc.scope.ledger);
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
    const have = more ? (rc.res.threads || rc.res.hits).length : 0;
    const d = await api((rc.q ? "/api/recall/search?" : "/api/recall/recent?") + rcParams(have), { signal: ctl.signal });
    if (rc.ctl !== ctl) return;
    d.recent = !rc.q;
    if (more) {
      if (d.threads) rc.res.threads.push(...d.threads); else rc.res.hits.push(...d.hits);
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

// What a session made, as the words a chip says.
const RC_PRODUCED = { commit: ["commit", "commits"], page: ["Page", "Pages"], pr: ["PR", "PRs"], issue: ["issue", "issues"], comment: ["comment", "comments"], release: ["release", "releases"], image: ["image", "images"] };
const RC_PRODUCED_ORDER = ["page", "pr", "issue", "commit", "release", "image", "comment"];

function rcProducedLabel(type, n) {
  const w = RC_PRODUCED[type];
  return w ? (n === undefined ? w[0] : `${n} ${w[n === 1 ? 0 : 1]}`) : String(type);
}

// rcProducedChips is the chip line of a result row: what the session made.
function rcProducedChips(counts) {
  const kinds = RC_PRODUCED_ORDER.filter((t) => counts && counts[t]);
  if (!kinds.length) return null;
  const d = document.createElement("div");
  d.className = "rc-foot rc-made";
  for (const t of kinds) d.append(rcTag(rcProducedLabel(t, counts[t]), "t-purple"));
  return d;
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
  const made = rcProducedChips(h.produced);
  if (made) a.append(made);
  return a;
}

// rcProducedCard is an artifact a query named, with the session and turn that made it.
function rcProducedCard(it) {
  const a = document.createElement("a");
  a.className = "rc-card rc-prod";
  a.href = rcSessionHash(it.session, { seq: it.seq, sub: it.sub });
  a.dataset.key = `${it.session.host}/${it.session.kind}/${it.session.id}`;
  const title = document.createElement("div");
  title.className = "rc-title";
  const ref = it.type === "commit" ? it.ref.slice(0, 12) : it.ref;
  title.append(rcTag(rcProducedLabel(it.type), "t-purple"), document.createTextNode(" " + ref));
  if (it.type === "commit" && it.rel && it.rel !== "made") title.append(document.createTextNode(` (${it.rel})`));
  a.append(title);
  if (it.title) {
    const t = document.createElement("div");
    t.className = "rc-snip";
    t.textContent = it.title;
    a.append(t);
  }
  const by = document.createElement("div");
  by.className = "rc-meta";
  by.textContent = `by ${it.session.title || it.session.id} · turn ${it.seq} · ${rcMetaLine(it.session)}`;
  a.append(by);
  const foot = document.createElement("div");
  foot.className = "rc-meta rc-foot";
  if (it.type === "commit" && it.verdict && it.verdict !== "unchecked") {
    const v = it.verdict === "landed" ? "landed" + (it.landed_as ? " as " + it.landed_as.slice(0, 8) : "") : it.verdict === "notinmain" ? "not on main" : it.verdict;
    foot.append(rcTag(v, it.verdict === "landed" ? "t-green" : it.verdict === "notinmain" ? "t-orange" : "t-muted"));
  }
  if (it.pr) foot.append(rcTag("PR " + it.pr, "t-blue"));
  const open = document.createElement("span");
  open.className = "rc-openat";
  open.textContent = `Open at turn ${it.seq} ▸`;
  foot.append(open);
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
    if (rc.res.threads) count.textContent = rc.res.total === 1 ? "1 piece of work" : `${rc.res.total} pieces of work`;
    const made = rc.res.produced || [];
    if (made.length) {
      const sec = document.createElement("div");
      sec.className = "rc-sec"; sec.id = "rc-produced";
      sec.textContent = "Produced";
      kids.push(sec);
      for (const it of made) kids.push(rcProducedCard(it));
      if (rc.res.hits.length) {
        const h2 = document.createElement("div");
        h2.className = "rc-sec";
        h2.textContent = "Sessions that mention it";
        kids.push(h2);
      }
    } else for (const c of rc.res.commits || []) kids.push(rcCommitCard(c));
    for (const t of rc.res.threads || []) kids.push(rcThreadCard(t));
    for (const h of rc.res.hits || []) kids.push(rcCard(h));
    if (!rc.res.total && !(rc.res.commits || []).length && !made.length) kids.push(rcNotice(rc.res.recent ? "No sessions match these filters." : rc.res.threads ? "No piece of work is about those words. Work looks for any of the topic words, in a brief, a report or the messages; Messages needs all of them." : "Nothing matches. Filters narrow this too, and only the start and end of long output is indexed."));
    if ((rc.res.threads || rc.res.hits).length < rc.res.total) {
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
  const key = r && r.session ? `${r.session.host}/${r.session.kind}/${r.session.id}` : r && r.thread ? "thread/" + r.thread : "";
  for (const c of document.querySelectorAll("#rc-results .rc-card")) c.classList.toggle("current", !!key && c.dataset.key === key);
}

// rcSpan says how long a piece of work took: "2 h 10 min".
function rcSpan(a, b) {
  const ms = Date.parse(b || "") - Date.parse(a || "");
  if (!(ms > 60000)) return "";
  const m = Math.round(ms / 60000);
  if (m < 60) return `${m} min`;
  if (m < 2880) return `${Math.floor(m / 60)} h${m % 60 ? ` ${m % 60} min` : ""}`;
  return `${Math.round(m / 1440)} days`;
}

// rcThreadCard is one piece of work in the Work tab.
function rcThreadCard(t) {
  const a = document.createElement("a");
  a.className = "rc-card rc-thread";
  a.href = rcThreadHash(t.id, rc.q);
  a.dataset.key = "thread/" + t.id;
  const title = document.createElement("div");
  title.className = "rc-title";
  title.textContent = t.title || t.id;
  const meta = document.createElement("div");
  meta.className = "rc-meta";
  meta.textContent = [t.hosts, t.branches, rcDate(t.started)].filter(Boolean).join(" · ");
  a.append(title, meta);
  const text = t.outcome || t.brief;
  if (text) {
    const sn = document.createElement("div");
    sn.className = "rc-snip";
    sn.textContent = text;
    a.append(sn);
  }
  const foot = document.createElement("div");
  foot.className = "rc-meta rc-foot";
  foot.append(document.createTextNode(`${t.sessions} ${t.sessions === 1 ? "session" : "sessions"}`));
  const v = rcVerdictTag(t.verdict);
  if (v) foot.append(v);
  a.append(foot);
  return a;
}

/* thread page and links */

// How an outcome's kind reads as a chip. Every outcome also says where it came from.
const RC_OUTCOME = {
  report: ["report", "t-blue"], "commit-landed": ["landed", "t-green"], "commit-notinmain": ["not on main", "t-orange"],
  "commit-unknown": ["commit", "t-muted"], "commit-unchecked": ["commit", "t-muted"], "pr-merged": ["PR merged", "t-green"],
  "pr-open": ["PR open", "t-blue"], "pr-closed": ["PR closed", "t-muted"], doc: ["Page", "t-purple"], deploy: ["deploy", "t-blue"],
  decision: ["decision", "t-blue"], abandoned: ["abandoned", "t-muted"], unmerged: ["unmerged", "t-orange"], handoff: ["handoff", "t-muted"],
};

async function rcOpenThread(r) {
  rcMarkCurrent();
  const key = "thread|" + r.thread;
  if (key === rc.sessKey) return;
  rc.sessKey = key;
  rc.sess = null; rc.msgs = []; rc.hits = []; rc.thread = null; rc.threadErr = "";
  $("rc-session").replaceChildren(rcNotice("Loading…"));
  try {
    const d = await api("/api/recall/threads/" + encodeURIComponent(r.thread));
    if (rc.sessKey !== key) return;
    rc.thread = d;
  } catch (e) {
    if (rc.sessKey !== key) return;
    rc.threadErr = e.code === "no_thread" ? "This piece of work isn't in the index. It was rebuilt since the link was made, or it hasn't been indexed yet." : (e.message || "the thread could not be read");
  }
  renderRecallThread();
}

function renderRecallThread() {
  const box = $("rc-session");
  const r = rc.route;
  if (!r || !r.thread) return;
  if (rc.threadErr) { box.replaceChildren(rcNotice(rc.threadErr)); return; }
  const t = rc.thread;
  if (!t) return;
  const body = document.createElement("div");
  body.className = "rc-sbody rc-threadpage";
  const title = document.createElement("h2");
  title.className = "rc-stitle"; title.textContent = t.title || t.id;
  const meta = document.createElement("div");
  meta.className = "rc-meta";
  meta.textContent = [t.hosts, t.branches, rcDate(t.started), rcSpan(t.started, t.ended)].filter(Boolean).join(" · ");
  const v = rcVerdictTag(t.verdict);
  if (v) { meta.append(" "); meta.append(v); }
  body.append(title, meta);
  const sec = (name) => {
    const h = document.createElement("div");
    h.className = "rc-sec"; h.textContent = name;
    body.append(h);
  };
  if (t.brief) {
    sec("Brief · from the handoff");
    const md = document.createElement("div");
    md.className = "md rc-brief";
    md.innerHTML = renderMarkdown(t.brief, "rcb" + t.id);
    body.append(md);
  }
  const outs = t.outcomes || [];
  if (outs.length) {
    sec("How it ended");
    const ul = document.createElement("ul");
    ul.className = "rc-outcomes";
    for (const o of outs) {
      const li = document.createElement("li");
      const [label, cls] = RC_OUTCOME[o.kind] || [o.kind, "t-muted"];
      const head = document.createElement("div");
      head.className = "rc-ohead";
      head.append(rcTag(label, cls));
      if (o.ref) {
        const c = document.createElement("code");
        c.textContent = /^[0-9a-f]{40}$/.test(o.ref) ? o.ref.slice(0, 12) : o.ref;
        head.append(" ", c);
      }
      li.append(head);
      if (o.kind === "report" && o.text) {
        const d = document.createElement("details");
        d.className = "rc-report";
        const sm = document.createElement("summary");
        sm.textContent = "Final report";
        const md = document.createElement("div");
        md.className = "md";
        md.innerHTML = renderMarkdown(o.text, "rco" + t.id);
        d.append(sm, md);
        li.append(d);
      } else if (o.text) {
        const x = document.createElement("div");
        x.className = "rc-otext"; x.textContent = o.text;
        li.append(x);
      }
      if (o.source) {
        const src = document.createElement("div");
        src.className = "rc-meta";
        src.textContent = "source: " + o.source;
        li.append(src);
      }
      ul.append(li);
    }
    body.append(ul);
  } else if (t.outcome) {
    sec("How it ended");
    const x = document.createElement("div");
    x.className = "rc-otext"; x.textContent = t.outcome;
    body.append(x);
  }
  const sessions = t.sessions || [];
  sec(sessions.length === 1 ? "Session" : `Sessions (${sessions.length})`);
  for (const s of sessions) {
    const a = document.createElement("a");
    a.className = "rc-card";
    a.href = rcSessionHash(s, { q: r.q });
    const st = document.createElement("div");
    st.className = "rc-title"; st.textContent = s.title || s.id;
    const sm = document.createElement("div");
    sm.className = "rc-meta"; sm.textContent = [s.host, s.kind, rcWhen(s.started)].filter(Boolean).join(" · ");
    a.append(st, sm);
    body.append(a);
  }
  box.replaceChildren(body);
  box.scrollTop = 0;
}

// rcOpenLink follows a link from another screen: to the session that work made, to its thread when
// several did, and otherwise says it is not there yet. The link's own history entry is replaced.
async function rcOpenLink(r) {
  const key = "for|" + r.link.agent + "|" + r.link.ledger;
  if (key === rc.sessKey) return;
  rc.sessKey = key;
  rc.sess = null; rc.msgs = []; rc.hits = [];
  const box = $("rc-session");
  box.replaceChildren(rcNotice("Finding the transcript…"));
  let d;
  try {
    d = await api("/api/recall/sessions?" + (r.link.ledger ? "ledger=" + encodeURIComponent(r.link.ledger) : "agent=" + encodeURIComponent(r.link.agent)));
  } catch (e) {
    if (rc.sessKey !== key) return;
    box.replaceChildren(rcNotice(e.code === "no_index" ? "This host has no transcript index yet." : (e.message || "the transcript could not be looked up")));
    return;
  }
  if (rc.sessKey !== key) return;
  const list = d.sessions || [];
  let h = "";
  if (list.length === 1) h = rcSessionHash(list[0]);
  else if (list.length > 1 && d.thread_id) h = rcThreadHash(d.thread_id);
  else if (list.length > 1) h = rcSearchHash("", r.link);
  if (h) {
    history.replaceState(history.state, "", location.pathname + location.search + h);
    syncRecall();
    return;
  }
  const entry = r.link.ledger ? (ledger.entries || []).find((e) => e.id === r.link.ledger) : null;
  const live = agentById(r.link.agent || (entry && entry.agent_id) || "");
  const kids = [rcNotice("This work isn't in the transcript index yet. A session is indexed after it has been archived, which happens about every hour.")];
  if (live) {
    const open = document.createElement("button");
    open.type = "button"; open.className = "btn rc-openagent"; open.id = "rc-open";
    open.dataset.hash = agentHash(live);
    open.textContent = "Open the live agent";
    kids.push(open);
  }
  box.replaceChildren(...kids);
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
  rc.sess = null; rc.msgs = []; rc.hits = []; rc.hitAt = -1; rc.sessErr = ""; rc.open = new Set(); rc.prod = new Map();
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
  if (!rc.sessErr) rcLoadProduced(r, key);
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

// rcLoadProduced fetches what the session made and marks the turns that made it. The
// transcript reads fine without it: a failure leaves the turns as they were.
async function rcLoadProduced(r, key) {
  try {
    const d = await api(rcSessionURL(r) + "/produced");
    if (rc.sessKey !== key) return;
    const by = new Map();
    for (const it of d.produced || []) {
      if ((it.sub || "") !== r.sub || (it.type === "commit" && it.rel === "seen")) continue;
      if (!by.has(it.seq)) by.set(it.seq, []);
      by.get(it.seq).push(it);
    }
    rc.prod = by;
    renderRecallMessages();
    if (r.seq !== null && rc.current === r.seq) {
      const el = document.querySelector(`#rc-session [data-seq="${r.seq}"]`);
      if (el) el.scrollIntoView({ block: "center" });
    }
  } catch {}
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

// rcLiveAgent is the running agent that holds this session, if there is one.
function rcLiveAgent(s) {
  return state.agents.find((a) => {
    const v = a.agent_session && a.agent_session.value;
    return v && String(v).includes(s.id) && (a.host || state.localHost) === s.host;
  });
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
  const live = rcLiveAgent(s);
  if (live) {
    const open = document.createElement("button");
    open.type = "button"; open.className = "btn rc-openagent"; open.id = "rc-open";
    open.dataset.hash = agentHash(live);
    open.textContent = "Open agent";
    body.append(open);
  }
  if (rc.sess.subs.length || s.thread_id) {
    const subs = document.createElement("nav");
    subs.className = "rc-subs"; subs.setAttribute("aria-label", rc.sess.subs.length ? "Subagents and thread" : "Thread");
    const link = (label, sub) => {
      const a = document.createElement("a");
      a.href = rcSessionHash(s, { sub, q: r.q });
      a.textContent = label;
      a.className = "rc-chip" + (sub === r.sub ? " on" : "");
      return a;
    };
    if (s.thread_id) {
      const t = document.createElement("a");
      t.href = rcThreadHash(s.thread_id, r.q);
      t.className = "rc-chip"; t.id = "rc-thread-link";
      t.textContent = "The whole piece of work ▸";
      subs.append(t);
    }
    if (rc.sess.subs.length) subs.append(link("Main thread", ""));
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
  const made = [call, result].filter(Boolean).flatMap((m) => rc.prod.get(m.seq) || []);
  if (!made.length) return det;
  const wrap = document.createElement("div");
  wrap.append(det);
  const line = document.createElement("div");
  line.className = "rc-made rc-madeline";
  for (const it of made) line.append(rcMadeChip(it));
  wrap.append(line);
  return wrap;
}

// rcMadeChip is what a turn made: a Page opens in Pages, a PR or issue on GitHub (by the
// URL its own output gave); a commit says where it landed.
function rcMadeChip(it) {
  let el;
  if (it.type === "page") {
    el = document.createElement("a");
    el.href = pageHash(it.ref);
  } else if (it.url && /^https:\/\/github\.com\//.test(it.url)) {
    el = document.createElement("a");
    el.href = it.url; el.target = "_blank"; el.rel = "noopener noreferrer";
  } else el = document.createElement("span");
  el.className = "rc-pchip";
  let t = `${rcProducedLabel(it.type)} ${it.type === "commit" ? it.ref.slice(0, 8) : it.ref}`;
  if (it.type === "page" && it.detail) t += ` · ${it.detail}`;
  if (it.type === "commit" && it.verdict && it.verdict !== "unchecked") t += it.verdict === "landed" ? ` · landed${it.landed_as ? " as " + it.landed_as.slice(0, 8) : ""}` : it.verdict === "notinmain" ? " · not on main" : ` · ${it.verdict}`;
  if (it.type === "commit" && it.pr) t += ` · ${it.pr}`;
  el.textContent = t;
  return el;
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
function openRecall() { location.hash = rcSearchHash(rc.q, {}); }

function closeRecall() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

function rcBack() {
  const r = rc.route;
  if (rcDetail(r) && !isDesktop()) location.hash = rcSearchHash(r.q);
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
      if (!rcDetail(r)) history.replaceState(null, "", location.pathname + location.search + rcSearchHash(rc.q));
      rcSearch();
    }, 300);
  });
  $("rc-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    clearTimeout(rc.timer);
    const v = $("rc-q").value.trim();
    if (rcDetail(rc.route) && !isDesktop()) { location.hash = rcSearchHash(v); return; }
    if (v !== rc.q) { rc.q = v; history.replaceState(null, "", location.pathname + location.search + rcSearchHash(rc.q)); rcSearch(); }
    $("rc-q").blur();
  });
  $("rc-filters").addEventListener("click", (ev) => {
    if (ev.target.closest("#rc-unscope")) location.hash = rcSearchHash(rc.q, {});
  });
  $("rc-tabs").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-mode]");
    if (!b || (b.dataset.mode === "work") === rcWork()) return;
    if (b.dataset.mode === "work") rc.filters.mode = "work"; else delete rc.filters.mode;
    rcSaveFilters();
    rcSearch();
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
  // Esc clears the query; with nothing to clear it does nothing (Back is the way out).
  $("recall").addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape" || ev.defaultPrevented) return;
    const q = $("rc-q");
    ev.preventDefault();
    if (ev.target !== q || !q.value) return;
    ev.stopPropagation();
    clearTimeout(rc.timer);
    q.value = "";
    rc.q = "";
    if (rc.route && !rcDetail(rc.route)) history.replaceState(null, "", location.pathname + location.search + rcSearchHash(""));
    rcSearch();
  });
  $("rc-session").addEventListener("click", (ev) => {
    const b = ev.target.closest("#rc-open");
    if (b) location.hash = b.dataset.hash;
  });
}

/* ---------- Pages: reports agents publish (comebye publish) ---------- */

// #/pages                  the list
// #/pages/<id>[/<n>]       one page (version n, default the newest)
// A page is code an agent wrote, so it is only ever shown in an iframe with
// sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox" (no allow-same-origin): its scripts run, but in an
// opaque origin that cannot read this app, its API, cookies or storage, and
// the server's Content-Security-Policy on /p/ forbids fetches and forms, leaves popups to a
// clicked link (a new tab, which is not this app), and forbids navigating this window. Everything about it that this screen
// prints (title, publisher) is agent text, escaped.
const PAGE_ID_RE = /^pg_[0-9a-f]{10}$/;
const pgs = { items: [], loaded: false, err: "", q: "", route: null, timer: null, ctl: null, publicUrl: "", pagesUrl: "", frameKey: "", frameVer: 0, focus: false, frameFollow: false, reload: false, made: new Map(), body: null, bodyTimer: null, bodySeq: 0,
  view: lsGet("comebye.pgs.view") === "list" || (lsGet("comebye.pgs.view") !== "grid" && matchMedia("(max-width: 699px)").matches) ? "list" : "grid", sort: ["updated", "opened", "title"].includes(lsGet("comebye.pgs.sort")) ? lsGet("comebye.pgs.sort") : "updated",
  chip: "all", opened: null, prev: new Map(), prevQ: [], prevBusy: 0, io: null, prevRaf: 0 };

// What the user has opened, per device: id -> { v: newest version seen, at: ms }.
// The hub never learns it. The first time Pages loads, every page already there
// counts as seen, so only pages published or republished after that carry the
// "new" / "updated" dot. Capped at 500 ids; storage that throws is only a lost memory.
const PGS_OPENED_KEY = "comebye.pgs.opened";
function pgsOpenedInit() {
  if (pgs.opened) return;
  let o = null;
  try { o = JSON.parse(lsGet(PGS_OPENED_KEY)); } catch {}
  if (o && typeof o === "object" && !Array.isArray(o)) { pgs.opened = o; return; }
  pgs.opened = {};
  for (const p of pgs.items) pgs.opened[p.id] = { v: p.version, at: 0 };
  pgsOpenedSave();
}
function pgsOpenedSave() {
  const ids = Object.keys(pgs.opened);
  if (ids.length > 500) {
    ids.sort((x, y) => (pgs.opened[y].at || 0) - (pgs.opened[x].at || 0));
    pgs.opened = Object.fromEntries(ids.slice(0, 500).map((id) => [id, pgs.opened[id]]));
  }
  lsSet(PGS_OPENED_KEY, JSON.stringify(pgs.opened));
}
// pgsMarkOpened: the newest version of a page was shown (an older version does not count).
function pgsMarkOpened(p) {
  pgsOpenedInit();
  const o = pgs.opened[p.id];
  if (o && o.v >= p.version && o.at) return;
  pgs.opened[p.id] = { v: p.version, at: Date.now() };
  pgsOpenedSave();
}
// pgsDot: "new" (never opened), "updated" (a newer version than the one opened), or "".
function pgsDot(p) {
  const o = pgs.opened && pgs.opened[p.id];
  if (!pgs.opened) return "";
  return !o ? "new" : o.v < p.version ? "updated" : "";
}

// The preview of a page: its skeleton (title is the card's own), made by the hub
// once per version and fetched when the card scrolls into view. The text is the
// page's own, so it is escaped here like every other agent text. Keyed by
// id@version: a republish is a new key, and the old one is never asked again.
function pgsSkelHtml(sk) {
  if (!sk || sk === "loading") return "";
  if (sk === "none" || (!sk.lead && !(sk.heads || []).length && !(sk.flags || []).length)) return `<span class="pgs-nopv">No preview</span>`;
  return `<span class="pgs-sk">${sk.lead ? `<span class="pgs-lead">${esc(sk.lead)}</span>` : ""}${(sk.heads || []).length ? `<span class="pgs-heads">${sk.heads.slice(0, 3).map((h) => `<span>${esc(h)}</span>`).join("")}</span>` : ""}${(sk.flags || []).length ? `<span class="pgs-flags">${sk.flags.map((f) => `<span>${esc(f)}</span>`).join("")}</span>` : ""}</span>`;
}
// The card's html holds only an empty thumb: pgsFill puts the skeleton in after a draw, and, for a version that has
// one, the thumbnail the publisher's Chrome made (an <img> from the hub, one per scheme; the one for the current
// theme shows). A skeleton or an image arriving never redraws the list.
const pgsScheme = () => { const t = window.ComebyeThemes && ComebyeThemes.byId[document.documentElement.getAttribute("data-theme")]; return t && t.mode === "dark" ? "dark" : "light"; };
function pgsThumbs(p) {
  const v = (p.versions || []).find((x) => x.n === p.version);
  return v && v.thumbs ? v.thumbs : [];
}
function pgsThumb(p) {
  return `<span class="pgs-thumb empty" data-prev="${esc(p.id + "@" + p.version)}" data-th="${esc(pgsThumbs(p).join(" "))}" aria-hidden="true"><span class="pgs-skw"></span></span>`;
}
function pgsThumbSrc(key, scheme) {
  const [id, v] = key.split("@");
  return "/api/pages/" + encodeURIComponent(id) + "/thumb?v=" + encodeURIComponent(v) + "&scheme=" + scheme;
}
// pgsImgs: put the thumbnail of the current scheme in each thumb that has one (the other scheme's if only one was made);
// an image that fails to load goes, and the skeleton shows.
function pgsImgs() {
  const sc = pgsScheme();
  for (const el of document.querySelectorAll("#pgs-items .pgs-thumb[data-th]")) {
    const have = el.dataset.th.split(" ").filter(Boolean);
    let img = el.querySelector("img.pgs-img");
    if (!have.length || el._noimg === el.dataset.prev) { if (img) img.remove(); el.classList.remove("has-img"); continue; }
    const use = have.includes(sc) ? sc : have[0];
    if (!img) {
      img = document.createElement("img");
      img.className = "pgs-img";
      img.alt = "";
      img.decoding = "async";
      img.setAttribute("loading", "lazy");
      img.addEventListener("error", () => { el._noimg = el.dataset.prev; img.remove(); el.classList.remove("has-img"); });
      el.appendChild(img);
    }
    const src = pgsThumbSrc(el.dataset.prev, use);
    if (img.getAttribute("src") !== src) img.setAttribute("src", src);
    el.classList.add("has-img");
  }
}
function pgsFill() {
  for (const el of document.querySelectorAll("#pgs-items .pgs-thumb")) {
    const sk = pgs.prev.get(el.dataset.prev);
    const empty = !sk || sk === "loading" || sk === "none";
    el.classList.toggle("empty", empty);
    const h = pgsSkelHtml(sk);
    if (el._sk !== h) { el._sk = h; el.firstElementChild.innerHTML = h; }
  }
  pgsImgs();
}
function pgsWantPreview(key) {
  if (pgs.prev.has(key)) return;
  pgs.prev.set(key, "loading");
  pgs.prevQ.push(key);
  pgsPump();
}
function pgsPump() {
  while (pgs.prevBusy < 4 && pgs.prevQ.length) {
    const key = pgs.prevQ.shift(), [id, v] = key.split("@");
    pgs.prevBusy++;
    api("/api/pages/" + encodeURIComponent(id) + "/preview?v=" + encodeURIComponent(v))
      .then((r) => pgs.prev.set(key, r && r.preview ? r.preview : "none"))
      .catch(() => pgs.prev.set(key, "none"))
      .finally(() => {
        pgs.prevBusy--;
        if (!pgs.prevRaf) pgs.prevRaf = requestAnimationFrame(() => { pgs.prevRaf = 0; if (pgs.route) pgsFill(); });
        pgsPump();
      });
  }
}
// pgsWatch: ask for the preview of every card on screen (and a screenful beyond), and no other.
function pgsWatch() {
  const thumbs = document.querySelectorAll("#pgs-items .pgs-thumb");
  if (typeof IntersectionObserver !== "function") { thumbs.forEach((el) => pgsWantPreview(el.dataset.prev)); return; }
  if (!pgs.io) {
    pgs.io = new IntersectionObserver((es) => {
      for (const e of es) if (e.isIntersecting) { pgs.io.unobserve(e.target); pgsWantPreview(e.target.dataset.prev); }
    }, { root: $("pgs-list-pane"), rootMargin: "300px 0px" });
  }
  thumbs.forEach((el) => { if (!pgs.prev.has(el.dataset.prev)) pgs.io.observe(el); });
}

function parsePagesRoute() {
  const m = location.hash.match(/^#\/pages(?:\/([^/]+)(?:\/(\d+))?)?$/);
  if (!m) return null;
  if (m[1] === undefined) return { id: "", ver: 0 };
  return PAGE_ID_RE.test(m[1]) ? { id: m[1], ver: m[2] ? Number(m[2]) : 0 } : { id: "", ver: 0 };
}

function pageHash(id, ver) { return "#/pages/" + id + (ver ? "/" + ver : ""); }

// pageFromUrl: a link to one of this hub's pages (/p/<id>) goes to the
// Pages screen instead of a new tab.
function pageFromUrl(href) {
  let u;
  try { u = new URL(href, location.href); } catch { return null; }
  if (u.origin !== location.origin && !(pgs.publicUrl && u.origin === pgs.publicUrl) && !(pgs.pagesUrl && u.origin === pgs.pagesUrl)) return null;
  const m = u.pathname.match(/^\/p\/(pg_[0-9a-f]{10})(?:\/v?(\d+))?$/);
  return m ? { id: m[1], ver: m[2] ? Number(m[2]) : 0 } : null;
}

async function pagesLoad() {
  try {
    const r = await api("/api/pages");
    pgs.items = r.pages || [];
    try { pgs.publicUrl = r.public_url ? new URL(r.public_url).origin : ""; } catch { pgs.publicUrl = ""; }
    pgs.pagesUrl = r.pages_url || ""; // pages served from an origin of their own (-pages-listen), or ""
    pgs.err = "";
    pgsOpenedInit();
  } catch (e) {
    pgs.err = e.status === 503 ? "Pages are off on this hub." : e.message;
  }
  pgs.loaded = true;
  if (pgs.route) renderPages();
}

// The search box's has:script term (also what the Interactive chip toggles): only the pages that run JavaScript.
// pgsQuery splits it from the words, which match the list's metadata and, on the hub, the pages' text.
const PGS_SCRIPT = "has:script";
function pgsQuery(q) {
  const words = String(q || "").trim().split(/\s+/).filter(Boolean);
  const rest = words.filter((w) => w.toLowerCase() !== PGS_SCRIPT);
  return { script: rest.length !== words.length, text: rest.join(" ") };
}

function pagesFiltered() {
  const pq = pgsQuery(pgs.q), q = pq.text.toLowerCase();
  let a = pgs.items;
  if (pq.script) a = a.filter((p) => p.script);
  if (pgs.chip === "rev") a = a.filter((p) => p.version > 1);
  else if (pgs.chip !== "all") a = a.filter((p) => p.host === pgs.chip);
  if (q) a = a.filter((p) => [p.title, p.by, p.host, p.id, p.key, p.kind, p.handoff].some((v) => String(v || "").toLowerCase().includes(q)));
  const upd = (p) => Date.parse(p.updated_at) || 0;
  const at = (p) => (pgs.opened && pgs.opened[p.id] && pgs.opened[p.id].at) || 0;
  if (pgs.sort === "title") return a.slice().sort((x, y) => String(x.title).localeCompare(String(y.title)) || upd(y) - upd(x));
  if (pgs.sort === "opened") return a.slice().sort((x, y) => at(y) - at(x) || upd(y) - upd(x));
  return a.slice().sort((x, y) => upd(y) - upd(x));
}

// The filter chips: all, the pages with more than one version, and one per host.
function pagesChips() {
  const hosts = [...new Set(pgs.items.map((p) => p.host).filter(Boolean))].sort();
  if (pgs.chip !== "all" && pgs.chip !== "rev" && !hosts.includes(pgs.chip)) pgs.chip = "all";
  const cs = [["all", "All " + pgs.items.length], ["rev", "Revised"], ...hosts.map((h) => [h, h])];
  const js = pgs.items.some((p) => p.script) || pgsQuery(pgs.q).script;
  return cs.map(([k, label]) => `<button type="button" class="pgs-chip" data-chip="${esc(k)}" aria-pressed="${pgs.chip === k}">${esc(label)}</button>`).join("")
    + (js ? `<button type="button" class="pgs-chip" data-script="1" aria-pressed="${pgsQuery(pgs.q).script}" title="Pages that run JavaScript (the search term ${PGS_SCRIPT})">Interactive</button>` : "");
}

// pgsMarked is a snippet from the hub, as HTML: text with \x02 and \x03 around the matched words,
// escaped first, so nothing in a page's text can become markup here.
function pgsMarked(text) {
  return String(text || "").split(/[\x02\x03]/).map((part, i) => (i % 2 ? `<mark>${esc(part)}</mark>` : esc(part))).join("");
}

// pagesBodySearch asks the hub which pages' text holds the words (the user's alone; it reads
// each page's newest version). The list filter above works on the metadata we already have, so
// this only adds the "In page text" section, a moment after typing stops.
function pagesBodySearch() {
  clearTimeout(pgs.bodyTimer);
  const q = pgs.q.trim();
  const seq = ++pgs.bodySeq;
  // has:script alone is the list's own filter: there is no text to look for
  if (pgsQuery(q).text.length < 2) { if (pgs.body) { pgs.body = null; renderPages(); } return; }
  pgs.bodyTimer = setTimeout(async () => {
    try {
      const d = await api("/api/pages?q=" + encodeURIComponent(q));
      if (seq !== pgs.bodySeq) return;
      pgs.body = { q, matches: d.matches || [], partial: !!d.partial };
    } catch {
      if (seq !== pgs.bodySeq) return;
      pgs.body = null; // refused or too fast: the titles still filter
    }
    renderPages();
  }, 350);
}

function pageSize(n) {
  return n >= 1048576 ? (n / 1048576).toFixed(1) + " MiB" : n >= 1024 ? (n / 1024).toFixed(1) + " KiB" : n + " B";
}

function pageAgo(t) { const at = Date.parse(t); return Number.isNaN(at) ? "" : fmtAgo(at) + " ago"; }

// pgsGroup splits a list sorted by "updated" into Today / Yesterday / Earlier. Under any other sort, or when everything falls in
// one group, there are no headings: a lone "Today" says nothing.
function pgsGroup(list) {
  if (pgs.sort !== "updated") return [{ label: "", items: list }];
  const d0 = new Date(); d0.setHours(0, 0, 0, 0);
  const t0 = d0.getTime(), day = 86400000, groups = [];
  for (const p of list) {
    const at = Date.parse(p.updated_at) || 0, label = at >= t0 ? "Today" : at >= t0 - day ? "Yesterday" : "Earlier";
    const g = groups[groups.length - 1];
    if (g && g.label === label) g.items.push(p); else groups.push({ label, items: [p] });
  }
  return groups.length > 1 ? groups : [{ label: "", items: list }];
}

function renderPages() {
  const r = pgs.route;
  if (!r) return;
  const list = pagesFiltered();
  $("pgs-count").textContent = pgs.loaded && !pgs.err ? `${pgs.items.length} page${pgs.items.length === 1 ? "" : "s"}` : "";
  let html;
  if (pgs.err) html = `<div class="pgs-empty">${esc(pgs.err)}</div>`;
  else if (!pgs.loaded) html = `<div class="pgs-empty">Loading…</div>`;
  else if (!pgs.items.length) html = `<div class="pgs-empty pgs-zero"><span class="pgs-zsk" aria-hidden="true"><i class="a"></i><i></i><i></i><i></i></span><b>No pages yet</b><span>An agent publishes one when it has something to show you: a plan, a mockup, a report.</span><button type="button" class="btn cp" data-copy="comebye publish report.html" aria-label="Copy the command"><code>comebye publish report.html</code></button><span class="pgs-meta">A <code>.md</code> file works too.</span></div>`;
  else {
    // A card reads top down: the preview, the title, who made it, when and which version. The "new" / "updated" dot sits on the
    // card's corner; the size and kind are in the viewer's (i).
    const card = (p) => {
      const dot = pgsDot(p), rev = p.updated_at !== p.created_at;
      return `<a class="pgs-card${p.id === r.id ? " current" : ""}" href="${esc(pageHash(p.id))}" data-id="${esc(p.id)}">${pgsThumb(p)}${dot ? `<span class="pgs-dot">${dot}</span>` : ""}<span class="pgs-txt">
        <span class="pgs-title">${esc(p.title)}</span>
        <span class="pgs-line"><span class="pgs-by">${esc(p.by)}</span>${p.host ? `<span class="pgs-meta">${esc(p.host)}</span>` : ""}</span>
        <span class="pgs-line"><span class="pgs-meta" title="${esc(new Date(p.updated_at).toLocaleString())}">${rev ? "updated " : ""}${esc(pageAgo(p.updated_at))}</span><span class="pgs-tags">${p.script ? `<span class="pgs-pill pgs-js" title="Runs JavaScript">JS</span>` : ""}<span class="pgs-pill">v${p.version}</span></span></span></span></a>`;
    };
    const body = pgs.body && pgs.body.q === pgs.q.trim() ? pgs.body.matches.filter((m) => pgs.items.some((p) => p.id === m.id)) : [];
    const hit = (m) => {
      const p = pgs.items.find((x) => x.id === m.id);
      return `<a class="pgs-hit${p.id === r.id ? " current" : ""}" href="${esc(pageHash(p.id, m.version))}" data-id="${esc(p.id)}">
      <span class="pgs-title">${esc(p.title)}</span>
      <span class="pgs-meta">v${m.version} · ${m.count} ${m.count === 1 ? "match" : "matches"}</span>
      <span class="pgs-snip">${pgsMarked(m.snippet)}</span></a>`;
    };
    if (!list.length && !body.length) html = `<div class="pgs-empty">No page matches “${esc(pgs.q)}”.</div>`;
    else {
      html = "";
      if (body.length) html += `<div class="pgs-sec">Titles${list.length ? "" : ": none"}</div>`;
      html += pgsGroup(list).map((g) => (g.label ? `<div class="pgs-sec">${esc(g.label)}</div>` : "") + g.items.map(card).join("")).join("");
      if (body.length) html += `<div class="pgs-sec" id="pgs-intext">In page text · ${body.length}${pgs.body.partial ? "+" : ""}</div>` + body.map(hit).join("");
    }
  }
  const items = $("pgs-items");
  items.className = pgs.view === "list" ? "pgs-rows" : "pgs-grid";
  const chips = pgs.loaded && !pgs.err && pgs.items.length ? pagesChips() : "";
  if ($("pgs-chips")._html !== chips) { $("pgs-chips")._html = chips; $("pgs-chips").innerHTML = chips; }
  $("pgs-chips").hidden = !chips;
  for (const b of document.querySelectorAll("#pgs-views button")) b.setAttribute("aria-pressed", String(b.dataset.view === pgs.view));
  if ($("pgs-sort").value !== pgs.sort) $("pgs-sort").value = pgs.sort;
  if (items._html !== html) {
    // A card that has focus keeps it across a redraw (previews arriving redraw the list).
    const f = document.activeElement, fid = f && items.contains(f) && f.dataset ? f.dataset.id : "", fcls = f && f.classList && f.classList.contains("pgs-hit") ? ".pgs-hit" : ".pgs-card";
    items._html = html; items.innerHTML = html;
    if (fid) { const again = items.querySelector(fcls + '[data-id="' + fid + '"]'); if (again) again.focus({ preventScroll: true }); }
  }
  pgsFill();
  pgsWatch();
  renderPageView();
}

// The two popovers of the viewer bar are static elements (index.html), registered with the shared popover rules (POPS).
function pgsFillPops(p, ver, r, opts, many) {
  $("pgs-prov").textContent = `Published by ${p.by}${p.host ? " on " + p.host : ""}${p.tagged ? " (a worker node)" : ""} · ${pageAgo(p.created_at)}${p.updated_at !== p.created_at ? ", updated " + pageAgo(p.updated_at) : ""} · ${p.kind} · ${pageSize(p.size)}${p.handoff ? " · handoff " + p.handoff : ""}`;
  const v = $("pgs-vrow"), sel = $("pgs-ver");
  v.hidden = !many;
  if (sel._html !== opts) { sel._html = opts; sel.innerHTML = opts; }
  $("pgs-open").href = pgs.pagesUrl + "/p/" + p.id + (r.ver ? "/" + r.ver : "");
}

function pgsPop(name, show) {
  const el = $(POPS[name].id), btn = POPS[name].trigger();
  const open = show ?? el.hidden;
  if (open === !el.hidden) return;
  el.hidden = !open;
  if (open) { popOpened(name); placePop(name, btn); popItems(el)[0]?.focus(); }
  else popClosed(name);
  if (btn) btn.setAttribute("aria-expanded", String(open));
}

// Focus: read with the list out of the way. It reuses the list's own collapse (the [ key and its rail), but only for as long
// as the reader stays on a page: the browser's remembered choice is never touched, and leaving the page puts the list back.
function pagesFocus(on) {
  if (on === pgs.focus || !isDesktop()) return;
  pgs.focus = on;
  setListCollapsed(on ? true : lsGet(LIST_COLLAPSED_KEY) === "1", false);
  $("pgs-focus")?.setAttribute("aria-pressed", String(on));
}

function renderPageView() {
  const r = pgs.route, view = $("pgs-view");
  $("pages").classList.toggle("in-page", !!(r && r.id));
  if (!r || !r.id) {
    pgsPop("pgsmenu", false); pgsPop("pgsinfo", false); pagesFocus(false);
    if (view.dataset.id) { view.replaceChildren(); view.dataset.id = ""; pgs.frameKey = ""; }
    if (!view.firstChild) view.innerHTML = `<div class="pgs-pick">Pick a page from the list.</div>`;
    return;
  }
  const p = pgs.items.find((x) => x.id === r.id);
  if (!p) {
    pgs.frameKey = "";
    view.dataset.id = "";
    view.innerHTML = `<div class="pgs-pick">${pgs.loaded ? "That page is gone." : "Loading…"}</div>`;
    return;
  }
  // The frame keeps the version it opened. When the agent publishes a newer
  // one while the viewer follows the latest, a pill offers it instead of
  // swapping the page under the reader; clicking it reloads the frame.
  let ver = r.ver || p.version, newer = 0;
  if (view.dataset.id === p.id && pgs.frameFollow && !r.ver && !pgs.reload && pgs.frameVer && pgs.frameVer < p.version) { newer = p.version; ver = pgs.frameVer; }
  pgs.reload = false;
  if (ver === p.version) pgsMarkOpened(p);
  const key = p.id + "/" + ver;
  const vers = (p.versions || []).slice().reverse();
  const opts = vers.map((v) => `<option value="${v.n}"${v.n === ver ? " selected" : ""}>v${v.n}${v.n === p.version ? " (latest)" : ""} · ${esc(pageAgo(v.at))}</option>`).join("");
  // The slim bar: back, title, version, then (i) for who made it and the sandbox note, focus, and ⋯ for the actions. What the
  // two popovers hold is filled in below (they are static, so a refresh of the bar does not close one).
  const ICON = {
    back: '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><path d="m15 5-7 7 7 7" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    info: '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><circle cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M12 11v5M12 8h.01" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>',
    focus: '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><rect x="3.5" y="4.5" width="17" height="15" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M9.5 4.5v15M16 9.5l-2.5 2.5 2.5 2.5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    chat: '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><path d="M4 5.5A2.5 2.5 0 0 1 6.5 3h11A2.5 2.5 0 0 1 20 5.5v8a2.5 2.5 0 0 1-2.5 2.5H11l-5 4v-4h-.5A1.5 1.5 0 0 1 4 14.5z" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"/></svg>',
    more: '<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><circle cx="5" cy="12" r="1.5" fill="currentColor"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/><circle cx="19" cy="12" r="1.5" fill="currentColor"/></svg>',
  };
  const bar = `<div class="pgs-bar">
    <button type="button" class="pgs-ib" id="pgs-vback" aria-label="Back to Pages">${ICON.back}</button>
    <h2 class="pgs-t">${esc(p.title)}</h2>
    ${newer ? `<button type="button" class="btn sm pgs-new" id="pgs-new">v${newer} is out · reload</button>` : `<span class="pgs-pill${ver === p.version ? " cur" : ""}" id="pgs-vpill">v${ver}${ver < p.version ? " · older" : ""}</span>`}
    <span class="pgs-sp"></span>
    <button type="button" class="pgs-ib" id="pgs-infob" aria-label="About this page" aria-haspopup="dialog" aria-controls="pgs-infopop" aria-expanded="${!$("pgs-infopop").hidden}">${ICON.info}</button>
    <button type="button" class="pgs-ib pgs-chatb" id="pgs-chat" aria-label="Chat about this page" title="Chat about this page: start a read-only helper">${ICON.chat}<span class="pgs-chatl">Chat</span></button>
    <button type="button" class="pgs-ib pgs-focusb" id="pgs-focus" aria-label="Focus: hide the list" aria-pressed="${pgs.focus}" title="Focus: hide the list ( [ )">${ICON.focus}</button>
    <button type="button" class="pgs-ib" id="pgs-menub" aria-label="More" aria-haspopup="menu" aria-controls="pgs-menu" aria-expanded="${!$("pgs-menu").hidden}">${ICON.more}</button></div>`;
  pgsFillPops(p, ver, r, opts, vers.length > 1);
  if (view.dataset.id === p.id && pgs.frameKey === key) {
    // Same page and version: refresh the bar only, so the page keeps its state.
    view.querySelector(".pgs-bar").outerHTML = bar;
    pagesMade(p.id, ver);
    pgsChatGate(p.id);
    return;
  }
  pgsPop("pgsmenu", false); pgsPop("pgsinfo", false);
  view.dataset.id = p.id;
  pgs.frameKey = key;
  pgs.frameVer = ver;
  pgs.frameFollow = !r.ver;
  // allow="clipboard-write": a page's Copy button may write the clipboard (on a click; it can never read it).
  view.innerHTML = bar + `<div class="pgs-sel" id="pgs-sel" hidden></div><div class="pgs-body"><iframe class="pgs-frame" style="color-scheme:${pgsScheme()}" sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox" allow="clipboard-write" referrerpolicy="no-referrer" title="${esc(p.title)}" src="${esc(pgs.pagesUrl + "/p/" + p.id + "/" + ver)}"></iframe><aside class="pgs-tray" id="pgs-tray" aria-label="Comments" hidden></aside></div>`;
  const fr = view.querySelector("iframe.pgs-frame");
  fr.addEventListener("load", sendPageTheme);
  fr.addEventListener("load", pgcMarks);
  pgcOpen(p, ver);
  pagesMade(p.id, ver);
  pgsChatGate(p.id);
}

// The session that made a version of a page: the transcript index knows, from the
// publish command's output, so this works for old pages too. The strip is the app's, outside the
// page's sandbox, and stays hidden when the index has nothing (or is off): the page reads the same.
const PGS_MADE_TTL = 60000;
async function pagesMade(id, ver) {
  let c = pgs.made.get(id);
  if (!c || Date.now() - c.at > PGS_MADE_TTL) {
    c = { at: Date.now(), items: [] };
    pgs.made.set(id, c);
    try { c.items = ((await api("/api/recall/produced?ref=" + encodeURIComponent(id))).produced || []).filter((x) => x.type === "page" && x.ref === id); } catch { c.items = []; }
  }
  const box = $("pgs-made");
  const r = pgs.route;
  if (!box || !r || r.id !== id || ((r.ver || (pgs.items.find((x) => x.id === id) || {}).version) !== ver)) return;
  const hasStamp = (((pgs.items.find((x) => x.id === id) || {}).versions || []).find((v) => v.n === ver) || {}).session;
  if (!c.items.length && !hasStamp) { box.hidden = true; box.replaceChildren(); return; }
  // Each version names its own session: a key can gather versions from several.
  const want = "version " + ver;
  const it = c.items.find((x) => x.detail === want);
  // The hub's publish stamp names the session when the index has not matched the publish command
  // (a session not archived yet); the link is only offered once the index has that session.
  const pg = pgs.items.find((x) => x.id === id);
  const stamp = !it && pg && ((pg.versions || []).find((v) => v.n === ver) || {}).session;
  if (stamp) {
    c.stamps = c.stamps || new Map();
    const k = stamp.host + "/" + stamp.kind + "/" + stamp.id;
    if (!c.stamps.has(k)) {
      c.stamps.set(k, null);
      try { c.stamps.set(k, await api(rcSessionURL({ session: stamp }) + "?limit=1")); } catch { c.stamps.set(k, false); }
      if ($("pgs-made") !== box || !pgs.route || pgs.route.id !== id) return;
    }
  }
  const stamped = stamp && c.stamps.get(stamp.host + "/" + stamp.kind + "/" + stamp.id);
  const kids = [];
  if (stamped) {
    const t = document.createElement("span");
    t.className = "pgs-madetxt";
    t.textContent = `Made in ${(stamped.session && stamped.session.title) || stamp.id} · ${stamp.host} · ${pageAgo(pg.updated_at)}`;
    const a = document.createElement("a");
    a.className = "btn pgs-opensess"; a.id = "pgs-opensess";
    a.href = rcSessionHash(stamp);
    a.textContent = "Open session ▸";
    kids.push(t, a);
  } else if (it) {
    const t = document.createElement("span");
    t.className = "pgs-madetxt";
    t.textContent = `Made in ${it.session.title || it.session.id} · ${it.session.host} · turn ${it.seq} · ${pageAgo(it.ts)}`;
    const a = document.createElement("a");
    a.className = "btn pgs-opensess"; a.id = "pgs-opensess";
    a.href = rcSessionHash(it.session, { seq: it.seq, sub: it.sub });
    a.textContent = "Open session ▸";
    kids.push(t, a);
  } else if (!c.items.length) {
    box.hidden = true; box.replaceChildren(); return;
  } else {
    const t = document.createElement("span");
    t.className = "pgs-madetxt";
    t.textContent = "No session found for this version. Other versions: " + c.items.map((x) => (x.detail || "").replace("version ", "v")).filter(Boolean).join(", ") + ".";
    kids.push(t);
  }
  box.replaceChildren(...kids);
  box.hidden = false;
}

// The page's frame is an opaque origin, so it cannot read this app's theme.
// The app hands it the colours instead: a message {type:"comebye-theme",
// scheme:"light"|"dark", tokens:{"--bg":"#...", ...}} after the frame loads
// and whenever the theme changes. Colours only, nothing else; the target
// origin is "*" because an opaque origin cannot be named. A page that does not
// listen ignores it. It also says font:"serif"|"sans" (the Font setting); the Markdown shell follows it.
function pageThemeMessage() {
  const t = window.ComebyeThemes && ComebyeThemes.byId[document.documentElement.getAttribute("data-theme")];
  if (!t) return null;
  const tokens = {};
  for (const k of Object.keys(t.vars)) tokens["--" + k] = String(t.vars[k]);
  // The depth tokens follow the Surfaces setting: under Flat they are the plain panel, border and shadow colours.
  const dt = CT.depthTokens(t.vars);
  for (const k of Object.keys(dt)) tokens["--" + k] = String(dt[k]);
  return { type: "comebye-theme", scheme: t.mode, font: CT.fontChoice() === "serif" ? "serif" : "sans", tokens };
}

function pageFrames() { return document.querySelectorAll("#pgs-view iframe.pgs-frame, #rs iframe.pgs-frame"); }

function sendPageTheme() {
  if (pgs.route) pgsImgs();
  // The frame element's own color-scheme is what a browser takes as the frame's prefers-color-scheme: set it
  // on every theme change, so a page that never listens (or has not yet) still paints in the app's mode.
  const sc = pgsScheme();
  for (const fr of pageFrames()) if (fr.style.colorScheme !== sc) fr.style.colorScheme = sc;
  const m = pageThemeMessage();
  if (!m) return;
  for (const fr of pageFrames()) if (fr.contentWindow) fr.contentWindow.postMessage(m, "*");
}

// A page says {type:"comebye-page-ready"} as soon as its script runs (before its images load), and gets the
// theme at once; the send on the frame's load stays as the fallback. No data in the message: only the type is read.
window.addEventListener("message", (e) => {
  if (!e.data || typeof e.data !== "object" || e.data.type !== "comebye-page-ready") return;
  if (e.origin !== "null" && !(pgs.pagesUrl && e.origin === pgs.pagesUrl)) return;
  const m = pageThemeMessage();
  if (!m) return;
  for (const fr of pageFrames()) if (fr.contentWindow && e.source === fr.contentWindow) { fr.style.colorScheme = pgsScheme(); fr.contentWindow.postMessage(m, "*"); }
});

// Every theme change (picker, preview, the OS scheme under System) sets data-theme.
new MutationObserver(sendPageTheme).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme", "data-font", "data-depth"] });

// ---- Comments on a page ----
// The page is untrusted code in a sandboxed frame. The hub adds a small script
// to every page it serves that reports the text the user selects:
//   {type:"comebye-selection", v:1, quote, before, after}   (all strings)
// That is the only thing a page can tell this app, and it is checked here, not
// believed: the message must come from the frame this app opened, carry exactly
// these keys with strings within the limits, and arrive no faster than 10 a
// second. It has no recipient and no page id: the comments go to the page the
// viewer shows, and the hub sends them to that page's publisher. Page text is
// put in the DOM with textContent only. See docs/security.md.
const PGC = { quote: 500, around: 80, note: 1000, max: 20, burst: 10 };
const pgc = { split: false, id: "", ver: 0, title: "", by: "", sel: null, composing: false, busy: false, drafts: new Map(), seen: [], sent: [], revs: 0, chg: false };

function pgcClean(s, n) {
  return String(s).replace(/[\u0000-\u001f\u007f-\u009f]/g, " ").replace(/\s+/g, " ").trim().slice(0, n);
}

// The strict schema: null for anything else.
function pageSelectionMessage(d) {
  if (!d || typeof d !== "object" || Array.isArray(d)) return null;
  const keys = Object.keys(d);
  if (keys.length !== 5 || !["type", "v", "quote", "before", "after"].every((k) => keys.includes(k))) return null;
  if (d.type !== "comebye-selection" || d.v !== 1) return null;
  if (typeof d.quote !== "string" || typeof d.before !== "string" || typeof d.after !== "string") return null;
  if (d.quote.length > PGC.quote * 2 || d.before.length > PGC.around * 2 || d.after.length > PGC.around * 2) return null;
  return { quote: pgcClean(d.quote, PGC.quote), before: pgcClean(d.before, PGC.around), after: pgcClean(d.after, PGC.around) };
}

// {type:"comebye-revisions", v:1, count}: how many blocks of the page carry a
// data-revision mark (an agent's note of what it changed). Exactly those keys.
function pageRevisionsMessage(d) {
  if (!d || typeof d !== "object" || Array.isArray(d)) return null;
  const keys = Object.keys(d);
  if (keys.length !== 3 || !["type", "v", "count"].every((k) => keys.includes(k))) return null;
  if (d.type !== "comebye-revisions" || d.v !== 1 || !Number.isInteger(d.count) || d.count < 0 || d.count > 10000) return null;
  return { count: d.count };
}

// The Page menu offers to outline what the agent marked as changed in this version.
function pgcChgItem() {
  const b = $("pgs-chg");
  if (!b) return;
  b.hidden = !(pgc.revs > 0 && pgc.ver > 1);
  b.textContent = pgc.chg ? "Hide change highlights" : "Highlight what changed in v" + pgc.ver;
}

function pgcToggleChanges() {
  const fr = pgcFrame();
  if (!fr || !fr.contentWindow) return;
  pgc.chg = !pgc.chg;
  fr.contentWindow.postMessage({ type: "comebye-changes", v: 1, on: pgc.chg, since: Math.max(0, pgc.ver - 1) }, "*");
  pgcChgItem();
}

function pgcFrame() { return document.querySelector("#rs:not([hidden]) iframe.pgs-frame") || document.querySelector("#pgs-view iframe.pgs-frame"); }

window.addEventListener("message", (e) => {
  const fr = pgcFrame();
  if (!fr || !fr.contentWindow || e.source !== fr.contentWindow) return; // not the frame this app opened
  // A sandboxed frame has the opaque origin "null"; one served from the pages origin may name it.
  if (e.origin !== "null" && !(pgs.pagesUrl && e.origin === pgs.pagesUrl)) return;
  const now = Date.now();
  pgc.seen = pgc.seen.filter((t) => now - t < 1000);
  if (pgc.seen.length >= PGC.burst) return;
  pgc.seen.push(now);
  const rv = pageRevisionsMessage(e.data);
  if (rv) { pgc.revs = rv.count; pgcChgItem(); return; }
  const m = pageSelectionMessage(e.data);
  if (!m) return;
  if (pgc.composing) return; // keep the text being commented while the user types
  pgc.sel = m.quote ? m : null;
  pgcRender();
});

function pgcKey() { return pgc.id + "/" + pgc.ver; }
function pgcItems() { return pgc.drafts.get(pgcKey()) || []; }

function pgcOpen(p, ver, split) {
  Object.assign(pgc, { id: p.id, ver, title: p.title, by: p.by, split: !!split, sel: null, composing: false, busy: false, seen: [], sent: [], revs: 0, chg: false });
  pgcRender();
  pgcLoad();
  pgcChgItem();
}

// The comments already sent on this page, with their status (hub: open, or
// addressed once a newer version no longer has the quoted text).
async function pgcLoad() {
  const id = pgc.id;
  try {
    const r = await api("/api/pages/" + encodeURIComponent(id) + "/comments");
    if (pgc.id !== id) return;
    pgc.sent = Array.isArray(r.comments) ? r.comments : [];
  } catch { return; }
  pgcRender();
}

// Tell the page which quotes to mark. The page does the marking (it alone can
// see its own text); it takes these only from this app's window and treats
// them as text to find, never as markup.
function pgcMarks() {
  const fr = pgcFrame();
  if (!fr || !fr.contentWindow) return;
  const quotes = pgc.sent.filter((c) => c.status === "open").map((c) => c.quote).concat(pgcItems().map((c) => c.quote)).slice(0, 40);
  fr.contentWindow.postMessage({ type: "comebye-marks", v: 1, quotes }, "*");
}

function pgcReveal(quote) {
  const fr = pgcFrame();
  if (fr && fr.contentWindow) fr.contentWindow.postMessage({ type: "comebye-reveal", v: 1, quote }, "*");
}

function pgcEl(tag, cls, text) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text != null) el.textContent = text;
  return el;
}

// pgcDrawStrip fills the strip for the selection: the quote and Comment, or the note box while composing.
function pgcDrawStrip(strip) {
  const q = pgcEl("span", "pgs-selq", "“" + pgc.sel.quote + "”");
  q.title = pgc.sel.quote;
  if (!pgc.composing) {
    const b = pgcEl("button", "btn primary", "Comment");
    b.type = "button"; b.id = "pgs-addc";
    b.addEventListener("click", () => { pgc.composing = true; pgcRender(); });
    strip.append(q, b);
  } else {
    const ta = pgcEl("textarea");
    ta.id = "pgs-note"; ta.maxLength = PGC.note; ta.rows = 3; ta.placeholder = "Your comment on this text";
    ta.setAttribute("aria-label", "Your comment on the selected text");
    const add = pgcEl("button", "btn primary", "Add"), cancel = pgcEl("button", "btn", "Cancel");
    add.type = cancel.type = "button"; add.id = "pgs-noteadd"; cancel.id = "pgs-notecancel";
    const done = () => {
      const note = ta.value.trim();
      if (!note || pgcItems().length >= PGC.max) return;
      const list = pgcItems().slice();
      list.push({ quote: pgc.sel.quote, before: pgc.sel.before, after: pgc.sel.after, note });
      pgc.drafts.set(pgcKey(), list);
      pgc.sel = null; pgc.composing = false; pgcRender();
    };
    add.addEventListener("click", done);
    cancel.addEventListener("click", () => { pgc.composing = false; pgc.sel = null; pgcRender(); });
    ta.addEventListener("keydown", (ev) => { if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); done(); } });
    const row = pgcEl("div", "pgs-selrow"); row.append(add, cancel);
    strip.append(q, ta, row);
    setTimeout(() => ta.focus(), 0);
  }
}

function pgcRender() {
  const strip = $("pgs-sel"), tray = $("pgs-tray");
  if (!strip || !tray) return;
  const items = pgcItems();
  // The strip: the selected text and what to do with it. Drawn again only when the selection or the composing state
  // changed: a redraw for anything else (the sent comments arriving, a draft added) between the press and the release
  // of Comment put a new button under the release, the browser then fired no click, and the tap was lost.
  const stripKey = pgc.sel ? JSON.stringify([pgc.sel.quote, pgc.sel.before, pgc.sel.after, !!pgc.composing]) : "";
  if (strip._key !== stripKey) {
    strip._key = stripKey;
    strip.replaceChildren();
    strip.hidden = !pgc.sel;
    if (pgc.sel) pgcDrawStrip(strip);
  }
  // The tray: the comments to send, Send, and below them what was sent.
  tray.replaceChildren();
  tray.hidden = items.length === 0 && pgc.sent.length === 0;
  rsNotesRefresh();
  pgcMarks();
  if (tray.hidden) return;
  if (items.length) tray.append(pgcEl("h3", "", "To send · " + items.length));
  items.forEach((c, i) => {
    const card = pgcEl("div", "pgs-cmt");
    const rm = pgcEl("button", "pgs-cmtx", "×");
    rm.type = "button"; rm.setAttribute("aria-label", "Remove comment " + (i + 1));
    rm.addEventListener("click", () => { const l = pgcItems().slice(); l.splice(i, 1); pgc.drafts.set(pgcKey(), l); pgcRender(); });
    card.append(rm, pgcEl("div", "pgs-cmtq", "“" + c.quote + "”"), pgcEl("div", "pgs-cmtn", c.note));
    tray.append(card);
  });
  if (items.length) {
    const send = pgcEl("button", "btn primary", pgc.busy ? "Sending…" : "Send to " + (pgc.by || "publisher"));
    send.type = "button"; send.id = "pgs-sendc"; send.disabled = pgc.busy;
    send.addEventListener("click", pgcSend);
    tray.append(send, pgcEl("div", "pgs-cmthint", pgc.split ? "Or leave them: they go with your answer on the right. Send files one To do item and messages the page’s publisher." : "Files one To do item and messages the page’s publisher."));
  }
  if (pgc.sent.length) {
    tray.append(pgcEl("h3", "pgs-senth", "Sent · " + pgc.sent.length));
    pgc.sent.slice().reverse().forEach((c) => {
      const card = pgcEl("div", "pgs-cmt sent");
      const rm = pgcEl("button", "pgs-cmtx", "×");
      rm.type = "button"; rm.setAttribute("aria-label", "Forget this comment");
      rm.addEventListener("click", async () => {
        try { await api("/api/pages/" + encodeURIComponent(pgc.id) + "/comments/" + encodeURIComponent(c.id), { method: "DELETE" }); } catch (e) { toast("Could not remove it: " + (e && e.message ? e.message : e), true); return; }
        pgc.sent = pgc.sent.filter((x) => x.id !== c.id); pgcRender();
      });
      const q = pgcEl("button", "pgs-cmtq link", "“" + c.quote + "”");
      q.type = "button"; q.title = "Show it in the page";
      q.addEventListener("click", () => pgcReveal(c.quote));
      const meta = pgcEl("div", "pgs-cmtmeta");
      meta.append(pgcEl("span", "chip " + (c.status === "addressed" ? "green" : "yellow"), c.status === "addressed" ? "addressed" : "open"), pgcEl("span", "", "v" + c.version));
      card.append(rm, q, pgcEl("div", "pgs-cmtn", c.note), meta);
      tray.append(card);
    });
  }
}

async function pgcSend() {
  const items = pgcItems();
  if (!items.length || pgc.busy) return;
  const key = pgcKey(), id = pgc.id, ver = pgc.ver;
  pgc.busy = true; pgcRender();
  try {
    const r = await api("/api/pages/" + encodeURIComponent(id) + "/comments", { method: "POST", body: JSON.stringify({ version: ver, comments: items }) });
    pgc.drafts.delete(key);
    pgcLoad();
    toast(r && r.sent === "queued" ? `Sent ${items.length} comment${items.length === 1 ? "" : "s"}: in To do and queued to the publisher` : (r && r.note) || "Saved in To do");
  } catch (e) {
    toast("Could not send the comments: " + (e && e.message ? e.message : e), true);
  }
  pgc.busy = false;
  if (pgcKey() === key) pgcRender();
}

// syncPages follows the hash: route() calls it.
function syncPages() {
  const r = parsePagesRoute();
  const el = $("pages");
  const was = !el.hidden, prev = pgs.route;
  pgs.route = r;
  el.hidden = !r;
  pagesStamp(r, was, prev);
  if (!r) {
    pgsPop("pgsmenu", false); pgsPop("pgsinfo", false); pagesFocus(false);
    clearInterval(pgs.timer); pgs.timer = null;
    $("pgs-view").replaceChildren(); $("pgs-view").dataset.id = ""; pgs.frameKey = "";
    return;
  }
  if (!was) {
    $("pgs-q").value = pgs.q;
    pgs.timer = setInterval(pagesLoad, 20000);
    pagesLoad();
  } else if (r.id && !pgs.items.some((p) => p.id === r.id)) {
    pagesLoad(); // a page published since the list was read
  }
  renderPages();
}

// Where did this history entry's page come from? Stamped once into the entry's own state (pgFrom), so it survives Back, Forward,
// a reload and the replaceState calls that keep history.state: "app" (arrived from another screen: Back returns there),
// "gallery" (from the list, or hopping between pages beside it: Back is the list), "cold" (the first screen this tab ever showed:
// nothing in the app to go back to).
function pagesStamp(r, was, prev) {
  const seen = pgs.booted; pgs.booted = true;
  if (!r || !r.id || (history.state && history.state.pgFrom)) return;
  const from = !seen ? "cold" : was ? "gallery" : "app";
  history.replaceState({ ...(history.state || {}), pgFrom: from }, "", location.href);
}
// pagesUnwind leaves a page: Back to the screen it was opened from, or to the gallery (replacing the page's entry, so the
// history shrinks instead of piling a gallery entry on top of the page).
function pagesUnwind() {
  if (history.state && history.state.pgFrom === "app") { history.back(); return; }
  history.replaceState(null, "", "#/pages");
  route();
}

function closePages() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

// Back from a page is the gallery; back from the gallery is the way out, on a phone. In the pane (desktop) the nav is the way out.
function pagesBack() {
  if (pgs.route && pgs.route.id) pagesUnwind();
  else if (!isDesktop()) closePages();
}

async function pagesDelete() {
  const r = pgs.route, p = r && pgs.items.find((x) => x.id === r.id);
  if (!p || !confirm(`Delete “${p.title}”?\n\nThis removes the page and all ${(p.versions || []).length} kept version(s) for good.`)) return;
  try {
    await api("/api/pages/" + encodeURIComponent(p.id), { method: "DELETE" });
    pgs.items = pgs.items.filter((x) => x.id !== p.id);
    toast("Deleted");
    pagesUnwind();
  } catch (e) { toast(e.message, true); }
}

function initPages() {
  $("pgs-back").addEventListener("click", pagesBack);
  $("pgs-q").addEventListener("input", (ev) => { pgs.q = ev.target.value; renderPages(); pagesBodySearch(); });
  $("pgs-sort").addEventListener("change", (ev) => { pgs.sort = ev.target.value; lsSet("comebye.pgs.sort", pgs.sort); renderPages(); });
  $("pgs-views").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-view]");
    if (!b) return;
    pgs.view = b.dataset.view; lsSet("comebye.pgs.view", pgs.view); renderPages();
  });
  $("pgs-chips").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-chip]");
    if (!b) return;
    pgs.chip = b.dataset.chip; renderPages();
  });
  $("pgs-chips").addEventListener("click", (ev) => { // Interactive: add or remove has:script in the search box
    if (!ev.target.closest("button[data-script]")) return;
    const pq = pgsQuery(pgs.q);
    pgs.q = pq.script ? pq.text : (PGS_SCRIPT + " " + pq.text).trim();
    $("pgs-q").value = pgs.q;
    renderPages(); pagesBodySearch();
  });
  $("pages").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && (!$("pgs-menu").hidden || !$("pgs-infopop").hidden)) return; // the popover answers it
    if (ev.key === "Escape") { ev.stopPropagation(); pagesBack(); }
  });
  $("pages").addEventListener("change", (ev) => {
    if (ev.target.id !== "pgs-ver" || !pgs.route) return;
    pgsPop("pgsmenu", false);
    const p = pgs.items.find((x) => x.id === pgs.route.id), n = Number(ev.target.value);
    history.replaceState(history.state, "", pageHash(pgs.route.id, p && n === p.version ? 0 : n));
    pgs.reload = true; // an explicit pick loads that version, pill or not
    route();
  });
  $("pages").addEventListener("click", async (ev) => {
    if (ev.target.closest("#pgs-vback")) { pagesBack(); return; }
    if (ev.target.closest("#pgs-menub")) { pgsPop("pgsmenu"); return; }
    if (ev.target.closest("#pgs-infob")) { pgsPop("pgsinfo"); return; }
    if (ev.target.closest("#pgs-focus")) { pagesFocus(!pgs.focus); return; }
    if (ev.target.closest("#pgs-chatpick")) { pgsPop("pgsmenu", false); openChatPick(pgs.route && pgs.route.id, false); return; }
    if (ev.target.closest("#pgs-chatnew")) { pgsPop("pgsmenu", false); pagesChat(true); return; }
    if (ev.target.closest("#pgs-chat, #pgs-chatmi")) { pgsPop("pgsmenu", false); pagesChat(); return; }
    if (ev.target.closest("#pgs-chg")) { pgsPop("pgsmenu", false); pgcToggleChanges(); return; }
    if (ev.target.closest("#pgs-del")) { pgsPop("pgsmenu", false); pagesDelete(); return; }
    if (ev.target.closest("#pgs-new")) { pgs.reload = true; renderPageView(); return; }
    if (!ev.target.closest("#pgs-copy")) return;
    pgsPop("pgsmenu", false);
    const r = pgs.route, base = pgs.pagesUrl || pgs.publicUrl || location.origin;
    const link = base + "/p/" + r.id + (r.ver ? "/" + r.ver : "");
    if (await copyText(link)) toast("Link copied"); else toast(link);
  });
  // A link in a report or to-do item to one of this hub's pages
  // opens it here.
  document.addEventListener("click", (ev) => {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey) return;
    const a = ev.target.closest && ev.target.closest("a[href]");
    if (!a || a.closest("#pages") || a.getAttribute("target") === "_blank" && a.id === "pgs-open") return;
    const pg = pageFromUrl(a.href);
    if (!pg) return;
    ev.preventDefault();
    location.hash = pageHash(pg.id, pg.ver);
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

// #/todo/<id> opens one To do item; the bare #/todo on a phone scrolls the To do card into view (on a desktop it is the page).
// The route is held in todos.deep until the list has loaded (todoDeepResolve, from loadTodos and from route).
const TODO_ID = /^t[0-9a-f]{10}$/;
function parseTodoRoute() {
  const m = location.hash.match(/^#\/todo(?:\/([^/?]*))?$/);
  if (!m) return null;
  if (m[1] === undefined) return isDesktop() ? null : { id: "" };
  try { return { id: decodeURIComponent(m[1]) }; } catch { return { id: m[1] }; }
}
// Takes the hash back to the plain page once the item the link opened is closed, so a reload does not reopen it.
function todoDeepClean() {
  todos.deepId = null; todos.deepHash = null; todos.pin = null;
  if (location.hash.startsWith("#/todo/")) history.replaceState(history.state, "", isDesktop() ? PAGE_HASH.todo : location.pathname + location.search);
}
function todoDeepNotice(id, text) {
  todos.deep = null; todos.deepHash = null;
  todos.notice = { id, text };
  history.replaceState(history.state, "", isDesktop() ? PAGE_HASH.todo : location.pathname + location.search);
  renderTodos(true);
  const el = isDesktop() ? $("pt-body") : $("todo");
  if (el && !isDesktop()) el.scrollIntoView?.({ block: "start" });
}
function todoDeepResolve() {
  const d = todos.deep;
  if (!d) return;
  if (todos.on === false && ledger.on === false) { if (d.id) todoDeepNotice(d.id, "To do is not available on this hub."); else todos.deep = null; return; }
  if (todos.on !== true) return; // the list has not arrived: keep the route
  if (!d.id) { // the bare #/todo on a phone
    todos.deep = null;
    $("todo")?.scrollIntoView?.({ block: "start" });
    return;
  }
  const bad = () => todoDeepNotice(d.id, `No to-do ${d.id.length > 24 ? d.id.slice(0, 24) + "…" : d.id} here. It may have been cleared, or it is on another hub.`);
  if (!TODO_ID.test(d.id)) { bad(); return; }
  const t = todos.items.find((x) => x.id === d.id);
  if (!t) {
    if (d.fetching) return;
    d.fetching = true;
    api("/api/todos/" + d.id).then((j) => {
      if (todos.deep !== d) return;
      if (j && j.todo && j.todo.id === d.id) { todos.pin = j.todo; todos.items.push(j.todo); d.fetching = false; todoDeepResolve(); } else bad();
    }, (e) => { if (todos.deep !== d) return; if (e && (e.status === 404 || e.status === 400)) bad(); else d.fetching = false; /* a failed fetch retries on the next poll */ });
    return;
  }
  todos.deep = null;
  todos.notice = null;
  todos.deepId = t.id; todos.deepHash = location.hash;
  if (t.state !== "open") todos.ansOpen = true;
  todos.openId = t.id; todos.lastId = t.id; todos.follow = true;
  if (!isDesktop()) { lsSet(TODO_FOLDED_KEY, "0"); closeStatusScreen(); }
  renderTodos(true);
  if (!rsActive()) {
    const el = todoListEl(), row = el && todoRowIn(el, t.id);
    if (row) row.scrollIntoView?.({ block: isDesktop() ? "nearest" : "start" });
  }
}

function routedAgent(r) {
  return state.agents.find((a) => a.pane_id === r.pane &&
    (r.host && r.host !== state.localHost ? a.host === r.host : isLocal(a)));
}

// route syncs the view to the URL hash. An agent that is not in the list
// yet (cold start from a notification) is opened once the list arrives.
// syncScreens follows the hash for every screen (.screen: Search, Pages, Peers, Usage, Ready to remove). The one place that
// knows the set: route(), openDetail and openPage call it, so an agent or a page replacing a screen in the pane (and the other
// way round) never leaves one showing. has-screen is what #detail-empty waits for; the nav's active row follows.
function syncScreens() {
  syncRecall();
  syncPages();
  syncPeers();
  syncUsage();
  syncSettings();
  syncReview();
  syncArchived();
  document.body.classList.toggle("has-screen", !!(rc.route || pgs.route || prs.route || us.route || sx.route || review.open || arch.route));
  renderNav();
}

function route() {
  syncScreens();
  syncStatusScreen();
  syncSubview();
  syncLaunch();
  syncMark();
  syncRestart();
  syncParkSheet();
  syncParkAll();
  syncSplit();
  syncFresh();
  syncSwitch();
  syncChatPick();
  const r = parseRoute();
  if (!r) {
    state.pendingRoute = null;
    if (applyListHash()) renderList(); else syncListHash();
    if (state.selected) closeDetail();
    if (isDesktop()) setView(viewFromHash()); // the hash names the page
    // A todo link is taken up once per hash: Back out of the split view lands on the same hash and must not reopen the item.
    const td = parseTodoRoute();
    if (!td) todos.deepHash = null;
    else if (location.hash !== todos.deepHash) { todos.deep = td; todos.deepId = null; todos.deepHash = null; todoDeepResolve(); }
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
  if (h && h.status === "down") {
    // A stopped worker: its sessions are not listed, but opening one is only a read (the Start banner).
    if (wakeOf(h)) { state.pendingRoute = null; openStoppedSession(r.host, r.pane); return; }
    if (!ph.data && !r.looked) { r.looked = true; loadProvHosts(false).then(() => applyPendingRoute()); return; }
  }
  state.pendingRoute = null;
  toast(h && h.status === "down" ? `${r.host} is unreachable${h.class ? `: ${h.class}` : ""}` : "that agent is gone", true);
  history.replaceState(null, "", location.pathname + location.search);
}

/* ---------- pages (desktop): Status, To do, Search fill the main pane ---------- */
//
// One selection fills one pane: an agent (state.selected) or a page
// (state.view), never both. The pages are only a desktop thing; below
// 1100px the hash routes do nothing new and the phone flow is unchanged.

const PAGE_HASH = { status: "#/status", todo: "#/todo" };
const viewFromHash = () => (isDesktop() ? Object.keys(PAGE_HASH).find((k) => PAGE_HASH[k] === (location.hash.startsWith("#/todo/") ? PAGE_HASH.todo : location.hash)) || null : null);

// setView shows a page (or none) without touching the history.
function setView(v) {
  state.view = isDesktop() ? v : null;
  for (const k of ["status", "todo"]) document.body.classList.toggle("view-" + k, state.view === k);
  document.body.classList.toggle("has-page", !!state.view);
  syncStatusScreen();
  renderNav();
  renderStatus();
  if (state.view === "todo") renderTodos(); else rsLeave();
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
  syncScreens();
  setView(v);
  // Focus left on the row or button that was in use (an agent row, say) stays there when the page
  // replaces the agent, so a key such as j lands outside the page and the page's own keys
  // (onTodoKey) ignore it. The keyboard route (g t) never clicks into the page: park focus on the body.
  const ae = document.activeElement;
  if (ae && ae !== document.body && ae.closest && ae.closest("#agents, #nav, #detail")) ae.blur();
}

// On a cold start the desktop opens to something: an agent that is waiting
// on you if there is one, otherwise the Status page. A deep link or a
// page's own hash wins, and it happens once.
function openToSomething() {
  if (state.opened || !isDesktop() || state.pendingRoute) return;
  state.opened = true;
  if (state.selected || state.view || (location.hash && !onListHash())) return;
  const w = document.querySelector("#agents .agent.blocked[data-id]");
  if (w) { openDetail(w.dataset.id); return; }
  if (ledger.on !== false) {
    history.replaceState(null, "", PAGE_HASH.status);
    setView("status");
  }
}

/* ---------- The board: lanes from GET /api/board, at the top of Status ---------- */
//
// Everything on it comes from the hub's records (to-dos, ledger, agents,
// releases). Read-only. Needs you is open at first and is the
// same in both groupings; the rest is by status (the lanes) or by project.

const BOARD_MODE_KEY = "comebye-board-mode";
const board = { data: null, at: 0, inflight: false, mode: lsGet(BOARD_MODE_KEY) === "project" ? "project" : "status", open: new Set(["needs_you"]), projOpen: new Set() };
const BOARD_LANE_LABEL = { running: "running", waiting: "waiting", landing: "to land", reports: "to read", landed: "landed", hub_did: "hub did" };

// The "new" mark: this browser remembers which item was in which lane when the
// board was last marked seen (BOARD_SEEN_KEY, {items: {id: lane}}). An item
// that is not in that snapshot is "new"; one that is in another lane is
// "moved". The first board this browser ever loads becomes the snapshot
// silently, so nothing is new on a first visit. "Mark seen" takes a new one.
const BOARD_SEEN_KEY = "comebye-board-seen";
function boardSeenLoad() {
  try {
    const v = JSON.parse(lsGet(BOARD_SEEN_KEY) || "null");
    return v && typeof v.items === "object" && v.items ? v : null;
  } catch { return null; }
}
board.seen = boardSeenLoad();

// boardSnapshot is every item of the board and its lane.
function boardSnapshot(d) {
  const items = {};
  for (const l of (d && d.lanes) || []) for (const it of l.items || []) items[it.id] = l.key;
  return { at: new Date().toISOString(), items };
}

// boardMark is "new", "moved" or "" for an item now in laneKey.
function boardMark(id, laneKey) {
  if (!board.seen) return "";
  const was = board.seen.items[id];
  return was === undefined ? "new" : was !== laneKey ? "moved" : "";
}

function boardMarkSeen() {
  if (!board.data) return;
  board.seen = boardSnapshot(board.data);
  lsSet(BOARD_SEEN_KEY, JSON.stringify(board.seen));
  for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
  renderStatus();
}

async function loadBoard() {
  if (board.inflight || ledger.on !== true) return;
  board.inflight = true;
  try {
    const d = await api("/api/board");
    if (d && Array.isArray(d.lanes)) {
      board.data = d;
      if (d.loose && arch.data && (d.loose.archived || 0) !== arch.data.count) loadArchived();
      if (!board.seen) { board.seen = boardSnapshot(d); lsSet(BOARD_SEEN_KEY, JSON.stringify(board.seen)); }
    }
  } catch { /* keep what we had */ } finally {
    board.inflight = false;
    board.at = Date.now();
  }
  renderStatus();
}

function boardItemHtml(it, laneKey, showLane) {
  const open = it.agent && isDesktop() && state.agents.some((a) => agentId(a) === it.agent) ? ` <button type="button" class="st-link" data-agent="${escAttr(it.agent)}">open</button>` : "";
  const when = it.since && fmtWhen(it.since) ? `<span class="bd-age">${esc(fmtWhen(it.since))}</span>` : "";
  const lane = showLane ? `<span class="bd-tag">${esc(BOARD_LANE_LABEL[laneKey] || "")}</span>` : "";
  const pk = boardPark(it);
  const paused = pk ? ` <span class="chip teal bd-park" title="${pk.label === "paused" ? "Parked mid-task: its pane is closed, and it comes back here when resumed." : "Its pane is closed; the work is kept."}">${esc(pk.label)}</span> <button type="button" class="st-link" data-bd-park="${escAttr(pk.rec.id)}" data-host="${escAttr(pk.host)}">resume</button>` : "";
  const mk = boardMark(it.id, laneKey);
  const mark = mk ? `<span class="bd-new" title="${mk === "new" ? "Not on the board when you last marked it seen" : "In another lane when you last marked it seen"}">${mk}</span>` : "";
  if (it.type === "todo") {
    return `<li><button type="button" class="bd-todo" data-bd-todo="${escAttr(it.id)}" aria-label="Open to-do ${escAttr(it.id)}: ${escAttr(it.title)}"><span class="td-kind">${esc(TODO_KIND[it.kind] || it.kind || "")}</span> <span class="bd-q">${esc(it.title)}</span>${mark}<code class="bd-id">${esc(it.id)}</code></button></li>`;
  }
  const pages = (it.pages || []).map((p) => `<a class="rv-page" href="${pageHash(p.id)}">${esc(p.title || p.id)}</a>`).join(" · ");
  let rep = "";
  if (it.type === "handoff" && laneKey === "reports") {
    const e = it.report ? ledger.entries.find((x) => x && x.id === it.id) : null;
    const op = statusUi.open.has(it.id);
    const rpt = e && e.final_report
      ? `<details class="st-row bd-rep" data-id="${escAttr(it.id)}"${op ? " open" : ""}><summary>Read report${reportAgeHtml(e)}</summary>${op ? `<div class="st-rep">${copyBtnHtml("rep:" + it.id, e.final_report, "Copy report", "Copy report as Markdown", "cpb-rep")}</div><div class="md st-rep">${renderMarkdown(e.final_report, "board:" + it.id)}</div>${newerReplyHtml(e, "board:" + it.id)}` : ""}</details>`
      : `<span class="bd-d">${it.report ? "report loading" : "no report captured"}</span>`;
    rep = `<div class="bd-d">${esc(it.kind || "report")}${pages ? " · Page: " + pages : ""}</div>${rpt}`;
  }
  return `<li><div class="bd-line"><b class="bd-n">${esc(it.title)}</b>${mark}${lane}${paused}${when}${open}</div>${it.detail ? `<div class="bd-d">${esc(it.detail)}</div>` : ""}${rep}</li>`;
}

function boardLaneHtml(key, name, count, items, open, showLane, extra) {
  if (!count) return "";
  const names = items.slice(0, 3).map((i) => i.title).join(", ") + (count > 3 ? ", …" : "");
  const fresh = items.filter((i) => boardMark(i.id, i.lane || key)).length;
  return `<details class="bd-lane${key === "needs_you" ? " need" : ""}" data-lane="${escAttr(key)}"${open ? " open" : ""}><summary><span class="bd-chip">${count}</span><span class="bd-name">${esc(name)}</span>${fresh ? `<span class="bd-new">${fresh} new</span>` : ""}<span class="bd-names">${esc(names)}</span></summary>` +
    `<ul>${items.map((i) => boardItemHtml(i, i.lane || key, showLane)).join("")}</ul>${count > items.length ? `<p class="bd-more">and ${count - items.length} older not shown</p>` : ""}${extra || ""}</details>`;
}

function boardHtml() {
  const d = board.data;
  if (!d) return "";
  const lanes = d.lanes || [];
  const needs = lanes.find((l) => l.key === "needs_you") || { count: 0, items: [] };
  const rest = lanes.filter((l) => l.key !== "needs_you");
  const total = rest.reduce((n, l) => n + l.count, 0);
  const sw = ["status", "project"].map((m) => `<button type="button" data-bd-mode="${m}" aria-pressed="${board.mode === m}">${m === "status" ? "By status" : "By project"}</button>`).join("");
  let body = "";
  if (board.mode === "status") {
    body = rest.map((l) => boardLaneHtml(l.key, l.name, l.count, l.items, board.open.has(l.key), false)).join("");
  } else {
    const by = new Map();
    for (const l of rest) for (const it of l.items) { if (!by.has(it.project)) by.set(it.project, []); by.get(it.project).push({ ...it, lane: l.key }); }
    body = [...by.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]))
      .map(([p, items]) => boardLaneHtml("p:" + p, p, items.length, items, board.open.has("p:" + p), true)).join("");
  }
  const loose = d.loose || {};
  // The archived count links to the Archived screen: they are in no lane.
  const foot = [loose.ready_to_remove ? esc(`${loose.ready_to_remove} ready to remove`) : "",
    loose.archived ? `<a class="bd-arch" href="${ARCHIVED_HASH}">${esc(loose.archived)} archived</a>` : ""].filter(Boolean).join(" · ");
  const counts = rest.map((l) => `${l.count} ${BOARD_LANE_LABEL[l.key] || l.name}`).join(" · ");
  let fresh = 0;
  for (const l of lanes) for (const it of l.items || []) if (boardMark(it.id, l.key)) fresh++;
  const seen = fresh ? `<button type="button" class="bd-seen" data-bd-seen>${fresh} new or moved · Mark seen</button>` : "";
  return `<section class="bd" aria-label="Board">` +
    (needs.count ? boardLaneHtml("needs_you", "Needs you", needs.count, needs.items, board.open.has("needs_you"), false) : `<p class="bd-clear">Nothing needs you.</p>`) +
    `<div class="bd-bar"><span class="bd-sum">${esc(total ? counts : "Nothing else in flight.")}</span><span class="bd-seg" role="group" aria-label="Group by">${sw}</span>${seen}</div>${body}` +
    (foot ? `<p class="bd-foot">${foot}</p>` : "") + `</section>`;
}

function boardGoTodo(id) {
  todos.openId = id;
  todos.follow = true;
  if (isDesktop()) openPage("todo");
  else {
    closeStatusScreen();
    $("todo")?.scrollIntoView?.({ block: "start" });
  }
  renderTodos(true);
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
    if (e.status === "waiting") proj.blocked.push({ e, why: e.wait_kind === "cap" && !(e.after || []).length ? (e.wait_note || "waiting for a free slot") : `queued behind ${(e.after || []).length || "earlier"} step${(e.after || []).length === 1 ? "" : "s"}` });
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
      `<div class="st-row"><span class="st-n">${esc(statusWho(e))}</span><span class="st-m warn">${esc(why)}</span><span class="st-r">${esc(e.branch || "")}${usageChipHtml(e.id, true)}${open(ag)}</span></div>`).join("");
    if (p.todos.length) h += `<div class="st-g you">Waiting on you</div>` + p.todos.map((t) =>
      `<div class="st-row"><span class="td-kind">${esc(TODO_KIND[t.kind] || t.kind)}</span><span class="st-n">${esc(t.question)}</span>${desktop ? `<span class="st-r"><button type="button" class="st-link" data-go="todo">To do</button></span>` : ""}</div>`).join("");
    if (p.progress.length) h += `<div class="st-g prog">In progress</div>` + p.progress.map(({ e, ag }) => {
      const at = Date.parse(e.created_at);
      return `<div class="st-row"><span class="st-n">${esc(statusWho(e))}</span>${e.branch ? `<span class="wt-chip cp" role="button" tabindex="0" aria-label="Copy branch" data-copy="${escAttr(e.branch)}">⑂ ${esc(e.branch)}</span>` : ""}<span class="st-m">${e.status === "landing" ? esc((landLabel(e, ag) || { text: "landing" }).text) : bgRunning(ag) ? "waiting on a background task" : quietMs(ag) ? "running · " + quietText(ag) : "running"}${Number.isNaN(at) ? "" : " " + esc(fmtAgo(at))}</span><span class="st-r">${usageChipHtml(e.id, true)}${open(ag)}</span>${noteHtml(ag).replace("pnote", "pnote st-note")}</div>`;
    }).join("");
    if (p.done.length) h += `<div class="st-g">Finished recently</div>` + p.done.map(({ e, at, ag }) => {
      const chip = { finished: ["", "reported"], landed: ["landed", "landed"], ready_to_remove: ["rtr", "ready to remove"], failed: ["failed", "failed"] }[e.status];
      const body = e.final_report ? `<div class="st-rep">${copyBtnHtml("rep:" + e.id, e.final_report, "Copy report", "Copy report as Markdown", "cpb-rep")}${reportAgeHtml(e)}</div><div class="md st-rep">${renderMarkdown(e.final_report, "status:" + e.id)}</div>${newerReplyHtml(e, "status:" + e.id)}<div class="hubchk st-rep" data-id="${escAttr(e.id)}" hidden></div>` : `<div class="st-m st-rep">${esc(e.error || "No report.")}</div>`;
      return `<details class="st-row" data-id="${escAttr(e.id)}"${statusUi.open.has(e.id) ? " open" : ""}><summary><span class="st-n">${esc(statusWho(e))}</span><span class="lg-chip ${chip[0]}">${chip[1]}</span>${usageChipHtml(e.id, true)}<span class="st-r">${esc(fmtAgo(at))} ago</span></summary>${statusUi.open.has(e.id) ? body : ""}</details>`;
    }).join("");
    return `<section class="st-proj${p.blocked.length || p.todos.length ? " hot" : ""}" aria-label="${escAttr(p.name)}"><div class="st-ph">${esc(p.name)}</div>${h}</section>`;
  };
  const repair = dmg.length ? `<section class="st-proj hot st-repair" aria-label="Repositories need repair"><div class="st-ph">Repositories need repair</div>${dmg.map(({ host, d }) => {
    const n = state.agents.filter((a) => repoDamageOf(a) === d).length;
    const since = Date.parse(d.since);
    return `<div class="st-row"><span class="st-n">${esc(multiHost() ? `${host} · ${d.repo}` : d.repo)}</span><span class="st-m warn">${esc(d.reason)}${d.ref ? ` at <code>${esc(d.ref)}</code>` : ""}</span><span class="st-r">${Number.isNaN(since) ? "" : "since " + esc(fmtAgo(since)) + " ago"}</span>` +
      `<div class="st-hint">${esc(d.hint)}</div><div class="st-m">${esc(d.path)} · found by ${esc(d.source)}${n ? ` · ${n} ${n === 1 ? "agent works" : "agents work"} in it` : ""}</div></div>`;
  }).join("")}</section>` : "";
  return `<div class="st"><p class="st-sum">${esc(sum)}</p>${repair}${tmpCardHtml()}${idxCardHtml()}${boardHtml()}${ps.map(card).join("")}</div>`;
}

/* ---------- hub-checked block beside a report ---------- */
// What the hub could look up about a worker's work (commits named, pushed, CI of the head, the worktree), and the
// claims of the report that contradict it. Fetched when a report is opened, kept for a minute.
const hubChk = new Map(); // entry id -> { at, data, busy }
const HUBCHK_MARK = { ok: "✓", warn: "!", bad: "✗", info: "·" };

function hubChkHTML(h) {
  if (!h || (!h.items.length && !h.note)) return "";
  const n = h.contradictions || 0;
  const head = n ? `${n} claim${n === 1 ? "" : "s"} in the report contradict${n === 1 ? "s" : ""} what the hub found` : "Hub-checked";
  const rows = h.items.map((i) => `<li class="${escAttr(i.state)}"><span class="hc-m" aria-hidden="true">${HUBCHK_MARK[i.state] || "·"}</span><span>${i.state === "bad" ? '<span class="vh">Contradicts: </span>' : ""}${esc(i.text)}</span></li>`).join("");
  return `<div class="hc-h">${esc(head)}</div>${h.note ? `<div class="hc-n">${esc(h.note)}</div>` : ""}<ul>${rows}</ul><div class="hc-cp">${cpOut(".hubchk", "Copy", "Copy the hub's checks")}</div>`;
}

function showHubChk(id) {
  const c = hubChk.get(id);
  const html = c && c.data ? hubChkHTML(c.data) : "";
  const bad = c && c.data && c.data.contradictions > 0;
  for (const el of document.querySelectorAll(`.hubchk[data-id="${CSS.escape(id)}"]`)) {
    el.innerHTML = html;
    el.classList.toggle("bad", !!bad);
    el.hidden = !html;
  }
}

function fillHubChecks(root) {
  for (const el of root.querySelectorAll(".hubchk[data-id]")) {
    const id = el.dataset.id;
    const c = hubChk.get(id);
    if (c && c.data) showHubChk(id);
    if (c && (c.busy || Date.now() - c.at < 60000)) continue;
    hubChk.set(id, { ...c, busy: true });
    api(`/api/ledger/${encodeURIComponent(id)}/hubcheck`)
      .then((data) => hubChk.set(id, { at: Date.now(), data }))
      .catch(() => hubChk.set(id, { at: Date.now(), data: c && c.data }))
      .finally(() => showHubChk(id));
  }
}

// renderStatus fills the desktop page and, on the phone, the Status screen.
function renderStatus() {
  for (const [el, desktop] of [[$("ps-body"), true], [$("ss-body"), false]]) {
    if (!el) continue;
    if (desktop && state.view !== "status") continue; // only the page on screen
    if (!desktop && !sts.open) continue;
    if (Date.now() - board.at > 20000) loadBoard();
    loadIdx();
    const html = statusHTML(desktop);
    if (el._html === html) continue;
    el._html = html;
    const sc = el.closest(".page-body, .ss-body");
    const y = sc ? sc.scrollTop : 0;
    const key = el.contains(document.activeElement) ? document.activeElement.dataset.bk : "";
    el.innerHTML = html;
    fillHubChecks(el);
    if (sc) sc.scrollTop = y;
    if (key) el.querySelector(`[data-bk="${key}"]`)?.focus({ preventScroll: true });
  }
}

function onStatusClick(ev) {
  const g = ev.target.closest("[data-go]"), a = ev.target.closest("[data-agent]");
  const bm = ev.target.closest("[data-bd-mode]"), bt = ev.target.closest("[data-bd-todo]"), bp = ev.target.closest("[data-bd-park]");
  if (bp) openParkSheet(bp.dataset.bdPark, bp.dataset.host);
  else if (bm) {
    board.mode = bm.dataset.bdMode === "project" ? "project" : "status";
    lsSet(BOARD_MODE_KEY, board.mode);
    for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
    renderStatus();
  } else if (ev.target.closest("[data-bd-seen]")) boardMarkSeen();
  else if (bt) boardGoTodo(bt.dataset.bdTodo);
  else if (g) openPage(g.dataset.go);
  else if (a) openDetail(a.dataset.agent);
}

// An opened report is kept across re-renders, and rendered when first opened.
function onStatusToggle(ev) {
  const d = ev.target;
  if (d.matches && d.matches("details.bd-lane")) {
    if (d.open === board.open.has(d.dataset.lane)) return;
    if (d.open) board.open.add(d.dataset.lane); else board.open.delete(d.dataset.lane);
    for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
    renderStatus();
    return;
  }
  if (!d.matches || !d.matches("details.st-row")) return;
  if (d.open === statusUi.open.has(d.dataset.id)) return; // a re-rendered <details open> fires toggle too: not a change
  if (d.open) statusUi.open.add(d.dataset.id); else statusUi.open.delete(d.dataset.id);
  for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
  renderStatus();
}

/* ---------- Peers: each host's status and agent settings ---------- */
//
//   #/peers            the list
//   #/peers/<name>     one host: how the hub sees it, what it can start, and
//                      the settings it applies when a start names none
//
// The settings live on the host itself (settings.json there); the hub reads
// and writes a peer's through /api/hosts/<name>/settings. Everything shown
// here that a host or agent chose (names, models) goes through esc().
const prs = {
  route: null, timer: null, seq: 0,
  info: {},      // name -> the hub's record of a peer (urls, source, added by); /api/peers, hub only
  hubVersion: "",
  cfg: {},       // name -> { state: "loading"|"ok"|"old"|"err", data, err }
  cats: {},      // name -> { kind -> catalog } once loaded
  draft: null,   // the form being edited: { host, rev, default_kind, allowed: [], kinds: { kind: { model, thinking } } }
  saving: false,
};
const PEER_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

function parsePeersRoute() {
  const m = location.hash.match(/^#\/peers(?:\/([^/]+))?$/);
  if (!m) return null;
  // "~releases" cannot be a host name (PEER_NAME_RE), so the tab shares the route.
  if (m[1] === "~releases") return { name: "", tab: "releases" };
  if (m[1] === "~prices") return { name: "", tab: "prices" };
  return { name: m[1] && PEER_NAME_RE.test(m[1]) ? m[1] : "", tab: "hosts" };
}
function tabHash(tab) { return tab === "releases" ? RELEASES_HASH : tab === "prices" ? PRICES_HASH : "#/peers"; }
function peerHash(name) { return "#/peers" + (name ? "/" + name : ""); }
function isLocalHostName(name) { return !!state.localHost && name === state.localHost; }
function settingsBase(name) { return isLocalHostName(name) ? "/api/settings" : `/api/hosts/${encodeURIComponent(name)}/settings`; }

async function peersLoadInfo() {
  try {
    const r = await api("/api/peers");
    prs.info = {};
    for (const p of r.peers || []) prs.info[p.name] = p;
    prs.hubVersion = r.version || "";
  } catch { /* not a hub: only this host is listed */ }
}

async function peersLoadCfg(name, force) {
  const h = hostStatus(name);
  if (!h || (h.status && h.status !== "ok")) { prs.cfg[name] = { state: "down" }; return; }
  const cur = prs.cfg[name];
  if (!cur) prs.cfg[name] = { state: "loading" };
  else if (cur.state === "loading" && !force) return;
  try {
    const data = await api(settingsBase(name));
    prs.cfg[name] = { state: "ok", data };
    // A form with no edits follows the host.
    if (prs.draft && prs.draft.host === name && !draftDirty()) prs.draft = draftFrom(name, data);
  } catch (e) {
    prs.cfg[name] = e.status === 404 ? { state: "old" } : { state: "err", err: e.message };
  }
  renderPeers();
}

async function peersLoadCats(name) {
  if (prs.cats[name]) return;
  prs.cats[name] = {};
  try {
    const d = await api(modelsBase(isLocalHostName(name) ? null : name));
    prs.cats[name] = Object.fromEntries((d.catalogs || []).map((c) => [c.kind, c]));
  } catch { prs.cats[name] = {}; }
  renderPeers();
}

// A hosts list that arrives (or changes) while Peers is open: draw it now, and load the settings of a host that
// was not reachable (or not known yet) when the screen opened, instead of waiting for the 20 s tick.
function peersHostsChanged() {
  if (!prs.route) return;
  for (const h of state.hosts || []) {
    const c = prs.cfg[h.name];
    if (!c || (c.state === "down" && peerStatusOf(h) === "ok")) peersLoadCfg(h.name, true);
  }
  if (!pz.nodes) loadProvNodes(false); // skipped at open while the ledger or the hosts were not loaded yet
  renderPeers();
}

function peersLoadAll() {
  loadReleases(false);
  loadProvNodes(false);
  if (pz.nodes && pz.nodes.enabled) loadProvHosts(false);
  peersLoadInfo().then(renderPeers);
  for (const h of state.hosts) peersLoadCfg(h.name);
}

function draftFrom(name, data) {
  const s = (data && data.settings) || {};
  const kinds = {};
  for (const [k, v] of Object.entries(s.kinds || {})) kinds[k] = { model: v.model || "", thinking: v.thinking || "" };
  return { host: name, rev: s.rev || 0, default_kind: s.default_kind || "", allowed: [...(s.allowed_kinds || [])], kinds, max_workers: s.max_workers_per_repo == null ? "" : String(s.max_workers_per_repo),
    // The scheduled sweep (sweep_loop): on unless switched off; "" minutes is the default hour. Hosts too old to have it send nothing.
    sweep_known: !!(data && data.sweep_loop && data.sweep_loop.hub), sweep_on: !(s.sweep_loop && s.sweep_loop.enabled === false), sweep_every: s.sweep_loop && s.sweep_loop.every_minutes ? String(s.sweep_loop.every_minutes) : "" };
}

function draftBody(d) {
  const kinds = {};
  for (const [k, v] of Object.entries(d.kinds)) if ((v.model || "").trim() || (v.thinking || "").trim()) kinds[k] = { model: (v.model || "").trim(), thinking: (v.thinking || "").trim() };
  const body = { default_kind: d.default_kind, allowed_kinds: d.allowed, kinds, rev: d.rev };
  const mw = String(d.max_workers == null ? "" : d.max_workers).trim();
  if (/^\d+$/.test(mw)) body.max_workers_per_repo = Number(mw);
  if (d.sweep_known) {
    const sl = {}, ev = String(d.sweep_every || "").trim();
    if (!d.sweep_on) sl.enabled = false;
    if (/^\d+$/.test(ev)) sl.every_minutes = Number(ev);
    body.sweep_loop = sl; // {} puts the default back
  }
  return body;
}

function draftDirty() {
  const d = prs.draft, c = d && prs.cfg[d.host];
  if (!d || !c || c.state !== "ok") return false;
  return JSON.stringify(draftBody(d)) !== JSON.stringify(draftBody(draftFrom(d.host, c.data)));
}

function peerStatusOf(h) { return h.local ? "ok" : (h.status || "connecting"); }
function peerStatusText(h) {
  const x = wakeOf(h);
  if (x) return x.kind === "stopped" ? "Stopped" : x.kind === "starting" ? "Starting…" : "Failed to start";
  return ({ ok: "Connected", down: "Down", connecting: "Connecting" })[peerStatusOf(h)] || peerStatusOf(h);
}

function peerDefaultText(name) {
  const c = prs.cfg[name];
  if (!c || c.state !== "ok") return "";
  const e = c.data.effective || {}, k = e.default_kind;
  if (!k) return "default: not set";
  const m = ((c.data.settings || {}).kinds || {})[k];
  return "default: " + k + (m && m.model ? " · " + m.model : "");
}

function renderPeersList() {
  const r = prs.route;
  const hosts = state.hosts || [];
  $("pr-count").textContent = hosts.length ? `${hosts.length} host${hosts.length === 1 ? "" : "s"}` : "";
  if (!hosts.length) { $("pr-items").innerHTML = '<div class="pr-empty">Loading…</div>'; return; }
  const html = hosts.map((h) => {
    const st = peerStatusOf(h);
    const mis = h.version_mismatch ? ` · <span class="warn">differs from the hub (${esc(prs.hubVersion || "?")})</span>` : "";
    const def = peerDefaultText(h.name);
    const old = prs.cfg[h.name] && prs.cfg[h.name].state === "old" ? " · <span class=\"warn\">needs an upgrade for settings</span>" : "";
    return `<a class="pr-card${r.name === h.name ? " current" : ""}" href="${esc(peerHash(h.name))}" data-name="${esc(h.name)}">
      <span class="pr-name"><span class="pr-st ${esc(st)}" aria-hidden="true"></span>${esc(h.name)}${h.local ? ' <span class="pr-meta">(this host)</span>' : ""}${hostNeedsSignin(h.name) ? '<span class="pr-need" role="img" aria-label="an agent here is signed out" title="An agent on this host is signed out"></span>' : ""}</span>
      <span class="pr-meta">${esc(peerStatusText(h))} · ${esc(h.version || "version unknown")}${mis} · ${h.no_agents ? "no agents here" : (h.agents || 0) + " agent" + (h.agents === 1 ? "" : "s")}</span>
      ${wakeOf(h) ? `<span class="pr-meta">${esc(wakeWhat(wakeOf(h)))}</span>` : ""}${def || old ? `<span class="pr-meta">${esc(def)}${old}</span>` : ""}${phHost(h.name) && !phHost(h.name).adopted ? `<span class="pr-meta pr-spend">built from the app · ${esc(phSpendText(phHost(h.name)))}</span>` : ""}</a>${wakeOf(h) ? `<div class="wk-line" data-wk-for="${esc(h.name)}">${wakeAction(h, wakeOf(h))}</div>` : ""}`;
  }).join("");
  const nudge = rls.data && rls.data.needs_you && rls.data.todo
    ? `<a class="pr-nudge" href="${RELEASES_HASH}">${esc(rls.data.todo.release.version)} needs you: ${esc(rls.data.needs_text || "see the Releases tab")} →</a>` : "";
  const browse = pz.nodes && pz.nodes.enabled
    ? `<a class="pr-browse" href="${PRICES_HASH}" id="pr-browse">Browse prices<span>${esc(pzDefaultNode() || "")}</span></a>` : "";
  // A provisioner's worker config whose cloud could not be read (an expired AWS login) is said here, for that config
  // only: the hosts of the other configs are listed and startable as usual.
  const cloud = ph.data && (ph.data.nodes || []).filter((n) => n.stale).map((n) => `${n.name}: ${n.stale}`);
  const cloudNote = cloud && cloud.length ? `<p class="pz-note warn pr-cloud" role="status">Not refreshed from the cloud: ${esc(cloud.join("; "))}</p>` : "";
  const all = browse + html + cloudNote + nudge;
  if ($("pr-items")._html !== all) { $("pr-items")._html = all; $("pr-items").innerHTML = all; }
}

// The Hosts | Releases tabs. The Releases tab carries an orange dot while a
// release needs the user.
function renderPeerTabs() {
  const r = prs.route, tab = r.tab || "hosts", rel = tab === "releases", prz = tab === "prices";
  $("peers").classList.toggle("tab-rel", rel);
  // The Prices tab exists only where a provisioner is set up (or while it is open).
  $("pr-tab-prices").hidden = !(pz.nodes && pz.nodes.enabled) && !prz;
  for (const b of document.querySelectorAll("#pr-tabs [data-tab]")) {
    const on = b.dataset.tab === tab;
    b.setAttribute("aria-selected", String(on));
    b.tabIndex = on ? 0 : -1;
  }
  const need = !!(rls.data && rls.data.needs_you);
  const dot = $("pr-dot");
  dot.hidden = !need;
  dot.textContent = need ? "1" : "";
  dot.setAttribute("aria-label", need ? "a release needs you: " + (rls.data.needs_text || "") : "");
  $("pr-tab-rel").title = need ? (rls.data.needs_text || "") : "";
  $("pr-releases").hidden = !rel;
  $("pr-prices").hidden = !prz;
  document.querySelector("#peers .pr-panes").hidden = rel || prz;
}

function fmtWhen(t) { const at = Date.parse(t); return Number.isNaN(at) ? "" : fmtAgo(at) + " ago"; }

function maxwNote(d, eff) {
  if (eff.max_workers_per_repo == null) return "This host is too old to apply a cap.";
  if (d.max_workers === "") return `Unset: the default of ${eff.max_workers_per_repo_source === "default" ? eff.max_workers_per_repo : 4} applies.`;
  return d.max_workers === "0" ? "0: no cap." : "A handoff over this waits, and starts when a worker in that repository finishes.";
}
function sweepNote(d, sw) {
  if (!d.sweep_on) return "Off: the hub still marks finished sessions as ready to remove, but removes nothing itself. You remove them in the removals review (or `comebye sweep` by hand).";
  const last = sw && sw.last_run_at ? `Last ran ${fmtWhen(sw.last_run_at)}: marked ${sw.last_marked}, ${sw.last_needs_look} need a look.` : "It has not run yet.";
  return `Removes finished sessions that have nothing to lose (clean, every commit in the default branch, nothing unpushed, checked on the host just before), and deletes their stale remote branches when git shows every commit is in the default branch and no pull request is open. Files no To do: what it did is on the board and in the removals review, and a deleted branch is logged with its SHA. Sessions that need a look stay in the review. ${last}`;
}
function peerSettingsForm(h, c, d) {
  const data = c.data;
  const installed = data.installed_kinds || [];
  const kindsAll = [...new Set([...installed, ...d.allowed, d.default_kind].filter(Boolean))];
  const modelKinds = data.model_kinds || [];
  const cats = prs.cats[h.name] || {};
  const eff = data.effective || {};
  const dsrc = eff.default_source === "flag" ? "Unset here: this host's -default-kind applies" : eff.default_source === "auto" ? "Unset: this host uses the kind most of its running agents use" : "";
  const sel = (k) => `<option value="${esc(k)}"${d.default_kind === k ? " selected" : ""}>${esc(k)}${installed.includes(k) ? "" : " (not installed)"}</option>`;
  const lists = kindsAll.filter((k) => modelKinds.includes(k)).map((k) => {
    const v = d.kinds[k] || { model: "", thinking: "" }, cat = cats[k] || {};
    const opts = (cat.models || []).map((m) => `<option value="${esc(m.id)}"></option>`).join("");
    const think = k === "pi" ? `<div class="pr-field"><label for="pr-th-${esc(k)}">Thinking</label><select id="pr-th-${esc(k)}" data-think="${esc(k)}">
        <option value="">Agent's own${cat.default_thinking ? " (" + esc(cat.default_thinking) + ")" : ""}</option>
        ${(cat.thinking_levels || THINKING_LEVELS).map((l) => `<option value="${esc(l)}"${v.thinking === l ? " selected" : ""}>${esc(l)}</option>`).join("")}</select></div>` : "";
    return `<div class="pr-kind"><b>${esc(k)}</b><div class="pr-row">
      <div class="pr-field"><label for="pr-m-${esc(k)}">Model</label><input type="text" id="pr-m-${esc(k)}" data-model="${esc(k)}" value="${esc(v.model)}" list="pr-dl-${esc(k)}" placeholder="${esc(cat.default ? "Agent's own (" + cat.default + ")" : "Agent's own default")}" autocomplete="off" autocapitalize="off" spellcheck="false" maxlength="128"><datalist id="pr-dl-${esc(k)}">${opts}</datalist></div>
      ${think}</div>${cat.error ? `<div class="pr-note warn">Could not list ${esc(k)} models here: ${esc(cat.error)}</div>` : ""}</div>`;
  }).join("");
  const dirty = draftDirty();
  return `<form class="pr-form" id="pr-form" autocomplete="off">
    <div class="pr-field"><label for="pr-default">Default agent</label>
      <select id="pr-default"><option value="">No default (guess)</option>${kindsAll.map(sel).join("")}</select>
      <div class="pr-note">Used when a launch, handoff or plan step names no agent. A request that names another one still wins, with a warning.${dsrc ? " " + esc(dsrc) + "." : ""}</div></div>
    <div class="pr-field"><span class="pr-lbl" id="pr-allow-l">Allowed agents</span>
      <div class="pr-checks" role="group" aria-labelledby="pr-allow-l">${kindsAll.map((k) => `<label class="pr-check"><input type="checkbox" data-allow="${esc(k)}"${d.allowed.includes(k) ? " checked" : ""}>${esc(k)}</label>`).join("") || '<span class="pr-note">No agent is installed here.</span>'}</div>
      <div class="pr-note">${d.allowed.length ? "Only these are started here; anything else is refused." : "None ticked: any installed agent may be started."}${eff.allowed_source === "flag" && !d.allowed.length ? " This host's -allowed-kinds applies." : ""}</div></div>
    <div class="pr-field"><label for="pr-maxw">Most running workers per repository</label>
      <input type="number" id="pr-maxw" min="0" max="1000" step="1" inputmode="numeric" value="${esc(d.max_workers)}" placeholder="${esc(eff.max_workers_per_repo_source === "default" || eff.max_workers_per_repo == null ? "4 (default)" : String(eff.max_workers_per_repo))}" autocomplete="off">
      <div class="pr-note" id="pr-maxw-note">${maxwNote(d, eff)} Agents cannot change it.</div></div>
    ${d.sweep_known ? `<div class="pr-field"><span class="pr-lbl" id="pr-sweep-l">Finished sessions</span>
      <div class="pr-checks" role="group" aria-labelledby="pr-sweep-l"><label class="pr-check"><input type="checkbox" id="pr-sweep-on"${d.sweep_on ? " checked" : ""}>Clean up automatically</label>
        <label class="pr-check" for="pr-sweep-every">every <input type="number" id="pr-sweep-every" min="15" max="1440" step="5" inputmode="numeric" value="${esc(d.sweep_every)}" placeholder="60" autocomplete="off"${d.sweep_on ? "" : " disabled"}> minutes</label></div>
      <div class="pr-note" id="pr-sweep-note">${esc(sweepNote(d, data.sweep_loop))}</div></div>` : ""}
    ${lists ? `<div class="pr-field"><span class="pr-lbl">Model and thinking by agent</span>${lists}<div class="pr-note">Empty passes no flag: the agent uses its own setting. A request's model wins.</div></div>` : ""}
    <div class="pr-acts"><button type="submit" class="btn primary" id="pr-save"${dirty && !prs.saving ? "" : " disabled"}>${prs.saving ? "Saving…" : "Save"}</button>
      <button type="button" class="btn" id="pr-revert"${dirty ? "" : " disabled"}>Revert</button>
      <span class="pr-note" id="pr-dirty">${dirty ? "Unsaved changes" : ""}</span></div>
  </form>`;
}

// ---- Sign an agent in on a host (internal/signin). The link comes from the host; the user opens it, copies the
// one-time code the page shows and pastes it here. Nothing is kept: the code goes to the host and out of the box.
const sis = { by: {}, timer: null }; // by host: { agent, id, state, url, reason, message, method, err, copied }
const SIGNIN_STORE = "comebye.signin";
const sinBase = (host) => isLocalHostName(host) ? "/api/signin" : `/api/hosts/${encodeURIComponent(host)}/signin`;
function sinSave() {
  try { sessionStorage.setItem(SIGNIN_STORE, JSON.stringify(Object.fromEntries(Object.entries(sis.by).filter(([, v]) => v.id).map(([h, v]) => [h, { agent: v.agent, id: v.id }])))); } catch {}
}
function sinStored(host) {
  try { return (JSON.parse(sessionStorage.getItem(SIGNIN_STORE) || "{}") || {})[host] || null; } catch { return null; }
}
const sinActive = (v) => !!v && ["starting", "waiting_code", "checking"].includes(v.state);
// A call that keeps its answer's body on an error (a 409 carries the running sign-in).
async function sinCall(method, path, body) {
  try {
    const res = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) });
    return { status: res.status, body: await res.json().catch(() => ({})) };
  } catch (e) { return { status: 0, body: { message: e && e.name === "TimeoutError" ? "timed out — check the connection" : "could not reach the host" } }; }
}
function sinApply(host, snap) {
  const v = sis.by[host];
  if (!v || !snap) return;
  Object.assign(v, { id: snap.id, agent: snap.agent || v.agent, state: snap.state, url: snap.url || "", reason: snap.reason || "", message: snap.message || "", method: snap.method || "", code_input: snap.code_input !== false });
  v.err = "";
  sinSave();
  if (snap.state === "signed_in") peersLoadCfg(host, true); // the host probes again right after a sign-in
}
function sinFail(host, msg) {
  const v = sis.by[host];
  if (v) { v.state = "failed"; v.message = msg; v.reason = "error"; v.url = ""; sinSave(); }
}
function sinTick() {
  const hosts = Object.keys(sis.by).filter((h) => sinActive(sis.by[h]) && sis.by[h].id);
  if (!hosts.length) { clearInterval(sis.timer); sis.timer = null; return; }
  for (const host of hosts) sinPoll(host);
}
function sinWatch() { if (!sis.timer) sis.timer = setInterval(sinTick, 2000); }
async function sinPoll(host) {
  const v = sis.by[host];
  if (!v || !v.id) return;
  const r = await sinCall("GET", `${sinBase(host)}/${encodeURIComponent(v.id)}`);
  if (sis.by[host] !== v) return;
  if (r.status === 200) sinApply(host, r.body);
  else if (r.status === 404 && v.attaching) { delete sis.by[host]; sinSave(); } // a stale record of one that ended
  else if (r.status === 404) sinFail(host, "The sign-in is gone (the host may have restarted). Try again.");
  // anything else (a hiccup): keep what is shown and ask again
  renderPeers();
}
async function sinStart(host, agent) {
  const v = sis.by[host] = { agent, id: "", state: "starting", url: "", reason: "", message: "", method: "", err: "", copied: false, code_input: true };
  renderPeers();
  const r = await sinCall("POST", sinBase(host), { agent });
  if (sis.by[host] !== v) { // cancelled while starting: end what it started
    if (r.status === 200 && r.body.id) sinCall("DELETE", `${sinBase(host)}/${encodeURIComponent(r.body.id)}`);
    return;
  }
  if (r.status === 200) sinApply(host, r.body);
  else if (r.status === 409 && r.body.session) sinApply(host, r.body.session); // one is already running: attach to it
  else sinFail(host, r.body.message || "Could not start the sign-in");
  if (sinActive(v)) sinWatch();
  renderPeers();
}
async function sinSend(host) {
  const v = sis.by[host], box = $("si-code");
  if (!v || !box || v.sending) return;
  const code = box.value.trim();
  if (!code) { v.err = "Paste the code first."; $("pr-view")._html = ""; renderPeers(); return; }
  v.sending = true; v.err = "";
  const r = await sinCall("POST", `${sinBase(host)}/${encodeURIComponent(v.id)}/code`, { code });
  v.sending = false;
  if (r.status !== 400) box.value = ""; // never kept here
  if (sis.by[host] !== v) return;
  if (r.status === 200) { sinApply(host, r.body); sinWatch(); }
  else if (r.status === 400) v.err = r.body.message || "That is not a code.";
  else if (r.status === 404) sinFail(host, "The sign-in is gone. Try again.");
  else v.err = r.body.message || "Could not send the code.";
  $("pr-view")._html = ""; renderPeers();
}
async function sinCancel(host) {
  const v = sis.by[host];
  if (!v) return;
  delete sis.by[host]; sinSave();
  renderPeers();
  if (v.id && sinActive(v)) await sinCall("DELETE", `${sinBase(host)}/${encodeURIComponent(v.id)}`);
  peersLoadCfg(host, true);
}
function sinClose(host) { delete sis.by[host]; sinSave(); renderPeers(); }
// Back on a host's page: re-attach to a sign-in the page started before it was left or reloaded.
function sinReattach(host) {
  if (sis.by[host]) return;
  const st = sinStored(host);
  if (!st || !st.id) return;
  sis.by[host] = { agent: st.agent, id: st.id, state: "checking", url: "", reason: "", message: "", method: "", err: "", copied: false, code_input: true, attaching: true };
  sinPoll(host).then(() => { const v = sis.by[host]; if (v) v.attaching = false; if (sinActive(v)) sinWatch(); });
}

// What an installed agent's row says about its sign-in: a chip tone and text, and whether to offer the button.
function agentAuthOf(d, k) {
  const a = (d.agent_auth || {})[k];
  if (!a) return null;
  if (a.method === "api_key") return { tone: "green", text: "Key set", can: false };
  if (a.state === "signed_in") return { tone: "green", text: "Signed in" + (a.method ? " · " + a.method : ""), can: false };
  if (a.state === "signed_out") return { tone: "orange", text: "Signed out", can: !!a.can_sign_in };
  return a.can_sign_in ? { tone: "", text: "Not checked", can: true } : { tone: "", text: "", can: false }; // nothing to say of an agent with no sign-in here
}
const SIGNIN_CHIP = { starting: ["orange", "Starting…"], waiting_code: ["orange", "Waiting for the code"], checking: ["orange", "Checking…"], signed_in: ["green", "Signed in"], failed: ["red", "Failed"] };
function signinDialog(h, v) {
  const [tone, label] = SIGNIN_CHIP[v.state] || ["", v.state];
  const head = `<div class="si-head"><h4 id="si-title">Sign in to ${esc(v.agent)} on ${esc(h.name)}</h4><span class="chip ${tone}" id="si-chip" role="status">${esc(label)}${v.state === "signed_in" && v.method ? " · " + esc(v.method) : ""}</span></div>`;
  let body = "", acts = "";
  if (v.state === "starting") body = `<div class="pr-note">Asking ${esc(h.name)} for a sign-in link…</div>`;
  else if (v.state === "waiting_code") {
    body = `<div class="si-step"><span class="si-n">1</span><div class="si-col"><div>Open this link and sign in. It signs ${esc(v.agent)} in on ${esc(h.name)}.</div>
        <div class="si-link"><code id="si-url" tabindex="0">${esc(v.url)}</code><button type="button" class="btn" id="si-copy" data-si="copy">${v.copied ? "Copied" : "Copy"}</button></div>
        <a class="si-open" href="${esc(v.url)}" target="_blank" rel="noopener noreferrer">Open the link</a></div></div>
      <div class="si-step"><span class="si-n">2</span><div class="si-col"><label for="si-code">Paste the code that page shows</label>
        <div class="si-send"><input type="text" id="si-code" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" inputmode="text" maxlength="512" placeholder="Code">
        <button type="button" class="btn primary" data-si="send"${v.sending ? " disabled" : ""}>${v.sending ? "Sending…" : "Send"}</button></div>
        ${v.err ? `<div class="pr-note warn" role="alert">${esc(v.err)}</div>` : ""}</div></div>`;
    acts = `<button type="button" class="btn" data-si="cancel">Cancel</button>`;
  } else if (v.state === "checking") {
    body = `<div class="pr-note">Checking that ${esc(v.agent)} is signed in on ${esc(h.name)}…</div>`;
    acts = `<button type="button" class="btn" data-si="cancel">Cancel</button>`;
  } else if (v.state === "signed_in") {
    body = `<div class="pr-note">${esc(v.agent)} is signed in on ${esc(h.name)}.</div>`;
    acts = `<button type="button" class="btn primary" data-si="close">Done</button>`;
  } else { // failed
    body = `<div class="pr-note err" role="alert">${esc((v.message || "The sign-in failed.").replace(/^./, (c) => c.toUpperCase()))}</div>`;
    acts = `<button type="button" class="btn primary" data-si="retry">Try again</button><button type="button" class="btn" data-si="close">Close</button>`;
  }
  if (v.state === "starting") acts = `<button type="button" class="btn" data-si="cancel">Cancel</button>`;
  return `<div class="si-dialog" role="dialog" aria-labelledby="si-title" data-state="${esc(v.state)}">${head}${body}${acts ? `<div class="si-acts">${acts}</div>` : ""}</div>`;
}
function agentRows(h, d, cats) {
  const kinds = d.installed_kinds || [];
  if (!kinds.length) return '<span class="pr-note">None found on this host.</span>';
  const v = sis.by[h.name];
  return `<div class="pr-agents">` + kinds.map((k) => {
    const ct = cats[k], au = agentAuthOf(d, k);
    const meta = [ct && ct.models ? `${ct.models.length} model${ct.models.length === 1 ? "" : "s"}` : "", ct && ct.default ? ct.default : ""].filter(Boolean).join(" · ");
    const mine = v && v.agent === k;
    let tone = au ? au.tone : "", text = au ? au.text : "";
    if (mine && v.state !== "signed_in") [tone, text] = SIGNIN_CHIP[v.state] || [tone, text];
    const btn = au && au.can && !mine && !(v && sinActive(v)) ? `<button type="button" class="btn si-btn" data-signin="${esc(k)}">Sign in</button>` : "";
    return `<div class="pr-agent" data-agent="${esc(k)}"><div class="pa-name"><b>${esc(k)}</b>${meta ? `<span class="pr-note">${esc(meta)}</span>` : ""}</div>${text ? `<span class="chip ${tone}" data-chip="${esc(k)}">${esc(text)}</span>` : ""}${btn}</div>`;
  }).join("") + `</div>` + (v ? signinDialog(h, v) : "");
}
// A host with an agent signed out needs the user: the orange dot on its row in the list.
function hostNeedsSignin(name) {
  const c = prs.cfg[name];
  return !!(c && c.state === "ok" && Object.values(c.data.agent_auth || {}).some((a) => a && a.state === "signed_out" && a.method !== "api_key"));
}

function renderPeerView() {
  const r = prs.route, view = $("pr-view");
  $("peers").classList.toggle("in-peer", !!r.name);
  if (!r.name) { view.dataset.name = ""; view.innerHTML = '<div class="pr-empty" style="margin:auto;text-align:center">Pick a host from the list.</div>'; return; }
  const h = (state.hosts || []).find((x) => x.name === r.name);
  if (!h) { view.dataset.name = ""; view.innerHTML = `<div class="pr-empty" style="margin:auto;text-align:center">${(state.hosts || []).length ? "That host is gone." : "Loading…"}</div>`; return; }
  const c = prs.cfg[h.name] || { state: "loading" };
  if (view.dataset.name !== h.name) { // a different host: a fresh form, and its models
    view.dataset.name = h.name;
    prs.draft = c.state === "ok" ? draftFrom(h.name, c.data) : null;
    if (c.state === "ok") peersLoadCats(h.name);
  }
  if (c.state === "ok" && (!prs.draft || prs.draft.host !== h.name)) { prs.draft = draftFrom(h.name, c.data); peersLoadCats(h.name); }
  const st = peerStatusOf(h), info = prs.info[h.name], wx = wakeOf(h);
  // A worker the cloud says is stopped is stopped, not "down, no response".
  const statusRow = wx
    ? `${wx.kind === "starting" ? "Starting" : wx.kind === "failed" ? "Stopped (failed to start)" : "Stopped"}${wx.p && wx.p.stopped_at && wx.kind !== "starting" ? " since " + esc(fmtWhen(wx.p.stopped_at)) : ""}`
    : `${esc(peerStatusText(h))}${h.since && st !== "ok" ? " since " + esc(fmtWhen(h.since)) : ""}${h.error ? ' <span class="warn">' + esc(h.error) + "</span>" : ""}`;
  const rows = [
    ["Status", statusRow],
    ["Version", `${esc(h.version || "unknown")}${h.version_mismatch ? ` <span class="warn">(the hub runs ${esc(prs.hubVersion || "another version")})</span>` : ""}`],
    ["Agents", h.no_agents ? "none by design (no herdr here)" : String(h.agents || 0)],
  ];
  if (h.last_seen && !h.local) rows.push(["Last seen", esc(fmtWhen(h.last_seen))]);
  if (info) {
    rows.push(["Address", (info.urls || []).map(esc).join("<br>")]);
    rows.push(["Source", info.source === "static" ? "Static: set in the hub's -peers, changed by editing its environment" : "Added" + (info.added_by ? " by " + esc(info.added_by) : "") + (info.added_at ? " " + esc(fmtWhen(info.added_at)) : "")]);
  } else if (h.local) rows.push(["Source", "This host"]);
  let reach = "", body = "";
  if (c.state === "ok") {
    const d = c.data, l = d.launch || {};
    rows.push(["Starting agents", l.enabled ? "On" : `<span class="warn">Off: ${esc(l.reason || "not available")}</span>`]);
    const cats = prs.cats[h.name] || {};
    sinReattach(h.name);
    reach = `<div class="pr-sec"><h3>Installed agents</h3>${agentRows(h, d, cats)}</div>`;
    if (d.effective && d.effective.notice) reach += `<div class="pr-sec"><div class="pr-note warn">${esc(d.effective.notice)}</div></div>`;
    const s = d.settings || {};
    body = `<div class="pr-sec"><h3>Defaults for this host</h3>${peerSettingsForm(h, c, prs.draft)}
      <div class="pr-note">${s.rev ? "Revision " + s.rev + (s.updated_by ? " · set by " + esc(s.updated_by) : "") + (s.updated_at ? " · " + esc(fmtWhen(s.updated_at)) : "") : "Nothing has been set on this host yet."}</div></div>`;
  } else if (c.state === "old") {
    body = `<div class="pr-sec"><h3>Defaults for this host</h3><div class="pr-note warn">This host runs ${esc(h.version || "an older version")}, which has no host settings. Upgrade it to set a default agent, model and thinking level.</div></div>`;
  } else if (c.state === "down") {
    body = `<div class="pr-sec"><h3>Defaults for this host</h3><div class="pr-note">Settings are kept on the host itself, ${wx ? "which is stopped" : "which is not answering"}. They can be edited again when it is back.</div></div>`;
  } else if (c.state === "err") {
    body = `<div class="pr-sec"><h3>Defaults for this host</h3><div class="pr-note warn">${esc(c.err || "Could not read the settings")}</div></div>`;
  } else body = '<div class="pr-sec"><div class="pr-note">Loading…</div></div>';
  const head = `<div class="pr-bar"><h2><span class="pr-st ${esc(st)}" aria-hidden="true"></span>${esc(h.name)}</h2></div>
    <div class="pr-sec"><h3>Host</h3><dl class="pr-kv">${rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join("")}</dl></div>`;
  // Keep the focus and caret in a field being typed in across a redraw.
  const ae = document.activeElement, keep = ae && view.contains(ae) && ae.id ? { id: ae.id, s: ae.selectionStart, e: ae.selectionEnd } : null;
  const wsec = wx ? `<div class="pr-sec wk-sec"><h3>${esc(wakeChip(wx))}</h3><div class="pr-note">${esc(wakeWhat(wx))}</div>${wakeAction(h, wx)}</div>` : "";
  const html = head + wsec + reach + body;
  if (view._html === html) return;
  view._html = html;
  const typed = document.getElementById("si-code"), typedV = typed && view.contains(typed) ? typed.value : "";
  view.innerHTML = html;
  const again = typedV && document.getElementById("si-code");
  if (again) again.value = typedV; // the code being pasted survives a redraw (it is never kept anywhere else)
  if (keep) {
    const el = document.getElementById(keep.id);
    if (el) { el.focus(); try { if (keep.s != null) el.setSelectionRange(keep.s, keep.e); } catch {} }
  }
}

function renderPeers() {
  if (!prs.route) return;
  renderPeerTabs();
  if (prs.route.tab === "releases") { renderReleases(); return; }
  if (prs.route.tab === "prices") { renderPrices(); return; }
  renderPeersList();
  renderPeerView();
}

function syncPeers() {
  const r = parsePeersRoute();
  const el = $("peers");
  const was = !el.hidden;
  prs.route = r;
  el.hidden = !r;
  if (!r) { clearInterval(prs.timer); prs.timer = null; return; }
  if (!was) {
    prs.cfg = {}; prs.cats = {}; prs.draft = null; $("pr-view").dataset.name = ""; $("pr-view")._html = "";
    rls.confirm = null; $("pr-releases")._html = ""; $("pr-prices")._html = "";
    prs.timer = setInterval(peersLoadAll, pollMs(20000));
    peersLoadAll();
  }
  renderPeers();
}

function closePeers() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}
function peersBack() {
  if (prs.route && prs.route.name && !isDesktop()) location.hash = "#/peers";
  else closePeers();
}

async function peersSave() {
  const d = prs.draft;
  if (!d || prs.saving || !draftDirty()) return;
  prs.saving = true;
  $("pr-view")._html = ""; renderPeers();
  try {
    const data = await api(settingsBase(d.host), { method: "PUT", body: JSON.stringify(draftBody(d)) });
    prs.cfg[d.host] = { state: "ok", data };
    prs.draft = draftFrom(d.host, data);
    toast("Saved");
  } catch (e) {
    if (e.code === "rev_conflict") {
      toast("Someone else changed these settings. Reloaded; check them and save again.", true);
      prs.draft = null;
      await peersLoadCfg(d.host, true);
    } else toast(e.message, true);
  }
  prs.saving = false;
  $("pr-view")._html = ""; renderPeers();
}

function initPeers() {
  $("pr-back").addEventListener("click", peersBack);
  $("pr-tabs").addEventListener("click", (ev) => {
    const b = ev.target.closest("[data-tab]");
    if (b) location.hash = tabHash(b.dataset.tab);
  });
  $("pr-tabs").addEventListener("keydown", (ev) => {
    if (ev.key !== "ArrowLeft" && ev.key !== "ArrowRight") return;
    ev.preventDefault();
    const tabs = [...document.querySelectorAll("#pr-tabs [data-tab]")].filter((b) => !b.hidden);
    const at = tabs.findIndex((b) => b.getAttribute("aria-selected") === "true");
    const next = tabs[Math.max(0, Math.min(tabs.length - 1, at + (ev.key === "ArrowRight" ? 1 : -1)))];
    location.hash = tabHash(next.dataset.tab);
    next.focus();
  });
  initPrices();
  initAddHost();
  initProvHosts();
  $("pr-releases").addEventListener("click", (ev) => {
    const b = ev.target.closest("[data-rel]");
    if (b && !b.disabled) relClick(b.dataset.rel, b.dataset.host);
  });
  $("peers").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.stopPropagation(); peersBack(); } });
  const v = $("pr-view");
  const touch = () => { v._html = ""; /* the dirty marks change; the fields keep what was typed */ };
  v.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && ev.target.id === "si-code") { ev.preventDefault(); sinSend(prs.route.name); }
  });
  v.addEventListener("input", (ev) => {
    const d = prs.draft, t = ev.target;
    if (!d) return;
    if (t.id === "pr-maxw") { d.max_workers = t.value.replace(/[^0-9]/g, ""); refreshDirty(); const n = $("pr-maxw-note"), c = prs.cfg[d.host]; if (n && c && c.data) n.textContent = maxwNote(d, c.data.effective || {}) + " Agents cannot change it."; }
    else if (t.id === "pr-sweep-every") { d.sweep_every = t.value.replace(/[^0-9]/g, ""); refreshDirty(); }
    else if (t.dataset.model) { (d.kinds[t.dataset.model] ||= { model: "", thinking: "" }).model = t.value; refreshDirty(); }
  });
  v.addEventListener("change", (ev) => {
    const d = prs.draft, t = ev.target;
    if (!d) return;
    if (t.id === "pr-default") d.default_kind = t.value;
    else if (t.id === "pr-sweep-on") d.sweep_on = t.checked;
    else if (t.dataset.allow) d.allowed = t.checked ? [...new Set([...d.allowed, t.dataset.allow])] : d.allowed.filter((k) => k !== t.dataset.allow);
    else if (t.dataset.think) (d.kinds[t.dataset.think] ||= { model: "", thinking: "" }).thinking = t.value;
    else return;
    touch(); renderPeers();
  });
  v.addEventListener("submit", (ev) => { ev.preventDefault(); peersSave(); });
  v.addEventListener("click", (ev) => {
    const host = prs.route && prs.route.name;
    const si = ev.target.closest("[data-si], [data-signin]");
    if (si && host) {
      const a = si.dataset.signin, op = si.dataset.si, cur = sis.by[host];
      if (a) sinStart(host, a);
      else if (op === "send") sinSend(host);
      else if (op === "cancel") sinCancel(host);
      else if (op === "close") sinClose(host);
      else if (op === "retry" && cur) sinStart(host, cur.agent);
      else if (op === "copy" && cur) copyText(cur.url).then((ok) => { cur.copied = ok; toast(ok ? "Link copied" : "Could not copy: select the link and copy it", !ok); $("pr-view")._html = ""; renderPeers(); });
      return;
    }
    if (!ev.target.closest("#pr-revert")) return;
    const c = prs.cfg[prs.route.name];
    if (c && c.state === "ok") { prs.draft = draftFrom(prs.route.name, c.data); touch(); renderPeers(); }
  });
}

// refreshDirty updates the Save buttons while typing, without redrawing the
// field being typed in.
function refreshDirty() {
  const dirty = draftDirty();
  const save = $("pr-save"), rev = $("pr-revert");
  if (save) save.disabled = !dirty || prs.saving;
  if (rev) rev.disabled = !dirty;
  const n = $("pr-dirty");
  if (n) n.textContent = dirty ? "Unsaved changes" : "";
}


/* ---------- Prices: what a provisioner can build, and what it costs (read-only) ---------- */
//
// Peers → Prices. The hub asks a provisioner (a peer that holds cloud
// credentials; -provision-host sets the default) for two things and shows them:
// which credentials it has (present or missing, never a value) and its price
// list. Nothing here creates anything. The hub caches both for five minutes;
// Recheck and Refresh ask again now.
const pz = {
  nodes: null, nodesErr: "", loadingNodes: false,
  via: "", region: "", kind: "aws", small: false, sort: "price",
  cat: null, catErr: "", catKey: "", loading: false, loadingKey: "", seq: 0,
  open: "", score: {}, scoring: "",
};
const PZ_CREDS = [
  ["aws", "AWS", "aws login"], ["gcp", "Google Cloud", "gcloud auth login"], ["tailnet", "Tailscale OAuth client", ""],
  ["openshell", "OpenShell gateway", "openshell gateway login"], ["kubevirt", "KubeVirt cluster", ""], ["ssh", "ssh key", ""],
];
const PZ_BANDS = { "<5%": 0, "5-10%": 1, "10-15%": 2, "15-20%": 3, ">20%": 4 };

// The region a node builds a kind of host in: the AWS config's region, or the region of the gcp config's zone.
function pzKindRegion(n, kind) {
  const pf = n && n.preflight;
  if (!pf) return "";
  if (kind === "gcp") return pf.gcp_zone ? pf.gcp_zone.replace(/-[a-z]$/, "") : "";
  return pf.region || "";
}
function pzDefaultNode() { const n = pz.nodes; return n ? (n.default || ((n.nodes || [])[0] || {}).name || "") : ""; }
function pzNode(name) { return ((pz.nodes && pz.nodes.nodes) || []).find((n) => n.name === name); }

async function loadProvNodes(force) {
  if (pz.loadingNodes || ledger.on !== true || !multiHost()) return;
  pz.loadingNodes = true;
  try {
    pz.nodes = force
      ? await api("/api/provision/refresh", { method: "POST", headers: { "X-Comebye-Csrf": (pz.nodes && pz.nodes.csrf) || "" }, body: JSON.stringify({ what: "nodes" }), timeout: 70000 })
      : await api("/api/provision/nodes", { timeout: 70000 });
    pz.nodesErr = "";
    if (!pz.via || !pzNode(pz.via)) pz.via = pzDefaultNode();
    const n = pzNode(pz.via);
    if (n && !pz.region) pz.region = (n.preflight && n.preflight.region) || "";
  } catch (e) {
    pz.nodesErr = e.message || "failed";
    if (e.code === "csrf") { pz.nodes = null; }
  } finally { pz.loadingNodes = false; }
  if (prs.route && prs.route.tab === "prices") loadProvCatalog(false);
  if (pz.nodes && pz.nodes.enabled) { azResume(); if (!ph.data) loadProvHosts(false); }
  renderPeers();
}

function pzCatPath(extra) {
  const q = new URLSearchParams({ kind: pz.kind, via: pz.via, region: pz.region });
  if (pz.small) q.set("small", "1");
  if (extra) for (const [k, v] of Object.entries(extra)) q.set(k, v);
  return "/api/provision/catalog?" + q;
}

async function loadProvCatalog(force) {
  const n = pzNode(pz.via);
  if (!n || !pz.region) return;
  const key = [pz.via, pz.kind, pz.region, pz.small].join("|");
  if (!force && pz.catKey === key && pz.cat && Date.now() - pz.cat._at < 60000) return;
  if (pz.loading && pz.loadingKey === key) return; // already asking this very question
  const seq = ++pz.seq; // an answer to a question the user has moved on from is dropped
  pz.loading = true; pz.loadingKey = key; pz.catErr = "";
  renderPeers();
  let res = null, err = null;
  try {
    res = force
      ? await api("/api/provision/refresh", { method: "POST", headers: { "X-Comebye-Csrf": (pz.nodes && pz.nodes.csrf) || "" }, body: JSON.stringify({ what: "catalog", via: pz.via, kind: pz.kind, region: pz.region, small: pz.small }), timeout: 340000 })
      : await api(pzCatPath(), { timeout: 340000 });
  } catch (e) { err = e; }
  if (seq !== pz.seq) return;
  if (res) { res._at = Date.now(); pz.cat = res; pz.catKey = key; }
  else {
    pz.catErr = err.code === "too_soon" ? "Prices were refreshed a moment ago: wait a few seconds." : err.code === "creds_expired" ? "creds_expired" : (err.message || "failed");
    if (err.code !== "too_soon") { pz.cat = null; pz.catKey = ""; }
  }
  pz.loading = false; pz.loadingKey = "";
  renderPeers();
}

async function pzCheckCapacity(type) {
  pz.scoring = type;
  renderPeers();
  try {
    const d = await api(pzCatPath({ score: type }), { timeout: 340000 });
    pz.score[pz.via + "|" + pz.region + "|" + type] = (d.catalog && d.catalog.placement) || { scores: [] };
  } catch (e) { pz.score[pz.via + "|" + pz.region + "|" + type] = { error: e.message || "failed" }; }
  pz.scoring = "";
  renderPeers();
}

function pzMoney(p) { return p == null ? "–" : "$" + (p >= 0.1 ? p.toFixed(3) : p.toFixed(4)); }
function pzAge(iso) { const t = Date.parse(iso); return Number.isNaN(t) ? "" : fmtAgo(t) + " ago"; }

function pzCredRows(n) {
  const pf = n.preflight;
  if (!pf) return `<p class="pz-note warn">${n.state === "offline" ? esc(n.name) + " is offline." : "No answer yet."} ${esc(n.error || "")}</p>`;
  return `<dl class="pz-creds">${PZ_CREDS.filter(([k]) => k !== "kubevirt" || pf.kubevirt).map(([k, label, fix]) => {
    const c = pf[k] || { state: "error" };
    const word = ({ ok: "present", missing: "missing", expired: "expired", error: "could not check" })[c.state] || c.state;
    const hint = c.state === "expired" && fix ? ` On ${esc(n.name)}, run <code>${esc(fix)}</code>.` : "";
    return `<dt>${esc(label)}</dt><dd class="pz-state ${esc(c.state)}">${esc(word)}</dd><dd class="pz-note">${esc(c.note || "")}${c.note && hint ? "." : ""}${hint}</dd>`;
  }).join("")}</dl>`;
}

function pzSortRows(rows) {
  const by = {
    price: (a, b) => (a.estimate_spot_usd_hour ?? 9e9) - (b.estimate_spot_usd_hour ?? 9e9),
    ondemand: (a, b) => (a.on_demand_usd_hour ?? 9e9) - (b.on_demand_usd_hour ?? 9e9),
    vcpu: (a, b) => b.vcpu - a.vcpu, memory: (a, b) => b.memory_gib - a.memory_gib,
    stops: (a, b) => (PZ_BANDS[a.interruption] ?? 9) - (PZ_BANDS[b.interruption] ?? 9),
  }[pz.sort] || (() => 0);
  return [...rows].sort((a, b) => (b.selectable - a.selectable) || by(a, b) || a.type.localeCompare(b.type));
}

function pzRow(r, n, pick) {
  const open = !pick && pz.open === r.type;
  const spec = `<span class="pz-cell"><span class="pz-lb">vCPU</span>${r.vcpu}</span><span class="pz-cell"><span class="pz-lb">Memory</span>${r.memory_gib} GiB</span><span class="pz-cell"><span class="pz-lb">Arch</span>${esc(r.arch || "?")}</span>`;
  const prices = `<span class="pz-cell"><span class="pz-lb">Spot from</span>${pzMoney(r.spot_min_usd_hour)}</span><span class="pz-cell"><span class="pz-lb">On-demand</span>${pzMoney(r.on_demand_usd_hour)}</span><span class="pz-cell"><span class="pz-lb">Stops</span>${esc(r.interruption || "–")}</span><span class="pz-cell"><span class="pz-lb">≈ per hour</span><b>${pzMoney(r.estimate_spot_usd_hour)}</b></span>`;
  const tags = r.gpus ? `<span class="pz-tag">${r.gpus} GPU</span>` : "";
  const zones = Object.entries(r.spot_usd_hour || {}).sort().map(([z, p]) => `<span class="pz-zone">${esc(z)} ${pzMoney(p)}</span>`).join("");
  const sc = pz.score[pz.via + "|" + pz.region + "|" + r.type];
  const scoreHtml = sc ? (sc.error ? `<span class="pz-note warn">${esc(sc.error)}</span>`
    : (sc.scores || []).length ? `<div class="pz-zones" aria-label="Capacity score by zone, 1 to 10">${sc.scores.map((z) => `<span class="pz-zone">${esc(z.zone_id)} · ${z.score}/10</span>`).join("")}</div>` : '<span class="pz-note">No capacity score came back.</span>') : "";
  const detail = open ? `<div class="pz-detail">
      ${pz.kind === "gcp" ? `<div class="pz-note">Prices for Google Cloud are estimates from a built-in list, not live: a Spot VM is priced at about 40% of on-demand and Google changes it without notice. Google reports no capacity score.</div>` : `<div><div class="pz-note">Spot price per zone (the market price, up to on-demand; not a bid)</div><div class="pz-zones">${zones || '<span class="pz-note">No per-zone prices: they need an AWS login on ${esc(n.name)}. The region-wide spot price is in the row.</span>'}</div></div>
      <div><button type="button" class="btn" data-pz-score="${escAttr(r.type)}"${pz.scoring ? " disabled" : ""}>${pz.scoring === r.type ? "Checking…" : "Check capacity"}</button>
      <span class="pz-note"> AWS's score for getting one ${esc(r.type)}, by zone id (10 is best).</span></div>${scoreHtml}`}
      ${r.note ? `<div class="pz-note warn">${esc(r.note)}</div>` : ""}</div>` : "";
  if (pick) return `<div class="pz-item${r.selectable ? "" : " off"}"><button type="button" class="pz-row" data-az-pick="${escAttr(r.type)}" aria-pressed="${az.type === r.type}"${r.selectable ? "" : " disabled"}>
    <span class="pz-type">${esc(r.type)}${tags}</span>${spec}${prices}</button></div>`;
  return `<div class="pz-item${r.selectable ? "" : " off"}"><button type="button" class="pz-row" data-pz-type="${escAttr(r.type)}" aria-expanded="${open}">
    <span class="pz-type">${esc(r.type)}${tags}</span>${spec}${prices}</button>${detail}</div>`;
}

function renderPrices() {
  const el = $("pr-prices");
  let html;
  if (az.open && pz.nodes && pz.nodes.enabled) {
    html = azBody();
    if (az.step === 2 && !pz.cat && !pz.loading && !pz.catErr && pz.via && pz.region) setTimeout(() => loadProvCatalog(false), 0);
  } else if (ledger.on !== true || !multiHost()) html = '<div class="pr-empty">Prices are browsed on a hub with peers.</div>';
  else if (!pz.nodes) html = `<div class="pr-empty">${pz.nodesErr ? "Could not load: " + esc(pz.nodesErr) : "Loading…"}</div>`;
  else if (!pz.nodes.enabled) html = `<div class="pz-wrap"><div class="pz-card"><h3>No provisioner</h3><p class="pz-note">No peer here can browse prices yet. Start a peer that holds the cloud login with <code>-provision-config</code> (the worker tool's config), and set <code>COMEBYE_PROVISION_HOST</code> on the hub to make it the default.</p></div></div>`;
  else {
    html = pzBody();
    // Opening the tab, or choosing another node or region, fetches the list.
    if (!pz.cat && !pz.loading && !pz.catErr && pz.via && pz.region) setTimeout(() => loadProvCatalog(false), 0);
  }
  if (el._html !== html) {
    const keep = document.activeElement && el.contains(document.activeElement) ? document.activeElement.id || document.activeElement.dataset.pzType || document.activeElement.dataset.pzScore : "";
    const top = el.scrollTop;
    el._html = html; el.innerHTML = html; el.scrollTop = top;
    if (keep) {
      const f = el.querySelector("#" + CSS.escape(keep)) || el.querySelector(`[data-pz-type="${CSS.escape(keep)}"]`) || el.querySelector(`[data-pz-score="${CSS.escape(keep)}"]`);
      if (f) f.focus({ preventScroll: true });
    }
  }
}

function pzBody() {
  const nodes = pz.nodes.nodes || [], n = pzNode(pz.via) || nodes[0];
  const pf = n && n.preflight;
  const regions = (pf && pf.regions && pf.regions.length ? pf.regions : [pz.region]).filter(Boolean);
  if (pz.region && !regions.includes(pz.region)) regions.unshift(pz.region);
  const sel = `<div class="pz-card"><h3>Provisioner</h3><div class="pz-bar">
    <div class="pz-field"><label for="pz-via">Node</label><select id="pz-via">${nodes.map((x) => `<option value="${escAttr(x.name)}"${x.name === pz.via ? " selected" : ""}>${esc(x.name)}${x.default ? " (default)" : ""}${x.state === "ok" ? "" : " · " + esc(x.state)}</option>`).join("")}</select></div>
    <div class="pz-field"><label for="pz-region">Region</label><select id="pz-region">${regions.map((r) => `<option value="${escAttr(r)}"${r === pz.region ? " selected" : ""}>${esc(r)}${pf && r === pf.region ? " (this node's own)" : ""}</option>`).join("")}</select></div>
    <button type="button" class="btn" id="pz-recheck"${pz.loadingNodes ? " disabled" : ""}>${pz.loadingNodes ? "Checking…" : "Recheck"}</button>
    <button type="button" class="btn primary" id="az-add"${n && n.state === "ok" ? "" : " disabled"}>Add host…</button></div>
    ${n ? pzCredRows(n) : ""}
    <p class="pz-note">Credentials stay on ${esc(n ? n.name : "the node")}: this page shows only whether each is present. Browsing prices creates nothing.${pf && pf.dry_run ? " This provisioner is in dry-run mode: Add host plans and walks through the stages but creates nothing." : ""}</p></div>`;
  let list = "";
  if (!n) list = "";
  else if (pz.loading && !pz.cat) list = '<div class="pz-card"><p class="pz-note">Fetching prices from ' + esc(n.name) + '… the first fetch for a region takes about ten seconds.</p></div>';
  else if (pz.catErr === "creds_expired" && !pz.cat) list = `<div class="pz-card pz-warn"><h3>Prices unavailable</h3><p class="pz-note warn">${esc(n.name)}'s AWS login expired. On ${esc(n.name)}, run <code>aws login</code>, then Recheck.</p></div>`;
  else if (pz.catErr && !pz.cat) list = `<div class="pz-card pz-warn"><h3>Prices unavailable</h3><p class="pz-note warn">${esc(pz.catErr)}</p><div class="pz-bar"><button type="button" class="btn" id="pz-refresh">Try again</button></div></div>`;
  else if (pz.cat) list = pzCatalog(n);
  return `<div class="pz-wrap">${phCard()}${sel}${list}</div>`;
}

function pzCatalog(n, pick) {
  const d = pz.cat, c = d.catalog;
  const stale = d.stale || c.degraded;
  const expired = (d.stale || "").startsWith("creds_expired") || c.degraded === "creds_expired";
  const banner = stale ? `<div class="pz-card pz-warn"><p class="pz-note warn">Prices from ${esc(pzAge(d.fetched_at) || "earlier")}: ${expired ? esc(n.name) + "'s AWS login expired. On " + esc(n.name) + ", run <code>aws login</code>, then Refresh." : "the last refresh failed (" + esc((d.stale || c.degraded).replace(/^creds_expired: ?/, "")) + ")."}</p></div>` : "";
  const rows = pzSortRows(c.rows || []);
  const src = c.sources || {};
  const SRC = { types: "Specs", on_demand: "On-demand", spot_price: "Spot (region)", spot_zones: "Spot per zone", spot_advisor: "Stops", placement: "Capacity" };
  const srcLine = ["types", "on_demand", "spot_price", "spot_zones", "spot_advisor"].filter((k) => src[k]).map((k) => {
    const x = src[k], gone = x.state === "missing";
    const why = gone && x.error === "creds_expired" ? ` (needs an AWS login on ${n.name})` : gone && k === "spot_zones" ? ` (needs an AWS login on ${n.name})` : "";
    return `<li><b>${SRC[k]}</b>: ${esc(x.source || "")}${gone ? " · unavailable" + esc(why) : " · " + esc(pzAge(x.fetched_at))}</li>`;
  }).join("");
  const est = c.estimate || {};
  return `${banner}<div class="pz-card"><h3>${pick ? "Choose a size" : "Instance types"} · ${esc(c.region)}</h3>
    <div class="pz-bar">
      <div class="pz-field"><label for="pz-sort">Sort by</label><select id="pz-sort">${[["price", "Cheapest (spot, estimated)"], ["ondemand", "Lowest on-demand"], ["vcpu", "Most vCPU"], ["memory", "Most memory"], ["stops", "Fewest interruptions"]].map(([v, l]) => `<option value="${v}"${pz.sort === v ? " selected" : ""}>${l}</option>`).join("")}</select></div>
      <label class="pz-check"><input type="checkbox" id="pz-small"${pz.small ? " checked" : ""}>Show small sizes${d.hidden_small ? ` (${d.hidden_small} hidden)` : ""}</label>
      <button type="button" class="btn" id="pz-refresh"${pz.loading ? " disabled" : ""}>${pz.loading ? "Refreshing…" : "Refresh prices"}</button></div>
    <p class="pz-note">Hidden by default: under ${d.min_vcpu} vCPU or ${d.min_memory_gib} GiB, too small for an agent host. ≈ per hour is the lowest spot price plus ${pzMoney(est.ipv4_usd_hour)} for the public IPv4 and ${est.disk_gb || 0} GB of disk. ${pz.kind === "gcp" ? "Google Cloud prices are estimates from a built-in list, not live: a Spot VM is priced at about 40% of on-demand, and Google changes that without notice. Sessions do not survive a stop." : "Spot is the market price and sessions do not survive a stop. Arm (Graviton) types boot the arm64 Debian image."}</p>
    <div class="pz-head" aria-hidden="true"><span>Type</span><span>vCPU</span><span>Memory</span><span>Arch</span><span>Spot from</span><span>On-demand</span><span>Stops</span><span>≈ per hour</span></div>
    <div class="pz-list">${rows.map((r) => pzRow(r, n, pick)).join("") || '<p class="pz-note">No sizes match.</p>'}</div>
    <details class="pz-src"><summary>Where each column comes from (through ${esc(n.name)}, ${esc(pzAge(d.fetched_at))})</summary><ul class="pz-note">${srcLine}</ul>
    <p class="pz-note">Specs, on-demand prices and the interruption band need no AWS login: they are AWS's public data. The region-wide spot price is from the feed behind AWS's spot-pricing page, which AWS does not document. Per-zone spot prices and capacity scores come from ${esc(n.name)}'s AWS login when it has one.</p></details></div>`;
}

function initPrices() {
  const el = $("pr-prices");
  el.addEventListener("change", (ev) => {
    const t = ev.target;
    if (t.id === "pz-via") { pz.via = t.value; const n = pzNode(pz.via); pz.region = (n && n.preflight && n.preflight.region) || pz.region; pz.cat = null; pz.catKey = ""; pz.open = ""; loadProvCatalog(false); }
    else if (t.id === "pz-region") { pz.region = t.value; pz.cat = null; pz.catKey = ""; pz.open = ""; loadProvCatalog(false); }
    else if (t.id === "pz-sort") pz.sort = t.value;
    else if (t.id === "pz-small") { pz.small = t.checked; loadProvCatalog(false); }
    else return;
    renderPeers();
  });
  el.addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (!b || b.disabled) return;
    if (b.id === "pz-recheck") loadProvNodes(true);
    else if (b.id === "pz-refresh") loadProvCatalog(true);
    else if (b.dataset.pzType) { pz.open = pz.open === b.dataset.pzType ? "" : b.dataset.pzType; renderPeers(); }
    else if (b.dataset.pzScore) pzCheckCapacity(b.dataset.pzScore);
  });
}


/* ---------- Hosts built from the app: spend, and Destroy… ---------- */
//
// GET /api/provision/hosts: every host a provisioner built, with what it has
// cost so far (the provisioner's estimate from the cloud's launch and stop
// times, never the cloud's bill) and a total. Destroy… asks the node that
// built the host to save its sessions, then remove the VM, its tailnet node
// and its peer entry; the name is typed to confirm.
const ph = { data: null, err: "", loading: false, dest: null, timer: null, polling: false };

function phMoney(v) { return v == null ? "–" : "$" + (v >= 100 ? v.toFixed(0) : v >= 0.1 ? v.toFixed(2) : v.toFixed(3)); }
function phHost(name) { return ph.data && (ph.data.hosts || []).find((h) => h.name === name && h.state !== "destroyed") || null; }

// phEnsure loads what the provisioners say about their hosts without the Peers screen being open, so that a
// session on a host that is down can say it is stopped. Asked again after half a minute if it did not come.
function phEnsure() {
  if (ph.data || ph.asking || Date.now() - (ph.askedAt || 0) < 30000) return;
  ph.asking = true; ph.askedAt = Date.now();
  (async () => {
    try {
      if (!pz.nodes) await loadProvNodes(false);
      if (pz.nodes && pz.nodes.enabled) await loadProvHosts(false);
    } catch (e) { /* a plain "unreachable" then */ }
    ph.asking = false;
    wakeRedraw();
  })();
}

async function loadProvHosts(force) {
  if (ph.loading || ledger.on !== true || !multiHost() || !(pz.nodes && pz.nodes.enabled)) return;
  ph.loading = true;
  try {
    ph.data = await api("/api/provision/hosts" + (force ? "?refresh=1" : ""), { timeout: 120000 });
    ph.err = "";
    wk.wakes = ph.data.wakes || {};
    if (Object.values(wk.wakes).some((w) => w.state === "starting")) wakeStartPoll(); // a wake begun elsewhere, or before a reload
  } catch (e) { ph.err = e.message || "failed"; }
  ph.loading = false;
  wakeRedraw();
}

function phSpendText(h) {
  const sp = h.spend || {};
  if (h.adopted) return "spend n/a (made with the worker tool)";
  if (h.kind === "openshell" || h.kind === "kubevirt") return h.state === "destroyed" ? "not priced" : h.kind === "kubevirt" ? "not priced (cluster)" : "not priced (sandbox)";
  if (h.state === "destroyed") return `final bill ≈ ${phMoney(sp.usd)}`;
  return `≈ ${phMoney(sp.usd)} so far`;
}

function phRow(h) {
  const sp = h.spend || {};
  const live = h.state !== "destroyed" && h.state !== "gone";
  const burn = live && sp.usd_hour ? ` · ≈ ${phMoney(sp.usd_hour)}/h now` : "";
  const pct = h.budget_cap_usd > 0 ? Math.round(100 * (sp.usd || 0) / h.budget_cap_usd) : 0;
  const what = [h.kind === "openshell" ? "OpenShell sandbox" : h.kind === "kubevirt" ? "KubeVirt VM" : h.type, h.kind === "openshell" || h.kind === "kubevirt" ? "" : h.mode === "spot" ? "spot" : "on-demand", h.via, h.budget_cap_usd > 0 && h.state !== "destroyed" ? `cap ${phMoney(h.budget_cap_usd)} · ${pct}% used` : ""].filter(Boolean).join(" · ");
  const note = h.state === "destroyed"
    ? `Destroyed ${pzAge(h.destroyed)}.${h.data_disk_kept ? (h.kind === "kubevirt" ? " Its data disk was kept on the cluster's storage." : ` Its ${h.data_disk_gb} GB data disk was kept and goes on costing about ${phMoney(h.data_disk_gb * (h.disk_usd_gb_month || 0.08))} a month (not in this bill).`) : ""}`
    : h.state === "stopped" ? `${h.cap_stopped ? "Stopped at its budget cap " + pzAge(h.cap_stopped) : "Stopped" + (h.stopped_at ? " " + pzAge(h.stopped_at) : "")}; only its disks cost now.${h.destroy_due ? " Destroyed automatically " + (Date.parse(h.destroy_due) > Date.now() ? "in " + fmtAgo(Date.now() - (Date.parse(h.destroy_due) - Date.now())) : "now") + "." : ""}`
    : "";
  const fail = h.destroy_failed ? `<p class="pz-note warn" role="alert">The last destroy did not finish: ${esc(h.destroy_failed)}</p>` : "";
  const dest = ph.dest && ph.dest.name === h.name ? phDestroyPanel(h) : "";
  const btn = live && h.state !== "destroying" && !dest && !h.adopted ? `<button type="button" class="btn danger" data-ph-destroy="${escAttr(h.name)}">Destroy…</button>` : "";
  return `<li class="ph-row" data-ph="${escAttr(h.name)}"><div class="ph-line"><span class="ph-name">${esc(h.name)}</span><span class="ph-chip ${esc(h.state)}">${esc(h.state)}</span>
    <span class="ph-spend">${esc(phSpendText(h))}</span></div>
    <div class="ph-meta">${esc(what)}${esc(burn)}${h.price_from_cloud || h.kind === "openshell" || h.kind === "kubevirt" ? "" : " · price estimated"}</div>
    ${note ? `<div class="ph-meta">${esc(note)}</div>` : ""}${fail}${btn}${dest}</li>`;
}

function phDestroyPanel(h) {
  const d = ph.dest;
  if (d.job) return phDestroyProgress(h);
  const ok = d.confirm === h.name && !d.busy;
  const stopped = h.state === "stopped";
  return `<div class="ph-dest" role="group" aria-label="Destroy ${escAttr(h.name)}">
    <p class="pz-note">This saves ${esc(h.name)}'s sessions to the archive, then removes the ${h.kind === "openshell" ? "sandbox" : "VM"}, its tailnet node and its entry on the hub. If the sessions cannot be saved, nothing is destroyed. A destroyed host cannot be brought back.</p>
    ${h.kind === "openshell" ? "" : h.kind === "kubevirt" ? `<label class="pz-check"><input type="checkbox" id="ph-del"${d.del ? " checked" : ""}>Also delete the data disk (kept unless you tick this; it stays on the cluster's storage)</label>` : `<label class="pz-check"><input type="checkbox" id="ph-del"${d.del ? " checked" : ""}>Also delete the ${h.data_disk_gb} GB data disk (kept, it costs about ${phMoney(h.data_disk_gb * (h.disk_usd_gb_month || 0.08))} a month)</label>`}
    <label class="pz-check"><input type="checkbox" id="ph-lose"${d.lose ? " checked" : ""}>Do not save its sessions: they are lost${stopped ? ". A stopped host cannot be read, so this is needed" : ""}</label>
    <div class="pz-field"><label for="ph-confirm">Type <b>${esc(h.name)}</b> to confirm</label><input class="az-in" id="ph-confirm" value="${escAttr(d.confirm)}" autocomplete="off" autocapitalize="none" spellcheck="false"></div>
    ${d.err ? `<p class="pz-note warn" role="alert">${esc(d.err)}</p>` : ""}
    <div class="ph-acts"><button type="button" class="btn" id="ph-cancel">Cancel</button><button type="button" class="btn danger" id="ph-go"${ok ? "" : " disabled"}>${d.busy ? "Starting…" : "Destroy " + esc(h.name)}</button></div></div>`;
}

function phDestroyProgress(h) {
  const j = ph.dest.job;
  const running = j.state === "running";
  const log = (ph.dest.items || []).slice(-60).map((i) => esc(i.msg || "")).filter(Boolean).join("\n");
  let head;
  if (running) head = `<p class="pz-note" role="status">Destroying ${esc(h.name)}… saving its sessions first.</p>`;
  else if (j.state === "ok") head = `<div class="az-dry" role="status"><b>${esc(h.name)} is destroyed.</b> Its bill is final.</div>`;
  else head = `<div class="pz-card pz-warn" role="alert"><b>Not destroyed.</b> ${esc(j.msg || "The destroy did not finish.")}<br><span class="pz-note">Nothing more is billed than before; ${esc(h.name)} is still on the list. Fix the cause and try again.</span></div>`;
  return `<div class="ph-dest">${head}<details class="pz-src"${running ? " open" : ""}><summary>The provisioner's log</summary><pre class="az-plan ph-log">${log || "(nothing yet)"}</pre></details>
    ${running ? "" : '<div class="ph-acts"><button type="button" class="btn" id="ph-close">Close</button></div>'}</div>`;
}

function phCard() {
  if (!pz.nodes || !pz.nodes.enabled) return "";
  const d = ph.data;
  if (!d) return ph.err ? `<div class="pz-card pz-warn"><h3>Hosts built here</h3><p class="pz-note warn">Could not load: ${esc(ph.err)}</p></div>` : "";
  const hosts = d.hosts || [];
  if (!hosts.length && !ph.err) return "";
  const t = d.total || {};
  const old = (d.nodes || []).filter((n) => n.error);
  const age = Math.max(0, ...(d.nodes || []).map((n) => n.age_s || 0));
  const stale = (d.nodes || []).filter((n) => n.stale).map((n) => `${esc(n.name)}: ${esc(n.stale)}`);
  return `<div class="pz-card" id="ph-card"><h3>Hosts built here</h3>
    <div class="ph-total" role="status"><b>≈ ${phMoney(t.usd)}</b> spent so far across ${hosts.length} host${hosts.length === 1 ? "" : "s"}${t.usd_hour ? ` · running now ≈ ${phMoney(t.usd_hour)}/h` : ""}</div>
    ${old.map((n) => `<p class="pz-note warn">${esc(n.name)}: ${esc(n.error)}. These figures are ${age >= 90 ? Math.round(age / 60) + " minutes" : "a moment"} old.</p>`).join("")}
    ${stale.length ? `<p class="pz-note warn">Not refreshed from the cloud: ${stale.join("; ")}</p>` : ""}
    <ul class="ph-list">${hosts.map(phRow).join("")}</ul>
    <p class="pz-note">Spend is about right, not a bill: the cloud's launch and stop times, times the price recorded when the host launched, plus the public IPv4 while it runs and the disks. Updated every few minutes${age ? " (this view is " + (age >= 90 ? Math.round(age / 60) + " min" : age + " s") + " old)" : ""}.</p>
    <div class="pz-bar"><button type="button" class="btn" id="ph-refresh"${ph.loading ? " disabled" : ""}>${ph.loading ? "Refreshing…" : "Refresh spend"}</button></div></div>`;
}

async function phSubmit() {
  const d = ph.dest;
  if (!d || d.busy) return;
  d.busy = true; d.err = ""; renderPeers();
  try {
    const j = await api(`/api/provision/hosts/${encodeURIComponent(d.name)}/destroy`, { method: "POST", headers: azCsrf(), body: JSON.stringify({ via: (phHost(d.name) || {}).via || "", confirm: d.confirm, delete_data_disk: !!d.del, lose_sessions: !!d.lose }), timeout: 60000 });
    d.job = j; d.items = j.items || []; d.cursor = j.next || 0;
    phStartPoll();
  } catch (e) { d.err = e.message || "failed"; }
  d.busy = false;
  renderPeers();
}

function phStartPoll() { if (!ph.timer) ph.timer = setInterval(phPoll, 1500); }
async function phPoll() {
  const d = ph.dest;
  if (!d || !d.job || ph.polling) { if (!d || !d.job) { clearInterval(ph.timer); ph.timer = null; } return; }
  ph.polling = true;
  try {
    const j = await api(`/api/provision/jobs/${encodeURIComponent(d.job.id)}?after=${d.cursor}`, { timeout: 20000 });
    d.items = d.items.concat(j.items || []); d.cursor = j.next; d.job = { ...d.job, state: j.state, msg: j.msg };
    if (j.state !== "running") {
      clearInterval(ph.timer); ph.timer = null;
      ph.polling = false;
      await loadProvHosts(true);
    }
  } catch (e) { /* the next tick tries again */ }
  ph.polling = false;
  renderPeers();
}

function initProvHosts() {
  const el = $("pr-prices");
  el.addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (!b || b.disabled) return;
    if (b.dataset.phDestroy) { ph.dest = { name: b.dataset.phDestroy, confirm: "", del: false, lose: (phHost(b.dataset.phDestroy) || {}).state === "stopped", busy: false, err: "", job: null, items: [], cursor: 0 }; renderPeers(); const c = $("ph-confirm"); if (c) c.focus(); }
    else if (b.id === "ph-cancel" || b.id === "ph-close") { ph.dest = null; renderPeers(); }
    else if (b.id === "ph-go") phSubmit();
    else if (b.id === "ph-refresh") loadProvHosts(true);
  });
  el.addEventListener("input", (ev) => {
    const t = ev.target;
    if (!ph.dest || ph.dest.job) return;
    if (t.id === "ph-confirm") { ph.dest.confirm = t.value; const go = $("ph-go"); if (go) go.disabled = !(ph.dest.confirm === ph.dest.name && !ph.dest.busy); }
    else if (t.id === "ph-del") ph.dest.del = t.checked;
    else if (t.id === "ph-lose") ph.dest.lose = t.checked;
  });
}

/* ---------- Stopped workers: a Start button (issue #66) ---------- */
//
// A worker that powered itself off is a provisioned host the cloud says is
// STOPPED: not a dead network. The hub knows it from the provisioners' look at
// their hosts (ph.data) and keeps a wake book (starting, up, failed) with the
// tool's own step line. Start asks the node that can start it; the page reads
// GET /api/provision/wake every moment or two until the host is up. A host
// stopped at its budget cap asks first: starting it again costs money.
const wk = { wakes: {}, confirm: "", busy: {}, err: {}, timer: null, polling: false };

// wakeOf says what the hub knows of a peer that is not up: a stopped worker, one
// being started, or one that failed to start. null: an ordinary host.
function wakeOf(h) {
  if (!h || h.local || peerStatusOf(h) === "ok") return null;
  const w = wk.wakes[h.name], p = phHost(h.name);
  if (w && w.state === "starting") return { kind: "starting", step: w.step || "starting", w, p };
  if (!p || p.state !== "stopped") return null;
  if (w && w.state === "failed") return { kind: "failed", reason: w.reason || "it did not start", w, p };
  return { kind: "stopped", w, p };
}

function wakeWhat(x) {
  const p = x.p || {};
  const bits = [p.provider || (p.kind && p.kind !== "openshell" ? p.kind : ""), p.type];
  if (x.kind === "stopped") return `${p.cap_stopped ? "stopped at its budget cap " + pzAge(p.cap_stopped) : "idle off" + (p.stopped_at ? " " + pzAge(p.stopped_at) : "")}${bits.filter(Boolean).length ? " · " + bits.filter(Boolean).join(" · ") : ""}`;
  if (x.kind === "starting") return x.step;
  return `failed to start: ${x.reason}`;
}

function wakeChip(x) { return x.kind === "stopped" ? (x.p && x.p.cap_stopped ? "stopped: cap" : "stopped") : x.kind === "starting" ? "starting…" : "failed to start"; }

// The button (or the confirmation) for one host.
function wakeAction(h, x) {
  const n = escAttr(h.name);
  if (x.kind === "starting") return `<div class="wk-prog" role="status"><span class="wk-spin" aria-hidden="true"></span><span>${esc(x.step)}</span></div>`;
  if (wk.confirm === h.name) {
    const p = x.p || {}, cap = p.cap_stopped ? `stopped at its $${(p.budget_cap_usd || 0).toFixed(2)} budget cap, with about ${phMoney((p.spend || {}).usd)} spent` : "due to be destroyed";
    return `<div class="wk-confirm" role="alertdialog" aria-label="Start ${n}"><p class="pz-note warn">${esc(h.name)} was ${esc(cap)}. Starting it again costs more${p.cap_stopped ? ", and the cap will stop it again" : ""}. Start it anyway?</p>
      <div class="wk-acts"><button type="button" class="btn" data-wk-no="1">Cancel</button><button type="button" class="btn primary" data-wk-yes="${n}">Start anyway</button></div></div>`;
  }
  const err = wk.err[h.name] ? `<p class="pz-note warn" role="alert">${esc(wk.err[h.name])}</p>` : "";
  return `${err}<button type="button" class="btn wk-start" data-wk-start="${n}"${wk.busy[h.name] ? " disabled" : ""}>${wk.busy[h.name] ? "Starting…" : x.kind === "failed" ? "Start again" : "Start"}</button>`;
}

function wakeStartPoll() { if (!wk.timer) wk.timer = setInterval(wakePoll, 1500); }
async function wakePoll() {
  if (wk.polling) return;
  wk.polling = true;
  try {
    const d = await api("/api/provision/wake", { timeout: 20000 });
    wk.wakes = d.wakes || {};
    if (!Object.values(wk.wakes).some((w) => w.state === "starting")) {
      clearInterval(wk.timer); wk.timer = null;
      wk.polling = false;
      await loadProvHosts(true);
    }
  } catch (e) { /* the next tick tries again */ }
  wk.polling = false;
  wakeRedraw();
}

// launchHostSync: the picked host of the launch sheet came up (its folders load) or is still stopped (the note follows the wake).
function launchHostSync() {
  if (!launch.open || !launch.host) return;
  if (!launch.opts && !launchHostStopped(launch.host) && (hostStatus(launch.host) || {}).status === "ok" && $("ln-list").querySelector("[data-wk-launch]")) setLaunchHost(launch.host);
  else if (launchHostStopped(launch.host)) renderLaunchStopped(launch.host);
}

function wakeRedraw() {
  if (launch.open) {
    // The launch picker: tags follow the wake, and a host that came up loads its folders.
    renderLaunchHosts();
    launchHostSync();
  }
  if (state.selected && !agentById(state.selected) && (ph.data || ph.asking)) renderDetail(); // a session of a host that is down: stopped, or not
  renderWakeBanner();
  if (typeof renderList === "function") renderList();
  if (prs.route) renderPeers();
}

async function wakeStart(name, confirmed) {
  const p = phHost(name);
  if (!confirmed && p && (p.cap_stopped || (p.destroy_due && Date.parse(p.destroy_due) <= Date.now()))) { wk.confirm = name; wakeRedraw(); return; }
  wk.confirm = ""; wk.busy[name] = true; wk.err[name] = ""; wakeRedraw();
  try {
    const d = await api(`/api/provision/hosts/${encodeURIComponent(name)}/start`, { method: "POST", headers: azCsrf(), body: JSON.stringify({ via: (p || {}).via || "", confirm_cap: !!confirmed }), timeout: 60000 });
    if (d.wake) wk.wakes[name] = d.wake;
    wakeStartPoll();
  } catch (e) {
    if (e.code === "needs_confirm") wk.confirm = name; else wk.err[name] = e.message || "failed";
  }
  wk.busy[name] = false;
  wakeRedraw();
}

// wakeWaitFor follows the wake of host until it is up (the call that began it
// answered 202 host_waking). It throws with the reason when it fails.
async function wakeWaitFor(host, first) {
  if (first) wk.wakes[host] = first;
  wakeStartPoll();
  wakeRedraw();
  const until = Date.now() + 14 * 60 * 1000;
  for (;;) {
    await new Promise((r) => setTimeout(r, 1500));
    let d;
    try { d = await api("/api/provision/wake", { timeout: 20000 }); } catch (e) { if (Date.now() > until) throw e; continue; }
    wk.wakes = d.wakes || wk.wakes;
    const w = wk.wakes[host];
    if (w && w.state === "up") { wakeRedraw(); return; }
    if (!w || w.state === "failed") { wakeRedraw(); throw new Error(`could not start ${host}: ${(w && w.reason) || "it did not start"}`); }
    if (Date.now() > until) throw new Error(`${host} did not start in time`);
  }
}

// apiWake is api() for an action on a worker that may be stopped: the hub
// answers 202 host_waking and starts it, this shows "starting kv1…" while it
// boots, then repeats the action. onWait says it is waiting (the caller shows it).
async function apiWake(host, path, opts, onWait) {
  let d = await api(path, opts);
  if (d && d.code === "host_waking") {
    if (onWait) onWait(host);
    await wakeWaitFor(host, d.wake);
    d = await api(path, opts);
  }
  return d;
}

// The banner above the composer of a session on a stopped worker. Opening the
// session does not start the host: this is how you do, or you just send.
function renderWakeBanner() {
  const el = $("wk-banner");
  if (!el) return;
  // The session may be gone from the list because its host is down: the id still names the host.
  const a = state.selected ? agentById(state.selected) : null;
  if (a) state.selName = a.name || "";
  const hn = state.selected ? hostOf(state.selected) : null;
  const h = hn ? hostStatus(hn) : null;
  if (h && !h.local && peerStatusOf(h) !== "ok") phEnsure();
  const x = h ? wakeOf(h) : null;
  if (!x) { el.hidden = true; el.textContent = ""; el.className = "wk-banner"; return; }
  // Keep a button that is being pressed: only redraw when the state changed.
  const key = [h.name, x.kind, x.step || x.reason || "", wk.confirm === h.name, !!wk.busy[h.name], wk.err[h.name] || ""].join("|");
  if (el.dataset.key === key && !el.hidden) return;
  el.dataset.key = key;
  el.hidden = false;
  el.className = "wk-banner " + x.kind;
  const title = x.kind === "starting" ? `Starting ${h.name}…` : x.kind === "failed" ? `${h.name} did not start` : `${h.name} is stopped. Start it?`;
  const sub = x.kind === "stopped" ? `Sending a message starts it (about 2 minutes) and your text is kept. ${state.selName ? state.selName + " may not survive the power-off: if it does not, you can resume or launch it again." : ""}` : x.kind === "failed" ? x.reason : "";
  el.innerHTML = `<p class="wk-title">${esc(title)}</p>${sub ? `<p class="wk-sub">${esc(sub)}</p>` : ""}${wakeAction(h, x)}`;
}

// A session that did not survive the power-off: said plainly, with a way on.
function sessionGoneToast(e, host) {
  const acts = [];
  if (e.parkId) acts.push({ label: "Resume", fn: () => apiWake(host, `/api/hosts/${encodeURIComponent(host)}/resume`, { method: "POST", body: JSON.stringify({ id: e.parkId }), timeout: 100000 }).then(() => toast("resumed"), (x) => toast(x.message, true)) });
  else acts.push({ label: "Launch again", fn: () => openLaunch() });
  toastActions(e.message, acts, 30000);
}

document.addEventListener("click", (ev) => {
  const b = ev.target.closest && ev.target.closest("[data-wk-start],[data-wk-yes],[data-wk-no]");
  if (!b || b.disabled) return;
  ev.preventDefault(); ev.stopPropagation();
  if (b.dataset.wkStart) wakeStart(b.dataset.wkStart, false);
  else if (b.dataset.wkYes) wakeStart(b.dataset.wkYes, true);
  else { wk.confirm = ""; wakeRedraw(); }
}, true);

/* ---------- Add host: the wizard (Peers → Prices → Add host…) ---------- */
//
// node → size → options → review → progress, with the cost on every step.
// The hub validates and forwards to the provisioner (POST /api/provision/plan,
// POST /api/provision, GET /api/provision/jobs/{id}); credentials never come
// here. On a provisioner in dry-run mode every step runs, the stages are
// listed as planned, and nothing is created.
const az = {
  open: false, step: 1, type: "", spec: null, plan: null, planErr: "", planning: false, ack: false,
  key: "", submitting: false, err: "", job: null, cursor: 0, items: [], timer: null, polling: false, acting: false,
};
const AZ_STEPS = ["Node", "Size", "Options", "Review", "Progress"];
const AZ_STAGES = { sandbox: "Sandbox", settings: "Settings", network: "Network", ssh_key: "ssh key", data_disk: "Data disk", instance: "Instance", ssh: "ssh reach", bootstrap: "Bootstrap", tailnet: "Tailnet", agents: "Agents", hub: "Joins the hub", destroy: "Clean up", create: "Create", config: "Config" };
const AZ_NAME = /^[a-z][a-z0-9-]{0,28}[a-z0-9]$/;
const AZ_JOB_KEY = "comebye.addhost.job";

function azDefaults(type, kind) {
  if (kind === "openshell" || kind === "kubevirt") return { kind, name: "", idle_minutes: 120, destroy_after_hours: 72 };
  return { kind: kind === "gcp" ? "gcp" : "aws", name: "", type: type || "", spot: true, zone: "", boot_disk_gb: 20, data_disk_gb: 50, idle_minutes: 120, max_run: "24h", budget_cap_usd: 10, destroy_after_hours: 72 };
}
// A sandbox and a KubeVirt VM are made from the node's own config: no size, disks or price to pick.
function azSandbox() { return !!(az.spec && (az.spec.kind === "openshell" || az.spec.kind === "kubevirt")); }
function azKube() { return !!(az.spec && az.spec.kind === "kubevirt"); }
// What a node can build now: its kinds, from the preflight (a kind whose login is missing is disabled, with why).
function azKinds(n) { return (n && n.kinds) || [{ kind: "aws", label: "AWS spot or on-demand VM", ok: true }]; }
function azGCE() { return !!(az.spec && az.spec.kind === "gcp"); }
function azCsrf() { return { "X-Comebye-Csrf": (pz.nodes && pz.nodes.csrf) || "" }; }
function azRow() { const c = pz.cat && pz.cat.catalog; return c ? (c.rows || []).find((r) => r.type === az.spec.type) : null; }
function azNode() { return pzNode(pz.via); }
function azDry() { const n = azNode(); return !!(n && n.preflight && n.preflight.dry_run); }

function azStart(type) {
  az.open = true; az.step = type ? 3 : 1; az.type = type || "";
  az.spec = azDefaults(type, type ? "aws" : (az.spec && az.spec.kind) || "aws"); az.plan = null; az.planErr = ""; az.ack = false; az.key = ""; az.err = ""; az.job = null; az.cursor = 0; az.items = [];
  const n = azNode();
  pz.kind = az.spec.kind === "gcp" ? "gcp" : "aws";
  const own = pzKindRegion(n, pz.kind);
  if (own) pz.region = own;
  renderPeers();
}
function azClose() {
  az.open = false; azStopPoll();
  if (pz.kind !== "aws") { pz.kind = "aws"; pz.cat = null; pz.catKey = ""; const own = pzKindRegion(azNode(), "aws"); if (own) pz.region = own; }
  try { localStorage.removeItem(AZ_JOB_KEY); } catch (e) { /* storage may be unavailable */ }
  renderPeers();
}

// The estimate of the cost footer, from the cached price list (the review step
// shows the provisioner's own figure).
function azCost() {
  const r = azRow(), c = pz.cat && pz.cat.catalog, sp = az.spec;
  if (!r || !c) return null;
  const e = c.estimate || {};
  const extra = (e.ipv4_usd_hour || 0) + (sp.boot_disk_gb + sp.data_disk_gb) * (e.disk_usd_gb_month || 0) / 730;
  let base = sp.spot ? ((sp.zone && r.spot_usd_hour && r.spot_usd_hour[sp.zone] != null) ? r.spot_usd_hour[sp.zone] : r.spot_min_usd_hour) : r.on_demand_usd_hour;
  if (base == null) return null;
  const hour = base + extra;
  return { hour, day: hour * 24, hours: sp.budget_cap_usd / hour };
}
function azFoot() {
  if (azSandbox()) return `<b>no per-hour price</b><small>${azKube() ? "a KubeVirt VM runs on the cluster's own capacity" : "an OpenShell sandbox runs on the gateway's own capacity"}</small>`;
  const c = azCost(), sp = az.spec;
  const cost = c
    ? `<b>≈ ${pzMoney(c.hour)}/h · $${c.day.toFixed(2)}/day</b><small>${sp.spot ? (azGCE() ? "Spot VM, estimated" : "spot, market price") : "on-demand"} · the $${sp.budget_cap_usd} cap lasts about ${c.hours >= 100 ? Math.round(c.hours) : c.hours.toFixed(1)} h</small>`
    : `<b>≈ – /h</b><small>${sp.type ? "no price for " + esc(sp.type) : "choose a size to see the cost"}</small>`;
  return cost;
}
function azUpdateFoot() { const el = $("az-cost"); if (el) el.innerHTML = azFoot(); }

function azSteps() {
  const steps = azSandbox() ? AZ_STEPS.map((l) => (l === "Size" ? (azKube() ? "KubeVirt" : "Sandbox") : l)) : AZ_STEPS;
  return `<ol class="az-steps" aria-label="Steps">${steps.map((l, i) => `<li class="${i + 1 === az.step ? "on" : i + 1 < az.step ? "done" : ""}"${i + 1 === az.step ? ' aria-current="step"' : ""}>${i + 1}. ${l}</li>`).join("")}</ol>`;
}

// A sandbox has no size to pick: the step shows what the node will make it from.
function azSandboxCard(n) {
  if (azKube()) {
    const kv = (n && n.preflight && n.preflight.kubevirt) || {};
    return `<div class="pz-card"><h3>KubeVirt VM on ${esc(n ? n.name : "the node")}</h3>
    <dl class="az-kv"><dt>Cluster</dt><dd>${esc(kv.note || kv.state || "not checked")}</dd></dl>
    <p class="pz-note">Its size, disks, image and namespace are ${esc(n ? n.name : "the node")}'s own settings (its KubeVirt worker config), not chosen here. A KubeVirt VM has no price: the namespace's quota and the provisioner's host-count cap hold it. Its sessions are saved before it is destroyed, and its data disk is kept unless you ask to delete it.</p></div>`;
  }
  const os = (n && n.preflight && n.preflight.openshell) || {};
  const row = (k, v) => (v ? `<dt>${k}</dt><dd>${esc(v)}</dd>` : "");
  return `<div class="pz-card"><h3>OpenShell sandbox on ${esc(n ? n.name : "the node")}</h3>
    <dl class="az-kv">${row("Gateway", os.gateway)}${row("Workspace", os.workspace)}${row("Image", os.image)}${row("Policy", os.policy)}${row("CPU", os.cpu)}${row("Memory", os.memory)}${os.sandboxes != null ? row("Already there", os.sandboxes + " sandbox" + (os.sandboxes === 1 ? "" : "es") + " on the gateway") : ""}</dl>
    <p class="pz-note">These are ${esc(n ? n.name : "the node")}'s own settings; the gateway and the image are chosen in its worker config, not here. A sandbox has no machine type, disks or price, and the gateway reports its health and version but no capacity or quota (the count above is all it offers): if it is full, the create fails with the gateway's own error. Its sessions are saved before it is destroyed, like a VM's.</p></div>`;
}

function azBody() {
  const n = azNode(), sp = az.spec;
  let main = "", next = "", back = "", can = true;
  if (az.step === 1) {
    const nodes = (pz.nodes && pz.nodes.nodes) || [];
    const dry = azDry();
    main = `<div class="pz-card"><h3>Which node builds it</h3><div class="pz-bar"><div class="pz-field"><label for="az-via">Node</label><select id="az-via">${nodes.map((x) => `<option value="${escAttr(x.name)}"${x.name === pz.via ? " selected" : ""}>${esc(x.name)}${x.default ? " (default)" : ""}${x.state === "ok" ? "" : " · " + esc(x.state)}</option>`).join("")}</select></div>
      <button type="button" class="btn" id="pz-recheck"${pz.loadingNodes ? " disabled" : ""}>${pz.loadingNodes ? "Checking…" : "Recheck"}</button></div>
      <fieldset class="az-kinds"><legend class="pz-note">What to build</legend>${azKinds(n).map((k) => `<label class="pz-check"><input type="radio" name="az-kind" value="${escAttr(k.kind)}"${sp.kind === k.kind ? " checked" : ""}${k.ok ? "" : " disabled"}>${esc(k.label)}${k.ok ? "" : ` <span class="pz-note warn">(${esc(k.why || "not available")})</span>`}</label>`).join("")}</fieldset>
      ${n ? pzCredRows(n) : ""}
      <p class="pz-note">${azKube() ? "A KubeVirt VM is made on the node's own cluster, not in a cloud region." : azSandbox() ? "A sandbox is made on the node's OpenShell gateway, not in a cloud region." : `Builds in <b>${esc(pzKindRegion(n, azGCE() ? "gcp" : "aws") || pz.region || "?")}</b>, this node's own ${azGCE() ? "zone's region" : "region"}.`} The credentials stay on ${esc(n ? n.name : "the node")}; this page only learns whether each is present.</p>
      ${dry ? `<div class="az-dry"><b>Dry run.</b> ${esc(n.name)} is set to plan only: every step below works, and the stages show as planned, but nothing is created and no cost is incurred.</div>` : ""}</div>`;
    const kk = azKinds(n).find((k) => k.kind === sp.kind);
    can = !!n && n.state === "ok" && (!kk || kk.ok);
    next = azKube() ? "KubeVirt →" : azSandbox() ? "Sandbox →" : "Size →";
  } else if (az.step === 2 && azSandbox()) {
    back = "← Node"; next = "Options →"; can = true;
    main = azSandboxCard(n);
  } else if (az.step === 2) {
    back = "← Node"; next = "Options →"; can = !!az.type;
    if (pz.loading && !pz.cat) main = `<div class="pz-card"><p class="pz-note">Fetching prices from ${esc(n ? n.name : "the node")}… the first fetch downloads AWS's public price list and can take a minute.</p></div>`;
    else if (pz.catErr && !pz.cat) main = `<div class="pz-card pz-warn"><h3>Prices unavailable</h3><p class="pz-note warn">${esc(pz.catErr === "creds_expired" ? "the price list could not be read" : pz.catErr)}</p><div class="pz-bar"><button type="button" class="btn" id="pz-refresh">Try again</button></div></div>`;
    else if (pz.cat && n) main = pzCatalog(n, true);
    else main = '<div class="pz-card"><p class="pz-note">Loading…</p></div>';
  } else if (az.step === 3 && azSandbox()) {
    back = azKube() ? "← KubeVirt" : "← Sandbox"; next = "Review →";
    const bad = sp.name && !AZ_NAME.test(sp.name);
    can = AZ_NAME.test(sp.name);
    const num = (id, label, v, min, max, hint) => `<div class="pz-field"><label for="${id}">${label}</label><input class="az-in" id="${id}" type="number" inputmode="decimal" min="${min}" max="${max}" value="${escAttr(v)}"><small class="pz-note">${hint}</small></div>`;
    main = `<div class="pz-card"><h3>${azKube() ? "KubeVirt VM" : "Sandbox"} on ${esc(pz.via)}</h3><div class="az-grid">
      <div class="pz-field az-wide"><label for="az-name">Name</label><input class="az-in${bad ? " bad" : ""}" id="az-name" value="${escAttr(sp.name)}" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="${azKube() ? "e.g. kv-2" : "e.g. sandbox-1"}"><small class="pz-note${bad ? " warn" : ""}">2 to 30 lowercase letters, digits and dashes. It becomes the host's name in Comebye.</small></div>
      ${num("az-idle", "Stop after idle (minutes)", sp.idle_minutes, 5, 1440, azKube() ? "Default 120. Its disks are kept." : "Default 120. Its volume is kept.")}
      ${num("az-destroy", "Destroy after stopped (hours)", sp.destroy_after_hours, 1, 720, "Default 72 (3 days). The provisioner then starts it, saves its sessions and destroys it; if it cannot, nothing is destroyed.")}
      </div></div>`;
  } else if (az.step === 3) {
    back = "← Size"; next = "Review →";
    const r = azRow();
    const zones = r && r.spot_usd_hour ? Object.keys(r.spot_usd_hour).sort() : [];
    const bad = sp.name && !AZ_NAME.test(sp.name);
    can = AZ_NAME.test(sp.name) && sp.budget_cap_usd >= 1;
    const num = (id, label, v, min, max, hint) => `<div class="pz-field"><label for="${id}">${label}</label><input class="az-in" id="${id}" type="number" inputmode="decimal" min="${min}" max="${max}" value="${escAttr(v)}"><small class="pz-note">${hint}</small></div>`;
    main = `<div class="pz-card"><h3>${esc(sp.type)} on ${esc(pz.via)}</h3><div class="az-grid">
      <div class="pz-field az-wide"><label for="az-name">Name</label><input class="az-in${bad ? " bad" : ""}" id="az-name" value="${escAttr(sp.name)}" autocomplete="off" autocapitalize="none" spellcheck="false" placeholder="e.g. build-box"><small class="pz-note${bad ? " warn" : ""}">2 to 30 lowercase letters, digits and dashes. It becomes the host's name in Comebye.</small></div>
      <div class="pz-field"><label for="az-spot">Pricing</label><select id="az-spot"><option value="1"${sp.spot ? " selected" : ""}>${azGCE() ? "Spot VM" : "Spot"} (cheaper, can be reclaimed)</option><option value="0"${sp.spot ? "" : " selected"}>On-demand</option></select><small class="pz-note">${sp.spot ? "A reclaim stops the host (disks kept) and agent sessions end." : "Full price, not reclaimed."}</small></div>
      <div class="pz-field"><label for="az-zone">Zone</label><select id="az-zone"><option value="">The node's default</option>${zones.map((z) => `<option value="${escAttr(z)}"${sp.zone === z ? " selected" : ""}>${esc(z)} · ${pzMoney(r.spot_usd_hour[z])}</option>`).join("")}</select><small class="pz-note">${zones.length ? "Spot price per zone." : azGCE() ? "Leave it on the node's zone." : "Zone prices need an AWS login on the node."}</small></div>
      ${num("az-boot", "Boot disk (GB)", sp.boot_disk_gb, 10, 200, "10 to 200")}
      ${num("az-data", "Data disk (GB)", sp.data_disk_gb, 10, 1000, "Kept when the host is destroyed unless you delete it.")}
      ${num("az-idle", "Stop after idle (minutes)", sp.idle_minutes, 5, 1440, "Default 120. Disks are kept.")}
      ${num("az-budget", "Budget cap ($)", sp.budget_cap_usd, 1, 100, "Required. You get a to-do at 80%; at 100% the provisioner stops the host, disks kept. A cap never destroys.")}
      ${num("az-destroy", "Destroy after stopped (hours)", sp.destroy_after_hours, 1, 720, "Default 72 (3 days). The provisioner then starts it, saves its sessions and destroys it; if it cannot, nothing is destroyed.")}
      </div></div>`;
  } else if (az.step === 4) {
    back = "← Options"; next = azDry() ? "Run dry run" : "Create host";
    main = azReview();
    can = !!az.plan && az.plan.ok && az.plan.ready && az.ack && !az.submitting;
  } else {
    main = azProgress();
    next = ""; back = "";
  }
  const foot = az.step === 5 ? "" : `<div class="az-foot"><div class="az-cost" id="az-cost">${azFoot()}</div><div class="az-acts">
    <button type="button" class="btn" id="az-cancel">Cancel</button>${back ? `<button type="button" class="btn" id="az-back">${back}</button>` : ""}
    ${next ? `<button type="button" class="btn primary" id="az-next"${can ? "" : " disabled"}>${next}</button>` : ""}</div></div>`;
  return `<div class="pz-wrap az-wrap"><div class="pz-card az-head"><h3>Add a host${azDry() ? " · dry run" : ""}</h3>${azSteps()}</div>${main}${az.err ? `<div class="pz-card pz-warn"><p class="pz-note warn" role="alert">${esc(az.err)}</p></div>` : ""}${foot}</div>`;
}

function azReview() {
  const sp = az.spec, n = azNode(), p = az.plan;
  const os = (n && n.preflight && n.preflight.openshell) || {};
  const kvp = (n && n.preflight && n.preflight.kubevirt) || {};
  const kv = azKube() ? `<dl class="az-kv"><dt>Node</dt><dd>${esc(pz.via)} · cluster ${esc(kvp.note || kvp.state || "?")}</dd><dt>Name</dt><dd>${esc(sp.name)}</dd><dt>Made from</dt><dd>the node's KubeVirt config (image, size, disks)</dd>
    <dt>Safety</dt><dd>stops after ${sp.idle_minutes} min idle · destroyed ${sp.destroy_after_hours} h after it stops · no budget cap (nothing here is priced; the namespace quota and the host cap hold it)</dd></dl>` : azSandbox() ? `<dl class="az-kv"><dt>Node</dt><dd>${esc(pz.via)} · gateway ${esc(os.gateway || "?")}</dd><dt>Name</dt><dd>${esc(sp.name)}</dd><dt>Made from</dt><dd>${esc(os.image || "the node's image")}${os.cpu || os.memory ? " · " + esc([os.cpu && os.cpu + " CPU", os.memory].filter(Boolean).join(", ")) : ""}</dd>
    <dt>Safety</dt><dd>stops after ${sp.idle_minutes} min idle · destroyed ${sp.destroy_after_hours} h after it stops · no budget cap (nothing here is priced)</dd></dl>` : `<dl class="az-kv"><dt>Node</dt><dd>${esc(pz.via)} · ${esc(pz.region)}</dd><dt>Name</dt><dd>${esc(sp.name)}</dd><dt>Size</dt><dd>${esc(sp.type)}, ${sp.spot ? "spot" : "on-demand"}${sp.zone ? ", " + esc(sp.zone) : ""}</dd>
    <dt>Disks</dt><dd>${sp.boot_disk_gb} GB boot + ${sp.data_disk_gb} GB data</dd><dt>Safety</dt><dd>stops after ${sp.idle_minutes} min idle · budget cap $${sp.budget_cap_usd} · destroyed ${sp.destroy_after_hours} h after it stops</dd></dl>`;
  let body;
  if (az.planning) body = '<p class="pz-note">Asking ' + esc(pz.via) + ' for the plan…</p>';
  else if (az.planErr) body = `<p class="pz-note warn" role="alert">${esc(az.planErr)}</p><div class="pz-bar"><button type="button" class="btn" id="az-replan">Try again</button></div>`;
  else if (p) {
    const c = p.cost;
    body = `${p.dry_run ? '<div class="az-dry"><b>Dry run.</b> This runs the tool\'s plan and reports every stage as planned. Nothing is created.</div>' : ""}
      ${c && c.mode === "sandbox" ? `<div class="az-big"><b>no per-hour price</b><span class="pz-note"> ${esc(c.basis || "")}</span></div>` : c ? `<div class="az-big"><b>≈ ${pzMoney(c.usd_hour)}/h · $${Number(c.usd_day).toFixed(2)}/day</b><span class="pz-note"> ${esc(c.basis || "")}${c.mode === "spot" && c.on_demand_usd_hour ? ` · on-demand would be ${pzMoney(c.on_demand_usd_hour)}/h` : ""}</span></div>` : '<p class="pz-note warn">The provisioner could not price this size.</p>'}
      ${(p.warnings || []).map((w) => `<p class="pz-note warn">${esc(w)}</p>`).join("")}
      ${p.refused ? `<p class="pz-note warn" role="alert"><b>Refused:</b> ${esc(p.refused)}</p>` : ""}
      ${!p.ready ? `<p class="pz-note warn" role="alert">${esc(pz.via)} cannot build now: ${esc((p.missing || []).join(", "))}. Fix it on ${esc(pz.via)} and Recheck.</p>` : ""}
      <details class="pz-src"><summary>The plan, in the tool's words</summary><pre class="az-plan">${esc(p.plan && p.plan.text || "")}</pre></details>
      <label class="pz-check az-ack"><input type="checkbox" id="az-ack"${az.ack ? " checked" : ""}>${p.dry_run ? "I understand this is a dry run." : azKube() ? "I understand this creates a VM on the cluster." : azSandbox() ? "I understand this creates a sandbox on the gateway." : `I understand this creates cloud resources that cost money${c ? " (up to about $" + Number(c.usd_day).toFixed(2) + " a day)" : ""}.`}</label>`;
  } else body = "";
  return `<div class="pz-card"><h3>Review</h3>${kv}${body}</div>`;
}

async function azPlan() {
  if (az.planning) return;
  az.planning = true; az.planErr = ""; az.plan = null; az.ack = false;
  renderPeers();
  try {
    const sp = azSpecOut();
    const r = await api("/api/provision/plan", { method: "POST", headers: azCsrf(), body: JSON.stringify({ via: pz.via, region: pz.region, spec: sp }), timeout: 120000 });
    az.plan = r; if (r.csrf && pz.nodes) pz.nodes.csrf = r.csrf;
  } catch (e) { az.planErr = e.field ? `${e.message}` : (e.message || "failed"); }
  az.planning = false;
  renderPeers();
}

function azSpecOut() {
  const s = az.spec;
  if (s.kind === "openshell" || s.kind === "kubevirt") return { kind: s.kind, name: s.name, idle_minutes: s.idle_minutes, destroy_after_hours: s.destroy_after_hours };
  const o = { ...s };
  if (!o.zone) delete o.zone;
  return o;
}

async function azSubmit() {
  if (az.submitting) return;
  if (!az.key) az.key = "k" + Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, "0")).join("");
  az.submitting = true; az.err = "";
  renderPeers();
  try {
    const j = await api("/api/provision", { method: "POST", headers: azCsrf(), body: JSON.stringify({ via: pz.via, region: pz.region, spec: azSpecOut(), key: az.key, ack: az.ack }), timeout: 120000 });
    azTakeJob(j, true);
    az.step = 5;
    try { localStorage.setItem(AZ_JOB_KEY, j.id); } catch (e) { /* storage may be unavailable */ }
    azStartPoll();
  } catch (e) { az.err = e.message || "failed"; }
  az.submitting = false;
  renderPeers();
}

function azTakeJob(j, reset) {
  if (reset) { az.items = []; az.cursor = 0; }
  if (j.csrf && pz.nodes) pz.nodes.csrf = j.csrf;
  az.job = j;
  for (const it of j.items || []) { if (it.seq > az.cursor) { az.items.push(it); az.cursor = it.seq; } }
  if (j.spec && (!az.spec || az.spec.name !== j.spec.name)) az.spec = { ...azDefaults(), ...j.spec, zone: j.spec.zone || "" };
}
function azStopPoll() { if (az.timer) { clearInterval(az.timer); az.timer = null; } }
function azStartPoll() {
  azStopPoll();
  az.timer = setInterval(azPoll, 1500);
}
async function azPoll() {
  if (az.polling || !az.job || !az.open) return;
  az.polling = true;
  try {
    const j = await api(`/api/provision/jobs/${encodeURIComponent(az.job.id)}?after=${az.cursor}`, { timeout: 20000 });
    azTakeJob(j, false);
    if (j.state !== "running" && j.state !== "cleaning") { azStopPoll(); if (!j.dry_run) loadProvHosts(true); } // a built host is on the books now
    renderPeers();
  } catch (e) { /* the next tick asks again */ }
  az.polling = false;
}

// Stage states from the journal: the last event of each stage.
function azStageStates() {
  const m = {};
  let attempt = az.job ? az.job.attempt : 1;
  for (const it of az.items) if (it.kind === "event" && it.attempt === attempt) m[it.stage] = it;
  return m;
}
const AZ_FAIL = {
  spot_capacity: "The cloud has no spot capacity for this size right now.",
  creds_expired: "The cloud login on the provisioner expired. Run aws login (or gcloud auth login, or openshell gateway login) on it, then Retry.",
  quota: "The cloud account's quota for this size is used up.",
  bootstrap_failed: "The host was created but setting it up failed.",
  needs_input: "The tool wanted to ask a question, and it cannot here.",
  other: "The build failed.",
};
function azProgress() {
  const j = az.job;
  if (!j) return '<div class="pz-card"><p class="pz-note">Starting…</p></div>';
  const st = azStageStates();
  const stages = (j.stages || []).filter((s) => !(s === "data_disk" && false));
  const rows = stages.map((s) => {
    const e = st[s];
    const cls = !e ? "" : e.state === "ok" ? "ok" : e.state === "failed" ? "fail" : e.state === "planned" ? "plan" : "run";
    const word = !e ? "waiting" : ({ ok: "done", failed: "failed", planned: "planned (dry run)", running: "running" })[e.state];
    return `<li class="az-st ${cls}"><span class="az-ic" aria-hidden="true">${cls === "ok" ? "✓" : cls === "fail" ? "!" : cls === "plan" ? "·" : cls === "run" ? "…" : ""}</span><b>${esc(AZ_STAGES[s] || s)}</b><small>${esc(word)}${e && e.msg ? " · " + esc(e.msg) : ""}</small></li>`;
  }).join("");
  const log = az.items.filter((i) => i.kind === "log").slice(-80).map((i) => esc(i.msg)).join("\n");
  let head, acts = "";
  const running = j.state === "running" || j.state === "cleaning";
  if (running) head = `<p class="pz-note" role="status">${j.state === "cleaning" ? "Cleaning up…" : j.dry_run ? "Planning (dry run)…" : "Building…"}</p>`;
  else if (j.state === "ok") head = j.dry_run
    ? `<div class="az-dry" role="status"><b>Dry run finished.</b> Every stage was planned; nothing was created. The real create is off on ${esc(j.via)}.</div>`
    : `<div class="pz-card az-ok" role="status"><b>${esc(j.name)} is built.</b> It joins the hub as a peer in a moment.</div>`;
  else if (j.state === "cleaned") head = `<div class="az-dry" role="status"><b>Cleaned up.</b> ${j.dry_run ? "Nothing had been created." : "What the failed attempt made was removed."}</div>`;
  else {
    const alt = j.alternatives, zs = (alt && alt.zones) || [];
    head = `<div class="pz-card pz-warn" role="alert"><b>${j.state === "interrupted" ? "Interrupted." : "Failed."}</b> ${esc(AZ_FAIL[j.class] || AZ_FAIL.other)}${j.zone ? " (" + esc(j.zone) + ")" : ""}<br><span class="pz-note">${esc(j.msg || "")}</span></div>`;
    acts = `<div class="pz-bar az-recover">
      ${j.can_retry ? '<button type="button" class="btn primary" data-az-act="retry">Retry</button>' : ""}
      ${j.class === "spot_capacity" && zs.length ? `<div class="pz-field"><label for="az-altzone">Try another zone</label><select id="az-altzone">${zs.map((z) => `<option>${esc(z)}</option>`).join("")}</select></div><button type="button" class="btn" data-az-act="zone">Use it</button>` : ""}
      ${j.class === "spot_capacity" && alt && alt.on_demand ? '<button type="button" class="btn" data-az-act="ondemand">Switch to on-demand</button>' : ""}
      ${j.can_cleanup ? '<button type="button" class="btn danger" data-az-act="cleanup">Clean up</button>' : ""}</div>`;
  }
  const off = j.offline ? `<p class="pz-note warn">${esc(j.offline)}</p>` : "";
  const done = !running;
  return `<div class="pz-card"><h3>${esc(j.name)} · ${esc(j.via)}${j.attempt > 1 ? " · attempt " + j.attempt : ""}</h3>${head}${off}<ol class="az-stages">${rows}</ol>${acts}
    <details class="pz-src"${log && !done ? " open" : ""}><summary>The provisioner's log</summary><pre class="az-plan">${log || "(nothing yet)"}</pre></details>
    ${done ? '<div class="pz-bar"><button type="button" class="btn" id="az-done">Close</button></div>' : ""}</div>`;
}

async function azAct(act) {
  if (az.acting || !az.job) return;
  az.acting = true; az.err = "";
  const id = encodeURIComponent(az.job.id);
  try {
    let j;
    if (act === "retry" || act === "cleanup") j = await api(`/api/provision/jobs/${id}/${act}`, { method: "POST", headers: azCsrf(), body: "{}", timeout: 60000 });
    else {
      const body = act === "ondemand" ? { spot: false } : { zone: ($("az-altzone") || {}).value || "" };
      j = await api(`/api/provision/jobs/${id}/change`, { method: "POST", headers: azCsrf(), body: JSON.stringify(body), timeout: 60000 });
      az.spec = { ...azDefaults(), ...j.spec, zone: j.spec.zone || "" };
      azTakeJob(j, true);
      try { localStorage.setItem(AZ_JOB_KEY, j.id); } catch (e) { /* storage may be unavailable */ }
    }
    if (act === "retry" || act === "cleanup") azTakeJob(j, false);
    azStartPoll();
  } catch (e) { az.err = e.message || "failed"; }
  az.acting = false;
  renderPeers();
}

function azResume() {
  let id = "";
  try { id = localStorage.getItem(AZ_JOB_KEY) || ""; } catch (e) { return; }
  if (!id || az.open) return;
  api(`/api/provision/jobs/${encodeURIComponent(id)}`, { timeout: 20000 }).then((j) => {
    az.open = true; az.step = 5; az.spec = { ...azDefaults(), ...j.spec, zone: j.spec.zone || "" };
    pz.via = j.via; azTakeJob(j, true);
    if (j.state === "running" || j.state === "cleaning") azStartPoll();
    renderPeers();
  }).catch(() => { try { localStorage.removeItem(AZ_JOB_KEY); } catch (e) { /* ignore */ } });
}

function initAddHost() {
  const el = $("pr-prices");
  el.addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (!b || b.disabled) return;
    if (b.id === "az-add") azStart("");
    else if (b.id === "az-cancel" || b.id === "az-done") azClose();
    else if (b.id === "az-back") {
      az.step = Math.max(1, az.step - 1); az.err = ""; renderPeers();
    } else if (b.id === "az-next") {
      az.err = "";
      if (az.step === 4) { azSubmit(); return; }
      az.step++;
      if (az.step === 4) { renderPeers(); azPlan(); return; }
      renderPeers();
    } else if (b.id === "az-replan") azPlan();
    else if (b.dataset.azPick) {
      az.type = b.dataset.azPick; az.spec.type = az.type; az.spec.zone = ""; renderPeers();
    } else if (b.dataset.azAct) azAct(b.dataset.azAct);
  });
  const edit = (ev) => {
    const t = ev.target;
    if (!az.open || !az.spec) return;
    const n = (v) => (v === "" ? 0 : Number(v));
    if (t.name === "az-kind") {
      const keep = az.spec.name; az.spec = azDefaults("", t.value); az.spec.name = keep; az.type = ""; az.plan = null;
      pz.kind = t.value === "gcp" ? "gcp" : "aws"; pz.cat = null; pz.catKey = ""; pz.catErr = "";
      const own = pzKindRegion(azNode(), pz.kind); if (own) pz.region = own;
      renderPeers(); return;
    }
    if (t.id === "az-via") { pz.via = t.value; const nd = azNode(); pz.region = pzKindRegion(nd, pz.kind) || pz.region; pz.cat = null; pz.catKey = ""; renderPeers(); return; }
    if (t.id === "az-name") { az.spec.name = t.value.trim().toLowerCase(); const nameBad = !!az.spec.name && !AZ_NAME.test(az.spec.name); t.classList.toggle("bad", nameBad); if (t.nextElementSibling) t.nextElementSibling.classList.toggle("warn", nameBad); const nb = $("az-next"); if (nb) nb.disabled = !(AZ_NAME.test(az.spec.name) && (azSandbox() || az.spec.budget_cap_usd >= 1)); return; }
    if (t.id === "az-spot") az.spec.spot = t.value === "1";
    else if (t.id === "az-zone") az.spec.zone = t.value;
    else if (t.id === "az-boot") az.spec.boot_disk_gb = n(t.value);
    else if (t.id === "az-data") az.spec.data_disk_gb = n(t.value);
    else if (t.id === "az-idle") az.spec.idle_minutes = n(t.value);
    else if (t.id === "az-budget") az.spec.budget_cap_usd = n(t.value);
    else if (t.id === "az-destroy") az.spec.destroy_after_hours = n(t.value);
    else if (t.id === "az-ack") { az.ack = t.checked; const nb = $("az-next"); if (nb) nb.disabled = !(az.plan && az.plan.ok && az.plan.ready && az.ack && !az.submitting); return; }
    else return;
    azUpdateFoot();
    if (t.tagName === "SELECT") renderPeers();
  };
  el.addEventListener("input", edit);
  el.addEventListener("change", edit);
}

/* ---------- Usage: tokens spent by agents ---------- */
//
// A screen of its own (#/usage), like Peers. The numbers come from GET
// /api/usage: counts and labels the hub gathered from every host's session
// files, never their text. "Fresh" tokens are input + output + cache writes;
// cache reads are 10x cheaper and 98% of the volume, so they are shown
// apart (and only added in by the "All tokens" measure). Charts are drawn
// here as inline SVG from theme tokens: nothing is loaded from elsewhere.
//   #/usage?r=14&d=kind&m=fresh&p=<project>&h=<handoff>
// r: days back (1, 7, 14, 30, 90), d: what the chart splits by, m: fresh|all|cost,
// p, h: the drill-down (project, then handoff, then its sessions).

const US_RANGES = [[1, "24h"], [7, "7 days"], [14, "14 days"], [30, "30 days"], [90, "90 days"]];
const US_DIMS = [["kind", "Agent"], ["model", "Model"], ["host", "Host"], ["project", "Project"], ["role", "Role"], ["purpose", "Purpose"], ["token", "Token type"]];
const US_MEAS = [["fresh", "Fresh tokens"], ["all", "All tokens"], ["cost", "Est. dollars"]];
const US_COLORS = ["--blue", "--purple", "--green", "--orange", "--teal", "--yellow", "--red"];
const US_TOKEN_COLOR = { input: "--blue", output: "--orange", "cache write": "--purple", "cache read": "--teal" };
const US_MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

const us = {
  route: null, timer: null, seq: 0, r: 14, d: "kind", m: "fresh", p: "", h: "",
  status: "loading", err: "",           // loading | ok | off | err
  main: null, prev: null, drill: null, roles: null, models: null,
  hosts: [], asOf: 0, picked: -1, prices: null, pricesMsg: "", hLabel: "",
};

function parseUsageRoute() {
  const m = location.hash.match(/^#\/usage(?:\?(.*))?$/);
  if (!m) return null;
  const q = new URLSearchParams(m[1] || "");
  const r = +q.get("r");
  return {
    r: US_RANGES.some((x) => x[0] === r) ? r : 14,
    d: US_DIMS.some((x) => x[0] === q.get("d")) ? q.get("d") : "kind",
    m: US_MEAS.some((x) => x[0] === q.get("m")) ? q.get("m") : "fresh",
    p: (q.get("p") || "").slice(0, 200), h: (q.get("h") || "").slice(0, 200),
  };
}

function usageHash() {
  const q = new URLSearchParams();
  if (us.r !== 14) q.set("r", us.r);
  if (us.d !== "kind") q.set("d", us.d);
  if (us.m !== "fresh") q.set("m", us.m);
  if (us.p) q.set("p", us.p);
  if (us.h) q.set("h", us.h);
  const s = q.toString();
  return "#/usage" + (s ? "?" + s : "");
}

// usageSet changes the screen's state, keeps the URL in step (no history entry) and reloads what moved.
function usageSet(patch, reload) {
  Object.assign(us, patch);
  us.picked = -1;
  if (location.hash !== usageHash()) history.replaceState(null, "", usageHash());
  if (reload) usageLoad(); else renderUsage();
}

// usNice rounds a chart's top up to a round number with four even gridlines (4, 8, 12, 16, 20 ... of a unit).
function usNice(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const k of [1, 1.2, 1.6, 2, 2.4, 3, 4, 5, 6, 8, 10]) if (k * p >= v) return k * p;
  return 10 * p;
}
const usFmtN = (n) => { const a = Math.abs(n); return a >= 1e9 ? (n / 1e9).toFixed(a >= 1e10 ? 1 : 2) + "B" : a >= 1e6 ? (n / 1e6).toFixed(a >= 1e8 ? 0 : 1) + "M" : a >= 1e3 ? (n / 1e3).toFixed(a >= 1e5 ? 0 : 1) + "k" : String(Math.round(n)); };
const usFmtD = (n) => "$" + (n >= 1000 ? Math.round(n).toLocaleString("en-US") : n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2));
const usPct = (x) => (x * 100 >= 99.95 ? "100" : (x * 100).toFixed(x * 100 >= 10 ? 0 : 1)) + "%";
// value of a group (or the totals) in the chosen measure
function usVal(t, m = us.m) { return m === "cost" ? (t.usd || 0) : m === "all" ? (t.fresh || 0) + (t.cr || 0) : (t.fresh || 0); }
const usFmt = (v, m = us.m) => (m === "cost" ? usFmtD(v) : usFmtN(v));
function usBucketVal(g, i, m = us.m) { return m === "cost" ? (g.usd_series ? g.usd_series[i] : 0) : m === "all" ? g.series[i] + (g.cr_series ? g.cr_series[i] : 0) : g.series[i]; }

function usQuery(extra) {
  const q = new URLSearchParams({ since: us.r + "d", tz: String(-new Date().getTimezoneOffset()), ...extra });
  if (us.r === 1) { q.set("since", "24h"); q.set("bucket", "hour"); }
  return "/api/usage?" + q;
}

async function usageLoad() {
  const seq = ++us.seq;
  const d = us.d === "token" && us.m === "cost" ? "kind" : us.d;
  const drill = us.h ? { by: "session", project: us.p, handoff: us.h } : us.p ? { by: "handoff", project: us.p } : { by: "project" };
  try {
    const [main, prev, dr, roles, models] = await Promise.all([
      api(usQuery({ by: d })),
      api("/api/usage?" + new URLSearchParams({ since: us.r * 2 + "d", to: us.r + "d", by: "kind", bucket: "day" })),
      api(usQuery({ ...drill, limit: "25" })),
      api(usQuery({ by: "role" })),
      api(usQuery({ by: "model" })),
    ]);
    if (seq !== us.seq) return;
    if (us.h && !us.hLabel && dr.groups && dr.groups[0] && dr.groups[0].label) us.hLabel = dr.groups[0].label; // arrived by a link
    Object.assign(us, { main, prev, drill: dr, roles, models, hosts: main.hosts || [], asOf: main.as_of, status: "ok", err: "" });
  } catch (e) {
    if (seq !== us.seq) return;
    us.status = e.status === 404 ? "off" : "err";
    us.err = e.message;
  }
  renderUsage();
}

function usageTiles() {
  const t = us.main.totals, pt = us.prev.totals;
  const tot = (t.fresh || 0), ptot = (pt.fresh || 0);
  let delta = "";
  if (ptot > 0) {
    const ch = (tot - ptot) / ptot;
    delta = `<div class="d ${ch > 0.005 ? "up" : ch < -0.005 ? "down" : ""}">${ch > 0.005 ? "▲" : ch < -0.005 ? "▼" : "▬"} ${Math.abs(Math.round(ch * 100))}% vs the ${us.r === 1 ? "day" : us.r + " days"} before</div>`;
  } else delta = '<div class="d">nothing before this to compare</div>';
  const parts = [["input", t.in], ["output", t.out], ["cache write", t.cw]];
  const stack = tot ? `<div class="us-stack" role="img" aria-label="${parts.map(([n, v]) => `${n} ${usPct(v / tot)}`).join(", ")}">${parts.map(([n, v]) => `<i style="width:${(v / tot) * 100}%;background:var(${US_TOKEN_COLOR[n]})"></i>`).join("")}</div><div class="d">${parts.map(([n, v]) => `<span style="color:var(${US_TOKEN_COLOR[n]})">■</span> ${n} ${usPct(v / tot)}`).join(" &nbsp;")}</div>` : "";
  const unp = t.unpriced ? ` · ${usFmtN(t.unpriced)} fresh tokens have no price` : "";
  return `<div class="us-tiles">
    <div class="us-tile"><span class="k">Fresh tokens</span><span class="v">${usFmtN(tot)}</span>${delta}${stack}</div>
    <div class="us-tile"><span class="k">Cache reads</span><span class="v">${usFmtN(t.cr || 0)}</span><div class="d">shown apart; ${usFmtN(t.calls || 0)} replies</div></div>
    <div class="us-tile"><span class="k">Cache hit</span><span class="v">${usPct(t.hit || 0)}</span><div class="d">of the prompt side came from the cache · ${(us.main.cache.read_per_write || 0).toFixed(1)} reads per write</div></div>
    <div class="us-tile"><span class="k">Est. cost</span><span class="v">${usFmtD(t.usd || 0)}</span><div class="d">list prices, an estimate${esc(unp)}</div></div>
  </div>`;
}

// usageChart draws the stacked bars for the main query in the chosen measure.
function usageChart() {
  const r = us.main, el = $("us-chart");
  let groups = r.groups.filter((g) => us.m === "all" || !g.apart);
  if (r.by === "token" && us.m === "cost") groups = groups.filter((g) => !g.apart);
  const n = r.buckets.length;
  const color = (g, i) => (g.key === "other" ? "--muted" : r.by === "token" ? US_TOKEN_COLOR[g.key] || US_COLORS[i % 7] : US_COLORS[i % US_COLORS.length]);
  const rows = r.buckets.map((_, i) => groups.map((g) => usBucketVal(g, i)));
  const W = Math.max(300, Math.floor(el.clientWidth) || 900), H = W < 500 ? 230 : 280, L = 46, R = 6, T = 8, B = 28;
  const max = Math.max(0, ...rows.map((row) => row.reduce((a, b) => a + b, 0)));
  const top = usNice(max), y = (v) => H - B - (v / top) * (H - B - T);
  const slot = (W - L - R) / Math.max(1, n), bw = Math.max(2, slot * 0.7);
  const hourly = r.bucket === "hour";
  const lab = (i) => { const d = new Date(r.buckets[i] * 1000); return hourly ? String(d.getHours()).padStart(2, "0") + ":00" : US_MON[d.getMonth()] + " " + d.getDate(); };
  const every = Math.max(1, Math.ceil(46 / slot));
  let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Tokens per ${hourly ? "hour" : "day"}, stacked by ${esc(r.by)}">`;
  for (let i = 0; i <= 4; i++) {
    const v = (top * i) / 4;
    svg += `<line x1="${L}" x2="${W - R}" y1="${y(v)}" y2="${y(v)}" stroke="var(--border)"/><text x="${L - 6}" y="${y(v) + 4}" text-anchor="end" font-size="11" fill="var(--muted)">${usFmt(v)}</text>`;
  }
  for (let i = 0; i < n; i++) {
    const cx = L + slot * i + slot / 2;
    let acc = 0, bars = "";
    rows[i].forEach((v, j) => {
      if (v <= 0) return;
      bars += `<rect x="${cx - bw / 2}" y="${y(acc + v)}" width="${bw}" height="${Math.max(0.6, y(acc) - y(acc + v))}" fill="var(${color(groups[j], j)})"/>`;
      acc += v;
    });
    const sel = us.picked === i;
    svg += `<g tabindex="0" data-i="${i}" role="button" aria-label="${esc(lab(i))}: ${usFmt(acc)}">${sel ? `<rect x="${cx - slot / 2}" y="${T}" width="${slot}" height="${H - B - T}" fill="var(--panel2)"/>` : ""}${bars}<rect x="${cx - slot / 2}" y="${T}" width="${slot}" height="${H - B - T}" fill="transparent"/></g>`;
    if (i % every === 0) svg += `<text x="${cx}" y="${H - B + 16}" text-anchor="middle" font-size="11" fill="var(--muted)">${esc(lab(i))}</text>`;
  }
  svg += "</svg>";
  el.innerHTML = svg;
  el._groups = groups; el._lab = lab; el._color = color;
  $("us-legend").innerHTML = groups.map((g, j) => `<span><i style="background:var(${color(g, j)})"></i>${esc(g.label && g.key !== g.label ? g.label : g.key)} <b>${usFmt(usVal(g.key === "cache read" ? { cr: g.cr, fresh: 0 } : g, g.key === "cache read" ? "all" : us.m))}</b></span>`).join("");
  usageReadout();
}

function usageReadout() {
  const el = $("us-chart"), box = $("us-read");
  const i = us.picked;
  if (i < 0 || !el._groups) { box.textContent = "Tap or focus a bar for its numbers."; return; }
  const parts = el._groups.map((g) => [g.label && g.key !== g.label ? g.label : g.key, usBucketVal(g, i)]).filter(([, v]) => v > 0);
  const total = parts.reduce((a, [, v]) => a + v, 0);
  box.textContent = `${el._lab(i)}: ${usFmt(total)}${parts.length ? " — " + parts.map(([k, v]) => `${k} ${usFmt(v)}`).join(", ") : ""}`;
}

function usageDrill() {
  const r = us.drill;
  const level = us.h ? 2 : us.p ? 1 : 0;
  const crumbs = [`<button type="button" data-us-crumb="0">All projects</button>`];
  if (us.p) crumbs.push(`<span class="sep">›</span><button type="button" data-us-crumb="1">${esc(us.p)}</button>`);
  if (us.h) crumbs.push(`<span class="sep">›</span><button type="button" data-us-crumb="2">${esc(us.hLabel || us.h)}</button>`);
  const total = r.groups.reduce((a, g) => a + usVal(g), 0) || 1;
  const maxv = Math.max(1, ...r.groups.map((g) => usVal(g)));
  const noun = ["project", "handoff", "session"][level];
  const rows = r.groups.map((g) => {
    let name = g.label && g.label !== g.key ? g.label : g.key;
    let sub = `cache ${usPct(g.hit || 0)} · ${usFmtN(g.calls || 0)} replies · ${g.usd || !g.unpriced ? usFmtD(g.usd || 0) + (g.unpriced ? "+" : "") : "no price"}`;
    let clickable = level < 2 && g.key !== "(none)" && g.key !== "other";
    if (level === 2) {
      const [host, kind, id] = g.key.split("/");
      name = (g.label ? g.label + " · " : "") + (id ? id.slice(-8) : g.key);
      sub = `${host || ""} ${kind || ""} · ` + sub;
    }
    if (level === 1 && g.key === "(none)") name = "(no handoff)";
    const data = clickable ? ` data-us-down="${escAttr(g.key)}" data-us-label="${escAttr(name)}"` : "";
    const inner = `<span class="nm">${esc(name)}</span><span class="val">${usFmt(usVal(g))} <span class="us-lab">${(usVal(g) / total * 100).toFixed(0)}%</span></span><span class="bar"><i style="width:${(usVal(g) / maxv) * 100}%"></i></span><span class="sub">${esc(sub)}</span>`;
    return clickable ? `<button type="button" class="us-row"${data}>${inner}</button>` : `<div class="us-row">${inner}</div>`;
  }).join("");
  return `<section class="us-card" aria-label="Where it went"><h2>Where it went: project › handoff › session</h2>
    <div class="us-crumbs">${crumbs.join("")}</div>
    <div class="us-rows">${rows || `<div class="us-empty">Nothing in this window.</div>`}</div>
    <p class="us-note">By ${noun}, in ${esc(US_MEAS.find((x) => x[0] === us.m)[1].toLowerCase())}. ${level < 2 ? "Click a name to go down a level." : "Sessions are shown by their last 8 characters; a handoff's name is the worker it started."}${r.sessions_from && r.from < r.sessions_from ? " Hours older than 180 days are rollups: no sessions or handoffs." : ""}</p></section>`;
}

function usageCoordinator() {
  const g = (us.roles.groups || []).find((x) => x.key === "coordinator");
  const wk = (us.roles.groups || []).find((x) => x.key === "worker");
  if (!g) return `<section class="us-card us-coord" aria-label="Coordinator"><h2>The coordinator's own usage</h2><div class="us-empty">No coordinator sessions in this window.</div></section>`;
  const ctx = (x) => (x && x.replies && x.replies.reduce((a, b) => a + b, 0) ? x.prompt.reduce((a, b) => a + b, 0) / x.replies.reduce((a, b) => a + b, 0) : 0);
  const totalFresh = us.roles.totals.fresh || 1;
  const line = usageLines(us.roles.buckets, [
    { name: "Coordinator", color: "--purple", v: g.prompt.map((p, i) => (g.replies[i] ? p / g.replies[i] : null)) },
    ...(wk ? [{ name: "Workers", color: "--blue", v: wk.prompt.map((p, i) => (wk.replies[i] ? p / wk.replies[i] : null)) }] : []),
  ]);
  return `<section class="us-card us-coord" aria-label="Coordinator"><h2>The coordinator's own usage</h2>
    <div class="us-kv"><div><b>${usFmtN(g.fresh)}</b><span>fresh tokens (${usPct(g.fresh / totalFresh)} of all)</span></div>
    <div><b>${usFmtN(g.calls || 0)}</b><span>replies</span></div>
    <div><b>${usFmtN(ctx(g))}</b><span>context sent per request${wk ? `, workers ${usFmtN(ctx(wk))}` : ""}</span></div>
    <div><b>${usPct(g.hit || 0)}</b><span>cache hit</span></div></div>
    <b style="font-size:13px">Average context per request, by ${us.roles.bucket}</b>
    <div class="us-chart" role="img" aria-label="Average context sent per request for the coordinator and for workers">${line}</div>
    <div class="us-legend"><span><i style="background:var(--purple)"></i>Coordinator</span>${wk ? '<span><i style="background:var(--blue)"></i>Workers</span>' : ""}</div>
    <p class="us-note">A coordinator is a session in the directory a running coordinator agent works in; it is a guess for sessions seen while none ran.</p></section>`;
}

function usageLines(buckets, lines) {
  const W = Math.max(280, Math.floor(($("us-view").clientWidth || 600) / ($("us-view").clientWidth > 820 ? 2 : 1)) - 30), H = 170, L = 46, R = 8, T = 8, B = 24;
  const max = Math.max(1, ...lines.flatMap((l) => l.v.filter((x) => x != null)));
  const top = usNice(max);
  const y = (v) => H - B - (v / top) * (H - B - T), x = (i) => L + (i * (W - L - R)) / Math.max(1, buckets.length - 1);
  let svg = `<svg viewBox="0 0 ${W} ${H}">`;
  for (let i = 0; i <= 4; i++) svg += `<line x1="${L}" x2="${W - R}" y1="${y((top * i) / 4)}" y2="${y((top * i) / 4)}" stroke="var(--border)"/><text x="${L - 6}" y="${y((top * i) / 4) + 4}" text-anchor="end" font-size="11" fill="var(--muted)">${usFmtN((top * i) / 4)}</text>`;
  const every = Math.max(1, Math.ceil(buckets.length / 6));
  buckets.forEach((b, i) => { if (i % every === 0) { const d = new Date(b * 1000); svg += `<text x="${x(i)}" y="${H - 6}" text-anchor="middle" font-size="11" fill="var(--muted)">${US_MON[d.getMonth()]} ${d.getDate()}</text>`; } });
  for (const l of lines) {
    let seg = [];
    const flush = () => { if (seg.length > 1) svg += `<polyline points="${seg.join(" ")}" fill="none" stroke="var(${l.color})" stroke-width="2.2"/>`; seg = []; };
    l.v.forEach((v, i) => { if (v == null) { flush(); return; } seg.push(`${x(i)},${y(v)}`); svg += `<circle cx="${x(i)}" cy="${y(v)}" r="3" fill="var(${l.color})"><title>${esc(l.name)}: ${usFmtN(v)}</title></circle>`; });
    flush();
  }
  return svg + "</svg>";
}

function usageCache() {
  const gs = (us.models.groups || []).filter((g) => g.key !== "other" || g.cr);
  const rows = gs.map((g) => {
    const rw = g.cw ? g.cr / g.cw : 0;
    const bad = g.hit < 0.8 && g.cr + g.cw + g.in > 5000;
    return `<div class="us-row"><span class="nm">${esc(g.key)}</span><span class="val" style="${bad ? "color:var(--yellow)" : ""}">${usPct(g.hit || 0)}</span><span class="bar"><i style="width:${(g.hit || 0) * 100}%;background:var(${bad ? "--yellow" : "--green"})"></i></span><span class="sub">${usFmtN(g.cr)} read · ${g.cw ? `${usFmtN(g.cw)} written · ${rw.toFixed(1)} reads per write` : "no cache writes recorded"}</span></div>`;
  }).join("");
  const c = us.main.cache, sv = c.saved_usd ? ` Cache reads saved an estimated ${usFmtD(c.saved_usd)} against paying the input price.` : "";
  return `<section class="us-card" aria-label="Cache efficiency"><h2>Cache efficiency</h2><div class="us-rows">${rows || '<div class="us-empty">No models in this window.</div>'}</div>
    <p class="us-note">Hit = cache reads ÷ (input + cache writes + cache reads). Under 80% turns yellow.${esc(sv)}</p></section>`;
}

function usagePricesCard() {
  return `<section class="us-card us-prices" aria-label="Prices"><details id="us-pricedet"><summary>Prices (estimates)</summary><div id="us-pricebody"><div class="us-empty">Loading…</div></div></details></section>`;
}

async function usageLoadPrices() {
  try { us.prices = await api("/api/usage/prices"); } catch (e) { us.prices = { error: e.message }; }
  renderUsagePrices();
}

function renderUsagePrices() {
  const box = $("us-pricebody");
  if (!box) return;
  const p = us.prices;
  if (!p) { box.innerHTML = '<div class="us-empty">Loading…</div>'; return; }
  if (p.error) { box.innerHTML = `<div class="us-empty">${esc(p.error)}</div>`; return; }
  const seen = (us.models ? us.models.groups : []).filter((g) => g.unpriced).map((g) => g.key);
  const names = [...new Set([...Object.keys(p.defaults), ...Object.keys(p.custom || {}), ...seen])].sort();
  const cell = (m, f) => { const v = (p.custom && p.custom[m]) || p.defaults[m]; return v ? v[f] : ""; };
  const rows = names.map((m) => `<tr data-model="${escAttr(m)}"><td>${esc(m)}${seen.includes(m) ? ' <span class="us-lab">no price</span>' : ""}</td>${["in", "out", "cw", "cr"].map((f) => `<td><input type="number" min="0" step="any" inputmode="decimal" data-f="${f}" value="${cell(m, f)}" aria-label="${esc(m)} ${f}"></td>`).join("")}</tr>`).join("");
  box.innerHTML = `<p class="us-note">Dollars per million tokens. The defaults are Anthropic's list prices for the Claude models (cache writes at the 5-minute rate): an estimate, not an invoice. Models with no price — Pi's — count tokens only until you add one.</p>
    <div class="tw"><table><thead><tr><th>Model</th><th>Input</th><th>Output</th><th>Cache write</th><th>Cache read</th></tr></thead><tbody>${rows}
    <tr id="us-newrow"><td><input class="mn" type="text" id="us-newmodel" placeholder="add a model name" aria-label="Model name to add" maxlength="120" autocomplete="off" autocapitalize="off" spellcheck="false"></td>${["in", "out", "cw", "cr"].map((f) => `<td><input type="number" min="0" step="any" inputmode="decimal" data-f="${f}" aria-label="New model ${f}"></td>`).join("")}</tr></tbody></table></div>
    <div class="pr-acts" style="margin-top:8px"><button type="button" class="btn primary" id="us-saveprices">Save prices</button><button type="button" class="btn" id="us-resetprices">Reset to defaults</button><span class="us-note" id="us-pricemsg">${esc(us.pricesMsg)}</span></div>`;
}

async function usageSavePrices(reset) {
  const p = us.prices;
  if (!p || p.error) return;
  const custom = {};
  if (!reset) {
    const num = (v) => (v === "" || v == null ? 0 : +v);
    for (const tr of $("us-pricebody").querySelectorAll("tr[data-model], #us-newrow")) {
      const m = tr.id === "us-newrow" ? $("us-newmodel").value.trim() : tr.dataset.model;
      if (!m) continue;
      const v = {};
      tr.querySelectorAll("input[data-f]").forEach((i) => { v[i.dataset.f] = num(i.value); });
      const d = p.defaults[m];
      if (d ? ["in", "out", "cw", "cr"].every((f) => d[f] === v[f]) : ["in", "out", "cw", "cr"].every((f) => !v[f])) continue;
      custom[m] = v;
    }
    // a default model whose prices were all cleared stays removed
  }
  try {
    us.prices = await api("/api/usage/prices", { method: "PUT", body: JSON.stringify({ custom }) });
    us.pricesMsg = reset ? "Back to the defaults." : "Saved.";
    usageLoad();
  } catch (e) { us.pricesMsg = e.message; }
  renderUsagePrices();
}

function usageHosts() {
  const bad = us.hosts.filter((h) => h.err);
  const warn = bad.length ? `<div class="us-warn" role="status">${bad.map((h) => `<b>${esc(h.name)}</b>: ${esc(h.err)}`).join("<br>")}<br>Its tokens are missing or stale until it answers.</div>` : "";
  return warn;
}

function renderUsage() {
  if (!us.route) return;
  const el = $("us-view");
  $("us-fresh").textContent = us.status === "ok" ? "as of " + new Date(us.asOf * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) + (us.hosts.length > 1 ? ` · ${us.hosts.length} hosts` : "") : "";
  if (us.status === "loading") { el.innerHTML = '<div class="us-empty">Loading…</div>'; return; }
  if (us.status === "off") { el.innerHTML = '<div class="us-empty">Token counting is off on this instance (it needs the state directory and <code>-usage</code>).</div>'; return; }
  if (us.status === "err") { el.innerHTML = `<div class="us-empty">${esc(us.err)}</div>`; return; }
  const seg = (name, items, cur, attr, dis) => `<span class="us-seg" role="group" aria-label="${name}">${items.map(([v, l]) => `<button type="button" ${attr}="${v}" aria-pressed="${cur === v}"${dis && dis(v) ? " disabled" : ""}>${esc(l)}</button>`).join("")}</span>`;
  const open = $("us-pricedet") && $("us-pricedet").open;
  const keep = $("us-body").scrollTop;
  el.innerHTML = `${usageHosts()}
    <div class="us-ctl"><span class="us-lab">Range</span>${seg("Range", US_RANGES, us.r, "data-us-r")}<span class="us-lab">Measure</span>${seg("Measure", US_MEAS, us.m, "data-us-m")}</div>
    ${usageTiles()}
    <section class="us-card" aria-label="Over time"><h2>Over time, split by</h2>
      <div class="us-ctl" style="margin-bottom:8px">${seg("Split by", US_DIMS, us.d, "data-us-d")}</div>
      <div class="us-chart" id="us-chart"></div><div class="us-legend" id="us-legend"></div><div class="us-read" id="us-read" aria-live="polite"></div></section>
    ${usageDrill()}
    <div class="us-two">${usageCoordinator()}${usageCache()}</div>
    ${usagePricesCard()}`;
  usageChart();
  if (open) { $("us-pricedet").open = true; renderUsagePrices(); }
  $("us-body").scrollTop = keep;
}

function usageClick(ev) {
  const t = ev.target;
  let b;
  if ((b = t.closest("[data-us-r]"))) return usageSet({ r: +b.dataset.usR }, true);
  if ((b = t.closest("[data-us-m]"))) return usageSet({ m: b.dataset.usM }, false);
  if ((b = t.closest("[data-us-d]"))) return usageSet({ d: b.dataset.usD }, true);
  if ((b = t.closest("[data-us-crumb]"))) {
    const l = +b.dataset.usCrumb;
    return usageSet({ p: l >= 1 ? us.p : "", h: l >= 2 ? us.h : "", hLabel: l >= 2 ? us.hLabel : "" }, true);
  }
  if ((b = t.closest("[data-us-down]"))) {
    if (!us.p) return usageSet({ p: b.dataset.usDown, h: "", hLabel: "" }, true);
    return usageSet({ h: b.dataset.usDown, hLabel: b.dataset.usLabel }, true);
  }
  if ((b = t.closest("g[data-i]"))) { us.picked = +b.dataset.i; usageChart(); return; }
  if (t.closest("#us-saveprices")) return usageSavePrices(false);
  if (t.closest("#us-resetprices")) return usageSavePrices(true);
}

function syncUsage() {
  const r = parseUsageRoute();
  const el = $("usage");
  const was = !el.hidden;
  us.route = r;
  el.hidden = !r;
  if (!r) { clearInterval(us.timer); us.timer = null; return; }
  const moved = r.r !== us.r || r.d !== us.d || r.m !== us.m || r.p !== us.p || r.h !== us.h;
  Object.assign(us, r);
  if (!was || moved) { if (!was) us.status = "loading"; us.picked = -1; usageLoad(); }
  // The Escape handler hears only keys pressed inside the screen, so focus goes in as the screen opens;
  // without it Escape worked only while focus happened to be left on Back from an earlier visit.
  if (!was) { us.timer = setInterval(usageLoad, 60000); ($("us-back").offsetParent ? $("us-back") : $("us-body")).focus({ preventScroll: true }); }
  renderUsage();
}

function closeUsage() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

/* ---------- Settings: passkeys ---------- */
//
// A screen of its own (#/settings), like Usage. For now it holds the passkeys
// that approve actions with Face ID: the hub's list (GET /api/passkeys), and
// register, rename and remove (CSRF token from that list). Apple Passwords
// passkeys only, and PRF is required: the browser makes the passkey, then does
// one assertion asking for PRF; with no result it stops and says how to turn
// Passwords on (the hub refuses such a registration as well). A second and
// later passkey is approved by an existing one first (an approval of action
// passkey-add, answered with Face ID). Nothing secret reaches this page: the
// PRF output of the check is dropped at once and nothing is stored.

const SX_NO_PRF = "This passkey cannot do PRF. In iOS, turn on Passwords under Settings, General, AutoFill & Passwords, AutoFill From.";
const sx = { route: false, status: "loading", err: "", list: [], rp: "", csrf: "", max: 16, busy: "", msg: "", msgErr: false, confirm: "", rename: "", name: "" };

const sxB64 = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const sxUnb64 = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));

function sxSupported() { return !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext); }

async function sxLoad() {
  try {
    const d = await api("/api/passkeys");
    Object.assign(sx, { list: d.passkeys || [], rp: d.rp_id || "", csrf: d.csrf || "", max: d.max || 16, status: "ok", err: "" });
  } catch (e) {
    sx.status = e.status === 404 || e.status === 503 ? "off" : "err";
    sx.err = e.message;
  }
  renderSettings();
}

const sxCall = (method, path, body) => api(path, { method, headers: { "X-Comebye-Csrf": sx.csrf }, body: JSON.stringify(body || {}) });

function sxSay(msg, isErr) { sx.msg = msg; sx.msgErr = !!isErr; renderSettings(); }

function sxAgo(iso) { const t = Date.parse(iso); return Number.isNaN(t) ? "" : fmtAgo(t) + " ago"; }

function sxCard(p) {
  const id = escAttr(p.id);
  if (sx.confirm === p.id) {
    return `<div class="card sx-card" data-id="${id}"><span class="sx-name">${esc(p.name)}</span>
      <div class="sx-confirm" role="group" aria-label="Confirm removing ${escAttr(p.name)}"><div>Remove <b>${esc(p.name)}</b>? It can no longer approve anything here. Also delete it in the iOS Passwords app. Hosts that pinned it keep trusting it until they are told to drop it.</div>
      <div class="sx-acts"><button type="button" class="btn danger" data-sx="remove-go"${sx.busy ? " disabled" : ""}>Remove ${esc(p.name)}</button><button type="button" class="btn" data-sx="cancel">Cancel</button></div></div></div>`;
  }
  if (sx.rename === p.id) {
    return `<div class="card sx-card" data-id="${id}"><form class="sx-acts" data-sx-form="rename"><input type="text" id="sx-rename" value="${escAttr(p.name)}" maxlength="60" aria-label="Name of this passkey" autocomplete="off" enterkeyhint="done">
      <button type="submit" class="btn primary"${sx.busy ? " disabled" : ""}>Save</button><button type="button" class="btn" data-sx="cancel">Cancel</button></form></div>`;
  }
  const meta = [
    `<span>Fingerprint <span class="sx-fp">${esc(p.fingerprint)}</span></span>`,
    p.created_at ? `<span>Added ${esc(sxAgo(p.created_at))}</span>` : "",
    `<span>${p.last_used_at ? "Last used " + esc(sxAgo(p.last_used_at)) : "Not used yet"}</span>`,
    p.backed_up ? `<span>Synced with iCloud</span>` : p.backup_eligible ? `<span>Can sync</span>` : `<span>This device only</span>`,
  ].filter(Boolean).join("");
  return `<div class="card sx-card" data-id="${id}" data-fp="${escAttr(p.fingerprint)}"><span class="sx-name">${esc(p.name)}</span><div class="sx-meta">${meta}</div>
    <div class="sx-acts"><button type="button" class="btn sm" data-sx="rename"${sx.busy ? " disabled" : ""}>Rename</button><button type="button" class="btn sm danger" data-sx="remove"${sx.busy ? " disabled" : ""}>Remove</button></div></div>`;
}

// The Appearance section: device-level choices kept in localStorage (web/themes.js), applied to <html> before
// first paint. Reading width is four presets; the strip draws the column to scale on this window's conversation pane.
function sxAppearance() {
  const cur = CT.colChoice();
  const pane = Math.max(1, Math.round($("settings").getBoundingClientRect().width || window.innerWidth)); // the screen is the conversation pane
  const colPx = (c) => (c.px ? Math.min(c.px, pane - 20) : pane - 20);
  const sel = CT.COLS.find((c) => c.id === cur);
  const w = colPx(sel);
  const pct = (n) => (Math.max(0, Math.min(1, n / pane)) * 100).toFixed(2);
  const same = CT.COLS.every((c) => Math.round(colPx(c)) === Math.round(colPx(CT.COLS[0])));
  const caption = same ? `On this window the column is always ${Math.round(w)} px: this setting has no effect here.`
    : `On this window the column is ${Math.round(w)} px of the ${pane} px conversation pane.` + (sel.id === "full" ? " Full means very long lines of prose." : "");
  return `<h2>Appearance</h2>
    <p class="sx-note">Saved on this device only.</p>
    <div class="sx-field"><div class="sx-lbl" id="sx-col-lbl">Reading width</div>
      ${sxRadios("col", "sx-col-lbl", CT.COLS, cur)}
      <div class="sx-strip" aria-hidden="true"><i class="sx-strip-col" style="left:${pct((pane - w) / 2)}%;width:${pct(w)}%"></i></div>
      <p class="sx-note" role="status">${esc(caption)}</p></div>
    <div class="sx-field"><div class="sx-lbl" id="sx-rs-lbl">Text size</div>
      ${sxRadios("rs", "sx-rs-lbl", CT.RSS, CT.rsChoice())}
      <p class="sx-note">Scales the conversation and the box you type in. The header, the list and the menus stay as they are. Wide and Wider pair well with a larger size.</p>
      <div class="sx-prev-wrap" aria-hidden="true"><div class="sx-prev" inert>
        <div class="msg user"><div class="bubble">Can you make the text easier to read?</div></div>
        <div class="msg assistant"><div class="bubble md"><p>This is how a reply reads on this window. A command such as <code>go test ./...</code> sits inside a sentence.</p><pre><code>func Scale(n int) int {\n\treturn n * 2 // a long comment on one line, longer than a phone is wide: scroll sideways to read it, or wrap it\n}</code></pre></div></div>
        <div class="thinking"><span class="tr-line" aria-expanded="${CT.openThink()}"><span class="tr-dot"></span><span class="tr-txt">Thought for 4s</span><span class="tr-chev">›</span></span><div class="think-body md"${CT.openThink() ? "" : " hidden"}><p>The user wants the text easier to read.</p></div></div>
        <div class="toolrun"><span class="tr-line" aria-expanded="${CT.openTools()}"><span class="tr-dot"></span><span class="tr-txt">Ran 3 commands</span><span class="tr-chev">›</span></span><div class="tr-list"${CT.openTools() ? "" : " hidden"}><div class="tr-item"><span class="tr-row"><span class="tr-v">Bash</span><span class="tr-o">go test ./...</span></span></div></div></div>
      </div></div>
      <p class="sx-note">A preview of the conversation at this window's width.</p></div>
    <div class="sx-field"><div class="sx-lbl" id="sx-sp-lbl">Spacing</div>
      ${sxRadios("sp", "sx-sp-lbl", CT.SPS, CT.spChoice())}
      <p class="sx-note">Line height in replies and thinking, and the space between messages. The preview above follows.</p></div>
    <h3 class="sx-grp">Code and conversation</h3>
    <div class="sx-field"><div class="sx-lbl" id="sx-wrap-lbl">Wrap long code lines</div>
      ${sxRadios("wrap", "sx-wrap-lbl", CT.WRAPS, CT.wrapChoice())}
      <p class="sx-note">Off scrolls a long line sideways. On wraps it in code blocks and tool output. Copying gives the original text either way. The preview above follows.</p></div>
    <div class="sx-field"><div class="sx-lbl" id="sx-openthink-lbl">Open thinking by default</div>
      ${sxRadios("openthink", "sx-openthink-lbl", CT.ONOFF, CT.openThink() ? "on" : "")}</div>
    <div class="sx-field"><div class="sx-lbl" id="sx-opentools-lbl">Open tool run lists by default</div>
      ${sxRadios("opentools", "sx-opentools-lbl", CT.ONOFF, CT.openTools() ? "on" : "")}
      <p class="sx-note">Sets only how a line first appears. One you open or close yourself stays as you left it. The preview above follows.</p></div>
    <h3 class="sx-grp">Theme and font</h3>
    <div class="sx-field sx-rows">
      <button type="button" class="sx-row" id="sx-row-theme" data-sx="theme" aria-haspopup="menu"><span class="sx-rl">Theme</span><span class="sx-rv" id="sx-theme-val">${esc(themeLabel())}</span><span class="sx-chev" aria-hidden="true">›</span></button></div>
    <div class="sx-field"><div class="sx-lbl" id="sx-depth-lbl">Surfaces</div>
      ${sxRadios("depth", "sx-depth-lbl", CT.DEPTHS, CT.depthChoice())}
      <p class="sx-note">Layered lifts cards, bars and menus a step above the page with quiet shadows, and tints the row you hover or select. Flat is one surface and a border, as before. Pages follow.</p></div>
    <div class="sx-field"><div class="sx-lbl" id="sx-font-lbl">Font</div>
      ${sxRadios("font", "sx-font-lbl", CT.FONTS, CT.fontChoice())}
      <p class="sx-note">Serif is for the text you read: replies, To do, Markdown and Pages. Code stays monospace; the key bar, action buttons and form fields stay sans.</p></div>
    <div class="sx-field"><button type="button" class="btn" id="sx-reset" data-sx="reset">Reset to defaults</button>
      <p class="sx-note">Puts reading width, text size, spacing, wrapping, open by default and surfaces back to the default. Your theme and font stay.</p></div>`;
}

// The Theme and Font rows open the same panels as the fleet menu, anchored under the row; the panel's trigger
// is the row, so Esc and a pick return the focus to it.
const sxRow = (n) => { const b = $("sx-row-" + n); return b && b.isConnected && b.getClientRects().length ? b : null; };
function sxOpenPanel(which) {
  toggleThemePanel(true, sxRow(which));
}
// A pick, or another tab, changed the theme: keep the row's value true.
function sxRowVals() {
  const t = $("sx-theme-val");
  if (t) t.textContent = themeLabel();
}
function sxReset() {
  CT.resetAppearance();
  renderSettings();
  const b = $("sx-view").querySelector("#sx-col-default");
  if (b) b.focus({ preventScroll: true });
}

function sxRadios(kind, lbl, list, cur) {
  return `<div class="sx-radios" role="radiogroup" aria-labelledby="${lbl}">${list.map((c) => `<button type="button" class="sx-radio" role="radio" id="sx-${kind}-${c.id || "default"}" aria-checked="${c.id === cur}" tabindex="${c.id === cur ? 0 : -1}" data-sx="${kind}" data-val="${c.id}"><span class="sx-rn">${esc(c.name)}</span><small>${esc(c.sub)}</small></button>`).join("")}</div>`;
}

// The preview is a copy of the conversation drawn at the window's own pane width and scaled down to fit the screen.
function sxFitPreview() {
  const wrap = document.querySelector("#sx-view .sx-prev-wrap"), pv = wrap && wrap.firstElementChild;
  if (!pv) return;
  const pane = Math.max(1, Math.round($("settings").getBoundingClientRect().width || window.innerWidth));
  const k = Math.min(1, wrap.clientWidth / pane);
  pv.style.width = pane + "px"; pv.style.transform = k < 1 ? `scale(${k})` : "";
  wrap.style.height = Math.ceil(pv.offsetHeight * k) + 2 + "px";
}

// The choice for a radio group by its kind ("col", "rs", "sp"): the list, and how to save it.
const SX_PREFS = () => ({ col: [CT.COLS, CT.colSet], rs: [CT.RSS, CT.rsSet], sp: [CT.SPS, CT.spSet], wrap: [CT.WRAPS, CT.wrapSet], depth: [CT.DEPTHS, CT.depthSet], font: [CT.FONTS, CT.fontSet], openthink: [CT.ONOFF, (v) => CT.openPart("think", !!v)], opentools: [CT.ONOFF, (v) => CT.openPart("tools", !!v)] });
function sxPick(kind, val) {
  const p = SX_PREFS()[kind];
  if (!p) return;
  p[1](val);
  renderSettings();
  const r = $("sx-view").querySelector(`#sx-${kind}-${val || "default"}`);
  if (r) r.focus({ preventScroll: true });
}

function sxColKey(ev) {
  const b = ev.target.closest && ev.target.closest(".sx-radio");
  const p = b && SX_PREFS()[b.dataset.sx];
  if (!p) return;
  const list = p[0], keys = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };
  let i = list.findIndex((c) => c.id === b.dataset.val);
  if (ev.key in keys) i = (i + keys[ev.key] + list.length) % list.length;
  else if (ev.key === "Home") i = 0;
  else if (ev.key === "End") i = list.length - 1;
  else return;
  ev.preventDefault();
  sxPick(b.dataset.sx, list[i].id);
}

function renderSettings() {
  const el = $("sx-view");
  if (!el || !sx.route) return;
  let h = sxAppearance() + `<h2>Passkeys</h2><p class="sx-note">A passkey approves a sensitive action with Face ID. Only Apple Passwords passkeys work, and they must support PRF: in iOS, turn on Passwords under Settings, General, AutoFill &amp; Passwords, AutoFill From.</p>`;
  if (sx.status === "loading") h += `<div class="empty">Loading…</div>`;
  else if (sx.status === "off") h += `<div class="empty"><b>Passkeys live on the hub</b><span>Open the hub's app to manage them.</span></div>`;
  else if (sx.status === "err") h += `<p class="sx-msg err" role="alert">${esc(sx.err)}</p>`;
  else {
    if (sx.rp && sx.rp !== location.hostname) h += `<p class="sx-note warn" role="alert">These passkeys belong to <b>${esc(sx.rp)}</b>. Open the app at that address to add or use one.</p>`;
    h += sx.list.length ? sx.list.map(sxCard).join("") : `<div class="empty"><b>No passkeys yet</b><span>Add one to approve actions with Face ID.</span></div>`;
    if (!sxSupported()) h += `<p class="sx-note warn">This browser cannot make passkeys here: they need a secure (https) address.</p>`;
    else if (sx.list.length >= sx.max) h += `<p class="sx-note">The list is full: remove a passkey to add another.</p>`;
    else h += `<form class="sx-add" data-sx-form="add"><label class="sx-note" for="sx-name">Name for the new passkey</label>
      <div class="sx-acts"><input type="text" id="sx-name" value="${escAttr(sx.name)}" maxlength="60" placeholder="${escAttr("Passkey " + (sx.list.length + 1))}" autocomplete="off" enterkeyhint="done">
      <button type="submit" class="btn primary" id="sx-add"${sx.busy ? " disabled" : ""}>${sx.busy === "add" ? "Waiting for Face ID…" : "Add a passkey"}</button></div>
      ${sx.list.length ? `<p class="sx-note">An existing passkey approves the new one first (Face ID, twice).</p>` : ""}</form>`;
  }
  if (sx.msg) h += `<p class="sx-msg${sx.msgErr ? " err" : ""}" role="${sx.msgErr ? "alert" : "status"}">${esc(sx.msg)}</p>`;
  if (el._html === h) return;
  el._html = h;
  const focus = document.activeElement && document.activeElement.id;
  el.innerHTML = h;
  const f = (sx.rename && $("sx-rename")) || (focus && $(focus));
  if (focus && /^sx-(col|rs|sp|wrap|openthink|opentools)-/.test(focus)) { const g = $(focus) && $(focus).closest(".sx-radios"), c = g && g.querySelector('.sx-radio[aria-checked="true"]'); if (c) c.focus({ preventScroll: true }); }
  sxFitPreview();
  if (f && f !== document.activeElement && (sx.rename || focus === "sx-name")) { f.focus({ preventScroll: true }); if (sx.rename) f.select(); }
}

// sxAssert asks for Face ID for an approval and posts the assertion. Resolves
// with the approval's answer; throws a message fit for the user.
async function sxAssert(approvalId, extra) {
  const d = await api(`/api/approvals/${encodeURIComponent(approvalId)}`);
  if (d.approval.state !== "pending") throw new Error("This request is no longer waiting: " + d.approval.state + ".");
  let cred;
  try {
    cred = await navigator.credentials.get({ publicKey: {
      challenge: sxUnb64(d.challenge), rpId: d.rp_id, userVerification: "required", timeout: 60000,
      allowCredentials: (d.credentials || []).map((c) => ({ type: "public-key", id: sxUnb64(c.id) })),
    } });
  } catch (e) { throw new Error(sxWebAuthnMsg(e)); }
  if (!cred) throw new Error("No passkey answered.");
  const r = cred.response;
  return sxCall("POST", `/api/approvals/${encodeURIComponent(approvalId)}/respond`, {
    credential_id: sxB64(cred.rawId), client_data_json: sxB64(r.clientDataJSON), authenticator_data: sxB64(r.authenticatorData), signature: sxB64(r.signature), ...(extra || {}),
  });
}

function sxWebAuthnMsg(e) {
  if (e && (e.name === "NotAllowedError" || e.name === "AbortError")) return "Face ID was cancelled or timed out.";
  if (e && e.name === "InvalidStateError") return "That passkey is already registered here.";
  return (e && e.message) || "The passkey could not be used.";
}

async function sxAdd() {
  if (sx.busy) return;
  const name = ($("sx-name") ? $("sx-name").value : sx.name).trim();
  sx.name = name;
  sx.busy = "add"; sx.msg = ""; renderSettings();
  try {
    let approval = "";
    if (sx.list.length) {
      const a = await sxCall("POST", "/api/approvals", { action: "passkey-add", subject: "Add a passkey" });
      approval = a.approval.id;
      const res = await sxAssert(approval);
      if (!res.approval || res.approval.state !== "approved") throw new Error("The existing passkey did not approve.");
    }
    const begin = await sxCall("POST", "/api/passkeys/register/begin", { approval });
    const o = begin.publicKey;
    // The salt only checks that PRF works: its output is dropped at once.
    const salt = await crypto.subtle.digest("SHA-256", new TextEncoder().encode("comebye-prf-register-check-v1"));
    let cred;
    try {
      cred = await navigator.credentials.create({ publicKey: {
        ...o, challenge: sxUnb64(o.challenge), user: { ...o.user, id: sxUnb64(o.user.id) },
        extensions: { prf: { eval: { first: salt } } },
      } });
    } catch (e) { throw new Error(sxWebAuthnMsg(e)); }
    if (!cred) throw new Error("No passkey was made.");
    const ext = cred.getClientExtensionResults ? cred.getClientExtensionResults() : {};
    let enabled = !!(ext.prf && ext.prf.enabled), result = !!(ext.prf && ext.prf.results && ext.prf.results.first);
    if (enabled && !result) { // one assertion with PRF, as the plan says
      try {
        const g = await navigator.credentials.get({ publicKey: {
          challenge: crypto.getRandomValues(new Uint8Array(32)), rpId: o.rp.id, userVerification: "required", timeout: 60000,
          allowCredentials: [{ type: "public-key", id: cred.rawId }], extensions: { prf: { eval: { first: salt } } },
        } });
        const ge = g && g.getClientExtensionResults ? g.getClientExtensionResults() : {};
        result = !!(ge.prf && ge.prf.results && ge.prf.results.first);
      } catch (e) { throw new Error(sxWebAuthnMsg(e)); }
    }
    if (!enabled || !result) {
      // Nothing is sent: the hub never hears of a passkey that cannot do PRF.
      throw new Error(SX_NO_PRF + " Then delete the passkey that was just made in the iOS Passwords app.");
    }
    const fin = await sxCall("POST", "/api/passkeys/register/finish", {
      challenge: o.challenge, id: sxB64(cred.rawId), client_data_json: sxB64(cred.response.clientDataJSON), attestation_object: sxB64(cred.response.attestationObject),
      prf: { enabled, result }, name,
    });
    sx.name = "";
    sx.busy = "";
    await sxLoad();
    sxSay(`Added ${fin.passkey.name}.`, false);
  } catch (e) {
    sx.busy = "";
    sxSay(e.message || "Could not add the passkey.", true);
  }
}

async function sxRenameSave() {
  const input = $("sx-rename");
  if (!input || sx.busy) return;
  sx.busy = "rename";
  try {
    await sxCall("PUT", `/api/passkeys/${encodeURIComponent(sx.rename)}`, { name: input.value });
    sx.rename = ""; sx.busy = "";
    await sxLoad();
    sxSay("Renamed.", false);
  } catch (e) { sx.busy = ""; sxSay(e.message, true); }
}

async function sxRemoveGo() {
  if (sx.busy || !sx.confirm) return;
  sx.busy = "remove";
  try {
    await sxCall("DELETE", `/api/passkeys/${encodeURIComponent(sx.confirm)}`);
    sx.confirm = ""; sx.busy = "";
    await sxLoad();
    sxSay("Removed. Delete it in the iOS Passwords app too.", false);
  } catch (e) { sx.busy = ""; sxSay(e.message, true); }
}

function settingsClick(ev) {
  const b = ev.target.closest("[data-sx]");
  if (!b) return;
  const id = b.closest("[data-id]") ? b.closest("[data-id]").dataset.id : "";
  switch (b.dataset.sx) {
    case "col": case "rs": case "sp": case "wrap": case "depth": case "font": case "openthink": case "opentools": sxPick(b.dataset.sx, b.dataset.val); break;
    case "theme": sxOpenPanel(b.dataset.sx); break;
    case "reset": sxReset(); break;
    case "rename": sx.rename = id; sx.confirm = ""; sx.msg = ""; renderSettings(); break;
    case "remove": sx.confirm = id; sx.rename = ""; sx.msg = ""; renderSettings(); break;
    case "cancel": sx.rename = ""; sx.confirm = ""; renderSettings(); break;
    case "remove-go": sxRemoveGo(); break;
  }
}

function settingsSubmit(ev) {
  const f = ev.target.closest("[data-sx-form]");
  if (!f) return;
  ev.preventDefault();
  if (f.dataset.sxForm === "add") sxAdd(); else if (f.dataset.sxForm === "rename") sxRenameSave();
}

function syncSettings() {
  const on = location.hash === "#/settings";
  const el = $("settings");
  const was = !el.hidden;
  sx.route = on;
  el.hidden = !on;
  if (!on) return;
  if (!was) {
    sx.status = "loading"; sx.msg = ""; sx.rename = ""; sx.confirm = ""; el._html = "";
    sxLoad();
    ($("sx-back").offsetParent ? $("sx-back") : $("sx-body")).focus({ preventScroll: true });
  }
  renderSettings();
}

function closeSettings() {
  history.replaceState(null, "", location.pathname + location.search);
  route();
}

function initSettings() {
  $("sx-back").addEventListener("click", closeSettings);
  $("settings").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape" && !(ev.target.closest && ev.target.closest("input"))) { ev.stopPropagation(); closeSettings(); }
    else if (ev.key === "Escape") { ev.stopPropagation(); if (sx.rename) { sx.rename = ""; renderSettings(); } }
  });
  $("sx-view").addEventListener("click", settingsClick);
  $("sx-view").addEventListener("keydown", sxColKey);
  // The strip is drawn for this window, and another tab can change the width: redraw the open screen.
  window.addEventListener("resize", () => { if (sx.route) renderSettings(); });
  window.addEventListener("storage", (e) => { if (sx.route && (e.key === CT.KEY_COL || e.key === CT.KEY_RS || e.key === CT.KEY_SP || e.key === CT.KEY_WRAP || e.key === CT.KEY_OPEN || e.key === CT.KEY_DEPTH || e.key === CT.KEY_FONT)) renderSettings(); });
  $("sx-view").addEventListener("submit", settingsSubmit);
}

/* ---------- approval items in To do ---------- */
//
// A to-do of kind "approval" waits for the user's passkey. Slice 1 shows what
// is asked and the Approve with Face ID button, which answers the hub's
// challenge. (What an unlock does with it comes later: the item closes when the
// hub accepts the assertion.)

const apBusy = new Set();

function approvalBarHtml(t) {
  const a = t.approval;
  if (guIs(a)) return guBarHtml(t);
  const left = Date.parse(a.expires_at) - Date.now();
  const line = a.state !== "pending" ? `This request is ${esc(a.state)}.` : left > 0 ? `Waiting for your passkey (Face ID). The request lasts until ${esc(new Date(a.expires_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }))}.` : "This request expired. Ask again from the host.";
  const can = a.state === "pending" && left > 0 && !apBusy.has(a.id);
  return `<div class="td-form ap-card" data-ap="${escAttr(a.id)}"><div class="sx-note" role="status">${line}</div>
    <div class="td-acts"><button type="button" class="ap-go td-answer-btn"${can ? "" : " disabled"}>${apBusy.has(a.id) ? "Waiting for Face ID…" : "Approve with Face ID"}</button><button type="button" class="ap-no">Not now</button></div></div>`;
}

async function approvalGo(id, item) {
  const bar = item.querySelector("[data-ap]");
  const apid = bar && bar.dataset.ap;
  if (!apid || apBusy.has(apid)) return;
  const ti = todos.items.find((x) => x.approval && x.approval.id === apid);
  if (ti && guIs(ti.approval)) { guGo(apid, item, ti.approval); return; }
  apBusy.add(apid);
  renderTodos(true);
  try {
    if (!sx.csrf) await sxLoad();
    await sxAssert(apid);
    toast("Approved");
  } catch (e) {
    toast(e.message || "Not approved", true);
  } finally {
    apBusy.delete(apid);
    await loadTodos();
  }
}

/* ---------- GPG unlock (issue #3, slice 3) ---------- */
//
// The items "GPG is locked on HOST" (action gpg-unlock) and "Set up GPG unlock
// on HOST" (gpg-setup). The host made a signed request; this page checks what
// it can of it (unlockcrypto.js), asks the passkey for Face ID with a PRF salt
// that names the host and its keys, derives K from the PRF output, seals K to
// the request's one-use key and posts the assertion and the sealed K. At setup
// it also encrypts the typed passphrase under K (the blob) and posts that. The
// hub relays everything to the host, which checks it again itself and unlocks.
//
// Secrets: the passphrase sits in its input until Face ID has answered, then
// is read once and the input is emptied; the PRF output and K are typed arrays
// that are zeroed as soon as they are used. Nothing is put in storage, in a
// draft or in the DOM, and the hub is never sent any of them in the clear.
// Apple Passwords only, PRF required: with no PRF result nothing is sent.

const GU_ACTIONS = new Set(["gpg-unlock", "gpg-setup"]);
const GU_LIFE = "8 hours"; // fixed on the host; the card says so and has no picker
const guIs = (a) => !!a && GU_ACTIONS.has(a.action);
const guItems = new Set(); // to-do ids whose Face ID this page is answering now
const gu = new Map(); // approval id -> { st: idle|loading|ready|err, d, info, err, ok, msg }

function guState(id) {
  let g = gu.get(id);
  if (!g) { g = { st: "idle", ok: false, msg: "", err: "" }; gu.set(id, g); }
  return g;
}

async function guLoad(id) {
  const g = guState(id);
  if (g.st !== "idle") return;
  g.st = "loading";
  try {
    const d = await api(`/api/approvals/${encodeURIComponent(id)}`);
    g.d = d;
    g.info = await CBUnlock.checkRequest(d.approval.request, d.challenge, d.approval.host, d.rp_id, Math.floor(Date.now() / 1000));
    g.st = "ready";
  } catch (e) { g.st = "err"; g.err = e.message || "The request could not be read."; }
  for (const k of [...gu.keys()]) if (!todos.items.some((t) => t.approval && t.approval.id === k)) gu.delete(k);
  renderTodos();
}

// guHostMsg words what a host refused, for the user and not a developer.
function guHostMsg(m, host) {
  m = String(m || "");
  if (/does not unlock the key/.test(m)) return "That passphrase is not the key's, so nothing was stored or unlocked. Run `comebye gpg unlock` on " + host + " and try again.";
  if (/not one this host trusts/.test(m)) return host + " does not trust that passkey. Run `comebye gpg setup` there to pin it.";
  if (/unknown, already used|expired/.test(m)) return "This request is used up or has expired. Run `comebye gpg unlock` on " + host + " for a new one.";
  if (/does not open the stored passphrase|cannot be read/.test(m)) return "This passkey is not the one the passphrase was stored with. Use that passkey, or run `comebye gpg reset` on " + host + " and set up again.";
  if (/another address than this hub/.test(m)) return "The passkey was used on another address than the hub's. Open the app at the hub's own address and try again.";
  if (/no passphrase is stored/.test(m)) return "No passphrase is stored on " + host + " yet. Run `comebye gpg setup` there.";
  return m;
}

function guErrMsg(e, host) {
  if (e && e.code === "host_refused") return guHostMsg(e.message, host);
  if (e && (e.name === "NotAllowedError" || e.name === "AbortError" || e.name === "InvalidStateError")) return sxWebAuthnMsg(e);
  return (e && e.message) || "Not unlocked.";
}

// guRich escapes a message and shows its `commands` as code.
const guRich = (m) => esc(m).replace(/`([^`]+)`/g, "<code>$1</code>");

function guBarHtml(t) {
  const a = t.approval, g = guState(a.id), setup = a.action === "gpg-setup";
  const left = Date.parse(a.expires_at) - Date.now();
  const pending = a.state === "pending" && left > 0;
  if (pending) guLoad(a.id);
  const busy = apBusy.has(a.id);
  const at = (iso) => esc(new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
  const host = esc(a.host);
  let h = `<div class="td-form ap-card gu-card" data-ap="${escAttr(a.id)}" data-gu="${setup ? "setup" : "unlock"}">`;
  h += `<p class="sx-note gu-life">${setup ? `Stores the passphrase of the GPG key on <b>${host}</b> sealed under your passkey, then unlocks it.` : `Unlocks the GPG key on <b>${host}</b>.`} The unlock lasts <b>${GU_LIFE}</b>, then it locks again.</p>`;
  if (a.state === "denied" && a.note) {
    h += `<p class="sx-msg err" role="alert">Not unlocked. ${guRich(guHostMsg(a.note, a.host))}</p>
      <div class="td-acts"><button type="button" class="ap-no">Dismiss</button></div></div>`;
    return h;
  }
  if (!pending) {
    h += a.action === "gpg-unlock" && (a.state === "expired" || a.state === "pending")
      ? `<p class="sx-note" role="status">That request ran out. ${host} sends a fresh one within a minute and this item keeps its place; if nothing comes, run <code>comebye gpg unlock</code> there.</p>
      <div class="td-acts"><button type="button" class="ap-no">Not now</button></div></div>`
      : `<p class="sx-note" role="status">This request is ${esc(a.state === "pending" ? "expired" : a.state)}. Run <code>comebye gpg unlock</code> on ${host} for a new one.</p>
      <div class="td-acts"><button type="button" class="ap-no">Dismiss</button></div></div>`;
    return h;
  }
  if (g.st === "err") {
    h += `<p class="sx-msg err" role="alert">${guRich(g.err)}</p><div class="td-acts"><button type="button" class="ap-no">Dismiss</button></div></div>`;
    return h;
  }
  if (setup && g.st === "ready") {
    h += `<div class="gu-fp"><span class="sx-note">Host key fingerprint (from the request)</span><code class="sx-fp gu-fpv">${esc(g.info.fingerprint)}</code></div>
      <label class="gu-check"><input type="checkbox" class="ap-confirm"${g.ok ? " checked" : ""}> <span>It matches what <code>comebye gpg setup</code> printed on ${host}</span></label>
      <label class="sx-note" for="gu-pass-${escAttr(a.id)}">Passphrase of the GPG key</label>
      <input type="password" class="ap-pass" id="gu-pass-${escAttr(a.id)}" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" enterkeyhint="go" maxlength="1024" aria-describedby="gu-pn-${escAttr(a.id)}">
      <p class="sx-note" id="gu-pn-${escAttr(a.id)}">Typed once. It is encrypted here under a key only your passkey can make; neither this app nor the hub keeps it.</p>`;
  }
  h += `<div class="sx-note" role="status">${g.st === "loading" || g.st === "idle" ? "Reading the request…" : `Waiting for your passkey (Face ID). The request lasts until ${at(a.expires_at)}.`}</div>`;
  if (g.msg) h += `<p class="sx-msg err" role="alert">${guRich(g.msg)}</p>`;
  h += `<div class="td-acts"><button type="button" class="ap-go td-answer-btn"${g.st === "ready" && !busy ? "" : " disabled"}>${busy ? "Waiting for Face ID…" : setup ? "Set up with Face ID" : "Unlock with Face ID"}</button><button type="button" class="ap-no">Not now</button></div></div>`;
  return h;
}

// guRepaint redraws the list and puts back what is typed in the passphrase
// box (a redraw makes a new one): the secret moves from the old input to the
// new, never anywhere else.
function guRepaint(apid) {
  const sel = `.gu-card[data-ap="${CSS.escape(apid)}"] .ap-pass`;
  const typed = [...document.querySelectorAll(sel)].map((e) => e.value).find(Boolean) || "";
  renderTodos(true);
  if (typed) for (const e of document.querySelectorAll(sel)) e.value = typed;
}

async function guGo(apid, item, a) {
  const g = guState(apid);
  if (apBusy.has(apid)) return;
  const setup = a.action === "gpg-setup";
  const say = (m) => { g.msg = m; guRepaint(apid); };
  if (g.st !== "ready") { say(g.st === "err" ? g.err : "Still reading the request: try again in a moment."); return; }
  const box = item.querySelector(".ap-pass");
  if (setup) {
    if (!g.ok) { say("Compare the fingerprint with the terminal first, then tick the box."); return; }
    if (!box || !box.value) { say("Type the passphrase of the GPG key."); return; }
  }
  if (!sxSupported()) { say("This browser cannot use a passkey here: it needs a secure (https) address."); return; }
  const btn = item.querySelector(".ap-go");
  apBusy.add(apid);
  guItems.add(item.dataset.id);
  g.msg = "";
  if (btn) { btn.disabled = true; btn.textContent = "Waiting for Face ID…"; } // in place: a redraw would lose the typed passphrase
  let prf = null, k = null, pass = null;
  try {
    const { d, info } = g;
    if (!sx.csrf) await sxLoad();
    const salt = await CBUnlock.prfSalt(info.host, info.keygrips);
    let cred;
    try {
      cred = await navigator.credentials.get({ publicKey: {
        challenge: CBUnlock.unb64(d.challenge), rpId: d.rp_id, userVerification: "required", timeout: 60000,
        allowCredentials: (d.credentials || []).map((c) => ({ type: "public-key", id: CBUnlock.unb64(c.id) })),
        extensions: { prf: { eval: { first: salt } } },
      } });
    } catch (e) { throw new Error(sxWebAuthnMsg(e)); }
    if (!cred) throw new Error("No passkey answered.");
    const ext = cred.getClientExtensionResults ? cred.getClientExtensionResults() : {};
    const first = ext.prf && ext.prf.results && ext.prf.results.first;
    if (!first) throw new Error(SX_NO_PRF + " Nothing was sent.");
    prf = new Uint8Array(first);
    k = await CBUnlock.deriveK(prf);
    CBUnlock.wipe(prf);
    const body = {
      credential_id: sxB64(cred.rawId), client_data_json: sxB64(cred.response.clientDataJSON),
      authenticator_data: sxB64(cred.response.authenticatorData), signature: sxB64(cred.response.signature),
      sealed: await CBUnlock.seal(info.curve, info.pub, info.challenge, k),
    };
    if (setup) {
      pass = new TextEncoder().encode(box.value);
      box.value = "";
      body.blob = await CBUnlock.encryptBlob(k, info.host, info.keygrips, pass);
    }
    const res = await sxCall("POST", `/api/approvals/${encodeURIComponent(apid)}/respond`, body);
    const until = res.unlocked_until ? new Date(res.unlocked_until).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";
    toast(until ? `Unlocked until ${until}` : "Unlocked");
    gu.delete(apid);
  } catch (e) {
    if (window.__guDebug) console.log("guGo failed:", e && e.name, e && e.message);
    g.msg = guErrMsg(e, a.host);
  } finally {
    CBUnlock.wipe(prf); CBUnlock.wipe(k); CBUnlock.wipe(pass);
    prf = k = pass = null; // the typed passphrase was emptied from its input when read; a cancelled Face ID keeps what was typed
    await loadTodos(); // still busy: the item this answer just closed is not "closed elsewhere"
    guItems.delete(item.dataset.id);
    apBusy.delete(apid);
    guRepaint(apid);
  }
}

/* ---------- Usage chips: a handoff's tokens, on its agent and in Status ---------- */
//
// One query (GET /api/usage, the last 14 days, the top 100 handoffs) gives every
// handoff its fresh tokens and cache hit, the same numbers as the Usage screen's
// drill-down row for a 14-day range; a chip links to that row
// (#/usage?p=<project>&h=<handoff>). An agent shows its handoff's chip. In the
// agent list the row is itself a button, so the chip there is only a label; the
// agent's detail and the Status page carry the link. The cache chip turns
// yellow below 80%. Nothing shows where the hub counts no tokens.

const uchip = { rows: new Map(), at: 0, inflight: false, off: false };

async function loadUsageChips() {
  if (uchip.inflight || uchip.off || ledger.on !== true || Date.now() - uchip.at < 60000) return;
  uchip.inflight = true;
  try {
    const d = await api("/api/usage?" + new URLSearchParams({ since: "14d", by: "kind", limit: "100", tz: String(-new Date().getTimezoneOffset()) }));
    uchip.rows = new Map((d.top_handoffs || []).map((h) => [h.handoff, h]));
  } catch (e) {
    if (e.status === 404 || e.status === 403) { uchip.off = true; uchip.rows = new Map(); }
  } finally {
    uchip.inflight = false;
    uchip.at = Date.now();
  }
  renderList();
  if (state.selected) renderDetail();
  for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = "";
  renderStatus();
}

// usageChipHtml is the chip of a ledger entry ("" without tokens); link: an <a> to its usage.
function usageChipHtml(entryId, link) {
  const r = uchip.rows.get(entryId);
  if (!r || !(r.fresh > 0)) return "";
  const hit = r.hit || 0;
  const cls = "us-chip" + (hit < 0.8 ? " low" : "");
  const text = `${usFmtN(r.fresh)} tokens · cache ${usPct(hit)}`;
  const title = `${r.fresh.toLocaleString()} fresh tokens over 14 days in ${r.sessions} ${r.sessions === 1 ? "session" : "sessions"}; ${usPct(hit)} of the prompt came from the cache${hit < 0.8 ? " (below 80%)" : ""}`;
  if (!link) return `<span class="${cls}" title="${escAttr(title)}">${esc(text)}</span>`;
  const q = new URLSearchParams({ p: r.project || "", h: entryId });
  return `<a class="${cls}" href="#/usage?${q}" title="${escAttr(title)}" aria-label="Usage of this handoff: ${escAttr(text)}">${esc(text)}</a>`;
}

function initUsage() {
  $("us-back").addEventListener("click", closeUsage);
  $("usage").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") { ev.stopPropagation(); closeUsage(); return; }
    const g = ev.target.closest && ev.target.closest("g[data-i]");
    if (g && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); us.picked = +g.dataset.i; usageChart(); const again = document.querySelector(`#us-chart g[data-i="${us.picked}"]`); if (again) again.focus(); }
  });
  $("us-view").addEventListener("click", usageClick);
  $("us-view").addEventListener("toggle", (ev) => { if (ev.target.id === "us-pricedet" && ev.target.open && !us.prices) usageLoadPrices(); else if (ev.target.id === "us-pricedet" && ev.target.open) renderUsagePrices(); }, true);
  // The charts are drawn to the width they have, and that is the pane's now, not the window's: the list collapsing or the
  // window resizing both change it.
  let t = null, w = 0;
  new ResizeObserver(() => {
    const nw = $("us-body").clientWidth;
    if (nw === w) return;
    w = nw;
    if (!us.route || us.status !== "ok") return;
    clearTimeout(t); t = setTimeout(renderUsage, 150);
  }).observe($("us-body"));
}

/* ---------- the page rows above the agent list (desktop) ---------- */
//
// One table; a row is a page (openPage) or an action (a dialog).

const NAV_ICON = {
  status: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 6h16M4 12h10M4 18h13"/><circle cx="19" cy="12" r="1.5"/></svg>',
  todo: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="4" y="4" width="16" height="16" rx="3"/><path d="m8 12 3 3 5-6"/></svg>',
  search: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></svg>',
  pages: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 3h7l5 5v13H7z"/><path d="M14 3v5h5M10 13h6M10 17h6"/></svg>',
  usage: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 20V10M12 20V4M19 20v-7"/></svg>',
  settings: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3"/><path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1"/></svg>',
  peers: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/></svg>',
  removals: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/></svg>',
  archived: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="5" rx="1"/><path d="M5 9v10h14V9M10 13h4"/></svg>',
};
const NAV_ROWS = [
  { key: "status", label: "Status", show: () => ledger.on === true },
  { key: "todo", label: "To do", show: () => todos.on === true, count: () => todos.open },
  { key: "search", label: "Search", show: () => true, open: () => openRecall(), on: () => !!rc.route },
  { key: "pages", label: "Pages", show: () => ledger.on === true, open: () => { location.hash = "#/pages"; }, on: () => !!pgs.route }, // a screen of its own, like Transcripts
  { key: "usage", label: "Usage", show: () => ledger.on === true, open: () => { location.hash = "#/usage"; }, on: () => !!us.route }, // tokens spent, by kind, model, project and handoff
  { key: "peers", label: "Peers", show: () => ledger.on === true && multiHost(), open: () => { location.hash = "#/peers"; }, on: () => !!prs.route, odot: () => relNeedsYou() }, // the hosts and what each starts by default
  { key: "settings", label: "Settings", show: () => ledger.on === true, open: () => { location.hash = "#/settings"; }, on: () => sx.route }, // the passkeys
  // Hidden at zero, except while you are on it.
  { key: "removals", label: "Ready to remove", show: () => ledger.on === true && (ledger.marked.size > 0 || review.open), count: () => ledger.marked.size, open: () => openReview(), on: () => review.open },
  // Finished work put away: no badge (nothing there waits on you).
  { key: "archived", label: "Archived", show: () => ledger.on === true && (archCount() > 0 || arch.route), open: () => { location.hash = ARCHIVED_HASH; }, on: () => arch.route },
];

function renderNav() {
  const el = $("nav");
  if (!el) return;
  const rows = NAV_ROWS.filter((r) => r.show());
  const html = rows.map((r) => {
    const n = r.count ? r.count() : 0;
    const on = r.on ? r.on() : state.view === r.key;
    const tip = n ? `${r.label}, ${n} open` : r.label;
    return `<button type="button" class="nv${on ? " on" : ""}" data-page="${r.key}" title="${escAttr(r.label)}" aria-label="${escAttr(tip)}"${on ? ' aria-current="page"' : ""}>${NAV_ICON[r.key]}${
      n ? `<span class="badge" aria-hidden="true">${n}</span>` : r.odot && r.odot() ? `<span class="bdot bdot-o" role="img" aria-label="a release needs you: ${escAttr((rls.data && rls.data.needs_text) || "")}"></span>` : r.dot && r.dot() ? '<span class="bdot" aria-label="new"></span>' : ""}</button>`;
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
    syncScreens(); // a screen that follows the hash gives way
  }
  state.selected = id;
  state.chat = null;
  clearTimeout(chatRetryTimer);
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
  if (window.SP) window.SP.onAgent(id);
}

function closeDetail() {
  state.stoppedOpen = false;
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
  clearTimeout(chatRetryTimer);
  state.ask = null;
  state.pushedRoute = false;
  if (window.SP) window.SP.onAgent(null);
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

// A chat helper's conversation starts with a strip: what it is about, and the way back to the Page.
function renderChatStrip(a) {
  const el = $("d-chatstrip");
  if (!el) return;
  const c = a && a.chat && /^pg_[0-9a-f]{10}$/.test(a.chat.page || "") ? a.chat : null;
  if (!c) { el.hidden = true; el.innerHTML = ""; return; }
  const title = c.title || "this Page";
  const html = `<span class="cs-ico" aria-hidden="true"></span><span class="cs-t">Chat about <b>${esc(title)}</b></span>${c.writable ? '<span class="chip cp-rwchip" title="Started with the read-only check overridden: this helper can change files and run commands.">not read-only</span>' : ""}<span class="cs-sep" aria-hidden="true">·</span><a class="cs-open" href="#/pages/${escAttr(c.page)}">Open page</a>`;
  if (el.innerHTML !== html) el.innerHTML = html;
  el.hidden = false;
}

function renderDetailNote() {
  const el = $("d-note"), a = agentById(state.selected);
  if (!el) return;
  el.innerHTML = a ? noteHtml(a) : "";
  el.hidden = !el.innerHTML;
}

// A session on a worker that is stopped: not listed (a host that is down lists nothing), but
// opening it is a read, not a start: the header, a note, the Start banner and the composer.
function stoppedSession(id) {
  const h = hostOf(id), st = h && hostStatus(h);
  return !!id && !agentById(id) && !!st && !st.local && peerStatusOf(st) !== "ok" && (!!wakeOf(st) || (!ph.data && ph.asking)); // (until the provisioners are heard from, assume it)
}

function openStoppedSession(host, pane) {
  setView(null);
  resetTerminalView();
  const hash = "#/agent/" + encodeURIComponent(host) + "/" + encodeURIComponent(pane);
  if (location.hash !== hash) {
    history.pushState(null, "", hash);
    state.pushedRoute = true;
    syncScreens();
  }
  state.selected = host + "/" + pane;
  state.stoppedOpen = true;
  state.chat = null;
  clearTimeout(chatRetryTimer);
  state.ask = null;
  state.lastStatus = null;
  $("list").style.display = "none";
  $("detail").style.display = "flex";
  document.body.classList.add("has-detail");
  markSelectedRow();
  renderDetail();
  loadChat();
}

function renderStoppedDetail() {
  const id = state.selected, host = hostOf(id);
  state.stoppedOpen = true;
  stopPolling(); // nothing to poll while the host is off; openDetail starts it again when the session is listed
  $("d-title").textContent = state.selName || id.slice(host.length + 1);
  $("d-subtitle").textContent = `${host} · stopped`;
  // Not a live status: the host is off, so the chip says so, not what the agent last did.
  const ds = $("d-status");
  ds.textContent = "stopped";
  ds.title = "The host is off: this session is not running";
  ds.className = "status stopped";
  $("d-usage").innerHTML = ""; $("d-usage").hidden = true;
  renderWakeBanner();
  loadChat();
}

function renderDetail() {
  renderWakeBanner();
  const a = agentById(state.selected);
  if (!a) {
    if (stoppedSession(state.selected)) { renderStoppedDetail(); return; }
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
  const ue = ledgerEntry(a), uc = $("d-usage");
  uc.innerHTML = ue ? usageChipHtml(ue.id, true) : "";
  uc.hidden = !uc.innerHTML;
  const st = $("d-status");
  st.textContent = bgRunning(a) ? "running" + (bgSinceMs(a) ? " · " + fmtAgo(bgSinceMs(a)) : "") : a.agent_status;
  st.className = "status " + agentStatusCls(a);
  st.title = bgRunning(a) ? bgTitle(a) : "";
  renderCtxChip();
  const why = $("d-why"), wt = whyBlocked(a) || (quietMs(a) ? quietText(a) : "") || noteOf(a) || (bgRunning(a) ? bgWhy(a) : "");
  why.classList.toggle("quiet", !whyBlocked(a) && !quietMs(a));
  why.classList.toggle("stuck", !whyBlocked(a) && !!quietMs(a));
  why.textContent = wt;
  why.title = quietMs(a) && !whyBlocked(a) ? quietTitle(a) : bgRunning(a) && !whyBlocked(a) && !noteOf(a) ? bgTitle(a) : wt;
  why.hidden = !wt;
  renderDetailNote();
  renderChatStrip(a);
  updateAnswerCard();
  updateSendButton();
  renderActivity();
  renderDetailMenu();
  if (sw.open) renderSwitch(); // the agent may have gone idle (or busy) meanwhile
  refreshQueued();
}

/* ---------- context usage chip ---------- */
//
// The transcript response carries how full the agent's context window is
// (the input plus cache tokens of its latest reply, and the model's window
// when it is known). The chip rides on the chat's own poll, so it costs no
// request of its own. Nothing known, nothing shown.

const CTX_WARN = 0.8, CTX_DANGER = 0.95;

function fmtCtxTokens(n) {
  if (n >= 1e6) return (n / 1e6).toFixed(2).replace(/\.?0+$/, "") + "M";
  return n >= 1000 ? Math.round(n / 1000) + "k" : String(n);
}

function ctxLevel(c) {
  if (!c || !c.window) return "";
  const f = c.tokens / c.window;
  return f >= CTX_DANGER ? "red" : f >= CTX_WARN ? "yellow" : "muted";
}

// A short, readable model name for the header chip. Claude ids become
// "Opus 5.5"; anything else keeps its last path segment minus a trailing
// date or build stamp ("enmaas/rits/deepseek-ai/DeepSeek-V4.1-Flash-1017"
// is "DeepSeek-V4.1-Flash"). The full provider/model stays in the tooltip.
function shortModel(id) {
  const raw = String(id || "").trim();
  if (!raw) return "";
  const last = raw.split("/").filter(Boolean).pop() || raw;
  const cap = (w) => w.charAt(0).toUpperCase() + w.slice(1);
  let m = /^claude-(opus|sonnet|haiku|fable)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?(?:\[[^\]]*\])?$/i.exec(last);
  if (m) return cap(m[1].toLowerCase()) + " " + m[2] + (m[3] ? "." + m[3] : "");
  m = /^claude-(\d+)(?:-(\d{1,2}))?-(opus|sonnet|haiku)(?:-\d{8})?$/i.exec(last);
  if (m) return cap(m[3].toLowerCase()) + " " + m[1] + (m[2] ? "." + m[2] : "");
  const t = last.replace(/-(?:\d{8}|\d{4})$/, "");
  return t || last;
}

// What the transcript reports it is using, else the model the agent was
// launched with (the ledger entry of a handed-off worker).
function agentModel() {
  const c = state.chat && state.chat.ctx;
  if (c && c.model) return { model: c.model, provider: c.provider || "" };
  const a = state.selected && agentById(state.selected);
  const e = a && ledgerEntry(a);
  if (e && e.model) return { model: e.model, provider: "" };
  return null;
}

function modelFull(m) { return (m.provider && !m.model.startsWith(m.provider + "/") ? m.provider + "/" : "") + m.model; }

function ctxDetail(c) {
  const m = agentModel();
  const parts = [];
  if (c && c.tokens > 0) parts.push(c.window
    ? `Context: ${c.tokens.toLocaleString()} of ${c.window.toLocaleString()} tokens (${Math.round((c.tokens / c.window) * 100)}%)`
    : `Context: ${c.tokens.toLocaleString()} tokens (the model's window is not known)`);
  if (m) parts.push("Model: " + modelFull(m));
  const comp = c && c.compacted ? Date.parse(c.compacted) : NaN;
  if (!isNaN(comp)) parts.push(`compacted ${fmtAgo(comp)} ago (${new Date(comp).toLocaleString()})`);
  return parts.join(" · ");
}

function renderCtxChip() {
  const el = $("d-ctx"), me = $("d-modelchip"), sm = $("d-submodel");
  const c = state.chat && state.chat.ctx;
  const m = agentModel();
  const detail = ctxDetail(c);
  if (m) {
    me.textContent = shortModel(m.model);
    me.title = detail;
    me.setAttribute("aria-label", detail);
    me.hidden = false;
    sm.textContent = me.textContent; sm.title = detail; sm.hidden = false;
  } else { me.hidden = true; me.textContent = ""; sm.hidden = true; sm.textContent = ""; }
  if (!c || !(c.tokens > 0)) { el.hidden = true; el.textContent = ""; el.dataset.level = ""; return; }
  const lvl = ctxLevel(c);
  el.className = "chip d-ctx " + (lvl || "muted");
  el.dataset.level = lvl;
  const tok = fmtCtxTokens(c.tokens);
  el.innerHTML = c.window
    ? `ctx ${Math.round((c.tokens / c.window) * 100)}%<span class="ctx-tok"> · ${tok}</span>`
    : `ctx ${tok}`;
  el.title = detail;
  el.setAttribute("aria-label", detail);
  el.hidden = false;
}

for (const id of ["d-ctx", "d-modelchip", "d-submodel"]) {
  $(id).addEventListener("click", () => {
    const d = ctxDetail(state.chat && state.chat.ctx);
    if (d) toast(d);
  });
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
  if (stoppedSession(pane)) { // nothing to read: the host is off
    state.chat = { rendered: new Set(), skip: 0, more: false, terminal: false, pending: state.chat && state.chat.pending || [], from: "", running: [], ctx: null };
    $("chat").innerHTML = `<div class="wk-gone"><p><strong>${esc(hostOf(pane))} is stopped.</strong> This session's messages show again once it is up.</p></div>`;
    return;
  }
  if (reset) {
    state.chat = { rendered: new Set(), skip: 0, more: false, terminal: false, pending: [], from: "", running: [], ctx: null };
    $("chat").innerHTML = "";
    renderCtxChip();
  }
  const chat = state.chat;
  try {
    // Polls ask only for what can have changed (?from=, see renderTranscript),
    // and the browser revalidates by ETag, so an unchanged transcript costs
    // a bodiless 304. The newest window is always skip=0: "Load earlier"
    // pages are fetched separately.
    const from = !reset && chat.from ? `&from=${encodeURIComponent(chat.from)}` : "";
    const a0 = agentById(pane);
    const doneAtStart = a0 && a0.agent_status === "done" ? a0.status_since || "" : null; // the final message cannot be older than this read
    const t = await api(`${pathFor(pane)}/transcript?limit=100${from}`);
    if (state.selected !== pane || !state.chat) return;
    renderTranscript(t, reset);
    syncTermButton();
    if (doneAtStart !== null) markSeen(pane, doneAtStart);
  } catch (e) {
    if (e.status === 404) {
      // No Pi session (or it is gone): fall back to raw pane output.
      chat.terminal = true;
      chat.auto = true; // the automatic fallback: the transcript is retried until it exists
      chat.ctx = null;
      renderCtxChip();
      showTerminal();
      refreshOutput();
      scheduleChatRetry();
    } else {
      toast("transcript failed: " + e.message, true);
    }
  }
}

// One button flips between the views: "Show raw terminal" over a chat, "Show
// chat" over a raw view chosen by hand. Hidden in the automatic fallback
// (there is no transcript to go back to).
const TERM_ICON = {
  term: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="m7 9 3 3-3 3M13 15h4"/></svg>',
  chat: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5.5A2.5 2.5 0 0 1 6.5 3h11A2.5 2.5 0 0 1 20 5.5v8a2.5 2.5 0 0 1-2.5 2.5H11l-5 4v-4h-.5A1.5 1.5 0 0 1 4 14.5z"/></svg>',
};
function syncTermButton() {
  const b = $("btn-terminal"), chat = state.chat;
  if (!chat || chat.auto) { b.hidden = true; return; }
  const label = chat.terminal ? "Show chat" : "Show raw terminal";
  b.setAttribute("aria-label", label);
  b.title = label;
  const k = chat.terminal ? "chat" : "term";
  if (b.dataset.icon !== k) { b.innerHTML = TERM_ICON[k]; b.dataset.icon = k; }
  b.hidden = false;
}

// markSeen tells the hub the user has the finished agent's latest message on
// screen, so it shows idle instead of done (herdr's "done" clears only when
// its own TUI focuses the pane). Only from the visible tab, and only for a
// read that began after the agent was done; once per done spell.
const seenSent = new Map();
function markSeen(id, stamp) {
  if (document.hidden || state.selected !== id) return;
  const a = agentById(id);
  if (!a || a.agent_status !== "done" || (a.status_since || "") !== stamp) return;
  if (seenSent.get(id) === stamp) return;
  seenSent.set(id, stamp);
  fetch(`${agentPath(a)}/seen`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" })
    .catch(() => { if (seenSent.get(id) === stamp) seenSent.delete(id); });
}

function showTerminal() {
  $("chat").style.display = "none";
  // "Show chat" only where a transcript exists: a raw view chosen by hand over a loaded chat.
  syncTermButton();
  $("output").style.display = "block";
  $("btn-copy-screen").hidden = false;
}

// While a view sits in the automatic terminal fallback (no session yet, or an
// agent just restarted), ask for the transcript again: every 3 s for the first
// minute, then every 20 s, and at once when the agent's status changes. The
// first 200 switches to the chat without a reselect. A terminal the user chose
// by hand is never switched back.
let chatRetryTimer = null;
function scheduleChatRetry() {
  clearTimeout(chatRetryTimer);
  chatRetryTimer = null;
  const chat = state.chat;
  if (!state.selected || !chat || !chat.terminal || !chat.auto) return;
  if (!chat.retryFrom) chat.retryFrom = Date.now();
  const delay = Date.now() - chat.retryFrom < 60000 ? 3000 : 20000;
  chatRetryTimer = setTimeout(() => retryTranscript(), delay);
}

async function retryTranscript() {
  clearTimeout(chatRetryTimer);
  chatRetryTimer = null;
  const pane = state.selected;
  const chat = state.chat;
  if (!pane || !chat || !chat.terminal || !chat.auto) return;
  if (document.hidden) return scheduleChatRetry();
  try {
    const t = await api(`${pathFor(pane)}/transcript?limit=100`);
    if (state.selected !== pane || state.chat !== chat || !chat.terminal || !chat.auto) return;
    // A session exists now: back to the chat view.
    chat.terminal = false;
    chat.auto = false;
    if (outputAbort) outputAbort.abort();
    outputAbort = null;
    outputBusy = false;
    $("output").style.display = "none";
    $("output").textContent = "";
    $("btn-copy-screen").hidden = true;
    copySrc.delete("term-screen");
    $("chat").style.display = "";
    $("chat").innerHTML = "";
    chat.rendered = new Set();
    chat.from = "";
    chat.skip = 0;
    renderTranscript(t, true);
    syncTermButton();
    refreshStatusLine();
  } catch (e) {
    if (state.selected === pane && state.chat === chat) scheduleChatRetry();
  }
}

function renderTranscript(t, reset) {
  const chatEl = $("chat");
  const chat = state.chat;
  const atBottom = chatEl.scrollHeight - chatEl.scrollTop - chatEl.clientHeight < 80;
  const spot = reset ? null : readingSpot(chatEl); // the poll's own changes (a fold, a new marker) must not move it

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
      // A tool call may have gained its result since it was rendered.
      if (m.kind === "tool") patchToolCard(m);
      continue;
    }
    chat.rendered.add(m.id);
    appendMessage(chatEl, m, chat);
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
  chat.ctx = t.context || null;
  renderCtxChip();
  renderActivity();
  ensureSubagentTicker();

  chatEl.appendChild(jumpAnchor()); // keep the jump button last
  if (appended && (atBottom || reset)) chatEl.scrollTop = chatEl.scrollHeight;
  else if (spot) restoreReading(chatEl, spot);
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
  if (state.chat && !state.chat.terminal) syncLiveRuns(chatEl, working);
  let row = chatEl.querySelector(".activity");
  if (!working) { activity.seenAt = 0; activity.status = null; clearInterval(activity.timer); activity.timer = null; }
  if (!state.chat || state.chat.terminal || (!working && !subs)) {
    syncLiveThinking(chatEl, false, null);
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
  // While the "Thinking…" line shows above it, the row would only repeat it: it keeps the timer.
  row.classList.toggle("thought", syncLiveThinking(chatEl, working && step.verb === "Thinking", row));
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

// toolMeta is one call's status: its duration, "failed" for a result that is
// an error, or still running.
function toolMeta(m) {
  if (m.err) return "failed" + (m.ms ? " · " + fmtMs(m.ms) : "");
  return m.ms ? fmtMs(m.ms) : (m.result ? "ran" : "running…");
}

// toolRunning: a call still waiting for its result.
function toolRunning(m) { return !m.result && !m.err && !m.ms; }

// toolResultText is a call's output: empty while the call runs, so a result
// that arrives later is not mistaken for a final "(no output)".
function toolResultText(m) {
  return m.result || (toolRunning(m) ? "" : "(no output)");
}

// stillChanging: a message a later poll may update.
function stillChanging(m) {
  if (m.kind !== "tool") return false;
  return m.subagent ? m.subagent.status === "running" : toolRunning(m);
}

/* ---------- tool runs: consecutive tool calls as one quiet line ---------- */
//
// A run of tool calls is one grey line ("Ran 3 commands, edited 2 files ›").
// Tapping it opens a compact list; tapping a row opens that call's input and
// output. TOOL_KINDS maps a tool's name (lower-cased) to what it does, for
// Claude's names (Bash, Read, Edit, Grep, WebFetch, mcp__server__tool…), Pi's
// (bash, read, edit, find, ls…) and opencode's; any other tool is "other".
const TOOL_KINDS = {
  bash: "command", bashoutput: "command", killshell: "command", killbash: "command", shell: "command", exec: "command", run: "command",
  read: "read", notebookread: "read", view: "read", cat: "read",
  edit: "edit", multiedit: "edit", notebookedit: "edit", patch: "edit", apply_patch: "edit", str_replace: "edit",
  write: "write", create: "write",
  grep: "search", glob: "search", find: "search", ls: "search", list: "search", search: "search", toolsearch: "search",
  websearch: "websearch", web_search: "websearch",
  webfetch: "webfetch", web_fetch: "webfetch", fetch: "webfetch",
  task: "agent", agent: "agent",
  todowrite: "plan", todoread: "plan", update_plan: "plan", exitplanmode: "plan",
};
function toolKind(name) {
  const n = String(name || "").toLowerCase();
  if (n.startsWith("mcp__") || n.startsWith("mcp_")) return "mcp";
  return TOOL_KINDS[n] || "other";
}
// kind -> [phrase for one, phrase for many ("%n" is the count), row verb, row verb while running].
const TOOL_PHRASES = {
  command: ["ran a command", "ran %n commands", "Ran", "Running"],
  read: ["read a file", "read %n files", "Read", "Reading"],
  edit: ["edited a file", "edited %n files", "Edited", "Editing"],
  write: ["wrote a file", "wrote %n files", "Wrote", "Writing"],
  search: ["ran a search", "ran %n searches", "Searched", "Searching"],
  websearch: ["searched the web", "searched the web %n times", "Searched the web", "Searching the web"],
  webfetch: ["fetched a page", "fetched %n pages", "Fetched", "Fetching"],
  agent: ["ran a subagent", "ran %n subagents", "Ran subagent", "Running subagent"],
  plan: ["updated the plan", "updated the plan %n times", "Updated plan", "Updating plan"],
  mcp: ["called an MCP tool", "called %n MCP tools", "Called", "Calling"],
  other: ["used a tool", "used %n tools", "Used", "Using"],
};
// toolRowVerb: the word in front of a call in the list.
function toolRowVerb(m) {
  const p = TOOL_PHRASES[toolKind(m.tool)];
  return toolRunning(m) ? p[3] : p[2];
}
// toolRunSummary describes a run of calls (the messages, in order): the
// phrase, how many failed, whether one is still running, and the first line
// of the last call's output when that call failed.
function toolRunSummary(calls) {
  const counts = new Map();
  let failed = 0, running = 0;
  for (const m of calls) {
    const k = toolKind(m.tool);
    counts.set(k, (counts.get(k) || 0) + 1);
    if (m.err) failed++;
    else if (toolRunning(m)) running++;
  }
  // When every call is of an unnamed kind, "used 4 tools" reads better than "used 4 other tools".
  const parts = [];
  for (const [k, n] of counts) parts.push(TOOL_PHRASES[k][n === 1 ? 0 : 1].replace("%n", n));
  let text = parts.join(", ");
  text = text.charAt(0).toUpperCase() + text.slice(1);
  const last = calls[calls.length - 1];
  let peek = "";
  if (last && last.err) {
    const line = String(last.result || "").split("\n").map((l) => l.trim()).find((l) => l) || "";
    peek = line || "failed";
  }
  return { text, failed, running, peek };
}

/* ---------- keyboard and screen readers for the lines ---------- */
//
// Every line (a run, a subagent, a thought) is a real <button> with aria-expanded and aria-controls. Up and Down
// move between the lines and the rows of open runs, Home and End to the first and last, Enter and Space are the
// button's own. Esc closes what is open and returns focus to its line; at a line that is shut it does nothing, so
// the app's own Esc (leaving the prompt, closing a sheet) is untouched. Letter keys (j, k, g, ...) are keys.js's
// and arrow keys outside these lines are not touched. Nothing here is a live region: a run changes every few
// seconds, so one polite status line says only that a call failed or a subagent finished, at most once a moment.

let trUid = 0;
const trId = (p) => p + "-" + (++trUid);

// toolStops: the focusable lines and rows of the chat, in reading order (those that are on screen).
function toolStops(chatEl) {
  return [...chatEl.querySelectorAll(".tr-line, .tr-row")].filter((b) => !b.closest("[hidden]") && !b.hidden);
}

function onToolKey(ev) {
  if (ev.altKey || ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.isComposing || ev.defaultPrevented) return;
  const chatEl = ev.currentTarget, t = ev.target;
  if (!t.closest) return;
  const k = ev.key;
  const stop = t.closest(".tr-line, .tr-row");
  if (stop && (k === "ArrowDown" || k === "ArrowUp" || k === "Home" || k === "End")) {
    const stops = toolStops(chatEl), i = stops.indexOf(stop);
    if (i < 0) return;
    const j = k === "Home" ? 0 : k === "End" ? stops.length - 1 : i + (k === "ArrowDown" ? 1 : -1);
    // At either end the key is the page's: it scrolls.
    if (j < 0 || j >= stops.length || j === i) return;
    ev.preventDefault();
    stops[j].focus();
    stops[j].scrollIntoView?.({ block: "nearest" });
    return;
  }
  if (k !== "Escape") return;
  // Esc: the innermost open thing around the focus closes, and the focus goes to its own line.
  const item = t.closest(".tr-item"), run = t.closest(".toolrun"), sub = t.closest(".subrun"), th = t.closest(".thinking");
  if (item && !item.querySelector(".tr-detail").hidden) {
    ev.preventDefault(); ev.stopPropagation();
    toggleToolRow(item, false);
    item.querySelector(".tr-row").focus();
  } else if (run && !run.querySelector(".tr-list").hidden) {
    ev.preventDefault(); ev.stopPropagation();
    run._manual = false;
    setRunOpen(run, false);
    run.querySelector(".tr-line").focus();
  } else if ((sub || th) && t.closest(".sa-body, .think-body, .tr-line")) {
    const root = sub || th, line = root.querySelector(".tr-line");
    if (line.getAttribute("aria-expanded") !== "true") return;
    ev.preventDefault(); ev.stopPropagation();
    line.click();
    line.focus();
  }
}

// The status line: calls that failed and subagents that finished, coalesced.
let toolSay = [], toolSayTimer = null;
function announceTools(text) {
  toolSay.push(text);
  if (toolSayTimer) return;
  toolSayTimer = setTimeout(() => {
    toolSayTimer = null;
    const live = $("tool-live");
    const t = toolSay.slice(-3).join(". ");
    toolSay = [];
    if (live) live.textContent = t;
  }, 1200);
}

function toggleToolRow(item, open) {
  const row = item.querySelector(".tr-row"), det = item.querySelector(".tr-detail");
  det.hidden = !open;
  row.setAttribute("aria-expanded", String(open));
}

function renderToolRun() {
  const d = document.createElement("div");
  d.className = "toolrun";
  d._calls = [];
  const lid = trId("trl"), pid = trId("trp");
  d.innerHTML =
    `<button type="button" class="tr-line" aria-expanded="false" aria-controls="${lid}"><span class="tr-dot" aria-hidden="true"></span>` +
    `<span class="tr-txt"></span><span class="tr-bad"></span><span class="tr-chev" aria-hidden="true">›</span></button>` +
    `<div class="tr-peek" id="${pid}" hidden></div><div class="tr-list" id="${lid}" role="group" aria-label="Tool calls" hidden><button type="button" class="tr-row tr-more" hidden></button></div>`;
  const line = d.querySelector(".tr-line"), list = d.querySelector(".tr-list");
  line.addEventListener("click", () => {
    // The user's own open or close is kept for this run and beats the live rule.
    d._manual = list.hidden;
    setRunOpen(d, list.hidden);
  });
  d.querySelector(".tr-more").addEventListener("click", () => { d._all = true; applyRunCap(d); });
  if (CT.openTools()) setRunOpen(d, true); // Open by default: the first state only; a click sets _manual
  return d;
}

// setRunOpen opens or closes a run's list.
function setRunOpen(run, open) {
  const list = run.querySelector(".tr-list"), line = run.querySelector(".tr-line");
  list.hidden = !open;
  line.setAttribute("aria-expanded", String(open));
}

// A run of more than TOOL_RUN_CAP calls lists only its newest TOOL_RUN_SHOW
// (the live tail is the point), behind a "Show N more" for the rest.
const TOOL_RUN_CAP = 25, TOOL_RUN_SHOW = 6;
function applyRunCap(run) {
  const items = run.querySelectorAll(".tr-item"), more = run.querySelector(".tr-more");
  const hide = !run._all && items.length > TOOL_RUN_CAP ? items.length - TOOL_RUN_SHOW : 0;
  items.forEach((it, i) => { it.hidden = i < hide; });
  more.hidden = !hide;
  more.textContent = hide ? "Show " + hide + " more" : "";
}

// isLiveRun: the run is the latest thing in the conversation and either a
// call in it is running or the agent is still working (nothing but the
// activity row, the queued messages and the jump button follow it).
function isLiveRun(run, working) {
  if (run._calls.some(toolRunning)) return true;
  if (!working) return false;
  for (let e = run.nextElementSibling; e; e = e.nextElementSibling) {
    if (e.classList.contains("activity") || e.classList.contains("live") || e.classList.contains("jump-anchor") || e.classList.contains("pending")) continue;
    return false;
  }
  return true;
}

// syncLiveRuns opens the live run and folds the ones that are not live any
// more, except where the user chose (run._manual). A fold keeps the reading
// position: it follows the bottom when the user is there, else holds the
// first message in view where it was.
function readingSpot(chatEl) {
  const top = chatEl.getBoundingClientRect().top;
  let anchor = null;
  // The first element that starts inside the view, so a change in the one cut by the top edge is
  // compensated for too; failing that, the first one in view.
  for (const el of chatEl.children) if (el.getBoundingClientRect().top >= top - 1) { anchor = el; break; }
  if (!anchor) for (const el of chatEl.children) if (el.getBoundingClientRect().bottom > top + 1) { anchor = el; break; }
  return { anchor, was: anchor ? anchor.getBoundingClientRect().top : 0 };
}
function restoreReading(chatEl, spot) {
  if (spot.anchor && spot.anchor.isConnected) chatEl.scrollTop += spot.anchor.getBoundingClientRect().top - spot.was;
}
function syncLiveRuns(chatEl, working) {
  const todo = [];
  for (const run of chatEl.querySelectorAll(":scope > .toolrun")) {
    const want = run._manual !== undefined ? run._manual : CT.openTools() || isLiveRun(run, working);
    if (want !== !run.querySelector(".tr-list").hidden) todo.push([run, want]);
  }
  if (!todo.length) return;
  const following = chatEl.scrollHeight - chatEl.scrollTop - chatEl.clientHeight < 80;
  const spot = readingSpot(chatEl);
  for (const [run, want] of todo) setRunOpen(run, want);
  if (following) chatEl.scrollTop = chatEl.scrollHeight;
  else restoreReading(chatEl, spot);
  updateJump();
}

function toolRowEl(m) {
  const item = document.createElement("div");
  item.className = "tr-item";
  item.dataset.id = m.id;
  const did = trId("trd");
  item.innerHTML =
    `<button type="button" class="tr-row" aria-expanded="false" aria-controls="${did}"><span class="tr-v"></span><span class="tr-o"></span><span class="tr-st"></span></button>` +
    `<div class="tr-detail" id="${did}" role="group" aria-label="Call details" hidden>` +
    `<div class="tr-diffbox" hidden><div class="tr-lab"><span class="df-title">Changes</span>${cpOut(".df-raw", "Copy", "Copy the diff")}</div><div class="tr-diff" tabindex="0" role="region" aria-label="Changes"></div><pre class="df-raw" hidden></pre></div>` +
    `<div class="tr-inbox"><div class="tr-lab"><span>Input</span>${cpOut(".toolargs", "Copy", "Copy what the agent ran")}</div><pre class="tr-pre toolargs" tabindex="0" role="region" aria-label="Input"></pre></div>` +
    `<div class="tr-lab"><span>Output</span>${cpOut(".toolresult", "Copy", "Copy the result")}</div><pre class="tr-pre toolresult" tabindex="0" role="region" aria-label="Output"></pre>` +
    `</div>`;
  const row = item.querySelector(".tr-row"), det = item.querySelector(".tr-detail");
  row.addEventListener("click", () => toggleToolRow(item, det.hidden));
  fillToolRow(item, m);
  return item;
}

// ---- diffs: an Edit, MultiEdit or Write call shows what it changed ----
const DIFF_CTX = 2;        // unchanged lines kept around a change
const DIFF_WRITE_LINES = 14; // a Write of a new file shows its first lines
const DIFF_MAX_CELLS = 250000; // past this, old and new are not matched line by line

// diffLines compares two arrays of lines and returns [{t:" "|"-"|"+", s}] (LCS; a long pair is all removed then all added).
function diffLines(a, b) {
  let top = 0;
  while (top < a.length && top < b.length && a[top] === b[top]) top++;
  let bot = 0;
  while (bot < a.length - top && bot < b.length - top && a[a.length - 1 - bot] === b[b.length - 1 - bot]) bot++;
  const ma = a.slice(top, a.length - bot), mb = b.slice(top, b.length - bot);
  const out = a.slice(0, top).map((s) => ({ t: " ", s }));
  if (ma.length * mb.length > DIFF_MAX_CELLS) {
    for (const s of ma) out.push({ t: "-", s });
    for (const s of mb) out.push({ t: "+", s });
  } else {
    const n = ma.length, m = mb.length, w = m + 1;
    const L = new Uint32Array((n + 1) * w);
    for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) L[i * w + j] = ma[i] === mb[j] ? L[(i + 1) * w + j + 1] + 1 : Math.max(L[(i + 1) * w + j], L[i * w + j + 1]);
    let i = 0, j = 0;
    while (i < n || j < m) {
      if (i < n && j < m && ma[i] === mb[j]) { out.push({ t: " ", s: ma[i] }); i++; j++; }
      else if (j >= m || (i < n && L[(i + 1) * w + j] >= L[i * w + j + 1])) out.push({ t: "-", s: ma[i++] });
      else out.push({ t: "+", s: mb[j++] });
    }
  }
  for (const s of a.slice(a.length - bot)) out.push({ t: " ", s });
  return out;
}
function splitLines(t) {
  if (!t) return [];
  const l = t.split("\n");
  if (l[l.length - 1] === "") l.pop();
  return l;
}
// editRows turns an edit into display rows: {t, s} lines and {gap:n} for hidden unchanged lines.
function editRows(e) {
  const rows = [];
  if (e.write) {
    const lines = splitLines((e.edits[0] && e.edits[0].new) || "");
    for (const s of lines.slice(0, DIFF_WRITE_LINES)) rows.push({ t: "+", s });
    if (lines.length > DIFF_WRITE_LINES) rows.push({ more: lines.length - DIFF_WRITE_LINES });
    return rows;
  }
  e.edits.forEach((x, k) => {
    if (k) rows.push({ sep: true });
    const d = diffLines(splitLines(x.old), splitLines(x.new));
    const keep = new Array(d.length).fill(false);
    d.forEach((r, i) => { if (r.t !== " ") for (let j = Math.max(0, i - DIFF_CTX); j <= Math.min(d.length - 1, i + DIFF_CTX); j++) keep[j] = true; });
    let hidden = 0;
    d.forEach((r, i) => {
      if (keep[i]) { if (hidden) { rows.push({ gap: hidden }); hidden = 0; } rows.push(r); } else hidden++;
    });
    if (hidden) rows.push({ gap: hidden });
  });
  return rows;
}
function editPlain(e, rows) {
  const out = ["--- " + (e.path || "file"), "+++ " + (e.path || "file")];
  for (const r of rows) {
    if (r.sep) out.push("@@");
    else if (r.gap) out.push("@@ " + r.gap + " unchanged lines @@");
    else if (r.more) out.push("… " + r.more + " more lines");
    else out.push(r.t + r.s);
  }
  if (e.truncated) out.push("… cut: the change is longer than the app keeps");
  return out.join("\n");
}
function fillDiff(item, m) {
  const box = item.querySelector(".tr-diffbox");
  const key = m.edit ? JSON.stringify(m.edit) : "";
  box.hidden = !m.edit;
  item.querySelector(".tr-inbox").hidden = !!m.edit;
  if (item._edit === key) return;
  item._edit = key;
  const view = item.querySelector(".tr-diff"), raw = item.querySelector(".df-raw");
  view.textContent = "";
  raw.textContent = "";
  if (!m.edit) return;
  const e = m.edit, rows = editRows(e);
  const mk = (cls, sign, text) => {
    const d = document.createElement("div");
    d.className = "df-l " + cls;
    const g = document.createElement("span");
    g.className = "df-g"; g.setAttribute("aria-hidden", "true"); g.textContent = sign;
    const t = document.createElement("span");
    t.className = "df-t"; t.textContent = text;
    d.append(g, t);
    return d;
  };
  const note = (text) => { const d = document.createElement("div"); d.className = "df-note"; d.textContent = text; return d; };
  item.querySelector(".df-title").textContent = (e.write ? "New file " : "Changes ") + (e.path ? e.path.split("/").slice(-2).join("/") : "");
  for (const r of rows) {
    if (r.sep) view.append(note("⋯"));
    else if (r.gap) view.append(note(r.gap + " unchanged line" + (r.gap === 1 ? "" : "s")));
    else if (r.more) view.append(note(r.more + " more line" + (r.more === 1 ? "" : "s")));
    else view.append(mk(r.t === "+" ? "add" : r.t === "-" ? "del" : "ctx", r.t === " " ? "" : r.t, r.s));
  }
  if (e.edits.length && e.edits.some((x) => x.all)) view.append(note("replaces every match"));
  if (e.truncated) view.append(note("cut: the change is longer than the app keeps"));
  raw.textContent = editPlain(e, rows);
}

// fillToolRow sets (or refreshes) a row from its message; the text is only
// touched when it changed, so an open row keeps its scroll and selection.
function fillToolRow(item, m) {
  const set = (el, t) => { if (el.textContent !== t) el.textContent = t; };
  item.classList.toggle("bad", !!m.err); // !!: the server omits err when false
  item.classList.toggle("running", toolRunning(m));
  set(item.querySelector(".tr-v"), toolRowVerb(m));
  set(item.querySelector(".tr-o"), m.args || m.tool || "tool");
  set(item.querySelector(".tr-st"), toolMeta(m));
  set(item.querySelector(".toolargs"), (m.tool ? m.tool + (m.args ? " " : "") : "") + (m.args || ""));
  fillDiff(item, m);
  const out = item.querySelector(".toolresult");
  set(out, toolResultText(m));
  out.classList.toggle("err", !!m.err);
}

// updateToolRun recomputes a run's line from its calls.
function updateToolRun(run) {
  const s = toolRunSummary(run._calls);
  const set = (el, t) => { if (el.textContent !== t) el.textContent = t; };
  const line = run.querySelector(".tr-line");
  set(run.querySelector(".tr-txt"), s.text);
  set(run.querySelector(".tr-bad"), s.failed ? s.failed + " failed" : "");
  run.querySelector(".tr-dot").className = "tr-dot" + (s.running ? " run" : "");
  const peek = run.querySelector(".tr-peek");
  set(peek, s.peek);
  peek.hidden = !s.peek;
  line.setAttribute("aria-label", s.text + (s.failed ? ", " + s.failed + " failed" : "") + (s.running ? ", still running" : ""));
  // The red first line of the last call's error is read after the name, while it is shown.
  if (s.peek) line.setAttribute("aria-describedby", peek.id); else line.removeAttribute("aria-describedby");
}

// addRunCall puts a call at the end (or, for earlier messages, the start) of a run.
function addRunCall(run, m, front) {
  const list = run.querySelector(".tr-list");
  const item = toolRowEl(m);
  if (front) { list.querySelector(".tr-more").after(item); run._calls.unshift(m); } else { list.appendChild(item); run._calls.push(m); }
  applyRunCap(run);
  updateToolRun(run);
}

// appendMessage renders m into parent. Consecutive plain tool calls share one
// run (ctx.run); anything else, a subagent card included, ends it.
function appendMessage(parent, m, ctx) {
  if (m.kind === "tool" && !isSubagentCard(m)) {
    if (!ctx.run || ctx.run.parentNode !== parent) { ctx.run = renderToolRun(); parent.appendChild(ctx.run); }
    addRunCall(ctx.run, m, false);
    return;
  }
  ctx.run = null;
  parent.appendChild(renderMessage(m));
}

function patchToolCard(m, root = $("chat")) {
  const el = root.querySelector(`[data-id="${CSS.escape(m.id)}"]`);
  if (!el) return;
  if (el.classList.contains("subrun")) { patchSubagentCard(el, m); return; }
  const run = el.closest(".toolrun");
  fillToolRow(el, m);
  if (run) {
    const i = run._calls.findIndex((c) => c.id === m.id);
    if (i >= 0 && !run._calls[i].err && m.err) announceTools("A tool call failed: " + (m.args || m.tool || "tool"));
    if (i >= 0) run._calls[i] = m;
    updateToolRun(run);
  }
}

/* ---------- thinking: a quiet line that opens to the thinking text ---------- */

// thoughtLabel: "Thought for 12s" when the server could tell how long, else "Thought".
function thoughtLabel(ms) {
  const s = Math.round((ms || 0) / 1000);
  if (s < 1) return "Thought";
  return "Thought for " + (s < 60 ? s + "s" : Math.floor(s / 60) + "m " + (s % 60) + "s");
}

// renderThinking builds the line; the text is rendered as markdown the first
// time it is opened. live: the agent is still thinking.
function renderThinking(text, ms, key, live) {
  const d = document.createElement("div");
  d.className = "thinking" + (live ? " live" : "");
  d._key = key;
  d._text = text;
  const bid = trId("thk");
  d.innerHTML =
    `<button type="button" class="tr-line" aria-expanded="false" aria-controls="${bid}"><span class="tr-dot" aria-hidden="true"></span>` +
    '<span class="tr-txt"></span><span class="tr-chev" aria-hidden="true">›</span></button>' +
    `<div class="think-body md" id="${bid}" hidden></div>`;
  d.querySelector(".tr-txt").textContent = live ? "Thinking…" : thoughtLabel(ms);
  d.querySelector(".tr-dot").className = "tr-dot" + (live ? " run" : "");
  const line = d.querySelector(".tr-line"), body = d.querySelector(".think-body");
  line.addEventListener("click", () => {
    const open = body.hidden;
    d._manual = open; // the user's own open or close beats the default
    body.hidden = !open;
    line.setAttribute("aria-expanded", String(open));
    if (open) fillThinking(d);
  });
  if (CT.openThink()) { body.hidden = false; line.setAttribute("aria-expanded", "true"); fillThinking(d); }
  return d;
}
// A change to Open by default in another tab reaches lines already on screen that the user has not toggled.
window.addEventListener("storage", (e) => {
  if (e.key !== CT.KEY_OPEN) return;
  for (const root of [$("chat"), $("sv-chat")]) {
    if (!root) continue;
    for (const d of root.querySelectorAll(".thinking")) {
      if (d._manual !== undefined) continue;
      const body = d.querySelector(".think-body"), on = CT.openThink();
      body.hidden = !on; d.querySelector(".tr-line").setAttribute("aria-expanded", String(on));
      if (on) fillThinking(d);
    }
  }
  const c = $("chat"), sv = $("sv-chat");
  if (c) syncLiveRuns(c, !!activity.seenAt);
  if (sv) for (const run of sv.querySelectorAll(".toolrun")) if (run._manual === undefined) setRunOpen(run, CT.openTools());
});
function fillThinking(d) {
  const body = d.querySelector(".think-body");
  if (body.dataset.md === d._text) return;
  body.dataset.md = d._text;
  body.innerHTML = renderMarkdown(d._text, d._key);
}

// syncLiveThinking keeps one "Thinking…" line above the activity row while the
// agent is thinking, which opens to the latest thought; it goes when the agent
// moves on (the finished thought is then the "Thought for Ns" line). Returns
// whether the line shows.
function syncLiveThinking(chatEl, show, row) {
  let el = chatEl.querySelector(":scope > .thinking.live");
  const th = show && state.chat && state.chat.thinking;
  if (!th || !th.text) { if (el) el.remove(); return false; }
  if (!el) el = renderThinking(th.text, 0, "live-thinking", true);
  if (el._text !== th.text) {
    el._text = th.text;
    if (!el.querySelector(".think-body").hidden) fillThinking(el);
  }
  if (el.nextSibling !== row || el.parentNode !== chatEl) chatEl.insertBefore(el, row);
  return true;
}

/* ---------- subagent lines (Claude's Agent/Task calls) ---------- */
//
// A subagent call is its own grey line, in the tool runs' style, never part
// of a run: an "agent" tag, what it is for (its description) and its status,
// ticking while it runs. Opened, it shows its type, the agent's report as
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
  const d = document.createElement("div");
  d.className = "subrun";
  d.dataset.id = m.id;
  const bid = trId("sab");
  d.innerHTML =
    `<button type="button" class="tr-line sa-line" aria-expanded="false" aria-controls="${bid}"><span class="tr-dot" aria-hidden="true"></span>` +
    '<span class="sa-tag">agent</span><span class="tr-txt sa-desc"></span><span class="sa-meta"></span>' +
    '<span class="tr-chev" aria-hidden="true">›</span></button>' +
    `<div class="sa-body" id="${bid}" role="group" aria-label="Subagent details" hidden><div class="sa-flags"></div><div class="sa-report"></div>' +
    '<button type="button" class="sa-steps" hidden>View steps ›</button>' +
    '<details class="sa-prompt"><summary>Prompt</summary><div class="sa-prompt-text"></div></details>' +
    '<pre class="tr-pre toolresult err" tabindex="0" role="region" aria-label="Error"></pre></div>`;
  // The report is rendered when the line is first opened (most never are).
  const line = d.querySelector(".sa-line"), body = d.querySelector(".sa-body");
  line.addEventListener("click", () => {
    const open = body.hidden;
    body.hidden = !open;
    line.setAttribute("aria-expanded", String(open));
    if (open) fillSubagentReport(d);
  });
  patchSubagentCard(d, m);
  return d;
}

function patchSubagentCard(el, m) {
  const s = subagentInfo(m);
  const was = el._m ? subagentInfo(el._m).status : "";
  el._m = m; // for the ticker and "View steps"
  const running = s.status === "running";
  if (was === "running" && !running) announceTools("Subagent " + (s.description || m.args || "") + " " + (s.status === "completed" ? "finished" : s.status));
  const bad = !!m.err || s.status === "failed" || s.status === "killed";
  el.classList.toggle("running", running);
  el.classList.toggle("err", bad);
  el.querySelector(".tr-dot").className = "tr-dot" + (running ? " run" : "");
  const desc = s.description || m.args || "";
  const set = (e, t) => { if (e.textContent !== t) e.textContent = t; };
  set(el.querySelector(".sa-desc"), desc || "Subagent");
  const flags = [s.type || m.tool || "", s.background ? "bg" : "", s.worktree ? "⑂ worktree" : ""].filter(Boolean);
  set(el.querySelector(".sa-flags"), flags.join(" · "));
  set(el.querySelector(".sa-meta"), subagentMeta(m));
  el.querySelector(".sa-line").setAttribute("aria-label", "Subagent: " + (desc || "subagent") + ", " + subagentMeta(m));
  el.querySelector(".sa-steps").hidden = !s.agent_id;
  const prompt = el.querySelector(".sa-prompt");
  prompt.hidden = !s.prompt;
  const promptText = prompt.querySelector(".sa-prompt-text");
  if (promptText.textContent !== (s.prompt || "")) promptText.textContent = s.prompt || "";
  // An errored call has no report, just the error.
  set(el.querySelector(".toolresult"), m.err ? m.result || "" : "");
  const report = s.report || "";
  if (el._report !== report || el._running !== running) {
    el._report = report;
    el._running = running;
    el.querySelector(".sa-report").dataset.md = "";
    if (!el.querySelector(".sa-body").hidden) fillSubagentReport(el);
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
  if (subagentTimer || !document.querySelector(".subrun.running")) return;
  subagentTimer = setInterval(() => {
    if (document.hidden) return;
    const cards = document.querySelectorAll(".subrun.running");
    if (!cards.length) { clearInterval(subagentTimer); subagentTimer = null; return; }
    for (const el of cards) if (el._m) el.querySelector(".sa-meta").textContent = subagentMeta(el._m);
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
      appendMessage(el, m, subview);
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
      `<div class="msgtime">${p.sending ? (p.waking ? "starting " + esc(p.waking) + "…" : "sending…") : working ? "queued — the agent picks it up at its next step" : "sent"}</div>`;
    chatEl.insertBefore(div, before);
  }
}

// originChip: the label of a message the user did not write.
function originChip(m) {
  if (m.origin === "agent") {
    const n = String(m.origin_name || "").replace(/\s*\(.*$/, "").trim();
    return "agent" + (n ? " " + n : "");
  }
  return "hub";
}

// renderOriginLine: a hub, relay or agent-sent message as a quiet line that
// opens to its full text.
function renderOriginLine(m) {
  const d = document.createElement("details");
  d.className = "msg sys origin-" + m.origin;
  const one = String(m.text || "").replace(/\s+/g, " ").trim();
  d.innerHTML = `<summary><span class="ochip"></span><span class="otxt"></span></summary><div class="obody"></div>` +
    (m.time ? `<div class="msgtime">${esc(fmtTime(m.time))}</div>` : "");
  d.querySelector(".ochip").textContent = originChip(m);
  d.querySelector(".otxt").textContent = one;
  d.querySelector(".obody").textContent = m.text || "";
  return d;
}

function renderMessage(m) {
  switch (m.kind) {
    case "user": {
      // Text the hub typed (a notice, a relayed answer) or another agent sent
      // is a quiet line with a chip, never the user's bubble. The server
      // decides from its own record (m.origin), not from the text.
      if (m.origin === "hub" || m.origin === "relay" || m.origin === "agent") return renderOriginLine(m);
      const div = document.createElement("div");
      div.className = "msg user";
      div.innerHTML = `<div class="bubble">${esc(m.text)}</div>` +
        (m.time || m.text ? `<div class="msgtime">${m.time ? esc(fmtTime(m.time)) : ""}${m.text ? copyBtnHtml("msg:" + m.id, m.text, "Copy", "Copy message", "cpb-msg") : ""}</div>` : "");
      return div;
    }
    case "assistant": {
      const wrap = document.createDocumentFragment();
      if (m.think) {
        wrap.appendChild(renderThinking(m.think, m.think_ms, "t:" + m.id));
      }
      const div = document.createElement("div");
      div.className = "msg assistant";
      div.innerHTML = `<div class="bubble md">${renderMarkdown(m.text, "a:" + m.id)}</div>` +
        (m.time || m.text ? `<div class="msgtime">${m.time ? esc(fmtTime(m.time)) + (m.model ? " · " + esc(m.model) : "") : ""}${m.text ? copyBtnHtml("msg:" + m.id, m.text, "Copy", "Copy message", "cpb-msg") : ""}</div>` : "");
      wrap.appendChild(div);
      return wrap;
    }
    case "tool": {
      if (isSubagentCard(m)) return renderSubagentCard(m);
      const run = renderToolRun();
      addRunCall(run, m, false);
      return run;
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
    const ctx = { run: null };
    for (const m of t.messages) {
      if (chat.rendered.has(m.id)) continue;
      chat.rendered.add(m.id);
      appendMessage(frag, m, ctx);
    }
    const earlier = chatEl.querySelector(".load-earlier");
    // A run cut by the page boundary continues in the run already on screen.
    const after = earlier ? earlier.nextElementSibling : chatEl.firstElementChild;
    if (ctx.run && after && after.classList.contains("toolrun")) {
      for (const m of [...ctx.run._calls].reverse()) addRunCall(after, m, true);
      ctx.run.remove();
    }
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
  $("btn-copy-screen").hidden = true;
  copySrc.delete("term-screen");
  $("chat").style.display = "";
  $("btn-terminal").hidden = true;
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
    copySrc.set("term-screen", (read.text || "").replace(/\n+$/, "")); // the plain text the hub read, for Copy screen
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
  // A live note (the auto-block countdown) ticks without re-rendering the card.
  const noteEl = el.querySelector(".ans-note");
  if (noteEl && d && d.note && noteEl.textContent !== d.note) noteEl.textContent = d.note;
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
    if (d.detail) html += cpOut(".ans-detail", "Copy", "Copy the details") + `<pre class="ans-detail">${esc(d.detail)}</pre>`;
    if (d.note) html += `<div class="ans-note" role="status">${esc(d.note)}</div>`;
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
  html += cpOut("#answer-snap", "Copy screen", "Copy what the agent's terminal shows") + `<pre class="snap" id="answer-snap">${esc(answer.snapText)}</pre></details>`;
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
    case "claude_trust":
      h.cancel = "Esc · Exit Claude";
      h.pad = "↑/↓ choose, Enter confirms, Esc exits Claude.";
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
  try { path = pathForSend(pane); } catch (e) { toast("prompt failed: " + e.message, true); return; }
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

// refusalNote: what a 409 from the prompt route means, said plainly. The
// server's message names the cause (the text in the box, the dialog).
function refusalNote(e) {
  const m = e.message || "";
  if (e.code === "input_busy") return "Not sent: " + m.replace(/^held: /, "");
  if (e.code === "dialog_open") return "Not sent: the agent is waiting at a dialog. Answer it first, or queue the message.";
  if (e.code === "agent_blocked") return "Not sent: the agent says it is waiting for input. Answer it, or queue the message.";
  return "Not sent: " + (m || "the agent refused the message");
}

// refusalActions: "Send anyway" where the server allows it (a held box, an
// unrecognised wait), and "Queue" (delivered when the agent is free).
function refusalActions(e, job) {
  const acts = [];
  const forceable = e.code === "input_busy" || e.code === "agent_blocked" || e.code === "dialog_open";
  if (forceable && e.code !== "dialog_open") acts.push({ label: "Send anyway", fn: () => resendPrompt(job, true) });
  const a = agentById(job.pane);
  if (a) acts.push({ label: "Queue", fn: () => queuePrompt(job, a) });
  return acts;
}

async function resendPrompt(job, force) {
  const { text, pane, path } = job;
  try {
    await api(`${path}/prompt`, { method: "POST", body: JSON.stringify({ text, force }) });
    if (state.selected === pane && promptValue().trim() === text.trim()) setPromptValue("");
    toast("sent");
    state.pollUntil = Date.now() + POLL_AFTER_SEND_MS;
  } catch (e) {
    toast(refusalNote(e) + (e.status === 409 ? "" : " (" + e.message + ")"), true);
  }
}

async function queuePrompt(job, a) {
  try {
    await api("/api/queue", { method: "POST", body: JSON.stringify({ agent: agentId(a), name: a.name || "", text: job.text }) });
    if (state.selected === job.pane && promptValue().trim() === job.text.trim()) setPromptValue("");
    toast("queued: it is sent when the agent is free");
    refreshQueued(true);
  } catch (e) {
    toast("could not queue: " + e.message, true);
  }
}

async function postPrompt(job) {
  const { text, pane, path, pending, terminal } = job;
  try {
    // A stopped worker is started by the send (the hub answers 202 host_waking); the text waits here.
    await apiWake(hostOf(pane), `${path}/prompt`, { method: "POST", body: JSON.stringify({ text }) }, (h) => { if (pending) { pending.waking = h; renderPending(); } });
    if (pending) delete pending.waking;
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
    if (e.code === "session_gone") {
      sessionGoneToast(e, hostOf(pane));
    } else if (e.status === 409 && e.code !== "wake_limit" && e.code !== "needs_confirm" && e.code !== "host_stopped") {
      toastActions(refusalNote(e), refusalActions(e, job), 20000);
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
      peersHostsChanged();
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
    peersHostsChanged();
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
      if (a && prev !== a.agent_status && state.chat && state.chat.terminal && state.chat.auto) retryTranscript();
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
// hostKindDefault: the model and thinking level this host starts a kind with
// when the request names none (its settings).
function hostKindDefault() {
  const o = launch.opts;
  return (o && o.kind_settings && launch.kind && o.kind_settings[launch.kind]) || {};
}

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
    const hd = hostKindDefault();
    val.textContent = hd.model ? `Host default (${hd.model})` : cat.default ? `Default (${cat.default})` : "Default";
    val.classList.remove("set");
  }
  // Like a model pill with its effort beside it: pi's thinking level, muted.
  $("ln-model-think").textContent = launch.kind === "pi" ? (launch.thinking || hostKindDefault().thinking || cat.default_thinking || "") : "";
  $("ln-model-btn").setAttribute("aria-expanded", String(launch.picking));
  $("ln-picker").hidden = !launch.picking;
  if (launch.picking) renderModelList();
  let note = "";
  if (cat.error) note = `Could not list models (${cat.error}). ${launch.kind === "pi" ? "" : "You can still type a name. "}Default still works.`;
  else if (!launch.model && hostKindDefault().model) note = `${launch.host || "This host"} starts ${launch.kind} with ${hostKindDefault().model} unless you choose another model. Change it under Peers.`;
  else if (!launch.model) note = "Default passes no flag: the agent uses its own setting.";
  else {
    const m = (cat.models || []).find((x) => x.id === launch.model);
    if (m && launch.kind === "pi" && !m.thinking && launch.thinking && launch.thinking !== "off") note = "This model does not reason: the thinking level has no effect.";
  }
  $("ln-model-note").textContent = note;
  const think = launch.kind === "pi";
  $("ln-thinking").hidden = !think;
  if (think) {
    const hdt = hostKindDefault().thinking, dt = hdt || cat.default_thinking;
    const dflt = dt ? `${hdt ? "Host default" : "Default"} (${dt})` : "Default";
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
  const f = hostSel();
  launch.host = f.length === 1 ? f[0] : state.localHost;
  // A hub-only hub starts nothing itself: begin on the first host that can.
  const hosting = (state.hosts || []).find((h) => h.name === launch.host);
  if (hosting && hosting.no_agents) {
    const other = state.hosts.find((h) => !h.no_agents && (h.local || h.status === "ok"));
    if (other) launch.host = other.name;
  }
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
  if (state.hosts.some((h) => !h.local && h.status !== "ok")) phEnsure(); // is a down host stopped? (the tag and Start follow)
  nav.innerHTML = state.hosts.map((h) => {
    const wx = wakeOf(h); // a stopped worker can be picked: launching starts it (issue #66)
    const down = !h.local && h.status !== "ok" && !wx;
    const off = down || h.no_agents; // a hub-only hub starts nothing itself
    const tag = wx ? ` <small class="ln-wk ${esc(wx.kind)}">${esc(wx.kind === "stopped" ? "stopped" : wx.kind === "starting" ? "starting…" : "failed to start")}</small>` : "";
    return `<button type="button" class="hf-chip${down ? " down" : ""}${wx ? " wk-" + esc(wx.kind) : ""}" data-host="${esc(h.name)}"` +
      `${h.no_agents ? ' title="This hub hosts no agents"' : ""}` +
      ` aria-pressed="${launch.host === h.name}"${off ? " disabled" : ""}>${esc(h.name)}${tag}</button>`;
  }).join("");
  nav.hidden = false;
}

// A stopped worker has no folders to list: its disk is off. Picking it shows
// that, with a Start button; launching on it (after the folder list is back,
// or from a folder chosen before it stopped) starts it through the wake.
function launchHostStopped(host) {
  const h = hostStatus(host);
  return !!h && !!wakeOf(h);
}
function renderLaunchStopped(host) {
  const h = hostStatus(host), x = h && wakeOf(h);
  if (!x) return;
  const msg = x.kind === "starting" ? `Starting ${host}…` : x.kind === "failed" ? `${host} did not start: ${x.reason}` : `${host} is stopped. Start it to choose a folder; launching on it also starts it (about 2 minutes).`;
  $("ln-list").innerHTML = `<div class="ln-empty wk-launch" data-wk-launch="${esc(host)}"><p>${esc(msg)}</p>${wakeAction(h, x)}</div>`;
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
  $("ln-crumb").textContent = "";
  if (launchHostStopped(host)) { renderLaunchStopped(host); return; }
  $("ln-list").innerHTML = '<div class="ln-empty">Loading…</div>';
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
  // The host's own default agent is marked, so a choice of another is deliberate.
  const hostDefault = o && (o.default_source === "setting" || o.default_source === "flag") ? o.default_kind : "";
  $("ln-kinds").innerHTML = kinds.map((k) =>
    `<button type="button" class="hf-chip" data-kind="${esc(k)}" aria-pressed="${k === launch.kind}"${k === hostDefault ? ' title="This host\'s default agent"' : ""}>${esc(k)}${k === hostDefault ? ' <small class="ln-defmark">default</small>' : ""}</button>`).join("") ||
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
      "It hands work to workers and follows them in the ledger; " +
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
    r = await apiWake(host, launchBase(host), { method: "POST", timeout: 180000, body: JSON.stringify(body) }, (h) => { $("ln-status").textContent = `Starting ${h}…`; });
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

// pagesChat: "Chat about this" in the viewer. One tap starts the read-only
// helper (POST /api/pages/{id}/chat) and opens its conversation. A second tap
// while the first is starting does nothing.
const PAGE_CHAT_SAYS = {
  chat_no_readonly: "That kind of agent has no read-only mode. Pick it in “Chat with another agent…” and confirm to start it anyway.",
  chat_override_user_only: "Only you can start a helper that is not read-only.",
  chat_no_host: "No host can start a chat right now. Check that a computer running agents is online, then try again.",
  chat_rate: "You started several chats a moment ago. Wait a minute, then try again.",
  chat_host_away: "The computer that runs this page's chat is not answering. Try again in a moment, or start a new chat.",
};
// pgsChatGate: "Chat about this" is off, with its reason, for a Page whose
// helper would be a kind with no read-only mode (the route refuses it too).
// The answer is asked once per Page and kept until the view is left.
const pgsChatOff = new Map(); // page id -> reason ("" = on)
function pgsChatShow(id) {
  const why = pgsChatOff.get(id) || "";
  for (const sel of ["#pgs-chat", "#pgs-chatmi", "#pgs-chatnew", "#pgs-chatpick"]) {
    const b = $(sel.slice(1));
    if (!b) continue;
    if (why) { b.setAttribute("aria-disabled", "true"); b.title = why; b.classList.add("pgs-chatoff"); }
    else { b.removeAttribute("aria-disabled"); b.classList.remove("pgs-chatoff"); }
  }
  document.querySelectorAll(`.rv-chat[data-page="${id}"]`).forEach((b) => {
    if (why) { b.setAttribute("aria-disabled", "true"); b.title = why; b.classList.add("pgs-chatoff"); }
    else { b.removeAttribute("aria-disabled"); b.classList.remove("pgs-chatoff"); b.title = RV_CHAT_TITLE; }
  });
  const main = $("pgs-chat");
  if (main && !why) main.title = "Chat about this page: start a read-only helper";
}
// What the options route said about a Page: the host and kind a plain tap
// uses, whether that kind needs the user's say-so (needs_override), and every
// host with its kinds. The button is never greyed for a kind with no
// read-only mode: a tap asks first.
const pgsChatInfo = new Map(); // page id -> chat-options answer
function pgsChatGate(id) {
  if (pgsChatInfo.has(id)) { pgsChatShow(id); return; }
  api("/api/pages/" + encodeURIComponent(id) + "/chat-options").then((o) => {
    pgsChatInfo.set(id, o || {});
    pgsChatOff.set(id, "");
    pgsChatShow(id);
  }, () => {});
}
async function pagesChat(fresh, pageId, pick) {
  const pid = pageId || (pgs.route && pgs.route.id);
  if (!pid || pgs.chatBusy) return;
  if (pgsChatOff.get(pid)) { toast(pgsChatOff.get(pid), true); return; }
  // A plain tap that would start a kind with no read-only mode asks first.
  const info = pgsChatInfo.get(pid);
  if (!pick && info && info.needs_override && (fresh || !info.existing)) { openChatPick(pid, fresh); return; }
  pgs.chatBusy = true;
  $("pgs-chat")?.setAttribute("aria-busy", "true");
  document.querySelectorAll(".rv-chat").forEach((b) => b.setAttribute("aria-busy", "true"));
  toast(fresh ? "Starting a new chat about this page…" : "Opening the chat about this page…");
  try {
    const r = await api("/api/pages/" + encodeURIComponent(pid) + "/chat", { method: "POST", body: JSON.stringify(Object.assign(fresh ? { new: true } : {}, pick || {})), timeout: 180000 });
    const pane = r.agent && r.agent.pane_id;
    const label = r.name || "chat";
    let note = "";
    let done = "";
    if (r.reused === "open") done = r.page_note ? `Opened ${label}. ${r.page_note}` : `Opened ${label}`;
    else if (r.reused === "resumed") done = r.page_note ? `Resumed ${label}. ${r.page_note}` : `Resumed ${label}`;
    else if (r.replaced) done = `The earlier chat was closed, so ${label} is a new one.`;
    if (r.writable) done = (done ? done + " " : "") + `${label} is not read-only.`;
    if (r.prompt_error) note = "The chat started, but its first message did not go in: " + r.prompt_error;
    else if (r.prompt_note) note = "The chat started but is waiting at a prompt. Open it and answer the prompt, and it will read the page.";
    if (pane) openLaunched(r.host, pane, label, note || "", done);
    else toast("The chat started, but its conversation could not be opened. Look for " + label + " in Agents.", true);
  } catch (e) {
    toast(PAGE_CHAT_SAYS[e.code] || (e.status === 429 ? PAGE_CHAT_SAYS.chat_rate : "Could not start a chat: " + (e.message || "the host did not answer") + "."), true);
  } finally {
    pgs.chatBusy = false;
    $("pgs-chat")?.removeAttribute("aria-busy");
    document.querySelectorAll(".rv-chat").forEach((b) => b.removeAttribute("aria-busy"));
  }
}

// openLaunched opens the new agent's chat as soon as the agent list (SSE)
// has it; herdr reports it within a second or two. A note replaces "Started".
function openLaunched(host, pane, label, note, said) {
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
  toast(note || said || `Started ${label}`, !!note);
  tick();
}

/* ---------- "Chat about this": pick the agent ---------- */
//
// A sheet over the Page: the host, the agent kind and (optionally) the model
// of the helper. A kind with no read-only mode (pi) is marked, and picking it
// shows what that means; "Start anyway" then sends allow_writable. The one
// tap on Chat is unchanged (the host's default); it opens this sheet only when
// that default has no read-only mode.

const cp = { open: false, page: "", fresh: false, host: "", kind: "", model: "", info: null, cat: null, seq: 0 };

function cpHost() { return ((cp.info && cp.info.hosts) || []).find((h) => h.name === cp.host) || null; }
function cpKindRO(kind) { const h = cpHost(); const k = h && h.kinds.find((x) => x.kind === kind); return !k || !!k.read_only; }

function openChatPick(pageId, fresh) {
  if (!pageId || cp.open || pgs.chatBusy) return;
  Object.assign(cp, { open: true, page: pageId, fresh: !!fresh, info: null, cat: null, model: "", host: "", kind: "" });
  history.pushState({ chatpick: true }, "", location.href);
  $("chatpick").hidden = false;
  document.body.classList.add("sheet-open");
  $("cp-status").textContent = "";
  renderChatPick();
  $("cp-close").focus();
  const seq = ++cp.seq;
  api("/api/pages/" + encodeURIComponent(pageId) + "/chat-options").then((o) => o || {}, () => ({ hosts: [] })).then((o) => {
    if (seq !== cp.seq || !cp.open) return;
    pgsChatInfo.set(pageId, o);
    cp.info = o;
    const hosts = (o.hosts || []).filter((h) => h.ok);
    const h = hosts.find((x) => x.name === o.host) || hosts[0];
    if (h) { cp.host = h.name; cp.kind = h.kinds.some((k) => k.kind === (o.kind || h.default_kind)) ? (o.kind || h.default_kind) : h.kinds[0].kind; }
    cpKindChosen();
  });
}

function cpKindChosen() {
  cp.model = "";
  cp.cat = null;
  const h = cpHost();
  const seq = ++cp.seq;
  if (h && cp.kind && (h.model_kinds || []).includes(cp.kind)) {
    cp.cat = { loading: true };
    api(modelsBase(h.local ? null : h.name) + "?kind=" + encodeURIComponent(cp.kind))
      .then((d) => (d.catalogs || [])[0] || { error: "the host sent no model list" }, (e) => ({ error: e.message }))
      .then((cat) => { if (seq === cp.seq && cp.open) { cp.cat = cat; renderChatPick(); } });
  }
  renderChatPick();
}

function hideChatPick() {
  cp.open = false;
  cp.seq++;
  $("chatpick").hidden = true;
  document.body.classList.remove("sheet-open");
  maybeReload();
}
function closeChatPick() {
  if ($("chatpick").classList.contains("busy")) return;
  if (history.state && history.state.chatpick) history.back();
  else hideChatPick();
}
function syncChatPick() {
  if (cp.open && !(history.state && history.state.chatpick)) hideChatPick();
}

function renderChatPick() {
  const info = cp.info;
  const h = cpHost();
  const hosts = ((info && info.hosts) || []);
  const ex = info && info.existing;
  $("cp-note").textContent = !info ? "Loading the choices…"
    : !hosts.some((x) => x.ok) ? "No host can start an agent right now."
    : ex && !cp.fresh ? `This Page already has a chat (${ex.name}). Starting one here replaces it; the old helper is left running.`
    : "The helper reads this Page and talks it over with you, in a scratch folder, not in your code.";
  const live = hosts.filter((x) => x.ok);
  $("cp-hostblock").hidden = live.length < 2;
  $("cp-hosts").innerHTML = hosts.map((x) =>
    `<button type="button" class="hf-chip${x.ok ? "" : " down"}" data-host="${esc(x.name)}" aria-pressed="${x.name === cp.host}"${x.ok ? "" : ` disabled title="${escAttr(x.reason || "")}"`}>${esc(x.name)}</button>`).join("");
  $("cp-kinds").innerHTML = h ? h.kinds.map((k) =>
    `<button type="button" class="hf-chip${k.read_only ? "" : " cp-rw"}" data-kind="${esc(k.kind)}" aria-pressed="${k.kind === cp.kind}">${esc(k.kind)}<small>${k.read_only ? "read-only" : "no read-only mode"}</small></button>`).join("") : "";
  const mb = $("cp-modelblock");
  const cat = cp.cat;
  mb.hidden = !cat;
  if (cat) {
    const sel = $("cp-model");
    const models = (cat.models || []);
    sel.innerHTML = `<option value="">${cat.loading ? "Loading…" : "Default" + (cat.default ? ` (${esc(cat.default)})` : "")}</option>` +
      models.map((m) => `<option value="${escAttr(m.id)}"${m.id === cp.model ? " selected" : ""}>${esc(m.id)}</option>`).join("");
    sel.value = cp.model;
  }
  const rw = !!h && !!cp.kind && !cpKindRO(cp.kind);
  const warn = $("cp-warn");
  warn.hidden = !rw;
  if (rw) warn.innerHTML = `<b>${esc(cp.kind)} has no read-only mode.</b> This helper can change files and run commands on ${esc(h.name)}. It starts in a scratch folder, not in your code. Start it anyway?`;
  const go = $("cp-go");
  go.textContent = rw ? "Start anyway" : (ex && !cp.fresh) || cp.fresh ? "Start a new chat" : "Start chat";
  go.classList.toggle("warn", rw);
  go.disabled = !h || !cp.kind || $("chatpick").classList.contains("busy");
}

async function submitChatPick() {
  const h = cpHost();
  if (!h || !cp.kind || $("chatpick").classList.contains("busy")) return;
  const pick = { host: h.name, kind: cp.kind };
  if (cp.model) pick.model = cp.model;
  if (!cpKindRO(cp.kind)) pick.allow_writable = true;
  const ex = cp.info && cp.info.existing;
  const page = cp.page;
  const fresh = cp.fresh || !!ex;
  hideChatPick();
  if (history.state && history.state.chatpick) history.replaceState(null, "", location.href); // the sheet's entry is spent
  await pagesChat(fresh, page, pick);
}

function initChatPick() {
  $("cp-close").addEventListener("click", closeChatPick);
  $("chatpick").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeChatPick(); } });
  $("cp-hosts").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-host]");
    if (!b || b.disabled) return;
    cp.host = b.dataset.host;
    const h = cpHost();
    if (!h.kinds.some((k) => k.kind === cp.kind)) cp.kind = h.default_kind || h.kinds[0].kind;
    cpKindChosen();
  });
  $("cp-kinds").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-kind]");
    if (b) { cp.kind = b.dataset.kind; cpKindChosen(); }
  });
  $("cp-model").addEventListener("change", () => { cp.model = $("cp-model").value; });
  $("cp-go").addEventListener("click", submitChatPick);
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
  return !!state.selected || state.sending || answer.inflight || !!promptValue().trim() || launch.open || review.open || sw.open || cp.open;
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
  $("chat").addEventListener("keydown", onToolKey);
  document.addEventListener("keydown", onTodoKey);
  $("btn-collapse").addEventListener("click", () => setListCollapsed(true));
  $("list-rail").addEventListener("click", () => setListCollapsed(false));
  if (lsGet(LIST_COLLAPSED_KEY) === "1") setListCollapsed(true, false);
  desktopMQ.addEventListener("change", () => { setView(viewFromHash()); renderList(); markSelectedRow(); syncStatusScreen(); });
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
    const card = b && b.closest(".subrun");
    if (card && card._m) openSubview(card._m);
  });
  window.addEventListener("popstate", route);
  window.addEventListener("hashchange", route);
  initStatusScreen();
  initRecall();
  initPlans();
  initPages();
  initPeers();
  initUsage();
  initSettings();
  initArchived();
  route(); // cold start on #/agent/[<host>/]<pane>
  $("btn-terminal").addEventListener("click", () => {
    const chat = state.chat;
    if (chat && chat.terminal && !chat.auto) {
      // "Show chat": back from a raw view chosen by hand.
      chat.terminal = false;
      if (outputAbort) outputAbort.abort();
      outputAbort = null;
      outputBusy = false;
      $("output").style.display = "none";
      $("btn-copy-screen").hidden = true;
      copySrc.delete("term-screen");
      $("chat").style.display = "";
      syncTermButton();
      loadChat(false);
      return;
    }
    // Show the raw pane by hand; the chat view stays behind it.
    if (chat) { chat.terminal = true; chat.auto = false; }
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
  $("rv-sweep").addEventListener("click", () => { if (!sweep.busy && !review.busy) runSweep(true); });
  $("rv-sweepbox").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-act]");
    if (b) sweepAction(b.dataset.act);
  });
  $("rv-list").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-act]");
    if (b) reviewAction(b.dataset.act, b.dataset.id, b.dataset.item);
  });
  $("rv-auto").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-act]");
    if (b) reviewAction(b.dataset.act, b.dataset.id);
  });
  $("rv-bulk").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-act]");
    if (b) reviewAction(b.dataset.act, b.dataset.id);
  });
  const rvSelect = (ev) => {
    const c = ev.target.closest("input.rv-chk, #rv-selall");
    if (!c) return;
    if (c.id === "rv-selall") review.sel = c.checked ? new Set(reviewOpenIds()) : new Set();
    else if (c.checked) review.sel.add(c.dataset.id); else review.sel.delete(c.dataset.id);
    review.confirm = null;
    renderReview();
  };
  $("rv-list").addEventListener("change", rvSelect);
  $("rv-bulk").addEventListener("change", rvSelect);
  $("d-refresh").addEventListener("click", refreshView);
  $("d-more").addEventListener("click", (ev) => { ev.stopPropagation(); toggleDetailMenu(); });
  $("d-mark").addEventListener("click", toggleMark);
  $("d-transcript").addEventListener("click", detailTranscript);
  $("d-markbar").addEventListener("click", (ev) => {
    const b = ev.target.closest("button[data-mb]");
    if (!b || !state.selected) return;
    if (b.dataset.mb === "unmark") unmarkAgent(state.selected);
  });
  $("row-mark").addEventListener("click", rowMenuMark);
  $("row-restart").addEventListener("click", () => { const id = $("row-menu").dataset.id; closeRowMenu(); if (id) openRestart(id); });
  $("rt-close").addEventListener("click", closeRestart);
  $("rt-go").addEventListener("click", submitRestart);
  $("rt").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeRestart(); } });
  $("row-park").addEventListener("click", () => { const id = $("row-menu").dataset.id; closeRowMenu(); if (id) parkAgentUI(id); });
  $("ps-close").addEventListener("click", closeParkAll);
  $("ps-go").addEventListener("click", submitParkAll);
  $("ps-list").addEventListener("change", (ev) => { const i = ev.target.dataset && ev.target.dataset.i; if (i != null) { parkAll.list[+i].on = ev.target.checked; renderParkAll(); } });
  $("ps").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeParkAll(); } });
  $("pk-close").addEventListener("click", closeParkSheet);
  $("pk-go").addEventListener("click", submitResume);
  $("pk-pin").addEventListener("click", togglePin);
  $("pk-rm").addEventListener("click", sendParkToRemoval);
  $("pk").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeParkSheet(); } });
  loadParked();
  setInterval(() => { if (!document.hidden) { loadParked(); loadArchived(); } }, 30000);
  $("row-transcript").addEventListener("click", rowMenuTranscript);
  initPopovers();
  initHostPicker();
  initListFilter();
  $("mk-close").addEventListener("click", closeMark);
  $("mk-go").addEventListener("click", submitMark);
  $("d-fresh").addEventListener("click", openFresh);
  $("fs-close").addEventListener("click", closeFresh);
  $("fs-go").addEventListener("click", submitFresh);
  $("fs").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeFresh(); } });
  $("mk").addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.preventDefault(); closeMark(); } });
  $("btn-fleet").addEventListener("click", (ev) => { ev.stopPropagation(); toggleFleetMenu(); });
  $("nav").addEventListener("click", onNavClick);
  for (const id of ["ps-body", "ss-body"]) {
    $(id).addEventListener("click", onStatusClick);
    $(id).addEventListener("toggle", onStatusToggle, true);
  }
  $("f-status").addEventListener("click", fleetAction(() => openStatusScreen(true)));
  $("f-pages").addEventListener("click", fleetAction(() => { location.hash = "#/pages"; }));
  $("f-usage").addEventListener("click", fleetAction(() => { location.hash = "#/usage"; }));
  $("f-peers").addEventListener("click", fleetAction(() => { location.hash = "#/peers"; }));
  $("f-settings").addEventListener("click", fleetAction(() => { location.hash = "#/settings"; }));
  $("f-notify").addEventListener("click", fleetAction(() => toggleNotifyPanel(true)));
  $("f-refresh").addEventListener("click", fleetAction(fleetRefresh));
  $("f-theme").addEventListener("click", fleetAction(() => toggleThemePanel(true)));
  initThemePanel();
  applyTheme();
  initFontPanel();
  applyFont();
  $("btn-fleet").addEventListener("keydown", (ev) => { if (ev.key === "ArrowDown" && $("f-menu").hidden) { ev.preventDefault(); toggleFleetMenu(true); } });
  $("d-model").addEventListener("click", openSwitch);
  initChatPick();
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
  testReady.then(() => {
    setInterval(relTick, pollMs(2000));
    setInterval(relBackground, pollMs(15000));
    setTimeout(relBackground, TEST_POLL_MS || 4000);
  });
  $("d-role").addEventListener("click", toggleRole);
  // The split review mode closes when the app goes somewhere else, and starts over when the layout changes.
  window.addEventListener("hashchange", () => { if (rsActive() && !(isDesktop() && location.hash === PAGE_HASH.todo) && !location.hash.startsWith("#/todo/")) { todos.openId = null; renderTodos(true); } });
  desktopMQ.addEventListener("change", () => { rsTeardown(); renderTodos(true); });
  window.addEventListener("resize", rsPlace);
  for (const id of ["todo", "pt-body"]) {
    const el = $(id);
    if (!el) continue;
    el.addEventListener("click", onTodoClick);
    el.addEventListener("input", onTodoInput);
    el.addEventListener("change", onTodoChange);
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
