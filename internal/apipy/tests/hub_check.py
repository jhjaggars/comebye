"""The module against a real, throwaway hub (internal/cli/api_e2e_test.go).

Run with `comebye api run tests/hub_check.py`: the test starts a hub on its
own state dir with a fake herdr, seeds the ledger, a to-do, a queued prompt
and a Page, then runs this file.
"""

import unittest

import comebye as cb


class Hub(unittest.TestCase):
    def test_whoami(self):
        me = cb.whoami()
        self.assertEqual((me.role, me.host, me.hub), ("agent", "devbox", "self"))

    def test_agents(self):
        names = {a.name for a in cb.agents()}
        self.assertIn("alpha", names)
        self.assertEqual(cb.agent("alpha").id, "devbox/w1:p1")
        self.assertEqual(cb.agent("devbox/w1:p1").name, "alpha")
        self.assertEqual([h.name for h in cb.hosts()], ["devbox"])
        with self.assertRaises(cb.NotFound):
            cb.agent("nobody")

    def test_ledger(self):
        self.assertEqual({e.agent_name for e in cb.entries(status="finished")}, {"old-worker", "ghost-worker"})
        self.assertEqual([e.agent_name for e in cb.entries(loop=True)], ["pr-watcher"])
        old = cb.entry("old-worker")
        self.assertTrue(old.report_age.startswith("report from 2 d"), old.report_age)
        self.assertEqual(cb.latest_reply(old).kind, "report")
        self.assertGreater(cb.age(old.report_at).days, 1)
        w = cb.entry("pr-watcher")
        self.assertTrue(w.newer_reply_age, "a newer reply")
        r = cb.latest_reply(w)
        self.assertEqual((r.kind, r.text), ("last_pass_report", "pass 9: PR #12 green"))
        run = cb.entry("fix-detail-menu")
        self.assertTrue(run.live.present)
        self.assertEqual(run.progress.text, "running the browser tests")
        self.assertIsInstance(cb.ledger().roles, dict)
        with self.assertRaises(cb.NotFound):
            cb.entry("nope")

    def test_todos(self):
        open_ = cb.todos("open")
        t = [t for t in open_ if t.question == "Land PR #12?"]
        self.assertEqual(len(t), 1, open_)
        t = t[0]
        self.assertEqual(cb.todo(t.id).question, "Land PR #12?")
        self.assertEqual(t.kind, "followup")
        self.assertGreaterEqual(cb.todo_list().open, 1)
        with self.assertRaises(AttributeError):
            t.title
        with self.assertRaises(cb.NotFound):
            cb.todo("t0000000000")

    def test_queue(self):
        q = cb.queue()
        self.assertEqual([(p.name, p.text) for p in q], [("needs-input", "please check the flaky test")])
        self.assertEqual(cb.queue(agent="devbox/w1:p1"), [])

    def test_pages(self):
        p = [p for p in cb.pages() if p.title == "API demo page"]
        self.assertEqual(len(p), 1)
        v = cb.page(p[0].id, text=True)
        self.assertIn("Hello", v.body)
        self.assertEqual(v.path, "/p/" + p[0].id)
        self.assertIsNone(cb.page(p[0].id).body)

    def test_more(self):
        self.assertEqual(cb.archived().count, 0)
        self.assertIsInstance(cb.log(since="2d", limit=5), list)
        self.assertIsInstance(cb.coordinators(), list)
        self.assertIn("version", cb.get("/api/version"))

    def test_forbidden(self):
        with self.assertRaises(cb.Forbidden) as c:
            cb.removals()
        self.assertEqual(c.exception.status, 403)
        self.assertIn("not open to the agent role", str(c.exception))


if __name__ == "__main__":
    unittest.main(verbosity=2)
