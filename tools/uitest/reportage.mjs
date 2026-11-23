// Fresh reports: every report view in the app says how old the report is, and a newer reply the worker left
// without filing a report (a loop pass, or a turn after its report) is shown under it, labelled as not a report.
// The ledger is the shared server's, with entries added by route.
//
//   node reportage.mjs BASE OUT_DIR      (env: SCHEME)
import { launch } from "./lib.mjs";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
const dark = process.env.SCHEME !== "light";
const { browser, page } = await launch({ width: 1280, height: 800, dark });
page.setDefaultTimeout(8000);
const log = [];
page.on("pageerror", (e) => log.push("pageerror: " + e.message));
const results = [];
async function check(name, fn) {
  try { await fn(); results.push({ name, ok: true }); console.log("ok   " + name); }
  catch (e) { results.push({ name, ok: false }); console.log("FAIL " + name + "\n     " + String(e.message).split("\n").join("\n     ")); }
  await page.screenshot({ path: `${out}/reportage-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
}
const same = (got, want, what) => { if (got !== want) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`); };
const ago = (min) => new Date(Date.now() - min * 60e3).toISOString();

const STALE = { id: "rafx1", host: "uitest", repo_name: "comebye", branch: "fix/ci-flake", agent_name: "ci-flake", status: "finished",
  created_at: ago(300), finished_at: ago(10), final_report: "## Summary\nCI flaked. STALE-MARK", report_source: "explicit", report_seq: 3,
  report_at: ago(120), last_pass_report: "CI is green now. NEWER-MARK", last_pass_report_at: ago(10), live: { present: false } };
const OLD = { id: "rafx2", host: "uitest", repo_name: "comebye", branch: "fix/old", agent_name: "old", status: "finished",
  created_at: ago(300), finished_at: ago(20), final_report: "Old report.", report_source: "explicit", report_seq: 1, live: { present: false } };

await page.route(/\/api\/ledger$/, async (r) => {
  const j = await (await r.fetch()).json();
  j.entries = [...(j.entries || []), STALE, OLD];
  r.fulfill({ json: j });
});

async function statusRow(id) {
  await page.goto("about:blank");
  await page.goto(base + "/#/status", { waitUntil: "load" });
  const d = page.locator(`#ps-body details[data-id="${id}"]`);
  await d.locator("summary").waitFor();
  if (!(await d.evaluate((x) => x.open))) await d.locator("summary").click();
  await d.locator(".md").first().waitFor();
  return d;
}

await check("Status: a report says its number and age, and a newer reply follows it", async () => {
  const d = await statusRow("rafx1");
  same(await d.locator(".rep-age").first().innerText(), "report 3 from 2h ago · newer reply 10m ago", "age chip");
  const newer = d.locator(".rep-newer");
  same(await newer.count(), 1, "newer reply blocks");
  const head = await newer.locator(".rep-newer-h").innerText();
  if (!/^Newer: its last reply, 10m ago \(.*not a report\)$/.test(head)) throw new Error("newer heading: " + head);
  if (!(await newer.locator(".md").innerText()).includes("NEWER-MARK")) throw new Error("newer reply text missing");
  const order = await d.evaluate((x) => x.innerHTML.indexOf("STALE-MARK") < x.innerHTML.indexOf("NEWER-MARK"));
  same(order, true, "report first, newer reply after");
});

await check("Status: a report from before report times says its age is unknown, with no newer block", async () => {
  const d = await statusRow("rafx2");
  same(await d.locator(".rep-age").first().innerText(), "report age unknown", "age chip");
  same(await d.locator(".rep-newer").count(), 0, "newer reply blocks");
});

fs.writeFileSync(`${out}/reportage-console.log`, log.join("\n"));
await browser.close();
const bad = results.filter((r) => !r.ok).length + (log.length ? 1 : 0);
if (log.length) console.log("page errors:\n" + log.join("\n"));
console.log(`${results.length - results.filter((r) => !r.ok).length}/${results.length} reportage checks passed`);
process.exit(bad ? 1 : 0);
