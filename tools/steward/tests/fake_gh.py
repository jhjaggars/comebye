#!/usr/bin/env python3
"""A fake `gh` for the steward pass's test: canned JSON from gh_fixtures.json.

Each fixture key is the command and its repo ("pr list OWNER/REPO",
"pr list ORG/UP @me", "issue view 42 OWNER/REPO", "api user", ...). A string
value like "-5h" anywhere in a fixture becomes the time 5 hours ago (UTC), so
ages stay right whenever the test runs. Unknown commands fail as gh would.
"""

import datetime
import json
import os
import re
import sys


def ago(v):
    if isinstance(v, str) and re.fullmatch(r"-\d+(\.\d+)?h", v):
        t = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=float(v[1:-1]))
        return t.strftime("%Y-%m-%dT%H:%M:%SZ")
    if isinstance(v, list):
        return [ago(x) for x in v]
    if isinstance(v, dict):
        return {k: ago(x) for k, x in v.items()}
    return v


def main(argv):
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "gh_fixtures.json")) as f:
        fx = json.load(f)
    repo = ""
    for flag in ("-R", "--repo"):
        if flag in argv:
            repo = argv[argv.index(flag) + 1]
    words = [a for a in argv[:3] if not a.startswith("-")]
    key = " ".join(words[:2])
    if key in ("issue view", "pr view", "run view"):
        key += " " + words[2]
    if repo:
        key += " " + repo
    if "--author" in argv:
        key += " " + argv[argv.index("--author") + 1]
    with open(os.environ.get("FAKE_GH_LOG", os.devnull), "a") as log:
        log.write(key + "\n")
    if key not in fx:
        sys.stderr.write("fake gh: no fixture for %r (argv %s)\n" % (key, argv))
        return 1
    print(json.dumps(ago(fx[key])))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
