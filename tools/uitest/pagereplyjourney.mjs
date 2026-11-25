// Answering a Page as one journey (#139 slice 6): four scripts, now parts run one after the other in one process and
// one Chrome, against the hub run.sh started, each publishing its Pages with the real CLI into that hub's state:
//   pagecomments   select, comment, send, and what the viewer refuses from a hostile Page;
//   pagefixed      the Page does not move while it is commented on;
//   pagerevisions  data-revision blocks outlined from the Page menu;
//   pagechat       "Chat about this": the button, the menu item, the toast, the refusals.
//
//   node pagereplyjourney.mjs BASE OUT_DIR    (env: COMEBYE_BIN, COMEBYE_STATE_DIR, SCHEME)
import { journey } from "./journey.mjs";

const [base, out] = process.argv.slice(2);
await journey("pagereplyjourney", ["pagecomments", "pagefixed", "pagerevisions", "pagechat"].map((part) => ({ part, argv: [base, out] })));
