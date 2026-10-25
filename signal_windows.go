//go:build windows

package gosh

// resumeStoppedCommands has nothing to do on Windows, which has no SIGTSTP.
func resumeStoppedCommands() func() {
	return func() {}
}
