"""The steward pass against a real, throwaway hub (internal/cli/steward_e2e_test.go).

Run with `comebye api run tests/pass_check.py` from tools/steward, with the
fake gh (tests/fake_gh.py) first on PATH: the test starts a hub on its own
state dir, seeds the ledger and the to-dos, then runs this file.
"""

import importlib.util
import json
import os
import tempfile
import unittest

import comebye as cb

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("steward_pass", os.path.join(HERE, "..", "pass.py"))
sp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sp)

NOTES = os.path.join(tempfile.mkdtemp(prefix="steward-notes"), "notes.md")
CONF = {
    "notes": NOTES,
    "prs": [
        {"repo": "OWNER/REPO"},
        {"repo": "OWNER/OTHER", "ignore_authors": ["app/renovate*"]},
        {"repo": "ORG/UP", "author": "@me", "flag_after_hours": 72},
    ],
    "plans": [{"repo": "OWNER/REPO"}],
    "comment_watches": [{"repo": "ORG/UP", "number": 5, "stale_hours": 24}],
    "run_watches": [{"repo": "ORG/UP", "branch": "main", "workflow": "Tests", "event": "push", "jobs": ["E2E"]}],
}


def run_pass():
    with open(NOTES, "w") as f:
        f.write("# notes\n- pass 1: 1 finished + 1 ready_to_remove = 2\n- (pass 1) waiting on OWNER/OTHER#99\n")
    sp.CONF.clear()
    sp.CONF.update(sp.DEFAULTS)
    sp.CONF.update(CONF)
    return sp.Pass(sp.CONF).run()


class StewardPass(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.p = run_pass()
        cls.f = {f["title"]: f for f in cls.p.findings}
        cls.w = {f["title"]: f for f in cls.p.watch}

    def find(self, needle, where=None):
        hits = [f for t, f in (where or self.f).items() if needle in t]
        self.assertEqual(len(hits), 1, "%r in %s" % (needle, json.dumps(list((where or self.f)), indent=1)))
        return hits[0]

    def test_no_errors(self):
        self.assertEqual(self.p.errors, [])

    def test_prs(self):
        red = self.find("OWNER/REPO#13")
        self.assertEqual(red["who"], "user")
        self.assertIn("CI red (test)", red["evidence"])
        self.assertIn("worker: none", red["evidence"])
        self.assertEqual(red["url"], "https://github.com/OWNER/REPO/pull/13")
        self.assertTrue(red["age"].startswith("2h") or red["age"].startswith("3h"), red["age"])
        # A to-do already asks about #12: watched, not a finding.
        covered = self.find("OWNER/REPO#12", self.w)
        self.assertEqual(covered["why_watch"], "covered")
        self.assertTrue(covered["covered"][0].startswith("t"), covered["covered"])
        self.find("OWNER/REPO#14", self.w)  # under an hour
        on = self.find("OWNER/REPO#15")
        self.assertEqual(on["who"], "coord")
        self.assertIn("worker: retry-me (running)", on["evidence"])
        # Ignored author: only the red one.
        self.find("OWNER/OTHER#31")
        self.assertFalse([t for t in list(self.f) + list(self.w) if "OWNER/OTHER#30" in t])
        # The user's drafts: old or red only.
        self.find("ORG/UP#20")
        self.find("ORG/UP#22")
        self.assertFalse([t for t in list(self.f) + list(self.w) if "ORG/UP#21" in t])

    def test_plans(self):
        self.find("OWNER/REPO#40 fully ticked (2/2)")
        due = self.find("OWNER/REPO#42")
        self.assertIn("the next slice", due["evidence"])
        self.assertIn("body says: The user approved all slices", due["evidence"])
        self.assertTrue(any("OWNER/REPO#41: 1/3 ticked, the rest marked later" in q for q in self.p.quiet), self.p.quiet)
        # A slice a running worker names is watched, not flagged.
        on = self.find("OWNER/REPO#44", self.w)
        self.assertEqual(on["why_watch"], "a worker is running on it")
        self.assertIn("retry-me (running)", on["evidence"])

    def test_stuck(self):
        self.find("nap-loop: pass overdue")
        self.find("decider finished with decisions for the user")
        self.find("failed handoff h00000abc009 (launch-fail)")
        self.assertTrue(any("was retried as retry-me" in q for q in self.p.quiet), self.p.quiet)
        never = self.find("answered to-do t0000000002")
        self.assertIn("ghost-worker has not acted", never["title"])
        self.assertIn("Land PR #7?", never["evidence"])
        # The to-do whose worker landed afterwards counts as acted on.
        self.assertFalse([t for t in self.f if "landed-worker" in t])
        # An approved review has nothing left to do; one asking for changes still has.
        self.assertFalse([t for t in self.f if "approved-worker" in t])
        self.find("answered to-do t0000000005: changes-worker has not acted")
        self.find("needs-input blocked", self.w)  # it blocked just now: watched for an hour first
        self.find("queued prompt", self.w)
        self.assertTrue(any(q.startswith("to-dos: 1 open; t0000000001 followup") for q in self.p.quiet), self.p.quiet)

    def test_watches(self):
        c = self.find("ORG/UP#5: maintainer comment unanswered")
        self.assertIn("by maintainer", c["evidence"])
        self.assertIn("head abcdef01", c["evidence"])
        g = self.find("ORG/UP main Tests/E2E is GREEN")
        self.assertEqual(g["url"], "https://github.com/ORG/UP/actions/runs/777")

    def test_counts_and_text(self):
        c = self.p.counts
        self.assertEqual(c["previous"], 2)
        self.assertEqual(c["change"], c["pileup"] - 2)
        out = self.p.text()
        self.assertIn("FINDINGS (%d)" % len(self.p.findings), out)
        self.assertIn("PILE-UP", out)
        self.assertNotIn("ERRORS", out)
        d = json.loads(json.dumps(self.p.as_dict(), default=str))
        self.assertGreater(d["calls"]["gh"], 0)
        self.assertTrue(d["report_at_known"])

    def test_helpers(self):
        self.assertEqual(sp.dur(70), "2d22h")
        self.assertEqual(sp.dur(0.5), "30m")
        self.assertEqual(sp.dur(5.25), "5h15m")
        self.assertEqual(sp.ci_state([]), ("none", []))
        self.assertEqual(sp.ci_state([{"name": "a", "status": "IN_PROGRESS"}])[0], "pending")


if __name__ == "__main__":
    unittest.main(verbosity=2)
