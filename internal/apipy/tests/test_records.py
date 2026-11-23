"""Unit tests of the comebye module's records (no hub needed).

Run by internal/apipy's Go test: python3 -m unittest discover -s tests,
with the module on PYTHONPATH.
"""

import datetime as dt
import re
import unittest

import comebye as cb
from comebye import _types
from comebye._record import _RECORDS

UTC = dt.timezone.utc


class Types(unittest.TestCase):
    def test_every_type_resolves(self):
        prim = {"str", "int", "float", "bool", "time", "any"}

        def check(owner, attr, typ):
            t = typ[:-1] if typ.endswith("?") else typ
            if t.startswith("[") or t.startswith("{"):
                return check(owner, attr, t[1:-1])
            self.assertTrue(t in prim or t in _RECORDS, "%s.%s: unknown type %r" % (owner, attr, typ))

        self.assertGreater(len(_types.RECORDS), 50)
        for name, cls in _types.RECORDS.items():
            for attr, (key, typ) in cls._fields.items():
                self.assertRegex(attr, r"^[A-Za-z_][A-Za-z0-9_]*$")
                check(name, attr, typ)

    def test_keyword_fields(self):
        self.assertEqual(cb.Entry._fields["from_"], ("from", "str"))
        self.assertEqual(cb.Entry({"from": "laharl/w1:p1"}).from_, "laharl/w1:p1")


class Records(unittest.TestCase):
    def test_strict(self):
        t = cb.Todo({"id": "t1", "question": "Land it?"})
        self.assertEqual(t.question, "Land it?")
        with self.assertRaises(AttributeError) as c:
            t.title
        msg = str(c.exception)
        self.assertIn("Todo has no field 'title'", msg)
        self.assertIn("did you mean 'question'", msg)
        self.assertIn("comebye api types Todo", msg)
        with self.assertRaises(AttributeError):
            t.question = "x"
        with self.assertRaises(TypeError) as c:
            t["question"]
        self.assertIn("use attributes", str(c.exception))
        with self.assertRaises(TypeError):
            {t}

    def test_absent_and_null_fields_are_empty(self):
        e = cb.Entry({"id": "h1", "previous_branches": None, "checks": None})
        self.assertEqual(e.final_report, "")
        self.assertEqual(e.report_seq, 0)
        self.assertIs(e.saw_working, False)
        self.assertEqual(e.previous_branches, [])
        self.assertIsNone(e.report_at)
        self.assertIsNone(e.checks)
        self.assertIsNone(e.mark)
        # A struct that is never null is an empty record, not None.
        self.assertIsInstance(e.live, cb.Live)
        self.assertIs(e.live.present, False)

    def test_nested(self):
        lg = cb.Ledger({
            "entries": [{"id": "h1", "live": {"present": True, "agent_status": "idle"},
                         "progress": {"text": "tests", "at": "2026-11-23T10:00:00Z"}}],
            "roles": {"laharl/w1:p1": {"role": "coordinator"}},
        })
        e = lg.entries[0]
        self.assertIsInstance(e, cb.Entry)
        self.assertEqual(e.live.agent_status, "idle")
        self.assertEqual(e.progress.at, dt.datetime(2026, 11, 23, 10, tzinfo=UTC))
        self.assertIsInstance(lg.roles["laharl/w1:p1"], cb.Role)
        self.assertEqual(lg.asks, {})
        self.assertEqual(cb.to_dict(lg.entries)[0]["id"], "h1")
        self.assertIn("progress", cb.fields(e))
        self.assertIn("question", cb.fields(cb.Todo))

    def test_repr(self):
        r = repr(cb.Todo({"id": "t1", "question": "x" * 100, "state": "open"}))
        self.assertTrue(r.startswith("Todo(id='t1', question='xxx"), r)
        self.assertLess(len(r), 200)


class Times(unittest.TestCase):
    def test_parse(self):
        p = cb.parse_time
        self.assertEqual(p("2026-11-23T10:02:05.968994855-05:00"), dt.datetime(2026, 11, 23, 15, 2, 5, 968994, tzinfo=UTC))
        self.assertEqual(p("2026-11-23T10:02:05Z"), dt.datetime(2026, 11, 23, 10, 2, 5, tzinfo=UTC))
        self.assertEqual(p("2026-11-23T10:02:05+05:30"), dt.datetime(2026, 11, 23, 4, 32, 5, tzinfo=UTC))
        self.assertIsNone(p("0001-01-01T00:00:00Z"))
        self.assertIsNone(p(""))
        self.assertIsNone(p(None))
        with self.assertRaises(ValueError):
            p("yesterday")

    def test_ago(self):
        now = cb.now()
        self.assertEqual(now.tzinfo, UTC)
        self.assertEqual(cb.ago(now - dt.timedelta(hours=3, minutes=5)), "3h ago")
        self.assertEqual(cb.ago(now - dt.timedelta(days=2)), "2d ago")
        self.assertRegex(cb.ago(now - dt.timedelta(seconds=5)), r"^\d+s ago$")
        self.assertEqual(cb.ago(None), "never")
        self.assertIsNone(cb.age(None))


class LatestReply(unittest.TestCase):
    def test_newer_last_pass_wins(self):
        e = cb.Entry({"final_report": "old", "report_at": "2026-11-20T00:00:00Z",
                      "last_pass_report": "new", "last_pass_report_at": "2026-11-23T00:00:00Z"})
        r = cb.latest_reply(e)
        self.assertEqual((r.text, r.kind), ("new", "last_pass_report"))

    def test_report_wins_when_newer(self):
        e = cb.Entry({"final_report": "new", "report_at": "2026-11-23T00:00:00Z",
                      "last_pass_report": "old", "last_pass_report_at": "2026-11-20T00:00:00Z"})
        self.assertEqual(cb.latest_reply(e).kind, "report")

    def test_none(self):
        self.assertEqual(cb.latest_reply(cb.Entry({})), ("", None, ""))


class Offline(unittest.TestCase):
    def test_no_connection(self):
        # Imported outside `comebye api run`: a clear error, not a crash.
        if cb._conn.base:
            self.skipTest("running under comebye api run")
        with self.assertRaises(cb.HubError) as c:
            cb.whoami()
        self.assertIn("comebye api run", str(c.exception))

    def test_get_is_api_only(self):
        with self.assertRaises(ValueError):
            cb.get("https://example.com/")


if __name__ == "__main__":
    unittest.main()
