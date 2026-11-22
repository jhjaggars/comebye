// The Archived screen (#/archived, issue #86 slice 6): finished work put away by the hub's sweep loop.
// The list with its totals and build-output figures, the filter and the search, Unarchive (the row
// leaves and the session is back on the board), Resume and Propose removal (what each posts), the
// links from the board's footer, Ready to remove and Recently closed, the archived session's park record
// folded out of Recently closed, and a session parked mid-task shown as "paused" on its board card.
// No sideways scroll; screenshots at 390 and 1280 px.
//
//   node archived.mjs BASE OUT_DIR       (env: SCHEME)
// The hub's answers are mocked, so the check does not depend on the shared fleet.
import { launch } from "./lib.mjs";
import { boardView } from "./boardfix.mjs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const scheme = dark ? "dark" : "light";
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
}

const ago = (h) => new Date(Date.now() - h * 3600e3).toISOString();
const GB = 1e9;
const row = (id, name, extra, archive) => ({ id, name, agent_id: `uitest/${id}:p1`, host: "uitest", project: "homelab", branch: "feat/" + name, worktree: "/home/u/.worktrees/homelab/" + name, ...extra,
  archive: { at: ago(30), by: "hub", from: "finished", unmerged: 0, unpushed: 0, ...archive } });
const ROWS = () => [
  row("h0000000000a1", "oc4025-proof", { project: "openclaw", branch: "feat/oidc-proof", purpose: "feature" }, { at: ago(5), why: "Archived when its third re-ask still said LEFT.", unmerged: 3, unpushed: 1, park_id: "pk_arch000001",
    left: [{ type: "followup", text: "run tf-apply on the admins root" }, { type: "other", text: "a git stash entry" }], worktree_bytes: 0.4 * GB }),
  row("h0000000000a2", "kv-spoke-build", { purpose: "poc", branch: "poc/kv-spoke", report: true }, { at: ago(48), why: "Archived after 3 days with no progress.", unmerged: 12, park_id: "pk_arch000002",
    build_freed: 18.4 * GB, build_dirs: ["target/"], worktree_bytes: 1.2 * GB }),
  row("h0000000000a3", "docs-sweep", { purpose: "docs", branch: "docs/sweep" }, { at: ago(200), why: "Archived after 7 days with no progress.", build_pending: true }),
];
const view = (rows) => ({ archived: rows, count: rows.length, build_freed: rows.reduce((n, r) => n + (r.archive.build_freed || 0), 0), worktree_bytes: rows.reduce((n, r) => n + (r.archive.worktree_bytes || 0), 0) });
// The ledger: the archived entries (the report one with its text), and a running handoff whose pane was parked by hand.
const LEDGER = { plans: [], marks: [], roles: {}, entries: [
  { id: "h0000000000a1", agent_id: "uitest/h0000000000a1:p1", agent_name: "oc4025-proof", host: "uitest", repo_name: "openclaw", status: "archived", created_at: ago(300), brief: "x" },
  { id: "h0000000000a2", agent_id: "uitest/h0000000000a2:p1", agent_name: "kv-spoke-build", host: "uitest", repo_name: "homelab", status: "archived", created_at: ago(300), brief: "x", final_report: "## Findings\n\nThe **spoke** builds in 4 min." },
  { id: "h0000000000a3", agent_id: "uitest/h0000000000a3:p1", agent_name: "docs-sweep", host: "uitest", repo_name: "homelab", status: "archived", created_at: ago(300), brief: "x" },
  { id: "h111111111111", agent_id: "uitest/w77:p1", agent_name: "briefing-board", host: "uitest", repo_name: "comebye", status: "running", created_at: ago(3), brief: "x", worktree: "/home/u/.worktrees/comebye/briefing-board" },
] };
const rec = (id, name, extra = {}) => ({ id, name, kind: "claude", session_kind: "id", session: "c3eb8f54-1111-4222-8333-444455556666", worktree: `/home/u/.worktrees/homelab/${name}`, branch: `feat/${name}`, why: "manual", parked_at: ago(6), ...extra });
const PARKED = { hosts: [{ host: "uitest", online: true, local: true, parked: [
  rec("pk_arch000001", "oc4025-proof", { why: "archive" }), rec("pk_arch000002", "kv-spoke-build", { why: "archive" }),
  rec("pk_paused0001", "briefing-board", { worktree: "/home/u/.worktrees/comebye/briefing-board" }),
] }] };

