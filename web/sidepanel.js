/* Side panels beside an agent's chat: a Doc panel (the document the agent
 * declared with `comebye show`, or a file the user picks), a Diff panel
 * (what the agent's checkout changed), a Status panel (the hub's Status
 * page, the same one #/status shows full screen) and a Pages panel (the Pages
 * this agent published, app.js's agentPages, and one of them open in the same
 * sandboxed frame, with the same comments, as the Pages screen). Doc and Diff are read through
 * the hub from the agent's own host (GET <agent>/file and <agent>/diff); this
 * file only draws them. Status is drawn by app.js's renderStatus into #sp-st,
 * so it stays live on the hub's events like the full-screen page. Pages come
 * from the Pages screen's own list (app.js's pagesLoad, GET /api/pages), read
 * again while the tab is shown as often as the Pages screen reads it.
 * Desktop: a split beside the chat (drag the divider; D toggles it);
 * phone: a bottom sheet with Doc, Diff, Status and Pages tabs.
 *
 * Uses app.js's globals: $, esc, escAttr, api, agentById, agentPath, state,
 * toast, renderMarkdown. Everything from the host is untrusted text: it goes
 * through esc() or the sanitizing renderMarkdown(), never into innerHTML raw.
 */
(function () {
  "use strict";
  const POLL_MS = 3000;
  // While the push stream is up, a poll is only a safety net.
  const SAFETY_MS = 30000;
  // Side by side needs about this much width in the body (two code columns).
  const SPLIT_MIN = 760;
  // The Doc panel renders Markdown up to the host's own 1 MB cap (chat bubbles stop at MD_MAX_CHARS).
  const DOC_MD_MAX = 1100000;
  const reduced = () => matchMedia("(prefers-reduced-motion:reduce)").matches;
  const TABS = ["doc", "diff", "status", "pages"];
  // The Pages tab reads the hub's Pages list as often as the Pages screen does (every 20 s; faster in test mode).
  const PAGES_MS = 20000;

  const st = {
    id: null,          // the selected agent's id
    open: false,
    tab: "doc",
    timer: null,
    es: null,          // EventSource of live pushes (panel/events)
    esKey: "",
    live: false,       // the push stream is connected: polls slow down
    fetchedAt: 0,
    split: false,      // the user's side-by-side choice (shown only when the panel is wide)
    wide: false,       // the panel body is wide enough for two columns
    // doc
    pick: null,        // a path the user chose (null: follow the agent's document)
    doc: null,         // last answer of /file for the shown document
    pin: null,         // the agent's declared document {path, section, at}
    pinSeen: "",       // pin.at already scrolled to
    list: null,        // the worktree's documents, once asked for
    docErr: "",
    docAt: 0,          // when the shown content last changed
    // diff
    diff: null,
    diffErr: "",
    diffAt: 0,
    closed: new Set(), // collapsed files (section|path)
    opened: new Set(), // files opened by the user although they started collapsed
    known: new Set(),  // files already seen (so a generated file starts collapsed once, not on every poll)
    loaded: new Map(), // "Load diff" answers: section|path -> the file, in full
    loading: new Set(), // fetches in flight
    loadErr: new Map(), // section|path -> why a fetch failed
    busy: false,
    again: false,      // a click arrived while a load was in flight: run it when that one ends
    // pages
    pg: null,          // the id of the Page open in the panel (null: the list)
    pgVer: 0,          // the version its frame shows (the newest when it was opened)
    pgKey: "",         // id/version of the frame now in the body ("" when there is none)
    pgLoading: false,  // a read of the Pages list is in flight
  };

  // ---- per-agent memory of the panel (open, tab, width) ----
  const memKey = () => "cb.sp." + st.id;
  function recall() {
    try { return JSON.parse(localStorage.getItem(memKey()) || "{}") || {}; } catch { return {}; }
  }
  function remember(patch) {
    if (!st.id) return;
    try { localStorage.setItem(memKey(), JSON.stringify({ ...recall(), ...patch })); } catch {}
  }

  function inject() {
    const det = $("detail");
    if (!det || $("sp")) return;
    const btn = document.createElement("button");
    btn.id = "sp-btn"; btn.type = "button"; btn.className = "more"; btn.setAttribute("aria-pressed", "false");
    btn.setAttribute("aria-label", "Side panel: document, diff, status and pages"); btn.title = "Document, diff, status and pages (D)";
    btn.innerHTML = '<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M14 4v16" stroke="currentColor" stroke-width="2"/></svg>';
    $("d-refresh").before(btn);
    const sp = document.createElement("aside");
    sp.id = "sp"; sp.setAttribute("aria-label", "Side panel");
    sp.innerHTML = `<div id="sp-grip" role="separator" aria-orientation="vertical" aria-label="Resize the panel" aria-valuemin="25" aria-valuemax="70" tabindex="0"></div>
      <div class="sp-tabs" role="tablist" aria-label="Side panel">
        <button type="button" class="sp-tab" role="tab" id="sp-tab-doc" data-tab="doc">Doc</button>
        <button type="button" class="sp-tab" role="tab" id="sp-tab-diff" data-tab="diff">Diff<span class="sp-badge" id="sp-n" hidden></span></button>
        <button type="button" class="sp-tab" role="tab" id="sp-tab-status" data-tab="status">Status</button>
        <button type="button" class="sp-tab" role="tab" id="sp-tab-pages" data-tab="pages">Pages<span class="sp-badge" id="sp-pgn" hidden></span></button>
        <button type="button" class="sp-x" aria-label="Close the panel">&times;</button>
      </div>
      <div class="sp-bar" id="sp-bar"></div>
      <div class="sp-body" id="sp-body" tabindex="-1" role="tabpanel"></div>`;
    det.append(sp);
    btn.addEventListener("click", () => toggle());
    sp.querySelector(".sp-x").addEventListener("click", () => toggle(false));
    sp.querySelectorAll(".sp-tab").forEach((t) => t.addEventListener("click", () => setTab(t.dataset.tab)));
    sp.querySelector(".sp-tabs").addEventListener("keydown", (e) => {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      e.preventDefault();
      setTab(TABS[(TABS.indexOf(st.tab) + (e.key === "ArrowRight" ? 1 : TABS.length - 1)) % TABS.length]);
      $("sp-tab-" + st.tab).focus();
    });
    sp.addEventListener("keydown", (e) => { if (e.key === "Escape" && st.open) { e.stopPropagation(); toggle(false); $("sp-btn").focus(); } });
    bindBar($("sp-bar"));
    bindBody($("sp-body"));
    // Status in the panel answers clicks as the Status page does (app.js's handlers), only inside #sp-st.
    $("sp-body").addEventListener("click", (e) => { if (e.target.closest && e.target.closest("#sp-st") && window.onStatusClick) window.onStatusClick(e); });
    $("sp-body").addEventListener("toggle", (e) => { if (e.target.closest && e.target.closest("#sp-st") && window.onStatusToggle) window.onStatusToggle(e); }, true);
    // A card in the Pages tab opens its Page here (a modified or middle click keeps the link's own behaviour: #/pages/<id>).
    $("sp-body").addEventListener("click", (e) => {
      const c = e.target.closest && e.target.closest("#sp-pgl a.pgs-card");
      if (!c || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      openPg(c.dataset.id);
    });
    bindGrip($("sp-grip"), det);
    if (typeof ResizeObserver !== "undefined") { // two columns only when the body is wide enough
      new ResizeObserver(() => {
        const w = $("sp-body").clientWidth >= SPLIT_MIN;
        if (w === st.wide) return;
        st.wide = w;
        if (st.open && st.tab === "diff") { renderBar(); if (st.split) renderBody({ keepScroll: true }); }
      }).observe($("sp-body"));
    }
    document.addEventListener("visibilitychange", () => { if (document.hidden) closeStream(); else tick(true); });
  }

  // ---- the divider (desktop) ----
  function setWidth(det, pct) {
    pct = Math.min(70, Math.max(25, pct));
    det.style.setProperty("--sp-w", pct + "%");
    $("sp-grip").setAttribute("aria-valuenow", String(Math.round(pct)));
    return pct;
  }
  function bindGrip(grip, det) {
    grip.addEventListener("pointerdown", (e) => {
      grip.setPointerCapture(e.pointerId);
      det.classList.add("sp-drag");
      const move = (m) => { const r = det.getBoundingClientRect(); setWidth(det, ((r.right - m.clientX) / r.width) * 100); };
      const up = () => {
        grip.removeEventListener("pointermove", move);
        grip.removeEventListener("pointerup", up);
        grip.removeEventListener("pointercancel", up);
        det.classList.remove("sp-drag");
        remember({ w: parseFloat(det.style.getPropertyValue("--sp-w")) || 44 });
      };
      grip.addEventListener("pointermove", move);
      grip.addEventListener("pointerup", up);
      grip.addEventListener("pointercancel", up);
    });
    grip.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
      e.preventDefault();
      const cur = parseFloat(det.style.getPropertyValue("--sp-w")) || 44;
      remember({ w: setWidth(det, cur + (e.key === "ArrowLeft" ? 3 : -3)) });
    });
  }

  // ---- open / close / tabs ----
  function toggle(on) {
    st.open = on === undefined ? !st.open : !!on;
    remember({ open: st.open });
    sync();
    if (st.open) tick(true); else stop();
  }
  // show: open the panel on one tab (the command palette's "Status beside the chat").
  function show(t) {
    if (!st.id || !TABS.includes(t)) return;
    st.tab = t;
    remember({ tab: t });
    toggle(true);
  }
  function setTab(t) {
    if (t === st.tab) return;
    st.tab = t;
    remember({ tab: t });
    sync();
    tick(true);
  }
  function sync() {
    const det = $("detail");
    det.classList.toggle("sp-open", st.open);
    $("sp-btn").setAttribute("aria-pressed", String(st.open));
    $("sp-btn").hidden = !st.id;
    for (const t of $("sp").querySelectorAll(".sp-tab")) {
      const on = t.dataset.tab === st.tab;
      t.setAttribute("aria-selected", String(on));
      t.tabIndex = on ? 0 : -1;
    }
    $("sp-body").setAttribute("aria-labelledby", "sp-tab-" + st.tab);
    if (st.open) { renderBar(); renderBody(); }
    if (st.open) start();
  }

  // The app tells us which agent is shown (null: none).
  function onAgent(id) {
    if (id === st.id) return;
    stop();
    // Status is not about one agent: open beside one agent's chat, it stays open on the next agent's (an "open" link in it,
    // j and k), and that agent remembers it. Doc and Diff are each agent's own and follow that agent's memory.
    const carry = !!id && !!st.id && st.open && st.tab === "status";
    st.id = id;
    Object.assign(st, { pick: null, doc: null, pin: null, pinSeen: "", list: null, docErr: "", diff: null, diffErr: "", closed: new Set(), opened: new Set(), known: new Set(), loaded: new Map(), loading: new Set(), loadErr: new Map() });
    newSources();
    const det = $("detail");
    if (!id) { st.open = false; det.classList.remove("sp-open"); $("sp-btn").hidden = true; return; }
    const m = carry ? { ...recall(), open: true, tab: "status" } : recall();
    if (carry) remember({ open: true, tab: "status" });
    st.open = !!m.open;
    st.tab = TABS.includes(m.tab) ? m.tab : "doc";
    st.pg = typeof m.pg === "string" && m.pg ? m.pg : null; st.pgVer = 0; st.pgKey = "";
    st.split = !!m.split;
    det.style.setProperty("--sp-w", (m.w || 44) + "%");
    $("sp-grip").setAttribute("aria-valuenow", String(Math.round(m.w || 44)));
    sync();
    if (st.open) tick(true);
  }

  // Has the hub answered for this agent's Doc and Diff yet (app.js's srcNew)? Until then the tab shows a skeleton,
  // then "Can't reach the hub": never "No document yet" or "No changes yet" before an answer says so.
  function newSources() {
    const redraw = () => { if (st.open) renderBody(); };
    st.docSrc = srcNew("sp-doc", () => tick(true), redraw, true);
    st.diffSrc = srcNew("sp-diff", () => tick(true), redraw, true);
  }
  newSources();
  // liveTag is the bar's "live": only while the body really is live. Not while the hub is out of reach (app.js's hubLost),
  // the last read failed, or nothing has come yet.
  function liveTag(src, err, okText) {
    const lost = window.hubLost && hubLost();
    const word = lost ? (navigator.onLine === false ? "offline" : "not updating") : err || srcDown(src) || srcStale(src) ? "not updating" : !src.at ? "waiting" : okText;
    return `<span class="live${word === okText ? "" : " off"}" id="sp-live" data-push="${st.live ? 1 : 0}">${esc(word)}</span>`;
  }
  // An answer from the hub (a 4xx is one) settles the wait; no answer, before any, fails it.
  function srcAfter(s, e) {
    if (e && e.status >= 400 && e.status < 500) srcOk(s);
    else if (e && !s.at) srcFail(s, e);
    else if (!e) srcOk(s);
  }

  // ---- polling: every 3 s while the panel is open, the shown tab only ----
  function start() { if (!st.timer && st.open && st.id) st.timer = setInterval(() => tick(false), window.pollMs ? window.pollMs(POLL_MS, 1000) : POLL_MS); }
  function stop() { clearInterval(st.timer); st.timer = null; closeStream(); }

  // ---- live push: the hub tells us when the checkout or the document changed ----
  function closeStream() {
    if (st.es) { st.es.onopen = st.es.onerror = null; st.es.close(); }
    st.es = null; st.esKey = "";
    if (st.live) { st.live = false; renderBar(); }
  }
  function syncStream() {
    const a = st.id && agentById(st.id);
    if (!st.open || !a || document.hidden || typeof EventSource === "undefined") { closeStream(); return; }
    const key = st.id + "|" + (st.pick || "");
    if (st.es && st.esKey === key) return;
    closeStream();
    st.esKey = key;
    const es = new EventSource(`${agentPath(a)}/panel/events` + (st.pick ? "?path=" + encodeURIComponent(st.pick) : ""));
    st.es = es;
    es.addEventListener("ready", () => { if (st.es === es) { st.live = true; renderBar(); tick(true); } }); // the hub has its baseline now; look once, in case a change landed before it
    // A push is a reason to look now; the answer is still the usual cheap "unchanged?" read.
    es.addEventListener("diff", () => { if (st.es === es && st.tab === "diff") tick(true); });
    es.addEventListener("doc", () => { if (st.es === es && st.tab === "doc") tick(true); });
    es.onerror = () => { if (st.es === es && st.live) { st.live = false; renderBar(); } }; // EventSource reconnects by itself; the 3 s poll covers the gap
  }

  async function tick(now) {
    syncStream();
    if (!st.open || !st.id || document.hidden) return;
    if (st.tab === "status") return; // drawn from the app's own state (renderStatus), which the hub's events keep current
    if (st.tab === "pages") { loadPages(now); return; }
    // With the push stream up, a poll is only a safety net.
    if (!now && st.live && Date.now() - st.fetchedAt < SAFETY_MS) return;
    // A load is in flight. A poll just waits for the next one, but a click (a
    // tab, another file, another agent: now) must not be lost until the next
    // poll 3 s later, so it runs the moment this load ends.
    if (st.busy) { if (now) st.again = true; return; }
    const a = agentById(st.id);
    if (!a) return;
    st.busy = true;
    st.fetchedAt = Date.now();
    const id = st.id;
    srcStart(st.tab === "diff" ? st.diffSrc : st.docSrc);
    try {
      if (st.tab === "diff") await loadDiff(a, id, now);
      else await loadDoc(a, id, now);
    } finally {
      st.busy = false;
      if (st.again) { st.again = false; tick(true); }
    }
  }

  // ---- pages: the Pages screen's list (pgs.items), read again when it is older than the Pages screen would let it get ----
  function loadPages(force) {
    if (st.pgLoading || !window.pagesLoad) return;
    const age = Date.now() - (pgs.at || 0);
    if (!force && pgs.loaded && age < (window.pollMs ? window.pollMs(PAGES_MS, 1000) : PAGES_MS)) return;
    if (!pgs.loaded) renderBody();
    st.pgLoading = true;
    window.pagesLoad().finally(() => { st.pgLoading = false; }); // pagesLoad calls pagesChanged when the list is in
  }
  // app.js's pagesLoad read the list (for the Pages screen, the review split or this tab): draw what changed.
  function pagesChanged() {
    badge();
    if (!st.open || st.tab !== "pages" || !st.id) return;
    renderBar();
    renderBody({ keepScroll: true });
  }
  const mine = () => (st.id && window.agentPages ? window.agentPages(agentById(st.id)) : []);
  function badge() {
    const n = $("sp-pgn");
    if (!n) return;
    const k = st.id && pgs.loaded ? mine().length : 0;
    n.textContent = String(k);
    n.hidden = !k;
  }
  function openPg(id) {
    st.pg = id; st.pgVer = 0;
    remember({ pg: id });
    renderBar(); renderBody();
  }
  function closePg() {
    const id = st.pg;
    st.pg = null; st.pgVer = 0;
    remember({ pg: null });
    renderBar(); renderBody();
    const c = [...document.querySelectorAll("#sp-pgl a.pgs-card")].find((x) => x.dataset.id === id);
    (c || $("sp-body")).focus({ preventScroll: !c });
  }
  // The comments strip and tray follow the frame they were opened for: when the Pages screen or the review split had them, and
  // that viewer is gone, the panel's Page takes them back.
  function pageRefocus() {
    if (!st.open || st.tab !== "pages" || !st.pg || !st.pgKey || !$("sp-body").querySelector("iframe.pgs-frame")) return;
    const p = pgs.items.find((x) => x.id === st.pg);
    if (p && window.pgcOpen) window.pgcOpen(p, st.pgVer);
  }

  async function loadDiff(a, id, force) {
    try {
      const d = await api(`${agentPath(a)}/diff` + (!force && st.diff ? `?since=${encodeURIComponent(st.diff.sig)}` : ""));
      if (id !== st.id) return;
      st.diffErr = "";
      srcAfter(st.diffSrc);
      if (d.unchanged) { renderBar(); return; }
      d.sections = (d.sections || []).map((s) => ({ ...s, files: s.files || [] })); // an empty section may arrive as null
      const first = !st.diff;
      st.diff = d;
      st.diffAt = Date.now();
      if (first) { // many files: start collapsed so the list reads as an outline
        const files = (d.sections || []).reduce((n, s) => n + s.files.length, 0);
        if (files > 8) for (const s of d.sections) for (const f of s.files) st.closed.add(s.key + "|" + f.path);
      }
      for (const s of d.sections) for (const f of s.files) { // generated files start collapsed, once
        const k = s.key + "|" + f.path;
        if (!st.known.has(k)) { st.known.add(k); if (f.generated) st.closed.add(k); }
      }
      refreshLoaded(a, id);
      renderBar();
      renderBody();
    } catch (e) {
      if (id !== st.id) return;
      st.diffErr = e.message || "could not read the changes";
      srcAfter(st.diffSrc, e);
      renderBar();
      if (!st.diff) renderBody();
    }
  }

  async function loadDoc(a, id, force) {
    const q = new URLSearchParams();
    if (st.pick) q.set("path", st.pick);
    if (!force && st.doc && st.doc.sig) q.set("since", st.doc.sig);
    try {
      const d = await api(`${agentPath(a)}/file` + (q.toString() ? "?" + q : ""));
      if (id !== st.id) return;
      st.docErr = "";
      srcAfter(st.docSrc);
      const pinChanged = JSON.stringify(d.pin || null) !== JSON.stringify(st.pin);
      st.pin = d.pin || null;
      if (!d.doc) { // nothing declared
        if (st.doc) { st.doc = null; }
        await ensureList(a, id);
        renderBar(); renderBody();
        return;
      }
      const same = d.doc.unchanged;
      if (!same) { st.doc = { ...d.doc, rel: d.rel || "" }; st.docAt = Date.now(); }
      renderBar();
      if (!same || pinChanged) renderBody({ keepScroll: !pinChanged || same });
    } catch (e) {
      if (id !== st.id) return;
      st.docErr = e.message || "could not read the document";
      srcAfter(st.docSrc, e);
      if (st.pick && (e.status === 403 || e.status === 404 || e.status === 413 || e.status === 415)) { st.pick = null; st.doc = null; }
      renderBar(); renderBody();
    }
  }

  async function ensureList(a, id) {
    if (st.list) return;
    try {
      const d = await api(`${agentPath(a)}/file?list=1`);
      if (id === st.id) st.list = d.files || [];
    } catch { if (id === st.id) st.list = []; }
  }

  // ---- bar ----
  const ago = (t) => {
    const s = Math.max(0, Math.round((Date.now() - t) / 1000));
    return s < 5 ? "just now" : s < 60 ? s + " s ago" : s < 3600 ? Math.round(s / 60) + " min ago" : Math.round(s / 3600) + " h ago";
  };

  // ---- "Load diff": one file the panel left out, fetched on its own ----
  const findFile = (key) => {
    const [sec, ...rest] = key.split("|"), p = rest.join("|");
    const s = st.diff && st.diff.sections.find((x) => x.key === sec);
    return s ? s.files.find((x) => x.path === p) : null;
  };
  async function loadFile(key) {
    const a = agentById(st.id), id = st.id, f = findFile(key);
    if (!a || !f || st.loading.has(key)) return;
    const sec = key.split("|")[0];
    st.loading.add(key); st.loadErr.delete(key);
    renderBody({ keepScroll: true });
    try {
      const q = new URLSearchParams({ file: f.path, section: sec });
      if (f.old_path) q.set("old", f.old_path);
      const r = await api(`${agentPath(a)}/diff?${q}`);
      if (id !== st.id) return;
      st.loaded.set(key, r.file);
    } catch (e) {
      if (id !== st.id) return;
      st.loadErr.set(key, e.message || "could not load this file");
    } finally {
      if (id === st.id) { st.loading.delete(key); renderBody({ keepScroll: true }); }
    }
  }
  // The checkout changed: what was loaded is read again (the old text stays until the new arrives).
  function refreshLoaded(a, id) {
    for (const key of [...st.loaded.keys()]) {
      const f = findFile(key);
      if (!f || !f.too_big) { st.loaded.delete(key); continue; }
      const sec = key.split("|")[0], q = new URLSearchParams({ file: f.path, section: sec });
      if (f.old_path) q.set("old", f.old_path);
      api(`${agentPath(a)}/diff?${q}`).then((r) => {
        if (id === st.id && st.loaded.has(key)) { st.loaded.set(key, r.file); renderBody({ keepScroll: true }); }
      }).catch(() => {});
    }
  }
  const kb = (n) => n >= 1024 * 1024 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";

  function renderBar() {
    const bar = $("sp-bar");
    if (!bar || !st.open) return;
    const n = $("sp-n");
    const files = st.diff ? st.diff.files : 0;
    n.textContent = String(files);
    n.hidden = !st.diff || !files;
    let html;
    if (st.tab === "pages") {
      badge();
      const p = st.pg && pgs.items.find((x) => x.id === st.pg);
      if (p) {
        const ver = st.pgVer || p.version;
        html = `<button type="button" id="sp-pgback" aria-label="Back to this agent's Pages">‹ Pages</button><span class="sp-tot sp-pgt" title="${escAttr(p.title)}">${esc(p.title)}</span>` +
          (ver < p.version ? `<button type="button" id="sp-pgnew" title="Show the newest version">v${p.version} is out · reload</button>` : `<span class="pgs-pill cur" id="sp-pgv">v${ver}</span>`) +
          `<button type="button" id="sp-pgfull" title="Open this Page on the Pages screen">Full screen</button>`;
      } else {
        const n = pgs.loaded ? mine().length : 0;
        html = `<span class="sp-tot">${n ? `<b>${n} page${n === 1 ? "" : "s"}</b> <span class="muted">this agent published</span>` : `<span class="muted">Pages this agent publishes</span>`}</span>${liveTag(pagesSrc, pgs.err || pgs.pollErr, "live")}`;
      }
    } else if (st.tab === "status") {
      // Full screen is the desktop's Status page; on a phone the sheet is nearly that already.
      const lost = hubLost() || srcStale(ledgerSrc);
      html = `<span class="sp-tot muted">The hub's Status${ledgerSrc.at && !lost ? ", live" : ""}</span>${lost || !ledgerSrc.at ? liveTag(ledgerSrc, "", "live") : ""}` + (isDesktop() ? `<button type="button" id="sp-full" title="Open the Status page in the main pane (g s)">Full screen</button>` : "");
    } else if (st.tab === "doc") {
      const opts = docOptions();
      const heads = headingEls().filter((h) => /^H[1-4]$/.test(h.tagName));
      html = `<select id="sp-file" aria-label="Document">${opts}</select>
        <select id="sp-toc" aria-label="Jump to a section"${heads.length ? "" : " disabled"}><option value="">Sections…</option>${heads.map((h, i) => `<option value="${i}">${esc("\u00a0\u00a0".repeat(Math.max(0, +h.tagName[1] - 1)) + hText(h))}</option>`).join("")}</select>
        <form id="sp-path-form" class="sp-pathf"><input id="sp-path" type="text" inputmode="url" autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="Open a file: docs/plan.md" aria-label="Path of a file in the worktree"><button type="submit">Open</button></form>
        <button type="button" id="sp-cpfile"${st.doc ? "" : " disabled"}>Copy file</button><button type="button" id="sp-cppath"${st.doc ? "" : " disabled"}>Copy path</button>
        ${liveTag(st.docSrc, st.docErr, st.doc ? "live · " + (Date.now() - st.docAt < 4000 ? "just updated" : "checked " + ago(st.docAt)) : "live")}`;
    } else {
      const d = st.diff;
      const flat = d ? d.sections.flatMap((s) => s.files.map((f) => ({ key: s.key + "|" + f.path, path: f.path }))) : [];
      const seen = new Set();
      const jump = flat.filter((f) => !seen.has(f.path) && seen.add(f.path));
      html = d && !d.not_git
        ? `<span class="sp-tot"><b>${d.files} file${d.files === 1 ? "" : "s"}</b> <span class="ad">+${d.add}</span> <span class="dl">−${d.del}</span>${d.base ? ` <span class="muted">vs ${esc(d.base)}</span>` : ""}</span>
          <select id="sp-jump" aria-label="Jump to a file"${jump.length ? "" : " disabled"}><option value="">Files…</option>${jump.map((f) => `<option value="${escAttr(f.key)}">${esc(f.path)}</option>`).join("")}</select>
          <button type="button" id="sp-fold" ${jump.length ? "" : "disabled"}>${st.diff && allClosed() ? "Expand all" : "Collapse all"}</button>
          ${st.wide ? `<button type="button" id="sp-split" aria-pressed="${st.split}" title="Old and new text in two columns">Side by side</button>` : ""}
          ${liveTag(st.diffSrc, st.diffErr, "live")}`
        : `<span class="sp-tot muted">${st.diffErr ? esc(st.diffErr) : "Changes in this agent's checkout"}</span>${liveTag(st.diffSrc, st.diffErr, "live")}`;
    }
    const focus = document.activeElement && document.activeElement.closest("#sp-bar") ? document.activeElement.id : "";
    const pathVal = $("sp-path") ? $("sp-path").value : "";
    if (bar.dataset.html === html) return;
    bar.dataset.html = html;
    patchChildren(bar, html);
    if ($("sp-path")) $("sp-path").value = pathVal;
    if (focus && $(focus)) $(focus).focus();
  }
  // The bar is patched, not rebuilt (app.js's patchHTML): every control stays the same element. Rebuilding the whole bar
  // (the push stream coming up flips "live", the panel widening adds Side by side) while someone was choosing from a menu
  // in it dropped the choice: the change landed on a select no longer in the page, so a pick from Files went nowhere.
  function patchChildren(parent, html) { window.patchHTML(parent, html); }

  function docOptions() {
    const rows = [];
    const cur = st.pick || (st.pin && st.pin.path) || "";
    if (st.pin) rows.push(`<option value="" ${!st.pick ? "selected" : ""}>★ ${esc(base(st.pin.path))} (agent's)</option>`);
    else rows.push(`<option value="" ${!st.pick && !st.doc ? "selected" : ""}>${st.list && st.list.length ? "Pick a file…" : "No document"}</option>`);
    const seen = new Set(st.pin ? [st.pin.path] : []);
    if (st.pick && !seen.has(st.pick)) { rows.push(`<option value="${escAttr(st.pick)}" selected>${esc(st.pick)}</option>`); seen.add(st.pick); }
    for (const f of st.list || []) { if (!seen.has(f.path)) { rows.push(`<option value="${escAttr(f.path)}">${esc(f.path)}</option>`); seen.add(f.path); } }
    return rows.join("");
  }
  const base = (p) => String(p).split("/").pop();
  const allClosed = () => st.diff && st.diff.sections.every((s) => s.files.every((f) => st.closed.has(s.key + "|" + f.path)));

  function bindBar(bar) {
    bar.addEventListener("change", async (e) => {
      const t = e.target;
      if (t.id === "sp-file") {
        st.pick = t.value || null;
        st.doc = null;
        tick(true);
      } else if (t.id === "sp-toc") {
        const h = headingEls().filter((x) => /^H[1-4]$/.test(x.tagName))[+t.value];
        if (h) jumpTo(h);
        t.value = "";
      } else if (t.id === "sp-jump") {
        const [sec, ...rest] = t.value.split("|");
        jumpToFile(sec + "|" + rest.join("|"));
        t.value = "";
      }
    });
    bar.addEventListener("focusin", async (e) => {
      if (e.target.id === "sp-file" && st.list === null) {
        const a = agentById(st.id);
        if (a) { await ensureList(a, st.id); renderBar(); }
      }
    });
    bar.addEventListener("click", (e) => {
      if (e.target.closest("#sp-full")) { if (window.openPage) window.openPage("status"); return; }
      if (e.target.closest("#sp-pgback")) { closePg(); return; }
      if (e.target.closest("#sp-pgnew")) { st.pgVer = 0; renderBar(); renderBody(); return; }
      if (e.target.closest("#sp-pgfull") && st.pg) { const p = pgs.items.find((x) => x.id === st.pg); location.hash = window.pageHash(st.pg, p && st.pgVer && st.pgVer < p.version ? st.pgVer : 0); return; }
      if (e.target.id === "sp-split") {
        st.split = !st.split;
        remember({ split: st.split });
        renderBar(); renderBody({ keepScroll: true });
        return;
      }
      const cp = e.target.closest("#sp-cpfile, #sp-cppath");
      if (cp && st.doc) {
        const txt = cp.id === "sp-cpfile" ? st.doc.content || "" : st.doc.rel || st.doc.path || "";
        flashCopy(cp, txt);
        return;
      }
      if (e.target.id === "sp-fold" && st.diff) {
        const close = !allClosed();
        for (const s of st.diff.sections) for (const f of s.files) close ? st.closed.add(s.key + "|" + f.path) : st.closed.delete(s.key + "|" + f.path);
        st.opened.clear();
        renderBar(); renderBody({ keepScroll: true });
      }
    });
    bar.addEventListener("submit", (e) => {
      if (e.target.id !== "sp-path-form") return;
      e.preventDefault();
      const v = $("sp-path").value.trim();
      if (!v) return;
      st.pick = v; st.doc = null; $("sp-path").value = "";
      tick(true);
    });
  }

  // ---- body ----
  function renderBody(opts = {}) {
    const body = $("sp-body");
    if (!body || !st.open) return;
    const top = body.scrollTop;
    body.classList.toggle("sp-flush", st.tab === "pages" && !!st.pg && pgs.items.some((x) => x.id === st.pg));
    if (st.tab !== "pages") st.pgKey = "";
    if (st.tab === "pages") { pagesBody(body, opts, top); renderBar(); return; }
    if (st.tab === "status") {
      if (!$("sp-st")) { body._pgl = ""; body.innerHTML = '<div id="sp-st" class="sp-st"></div>'; }
      if (window.renderStatus) window.renderStatus();
      renderBar();
      return;
    }
    body._pgl = "";
    body.innerHTML = st.tab === "doc" ? docHtml() : diffHtml();
    if (st.tab === "doc") afterDoc(opts);
    else if (opts.keepScroll) body.scrollTop = top;
    if (opts.keepScroll) body.scrollTop = top;
    renderBar();
  }

  // The Pages tab: the list (the Pages screen's cards, in its rows layout), or one Page in the Pages screen's frame.
  function pagesBody(body, opts, top) {
    const p = st.pg && pgs.items.find((x) => x.id === st.pg);
    if (st.pg && !p && pgs.loaded && !pgs.err) { st.pg = null; remember({ pg: null }); } // deleted since
    if (p) {
      const ver = st.pgVer || p.version, key = p.id + "/" + ver;
      if (st.pgKey === key && body.querySelector("iframe.pgs-frame")) return; // the same frame: the page keeps its state
      st.pgVer = ver; st.pgKey = key;
      body._pgl = "";
      body.innerHTML = `<div class="sp-pgv"><div class="pgs-sel" id="pgs-sel" hidden></div><div class="pgs-body">${window.pageFrameHtml(p, ver)}<aside class="pgs-tray" id="pgs-tray" aria-label="Comments" hidden></aside></div></div>`;
      window.pageFrameWire(body);
      window.pgcOpen(p, ver);
      if (ver === p.version) window.pgsMarkOpened(p);
      return;
    }
    st.pgKey = "";
    let html;
    if (pgs.err) html = `<div class="empty" role="status">${esc(pgs.err)}</div>`;
    else if (!pgs.loaded) html = srcWaitHtml(pagesSrc);
    else {
      const list = mine();
      html = list.length ? `<div class="pgs-rows sp-pgl" id="sp-pgl" aria-label="Pages this agent published">${list.map((x) => window.pgsCardHtml(x, false)).join("")}</div>`
        : `<div class="empty sp-pg0" role="status"><b>No Pages from this agent yet.</b><br>When it runs <code>comebye publish FILE</code>, the page shows up here.</div>`;
    }
    if (body._pgl === html && body.querySelector("#sp-pgl, .empty")) return; // unchanged: no redraw, focus and scroll stay
    const f = document.activeElement, fid = f && body.contains(f) && f.dataset ? f.dataset.id : "";
    body._pgl = html;
    body.innerHTML = html;
    if (fid) { const again = [...body.querySelectorAll("#sp-pgl a.pgs-card")].find((x) => x.dataset.id === fid); if (again) again.focus({ preventScroll: true }); }
    if (opts.keepScroll) body.scrollTop = top; else body.scrollTop = 0;
    for (const el of body.querySelectorAll("#sp-pgl .pgs-thumb")) window.pgsWantPreview(el.dataset.prev);
    window.pgsFill();
  }

  function docHtml() {
    if (!st.doc && !st.docSrc.at) return srcWaitHtml(st.docSrc);
    if (st.docErr && !st.doc) return `<div class="empty" role="status">${esc(st.docErr)}</div>`;
    if (!st.doc) {
      const l = st.list;
      return `<div class="empty" role="status"><b>No document yet.</b><br>When the agent runs <code>comebye show FILE</code> its document opens here, live.${
        l && l.length ? " Or pick one of this worktree's files above." : ""}</div>`;
    }
    const d = st.doc;
    const sec = st.pin && !st.pick && st.pin.section ? st.pin.section : "";
    const cite = sec ? `<div class="cite">The agent points at <b>${esc(sec)}</b> <button type="button" data-act="cite">Show</button></div>` : "";
    let content;
    if (d.kind === "markdown") content = `<div class="doc md" id="sp-doc">${renderMarkdown(d.content || "", "sp:" + st.id + ":" + d.path, DOC_MD_MAX)}</div>`;
    else content = `<div class="doc md" id="sp-doc">${d.kind === "html" ? '<div class="note">HTML is shown as its source here.</div>' : ""}<pre class="src">${esc(d.content || "")}</pre></div>`;
    return cite + content + (st.docErr ? `<div class="empty" role="status">${esc(st.docErr)}</div>` : "");
  }

  const slug = (s) => s.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "-").replace(/^-+|-+$/g, "") || "section";
  const hText = (h) => [...h.childNodes].filter((n) => !(n.nodeType === 1 && n.classList.contains("anchor"))).map((n) => n.textContent).join("");
  function headingEls() { return [...document.querySelectorAll("#sp-doc h1,#sp-doc h2,#sp-doc h3,#sp-doc h4,#sp-doc h5,#sp-doc h6")]; }

  // After a document is drawn: anchors on the headings, the section the agent cites.
  function afterDoc(opts) {
    const seen = new Map();
    for (const h of headingEls()) {
      let id = slug(hText(h));
      const n = seen.get(id) || 0;
      seen.set(id, n + 1);
      if (n) id += "-" + n;
      h.id = "sp-h-" + id;
      h.setAttribute("tabindex", "-1");
      const a = document.createElement("a");
      a.className = "anchor"; a.href = "#" + h.id; a.setAttribute("aria-label", "Link to this section"); a.textContent = "#";
      a.addEventListener("click", (e) => { e.preventDefault(); jumpTo(h); });
      h.append(a);
    }
    const sec = st.pin && !st.pick ? st.pin.section : "";
    const cited = sec ? findHeading(sec) : null;
    headingEls().forEach((h) => h.classList.toggle("cited", h === cited));
    // A new declaration (or the first draw) scrolls to the cited section.
    const key = st.pin ? st.pin.at + "|" + st.pin.path + "|" + st.pin.section : "";
    if (cited && key !== st.pinSeen) jumpTo(cited, true);
    if (!opts.keepScroll && !cited) $("sp-body").scrollTop = 0;
    if (st.pin && !st.pick) st.pinSeen = key;
  }

  const norm = (s) => s.toLowerCase().replace(/#$/, "").replace(/^[#\s]+/, "").replace(/[\s.:)]+$/g, "").trim();
  function findHeading(want) {
    const w = norm(want);
    if (!w) return null;
    const hs = headingEls();
    const text = (h) => norm(hText(h));
    return hs.find((h) => text(h) === w) || hs.find((h) => text(h).startsWith(w)) || null;
  }

  function jumpTo(h, instant) {
    h.scrollIntoView({ block: "start", behavior: instant || reduced() ? "auto" : "smooth" });
    h.classList.remove("flash"); void h.offsetWidth; h.classList.add("flash");
    setTimeout(() => h.classList.remove("flash"), 1700);
  }

  // ---- diff ----
  function parseHunks(diff) {
    const hunks = [];
    let h = null, a = 0, b = 0;
    for (const line of diff.split("\n")) {
      if (line.startsWith("@@")) {
        const m = /^@@ -(\d+)(?:,\d+)? \+(\d+)/.exec(line);
        a = m ? +m[1] : 0; b = m ? +m[2] : 0;
        h = { head: line, rows: [] }; hunks.push(h);
      } else if (h && line.startsWith("\\")) {
        h.rows.push({ t: "\\", s: line.slice(1).trim() });
      } else if (h) {
        const t = line[0] || " ", s = line.slice(1);
        if (t === "+") h.rows.push({ t, s, b: b++ });
        else if (t === "-") h.rows.push({ t, s, a: a++ });
        else h.rows.push({ t: " ", s, a: a++, b: b++ });
      }
    }
    return hunks;
  }

  // ---- syntax colours (highlight.js, vendored; loaded the first time a diff is drawn) ----
  const LANG_EXT = { go: "go", js: "javascript", mjs: "javascript", cjs: "javascript", jsx: "javascript", ts: "typescript", tsx: "typescript", py: "python",
    sh: "bash", bash: "bash", zsh: "bash", json: "json", yml: "yaml", yaml: "yaml", md: "markdown", markdown: "markdown", css: "css", html: "xml", htm: "xml",
    xml: "xml", svg: "xml", diff: "diff", patch: "diff", rs: "rust", sql: "sql", mk: "makefile" };
  const LANG_NAME = { makefile: "makefile", dockerfile: "dockerfile", gnumakefile: "makefile" };
  const langOf = (path) => {
    const b = base(path).toLowerCase();
    if (LANG_NAME[b]) return LANG_NAME[b];
    const i = b.lastIndexOf(".");
    return i > 0 ? LANG_EXT[b.slice(i + 1)] || "" : "";
  };
  const hl = { lib: null, loading: null, langs: new Map(), cache: new Map() }; // langs: name -> true | "failed" | Promise
  function hlReady(lang) { return !!(hl.lib && hl.langs.get(lang) === true); }
  // Asks for the highlighter and the languages the shown files use; redraws once when they arrive.
  function hlWant(langs) {
    const need = [...langs].filter((l) => l && !hl.langs.has(l));
    if (!need.length) return;
    if (!hl.loading) hl.loading = import("./vendor/hljs/core.min.js").then((m) => { hl.lib = m.default; }).catch(() => { hl.lib = null; hl.failed = true; });
    for (const l of need) {
      hl.langs.set(l, "loading");
      hl.loading.then(() => hl.lib ? import(`./vendor/hljs/${l}.min.js`).then((m) => { hl.lib.registerLanguage(l, m.default); hl.langs.set(l, true); }) : null)
        .catch(() => hl.langs.set(l, "failed"))
        .then(() => { if (st.open && st.tab === "diff") renderBody({ keepScroll: true }); });
    }
  }
  // Highlighted HTML of text, one string per line, with spans closed and reopened at each line break.
  function hlLines(text, lang) {
    const html = hl.lib.highlight(text, { language: lang, ignoreIllegals: true }).value;
    const out = [], open = [];
    let cur = "";
    for (const part of html.split(/(<span[^>]*>|<\/span>|\n)/)) {
      if (part === "\n") { out.push(cur + "</span>".repeat(open.length)); cur = open.join(""); }
      else if (part.startsWith("<span")) { open.push(part); cur += part; }
      else if (part === "</span>") { open.pop(); cur += part; }
      else cur += part;
    }
    out.push(cur + "</span>".repeat(open.length));
    return out;
  }
  const MAX_HL_LINES = 1500;
  // Gives each row of a hunk its coloured text (r.h), or leaves it plain. Cached per hunk text.
  function colour(path, h) {
    const lang = langOf(path);
    if (!lang || !hlReady(lang) || h.rows.length > MAX_HL_LINES) return;
    const key = lang + "\n" + h.head + "\n" + h.rows.map((r) => r.t + r.s).join("\n");
    let hit = hl.cache.get(key);
    if (!hit) {
      try {
        const oldT = [], newT = [];
        for (const r of h.rows) { if (r.t === "\\") continue; if (r.t !== "+") oldT.push(r.s); if (r.t !== "-") newT.push(r.s); }
        hit = { o: hlLines(oldT.join("\n"), lang), n: hlLines(newT.join("\n"), lang) };
      } catch { hit = "plain"; }
      if (hl.cache.size > 300) hl.cache.clear();
      hl.cache.set(key, hit);
    }
    if (hit === "plain") return;
    let o = 0, n = 0;
    for (const r of h.rows) {
      if (r.t === "\\") continue;
      if (r.t === "-") r.h = hit.o[o++];
      else if (r.t === "+") r.h = hit.n[n++];
      else { r.h = hit.n[n++]; o++; }
    }
  }
  const cell = (r) => r.h !== undefined ? r.h : esc(r.s);

  // One hunk as rows. Unified: old and new numbers, one text column. Split: a removed run and
  // the added run that replaces it sit on the same rows, left and right.
  function hunkRows(h, split) {
    if (!split) {
      return h.rows.map((r) => r.t === "\\" ? `<div class="ln nl"><i></i><i></i><b>${esc(r.s)}</b></div>`
        : `<div class="ln ${r.t === "+" ? "add" : r.t === "-" ? "del" : ""}${r.h !== undefined ? " hl" : ""}"><i>${r.a ?? ""}</i><i>${r.b ?? ""}</i><b><u>${esc(r.t)}</u>${cell(r)}</b></div>`).join("");
    }
    const out = [], rows = h.rows.filter((r) => r.t !== "\\");
    const half = (r, side) => r
      ? `<i>${(side === "l" ? r.a : r.b) ?? ""}</i><b class="${r.t === "+" ? "add" : r.t === "-" ? "del" : ""}${r.h !== undefined ? " hl" : ""}">${cell(r)}</b>`
      : `<i></i><b class="void"></b>`;
    for (let i = 0; i < rows.length;) {
      if (rows[i].t === " ") { out.push(`<div class="sr">${half(rows[i], "l")}${half(rows[i], "r")}</div>`); i++; continue; }
      const dels = [], adds = [];
      while (i < rows.length && rows[i].t === "-") dels.push(rows[i++]);
      while (i < rows.length && rows[i].t === "+") adds.push(rows[i++]);
      for (let k = 0; k < Math.max(dels.length, adds.length); k++) out.push(`<div class="sr">${half(dels[k], "l")}${half(adds[k], "r")}</div>`);
    }
    return out.join("");
  }

  function diffHtml() {
    const d = st.diff;
    if (!d) return st.diffErr && st.diffSrc.at ? `<div class="empty" role="status">${esc(st.diffErr)}</div>` : srcWaitHtml(st.diffSrc);
    if (d.not_git) return `<div class="empty" role="status">This agent's directory is not a git checkout, so there is nothing to compare.</div>`;
    const sections = d.sections.filter((s) => s.files.length);
    if (!sections.length) return `<div class="empty" role="status">No changes yet. Edits, staged files and new files show up here within a few seconds.</div>`;
    const commits = d.commits && d.commits.length
      ? `<ul class="commits" aria-label="Commits since ${escAttr(d.base)}">${d.commits.map((c) => `<li><code>${esc(c.sha)}</code> ${esc(c.subject)}</li>`).join("")}</ul>` : "";
    return (d.note ? `<div class="note" role="status">${esc(d.note)}</div>` : "") +
      (d.redacted ? `<div class="redact" role="status">${d.redacted} secret${d.redacted === 1 ? "" : "s"} hidden in the text below.</div>` : "") +
      sections.map((s) => `<section class="ds" aria-label="${escAttr(s.title)}"><h3>${esc(s.title)} <span class="sp-badge">${s.files.length}</span></h3>${s.key === "commits" ? commits : ""}${
        s.files.map((f) => fileHtml(s.key, f)).join("")}</section>`).join("");
  }

  // The placeholder for a file whose text is not here, with the true reason.
  function skippedHtml(key, f) {
    const size = (f.bytes ? kb(f.bytes) + ", " : "") + f.lines + " lines";
    const err = st.loadErr.get(key) ? ` <span class="err">${esc(st.loadErr.get(key))}</span>` : "";
    const busy = st.loading.has(key);
    const btn = (label) => `<button type="button" class="ld" data-load="${escAttr(key)}"${busy ? " disabled" : ""}>${busy ? "Loading…" : label}</button>`;
    if (f.skip === "total") return `<div class="ph">Not loaded: the change set is large (over ${panelTotalKB} KB). ${size}. ${btn("Load diff")}${err}</div>`;
    if (f.skip === "load") return `<div class="ph">Too big to show even when loaded: ${size}.</div>`;
    return `<div class="ph">Too big for the panel (${size}). ${btn("Load anyway")}${err}</div>`;
  }
  const panelTotalKB = 768;

  function fileHtml(sec, f0) {
    const key = sec + "|" + f0.path;
    const f = f0.too_big && st.loaded.has(key) ? st.loaded.get(key) : f0; // the fetched file replaces the placeholder whole
    const open = !st.closed.has(key);
    const tag = f.status === "added" || f.status === "untracked" ? `<span class="tg nw">${f.status === "added" ? "new" : "untracked"}</span>`
      : f.status === "deleted" ? '<span class="tg dd">deleted</span>' : f.status === "renamed" ? '<span class="tg rn">renamed</span>' : "";
    let inner;
    if (f.withheld) inner = `<div class="ph">Contents withheld (${esc(f.withheld)}): ${f.lines} changed line${f.lines === 1 ? "" : "s"}.</div>`;
    else if (f.binary) inner = `<div class="ph">Binary file.</div>`;
    else if (f.too_big) inner = skippedHtml(key, f);
    else if (!f.diff) inner = `<div class="ph">No text changes.</div>`;
    else {
      const split = st.split && st.wide;
      if (open) hlWant([langOf(f.path)]);
      inner = parseHunks(f.diff).map((h, j) => { if (open) colour(f.path, h); return `<div class="hk"><span>${esc(h.head)}</span><button type="button" class="hk-cp" data-copyhunk="${escAttr(key)}:${j}" aria-label="Copy this hunk of ${escAttr(f.path)}">Copy</button><button type="button" data-send="${escAttr(key)}:${j}" aria-label="Send this hunk of ${escAttr(f.path)} to the agent">Send to agent</button></div><div class="code${split ? " split" : ""}">${hunkRows(h, split)}</div>`; }).join("");
    }
    return `<details class="df" data-key="${escAttr(key)}"${open ? " open" : ""}><summary><span class="p" title="${escAttr(f.path)}">${esc(f.path)}</span>${tag}<span class="ad">+${f.add}</span><span class="dl">−${f.del}</span></summary>${inner}</details>`;
  }

  function bindBody(body) {
    // A file's fold is kept the moment its header is clicked (or Enter/Space on it), not only on the toggle event: that
    // comes a task later, so a redraw in between lost the click, and a file just opened is drawn again (redrawFile)
    // with its own pending toggle, which the closing click's merges into, so the close was never kept and the next
    // redraw (the push stream, the poll) opened the file again. The click runs before the details flips.
    body.addEventListener("click", (e) => {
      const sum = e.target.closest && e.target.closest("details.df > summary");
      if (!sum || e.target.closest("button, a, select, input")) return;
      const el = sum.parentElement;
      if (el.open) st.closed.add(el.dataset.key); else st.closed.delete(el.dataset.key);
    }, true);
    body.addEventListener("toggle", (e) => {
      const el = e.target;
      if (!el.matches || !el.matches("details.df")) return;
      if (el.open) st.closed.delete(el.dataset.key); else st.closed.add(el.dataset.key);
      renderBar();
      if (el.open && !el.querySelector(".ln.hl, .sr .hl")) redrawFile(el);
    }, true);
    body.addEventListener("click", (e) => {
      const cite = e.target.closest("[data-act=cite]");
      if (cite) { const h = st.pin && findHeading(st.pin.section); if (h) jumpTo(h); return; }
      const ld = e.target.closest("[data-load]");
      if (ld) { loadFile(ld.dataset.load); return; }
      const send = e.target.closest("[data-send]");
      if (send) sendHunk(send.dataset.send);
      const cph = e.target.closest("[data-copyhunk]");
      if (cph) { const h = hunkOf(cph.dataset.copyhunk); if (h) flashCopy(cph, hunkBody(h.h)); }
    });
  }

  // A file drawn while closed has plain text and its language was never asked for; when it is opened,
  // draw just that file again so it is coloured now, not at the next change.
  function redrawFile(el) {
    const key = el.dataset.key, i = key.indexOf("|");
    const f0 = st.diff && st.diff.sections.filter((s) => s.key === key.slice(0, i)).flatMap((s) => s.files).find((x) => x.path === key.slice(i + 1));
    if (!f0 || !langOf(f0.path)) return;
    const focus = document.activeElement && el.contains(document.activeElement);
    const tpl = document.createElement("template");
    tpl.innerHTML = fileHtml(key.slice(0, i), f0);
    const fresh = tpl.content.firstElementChild;
    if (!fresh) return;
    el.replaceWith(fresh);
    if (focus) fresh.querySelector("summary").focus();
  }

  function jumpToFile(key) {
    st.closed.delete(key);
    const el = [...document.querySelectorAll("#sp-body details.df")].find((x) => x.dataset.key === key);
    if (!el) return;
    el.open = true;
    el.scrollIntoView({ block: "start", behavior: reduced() ? "auto" : "smooth" });
    el.classList.remove("flash"); void el.offsetWidth; el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1700);
  }

  // Copy: the button says Copied (or Not copied) for 1.5 s. copyText is app.js's.
  function flashCopy(btn, txt) {
    if (btn.dataset.label == null) btn.dataset.label = btn.textContent;
    copyText(txt).then((ok) => {
      clearTimeout(btn._t);
      btn.textContent = ok ? "Copied" : "Not copied";
      if (!ok) toast("Could not copy", true);
      btn._t = setTimeout(() => { btn.textContent = btn.dataset.label; }, 1500);
    });
  }
  function hunkOf(ref) {
    const i = ref.lastIndexOf(":");
    const key = ref.slice(0, i), j = +ref.slice(i + 1);
    const [sec, ...rest] = key.split("|");
    const f0 = st.diff && st.diff.sections.filter((s) => s.key === sec).flatMap((s) => s.files).find((x) => x.path === rest.join("|"));
    const f = f0 && st.loaded.has(key) ? st.loaded.get(key) : f0;
    const h = f && f.diff ? parseHunks(f.diff)[j] : null;
    return h ? { f, h } : null;
  }
  const hunkBody = (h) => h.head.replace(/ @@.*/, " @@") + "\n" + h.rows.map((r) => (r.t === "\\" ? "\\ " : r.t) + r.s).join("\n");

  function sendHunk(ref) {
    const x = hunkOf(ref);
    if (!x) return;
    const { f, h } = x;
    const m = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(h.head);
    const range = m ? (+(m[4] ?? 1) > 1 ? `lines ${m[3]}\u2013${+m[3] + +m[4] - 1}` : `line ${m[3]}`) : "";
    const body = hunkBody(h);
    const txt = `About this change in ${f.path}${range ? " (" + range + ")" : ""}:\n\`\`\`diff\n${body}\n\`\`\`\n\n`;
    const box = $("prompt-text");
    if (!box) return;
    // What the user already typed stays: the hunk goes after it.
    const had = box.textContent.trim();
    box.textContent = had ? box.textContent.replace(/\s*$/, "") + "\n\n" + txt : txt;
    box.dispatchEvent(new Event("input", { bubbles: true }));
    box.focus();
    const sel = getSelection(), r = document.createRange();
    r.selectNodeContents(box); r.collapse(false); sel.removeAllRanges(); sel.addRange(r);
    toast("Hunk added to the prompt");
    if (matchMedia("(max-width:1099px)").matches) toggle(false); // the sheet covers the prompt
  }

  // hubChanged: the live connection came or went (app.js's strip): the bar's "live" follows it.
  const hubChanged = () => { if (st.open && st.id) renderBar(); };
  window.SP = { toggle, show, onAgent, isOpen: () => st.open, statusShown: () => st.open && !!st.id && st.tab === "status" && !!$("sp-st"), pagesChanged, pageRefocus, hubChanged };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", inject); else inject();
})();
