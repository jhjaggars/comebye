// The side panel as one journey (#139 slice 6): its tabs' three scripts, now parts run one after the other in one process
// and one Chrome, against one hub: run.sh's side-panel hub, whose fake herdr has an agent "panel" (w7:p1) in a real git
// worktree beside the usual ones.
// sidepanel goes first, on the hub as it was started (as sidepanel.mjs ran): after the others it failed 2 to 3 in 10.
//   sidepanel    the Doc and Diff tabs, with the real CLI declaring the document (the "panel" agent);
//   statussplit  Status as the third tab (two of the hub's other agents);
//   pagessplit   the agent's Pages as the fourth tab (two of its other agents, publishing with the real CLI).
//
//   node paneljourney.mjs BASE OUT_DIR    (env: PANEL_HOME, COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME, WIDTHS)
import { journey } from "./journey.mjs";

const [base, out] = process.argv.slice(2);
await journey("paneljourney", ["sidepanel", "statussplit", "pagessplit"].map((part) => ({ part, argv: [base, out] })));
