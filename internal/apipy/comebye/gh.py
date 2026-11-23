"""Read-only GitHub helpers over the `gh` CLI (its own login; plain dicts).

    from comebye import gh
    pr = gh.pr(97, repo="owner/name")      # state, mergeable, checks rollup...
    gh.checks(97, repo="owner/name")       # [{name, state, bucket, link, workflow}]
    gh.run_jobs(123456, repo="owner/name") # [{name, status, conclusion, url, ...}]

repo defaults to the current directory's repository. A failure raises
GhError with gh's own message.
"""

import json
import shutil
import subprocess

PR_FIELDS = ("number,title,state,isDraft,url,headRefName,headRefOid,baseRefName,"
             "mergeable,mergeStateStatus,reviewDecision,mergedAt,closedAt,updatedAt,statusCheckRollup")


class GhError(Exception):
    pass


def run(*args):
    """Run `gh ARGS...` and return its JSON answer (read-only commands only, by convention)."""
    if shutil.which("gh") is None:
        raise GhError("gh is not on PATH: the GitHub helpers need the GitHub CLI")
    p = subprocess.run(["gh"] + [str(a) for a in args], capture_output=True, text=True)
    if p.returncode != 0:
        raise GhError("gh %s: %s" % (" ".join(str(a) for a in args[:3]), (p.stderr or p.stdout).strip()))
    out = p.stdout.strip()
    return json.loads(out) if out else None


def _repo(repo):
    return ["--repo", repo] if repo else []


def pr(number, repo=None, fields=PR_FIELDS):
    """A pull request's state as a dict (`gh pr view --json`)."""
    return run("pr", "view", number, "--json", fields, *_repo(repo))


def checks(number, repo=None):
    """A pull request's checks: [{name, state, bucket, link, workflow}]. bucket is pass, fail, pending, skipping or cancel."""
    if shutil.which("gh") is None:
        raise GhError("gh is not on PATH: the GitHub helpers need the GitHub CLI")
    args = ["pr", "checks", str(number), "--json", "name,state,bucket,link,workflow"] + _repo(repo)
    p = subprocess.run(["gh"] + args, capture_output=True, text=True)
    # gh pr checks exits 8 while checks are pending and 1 when one failed; the JSON is still there.
    if p.stdout.strip().startswith("["):
        return json.loads(p.stdout)
    raise GhError("gh pr checks %s: %s" % (number, (p.stderr or p.stdout).strip()))


def runs(repo=None, branch=None, workflow=None, limit=10):
    """Recent workflow runs: [{databaseId, name, status, conclusion, headBranch, headSha, url, createdAt}]."""
    args = ["run", "list", "--limit", limit, "--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt"]
    if branch:
        args += ["--branch", branch]
    if workflow:
        args += ["--workflow", workflow]
    return run(*(args + _repo(repo)))


def run_jobs(run_id, repo=None):
    """A workflow run's jobs: [{name, status, conclusion, url, startedAt, completedAt, steps}]."""
    return (run("run", "view", run_id, "--json", "jobs", *_repo(repo)) or {}).get("jobs", [])
