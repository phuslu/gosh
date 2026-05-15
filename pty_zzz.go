//go:build !windows

package gosh

import "io"

func ptyNewConsoleANSIWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
