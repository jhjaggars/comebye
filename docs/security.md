# Security model

**This app is a remote shell in a browser.** Sending a prompt to a coding
agent is, in practice, arbitrary command execution on the host as your user.
Treat access to it like SSH access to that machine.

- **Loopback only.** The server listens on
  `127.0.0.1`. The only intended
  path from your phone is Tailscale Serve, which terminates HTTPS and is
  reachable only from inside your tailnet.
- <a id="tagged-nodes-through-serve"></a><a id="tagged-nodes-and-the-peer-listener"></a>**Tagged nodes through Serve
  (opt-in).** With `-ts-allow-cap`, a tagged node holding that app capability
  from a tailnet grant is admitted on the main listener, through Serve run with
  `--accept-app-caps`. The role: the
  forwarded agent only, [limited as below](#what-a-tagged-worker-reaches), a
  registered peer (or one announcing itself), `/api/*` only, and no login,
  token or host name it claims counts.
  - **Identity:** Serve's `Tailscale-App-Capabilities` header must hold the
    capability, and tailscaled's WhoIs on the address in `X-Forwarded-For` must
    show a tagged node that holds it too. Serve replaces both headers, so a
    tailnet node cannot forge them. WhoIs answers are cached for 30 s. A grant
    removed from the policy stops the header as soon as the hub's tailscaled
    sees it.
  - **The trade-off:** a **local process on the hub**
    can forge both headers on the loopback port, the same way it can forge
    `Tailscale-User-Login`. This adds no new power: forging the login already
    gets it the user role, while this gets it only the agent role, and only as
    a node that really holds the grant.
  - **Admission needs two things:** the capability (in the header and in
    WhoIs's answer), and a registered peer whose address is that very node
    (and that carries the peer's tag, if one was recorded). A tagged VM that
    nobody registered gets 403.
  - **Self-registration** (`-peer-self-register`, off by default) lets such a
    node add *only itself*: the announced URL must be its own node, `https`
    under `*.ts.net`; names are unique; repeating it is a no-op; it is logged
    and recorded as `added_by tag:worker@<node>`. It can never remove or alter
    a peer; removal is the user's alone. Note what registration grants: the
    hub sends a peer its prompts and trusts its agent list, so tag (and
    self-register) only machines you control.
  - **Removed:** releases up to v0.5.x could also open a plain-HTTP "peer
    listener" on its tailnet IP (`-peer-listen`, port 8742, with
    `-ts-allow-tag`). Tags alone no longer admit anything; the capability is
    the only way in. Delete the tailnet grant to that port and the hub's
    firewall opening for it.
  - **Reach:** the grant's `ip` block lets those nodes reach the hub's Serve
    port, so they can load the static UI shell if the capability header is
    missing (for example Serve lacks the flag). They still get no API.
  - Requests with a login, or without the capability, are handled as if this
    feature were off.
- <a id="what-a-tagged-worker-reaches"></a>**What a tagged worker reaches.**
  A worker VM may run code you would not trust with the rest of your fleet, and
  its identity comes from WhoIs, so these limits hold even against a malicious
  worker. Agent routes refuse a tagged node unless they are opened to it
  (403 `worker_forbidden`), so a new route starts closed. What is open:
  - **Its own bookkeeping:** `POST /api/report`, adding a to-do, `whoami`,
    `version`, the playbook, the agent and host lists (the slim view, never
    `?full=1`) and the roles.
  - **Prompts,** direct or queued, only to a recorded coordinator on any host
    and to agents on its own host (403 `prompt_forbidden`). This is how a worker
    reaches its coordinator. Coordinators should treat these prompts as data.
  - **Reads limited to its own host.** `GET /api/ledger` gives other hosts'
    entries in outline (no brief, report, mark or checks), plus the report of
    any entry one of its host's entries waits on, and no plans. `GET /api/queue`
    lists prompts to or from its host, and it can withdraw only those it queued.
    `GET /api/todos` and `/api/todos/{id}` show items created on its host or
    waiting on an agent there (others: 404). `GET /api/inspect` answers only for
    its host's agents (403 `read_forbidden`).
  - **Everything else is refused:** other agents' output and transcripts on any
    host, its own panes included; the briefing; plans; handoffs, on any host
    (they could start an agent elsewhere, and `--after` pastes other workers'
    reports into the new prompt); landings; marks and sweeps; model switches;
    closing to-dos. Keys, answers, images and removals remain the user's alone.

  What this leaves open: a worker can still put text in front of a coordinator
  by prompt, in a to-do or in its report. Its brief and any report it waits on
  are visible to it by design.
- <a id="searching-transcripts"></a>**Searching transcripts.** `/api/recall/*` reads
  the index: every session the host indexed, tool output included. Those routes are
  the user's alone, with no agent level at all. The agent levels would admit a tagged
  worker or a forwarding peer, and the plaintext index must never reach a worker;
  keeping other agents' output out of an agent's reach also closes a prompt-injection
  route. Agents on a host with the index keep `comebye recall`, which reads the file
  and needs nothing from the server. The usual limit applies: a local process can
  read the file anyway. The server opens the index with `sqlite3 -readonly -safe`, in
  a process per query with a 3 s limit and at most two at once, and checks path values
  against an allowlist. It does not log queries. Answers are `no-store`, and the app
  keeps no query or transcript text in browser storage (it remembers filters; a query
  does appear in the browser's history, through the URL). Messages go through the same
  sanitizer as chat; tool arguments and output are only ever text nodes. Secret-looking
  text is [masked when indexed](archive.md#what-the-index-keeps), which limits what a
  shoulder-surfer or a snapshot sees and is not access control.
- <a id="reading-other-agents"></a>**Reading other agents' output.** Output and
  transcripts (`/output`, `/transcript`, the subagent transcript, bare or under
  `/api/hosts/{host}/`, and so `comebye read`) hold tool output and possibly
  secrets. The user reads any. An agent reads those **on its own host**, and an
  agent recorded as a **coordinator** on the hub reads any host's. A tagged node
  reads none. Any other request gets 403 `read_forbidden`. The rule is applied
  on the hub, since a peer with `-hub` forwards its agents' reads there; the hub
  then calls the peer as itself, with none of the caller's claims. A coordinator
  must be recorded: the Coordinator preset or the app's "Make coordinator" does
  it, and one you started by hand reads only its own host until then.
  **Limitation, inherited and not widened (issues #3 and #4):** for an untagged
  agent this is a guard rail, not a boundary. The pane and host it is judged by
  are self-asserted, so a process with the agent token can claim a coordinator's
  pane, and any local process can forge the identity header on the loopback
  port or read the session files directly. A worker on the same host as your
  other agents is not contained by it. For a real boundary, run the work on a
  tagged worker VM, where none of those claims count.
- **Identity allowlist.** Tailscale Serve adds a `Tailscale-User-Login`
  header to every request it proxies from a tailnet device and strips any
  client-supplied copy. With `-ts-allow-login`, only the listed logins get
  through; other tailnet users, tagged devices (which carry no user
  identity; see above for how they can be admitted, never as the user), and
  Funnel traffic are rejected with 401, and the app shows a
  "Not authorized" message. Matching is case-insensitive. The server
  refuses to start with an allowlist unless it listens on loopback, since
  any other client could forge the header. There are no passwords, tokens
  or login endpoints; static assets (the UI shell) are served without auth
  and every `/api/*` call requires an allowed login.
- **Cross-site requests are rejected.** The identity header is ambient,
  like a cookie, so the server wraps everything in Go's
  `http.CrossOriginProtection` (checks `Sec-Fetch-Site`/`Origin`) and
  requires `Content-Type: application/json` on POSTs. A web page the phone
  visits cannot drive your agents through Serve.
- **`-ts-allow-login` is required.** Without it the API would be
  unauthenticated and every device on your tailnet could drive your agents,
  so the server refuses to start unless you also pass `-insecure-no-auth`
  (local testing only; it logs a warning).
- **DNS rebinding is blocked.** Every request (UI and API) must be
  addressed to `localhost`, a loopback IP, a `*.ts.net` name (what
  Tailscale Serve forwards) or a name listed with `-allow-host`; anything
  else gets 421. Otherwise a web page in a browser on the host could
  re-resolve its own domain to `127.0.0.1` and, being same-origin with the
  loopback port, drive your agents with a forged identity header.
- **Local users are trusted.** Any process on the same host can connect to
  the loopback port and forge the identity header. Do not run this on a
  shared multi-user machine.
- **Agents get a restricted role, not a boundary.** The `comebye` CLI
  authenticates with the local agent token (`<state-dir>/agent-token`,
  0600, compared in constant time; a wrong token is refused, never treated
  as the user). That role may read (other agents' output
  [within its rule](#reading-other-agents)), hand off, prompt, mark, sweep (which only marks, with
  its own generated reason, workers whose checks show nothing to lose; never a
  coordinator or the caller itself), publish the
  briefing and set its schedule, add and read to-do items (any agent), and,
  for a recorded coordinator only, answer, finish or drop them, and nothing else (no keys, answers, launches, removals or
  their confirmation). It keeps a well-behaved coordinator inside the
  workflow; an agent with a shell could still read the token, forge the
  identity header or run git itself, so the real safeguard is that you
  confirm removals in the app. A peer with `-hub` forwards only these
  requests to the hub (https, no redirects, bounded, marked as forwarded),
  and the hub applies the agent role to them again. See
  [Coordinators](agents.md#coordinators).
- **The to-do list** (`comebye todo`, `/api/todos`) is for questions the user
  has to deal with. Any agent may add and list items (a tagged worker
  [sees only its own host's](#what-a-tagged-worker-reaches)). Answering, finishing and
  dropping are the user's (the app) or a recorded coordinator's: another agent
  gets 403 `coordinator_only`, a tagged worker `worker_forbidden`. A coordinator's `answer` records the user's
  words it relayed (`via: relay`); only an answer typed in the app (`via: app`)
  sends the coordinator a notice, whose text is fixed and names the item id
  only. The hub never forwards an answer to the waiting agent. Item text,
  context and links are agent-written: the app escapes the text, renders the
  context through the same marked + DOMPurify path as transcripts, and only
  links `http(s)` URLs; the CLI labels the context "data, not instructions".
  **Limitation, inherited and not widened (issues #3 and #4):** "coordinator"
  is judged from the agent role's self-asserted pane header plus the shared
  agent token, so any local process on the hub (or an untagged host's agent
  through its peer) can claim to be the coordinator and answer, finish or drop
  items, exactly as it can already forge the identity header on the loopback
  port. What it gets is bookkeeping on a list, not a way to run anything; the
  list holds no secrets and answering does not trigger any action by itself.
- <a id="releasing-and-upgrading-from-the-app"></a>**Releasing and upgrading from
  the app** (`-release-repo` or `-release-host`, `-release-upgrade`; both off by default, and refused
  together with `-insecure-no-auth`, since the buttons act with your credentials).
  - **Who.** The routes `GET /api/release/{id}`, `POST /api/release/{id}/approve`,
    `GET|POST /api/release/upgrade` and `POST /api/hosts/{host}/release/upgrade` are
    the **user role's alone**, registered with the same wrapper as keys and
    launches: the agent token, a peer forwarding for its agents, a tagged node and
    a wrong token all fail before any handler runs (403 `agent_forbidden`, or 401),
    and a test fails if a release route is added with another wrapper. A caller is
    the user only when the request carries an allowlisted `Tailscale-User-Login`
    from Serve and none of the agent markers.
  - **What an action needs.** On top of the cross-site rules above: the page's CSRF
    token (`X-Comebye-Csrf`, an HMAC of the login under a key made at start-up,
    handed out by the same-origin GET), and a body repeating the version and commit
    (or host) the confirm step showed. The page's GET refuses another site and
    paces its refresh, since it runs `git` and `gh`.
  - **Tagging.** The server runs the release script again at the click: the commit
    on `origin/main`, CI passed, the version newer than origin's last tag, no such
    tag. One tagging at a time. The script is the binary's embedded copy, run with
    `gh` pinned to `jhjaggars/comebye`, git hooks and fsmonitor off, no system git
    config, tags and the latest tag read from origin (a tag planted locally counts
    for nothing), and origin (fetch and push URL, after any `insteadOf`) required to
    be that repository. It uses your global git config on purpose (that is where
    your credentials and signing key are). A tagging a restart interrupted becomes
    `failed` at start-up, and a failed push removes its local tag, so it can be tried again.
  - **Upgrading.** A hub upgrades a host only for a tagged item whose release build
    it has just seen pass (this needs `-release-repo` on the hub), with an exact
    version (never `latest`, `--from-source` or `--force`). A peer is asked by the
    hub, on your click, over `POST /api/release/upgrade`; that route is **only for
    a hub calling its peer**: it needs the hub-call marker the hub's own calls carry
    (no browser path sets it) and a host that keeps no ledger, so on a hub it is
    refused outright. The marker is not authentication, only a guard: whoever can
    forge the login can add it.
  - **What an agent can do.** Raise the item, and only a recorded coordinator may:
    it fixes a version and a full commit id and nothing else is acted on. The title
    is the server's (`Release vX (abc1234)?`), the facts shown are the server's own
    check, and the item's context stays untrusted text. Answering, finishing or
    dropping the item as a coordinator approves nothing. An agent can still squat a
    version with an open item until you drop it.
  - **Audit.** Every action goes to the ledger's audit log and the journal. The log
    is **not tamper-proof against agents**: they run as the same OS user and can
    edit the ledger file, and the result of an upgrade is read from a record file in
    the state directory that they can write too (a forged "running" record also
    blocks an upgrade for 15 minutes).
  - **Limitations, inherited and not widened (issues #3 and #4).** Anything that can
    present your login can do these: a local process forging the header on the
    loopback port, and also a process on **any untagged peer host**, which can call
    the hub's Serve URL (Serve stamps your login on it) without the forwarded
    marker. The CSRF token does not stop it, since the same GET hands it out.
    What bounds it: the tag must be a CI-green commit of `main` and newer than the
    last release, and an upgrade needs a tagged release with a passed build. And an
    agent with a shell can run `git push` of a tag or `comebye upgrade` itself, as it
    always could, so the harness rule against agents pushing tags still matters.
  - **Tagging through a peer** (`-release-host NAME` on the hub; the peer sets
    `-release-repo`). Then **the peer, not the hub, holds the git and `gh`
    credentials** and the clone; the hub holds none for releases and runs no
    script. The trust path is *you → the hub's page → the hub → that peer*:
    - The hub checks everything the user does (user role, CSRF token, confirm
      body, the item's state, one tagging at a time) before it asks, and only a
      user click on an item a coordinator raised ever causes a call. It asks over
      the existing fleet connection with three hub-only ops, `release-check`,
      `release-tag` and `release-build` (`POST /api/release/check`, `POST
      /api/release/tag`, `GET /api/release/build`).
    - The peer answers them only for **the hub's own call**: the user role (the
      hub device owner's login from Serve, so a wrong or foreign login is 401), the
      hub-call marker `X-Comebye-Hub-Call` (set by the hub's `CallHost` and by no
      browser forward; `ProxyHost` refuses these ops), and a host with no ledger.
      A browser, an agent token, a peer forwarding for its agents and a tagged
      node are refused (403 `agent_forbidden` or `hub_only`, 401), and a host that
      keeps the ledger (a hub) refuses them outright. Tests cover each.
    - The peer **does not trust the hub's judgement**: it validates the version and
      the 40-character commit again, then runs the same embedded script with all
      its checks (commit on `origin/main`, CI green, newer than origin's last tag,
      tag absent, origin pinned) and the same pins on `gh`, hooks and git config.
      Its audit lines (`release.tagged`, `release.failed`, `release.dry_run`) go to
      its journal; the hub's log records the same event with the peer as its
      host. A peer's dry-run flag is its own. The peer refuses a check while it
      is tagging (they share the clone), and the hub's page does not ask one then.
    - A tag that went through while its answer was lost is reconciled by the hub:
      an item the user approved, failed, whose tag is on origin at exactly the
      item's commit (read by the peer's `--check`) becomes tagged, with a
      `release.reconciled` audit line. It adds no way to tag, only a record.
    - The hub does not trust the peer's answer more than it must: a check about
      another commit is refused, a dry run that reports a push is a failure, a link
      that is not an `https://github.com/` URL is dropped, and a run with no stated push is a failure.
      A compromised peer can of course lie about CI, which the hub shows, and it
      holds the credentials anyway: **the peer's account is the release boundary**.
    - Limitation, same as upgrades: the hub-call marker is a guard, not
      authentication. Whoever can present the hub device owner's login to the
      peer, and add the header, can call these ops directly (a local process on the
      peer forging the login header on its loopback port; a host that can reach the
      peer's Serve URL with that login). That is bounded by the script's checks
      above and is no wider than what the peer's account could already do with the
      same credentials.
  - **Recommended: the release peer is its own OS user** (decided for laharl, where
    agents run). The marker does not authenticate the hub, so on a host where
    agents run as the user that owns the credentials, a forged call to the peer's
    loopback port, or just reading `~/.config/gh`, `~/.ssh` and the clone, gets a
    tag with no click. Run a second comebye there as a dedicated account with its
    own home (mode 700), `-hub`, `-release-repo` on its own clone, its own
    `-host-name`, port and Serve name, and only a repo-scoped `gh` token and push
    credential that only it can read; the hub's `-release-host` names that peer.
    The agents' comebye on the host keeps no `-release-repo`. What remains is the
    loopback port: the script's checks (a CI-green commit of `main`, newer than the
    last tag, origin pinned) still bound a forged call, and the credentials are out
    of the agents' reach. Setup steps are in `docs/releasing.md`.
  - **The checkout.** `git` and `gh` run in `-release-repo` (on the hub, or on the
    `-release-host` peer) with that host's user's credentials,
    and anyone who can write that directory can write its `.git/config`. Give it a
    **dedicated clone** that no agent works in (not a worktree host or an agent's
    checkout); the pins above close the easy routes, not an agent with write access
    to the same account's files. `-release-simulate` (demos and browser tests)
    runs nothing, is loopback-only and cannot go with the real flags; the page says
    SIMULATED when it is on.
- **Never use Tailscale Funnel**, and do not put another reverse proxy in
  front of it.
- **Key input is whitelisted** (esc, enter, arrows, ctrl+c, and similar,
  plus the single characters 1-9, y and n for answering dialogs). No other
  printable characters; free text goes through the prompt endpoint, which
  herdr refuses while the agent is blocked (and the app holds back while a
  transcript question is pending).
- **Push endpoints are restricted.** The server POSTs to whatever endpoint
  a browser subscribed with, so endpoints must be `https` on the default
  port at a known push service (FCM, Mozilla, Apple, WNS), and the
  outbound dialer refuses private, loopback, link-local and CGNAT
  (tailnet) addresses after DNS resolution, with no redirects. Push
  payloads are encrypted end to end; logs show only the push service
  origin, never the endpoint path.
- **A hub is not an open proxy.** It only contacts the peers named in
  `-peers`, or added by the user through the peer registry
  (`comebye peer add`: user role only, tailnet hosts only), https only (no
  redirects followed, no proxy from the environment), and only for the per-agent calls on
  `/api/agents/<target>/…` (`output`, `transcript`, `prompt`, `keys`,
  `dialog`, `answer`, `image`) and the per-host launch calls on
  `/api/launch/…`, with a fixed set of query parameters, size-capped
  bodies and responses, and timeouts. An
  unknown `{host}` is a 404. None of the browser's headers are forwarded.
  The hub's own allowlist, cross-origin protection and JSON-only POST rule
  apply before anything is proxied, and every peer applies its own again.
  Anyone allowed on the hub can drive every peer.
- **Launching is confined to the launch roots.** Directory listings and
  launches resolve the path (symlinks included) and refuse anything
  outside `-launch-root` with 403, without saying whether it exists;
  relative paths are refused. Agent kinds are limited to the ones herdr
  supports and that are installed (or running) on the host, and agent
  names to herdr's `[a-z][a-z0-9_-]{0,31}`. `-launch=false` turns it off.
- Static files are served with a restrictive Content-Security-Policy
  (same-origin scripts only, no inline script, no remote images),
  `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.
- **Agent text is untrusted.** Markdown from transcripts is parsed with
  [marked](https://github.com/markedjs/marked) and the resulting HTML is
  always passed through [DOMPurify](https://github.com/cure53/DOMPurify)
  with a small allowlist: no script, style, iframe, form, image or SVG
  elements, no event-handler, style or id attributes, only `http(s):` and
  `mailto:` links (opened with `rel="noopener noreferrer"`), and only
  read-only task-list checkboxes. Raw HTML tags outside a short list of
  formatting tags are shown as text; images are shown as links. Both
  libraries are vendored in `web/vendor/` at pinned versions, copied byte
  for byte from the npm tarballs with their hashes recorded in
  `web/vendor/README.md` (checked by `go test`), and served from the app's
  own origin; nothing is loaded from a CDN.
- **Voice input** ([docs/speech-to-text.md](speech-to-text.md)) adds one word
  to the policy: `script-src 'self' 'wasm-unsafe-eval'`. It allows compiling
  WebAssembly (the speech runtime) and nothing else; `eval` and
  `new Function` stay blocked, and `connect-src`, `worker-src` and the rest
  are unchanged. The audio is transcribed in the browser and goes nowhere. The
  files it needs are served only by exact pinned name, were checked against
  pinned SHA-256s when fetched, and no `Permissions-Policy` is sent, so the
  microphone is not blocked.

See [SECURITY.md](../SECURITY.md) for the threat model and how to report a
vulnerability.
