// The left nav as one journey (#139 slice 6): its four slices' scripts, now parts run one after the other in one process
// and one Chrome, against the hub run.sh started (each part answers the list with a fleet of its own in the page):
//   leftnav     the nav rows as one row of icon buttons (slice 1);
//   hostpicker  the host picker (slice 2);
//   listfilter  the status tabs and the filter box (slice 3);
//   groupby     the Group by menu (slices 4 and 5).
//
//   node navjourney.mjs BASE OUT_DIR      (env: SCHEME; PARTS="a b" runs only those parts)
import { journey } from "./journey.mjs";

const [base, out] = process.argv.slice(2);
await journey("navjourney", ["leftnav", "hostpicker", "listfilter", "groupby"].map((part) => ({ part, argv: [base, out] })));
