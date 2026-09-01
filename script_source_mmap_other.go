//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package gosh

import (
	"io"
	"os"
)

func mapScriptFile(file *os.File, _ int64) ([]byte, func() error, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}
