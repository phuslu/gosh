package gosh

import (
	"errors"
	"io"
)

// builtinUlimit reports that Windows has no resource limits.
func builtinUlimit(args []string, stdout io.Writer) error {
	return errors.New("ulimit: not supported on Windows")
}
