package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config says where the archive is and how it is encrypted. The CLI reads
// it from flags and COMEBYE_ARCHIVE_* variables; the server (for the flush
// on removal) from its own flags. Only Dir and Recipient are needed to
// write; Identity is needed to read back (verify, index, restore).
type Config struct {
	// Encryption is ModeAge or ModeNone. There is no default: it is chosen
	// explicitly, and an existing archive must match the mode it was
	// created in.
	Encryption string
	Host       string
	Dir        string // the filesystem backend
	// S3 is the bucket's URL, https://HOST/BUCKET, instead of Dir. It needs
	// S3Credentials (a mode-0600 file of key_id= and secret_key= lines).
	S3            string
	S3Credentials string
	S3Region      string // default "garage"
	// S3Plaintext must be "homelab" to use ModeNone with S3: a stated
	// acknowledgement that the bucket meets the rule in docs/archive.md, so
	// that a mistyped URL cannot send plaintext to some other bucket.
	S3Plaintext string
	// AdoptHost lets this machine write under a host name another machine
	// claimed in the bucket.
	AdoptHost bool
	Recipient string // age1... and/or ssh-... public keys (comma or newline separated), or a file of them
	Identity  string // AGE-SECRET-KEY-1... (or an unencrypted OpenSSH key), or a file holding it
	ClaudeDir string // default $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects
	PiDir     string // default ~/.pi/agent/sessions
	Index     string // default ~/.local/state/comebye/recall/index.db
}

// ErrNotConfigured means no archive location was given.
var ErrNotConfigured = errors.New("archive: no location configured (set COMEBYE_ARCHIVE_DIR or pass --dir, or COMEBYE_ARCHIVE_S3 or --s3)")

// Configured reports whether an archive location is set.
func (c Config) Configured() bool { return c.Dir != "" || c.S3 != "" }

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// identityText returns the identity's text from a literal or a file.
func identityText(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if strings.HasPrefix(v, "AGE-SECRET-KEY-") || strings.Contains(v, "PRIVATE KEY") {
		return v, nil
	}
	b, err := os.ReadFile(expand(v))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Sources returns the agent session directories to archive.
func (c Config) Sources() []Source {
	claude := c.ClaudeDir
	if claude == "" {
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			claude = filepath.Join(d, "projects")
		} else {
			claude = "~/.claude/projects"
		}
	}
	pi := c.PiDir
	if pi == "" {
		pi = "~/.pi/agent/sessions"
	}
	return []Source{{Kind: "claude", Root: expand(claude)}, {Kind: "pi", Root: expand(pi)}}
}

// IndexPath is where the (plaintext) index lives.
func (c Config) IndexPath() string {
	if c.Index != "" {
		return expand(c.Index)
	}
	return expand("~/.local/state/comebye/recall/index.db")
}

// Open builds the archiver. Encryption must be set. In ModeAge it needs
// recipients (or an identity, from which the native recipients are derived);
// in ModeNone it refuses recipients so a typo cannot leave an archive
// believed to be encrypted in plaintext.
func (c Config) Open() (*Archiver, error) { return c.open(false) }

// OpenDeferred is Open for a long-running server: with an S3 backend, the
// checks that need the network wait for the first write. An unreachable
// Garage must not stop the server from starting, and a failed write only
// costs a retry, because the session files are still on disk. Mistakes in the
// configuration itself still fail here, and a filesystem backend is checked
// at once as usual.
func (c Config) OpenDeferred() (*Archiver, error) { return c.open(true) }

