//go:build windows

package gosh

import "errors"

// Windows has no file creation mask; it reads as the usual 022.
func getUmask() (int, error) { return 0o022, nil }

func setUmask(mask int) error {
	return errors.New("not supported on Windows")
}
