// Puts the dev server's ledger into the states the fixtures need, through
// the app's own API: a coordinator, a session marked ready to remove, and a
// briefing. Run by dev.sh after the server is up: node seed.mjs BASE
const base = process.argv[2];
async function call(method, path, body) {
  const r = await fetch(base + path, { method, headers: { "content-type": "application/json" }, body: body && JSON.stringify(body) });
  if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${await r.text()}`);
  return r.json().catch(() => ({}));
}
const { agents } = await call("GET", "/api/agents");
const id = (name) => {
  const a = agents.find((x) => x.name === name);
  if (!a) throw new Error("fixture agent missing: " + name);
  return a.id;
};
await call("POST", "/api/roles", { agent_id: id("laharl"), role: "coordinator" });
await call("POST", "/api/marks", { agent_id: id("old-worker"), reason: "merged; safe to remove", by: "laharl" });
await call("POST", "/api/brief", { markdown: [
  "## comebye",
  "- **fix-detail-menu** is running: the agent view's ⋯ menu.",
  "- **old-worker** finished and is marked ready to remove.",
  "",
  "## notes",
  "- **needs-input** is blocked on a question: which cache database?",
].join("\n") });
console.log("seeded: coordinator laharl, mark old-worker, briefing");