func (c Config) open(deferred bool) (*Archiver, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if c.Dir != "" && c.S3 != "" {
		return nil, errors.New("archive: both a directory and an S3 bucket are configured; choose one")
	}
	var s Sealer
	switch c.Encryption {
	case ModeNone:
		if strings.TrimSpace(c.Recipient) != "" {
			return nil, errors.New("archive: encryption is \"none\" but recipients are set; remove them, or set encryption to \"age\"")
		}
		s = NoneSealer{}
	case ModeAge:
		spec := strings.TrimSpace(c.Recipient)
		if spec == "" {
			it, err := identityText(c.Identity)
			if err != nil || it == "" {
				return nil, errors.New("archive: no recipients (COMEBYE_ARCHIVE_RECIPIENT or --recipient: age1... keys, ssh public keys, or a file of them)")
			}
			if spec, err = RecipientsOf(it); err != nil {
				return nil, err
			}
		}
		var err error
		if s, err = NewSealer(spec); err != nil {
			return nil, err
		}
	case "":
		return nil, errors.New("archive: choose the encryption explicitly: --encryption age (recipients required) or --encryption none (plaintext; only where something else encrypts the copy). COMEBYE_ARCHIVE_ENCRYPTION")
	default:
		return nil, fmt.Errorf("archive: unknown encryption %q (want age or none)", c.Encryption)
	}
	host := c.Host
	if host == "" {
		h, _ := os.Hostname()
		host = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	if c.S3 != "" {
		if s.Mode() == ModeNone && c.S3Plaintext != "homelab" {
			return nil, errors.New("archive: encryption \"none\" with an S3 bucket needs COMEBYE_ARCHIVE_S3_PLAINTEXT=homelab, which states that the bucket meets the rule in docs/archive.md; otherwise use encryption age")
		}
		if c.S3Credentials == "" {
			return nil, errors.New("archive: an S3 bucket needs a credentials file (COMEBYE_ARCHIVE_S3_CREDENTIALS or --s3-credentials)")
		}
		id, secret, err := LoadS3Credentials(c.S3Credentials)
		if err != nil {
			return nil, err
		}
		b, err := NewS3Backend(c.S3, id, secret, c.S3Region)
		if err != nil {
			return nil, err
		}
		a := &Archiver{Host: host, B: b, S: s}
		a.beforeWrite = func() error { return a.claimHost(c.AdoptHost) }
		mode := s.Mode()
		if deferred {
			claim := a.beforeWrite
			a.beforeWrite = func() error {
				if err := EnsureMode(b, mode); err != nil {
					return err
				}
				return claim()
			}
			return a, nil
		}
		if err := EnsureMode(b, mode); err != nil {
			return nil, err
		}
		return a, nil
	}
	b, err := NewFSBackend(expand(c.Dir))
	if err != nil {
		return nil, err
	}
	if err := EnsureMode(b, s.Mode()); err != nil {
		return nil, err
	}
	return &Archiver{Host: host, B: b, S: s}, nil
}

// hostClaim is hosts/<host>.json: which machine writes under this host name.
// Segments are only safe from a lost HEAD-then-PUT race if one machine writes
// each host's prefix (see S3Backend), so a second machine is refused.
type hostClaim struct {
	Machine   string `json:"machine"` // a hash of the machine id
	Hostname  string `json:"hostname"`
	FirstSeen string `json:"first_seen"`
	// A worker's sessions are uploaded by the machine that runs tools/worker,
	// so Machine and Hostname above are that machine. See ClaimWorker.
	Kind            string `json:"kind,omitempty"`
	Worker          string `json:"worker,omitempty"` // its name, as the ledger has it
	Provider        string `json:"provider,omitempty"`
	InstanceID      string `json:"instance_id,omitempty"`
	InstanceCreated string `json:"instance_created,omitempty"`
}

const hostKindWorker = "worker"

func hostClaimKey(host string) string { return "hosts/" + host + ".json" }

// machineID identifies this machine without publishing its machine id.
func machineID() string {
	raw := ""
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
			raw = strings.TrimSpace(string(b))
			break
		}
	}
	if raw == "" {
		raw, _ = os.Hostname()
	}
	return hexSHA256([]byte("comebye-archive|" + raw))[:32]
}

// claimHost records this machine as the writer of a.Host, or checks that it is.
func (a *Archiver) claimHost(adopt bool) error {
	if adopt {
		return nil
	}
	key := hostClaimKey(a.Host)
	mine := machineID()
	read := func() error {
		raw, err := a.B.Get(key)
		if err != nil {
			return fmt.Errorf("archive: reading %s: %w", key, err)
		}
		var cl hostClaim
		if err := json.Unmarshal(raw, &cl); err != nil {
			return fmt.Errorf("archive: reading %s: %w", key, err)
		}
		if cl.Machine != mine {
			return fmt.Errorf("archive: host name %q belongs to another machine (%s, since %s). Set a different COMEBYE_HOST_NAME, or pass --adopt-host if this machine really replaces it", a.Host, cl.Hostname, cl.FirstSeen)
		}
		return nil
	}
	err := read()
	if err == nil || !errors.Is(err, ErrNotExist) {
		return err
	}
	name, _ := os.Hostname()
	body, _ := json.Marshal(hostClaim{Machine: mine, Hostname: name, FirstSeen: a.now().UTC().Format(time.RFC3339)})
	if err := a.B.Put(key, body); errors.Is(err, ErrExists) {
		return read()
	} else {
		return err
	}
}

// OpenIdentity returns the reading side for the configured mode: an age
// identity in ModeAge, nothing in ModeNone.
func (c Config) OpenIdentity() (Opener, error) {
	switch c.Encryption {
	case ModeNone:
		return NoneSealer{}, nil
	case ModeAge:
	default:
		return nil, errors.New("archive: choose the encryption explicitly (--encryption age|none)")
	}
	it, err := identityText(c.Identity)
	if err != nil {
		return nil, err
	}
	if it == "" {
		return nil, errors.New("archive: no identity (COMEBYE_ARCHIVE_IDENTITY); it is needed to read the archive back")
	}
	return NewOpener(it)
}

