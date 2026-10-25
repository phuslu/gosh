//go:build !windows

package gosh

import (
	"os"
	"os/signal"
	"syscall"
)

// resumeStoppedCommands keeps ^Z from stopping an interactive shell which
// has no job control. The terminal sends SIGTSTP to its whole foreground
// process group: gosh would stop along with the running command, and the
// session would hang with nothing left to resume either. gosh catches it
// instead and continues its process group, which also discards a stop the
// command has not acted on yet, so ^Z has no effect, as in Bash with job
// control off. The returned function stops it.
func resumeStoppedCommands() func() {
	stops := make(chan os.Signal, 1)
	signal.Notify(stops, syscall.SIGTSTP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-stops:
				_ = syscall.Kill(0, syscall.SIGCONT)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(stops)
		close(done)
	}
}
