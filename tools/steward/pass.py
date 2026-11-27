"""The steward's pass: every check of its brief in one read-only script.

    comebye api run tools/steward/pass.py --config STEWARD.json [--json]

It gathers and pre-checks; it decides nothing and writes nothing. It prints
the findings a pass should judge (each with its evidence: ids, ages, URLs),
what it is watching, what was quiet, and the pile-up count. The steward reads
that, decides what to file (comebye todo add) or tell the coordinator
(comebye prompt), and keeps its own notes.

The checks (the steward's brief):
  1. prs      open PRs per configured repo: age, CI, a to-do about it, a worker on it
  2. plans    open issues with a Slices checklist: due unticked slices, fully ticked but open
  3. stuck    running workers with no progress, overdue loops, blocked agents with no to-do,
              finished workers with decisions and no to-do, prompts queued over an hour,
              failed handoffs, answered to-dos whose agent never acted
  4. release  release to-dos older than a day, hosts not on one version, hosts down
  5. pileup   finished + ready_to_remove (+ archived), and the change since the notes' last count
  plus the standing watches the config lists: maintainer comments on a PR with no
  reply, and named jobs of a branch's latest workflow run.

Schedule: the steward is a standing loop (`comebye loop set NAME --every 1h
--text "run a steward pass"`). The hub sends each pass as a prompt when it is
due; the steward runs this script, acts, and stops. It schedules no wakeup
or cron of its own.

All times are UTC; ages come from the hub's timezone-aware fields. Repos,
watches and thresholds live in the config (see config.example.json), never
here: this file is public.
"""

import argparse
import concurrent.futures
import fnmatch
import json
import os
import re
import shutil
import subprocess
import sys
import time

import comebye as cb

DEFAULTS = {
    "notes": "",                  # the steward's notes file: keys found there are tagged "in notes"
    "min_age_hours": 1,           # younger than this: watch, don't flag
    "prs": [],                    # [{repo, author?, ignore_authors?, flag_after_hours?, only_red_or_old?}]
    "plans": [],                  # [{repo}]
    "quiet_hours": 3,             # a running worker with no progress note or activity for this long
    "queue_hours": 1,
    "answered_window_hours": 48,  # answered to-dos this recent are checked for an agent that acted
    "failed_window_hours": 48,
    "decisions_window_hours": 72, # finished workers this recent are checked for decisions with no to-do
    "release_hours": 24,
    "host_down_hours": 1,
    "host_probes": {},            # {host: [argv]}: a read-only command whose output explains a down host
    "comment_watches": [],        # [{repo, number, stale_hours}]: a maintainer's comment with no reply
    "run_watches": [],            # [{repo, branch, workflow, event?, jobs: [names]}]
    "gh_timeout": 60,
}

STATS = {"hub": 0, "gh": 0, "gh_seconds": 0.0}


# ---- small helpers ----

def hours(t):
    a = cb.age(t)
    return None if a is None else a.total_seconds() / 3600


