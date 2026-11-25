// The Page viewer as one journey (#139 slice 6): four scripts, now parts run one after the other in one process and one
// Chrome, against the hub run.sh started, each publishing its Pages with the real CLI into that hub's state:
//   pages       the sandbox, the screen and its cards (the smoke set's Pages script);
//   pageback    Back returns to where you came from;
//   pagescript  the Pages that run JavaScript are flagged;
//   pagetheme   a Page paints in the app's mode from its first frame.
// SMOKE=1 (tools/ci/browser.sh smoke) runs the pages part only, as the smoke set ran pages.mjs alone.
//
//   node pagesjourney.mjs BASE OUT_DIR    (env: COMEBYE_BIN, COMEBYE_STATE_DIR, PAGES_URL for pages, SCHEME, WIDTHS)
import { journey } from "./journey.mjs";

const [base, out] = process.argv.slice(2);
// PAGES_URL was run.sh's for pages.mjs alone
const parts = ["pagescript", "pageback", "pages", "pagetheme"].map((part) => ({ part, argv: [base, out], env: part === "pages" ? {} : { PAGES_URL: undefined } }));
await journey("pagesjourney", process.env.SMOKE ? parts.filter((p) => p.part === "pages") : parts);
