"""comebye: read the hub from a Python script (`comebye api run`).

Read-only: every call is a GET, made as the agent role with the agent token
of the CLI's own config. Run `comebye api --help` for the guide and
`comebye api types [NAME...]` for the records.
"""

import collections
import datetime as _dt
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

if sys.version_info < (3, 8):
    raise SystemExit("comebye api: needs Python 3.8 or newer, this is %d.%d" % sys.version_info[:2])

from . import _types
from ._record import Record, fields, parse_time, register, to_dict
from ._types import *  # noqa: F401,F403 (the record classes)
from . import gh  # noqa: F401

register(_types.RECORDS)

__all__ = [
    "HubError", "Forbidden", "NotFound", "Record", "to_dict", "fields", "parse_time",
    "now", "age", "ago", "whoami", "agents", "agent", "hosts", "ledger", "entries", "entry",
    "marks", "latest_reply", "todos", "todo_list", "todo", "queue", "pages", "page", "archived",
    "log", "coordinators", "removals", "get", "gh",
] + sorted(_types.RECORDS)


# ---- errors ----

class HubError(Exception):
    """A hub call that failed: status (0 when unreachable), code, message, path."""

    def __init__(self, path, status, code, message):
        self.path, self.status, self.code, self.message = path, status, code, message
        super().__init__(self._text())

    def _text(self):
        where = "GET " + self.path
        if self.status == 0:
            return "%s: %s" % (where, self.message)
        detail = self.message or self.code or "HTTP %d" % self.status
        return "%s: %d %s" % (where, self.status, detail)


class Forbidden(HubError):
    """403: the route is not open to the agent role (removals, releases, usage...)."""

    def _text(self):
        return ("GET %s: 403, not open to the agent role a script runs as (the hub: %s)"
                % (self.path, self.message or self.code or "forbidden"))


class NotFound(HubError):
    """404: no such id, or the feature is off on this hub."""


# ---- the connection (from `comebye api run`) ----

class _Conn:
    def __init__(self):
        self.base = os.environ.get("COMEBYE_API_URL", "").rstrip("/")
        self.pane = os.environ.get("COMEBYE_API_PANE", "")
        self.host = os.environ.get("COMEBYE_API_HOST", "")
        self.token_err = os.environ.get("COMEBYE_API_TOKEN_ERROR", "")
        self._token = None
        fd = os.environ.get("COMEBYE_API_TOKEN_FD")
        if fd:
            try:
                with os.fdopen(int(fd), "rb") as f:
                    self._token = f.read().decode().strip()
            except (OSError, ValueError) as e:
                self.token_err = "cannot read the agent token from comebye: %s" % e
        for k in ("COMEBYE_API_TOKEN_FD",):
            os.environ.pop(k, None)
        self._opener = urllib.request.build_opener(_NoRedirect, urllib.request.ProxyHandler({}))

    def get(self, path, query=None):
        if not self.base:
            raise HubError(path, 0, "", "no hub URL: run the script with `comebye api run FILE`")
        if self._token is None:
            raise HubError(path, 0, "", self.token_err or "no agent token: run the script with `comebye api run FILE`")
        q = {k: v for k, v in (query or {}).items() if v is not None and v is not False and v != ""}
        url = self.base + path + ("?" + urllib.parse.urlencode(q, doseq=True) if q else "")
        req = urllib.request.Request(url, method="GET")
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "comebye-api/1")
        req.add_header("X-Comebye-Agent-Token", self._token)
        if self.pane:
            req.add_header("X-Comebye-Agent", self.pane)
        if self.host:
            req.add_header("X-Comebye-Agent-Host", self.host)
        try:
            with self._opener.open(req, timeout=200) as r:
                data = r.read()
        except urllib.error.HTTPError as e:
            body = e.read()
            code, msg = "", ""
            try:
                m = json.loads(body)
                code, msg = m.get("code", ""), m.get("message", "")
            except (ValueError, AttributeError):
                msg = body.decode("utf-8", "replace").strip()[:300]
            cls = {403: Forbidden, 404: NotFound}.get(e.code, HubError)
            raise cls(path, e.code, code, msg) from None
        except (urllib.error.URLError, OSError) as e:
            raise HubError(path, 0, "", "cannot reach Comebye at %s: is it running? (%s)" % (self.base, getattr(e, "reason", e))) from None
        try:
            return json.loads(data)
        except ValueError:
            raise HubError(path, 200, "", "the answer is not JSON") from None


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # The hub never redirects; a redirect would carry the token elsewhere.
    def redirect_request(self, *a, **k):
        return None