def dur(h):
    """An age in hours as 45m, 5h10m or 3d2h: never rounded up (70h is 2d22h, not 3d)."""
    if h is None:
        return "?"
    m = int(h * 60)
    if m < 60:
        return "%dm" % max(m, 0)
    if m < 48 * 60:
        return "%dh%02dm" % (m // 60, m % 60) if m % 60 else "%dh" % (m // 60)
    return "%dd%dh" % (m // 1440, (m % 1440) // 60)


def utc(t):
    t = cb.parse_time(t)
    return t.strftime("%m-%d %H:%MZ") if t else "-"


class GhFail(Exception):
    pass


def gh(*args, timeout=None):
    if shutil.which("gh") is None:
        raise GhFail("gh is not on PATH")
    t0 = time.time()
    STATS["gh"] += 1
    try:
        p = subprocess.run(["gh"] + [str(a) for a in args], capture_output=True, text=True,
                           timeout=timeout or CONF["gh_timeout"])
    except subprocess.TimeoutExpired:
        raise GhFail("gh %s: timed out" % " ".join(map(str, args[:4])))
    finally:
        STATS["gh_seconds"] += time.time() - t0
    if p.returncode != 0:
        raise GhFail("gh %s: %s" % (" ".join(map(str, args[:4])), (p.stderr or p.stdout).strip()[:200]))
    out = p.stdout.strip()
    return json.loads(out) if out else None


def hub(fn, *a, **k):
    STATS["hub"] += 1
    return fn(*a, **k)


def ci_state(rollup):
    """green, red, pending or none from a statusCheckRollup, with the failing names."""
    if not rollup:
        return "none", []
    red, pending = [], False
    for c in rollup:
        concl = (c.get("conclusion") or c.get("state") or "").upper()
        status = (c.get("status") or "").upper()
        name = c.get("name") or c.get("context") or "?"
        if concl in ("FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE"):
            red.append(name)
        elif status in ("IN_PROGRESS", "QUEUED", "PENDING", "WAITING") or concl in ("PENDING", "EXPECTED", ""):
            pending = True
    if red:
        return "red", red
    return ("pending" if pending else "green"), []


# ---- findings ----

class Pass:
    def __init__(self, conf):
        self.conf = conf
        self.findings = []   # things to judge now
        self.watch = []      # young, or covered: recheck next pass
        self.quiet = []      # checked and fine
        self.errors = []     # a check that could not run
        self.counts = {}
        self.notes = ""
        if conf.get("notes"):
            try:
                with open(os.path.expanduser(conf["notes"])) as f:
                    self.notes = f.read()
            except OSError as e:
                self.errors.append("notes: %s" % e)

    def covered(self, keys):
        """Where a finding is already handled: open to-dos, the user's recent answers or drops, the notes."""
        out = []
        keys = [k for k in keys if k]
        for t in self.todos_all:
            if t.state not in ("open", "answered") and (hours(t.closed_at or t.updated_at) or 1e9) > 7 * 24:
                continue
            hay = " ".join([t.question, t.context] + [l.url for l in t.links])
            if any(re.search(r"(?<![\w/-])%s(?![\w-])" % re.escape(k), hay) for k in keys):
                out.append("%s %s %s" % (t.id, t.state, utc(t.created_at)))
        out = out[::-1]   # newest first
        if len(out) > 3:
            out = out[:3] + ["+%d older" % (len(out) - 3)]
        if self.notes and any(re.search(r"(?<![\w/-])%s(?![\w-])" % re.escape(k), self.notes) for k in keys):
            out.append("in notes")
        return out

    def add(self, check, who, title, evidence, keys=(), at=None, url=""):
        """who: "user" (a to-do candidate), "coord" (a note to the coordinator) or "info"."""
        f = {"check": check, "who": who, "title": title, "evidence": evidence, "url": url,
             "age": dur(hours(at)) if at else "",
             "covered": self.covered(list(keys) + ([url] if url else [])) if who != "info" else []}
        young = at is not None and (hours(at) or 0) < self.conf["min_age_hours"]
        if young:
            f["why_watch"] = "under %sh old" % self.conf["min_age_hours"]
            self.watch.append(f)
        elif f["covered"] and who != "info":
            f["why_watch"] = "covered"
            self.watch.append(f)
        else:
            self.findings.append(f)

    # ---- the hub ----

    def load_hub(self):
        self.me = hub(cb.whoami)
        self.agents = {a.id: a for a in hub(cb.agents)}
        self.hosts = hub(cb.hosts)
        self.entries = hub(cb.entries)
        self.todos_all = hub(cb.todos, "all")
        self.queue = hub(cb.queue)
        self.has_report_at = any(e.report_at for e in self.entries)

    def active_on(self, branch=None, needle=None):
        """Live workers on a branch, or whose brief names needle (an issue)."""
        out = []
        for e in self.entries:
            if e.status not in ("running", "finished", "landing", "waiting"):
                continue
            if branch and (e.branch == branch or branch in e.previous_branches):
                out.append("%s (%s)" % (e.agent_name or e.id, e.status))
            elif needle and needle.search(e.brief or ""):
                out.append("%s (%s)" % (e.agent_name or e.id, e.status))
        return out

    # ---- 1. PRs ----

    def prs(self, w):
        repo = w["repo"]
        args = ["pr", "list", "-R", repo, "--state", "open", "--limit", "100", "--json",
                "number,title,url,author,isDraft,createdAt,headRefName,reviewDecision,statusCheckRollup"]
        if w.get("author"):
            args += ["--author", w["author"]]
        prs = gh(*args) or []
        ignore = w.get("ignore_authors", [])  # glob patterns: "app/renovate*"
        shown = 0
        for p in prs:
            login = (p.get("author") or {}).get("login", "")
            ci, red = ci_state(p.get("statusCheckRollup"))
            h = hours(p["createdAt"])
            if any(fnmatch.fnmatchcase(login, pat) or fnmatch.fnmatchcase("app/" + login, pat) for pat in ignore):
                if ci == "red":
                    self.add("prs", "user", "%s#%d by %s: CI red (author normally ignored)" % (repo, p["number"], login),
                             "failing: %s" % ", ".join(red[:4]), keys=["%s#%d" % (repo, p["number"])],
                             at=p["createdAt"], url=p["url"])
                continue
            fa = w.get("flag_after_hours")
            if fa is not None and ci != "red" and (h or 0) < fa:
                continue   # waits on the user by design: flag only when old or red
            shown += 1
            workers = self.active_on(branch=p.get("headRefName"))
            ev = "opened %s (%s ago); CI %s%s; %s; review %s; worker: %s" % (
                utc(p["createdAt"]), dur(h), ci, (" (" + ", ".join(red[:4]) + ")") if red else "",
                "draft" if p.get("isDraft") else "ready", p.get("reviewDecision") or "none yet",
                ", ".join(workers) or "none")
            self.add("prs", "user" if not workers or ci == "red" else "coord",
                     "%s#%d %s" % (repo, p["number"], p["title"][:70]), ev,
                     keys=["%s#%d" % (repo, p["number"]), "pull/%d" % p["number"]], at=p["createdAt"], url=p["url"])
        skipped = len(prs) - shown
        self.quiet.append("prs %s: %d open%s" % (repo, len(prs), ", %d not due (ignored author, or young and green)" % skipped if skipped else ""))

    # ---- 2. plans ----

    SLICE = re.compile(r"^\s*[-*]\s+\[([ xX])\]\s+(.*)$")
    NOT_DUE = re.compile(r"\blater\b|not approved", re.I)

    def plans(self, w):
        repo = w["repo"]
        issues = gh("issue", "list", "-R", repo, "--state", "open", "--limit", "100",
                    "--json", "number,title,url,body,updatedAt") or []
        n = 0
        for i in issues:
            body = i.get("body") or ""
            if not re.search(r"(?im)^#+.*slices", body):
                continue
            n += 1
            slices = [(m.group(1) != " ", m.group(2).strip()) for m in map(self.SLICE.match, body.splitlines()) if m]
            if not slices:
                continue
            due = [s for done, s in slices if not done and not self.NOT_DUE.search(s)]
            later = [s for done, s in slices if not done and self.NOT_DUE.search(s)]
            ticked = sum(1 for done, _ in slices if done)
            ref = "%s#%d" % (repo, i["number"])
            if not due and not later:
                self.add("plans", "coord", "%s fully ticked (%d/%d) but still open" % (ref, ticked, len(slices)),
                         "updated %s" % utc(i["updatedAt"]), keys=[ref, i["url"]], url=i["url"])
                continue
            if not due:
                self.quiet.append("plans %s: %d/%d ticked, the rest marked later/not approved" % (ref, ticked, len(slices)))
                continue
            workers = self.active_on(needle=re.compile(r"(issues/|#)%d\b" % i["number"]))
            approval = self.approval(repo, i["number"], body)
            title = "%s %s: %d/%d slices ticked, %d unticked and not marked later" % (ref, i["title"][:50], ticked, len(slices), len(due))
            ev = "next unticked: %s; workers naming it: %s; %s" % (due[0][:90], ", ".join(workers) or "none", approval)
            if any(x.endswith("(running)") for x in workers):
                self.watch.append({"check": "plans", "title": title, "why_watch": "a worker is running on it",
                                   "evidence": ev, "url": i["url"]})
                continue
            self.add("plans", "coord", title, ev, keys=[ref, i["url"]], url=i["url"])
        self.quiet.append("plans %s: %d open issues, %d with Slices" % (repo, len(issues), n))

    def approval(self, repo, number, body=""):
        said = [l.strip() for l in body.splitlines() if re.search(r"approv", l, re.I) and not self.SLICE.match(l)]
        if said:
            m = re.search(r"[^.]*approv[^.]*\.?", said[-1], re.I)
            return "body says: " + (m.group(0).strip() if m else said[-1])[:110]
        try:
            d = gh("issue", "view", number, "-R", repo, "--json", "comments") or {}
        except GhFail as e:
            return "approval: not read (%s)" % e
        hits = [c for c in d.get("comments", []) if re.search(r"approv", c.get("body", ""), re.I)]
        if not hits:
            return "neither the body nor a comment mentions approval"
        c = hits[-1]
        return "last approval comment %s by %s: %s" % (utc(c.get("createdAt")), (c.get("author") or {}).get("login", "?"),
                                                      " ".join(c.get("body", "").split())[:100])

    # ---- 3. stuck ----

    def stuck(self):
        c = self.conf
        waiting_on = {}
        for t in self.todos_all:
            if t.waiting and t.waiting.id:
                waiting_on.setdefault(t.waiting.id, []).append(t)
        for e in self.entries:
            a = self.agents.get(e.agent_id)
            if e.status == "running" and e.loop:
                every = e.loop_every_minutes
                if every and e.last_pass_at:
                    late = hours(e.last_pass_at) * 60 - every
                    if late > max(10, every / 4):
                        self.add("stuck", "coord", "loop %s: pass overdue by %dm" % (e.agent_name, late),
                                 "every %dm, last pass ended %s; agent %s" % (every, utc(e.last_pass_at), a.agent_status if a else "not seen"),
                                 keys=[e.id, e.agent_name])
                continue
            if e.status == "running":
                last = max([t for t in (e.progress.at if e.progress else None, a.last_active if a else None, e.created_at) if t])
                status = a.agent_status if a else "not seen"
                if not a:
                    self.add("stuck", "coord", "%s running but its agent is not seen" % e.agent_name,
                             "handoff %s on %s, created %s" % (e.id, e.host, utc(e.created_at)), keys=[e.id, e.agent_name], at=e.created_at)
                elif a.quiet_since or (hours(last) > c["quiet_hours"] and status != "working"):
                    self.add("stuck", "coord", "%s: no progress for %s (%s)" % (e.agent_name, dur(hours(last)), status),
                             "handoff %s; last progress %s%s; quiet since %s" % (
                                 e.id, utc(e.progress.at) if e.progress else "none",
                                 (" \"%s\"" % e.progress.text[:60]) if e.progress else "", utc(a.quiet_since)),
                             keys=[e.id, e.agent_name], at=e.created_at)
            if e.status == "finished":
                r = cb.latest_reply(e)
                at = r.at or e.finished_at
                if at and hours(at) > c["decisions_window_hours"]:
                    continue
                m = re.search(r"##\s*Decisions for the user\s*\n(.*?)(\n## |\Z)", r.text or "", re.S)
                if m and not re.match(r"\s*(None\.?|-|none)\s*$", m.group(1), re.I):
                    later = [t for t in waiting_on.get(e.agent_id, []) if at is None or t.created_at >= at - _MIN10]
                    if not later:
                        self.add("stuck", "coord", "%s finished with decisions for the user and no to-do waiting on it" % e.agent_name,
                                 "handoff %s; report %s%s; decision: %s" % (
                                     e.id, utc(at), "" if r.at else " (finished_at: no report_at)",
                                     " ".join(m.group(1).split())[:120]),
                                 keys=[e.id, e.agent_name], at=at)
            if e.status == "failed" and (hours(e.created_at) or 0) < c["failed_window_hours"]:
                retried = [x for x in self.entries if x is not e and x.created_at > e.created_at
                           and (x.agent_name == e.agent_name or x.branch == e.branch) and x.status != "failed"]
                if retried:
                    self.quiet.append("failed handoff %s (%s) was retried as %s" % (e.id, e.agent_name or e.branch, retried[0].agent_name or retried[0].id))
                else:
                    self.add("stuck", "coord", "failed handoff %s (%s)" % (e.id, e.agent_name or e.branch),
                             "created %s; error: %s" % (utc(e.created_at), (e.error or "?")[:120]), keys=[e.id], at=e.created_at)
        for a in self.agents.values():
            if a.agent_status == "blocked" and not any(t.state == "open" for t in waiting_on.get(a.id, [])):
                self.add("stuck", "user", "%s blocked on %s with no open to-do" % (a.name or a.id, a.blocked_on.label if a.blocked_on else "a dialog"),
                         "agent %s, blocked since %s" % (a.id, utc(a.status_since)), keys=[a.id, a.name], at=a.status_since)
        pend = [q for q in self.queue if not q.settled_at]
        for q in pend:
            if (hours(q.at) or 0) >= c["queue_hours"]:
                tgt = self.agents.get(q.agent_id)
                self.add("stuck", "coord", "queued prompt %s to %s pending %s" % (q.id, q.name, dur(hours(q.at))),
                         "from %s at %s; target %s%s%s" % (q.from_, utc(q.at), tgt.agent_status if tgt else "not seen",
                                                          ", parked" if q.parked else "", (", held: " + q.held) if q.held else ""),
                         keys=[q.id])
            else:
                self.watch.append({"check": "stuck", "title": "queued prompt %s to %s" % (q.id, q.name), "age": dur(hours(q.at)),
                                   "why_watch": "pending under %sh" % c["queue_hours"], "evidence": "from %s" % q.from_})
        n = 0
        for t in self.todos_all:
            if not (t.answer and t.answer.at and t.waiting and t.waiting.id) or t.state == "dropped":
                continue
            if t.source == "hub-blocked" or t.kind in ("approval", "release"):
                continue
            if t.kind == "review" and t.review and t.review.verdict and t.review.verdict.verdict == "good":
                continue  # approved: nothing is left for its worker to do
            h = hours(t.answer.at)
            if h is None or h > c["answered_window_hours"]:
                continue
            n += 1
            a = self.agents.get(t.waiting.id)
            acted = a is not None and (a.agent_status == "working" or (a.last_active and a.last_active > t.answer.at))
            e = next((x for x in reversed(self.entries) if x.agent_id == t.waiting.id), None)
            if not acted and e is not None:
                # Its handoff moved on after the answer: it landed, or reported, or the hub removed it.
                moved = [x for x in (e.finished_at, e.report_at, e.last_pass_report_at, e.removed_at,
                                     e.progress.at if e.progress else None) if x and x > t.answer.at]
                acted = bool(moved) or e.status in ("landed", "removed", "ready_to_remove", "archived")
            relay = (" relay %s" % t.relay.state) if t.relay else ""
            if not acted:
                self.add("stuck", "coord", "answered to-do %s: %s has not acted" % (t.id, t.waiting.name or t.waiting.id),
                         "\"%s\" answered %s (\"%s\"); agent %s, last active %s; handoff %s%s" % (
                             t.question[:70], utc(t.answer.at), " ".join(t.answer.text.split())[:50],
                             (a.agent_status if a else "gone" + (" (parked)" if t.waiting.parked else "")),
                             utc(a.last_active) if a else "-", ("%s %s" % (e.id, e.status)) if e else "none", relay),
                         keys=[t.id], at=t.answer.at)
        opened = [t for t in self.todos_all if t.state == "open"]
        self.quiet.append("to-dos: %d open%s" % (len(opened), "".join(
            "; %s %s %s \"%s\"" % (t.id, t.kind, utc(t.created_at), t.question[:50]) for t in opened[:5])))
        self.quiet.append("stuck: %d running, %d queued pending, %d answered to-dos checked for an agent that acted" % (
            sum(1 for e in self.entries if e.status == "running"), len(pend), n))

    # ---- 4. releases and hosts ----

    def releases(self):
        c = self.conf
        rel = [t for t in self.todos_all if t.kind == "release" and t.state in ("open", "answered")]
        for t in rel:
            st = t.release.state if t.release else ""
            ev = "state %s; version %s; opened %s" % (st or "?", t.release.version if t.release else "?", utc(t.created_at))
            if t.release and t.release.tagged_at:
                ev += "; tagged %s" % utc(t.release.tagged_at)
            if t.release and t.release.rollout_note:
                ev += "; rollout: " + t.release.rollout_note[:100]
            if (hours(t.created_at) or 0) > c["release_hours"]:
                self.add("release", "user", "release %s open over %dh: %s" % (t.id, c["release_hours"], t.question[:60]), ev,
                         keys=[t.id], at=t.created_at)
            else:
                self.watch.append({"check": "release", "title": "%s %s" % (t.id, t.question[:60]), "age": dur(hours(t.created_at)),
                                   "why_watch": "flag after %dh" % c["release_hours"], "evidence": ev})
        up = [h for h in self.hosts if h.status == "ok"]
        versions = sorted({h.version for h in up if h.version})
        if len(versions) > 1:
            self.add("release", "coord", "hosts on different versions: " + ", ".join(
                "%s %s" % (h.name, h.version) for h in up), "hub %s" % self.me.version, keys=["version"])
        for h in self.hosts:
            if h.status == "ok":
                continue
            probe = ""
            argv = c["host_probes"].get(h.name)
            if argv:
                try:
                    p = subprocess.run(argv, capture_output=True, text=True, timeout=30)
                    probe = "; probe: " + " ".join((p.stdout or p.stderr).split())[:80]
                except (OSError, subprocess.TimeoutExpired) as e:
                    probe = "; probe failed: %s" % e
            ev = "%s since %s: %s%s" % (h.status, utc(h.since), h.error[:80], probe)
            if (hours(h.since) or 0) >= c["host_down_hours"]:
                self.add("release", "info", "host %s %s for %s" % (h.name, h.status, dur(hours(h.since))), ev, keys=[h.name])
            else:
                self.watch.append({"check": "hosts", "title": "host %s %s" % (h.name, h.status), "age": dur(hours(h.since)),
                                   "why_watch": "down under %sh" % c["host_down_hours"], "evidence": ev})
        self.quiet.append("release: %d open release to-dos; hosts %s" % (
            len(rel), ", ".join("%s %s" % (h.name, h.version or h.status) for h in self.hosts)))

    # ---- 5. pile-up ----

    def pileup(self):
        from collections import Counter
        n = Counter(e.status for e in self.entries)
        self.counts = {"finished": n["finished"], "ready_to_remove": n["ready_to_remove"], "archived": n["archived"],
                       "running": n["running"], "pileup": n["finished"] + n["ready_to_remove"]}
        prev = re.findall(r"(\d+) finished \+ (\d+) ready_to_remove", self.notes)
        if prev:
            p = int(prev[-1][0]) + int(prev[-1][1])
            self.counts["previous"] = p
            self.counts["change"] = self.counts["pileup"] - p

    # ---- standing watches ----

    def comments(self, w):
        repo, num = w["repo"], w["number"]
        d = gh("pr", "view", num, "-R", repo, "--json", "author,comments,reviews,headRefOid,url,state") or {}
        me = {self.gh_user(), (d.get("author") or {}).get("login")}
        items = []
        for c in d.get("comments", []) + d.get("reviews", []):
            login = (c.get("author") or {}).get("login", "")
            at = c.get("createdAt") or c.get("submittedAt")
            if login.endswith("[bot]") or login in ("github-actions",) or not at:
                continue
            items.append((cb.parse_time(at), login, " ".join((c.get("body") or c.get("state") or "").split())))
        items.sort()
        theirs = [i for i in items if i[1] not in me]
        mine = [i for i in items if i[1] in me]
        ref = "%s#%s" % (repo, num)
        if not theirs:
            self.quiet.append("watch %s: no maintainer comments" % ref)
            return
        last = theirs[-1]
        ev = "last maintainer comment %s by %s: \"%s\"; my last reply %s; head %s; state %s" % (
            utc(last[0]), last[1], last[2][:80], utc(mine[-1][0]) if mine else "none", (d.get("headRefOid") or "")[:8], d.get("state"))
        if mine and mine[-1][0] > last[0]:
            self.quiet.append("watch %s: answered (%s)" % (ref, ev))
            return
        stale = w.get("stale_hours", 24)
        h = hours(last[0])
        if h >= stale:
            self.add("watch", "coord", "%s: maintainer comment unanswered %s" % (ref, dur(h)), ev, keys=[ref], url=d.get("url", ""))
        else:
            self.watch.append({"check": "watch", "title": "%s comment by %s" % (ref, last[1]), "age": dur(h),
                               "why_watch": "unanswered, flag after %sh" % stale, "evidence": ev, "url": d.get("url", "")})

    def runs(self, w):
        repo = w["repo"]
        args = ["run", "list", "-R", repo, "--branch", w["branch"], "--workflow", w["workflow"], "-L", "1",
                "--json", "databaseId,headSha,conclusion,status,createdAt,url"]
        if w.get("event"):
            args += ["--event", w["event"]]
        rs = gh(*args) or []
        if not rs:
            self.errors.append("run watch %s %s: no runs" % (repo, w["workflow"]))
            return
        r = rs[0]
        jobs = (gh("run", "view", r["databaseId"], "-R", repo, "--json", "jobs") or {}).get("jobs", [])
        want = w.get("jobs") or [j["name"] for j in jobs]
        got = {j["name"]: j for j in jobs}
        states = ["%s %s" % (n, (got[n].get("conclusion") or got[n].get("status")) if n in got else "missing") for n in want]
        green = all(n in got and got[n].get("conclusion") == "success" for n in want)
        red_all = sorted(j["name"] for j in jobs if j.get("conclusion") == "failure")
        ev = "run %s on %s at %s (%s ago): %s; failing jobs: %s" % (
            r["databaseId"], r["headSha"][:8], utc(r["createdAt"]), dur(hours(r["createdAt"])), "; ".join(states), ", ".join(red_all) or "none")
        title = "%s %s %s/%s" % (repo, w["branch"], w["workflow"], "+".join(want))
        if green:
            self.add("watch", "coord", title + " is GREEN", ev, keys=[str(r["databaseId"])], url=r["url"])
        else:
            self.quiet.append("watch %s: not green (%s)" % (title, ev))

    _gh_user = None

    def gh_user(self):
        if self._gh_user is None:
            try:
                self._gh_user = (gh("api", "user") or {}).get("login", "")
            except GhFail as e:
                self.errors.append("gh user: %s" % e)
                self._gh_user = ""
        return self._gh_user

    # ---- run ----

    def run(self):
        t0 = time.time()
        self.load_hub()
        jobs = [("prs " + w["repo"], self.prs, w) for w in self.conf["prs"]]
        jobs += [("plans " + w["repo"], self.plans, w) for w in self.conf["plans"]]
        jobs += [("watch %s#%s" % (w["repo"], w["number"]), self.comments, w) for w in self.conf["comment_watches"]]
        jobs += [("run watch %s %s" % (w["repo"], w["workflow"]), self.runs, w) for w in self.conf["run_watches"]]
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as ex:
            futs = {ex.submit(fn, w): name for name, fn, w in jobs}
            for name, fn in (("stuck", self.stuck), ("release", self.releases), ("pileup", self.pileup)):
                try:
                    fn()
                except Exception as e:  # one broken check must not hide the others
                    self.errors.append("%s: %s: %s" % (name, type(e).__name__, e))
            for f in concurrent.futures.as_completed(futs):
                try:
                    f.result()
                except Exception as e:
                    self.errors.append("%s: %s" % (futs[f], e))
        self.seconds = time.time() - t0
        order = {"user": 0, "coord": 1, "info": 2}
        self.findings.sort(key=lambda f: (order.get(f["who"], 3), f["check"]))
        return self

    def as_dict(self):
        return {"at": cb.now().isoformat(), "hub_version": self.me.version, "report_at_known": self.has_report_at,
                "findings": self.findings, "watch": self.watch, "quiet": self.quiet, "counts": self.counts,
                "errors": self.errors, "calls": dict(STATS), "seconds": round(self.seconds, 1)}

    def text(self):
        L = []
        L.append("steward pass %s (hub %s; %d hub + %d gh calls, %.1fs)" % (
            cb.now().strftime("%Y-%m-%d %H:%MZ"), self.me.version, STATS["hub"], STATS["gh"], self.seconds))
        if not self.has_report_at:
            L.append("note: no entry has report_at yet (a hub before v0.5.53, or no report since it upgraded): "
                     "report ages fall back to finished_at")
        L.append("")
        L.append("FINDINGS (%d) - judge each; who = user (a to-do?) / coord (a note?) / info" % len(self.findings))
        for i, f in enumerate(self.findings, 1):
            L.append("%d. [%s] %s: %s%s" % (i, f["who"], f["check"], f["title"], (" (%s)" % f["age"]) if f["age"] else ""))
            L.append("   " + f["evidence"])
            if f["url"]:
                L.append("   " + f["url"])
            if f["covered"]:
                L.append("   seen: " + "; ".join(f["covered"]))
        if not self.findings:
            L.append("none")
        L.append("")
        L.append("WATCH (%d) - recheck next pass" % len(self.watch))
        for f in self.watch:
            L.append("- %s: %s%s [%s]" % (f["check"], f["title"], (" (%s)" % f["age"]) if f.get("age") else "", f["why_watch"]))
            if f.get("evidence"):
                L.append("  " + f["evidence"])
            if f.get("covered"):
                L.append("  seen: " + "; ".join(f["covered"]))
            if f.get("url"):
                L.append("  " + f["url"])
        L.append("")
        L.append("QUIET")
        L.extend("- " + q for q in self.quiet)
        c = self.counts
        if c:
            L.append("")
            s = "PILE-UP %d finished + %d ready_to_remove = %d (%d archived, %d running)" % (
                c["finished"], c["ready_to_remove"], c["pileup"], c["archived"], c["running"])
            if "previous" in c:
                s += "; last noted %d, change %+d" % (c["previous"], c["change"])
            L.append(s)
        if self.errors:
            L.append("")
            L.append("ERRORS - these checks did not run; say so in the pass")
            L.extend("- " + e for e in self.errors)
        return "\n".join(L)


import datetime as _dt  # noqa: E402
_MIN10 = _dt.timedelta(minutes=10)

CONF = dict(DEFAULTS)


def load_config(path):
    conf = dict(DEFAULTS)
    if path:
        with open(os.path.expanduser(path)) as f:
            user = json.load(f)
        unknown = set(user) - set(DEFAULTS) - {"_comment"}
        if unknown:
            raise SystemExit("steward pass: unknown config keys: %s" % ", ".join(sorted(unknown)))
        conf.update(user)
        if conf["notes"] and not os.path.isabs(os.path.expanduser(conf["notes"])):
            conf["notes"] = os.path.join(os.path.dirname(os.path.abspath(path)), conf["notes"])
    return conf


def main(argv=None):
    ap = argparse.ArgumentParser(prog="pass.py", description="The steward's read-only pass (see the module doc).")
    ap.add_argument("--config", help="the steward's config (JSON; see tools/steward/config.example.json)")
    ap.add_argument("--json", action="store_true", help="print the result as JSON")
    a = ap.parse_args(argv)
    CONF.clear()
    CONF.update(load_config(a.config))
    p = Pass(CONF).run()
    print(json.dumps(p.as_dict(), indent=1, default=str) if a.json else p.text())
    return 0


if __name__ == "__main__":
    sys.exit(main())
