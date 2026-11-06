/* The intro panel and the guided tour. Loaded after app.js, so it can call the
   app's own functions (openDetail, openPage, openReview...): the tour drives
   the real app rather than a picture of it. */
(function () {
  "use strict";
  const el = (id) => document.getElementById(id);
  const REPO = "https://github.com/jhjaggars/comebye";
  PUSH_TEXT.unsupported = "Push notifications are off in this demo. On a real install, you get one when an agent finishes or needs input, even with the app closed.";

  const STORE = "studio/w4:p1", WORKER = "buildbox/w3:p1", COORD = "studio/w1:p1";
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  async function until(fn, ms = 2500) {
    const end = Date.now() + ms;
    for (;;) {
      const v = fn();
      if (v || Date.now() > end) return v;
      await wait(40);
    }
  }

  // leaves whichever sheet or screen is open, without leaving history entries behind
  function clean() {
    toggleFleetMenu(false);
    if (launch.open) { if (history.state && history.state.launch) history.replaceState(null, "", location.href); hideLaunch(); }
    if (review.open) { if (history.state && history.state.review) history.replaceState(null, "", location.href); hideReview(); }
    if (!el("recall").hidden) closeRecall();
  }
  function showAgent(id) {
    clean();
    if (state.selected !== id) openDetail(id);
  }
  function showPage(p) { clean(); openPage(p); }
  const TOUR_PROMPT = "What's left before the rate limits can ship?";
  const getPromptValueIsTourText = () => el("prompt-text").textContent.trim() === TOUR_PROMPT;
  const openFirstTodo = () => {
    const d = document.querySelector("#pt-body .td-item:not(.answered)");
    if (d) d.open = true;
  };

  const STEPS = [
    { title: "One app over all your agents",
      body: `<p>You run coding agents in <a href="https://herdr.dev" target="_blank" rel="noopener">herdr</a> terminal panes, often on several machines. Comebye shows them all in one live list, and your terminal stays in sync because there is nothing to sync: both drive the same panes.</p><p>Here that's 11 agents on three hosts, with the list on the left and whatever you open on the right.</p>`,
      go() { showPage("status"); }, target: () => ["#list header", "#agents"] },
    { title: "Grouped by project, across hosts",
      body: `<p>Agents are grouped by repository. A project's <b>home agent</b> works in the main checkout, and indented <b>workers</b> each have their own branch and worktree.</p><p class="try"><b>Try it:</b> click a host chip to filter. A filter never hides an agent that's waiting on you.</p>`,
      go() { showPage("status"); }, target: () => ["#host-filter", "#agents"] },
    { title: "Needs you",
      body: `<p>Agents stuck on a question or a permission prompt rise to the top, whichever host or project they're on. On a real install you also get a push notification.</p><p>Press <kbd>n</kbd> to jump to the next one.</p>`,
      go() { showPage("status"); }, target: () => [".needs-sec"] },
    { title: "Answer without opening a terminal",
      body: `<p>Comebye reads the prompt from the agent's screen and turns it into buttons. Your choice is sent as the keystrokes the prompt expects.</p><p class="try"><b>Try it:</b> pick a store. The worker carries on and reports back.</p>`,
      async go() {
        showAgent(STORE);
        await until(() => document.querySelector("#answer .opt"));
      }, target: () => ["#answer"], fallback: () => ["#chat"], fallbackNote: "This agent has already been answered. Next moves on." },
    { title: "Read what an agent is doing",
      body: `<p>Each session reads as a chat: its Markdown rendered, and each tool call as a card you can open. This worker is live, so new steps appear while you watch.</p><p class="try"><b>Try it:</b> open the red test failure to see its output.</p>`,
      async go() {
        showAgent(WORKER);
        await until(() => document.querySelector("#chat .toolcard"));
        const c = el("chat"); c.scrollTop = c.scrollHeight;
      }, target: () => ["#chat"] },
    { title: "Send a prompt",
      body: `<p>What you type goes into the agent's own pane, as if you typed it in the terminal. You can attach a screenshot, or send raw keys from the <b>+</b> menu.</p><p class="try"><b>Try it:</b> press <kbd>Enter</kbd> to send this one, or write your own. The reply is canned.</p>`,
      async go() {
        showAgent(COORD);
        await until(() => document.querySelector("#chat .msg"));
        setPromptValue(TOUR_PROMPT);
        updateSendButton();
      }, focus: "#prompt-text", target: () => [".prompt"] },
    { title: "Coordinators organize the work",
      body: `<p>A <b>coordinator</b> is an agent whose job is to plan and delegate. <b>quill</b> is pinned above the projects. In its chat you can see it split a request into a plan, hand each step to a worker with <code>comebye handoff</code>, and read their reports.</p><p>You talk to it, and it keeps the workers moving.</p>`,
      async go() {
        showAgent(COORD);
        if (getPromptValueIsTourText()) setPromptValue("");
        updateSendButton();
        await until(() => document.querySelector("#chat .toolcard"));
        const c = el("chat"); c.scrollTop = 0;
      }, target: () => ["#chat"], place: "left" },
    { title: "Hand work to a worker",
      body: `<p>You can also start one yourself. A handoff picks a host, a repository and a branch, creates a git worktree, starts the agent there, and records it in the hub's <b>ledger</b>. Merging stays with you.</p><p class="try"><b>Try it:</b> this form is filled in for you, so press <b>Hand off</b>. A fake worker starts and reports in a few seconds.</p>`,
      async go() {
        clean();
        if (state.selected != null) closeDetail(); // the app opens the new worker only if nothing else is open
        openLaunch();
        await until(() => launch.opts);
        setLaunchMode("worker");
        const repo = el("hw-repo");
        await until(() => repo.options.length > 1);
        repo.value = "/home/dev/code/tidepool";
        el("hw-branch").value = "changelog-2-4";
        el("hw-brief").value = "Write the changelog for tidepool 2.4 from the commits since 2.3. Finish with a report.";
        renderLaunchForm();
      }, target: () => ["#launch"], fallback: () => ["#chat"] },
    { title: "Status, generated from the ledger",
      body: `<p>This page answers "where does everything stand?": what's running, what's blocked, what's waiting on you and what finished recently, by project. It's generated from the ledger and the to-do list, so it's always current.</p><p class="try"><b>Try it:</b> open a finished worker to read its report.</p>`,
      go() { showPage("status"); }, target: () => ["#p-status"] },
    { title: "To do: what only you can settle",
      body: `<p>Agents put questions and decisions here with context and links, so they don't get lost in a chat. Your answer goes back through the coordinator to the agent that's waiting.</p><p class="try"><b>Try it:</b> type an answer and press Answer. It's relayed a few seconds later.</p>`,
      async go() {
        showPage("todo");
        await until(() => document.querySelector("#pt-body .td-item"));
        openFirstTodo();
      }, target: () => ["#p-todo"] },
    { title: "Search every past session",
      body: `<p>Sessions are archived and indexed across all your hosts, including workers that are long gone. Filter by host, agent or project, and by whether the words were said, run as commands or appeared in output.</p><p class="try"><b>Try it:</b> search for something else, like <code>jitter</code> or <code>migration</code>.</p>`,
      async go() {
        clean();
        if (state.selected != null) closeDetail();
        location.hash = rcSearchHash("backoff");
        await until(() => document.querySelector("#rc-results .rc-card"));
      }, target: () => [".rc-list"] },
    { title: "Read a session in full",
      body: `<p>A result opens the transcript at the match, with the handoff that started it, tool calls and any subagents. The arrows step through matches. Likely secrets are masked when the session is indexed.</p>`,
      async go() {
        if (el("recall").hidden) { location.hash = rcSearchHash("backoff"); }
        const card = await until(() => document.querySelector("#rc-results .rc-card"));
        if (card && !document.querySelector("#rc-session .rc-stitle")) card.click();
        await until(() => document.querySelector("#rc-session .rc-stitle"));
      }, target: () => ["#rc-session"] },
    { title: "Ready to remove",
      body: `<p>Finished sessions pile up, so workers and coordinators mark them for removal, but nothing goes until you confirm. Each one is checked first: is the worktree clean, is the work merged, and does the session itself say it's safe?</p><p>Removing keeps the branch. <b>cache-view-spike</b> has unmerged commits, so it shows exactly what discarding would delete.</p>`,
      async go() {
        clean();
        if (state.selected != null) closeDetail();
        setView(null);
        openReview();
        await until(() => document.querySelector("#rv-list .rv-item"));
      }, target: () => ["#rv-list"] },
    { title: "Light or dark, then it's yours",
      body: `<p>The ⋮ menu holds the theme, along with the sweep for finished workers and the briefing schedule. Pick a theme:</p>
        <div class="themes"><button type="button" class="demo-btn" data-theme-pick="system">System</button><button type="button" class="demo-btn" data-theme-pick="light">Light</button><button type="button" class="demo-btn" data-theme-pick="dark">Dark</button></div>
        <p style="margin-top:10px">That's the tour. Everything still works, so click around. Below 1100px wide the same app becomes the phone layout it was built for.</p>`,
      async go() {
        clean();
        showPage("status");
        await wait(0); // past the click that got here, or the menu takes it as a click outside
        toggleFleetMenu(true, false);
      }, target: () => ["#f-menu"], fallback: () => ["#btn-fleet"], last: true },
  ];

  /* ---------- the overlay ---------- */
  let at = -1, spot = null, card = null, ticker = null, goSeq = 0, usedFallback = false;

  function rectOf(sels) {
    let l = Infinity, t = Infinity, r = -Infinity, b = -Infinity;
    for (const s of sels) {
      const e = document.querySelector(s);
      if (!e || e.hidden) continue;
      const q = e.getBoundingClientRect();
      if (!q.width || !q.height) continue;
      l = Math.min(l, q.left); t = Math.min(t, q.top); r = Math.max(r, q.right); b = Math.max(b, q.bottom);
    }
    if (l === Infinity) return null;
    const W = innerWidth, H = innerHeight;
    l = Math.max(4, l); t = Math.max(4, t); r = Math.min(W - 4, r); b = Math.min(H - 4, b);
    return { left: l, top: t, right: r, bottom: b, width: r - l, height: b - t };
  }

  function currentRect(step) {
    const r = rectOf(step.target());
    if (r || !step.fallback) { usedFallback = false; return r; }
    usedFallback = true;
    return rectOf(step.fallback());
  }

  function place() {
    if (at < 0) return;
    const step = STEPS[at];
    const r = currentRect(step);
    const note = card.querySelector(".fallback");
    if (note) note.hidden = !usedFallback;
    if (r) {
      spot.hidden = false;
      Object.assign(spot.style, { left: r.left - 3 + "px", top: r.top - 3 + "px", width: r.width + 6 + "px", height: r.height + 6 + "px" });
    } else spot.hidden = true;
    const W = innerWidth, H = innerHeight, cw = card.offsetWidth, ch = card.offsetHeight, gap = 14;
    let x, y;
    const clampY = (v) => Math.max(12, Math.min(H - ch - 12, v));
    const clampX = (v) => Math.max(12, Math.min(W - cw - 12, v));
    if (!r) { x = (W - cw) / 2; y = (H - ch) / 2; }
    else if (step.place !== "left" && W - r.right >= cw + gap + 12) { x = r.right + gap; y = clampY(r.top); }
    else if (r.left >= cw + 16) { x = Math.max(8, r.left - cw - gap); y = clampY(r.top); }
    else if (H - r.bottom >= ch + gap + 12) { x = clampX(r.left); y = r.bottom + gap; }
    else if (r.top >= ch + gap + 12) { x = clampX(r.left); y = r.top - ch - gap; }
    else { x = W - cw - 16; y = H - ch - 16; } // a target as big as the window: inside it, out of the way
    card.style.left = Math.round(x) + "px";
    card.style.top = Math.round(y) + "px";
  }

  async function show(i) {
    at = i;
    const seq = ++goSeq;
    const step = STEPS[i];
    card.innerHTML = `<div class="bar"><i style="width:${((i + 1) / STEPS.length) * 100}%"></i></div>
      <div class="n">Step ${i + 1} of ${STEPS.length}</div>
      <h3 id="demo-title">${step.title}</h3>${step.body}
      ${step.fallbackNote ? `<p class="fallback" hidden>${step.fallbackNote}</p>` : ""}
      <div class="row"><button type="button" class="skip" data-act="skip">${step.last ? "" : "Skip tour"}</button><span class="grow"></span>
        <button type="button" class="demo-btn" data-act="back"${i ? "" : " disabled"}>Back</button>
        <button type="button" class="demo-btn primary" data-act="next">${step.last ? "Finish" : "Next"}</button></div>`;
    card.setAttribute("aria-labelledby", "demo-title");
    place();
    try { await step.go(); } catch (e) { console.warn("tour step:", e); }
    if (seq !== goSeq) return;
    await wait(30);
    place();
    const f = step.focus && document.querySelector(step.focus);
    if (f) f.focus(); else card.querySelector('[data-act="next"]').focus({ preventScroll: true });
  }

  function end() {
    at = -1;
    goSeq++;
    clearInterval(ticker);
    ticker = null;
    if (spot) spot.remove();
    if (card) card.remove();
    spot = card = null;
    window.removeEventListener("keydown", onKey, true);
    window.removeEventListener("resize", onResize);
    toggleFleetMenu(false);
    if (launch.open || review.open) clean();
  }

  function onKey(ev) {
    if (at < 0) return;
    if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); end(); return; }
    const t = ev.target;
    if (t.closest && t.closest("input, textarea, select, [contenteditable]")) return;
    if (ev.altKey || ev.ctrlKey || ev.metaKey) return;
    if (ev.key === "ArrowRight") { ev.preventDefault(); next(); }
    else if (ev.key === "ArrowLeft") { ev.preventDefault(); if (at > 0) show(at - 1); }
  }
  function next() { if (at === STEPS.length - 1) end(); else show(at + 1); }
  function onResize() {
    if (!isDesktop()) { end(); showIntro(); } else place();
  }

  function startTour() {
    closeIntro();
    if (!isDesktop()) { showIntro(); return; }
    // Start from a known state: anything answered or removed earlier comes back.
    clean();
    if (state.selected != null) closeDetail();
    window.ComebyeDemo.reset();
    setHostFilter("all");
    review.results = new Map();
    todos.loadedAt = 0; ledger.loadedAt = 0;
    loadLedger();
    refreshAgents().catch(() => {});
    if (spot) end();
    spot = document.createElement("div"); spot.id = "demo-spot";
    card = document.createElement("div"); card.id = "demo-card"; card.setAttribute("role", "dialog");
    document.body.append(spot, card);
    card.addEventListener("click", (ev) => {
      const pick = ev.target.closest("[data-theme-pick]");
      if (pick) { applyTheme(pick.dataset.themePick); renderFleetMenu(); return; }
      const b = ev.target.closest("[data-act]");
      if (!b) return;
      if (b.dataset.act === "next") next();
      else if (b.dataset.act === "back") show(at - 1);
      else end();
    });
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("resize", onResize);
    ticker = setInterval(place, 300); // the list re-renders as agents change
    show(0);
  }

  /* ---------- the intro panel ---------- */
  function showIntro() {
    if (el("demo-intro")) return;
    const wide = isDesktop();
    const d = document.createElement("div");
    d.id = "demo-intro";
    d.innerHTML = `<div class="box" role="dialog" aria-labelledby="demo-intro-t">
      <h2 id="demo-intro-t">Comebye, without installing it</h2>
      <p>Comebye is a small web app for the coding agents you run under herdr, on one machine or several. See which are working and which are waiting on you, read their conversations, answer their questions and hand out new work.</p>
      <div class="flow" aria-label="A browser talks to one Comebye hub, which reaches herdr panes on each host"><span>Browser or phone</span><i>→</i><span class="hub">Comebye hub</span><i>→</i><span>herdr on each host</span></div>
      <p>This is the real web app running in this page against a fake backend. <b>Every agent, project and transcript is made up</b>, and nothing you type leaves your browser. Replies are canned.</p>
      ${wide ? "" : `<p class="narrow">The tour needs a window at least 1100px wide. At this width you get the phone layout, which works too: close this and tap around.</p>`}
      <div class="acts">
        ${wide ? `<button type="button" class="demo-btn primary" data-act="tour">Take the tour (${STEPS.length} steps)</button>` : ""}
        <button type="button" class="demo-btn${wide ? "" : " primary"}" data-act="close">Explore on my own</button>
      </div>
      <p class="fine">Source and install guide: <a href="${REPO}" target="_blank" rel="noopener">github.com/jhjaggars/comebye</a> · <a href="${REPO}/tree/main/docs" target="_blank" rel="noopener">docs</a></p>
    </div>`;
    d.addEventListener("click", (ev) => {
      const b = ev.target.closest("[data-act]");
      if (b && b.dataset.act === "tour") startTour();
      else if (b || ev.target === d) closeIntro();
    });
    d.addEventListener("keydown", (ev) => { if (ev.key === "Escape") { ev.stopPropagation(); closeIntro(); } });
    document.body.append(d);
    const first = d.querySelector(".demo-btn.primary");
    if (first) first.focus({ preventScroll: true });
  }
  function closeIntro() { const d = el("demo-intro"); if (d) d.remove(); }

  function init() {
    const foot = document.createElement("div");
    foot.id = "demo-foot";
    foot.innerHTML = `<span class="tag" role="button" tabindex="0" aria-label="About this demo">Demo</span><span class="grow">Fake data, nothing leaves this page</span><button type="button" class="demo-btn" id="demo-about">About</button><button type="button" class="demo-btn primary" id="demo-tour">Take the tour</button>`;
    document.body.append(foot);
    el("demo-tour").addEventListener("click", startTour);
    el("demo-about").addEventListener("click", showIntro);
    foot.querySelector(".tag").addEventListener("click", showIntro);
    // The viewer's frame can load narrow and widen a moment later: the intro must follow.
    desktopMQ.addEventListener("change", () => { if (el("demo-intro")) { closeIntro(); showIntro(); } });
    showIntro();
  }
  if (document.readyState === "loading") window.addEventListener("DOMContentLoaded", init);
  else init();
  window.ComebyeDemo.tour = { start: startTour, end, go: (i) => show(i), steps: STEPS, at: () => at };
})();
