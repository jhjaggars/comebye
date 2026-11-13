// Park and resume in the app: the "Recently closed" section on the Agents screen (all hosts, an offline
// host greyed), the sheet with Resume and Keep, and "Park…" in a row's menu (with the dirty-worktree confirm).
//
//   node parked.mjs BASE OUT_DIR       (env: SCHEME)
// The hub's answers are mocked, so the check does not depend on the shared fleet.
import { launch } from "./lib.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const ago = (h) => new Date(Date.now() - h * 3600e3).toISOString();
const rec = (id, name, kind, extra = {}) => ({ id, name, kind, session_kind: kind === "pi" ? "path" : "id", session: kind === "pi" ? "/home/u/.pi/agent/sessions/x/2026-11-10T09.jsonl" : "c3eb8f54-1111-4222-8333-444455556666",
  worktree: `/home/u/.worktrees/proj/${name}`, branch: `feat/${name}`, why: "manual", parked_at: ago(50), ...extra });
const HOSTS = [
  { host: "uitest", online: true, local: true, parked: [rec("pk_aaaaaaaaaa", "netpol-guard-impl", "claude", { scratch_bytes: 330e6, worktree_bytes: 22e6, session_bytes: 412e3 }), rec("pk_bbbbbbbbbb", "openshell-homelab", "claude", { dirty: true, dirty_files: 3, parked_at: ago(120), worktree_bytes: 1.1e9, session_bytes: 30e6 })] },
  { host: "peerbox", online: false, seen: ago(2), error: "dial failed", parked: [rec("pk_cccccccccc", "spike-sandbox", "opencode", { parked_at: ago(140) })] },
];

