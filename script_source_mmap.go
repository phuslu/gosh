//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package gosh

import (
	"os"

	"golang.org/x/sys/unix"
)

func mapScriptFile(file *os.File, size int64) ([]byte, func() error, error) {
	data, err := unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return unix.Munmap(data) }, nil
}