for (const width of [390, 1280]) {
  const { browser, page } = await launch({ width, height: width > 500 ? 860 : 844, dark });
  let rows = ROWS();
  const posts = [];
  const board = () => {
    const b = boardView();
    b.loose = { reports: 0, ready_to_remove: 3, archived: rows.length };
    // An unarchived session is back in its lane.
    if (!rows.some((r) => r.id === "h0000000000a3")) b.lanes.find((l) => l.key === "landing").items.unshift({ id: "h0000000000a3", type: "handoff", title: "docs-sweep", project: "homelab", state: "finished", since: ago(200) });
    return b;
  };
  await page.route(/\/api\/archived$/, (r) => r.fulfill({ json: view(rows) }));
  await page.route(/\/api\/archived\/restore$/, (r) => {
    const body = r.request().postDataJSON();
    posts.push({ url: "/api/archived/restore", body });
    rows = rows.filter((x) => x.id !== body.ref);
    return r.fulfill({ json: { entry: { id: body.ref, status: "finished" }, ...(body.resume ? { resume: { agent: { id: "uitest/w9:p1" } } } : {}) } });
  });
  await page.route(/\/api\/marks$/, (r) => {
    const body = r.request().postDataJSON();
    posts.push({ url: "/api/marks", body });
    rows = rows.filter((x) => x.agent_id !== body.agent_id);
    return r.fulfill({ json: { mark: body, entry: {} } });
  });
  await page.route(/\/api\/board$/, (r) => r.fulfill({ json: board() }));
  await page.route(/\/api\/ledger$/, (r) => r.fulfill({ json: LEDGER }));
  await page.route(/\/api\/parked\/all$/, (r) => r.fulfill({ json: PARKED }));
  const noScroll = async () => { if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error("the page scrolls sideways"); };
  const rowNames = () => page.evaluate(() => [...document.querySelectorAll("#ar-list .ar-item:not([hidden]) .rv-name")].filter((x) => x.offsetParent).map((x) => x.textContent));

  await check(`list at ${width}px (${scheme}): totals, why, what is left, build output freed, no sideways scroll`, async () => {
    await page.goto(base + "/#/archived", { waitUntil: "load" });
    await page.waitForSelector("#ar-list .ar-item", { timeout: 15000 });
    const g = await page.evaluate(() => ({ sum: document.querySelector("#ar-sum").textContent, items: [...document.querySelectorAll("#ar-list .ar-item")].map((x) => x.innerText),
      seg: document.querySelector("#ar-seg").innerText.replace(/\s+/g, " "), inside: [...document.querySelectorAll("#ar-list .ar-item, #ar-list .btn")].every((el) => { const b = el.getBoundingClientRect(); return b.left >= 0 && b.right <= innerWidth + 1; }) }));
    if (!/3 archived · 1\.6 GB on disk · 18\.4 GB of build output freed · no source is ever deleted/.test(g.sum)) throw new Error("summary: " + g.sum);
    if (g.items.length !== 3 || !/^oc4025-proof/.test(g.items[0])) throw new Error("rows " + JSON.stringify(g.items));
    const [a, b, c] = g.items;
    for (const [t, want] of [[a, "2 left"], [a, "Owes you: run tf-apply on the admins root"], [a, "Can’t check: a git stash entry"], [a, "3 commits not merged, 1 on no remote"], [a, "openclaw · feat/oidc-proof"],
      [b, "Freed 18.4 GB of build output (target/)"], [b, "12 commits not merged, pushed"], [b, "Final report"], [c, "cleared when uitest is next up"]]) if (!t.includes(want)) throw new Error(`missing "${want}" in ${t}`);
    if (!/Resume/.test(a) || /Resume/.test(c)) throw new Error("Resume only where there is a park record");
    if (!/All 3 Unmerged 2 Reports 1 Something left 1/.test(g.seg)) throw new Error("filter " + g.seg);
    if (!g.inside) throw new Error("a row or a button is past the edge");
    await noScroll();
    await page.screenshot({ path: `${out}/archived-${width}-${scheme}.png`, fullPage: false });
  });

  await check(`filter and search at ${width}px`, async () => {
    await page.click('#ar-seg [data-ar-f="unmerged"]');
    let n = await rowNames();
    if (n.join() !== "oc4025-proof,kv-spoke-build") throw new Error("unmerged " + n);
    await page.click('#ar-seg [data-ar-f="left"]');
    n = await rowNames();
    if (n.join() !== "oc4025-proof") throw new Error("left " + n);
    await page.click('#ar-seg [data-ar-f="all"]');
    await page.fill("#ar-q", "spoke");
    n = await rowNames();
    if (n.join() !== "kv-spoke-build") throw new Error("search " + n);
    await page.fill("#ar-q", "tf-apply"); // what is left is searched too
    n = await rowNames();
    if (n.join() !== "oc4025-proof") throw new Error("search left " + n);
    await page.fill("#ar-q", "");
    // The report opens in place.
    await page.click('#ar-list details.rv-report[data-id="h0000000000a2"] summary');
    await page.waitForSelector('#ar-list details.rv-report[data-id="h0000000000a2"] .md strong', { timeout: 5000 });
  });

  await check(`Unarchive at ${width}px: posts its id, the row leaves, and it is back on the board`, async () => {
    await page.click('#ar-list button[data-ar="restore"][data-id="h0000000000a3"]');
    await page.waitForFunction(() => /Unarchived docs-sweep: it is back on the board/.test(document.querySelector("#ar-msg").textContent), null, { timeout: 5000 });
    await page.waitForFunction(() => !document.querySelector('#ar-list .ar-item[data-id="h0000000000a3"]'), null, { timeout: 5000 });
    const p = posts.filter((x) => x.url === "/api/archived/restore");
    if (p.length !== 1 || p[0].body.ref !== "h0000000000a3" || p[0].body.resume) throw new Error("posts " + JSON.stringify(posts));
    await page.goto(base + "/#/status", { waitUntil: "load" });
    const box = width > 1000 ? "#ps-body" : "#ss-body";
    await page.waitForSelector(`${box} .bd .bd-foot`, { timeout: 15000 });
    const g = await page.evaluate((b) => ({ foot: document.querySelector(b + " .bd-foot").textContent, names: [...document.querySelectorAll(b + " .bd-lane")].map((l) => l.textContent).join(" | ") }), box);
    if (!/2 archived/.test(g.foot)) throw new Error("footer " + g.foot);
    if (!g.names.includes("docs-sweep")) throw new Error("not on the board: " + g.names.slice(0, 300));
  });

  await check(`board at ${width}px: a session parked mid-task says "paused" with resume, and the footer links to Archived`, async () => {
    const box = width > 1000 ? "#ps-body" : "#ss-body";
    await page.evaluate(() => { board.open.add("running"); for (const el of [$("ps-body"), $("ss-body")]) if (el) el._html = ""; renderStatus(); });
    const g = await page.evaluate((b) => { const t = document.querySelector(b + " .bd-park"); return { t: t && t.textContent, li: t && t.closest("li").textContent, btn: !!document.querySelector(b + " [data-bd-park=pk_paused0001]") }; }, box);
    if (g.t !== "paused" || !/briefing-board/.test(g.li) || !g.btn) throw new Error(JSON.stringify(g));
    await page.evaluate((b) => document.querySelector(b + ' .bd-lane[data-lane="running"]').scrollIntoView({ block: "start" }), box);
    await page.waitForTimeout(500); // the phone's Status screen slides in
    await page.screenshot({ path: `${out}/archived-board-${width}-${scheme}.png` });
    await page.click(`${box} .bd-foot a.bd-arch`);
    await page.waitForSelector("#archived:not([hidden]) #ar-list .ar-item", { timeout: 8000 });
    if ((await rowNames()).length !== 2) throw new Error("rows after the footer link " + (await rowNames()));
  });

  await check(`Resume and Propose removal at ${width}px: what each posts`, async () => {
    posts.length = 0;
    await page.click('#ar-list button[data-ar="resume"][data-id="h0000000000a1"]');
    await page.waitForFunction(() => /resumed its pane/.test(document.querySelector("#ar-msg").textContent), null, { timeout: 5000 });
    await page.click('#ar-list button[data-ar="propose"][data-id="h0000000000a2"]');
    await page.waitForFunction(() => /under Ready to remove/.test(document.querySelector("#ar-msg").textContent), null, { timeout: 5000 });
    const r = posts.find((x) => x.url === "/api/archived/restore"), m = posts.find((x) => x.url === "/api/marks");
    if (!r || r.body.ref !== "h0000000000a1" || r.body.resume !== true) throw new Error("resume " + JSON.stringify(posts));
    if (!m || m.body.agent_id !== "uitest/h0000000000a2:p1" || !m.body.reason) throw new Error("propose " + JSON.stringify(posts));
    await page.waitForFunction(() => /Nothing is archived/.test(document.querySelector("#ar-list").textContent), null, { timeout: 5000 });
    await noScroll();
  });

  await check(`Ready to remove and Recently closed at ${width}px: the Archived link; archived park records folded out`, async () => {
    rows = ROWS();
    await page.goto(base + "/#/remove", { waitUntil: "load" });
    await page.waitForFunction(() => /Archived \(3\)/.test(document.querySelector("#rv-arch")?.textContent || ""), null, { timeout: 15000 });
    await page.goto(base + "/", { waitUntil: "load" });
    await page.waitForSelector(".parked-sec .pk-arch", { timeout: 15000 });
    const g = await page.evaluate(() => ({ ids: [...document.querySelectorAll(".parked-sec .parked-row")].map((r) => r.dataset.pk), st: document.querySelector('.parked-row[data-pk="pk_paused0001"] .status')?.textContent,
      link: document.querySelector(".parked-sec .pk-arch").textContent }));
    if (g.ids.join() !== "pk_paused0001" || g.st !== "paused" || !/3 archived/.test(g.link)) throw new Error(JSON.stringify(g));
    await noScroll();
  });
  await browser.close();
}
const bad = results.filter((r) => !r.ok).length;
console.log(`${results.length - bad}/${results.length} archived checks passed`);
process.exit(bad ? 1 : 0);