for (const width of [390, 1280]) {
  const { browser, ctx, page } = await launch({ width, height: width > 500 ? 800 : 844, dark });
  const posts = [];
  await page.route(/\/api\/parked\/all$/, (r) => r.fulfill({ json: { hosts: HOSTS } }));
  await page.route(/\/api\/park\/mark$/, async (r) => {
    posts.push({ url: new URL(r.request().url()).pathname, body: r.request().postDataJSON() });
    return r.fulfill({ json: { marked: true } });
  });
  await page.route(/\/api\/hosts\/[^/]+\/(resume|park\/pin|park)$/, async (r) => {
    const req = r.request();
    const body = req.postDataJSON();
    posts.push({ url: new URL(req.url()).pathname, body });
    if (/\/park$/.test(req.url()) && !body.dirty && posts.dirtyOnce) return r.fulfill({ status: 409, json: { code: "dirty_worktree", message: "the worktree has 2 uncommitted file(s); parking keeps them in place. Confirm to park anyway" } });
    if (/\/resume$/.test(req.url())) return r.fulfill({ json: { agent: { id: "uitest/w9:p1", name: "netpol-guard-impl" }, note: "n" } });
    if (/\/park$/.test(req.url())) return r.fulfill({ json: { record: rec("pk_dddddddddd", "x", "claude"), closed: "pane", scratch: "moved 1.0 MB to disk" } });
    return r.fulfill({ json: { record: {} } });
  });

  await check(`recently closed at ${width}px: all hosts in one section, the offline host greyed, no sideways scroll`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector(".parked-sec .parked-row", { timeout: 15000 });
    const g = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".parked-sec .parked-row")];
      const off = document.querySelector('.parked-row[data-pk="pk_cccccccccc"]');
      const head = document.querySelector(".parked-sec .proj-head");
      return { n: rows.length, names: rows.map((r) => r.querySelector(".name").textContent),
        offOpacity: +getComputedStyle(off).opacity, offChip: /host offline/.test(off.textContent), head: head.textContent.replace(/\s+/g, " ").trim(),
        edited: /3 files edited/.test(document.querySelector('[data-pk="pk_bbbbbbbbbb"]').textContent),
        scroll: document.documentElement.scrollWidth > innerWidth,
        inside: rows.every((r) => { const b = r.getBoundingClientRect(); return b.left >= 0 && b.right <= innerWidth + 1; }),
        dot: getComputedStyle(rows[0].querySelector(".dot")).borderTopWidth };
    });
    if (g.n !== 3 || g.names[0] !== "netpol-guard-impl" || g.names[2] !== "spike-sandbox") throw new Error("rows " + JSON.stringify(g));
    if (!(g.offOpacity < 1) || !g.offChip) throw new Error("offline row not greyed " + JSON.stringify(g));
    if (!/Recently closed 3/.test(g.head) || !g.edited || g.scroll || !g.inside) throw new Error("section " + JSON.stringify(g));
    const cost = await page.evaluate(() => document.querySelector(".parked-sec .pk-cost")?.textContent || "");
    if (!/3 kept: 1\.1 GB of worktrees, 30 MB of sessions, 330 MB of scratchpads moved off \/tmp/.test(cost)) throw new Error("cost line: " + cost);
    await page.locator(".parked-sec").screenshot({ path: `${out}/parked-section-${width}.png` });
  });

  await check(`resume sheet at ${width}px: what is kept, Resume posts the record id`, async () => {
    await page.click('.parked-row[data-pk="pk_aaaaaaaaaa"]');
    await page.waitForSelector("#pk", { state: "visible" });
    const g = await page.evaluate(() => ({ kv: document.querySelector("#pk-kv").textContent, go: document.querySelector("#pk-go").disabled,
      scroll: document.documentElement.scrollWidth > innerWidth, goH: document.querySelector("#pk-go").getBoundingClientRect().height }));
    if (!/\/home\/u\/\.worktrees\/proj\/netpol-guard-impl/.test(g.kv) || !/feat\/netpol-guard-impl/.test(g.kv) || !/330\.0 MB/.test(g.kv) || g.go || g.scroll) throw new Error("sheet " + JSON.stringify(g));
    if (width < 500 && g.goH < 44) throw new Error("Resume is " + g.goH + "px tall on a phone");
    await page.screenshot({ path: `${out}/parked-sheet-${width}.png` });
    await page.click("#pk-go");
    await page.waitForFunction(() => document.querySelector("#pk").hidden, null, { timeout: 5000 });
    const p = posts.filter((x) => x.url.endsWith("/resume"));
    if (p.length !== 1 || p[0].body.id !== "pk_aaaaaaaaaa" || !p[0].url.includes("/hosts/uitest/")) throw new Error("posts " + JSON.stringify(posts));
  });

  await check(`resume sheet at ${width}px: Remove… sends the parked agent to the removal review (nothing is deleted)`, async () => {
    await page.click('.parked-row[data-pk="pk_bbbbbbbbbb"]');
    await page.waitForSelector("#pk", { state: "visible" });
    const h = await page.evaluate(() => document.querySelector("#pk-rm").getBoundingClientRect().height);
    if (width < 500 && h < 44) throw new Error("Remove… is " + h + "px tall");
    posts.length = 0;
    await page.click("#pk-rm");
    await page.waitForFunction(() => document.querySelector("#pk").hidden, null, { timeout: 5000 });
    const p = posts.filter((x) => x.url.endsWith("/park/mark"));
    if (p.length !== 1 || p[0].body.id !== "pk_bbbbbbbbbb" || p[0].body.host !== "uitest") throw new Error("mark posts " + JSON.stringify(posts));
    await page.waitForFunction(() => /Ready to remove/.test(document.querySelector("#toast").textContent));
  });

  await check(`resume sheet at ${width}px: an offline host cannot be resumed and says why; Keep posts a pin`, async () => {
    await page.click('.parked-row[data-pk="pk_cccccccccc"]');
    await page.waitForSelector("#pk", { state: "visible" });
    const g = await page.evaluate(() => ({ go: document.querySelector("#pk-go").disabled, note: document.querySelector("#pk-note").textContent }));
    if (!g.go || !/peerbox is offline/.test(g.note)) throw new Error("offline sheet " + JSON.stringify(g));
    await page.click("#pk-close");
    await page.waitForFunction(() => document.querySelector("#pk").hidden);
    await page.click('.parked-row[data-pk="pk_bbbbbbbbbb"]');
    await page.waitForSelector("#pk", { state: "visible" });
    await page.click("#pk-pin");
    await page.waitForFunction(() => !document.querySelector("#pk-pin").disabled);
    const p = posts.filter((x) => x.url.endsWith("/park/pin"));
    if (p.length !== 1 || p[0].body.id !== "pk_bbbbbbbbbb" || p[0].body.pinned !== true) throw new Error("pin posts " + JSON.stringify(posts));
    await page.click("#pk-close");
  });

  await check(`row menu at ${width}px: Park… only for an idle agent; a dirty worktree asks first, then parks with the confirm`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent[data-id]", { timeout: 15000 });
    const ids = await page.evaluate(() => {
      const mk = (pane, st, name) => ({ ...state.agents[0], pane_id: pane, id: (state.agents[0].host ? state.agents[0].host + "/" : "") + pane, name, agent: "claude", agent_status: st, background: undefined, blocked_on: undefined });
      const a = mk("w91:p1", "idle", "park-me"), b = mk("w92:p1", "working", "busy-one");
      state.agents = [a, b]; renderList();
      return [agentId(a), agentId(b)];
    });
    const shown = async (id) => page.evaluate((i) => { openRowMenu(document.querySelector(`#agents .agent[data-id="${i}"] .row-more`)); const v = !document.querySelector("#row-park").hidden; closeRowMenu(); return v; }, id);
    if (!(await shown(ids[0]))) throw new Error("no Park… for an idle agent");
    if (await shown(ids[1])) throw new Error("Park… offered for a working agent");
    posts.length = 0; posts.dirtyOnce = true;
    page.once("dialog", (d) => { if (!/Park it anyway/.test(d.message())) throw new Error("dialog: " + d.message()); d.accept(); });
    await page.evaluate((i) => { openRowMenu(document.querySelector(`#agents .agent[data-id="${i}"] .row-more`)); }, ids[0]);
    await page.click("#row-park");
    await page.waitForFunction(() => /Parked park-me/.test(document.querySelector("#toast").textContent), null, { timeout: 5000 });
    const p = posts.filter((x) => x.url.endsWith("/park"));
    if (p.length !== 2 || p[0].body.dirty || p[1].body.dirty !== true || p[0].body.agent !== "w91:p1" || p[0].body.name !== "park-me") throw new Error("park posts " + JSON.stringify(p));
    posts.dirtyOnce = false;
  });
  await check(`suggest at ${width}px: a "can park" chip, a banner, and one tap parks the listed agents`, async () => {
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector("#agents .agent[data-id]", { timeout: 15000 });
    await page.evaluate(() => {
      const mk = (pane, name, hint) => ({ ...state.agents[0], pane_id: pane, id: (state.agents[0].host ? state.agents[0].host + "/" : "") + pane, name, agent: "claude", agent_status: "idle",
        background: undefined, blocked_on: undefined, park_hint: hint ? { since: new Date(Date.now() - 30 * 3600e3).toISOString() } : undefined });
      state.agents = [mk("w81:p1", "idle-a", true), mk("w82:p1", "idle-b", true), mk("w83:p1", "fresh-c", false)];
      renderList();
    });
    const g = await page.evaluate(() => ({ chips: [...document.querySelectorAll("#agents .can-park")].length, banner: document.querySelector("#park-banner")?.textContent.trim(),
      scroll: document.documentElement.scrollWidth > innerWidth, bh: document.querySelector("#park-banner")?.getBoundingClientRect().height }));
    if (g.chips !== 2 || !/2 idle agents can be parked/.test(g.banner || "") || g.scroll || g.bh < 44) throw new Error("banner " + JSON.stringify(g));
    posts.length = 0;
    await page.click("#park-banner");
    await page.waitForSelector("#ps", { state: "visible" });
    const l = await page.evaluate(() => ({ rows: document.querySelectorAll("#ps-list .ps-row").length, go: document.querySelector("#ps-go").textContent, scroll: document.documentElement.scrollWidth > innerWidth }));
    if (l.rows !== 2 || l.go !== "Park 2 agents" || l.scroll) throw new Error("list " + JSON.stringify(l));
    await page.screenshot({ path: `${out}/parked-suggest-${width}.png` });
    await page.click('#ps-list .ps-row:first-child input');   // untick one
    await page.waitForFunction(() => document.querySelector("#ps-go").textContent === "Park 1 agent");
    await page.click("#ps-go");
    await page.waitForFunction(() => /Parked 1/.test(document.querySelector("#ps-status").textContent), null, { timeout: 8000 });
    const p = posts.filter((x) => x.url.endsWith("/park"));
    if (p.length !== 1 || p[0].body.name !== "idle-b" || p[0].body.why !== "suggested") throw new Error("park posts " + JSON.stringify(p));
    await page.click("#ps-close");
    await page.waitForFunction(() => document.querySelector("#ps").hidden);
  });
  if (width > 1000) await check("a to-do waiting on a parked agent says parked (it is not gone) at 1280px", async () => {
    const p2 = await ctx.newPage();
    await p2.setViewportSize({ width, height: 800 });
    await p2.route(/\/api\/ledger$/, (r) => r.fulfill({ json: { plans: [], entries: [], marks: [], roles: {} } }));
    await p2.route(/\/api\/parked\/all$/, (r) => r.fulfill({ json: { hosts: HOSTS } }));
    await p2.route(/\/api\/todos$/, (r) => r.fulfill({ json: { open: 1, answered: 0, todos: [{ id: "t4f1a000001", kind: "decision", question: "Should the guard also cover egress rules?", state: "open",
      context: "", agent_name: "netpol-guard-impl", created_at: ago(3), waiting: { id: "uitest/w5:p1", name: "netpol-guard-impl", parked: true, park_id: "pk_aaaaaaaaaa" } }] } }));
    await p2.goto(base + "/#/todo", { waitUntil: "load" });
    await p2.waitForSelector(".td-row", { state: "attached", timeout: 15000 });
    await p2.locator(".td-row:visible").first().click();
    await p2.waitForSelector(".td-strip .chip.teal", { state: "attached", timeout: 15000 });
    const g = await p2.evaluate(() => { const c = [...document.querySelectorAll(".td-strip .chip.teal")].find((x) => x.offsetParent); return { t: c && c.textContent, title: c && c.title, scroll: document.documentElement.scrollWidth > innerWidth }; });
    if (g.t !== "parked" || !/resumed/.test(g.title) || g.scroll) throw new Error(JSON.stringify(g));
    await p2.screenshot({ path: `${out}/parked-todo-${width}.png` });
    await p2.close();
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`parked: ${results.length - bad}/${results.length} passed`);
process.exit(bad ? 1 : 0);