const modeKey = "ARCHIVE-MODE"

// EnsureMode records the archive's encryption mode in the backend, or
// checks it against the recorded one. An archive made before the marker
// existed is read from its first segment. A mismatch is an error: a
// plaintext archive must not quietly start receiving encrypted segments
// (or the reverse); `archive rewrap` converts.
func EnsureMode(b Backend, mode string) error {
	cur, err := ReadMode(b)
	if err != nil {
		return err
	}
	if cur == "" {
		err := b.Put(modeKey, []byte(mode+"\n"))
		if !errors.Is(err, ErrExists) {
			return err
		}
		// Another host wrote the marker first: go by what it wrote.
		if cur, err = ReadMode(b); err != nil {
			return err
		}
	}
	if cur != mode {
		return fmt.Errorf("archive: this archive is %q but encryption %q was asked for; mixing modes is refused (convert with `comebye archive rewrap --to-dir NEW --to-encryption %s`)", cur, mode, mode)
	}
	return nil
}

// ReadMode returns the recorded mode, inferring it from the first segment
// of an archive with no marker; "" for an empty archive.
func ReadMode(b Backend) (string, error) {
	raw, err := b.Get(modeKey)
	if err == nil {
		m := strings.TrimSpace(string(raw))
		if m != ModeAge && m != ModeNone {
			return "", fmt.Errorf("archive: unreadable mode marker %q", m)
		}
		return m, nil
	}
	if !errors.Is(err, ErrNotExist) {
		return "", fmt.Errorf("archive: reading the mode marker: %w", err)
	}
	keys, err := b.List("v1/")
	if err != nil || len(keys) == 0 {
		return "", err
	}
	body, err := b.Get(keys[0])
	if err != nil || len(body) < 4 {
		return "", fmt.Errorf("archive: cannot tell the mode of an archive with no marker")
	}
	if string(body[:4]) == magicNone {
		return ModeNone, nil
	}
	return ModeAge, nil
}

// CheckPerms returns a warning for each existing path that group or others
// can access. The archive directory, and the directory of the (plaintext)
// index, should be private to the user.
func CheckPerms(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if m := fi.Mode().Perm(); m&0o077 != 0 {
			out = append(out, fmt.Sprintf("%s is mode %04o: group or others can access it (chmod 700)", p, m))
		}
	}
	return out
}

// SessionRef names an archived session and its streams.
type SessionRef struct {
	Kind, Slug, SID string
	Streams         []string
	Bytes           int64
	Segments        int
}

// Sessions lists the archived sessions of this host.
func (a *Archiver) Sessions() ([]SessionRef, error) {
	state, err := scan(a.B, a.Host)
	if err != nil {
		return nil, err
	}
	m := map[string]*SessionRef{}
	for id, st := range state {
		kind, stream, _ := strings.Cut(id, "/")
		slug, sid, ok := SessionOf(kind, stream)
		if !ok {
			continue
		}
		k := kind + "/" + slug + "/" + sid
		r := m[k]
		if r == nil {
			r = &SessionRef{Kind: kind, Slug: slug, SID: sid}
			m[k] = r
		}
		r.Streams = append(r.Streams, stream)
		r.Bytes += st.Contiguous
		r.Segments += len(st.Segs)
	}
	var out []SessionRef
	for _, r := range m {
		sortStrings(r.Streams)
		out = append(out, *r)
	}
	sortRefs(out)
	return out, nil
}

// FindSession resolves an id, a unique prefix of one, or (pi) a prefix of
// the id after the timestamp.
func (a *Archiver) FindSession(id string) (SessionRef, error) {
	all, err := a.Sessions()
	if err != nil {
		return SessionRef{}, err
	}
	var hit []SessionRef
	for _, r := range all {
		if r.SID == id {
			return r, nil
		}
		if strings.HasPrefix(r.SID, id) || strings.Contains(r.SID, "_"+id) {
			hit = append(hit, r)
		}
	}
	switch len(hit) {
	case 0:
		return SessionRef{}, fmt.Errorf("no archived session %s on host %s (another host's session needs --host; `comebye recall list` shows the host)", id, a.Host)
	case 1:
		return hit[0], nil
	}
	return SessionRef{}, errors.New(id + " matches several archived sessions; give more of the id")
}

// Size is the stored size of an object (for status).
func (b *FSBackend) Size(key string) (int64, error) {
	fi, err := os.Stat(b.path(key))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
