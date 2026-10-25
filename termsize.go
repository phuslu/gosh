package gosh

import (
	"os"

	"golang.org/x/term"
)

// terminalSize reports the terminal size to readline, from stdout or else
// stderr. A terminal which reports no size, such as a serial console or a
// fresh pseudo-terminal, counts as 80x24: readline refuses to complete on a
// terminal 0 columns wide, and Tab would only ring the bell.
func terminalSize() (width, height int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width, height, _ = term.GetSize(int(os.Stderr.Fd()))
	}
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	return width, height
}
