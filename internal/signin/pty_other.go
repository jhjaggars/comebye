//go:build !linux && !darwin

package signin

import (
	"errors"
	"os"
)

func openPTY() (master, slave *os.File, err error) {
	return nil, nil, errors.New("sign-in needs a pseudo-terminal, which this platform does not have here")
}
