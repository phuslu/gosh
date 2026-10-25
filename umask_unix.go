//go:build !windows

package gosh

import "syscall"

// getUmask reads the process's file creation mask, which can only be read
// by setting it.
func getUmask() (int, error) {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return mask, nil
}

func setUmask(mask int) error {
	syscall.Umask(mask)
	return nil
}
