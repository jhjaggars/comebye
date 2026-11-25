// Stopped workers and adding hosts as one journey (#139 slice 6): seven scripts, now parts run one after the other in
// one process and one Chrome, on run.sh's fake herdr. Each part still starts the servers it needs (a hub and peers, a
// provisioner running a stand-in for the worker tool) and stops them when it ends: their set-ups differ in what is
// under test (create on or off, one worker config or two, a default provisioner or none, workers up or stopped), so
// one hub for all seven would change what they check.
//   provision, provhosts       adding a host (slices 1 and 3);
//   provkubevirt               Add host and Destroy… for a KubeVirt worker;
//   wake, wakeuse, wakepolish  waking a stopped worker (#66 slices 1, 3 and 4);
//   multiconfig                one provisioner with two worker configs.
//
//   node fleetjourney.mjs OUT_DIR     (env: COMEBYE_BIN, FAKE_SOCK, SCHEME)
import { journey } from "./journey.mjs";

const [out] = process.argv.slice(2);
await journey("fleetjourney", ["provision", "provhosts", "provkubevirt", "wake", "multiconfig", "wakeuse", "wakepolish"].map((part) => ({ part, argv: [out] })));
