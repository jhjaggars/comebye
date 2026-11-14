// Package passkey checks WebAuthn passkeys with no dependency beyond the
// standard library: the small CBOR subset a registration carries, the COSE
// ES256 key, attestation "none", and the assertion check.
package passkey

import (
	"errors"
	"fmt"
)

// A minimal CBOR decoder: definite lengths only, maps with integer or text
// keys, nesting capped. It decodes into int64, uint64 (for values above
// int64), []byte, string, bool, nil, []any and map[any]any. Anything else
// (tags, floats, indefinite lengths) is refused: a passkey needs none of it.

const maxCBORDepth = 8

var errCBOR = errors.New("malformed CBOR")

type cborReader struct {
	b []byte
}

// cborDecode decodes one item and returns the bytes after it.
func cborDecode(b []byte) (any, []byte, error) {
	r := &cborReader{b: b}
	v, err := r.item(0)
	if err != nil {
		return nil, nil, err
	}
	return v, r.b, nil
}

func (r *cborReader) take(n uint64) ([]byte, error) {
	if n > uint64(len(r.b)) {
		return nil, fmt.Errorf("%w: truncated", errCBOR)
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out, nil
}

// head reads the initial byte and its argument.
func (r *cborReader) head() (major byte, arg uint64, err error) {
	if len(r.b) == 0 {
		return 0, 0, fmt.Errorf("%w: truncated", errCBOR)
	}
	ib := r.b[0]
	r.b = r.b[1:]
	major, ai := ib>>5, ib&0x1f
	switch {
	case ai < 24:
		return major, uint64(ai), nil
	case ai >= 24 && ai <= 27:
		n := 1 << (ai - 24)
		raw, err := r.take(uint64(n))
		if err != nil {
			return 0, 0, err
		}
		for _, c := range raw {
			arg = arg<<8 | uint64(c)
		}
		return major, arg, nil
	}
	return 0, 0, fmt.Errorf("%w: indefinite or reserved length", errCBOR)
}

func (r *cborReader) item(depth int) (any, error) {
	if depth > maxCBORDepth {
		return nil, fmt.Errorf("%w: nested too deep", errCBOR)
	}
	major, arg, err := r.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		if arg > 1<<63-1 {
			return nil, fmt.Errorf("%w: integer too large", errCBOR)
		}
		return int64(arg), nil
	case 1:
		if arg > 1<<63-1 {
			return nil, fmt.Errorf("%w: integer too large", errCBOR)
		}
		return -1 - int64(arg), nil
	case 2:
		raw, err := r.take(arg)
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), raw...), nil
	case 3:
		raw, err := r.take(arg)
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	case 4:
		if arg > uint64(len(r.b)) { // each element takes at least a byte
			return nil, fmt.Errorf("%w: truncated", errCBOR)
		}
		out := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		if arg > uint64(len(r.b))/2 {
			return nil, fmt.Errorf("%w: truncated", errCBOR)
		}
		out := make(map[any]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, fmt.Errorf("%w: map key of a kind this check does not read", errCBOR)
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%w: duplicate map key", errCBOR)
			}
			v, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case 7:
		switch arg {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22:
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%w: unsupported item", errCBOR)
}
