// Keyed redraws (#139 slice 4): the agent list is patched on every poll, not rebuilt (app.js patchHTML), so a row the
// poll did not change is the same element afterwards. At 390 px (real touch input, CDP Input.dispatchTouchEvent) and at
// 1280 px (a mouse):
//   - a handle to a row taken before five polls is still that row after them (connected, and the row the list shows);
//   - a row whose name or status changed is updated in place: the same element, with the new text and class (also when
//     the new status moves it to another group, as the desktop's Needs you or a group-by-status list does);
//   - the keyboard focus and the list's scroll position ride through polls;
//   - a press whose release comes after a redraw of the list still opens the row, and the press layer stays on it.
// And the test-only "settled" signal (#139 slice 5): it comes, a held fetch and a finite animation hold it off, an
// infinite animation does not, and without lib.mjs's flag the app has none.
//
//   node keyed.mjs BASE OUT_DIR      (env: SCHEME)
import { launch, makeCheck, settled } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const dark = process.env.SCHEME !== "light";
const assert = (ok, msg) => { if (!ok) throw new Error(msg); };
const ROW = "#agents .agent[data-id]";

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, dark });
  page.setDefaultTimeout(8000);
  const errors = [];
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));
  const check = makeCheck({ page, out });
  const at = `${width}px:`;
  // polls(n): n polls of the list, one after the other: the app's own refreshAgents (what a resync from the hub runs),
  // which fetches /api/agents and redraws the list with the answer. The hub's event stream only sends a change, and the
  // fixtures may not change for a while.
  const polls = (n) => page.evaluate(async (n) => {
    for (let i = 0; i < n; i++) await refreshAgents();
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  }, n);
  const open = async () => {
    await page.goto("about:blank");
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector(ROW, { state: "visible" });
    await polls(1);
  };
  await open();

  await check(`${at} a row is the same element after five polls`, async () => {
    const h = await page.$(ROW);
    const id = await h.evaluate((el) => el.dataset.id);
    await polls(5);
    const same = await h.evaluate((el) => el.isConnected && el === rowFor(el.dataset.id));
    assert(same, `the row ${id} was replaced by a poll`);
  });

  await check(`${at} a row whose name and status change is updated in place`, async () => {
    const r = await page.evaluate(() => {
      const rows = [...document.querySelectorAll("#agents .agent[data-id]")];
      const el = rows.find((x) => x.classList.contains("idle")) || rows[rows.length - 1];
      const others = rows.filter((x) => x !== el);
      const a = state.agents.find((x) => agentId(x) === el.dataset.id);
      a.name = "Renamed by keyed.mjs"; a.agent_status = "blocked"; // on a desktop: into Needs you
      renderList();
      const now = rowFor(el.dataset.id);
      return {
        same: now === el, text: el.querySelector(".name").textContent, cls: el.className,
        othersKept: others.every((x) => x.isConnected && x === rowFor(x.dataset.id)),
      };
    });
    assert(r.same, "the changed row is a new element");
    assert(r.text.includes("Renamed by keyed.mjs"), `its name was not updated: ${r.text}`);
    assert(/\bblocked\b/.test(r.cls) && !/\bidle\b/.test(r.cls), `its class was not updated: ${r.cls}`);
    assert(r.othersKept, "a row that did not change was replaced");
    await polls(2); // the hub's answer puts it back
  });

  await check(`${at} the focus stays on a row across polls`, async () => {
    const id = await page.evaluate(() => { const el = document.querySelectorAll("#agents .agent[data-id]")[1]; el.focus({ preventScroll: true }); window.__f = el; return el.dataset.id; });
    await polls(3);
    const ok = await page.evaluate(() => document.activeElement === window.__f && window.__f.isConnected);
    assert(ok, `the focus left the row ${id}`);
    await page.evaluate(() => document.activeElement.blur());
  });

  await check(`${at} the list keeps its scroll position across polls`, async () => {
    const before = await page.evaluate(() => {
      const row = document.querySelector("#agents .agent[data-id]");
      let sc = row.parentElement;
      while (sc && sc !== document.body && !(/auto|scroll/.test(getComputedStyle(sc).overflowY) && sc.scrollHeight > sc.clientHeight + 1)) sc = sc.parentElement;
      if (!sc || sc === document.body) sc = document.scrollingElement;
      sc.scrollTop = 0; sc.scrollTop = Math.min(120, sc.scrollHeight - sc.clientHeight);
      window.__sc = sc;
      // the row in view at the top of the scroller, and where it is on the screen
      const top = sc === document.scrollingElement ? 0 : sc.getBoundingClientRect().top;
      const r = [...document.querySelectorAll("#agents .agent[data-id]")].find((x) => x.getBoundingClientRect().top >= top);
      window.__anchor = r;
      return { y: sc.scrollTop, id: r && r.dataset.id, at: r && Math.round(r.getBoundingClientRect().top) };
    });
    assert(before.y > 0, "the list does not scroll at this size (the fixture list is too short)");
    await polls(3);
    const after = await page.evaluate(() => ({ y: window.__sc.scrollTop, at: window.__anchor && window.__anchor.isConnected ? Math.round(window.__anchor.getBoundingClientRect().top) : null }));
    // Kept: the same scroll, or the row in view still about where it was on the screen. The fixture hub's agents change
    // between polls: when something above the rows changes height, the browser's scroll anchoring moves the scroll by as
    // much (120 to 157 on the VM), and a row between its anchor and this one can still grow (the row then moved 13 px).
    // What a rebuild did was lose the place: back to the top, or a screenful away. So: the same scroll, or the row
    // within 20 px of where it was.
    assert(after.y === before.y || (after.at !== null && Math.abs(after.at - before.at) <= 20), `the list moved: scroll ${before.y} -> ${after.y}, the row ${before.id} on the screen ${before.at} -> ${after.at}`);
    if (after.y !== before.y) console.log(`     (scroll ${before.y} -> ${after.y} with the row still at ${after.at}: anchored)`);
    await page.evaluate(() => { window.__sc.scrollTop = 0; });
  });

  await check(`${at} a press whose release comes after a redraw opens the row, and keeps its press layer`, async () => {
    const box = await page.evaluate(() => {
      const el = [...document.querySelectorAll("#agents .agent[data-id]")].find((x) => x.offsetParent && x.getBoundingClientRect().top > 60);
      el.scrollIntoView({ block: "center" });
      window.__held = el;
      const r = el.getBoundingClientRect();
      return { x: r.left + r.width / 2, y: r.top + r.height / 2, id: el.dataset.id };
    });
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(r)));
    const cdp = width <= 500 ? await page.context().newCDPSession(page) : null;
    if (cdp) await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: box.x, y: box.y, id: 1 }] });
    else { await page.mouse.move(box.x, box.y); await page.mouse.down(); }
    // A redraw under the finger that changes another row, as a poll does, then the release.
    const mid = await page.evaluate(async () => {
      await new Promise((r) => setTimeout(r, 120)); // past the press layer's delay inside a scroller
      const other = [...document.querySelectorAll("#agents .agent[data-id]")].find((x) => x !== window.__held);
      const a = state.agents.find((x) => agentId(x) === other.dataset.id);
      a.name = "Changed under the finger";
      renderList();
      return { same: rowFor(window.__held.dataset.id) === window.__held, pressed: window.__held.classList.contains("is-pressed") };
    });
    if (cdp) await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    else await page.mouse.up();
    assert(mid.same, "the held row was replaced by the redraw");
    assert(mid.pressed, "the press layer left the held row in the redraw");
    await page.waitForFunction((id) => state.selected === id, box.id, { timeout: 4000 }).catch(() => { throw new Error(`the click did not open ${box.id} (selected: ${"?"})`); });
  });

  // The test-only "settled" signal (#139 slice 5, app.js settle), once, at 390 px.
  if (width === 390) {
    await open();
    await check(`${at} settled: the app says when it is settled, and counts it`, async () => {
      assert(await settled(page), "never settled in 5 s");
      const r = await page.evaluate(() => ({ attr: document.documentElement.dataset.settled, n: window.__cb.settles, is: window.__cb.isSettled }));
      assert(r.attr === "1" && r.is && r.n >= 1, `settled: ${JSON.stringify(r)}`);
    });
    await check(`${at} settled: a fetch in flight holds it off until its answer is drawn`, async () => {
      let let_go;
      const held = new Promise((r) => { let_go = r; });
      await page.route(/\/api\/agents$/, async (rt) => { await held; await rt.continue(); });
      await settled(page);
      const n0 = await page.evaluate(() => window.__cb.settles);
      await page.evaluate(() => { window.__polled = refreshAgents(); });
      await page.waitForTimeout(400); // the held fetch, over many frames: never settled meanwhile
      const mid = await page.evaluate(() => ({ attr: document.documentElement.dataset.settled, n: window.__cb.settles }));
      let_go();
      await page.evaluate(() => window.__polled);
      assert(await settled(page), "never settled after the answer");
      await page.unroute(/\/api\/agents$/);
      assert(mid.attr === "0" && mid.n === n0, `settled while the fetch was held: ${JSON.stringify(mid)} (count before ${n0})`);
    });
    await check(`${at} settled: a finite animation holds it off; an infinite one does not`, async () => {
      await settled(page);
      const r = await page.evaluate(async () => {
        const el = document.querySelector("#agents .agent[data-id]");
        const spin = el.animate([{ opacity: 1 }, { opacity: 0.9 }], { duration: 500, iterations: Infinity });
        await new Promise((res) => setTimeout(res, 100));
        const withSpin = await window.__cb.settled(2000);
        const a = el.animate([{ opacity: 1 }, { opacity: 0.5 }], { duration: 400 });
        await new Promise((res) => requestAnimationFrame(res));
        const during = window.__cb.isSettled;
        const t0 = performance.now();
        await window.__cb.settled(3000);
        const after = performance.now() - t0, done = a.playState;
        spin.cancel();
        return { withSpin, during, after, done };
      });
      assert(r.withSpin, "an infinite animation kept it from settling");
      assert(!r.during && r.done === "finished", `settled during a 400 ms animation: ${JSON.stringify(r)}`);
    });
    await check(`${at} settled: without the uitest's flag there is no signal (a user's app)`, async () => {
      const c = await browser.newContext({ viewport: { width: 390, height: 844 } });
      await c.addInitScript(() => { delete window.__cbSettle; });
      const p = await c.newPage();
      await p.goto(base + "/", { waitUntil: "load" });
      await p.waitForSelector(ROW, { state: "visible" });
      const r = await p.evaluate(() => ({ cb: typeof window.__cb, attr: document.documentElement.getAttribute("data-settled"), fetch: /native code/.test(String(window.fetch)) }));
      await c.close();
      assert(r.cb === "undefined" && r.attr === null && r.fetch, `the signal is on without the flag: ${JSON.stringify(r)}`);
    });
  }

  if (errors.length) await check(`${at} no page errors`, async () => { throw new Error(errors.join("\n")); });
  await browser.close();
  if (check.failed) process.exitCode = 1;
}
