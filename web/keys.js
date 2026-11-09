// The keyboard layer: a command palette (Ctrl/⌘+K), single-key shortcuts, g-chords
// and the cheat sheet. It loads after app.js and uses its globals (state, POPS,
// openDetail, openPage, ...). Nothing here fires while you type in an input;
// the only keys that work in one are Ctrl/⌘+K and Esc (which leaves the prompt box).
(function () {
  "use strict";

  const MAC = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent || "");
  const MOD = MAC ? "⌘" : "Ctrl";
  const G_MS = 1200; // how long "g" waits for its second key

  const typing = (el) => !!(el && el.closest && el.closest("input, textarea, select, [contenteditable]:not([contenteditable=false])"));
  const mods = (ev) => ev.ctrlKey || ev.metaKey || ev.altKey;

  /* ---------- the agents ---------- */

  const agentName = (a) => a.name || a.terminal_title_stripped || a.display_agent || a.agent || a.pane_id || agentId(a);
  const needsYou = (a) => a.agent_status === "blocked";
  const rows = () => [...document.querySelectorAll("#agents .agent[data-id]")].filter((r) => r.offsetParent);
  const cur = () => (state.selected ? agentById(state.selected) : null);

  function focusRow(id) {
    const r = rowFor(id);
    if (r) { r.focus({ preventScroll: true }); r.scrollIntoView?.({ block: "nearest" }); }
  }

  // moveAgent opens the previous or next agent in the list (j / k).
  function moveAgent(d) {
    const rs = rows();
    if (!rs.length) return;
    let i = rs.findIndex((r) => r.dataset.id === state.selected);
    i = i < 0 ? (d > 0 ? 0 : rs.length - 1) : Math.max(0, Math.min(rs.length - 1, i + d));
    openDetail(rs[i].dataset.id);
    focusRow(rs[i].dataset.id);
  }

  // nextNeeds opens the next (d = 1) or previous agent that is waiting on you (n / Shift+N).
  function nextNeeds(d) {
    const rs = rows().filter((r) => r.classList.contains("blocked"));
    if (!rs.length) { toast("No agent needs you"); return; }
    const i = rs.findIndex((r) => r.dataset.id === state.selected);
    const r = rs[i < 0 ? (d > 0 ? 0 : rs.length - 1) : (i + d + rs.length) % rs.length];
    openDetail(r.dataset.id);
    focusRow(r.dataset.id);
  }

  function jumpTo(n) {
    const r = rows()[n - 1];
    if (!r) return;
    openDetail(r.dataset.id);
    focusRow(r.dataset.id);
  }

  function focusPrompt() {
    if (!state.selected) { toast("Open an agent first"); return false; }
    const t = $("prompt-text");
    if (!t || !t.offsetParent) return false;
    t.focus();
    return true;
  }

  // leavePrompt takes the focus off the prompt box, onto the agent's row.
  function leavePrompt() {
    const t = $("prompt-text");
    if (t) t.blur();
    if (state.selected && rowFor(state.selected)) focusRow(state.selected);
  }

  function goSearch() {
    if (!rc.route) openRecall();
    else $("rc-q").focus();
    if (isDesktop()) setTimeout(() => $("rc-q").focus(), 0);
  }

  /* ---------- the actions (shared by the palette, the keys and the cheat sheet) ---------- */

  const hasLedger = () => ledger.on === true;
  const canRename = (a) => !!a && a.agent === "claude";

  async function renameAgent(a, name) {
    name = name.trim();
    if (!name) return;
    if (!canRename(a)) { toast("Rename is only available for Claude agents", true); return; }
    try {
      await api(`${pathFor(agentId(a))}/prompt`, { method: "POST", body: JSON.stringify({ text: "/rename " + name }) });
      toast("Renaming to " + name);
    } catch (e) { toast("Could not rename: " + e.message, true); }
  }

  async function chooseModel(a, model) {
    try {
      await api(`${pathFor(agentId(a))}/model`, { method: "POST", body: JSON.stringify({ model }) });
      toast("Switched to " + model);
    } catch (e) { toast(e.message, true); }
  }

  const pages = () => [
    { id: "go-status", label: "Status", keys: ["g", "s"], run: () => openPage("status"), show: () => ledger.on !== false },
    { id: "go-todo", label: "To do", keys: ["g", "t"], run: () => openPage("todo"), show: () => todos.on !== false },
    { id: "go-search", label: "Search", keys: ["/"], run: () => goSearch() },
    { id: "go-remove", label: "Ready to remove", keys: ["g", "r"], run: () => openReview(), show: () => ledger.on !== false },
    { id: "go-brief", label: "Briefing", keys: ["g", "b"], run: () => openPage("briefing"), show: () => ledger.on !== false && isDesktop() },
    { id: "go-agents", label: "Agent list (focus it)", keys: ["g", "a"], run: () => (rows()[0] && focusRow(state.selected && rowFor(state.selected) ? state.selected : rows()[0].dataset.id)) },
  ];

  // The current agent's actions. Stop and interrupt live here only, behind a confirm.
  const agentActions = (a) => {
    const nm = agentName(a);
    const marked = ledger.marked && ledger.marked.has(agentId(a));
    const L = [
      { id: "a-prompt", label: "Focus the prompt box", keys: ["i"], run: () => focusPrompt() },
    ];
    L.push({ id: "a-panel", label: "Show / hide the document and diff panel", keys: ["d"], run: () => window.SP && window.SP.toggle() });
    if (hasLedger()) {
      L.push({ id: "a-mark", label: (marked ? "Unmark " : "Mark ready to remove: ") + nm, keys: ["m"], run: () => toggleMark() });
      L.push({ id: "a-role", label: "Set role for " + nm + "…", step: "role" });
    }
    if (canRename(a)) L.push({ id: "a-rename", label: "Rename " + nm + "…", keys: ["r"], step: "rename" });
    else L.push({ id: "a-rename", label: "Rename " + nm + " (Claude agents only)", run: () => toast("Rename is only available for Claude agents", true) });
    if (switchable(a)) L.push({ id: "a-model", label: "Switch model for " + nm + "…", keys: ["Shift", "M"], step: "model" });
    L.push({ id: "a-int", label: "Interrupt " + nm + " (send Esc)…", step: "confirm-int" });
    L.push({ id: "a-stop", label: "Stop " + nm + " (send Ctrl+C)…", step: "confirm-stop" });
    return L;
  };

  const globalActions = () => [
    { id: "g-new", label: "New agent or handoff…", run: () => openLaunch() },
    { id: "g-theme", label: "Theme…", run: () => toggleThemePanel(true) },
    { id: "g-notify", label: "Notifications…", run: () => toggleNotifyPanel(true) },
    { id: "g-refresh", label: "Refresh", run: () => fleetRefresh() },
    { id: "g-hist", label: "Briefing history", run: () => openBriefs(true), show: () => ledger.on !== false },
    { id: "g-keys", label: "Keyboard shortcuts", keys: ["?"], run: () => openCheats() },
  ];

  /* ---------- fuzzy matching ---------- */

  // A subsequence match: every letter of q in order. Runs, word starts and early hits score higher.
  function fuzzy(q, s) {
    q = q.toLowerCase();
    const t = s.toLowerCase();
    if (!q) return { score: 0, idx: [] };
    const idx = [];
    let i = 0, score = 0, last = -2;
    for (let j = 0; j < t.length && i < q.length; j++) {
      if (t[j] !== q[i]) continue;
      score += 1;
      if (j === last + 1) score += 3;
      if (j === 0 || /[\s/:_\-.]/.test(t[j - 1])) score += 4;
      idx.push(j); last = j; i++;
    }
    if (i < q.length) return null;
    const at = t.indexOf(q);
    if (at >= 0) score += 10 - Math.min(at, 8);
    return { score: score - idx[idx.length - 1] * 0.05, idx };
  }

  function marked(text, idx) {
    const frag = document.createDocumentFragment();
    if (!idx || !idx.length) { frag.append(text); return frag; }
    const set = new Set(idx);
    let run = "", on = false;
    const flush = () => {
      if (!run) return;
      if (on) { const m = document.createElement("mark"); m.textContent = run; frag.append(m); } else frag.append(run);
      run = "";
    };
    for (let i = 0; i < text.length; i++) {
      const h = set.has(i);
      if (h !== on) { flush(); on = h; }
      run += text[i];
    }
    flush();
    return frag;
  }

  /* ---------- the palette ---------- */

  // A step is a second question the palette asks (a name, a model, a role, a confirm).
  // Esc goes back one step; at the first level it closes the palette.
  const pal = { open: false, from: null, step: null, items: [], sel: 0, seq: 0 };
  const statusLabel = { blocked: "needs you", working: "working", idle: "idle", done: "done", unknown: "" };

  function openPalette(prefill) {
    if (pal.open) return;
    pal.open = true;
    pal.from = document.activeElement && document.activeElement !== document.body ? document.activeElement : null;
    pal.step = null;
    pal.sel = 0;
    $("palette").hidden = false;
    popOpened("palette");
    $("pal-q").value = prefill || "";
    $("pal-q").placeholder = "Type a command, or an agent name…";
    $("pal-q").focus();
    buildPalette();
    syncMode();
  }

  // closePalette hides it. restore puts the focus back where it was before it opened.
  function closePalette(restore) {
    if (!pal.open) return;
    pal.open = false;
    pal.step = null;
    pal.seq++;
    $("palette").hidden = true;
    popClosed("palette");
    const f = pal.from;
    pal.from = null;
    if (restore !== false && f && f.isConnected) f.focus();
    else if (document.activeElement === $("pal-q")) $("pal-q").blur();
    syncMode();
  }

  // backPalette goes back one step and reports whether it did.
  function backPalette() {
    if (!pal.open) return false;
    if (!pal.step) return false;
    pal.step = null;
    pal.seq++;
    $("pal-q").value = "";
    $("pal-q").placeholder = "Type a command, or an agent name…";
    pal.sel = 0;
    buildPalette();
    return true;
  }

  function startStep(step) {
    pal.step = step;
    pal.sel = 0;
    pal.seq++;
    $("pal-q").value = step.value || "";
    $("pal-q").placeholder = step.title;
    buildPalette();
    $("pal-q").select();
    if (step.load) {
      const seq = pal.seq;
      step.load().then((items) => { if (seq === pal.seq && pal.step === step) { step.items = items; buildPalette(); } },
        (e) => { if (seq === pal.seq && pal.step === step) { step.items = []; step.err = e.message; buildPalette(); } });
    }
  }

  function stepFor(name, a) {
    const nm = agentName(a);
    const id = agentId(a);
    if (name === "rename") return { kind: "input", title: "Rename " + nm, value: nm, submit: (v) => { closePalette(); renameAgent(a, v); } };
    if (name === "role") {
      const coord = isCoordinator(a);
      return { kind: "choices", title: "Set role for " + nm, items: [
        { label: "worker", meta: coord ? "" : "current", run: () => { closePalette(); if (coord) toggleRole(); } },
        { label: "coordinator", meta: coord ? "current" : "", run: () => { closePalette(); if (!coord) toggleRole(); } },
      ] };
    }
    if (name === "model") {
      return { kind: "choices", title: "Switch model for " + nm, items: null, load: async () => {
        const d = await api(modelsBase(a.host || state.localHost) + "?kind=" + encodeURIComponent(a.agent));
        const cat = (d.catalogs || [])[0];
        if (!cat || !(cat.models || []).length) return [];
        return cat.models.map((m) => ({ label: m.id, meta: m.provider || "", run: () => { closePalette(); chooseModel(a, m.id); } }));
      } };
    }
    const keys = name === "confirm-int" ? ["esc"] : ["ctrl+c"];
    const verb = name === "confirm-int" ? "Interrupt" : "Stop";
    const note = name === "confirm-int" ? "Sends Esc to the agent, which stops what it is doing." : "Sends Ctrl+C to the agent, which can end its session.";
    return { kind: "choices", title: verb + " " + nm + "?", note, items: [
      { label: verb + " " + nm, meta: "confirm", danger: true, run: () => { closePalette(); if (state.selected === id) sendKeys(keys, verb); else toast("That agent is no longer open", true); } },
      { label: "Cancel", meta: "", run: () => backPalette() },
    ] };
  }

  function parseQuery(raw) {
    const m = raw.match(/^([>@#])\s?/);
    return m ? { p: m[1], q: raw.slice(m[0].length) } : { p: "", q: raw };
  }

  // buildPalette fills the list for the current query or step.
  function buildPalette() {
    if (!pal.open) return;
    const raw = $("pal-q").value;
    const pfx = $("pal-pfx");
    const foot = $("pal-foot");
    const list = $("pal-list");
    const step = pal.step;
    pfx.hidden = true;
    const groups = [];
    let hint = "";
    if (step && step.kind === "input") {
      list.replaceChildren(note(step.title + ": press Enter to save, Esc to go back."));
      pal.items = [];
      foot.textContent = "";
      foot.append(keyHint("Enter", "save"), keyHint("Esc", "back"));
      return;
    }
    if (step) {
      if (step.items === null) { list.replaceChildren(note("Loading…")); pal.items = []; foot.textContent = ""; foot.append(keyHint("Esc", "back")); return; }
      const items = (step.items || []).map((it) => { const f = fuzzy(raw, it.label); return f ? { ...it, kind: "choice", idx: f.idx } : null; }).filter(Boolean);
      groups.push([step.title, items, step.note]);
      hint = step.err ? step.err : "";
    } else {
      const { p, q } = parseQuery(raw);
      if (p) { pfx.hidden = false; pfx.textContent = p; }
      const a = cur();
      if (p === "" || p === "@") {
        const ag = [];
        state.agents.forEach((x) => {
          const nm = agentName(x);
          const f = fuzzy(q, nm);
          const meta = q && [agentId(x), x.host, x.worktree && x.worktree.repo].filter(Boolean).join(" ").toLowerCase().includes(q.toLowerCase()) ? { score: 6, idx: [] } : null;
          if (!f && !meta) return;
          const recent = x.last_active ? Date.parse(x.last_active) / 3.6e6 : 0; // hours: larger is newer
          // With no query, who needs you leads; with one, the best match does.
          const rank = (f || meta).score + (q ? 0 : (needsYou(x) ? 100 : 0) + recent / 1e5) - (x === a ? 2 : 0);
          ag.push({ kind: "agent", a: x, rank, idx: f ? f.idx : [], recent });
        });
        ag.sort((x, y) => y.rank - x.rank || y.recent - x.recent);
        groups.push([q || p ? "Agents" : "Agents: needs you first, then recent", p || q ? ag : ag.slice(0, 6)]);
      }
      if (p === "" || p === ">") {
        const cmd = (c, sec) => { const f = fuzzy(q, c.label + (q ? " " + sec : "")); return f ? { kind: "cmd", c, sec, rank: f.score, idx: (fuzzy(q, c.label) || { idx: [] }).idx } : null; };
        const sections = [];
        if (a) sections.push(["Current agent: " + agentName(a), agentActions(a)]);
        sections.push(["Go to", pages().filter((c) => !c.show || c.show())]);
        sections.push(["Fleet", globalActions().filter((c) => !c.show || c.show())]);
        if (q) {
          const all = sections.flatMap(([sec, cs]) => cs.map((c) => cmd(c, sec))).filter(Boolean).sort((x, y) => y.rank - x.rank);
          groups.push(["Commands", all.slice(0, 10)]);
        } else {
          for (const [sec, cs] of sections) groups.push([sec, cs.map((c) => cmd(c, sec)).filter(Boolean)]);
        }
      }
      if (p === "" || p === "#") {
        const td = todos.items.filter((t) => t.state === "open").map((t) => {
          const f = fuzzy(q, t.question.replace(/\s+/g, " "));
          return f ? { kind: "todo", t, rank: f.score, idx: f.idx } : null;
        }).filter(Boolean).sort((x, y) => y.rank - x.rank);
        groups.push([q || p ? "To do" : "To do: open", p || q ? td : td.slice(0, 3)]);
      }
      hint = p ? `prefix ${p}: ${({ ">": "commands", "@": "agents", "#": "to-dos" })[p]} only` : "";
    }
    // Typing: the group holding the best match comes first (so "stop" finds the command, not an agent).
    if (!step && parseQuery(raw).q) groups.sort((x, y) => ((y[1][0] && y[1][0].rank) || -1) - ((x[1][0] && x[1][0].rank) || -1));
    renderGroups(list, groups, raw);
    foot.textContent = "";
    if (hint) { const s = document.createElement("span"); s.textContent = hint; foot.append(s); }
    else if (!step) { const s = document.createElement("span"); s.append("type ", kbd(">"), " commands, ", kbd("@"), " agents, ", kbd("#"), " to-dos"); foot.append(s); }
    foot.append(keyHint("↑↓", "move"), keyHint("Enter", "run"), keyHint("Esc", step ? "back" : "close"));
  }

  const kbd = (t) => { const k = document.createElement("kbd"); k.textContent = t; return k; };
  function keyHint(key, text) { const s = document.createElement("span"); s.append(kbd(key), " " + text); return s; }
  function note(text) { const d = document.createElement("div"); d.className = "pal-empty"; d.textContent = text; return d; }

  function renderGroups(list, groups, raw) {
    const kids = [];
    const flat = [];
    for (const [title, items, sub] of groups) {
      if (!items.length) continue;
      const h = document.createElement("div");
      h.className = "pal-sec"; h.textContent = title;
      kids.push(h);
      if (sub) kids.push(note(sub));
      for (const it of items) {
        const n = flat.length;
        flat.push(it);
        kids.push(itemEl(it, n));
      }
    }
    pal.items = flat;
    if (pal.sel >= flat.length) pal.sel = Math.max(0, flat.length - 1);
    if (!flat.length) kids.push(note(raw ? `No match for “${raw}”.` : "Nothing here."));
    list.replaceChildren(...kids);
    markSel(true);
  }

  function itemEl(it, n) {
    const d = document.createElement("div");
    d.className = "pal-it" + (it.danger ? " danger" : "");
    d.id = "pal-o" + n;
    d.dataset.n = n;
    d.setAttribute("role", "option");
    d.setAttribute("aria-selected", "false");
    const ic = document.createElement("span");
    ic.className = "pal-ic";
    const main = document.createElement("span");
    main.className = "pal-main";
    const meta = document.createElement("span");
    meta.className = "pal-meta";
    if (it.kind === "agent") {
      const x = it.a;
      const dot = document.createElement("span");
      dot.className = "dot " + (x.agent_status || "");
      ic.append(dot);
      main.append(marked(agentName(x), it.idx));
      const where = [x.host && multiHost() ? x.host : "", x.worktree && x.worktree.repo].filter(Boolean).join(" · ");
      meta.textContent = [where, statusLabel[x.agent_status] || x.agent_status].filter(Boolean).join(" · ");
      d.append(ic, main, meta);
    } else if (it.kind === "todo") {
      ic.textContent = "☐";
      main.append(marked(it.t.question.replace(/\s+/g, " "), it.idx));
      meta.textContent = it.t.agent_name || "";
      d.append(ic, main, meta);
    } else if (it.kind === "choice") {
      ic.textContent = "›";
      main.append(marked(it.label, it.idx));
      meta.textContent = it.meta || "";
      d.append(ic, main, meta);
    } else {
      ic.textContent = it.sec === "Go to" ? "→" : "›";
      main.append(marked(it.c.label, it.idx));
      d.append(ic, main);
      if (it.c.keys) {
        const ks = document.createElement("span");
        ks.className = "pal-keys";
        it.c.keys.forEach((k, i) => { if (i && it.c.keys[0] === "g") ks.append("then"); ks.append(kbd(k)); });
        d.append(ks);
      }
    }
    return d;
  }

  function markSel(scroll) {
    document.querySelectorAll("#pal-list .pal-it").forEach((e) => e.setAttribute("aria-selected", String(+e.dataset.n === pal.sel)));
    const e = $("pal-o" + pal.sel);
    $("pal-q").setAttribute("aria-activedescendant", e ? e.id : "");
    if (e && scroll) e.scrollIntoView?.({ block: "nearest" });
  }

  function runItem(it) {
    if (!it) return;
    if (it.kind === "agent") { closePalette(false); openDetail(agentId(it.a)); focusRow(agentId(it.a)); return; }
    if (it.kind === "todo") { closePalette(false); openPage("todo"); todos.nxt = null; todoExpand(it.t.id); return; }
    if (it.kind === "choice") { it.run(); return; }
    const c = it.c;
    if (c.step) { const a = cur(); if (a) startStep(stepFor(c.step, a)); return; }
    closePalette(false);
    c.run();
  }

  function onPaletteKey(ev) {
    if (!pal.open) return;
    const k = ev.key;
    if (k === "ArrowDown" || k === "ArrowUp") {
      ev.preventDefault();
      const n = pal.items.length;
      if (n) { pal.sel = (pal.sel + (k === "ArrowDown" ? 1 : -1) + n) % n; markSel(true); }
    } else if (k === "Enter" && !ev.isComposing) {
      ev.preventDefault();
      if (pal.step && pal.step.kind === "input") pal.step.submit($("pal-q").value);
      else runItem(pal.items[pal.sel]);
    } else if (k === "Tab") {
      ev.preventDefault(); // the field is the only stop
    }
  }

  /* ---------- the cheat sheet ---------- */

  const cheat = { open: false, from: null };
  function openCheats() {
    if (cheat.open) return;
    cheat.open = true;
    cheat.from = document.activeElement && document.activeElement !== document.body ? document.activeElement : null;
    const sec = (title, rowsList) => {
      const d = document.createElement("div");
      const h = document.createElement("h4");
      h.textContent = title;
      d.append(h);
      for (const [text, keys] of rowsList) {
        const r = document.createElement("div");
        r.className = "ch-row";
        const t = document.createElement("span");
        t.textContent = text;
        const k = document.createElement("span");
        k.className = "pal-keys";
        keys.forEach((x, i) => { if (i && keys[0] === "g") k.append("then"); k.append(kbd(x)); });
        r.append(t, k);
        d.append(r);
      }
      return d;
    };
    const box = $("cheat-box");
    const h = document.createElement("h3");
    h.textContent = "Keyboard shortcuts";
    const sub = document.createElement("span");
    sub.className = "ch-sub";
    sub.textContent = "Esc closes · none fire while you type";
    h.append(sub);
    const cols = document.createElement("div");
    cols.className = "ch-cols";
    cols.append(
      sec("Everywhere", [["Command palette", [MOD, "K"]], ["This sheet", ["?"]], ["Leave the prompt box", ["Esc"]], ["Send a prompt", ["Enter"]]]),
      sec("Agents", [["Next / previous agent", ["j", "k"]], ["Next agent that needs you", ["n"]], ["Previous that needs you", ["Shift", "N"]], ["Agent 1–9 in the list", ["Alt", "1–9"]],
        ["Focus the prompt", ["i"]], ["Doc and diff panel", ["d"]], ["Mark / unmark", ["m"]], ["Rename (Claude agents)", ["r"]], ["Switch model", ["Shift", "M"]]]),
      sec("Go to", [["Search", ["/"]], ["To do", ["g", "t"]], ["Status", ["g", "s"]], ["Ready to remove", ["g", "r"]], ["Briefing", ["g", "b"]], ["Agent list", ["g", "a"]]]),
      sec("To do page", [["Next / previous item", ["j", "k"]], ["Open the answer box", ["Enter"]], ["Pick an option", ["1–9"]], ["Close the item", ["Esc"]], ["Send an answer", [MOD, "Enter"]]]),
    );
    box.replaceChildren(h, cols);
    $("cheats").hidden = false;
    popOpened("cheats");
    box.focus();
    syncMode();
  }
  function closeCheats(restore) {
    if (!cheat.open) return;
    cheat.open = false;
    $("cheats").hidden = true;
    popClosed("cheats");
    const f = cheat.from;
    cheat.from = null;
    if (restore !== false && f && f.isConnected) f.focus();
    syncMode();
  }

  /* ---------- the mode chip ---------- */

  function mode() {
    if (pal.open) return "PALETTE";
    if (cheat.open) return "SHEET";
    return typing(document.activeElement) ? "TYPING" : "NAV";
  }
  function syncMode() {
    const m = mode();
    const chip = $("kb-mode");
    if (!chip) return;
    chip.textContent = m;
    chip.dataset.mode = m;
    document.body.dataset.keymode = m;
    const h = $("kb-hint");
    if (h) {
      h.textContent = "";
      if (m === "TYPING") h.append(kbd("Esc"), " leave the box");
      else if (m === "PALETTE") h.append(kbd("Esc"), " back");
      else if (m === "SHEET") h.append(kbd("Esc"), " close");
      else h.append(kbd(MOD + " K"), " for commands · ", kbd("?"), " shortcuts");
    }
  }

  /* ---------- the keys ---------- */

  let gAt = 0;
  const G = { t: () => openPage("todo"), s: () => openPage("status"), r: () => openReview(), f: () => goSearch(), a: () => pages().find((p) => p.id === "go-agents").run(), b: () => openPage("briefing") };

  function onKey(ev) {
    if (ev.defaultPrevented || ev.isComposing) return;
    const k = ev.key;
    // The palette key works everywhere, typing or not.
    if ((ev.ctrlKey || ev.metaKey) && !ev.altKey && !ev.shiftKey && (k === "k" || k === "K")) {
      ev.preventDefault();
      if (pal.open) closePalette(); else if (cheat.open) { closeCheats(false); openPalette(); } else openPalette();
      return;
    }
    // Another layer owns the keys while it is open (the palette's field, a sheet, a menu).
    if (pal.open || cheat.open || anotherLayerOpen()) return;
    if (typing(ev.target)) {
      if (k === "Escape" && ev.target.id === "prompt-text") { ev.preventDefault(); leavePrompt(); }
      return;
    }
    // Alt+1..9: the agent at that place in the list.
    if (ev.altKey && !ev.ctrlKey && !ev.metaKey && !ev.shiftKey && /^Digit[1-9]$/.test(ev.code) && isDesktop()) {
      ev.preventDefault();
      jumpTo(+ev.code.slice(5));
      return;
    }
    if (mods(ev) || ev.repeat) return;
    if (gAt) {
      const fresh = Date.now() - gAt < G_MS;
      gAt = 0;
      if (fresh && G[k]) { ev.preventDefault(); G[k](); return; }
    }
    const todoPage = state.view === "todo" && isDesktop();
    switch (k) {
      case "g": ev.preventDefault(); gAt = Date.now(); return;
      case "i": if (focusPrompt()) ev.preventDefault(); return;
      case "/": ev.preventDefault(); goSearch(); return;
      case "?": ev.preventDefault(); openCheats(); return;
      case "j": case "k":
        if (todoPage || !isDesktop()) return; // the To do page moves its own items
        ev.preventDefault(); moveAgent(k === "j" ? 1 : -1); return;
      case "n": if (isDesktop()) { ev.preventDefault(); nextNeeds(1); } return;
      case "N": if (isDesktop()) { ev.preventDefault(); nextNeeds(-1); } return;
      case "d": if (state.selected && window.SP) { ev.preventDefault(); window.SP.toggle(); } return;
      case "m": if (state.selected && hasLedger()) { ev.preventDefault(); toggleMark(); } return;
      case "M": if (state.selected && switchable(cur())) { ev.preventDefault(); openSwitch(); } return;
      case "r":
        if (!state.selected) return;
        ev.preventDefault();
        if (canRename(cur())) { openPalette(); startStep(stepFor("rename", cur())); } else toast("Rename is only available for Claude agents", true);
        return;
    }
  }

  function init() {
    // The palette and the sheet are popovers: one manager closes the others, answers Esc and the outside click.
    document.addEventListener("keydown", onKey);
    $("pal-q").addEventListener("keydown", onPaletteKey);
    $("pal-q").addEventListener("input", () => { pal.sel = 0; buildPalette(); });
    $("pal-list").addEventListener("click", (ev) => { const e = ev.target.closest(".pal-it"); if (e) runItem(pal.items[+e.dataset.n]); });
    $("pal-list").addEventListener("mousemove", (ev) => { const e = ev.target.closest(".pal-it"); if (e && pal.sel !== +e.dataset.n) { pal.sel = +e.dataset.n; markSel(false); } });
    $("palette").addEventListener("mousedown", (ev) => { if (ev.target === $("palette")) { ev.preventDefault(); closePalette(); } });
    $("cheats").addEventListener("mousedown", (ev) => { if (ev.target === $("cheats")) { ev.preventDefault(); closeCheats(); } });
    $("btn-cmds").addEventListener("click", () => openPalette());
    $("kb-open").addEventListener("click", () => openPalette());
    document.addEventListener("focusin", syncMode);
    document.addEventListener("focusout", () => setTimeout(syncMode, 0));
    syncMode();
    // The cheat sheet's Esc and the sheet's close go through the popover manager.
  }

  window.CMDK = { open: openPalette, close: closePalette, back: backPalette, cheats: openCheats, closeCheats, mode, hint: MOD,
    from: () => pal.from, cheatFrom: () => cheat.from };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();
})();
