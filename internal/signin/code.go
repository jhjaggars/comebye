package signin

import "errors"

// MaxCode is the longest code taken.
const MaxCode = 512

// ErrBadCode is returned for a code of the wrong shape. It never carries the
// code.
var ErrBadCode = errors.New("the code is empty, longer than 512 bytes, or has a character other than letters, digits and _ # . ~ -")

// ValidCode reports whether c has the shape of a pasted code: 1 to 512
// bytes of [A-Za-z0-9_#.~-]. Nothing else (a newline, an escape, a control
// byte) may reach the process.
func ValidCode(c string) bool {
	if len(c) == 0 || len(c) > MaxCode {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '_', b == '#', b == '.', b == '~', b == '-':
		default:
			return false
		}
	}
	return true
}
