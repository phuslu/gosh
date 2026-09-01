package main

import (
	"context"
	"os"

	"github.com/phuslu/gosh"
	"github.com/phuslu/pty"
)

func main() {
	err := gosh.Run(gosh.Config{
		Version:       "0.0.0",
		Args:          os.Args,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		NotifySignals: true,
		// Like Bash, interactivity follows stdin: prompts render on stderr
		// wherever it points, so redirecting stderr should not silently turn
		// the session into a non-interactive stdin script.
		IsTerminal:    pty.IsTerminal(os.Stdin.Fd()),
		OnPromptReset: func(_ context.Context) { pty.EnableVirtualTerminal(true, false, false) },
	})
	if err != nil {
		os.Exit(gosh.ExitCode(err))
	}
}
