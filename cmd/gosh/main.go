package main

import (
	"os"

	"github.com/phuslu/gosh"
	"github.com/phuslu/pty"
)

func main() {
	err := gosh.Run(gosh.Config{
		Version:               "0.0.0",
		Args:                  os.Args,
		Stdin:                 os.Stdin,
		Stdout:                os.Stdout,
		Stderr:                os.Stderr,
		NotifySignals:         true,
		IsTerminal:            pty.IsTerminal(os.Stdin.Fd()) && pty.IsTerminal(os.Stderr.Fd()),
		EnableVirtualTerminal: pty.EnableVirtualTerminal,
	})
	if err != nil {
		os.Exit(gosh.ExitCode(err))
	}
}