_conn = _Conn()


def get(path, **query):
    """GET any hub route as plain JSON (dicts and lists, no records).

    For routes this module has no function for. path starts with /api/;
    keyword arguments become the query string.
    """
    if not path.startswith("/api/"):
        raise ValueError("path must start with /api/, got %r" % (path,))
    return _conn.get(path, query)


def _hook(kind, value, tb):
    # A hub error ends the script with its one line, not a traceback.
    if issubclass(kind, HubError):
        sys.stderr.write("comebye api: %s\n" % value)
        sys.stderr.flush()
        return
    sys.__excepthook__(kind, value, tb)


sys.excepthook = _hook


# ---- time ----

def now():
    """The current time, timezone-aware UTC (compare record times with it)."""
    return _dt.datetime.now(_dt.timezone.utc)


def age(t):
    """How long ago t was (a timedelta); t is a datetime or an RFC 3339 string. None stays None."""
    t = parse_time(t)
    return None if t is None else now() - t


def ago(t):
    """t as a short age: "40s ago", "12m ago", "3h ago", "2d ago" ("never" for None)."""
    d = age(t)
    if d is None:
        return "never"
    s = int(d.total_seconds())
    if s < 0:
        return "in the future"
    for unit, n in (("d", 86400), ("h", 3600), ("m", 60)):
        if s >= n:
            return "%d%s ago" % (s // n, unit)
    return "%ds ago" % s


# ---- the hub ----

def whoami():
    """Who this script is to the hub: Whoami (host, role, agent, hub, version)."""
    return Whoami(get("/api/whoami"))


def _agents():
    return Agents(get("/api/agents"))


def agents(host=None, name=None):
    """Every agent on every host: [Agent]. host= and name= filter."""
    out = _agents().agents
    if host is not None:
        out = [a for a in out if a.host == host]
    if name is not None:
        out = [a for a in out if a.name == name]
    return out


def agent(ref):
    """One agent by id (host/pane), pane id or name. NotFound if none, ValueError if a name is ambiguous."""
    al = _agents().agents
    for a in al:
        if a.id == ref or a.pane_id == ref:
            return a
    hits = [a for a in al if a.name == ref]
    if len(hits) > 1:
        raise ValueError("%d agents are named %r: use the id (%s)" % (len(hits), ref, ", ".join(a.id for a in hits)))
    if not hits:
        raise NotFound("/api/agents", 404, "not_found", "no agent %r" % (ref,))
    return hits[0]


def hosts():
    """Every host's status: [Host]."""
    return _agents().hosts


_ledger_cache = None


def ledger(refresh=False):
    """The hub's ledger: Ledger (entries, marks, plans, roles, asks).

    One GET per script run: later calls reuse it unless refresh=True
    (the ledger can be large).
    """
    global _ledger_cache
    if _ledger_cache is None or refresh:
        _ledger_cache = Ledger(get("/api/ledger"))
    return _ledger_cache


ACTIVE = ("running", "finished", "landing", "landed", "ready_to_remove", "archived")


def entries(status=None, active=None, repo=None, name=None, loop=None):
    """Ledger entries (handoffs): [Entry], oldest first.

    status: one status or a list ("waiting", "running", "finished", "landing",
    "landed", "ready_to_remove", "removed", "failed", "lost", "archived");
    active=True: the statuses with a live worktree (ACTIVE); repo: repo_name;
    name: agent_name; loop=True: standing loops only.
    """
    out = ledger().entries
    if status is not None:
        want = {status} if isinstance(status, str) else set(status)
        out = [e for e in out if e.status in want]
    if active is not None:
        out = [e for e in out if (e.status in ACTIVE) == bool(active)]
    if repo is not None:
        out = [e for e in out if e.repo_name == repo]
    if name is not None:
        out = [e for e in out if e.agent_name == name]
    if loop is not None:
        out = [e for e in out if bool(e.loop) == bool(loop)]
    return out


def entry(ref):
    """The newest entry whose id, agent id, agent name or branch is ref. NotFound if none."""
    for e in reversed(ledger().entries):
        if ref in (e.id, e.agent_id, e.agent_name, e.branch):
            return e
    raise NotFound("/api/ledger", 404, "not_found", "no ledger entry %r" % (ref,))


def marks():
    """Standalone marks (sessions proposed for removal without a ledger entry): [Mark]."""
    return ledger().marks


Reply = collections.namedtuple("Reply", "text at kind")


def latest_reply(e):
    """The newest thing an entry's worker said, as Reply(text, at, kind).

    kind is "report" (final_report, at report_at) or "last_pass_report"
    (a loop's or a re-finished worker's last reply, when it is newer than
    the report). Reply("", None, "") when there is neither. Check `at`: a
    report can be days old (see Entry.report_age).
    """
    rep = Reply(e.final_report, e.report_at, "report") if e.final_report else None
    lp = Reply(e.last_pass_report, e.last_pass_report_at, "last_pass_report") if e.last_pass_report else None
    if lp and (rep is None or (lp.at and (rep.at is None or lp.at > rep.at))):
        return lp
    return rep or Reply("", None, "")


def todo_list(state=None):
    """The to-do list with its counts: TodoList (todos, open, answered, auto).

    state: None (open and answered), "open", "answered", "done", "dropped" or "all".
    """
    return TodoList(get("/api/todos", state=state))


def todos(state=None):
    """To-dos: [Todo]. state as in todo_list. A to-do's text is .question."""
    return todo_list(state).todos


def todo(id):
    """One to-do by id (t + 10 hex): Todo. NotFound if none."""
    return TodoView(get("/api/todos/" + urllib.parse.quote(id, safe=""))).todo


def queue(agent=None, settled=False):
    """Prompts queued with `comebye prompt --queue`: [QueuedPrompt]. settled=True includes delivered/cancelled ones."""
    return Queue(get("/api/queue", agent=agent, all="1" if settled else None)).queue


def pages(mine=False, handoff=None):
    """Published Pages (metadata, no bodies): [Page]. mine=True: this agent's; handoff=: one handoff's."""
    return PageList(get("/api/pages", mine="1" if mine else None, handoff=handoff)).pages


def page(id, body=False, text=False, version=None):
    """One Page: PageView (page, path, url; body with body=True, as plain text with text=True).

    An agent reads bodies of its own host's Pages only (Forbidden otherwise).
    """
    q = {}
    if body or text:
        q["body"] = "1"
    if text:
        q["text"] = "1"
    if version is not None:
        q["version"] = str(version)
    return PageView(get("/api/pages/" + urllib.parse.quote(id, safe=""), **q))


def archived():
    """Archived sessions, newest first: Archived (archived, count, build_freed, worktree_bytes)."""
    return Archived(get("/api/archived"))


def log(since="24h", kind=None, limit=None):
    """What the hub handled itself, newest first: [Event].

    since: a duration ("90m", "24h", "2d") or an RFC 3339 time; kind: one
    kind or a list; limit: at most this many.
    """
    if kind is not None and not isinstance(kind, str):
        kind = ",".join(kind)
    return Log(get("/api/log", since=since, kind=kind, limit=None if limit is None else str(limit))).events


def coordinators():
    """The coordinators the hub knows: [Coordinator]."""
    return Coordinators(get("/api/coordinators")).coordinators


def removals():
    """Sessions marked ready to remove, with what automatic removal would decide: Removals.

    The user's route: an agent gets Forbidden (403). Reads only (?dry=1):
    it never re-asks a marked session.
    """
    return Removals(get("/api/removals", dry="1"))
