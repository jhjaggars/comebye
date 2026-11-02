package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// An archive object is a segment: the bytes of one source file from byte
// Off to Off+Len, zstd-compressed and age-encrypted. Segments of a file are
// contiguous and never rewritten, so the file at any past moment is the
// concatenation of its first N segments. The key carries the position:
//
//	v1/<host>/<kind>/<stream>/<offset:016x>-<length:016x>.seg
//
// where <stream> is the file's path relative to the agent's session root
// (Claude: ~/.claude/projects, pi: ~/.pi/agent/sessions).
//
// Body: "CBA2" | codec (2 = zstd) | an age file. The age plaintext is
// "cba2|host|kind|stream|offset|length\n" followed by the compressed bytes;
// the header is authenticated by age, and a segment whose header does not
// match its key (moved, renamed, or swapped) is refused. To read a segment
// without Comebye: drop the first 5 bytes, `age -d -i identity.key`, drop
// the header line, `zstd -d`.

const (
	magic     = "CBA2"
	codecZstd = 2
)

var (
	zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	zstdDec, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(1<<30), zstd.WithDecoderConcurrency(1))
)

// segName is a parsed segment key.
type segName struct {
	Host, Kind, Stream string
	Off, Len           int64
}

func segKeyFor(host, kind, stream string, off, n int64) string {
	return fmt.Sprintf("v1/%s/%s/%s/%016x-%016x.seg", host, kind, stream, off, n)
}

func streamPrefix(host, kind, stream string) string {
	return fmt.Sprintf("v1/%s/%s/%s/", host, kind, stream)
}

var segFileRE = regexp.MustCompile(`^([0-9a-f]{16})-([0-9a-f]{16})\.seg$`)

func parseSegKey(key string) (segName, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 5 || parts[0] != "v1" {
		return segName{}, false
	}
	m := segFileRE.FindStringSubmatch(parts[len(parts)-1])
	if m == nil {
		return segName{}, false
	}
	off, _ := strconv.ParseInt(m[1], 16, 64)
	n, _ := strconv.ParseInt(m[2], 16, 64)
	return segName{Host: parts[1], Kind: parts[2], Stream: strings.Join(parts[3:len(parts)-1], "/"), Off: off, Len: n}, true
}

func segHeader(host, kind, stream string, off, n int64) string {
	return fmt.Sprintf("cba2|%s|%s|%s|%d|%d\n", host, kind, stream, off, n)
}

func encodeSegment(s Sealer, host, kind, stream string, off int64, raw []byte) ([]byte, error) {
	plain := append([]byte(segHeader(host, kind, stream, off, int64(len(raw)))), zstdEnc.EncodeAll(raw, nil)...)
	sealed, err := s.Seal(plain)
	if err != nil {
		return nil, err
	}
	return append([]byte(magic+string([]byte{codecZstd})), sealed...), nil
}

func decodeSegment(o Opener, n segName, body []byte) ([]byte, error) {
	if len(body) < 5 || string(body[:4]) != magic {
		return nil, errors.New("archive: not a segment")
	}
	if body[4] != codecZstd {
		return nil, fmt.Errorf("archive: unknown codec %d", body[4])
	}
	plain, err := o.Open(body[5:])
	if err != nil {
		return nil, err
	}
	want := segHeader(n.Host, n.Kind, n.Stream, n.Off, n.Len)
	if !bytes.HasPrefix(plain, []byte(want)) {
		return nil, errors.New("archive: segment does not belong at this key (moved or swapped)")
	}
	raw, err := zstdDec.DecodeAll(plain[len(want):], nil)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != n.Len {
		return nil, fmt.Errorf("archive: segment holds %d bytes, key says %d", len(raw), n.Len)
	}
	return raw, nil
}

// streamState is what the backend holds for one stream.
type streamState struct {
	Segs []segName
	// Contiguous is how many bytes from offset 0 are stored without a gap.
	Contiguous int64
	// Overlap is set when two segments cover the same bytes (a conflict;
	// restore refuses such a stream).
	Overlap bool
}

func (st *streamState) finish() {
	sort.Slice(st.Segs, func(i, j int) bool {
		if st.Segs[i].Off != st.Segs[j].Off {
			return st.Segs[i].Off < st.Segs[j].Off
		}
		return st.Segs[i].Len < st.Segs[j].Len
	})
	var pos int64
	for _, s := range st.Segs {
		switch {
		case s.Off == pos:
			pos += s.Len
		case s.Off < pos:
			st.Overlap = true
		default:
			st.Contiguous = pos
			return // a gap
		}
	}
	st.Contiguous = pos
}

// scan lists everything this host has stored: stream key ("kind/stream")
// to state.
func scan(b Backend, host string) (map[string]*streamState, error) {
	keys, err := b.List("v1/" + host + "/")
	if err != nil {
		return nil, err
	}
	out := map[string]*streamState{}
	for _, k := range keys {
		n, ok := parseSegKey(k)
		if !ok {
			continue
		}
		id := n.Kind + "/" + n.Stream
		if out[id] == nil {
			out[id] = &streamState{}
		}
		out[id].Segs = append(out[id].Segs, n)
	}
	for _, st := range out {
		st.finish()
	}
	return out, nil
}

// StreamTo writes the whole stored stream to w, decrypting and checking
// every segment; it fails on a gap or an overlap.
func StreamTo(w io.Writer, b Backend, o Opener, host, kind, stream string) (int64, error) {
	keys, err := b.List(streamPrefix(host, kind, stream))
	if err != nil {
		return 0, err
	}
	st := &streamState{}
	for _, k := range keys {
		if n, ok := parseSegKey(k); ok && n.Stream == stream {
			st.Segs = append(st.Segs, n)
		}
	}
	if len(st.Segs) == 0 {
		return 0, fmt.Errorf("archive: no segments for %s/%s", kind, stream)
	}
	st.finish()
	var total int64
	for _, s := range st.Segs {
		total += s.Len
	}
	if st.Overlap || st.Contiguous != total {
		return 0, fmt.Errorf("archive: %s/%s has a gap or overlapping segments", kind, stream)
	}
	var written int64
	for _, s := range st.Segs {
		key := segKeyFor(host, kind, stream, s.Off, s.Len)
		body, err := b.Get(key)
		if err != nil {
			return written, err
		}
		raw, err := decodeSegment(o, segName{Host: host, Kind: kind, Stream: stream, Off: s.Off, Len: s.Len}, body)
		if err != nil {
			return written, fmt.Errorf("%s: %w", path.Base(key), err)
		}
		n, err := w.Write(raw)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func sortStrings(s []string) { sort.Strings(s) }

func sortRefs(r []SessionRef) {
	sort.Slice(r, func(i, j int) bool {
		if r[i].Kind != r[j].Kind {
			return r[i].Kind < r[j].Kind
		}
		return r[i].SID < r[j].SID
	})
}
