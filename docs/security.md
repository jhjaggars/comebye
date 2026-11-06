# Security model

**This app is a remote shell in a browser.** Sending a prompt to a coding
agent is, in practice, arbitrary command execution on the host as your user.
Treat access to it like SSH access to that machine.

- **Loopback only** (with one opt-in exception, below). The server listens on
  `127.0.0.1`. The only intended
  path from your phone is Tailscale Serve, which terminates HTTPS and is
  reachable only from inside your tailnet.
- <a id="tagged-nodes-and-the-peer-listener"></a>**Tagged nodes and the peer
  listener (opt-in).** Tagged tailnet nodes (`tag:worker`) have no login, so
  Serve cannot identify them by one. With `-peer-listen` (needs `-ts-allow-tag`
  or `-ts-allow-cap`) the
  hub opens **one more listener, on its tailnet IP only** (never a wildcard
  or loopback address; the server refuses to serve it otherwise). This is
  the only exception to "loopback only", and everything else stays as
  above: the main listener, its login allowlist and Serve are unchanged.
  On the peer listener:
  - **Identity** is tailscaled's WhoIs on the connection's remote address.
    WireGuard authenticates that address; a local process cannot open a
    connection that arrives from another node's tailnet IP, and
    `Tailscale-*` headers (and the agent token, forwarding and host headers)
    are discarded, so the loopback header forgery described below does not
    apply to it. Answers are cached for 30 s, so a tag or grant removed in the
    admin console takes effect within that time. WhoIs failing (tailscaled down)
    is a refusal, never an admission.
  - **Role: the forwarded agent only**, never the user, and with the
    [limits on a tagged worker](#what-a-tagged-worker-reaches). The web app
    and every user-only route (keys, answers, launching, removals, push, the
    peer registry) do not answer. A tagged node's requests are attributed to
    its peer name, not to a host it names itself.
  - **Admission needs two things:** an allowed tag (or a tag plus the
    `-ts-allow-cap` capability in WhoIs's answer), and a registered peer
    whose address is that very node (and that carries the peer's tag, if one
    was recorded). A tagged VM that nobody registered gets 403.
  - **Self-registration** (`-peer-self-register`, off by default) lets such
    a node add *only itself*: the announced URL must be its own node,
    `https` under `*.ts.net`; names are unique; repeating it is a no-op; it
    is logged and recorded as `added_by tag:worker@<node>`. It can never
    remove or alter a peer; removal is the user's alone. Note what
    registration grants: the hub sends a peer its prompts and trusts its
    agent list, so tag (and self-register) only machines you control.
  - **Plain HTTP.** The hop is encrypted and authenticated by WireGuard, but
    Serve's HTTPS does not cover it, and it exposes a TCP port on the hub's
    tailnet address to every node your tailnet ACLs let reach it. **Add an
    ACL** so that only `tag:worker` (and, if you want, your own devices) may
    reach `<hub>:<port>`; for example a rule allowing `tag:worker` to
    `<hub-tag-or-host>:8742`, and no `*:*` grants to that port from other
    tags. Without it, other nodes get a 401/403 from the listener, which is
    still the whole defence for nodes you did not mean to allow.
  - The remaining limit is unchanged: **a local process on the hub** can
    still forge the Serve identity header on the loopback port (the main
    listener), and a local process on a *worker* is the worker's agent.
- <a id="tagged-nodes-through-serve"></a>**Tagged nodes through Serve
  (opt-in).** With `-ts-allow-cap`, a tagged node holding that app capability
  from a tailnet grant is admitted on the main listener, through Serve run with
  `--accept-app-caps`. The role and rules are the peer listener's: the
  forwarded agent only, [limited as below](#what-a-tagged-worker-reaches), a
  registered peer (or one announcing itself), `/api/*` only, and no login,
  token or host name it claims counts.
  - **Identity:** Serve's `Tailscale-App-Capabilities` header must hold the
    capability, and tailscaled's WhoIs on the address in `X-Forwarded-For` must
    show a tagged node that holds it too. Serve replaces both headers, so a
    tailnet node cannot forge them. WhoIs answers are cached for 30 s. A grant
    removed from the policy stops the header as soon as the hub's tailscaled
    sees it.
  - **The trade-off against the peer listener:** a **local process on the hub**
    can forge both headers on the loopback port, the same way it can forge
    `Tailscale-User-Login`. This adds no new power: forging the login already
    gets it the user role, while this gets it only the agent role, and only as
    a node that really holds the grant. The peer listener doesn't have this
    weakness, but the login forgery on the same host remains either way.
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

See [SECURITY.md](../SECURITY.md) for the threat model and how to report a
vulnerability.
