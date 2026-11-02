package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	Dir        string // the filesystem backend; Garage/S3 would add another field
	Recipient  string // age1... and/or ssh-... public keys (comma or newline separated), or a file of them
	Identity   string // AGE-SECRET-KEY-1... (or an unencrypted OpenSSH key), or a file holding it
	ClaudeDir  string // default $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects
	PiDir      string // default ~/.pi/agent/sessions
	Index      string // default ~/.local/state/comebye/recall/index.db
}

// ErrNotConfigured means no archive directory was given.
var ErrNotConfigured = errors.New("archive: no directory configured (set COMEBYE_ARCHIVE_DIR or pass --dir)")

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
func (c Config) Open() (*Archiver, error) {
	if c.Dir == "" {
		return nil, ErrNotConfigured
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
	b, err := NewFSBackend(expand(c.Dir))
	if err != nil {
		return nil, err
	}
	if err := EnsureMode(b, s.Mode()); err != nil {
		return nil, err
	}
	host := c.Host
	if host == "" {
		h, _ := os.Hostname()
		host = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	return &Archiver{Host: host, B: b, S: s}, nil
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
		return b.Put(modeKey, []byte(mode+"\n"))
	}
	if cur != mode {
		return fmt.Errorf("archive: this archive is %q but encryption %q was asked for; mixing modes is refused (convert with `comebye archive rewrap --to-dir NEW --to-encryption %s`)", cur, mode, mode)
	}
	return nil
}

// ReadMode returns the recorded mode, inferring it from the first segment
// of an archive with no marker; "" for an empty archive.
func ReadMode(b Backend) (string, error) {
	if raw, err := b.Get(modeKey); err == nil {
		m := strings.TrimSpace(string(raw))
		if m != ModeAge && m != ModeNone {
			return "", fmt.Errorf("archive: unreadable mode marker %q", m)
		}
		return m, nil
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
		return SessionRef{}, errors.New("no archived session " + id)
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
