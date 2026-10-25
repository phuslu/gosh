//go:build !windows

package gosh

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestInteractiveShellSurvivesTerminalStop sends SIGTSTP to the process
// group of an interactive gosh and the command it runs, as ^Z does. Neither
// may stay stopped.
func TestInteractiveShellSurvivesTerminalStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, testGoshBinary, "-i", "--norc")
	// Its own process group, so the stop cannot reach the test itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewBufferString("sh -c 'kill -TSTP 0; echo resumed'\necho alive\n")
	cmd.Env = testEnv(t)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gosh failed: %v\nstdout: %q\nstderr: %q", err, stdout.String(), stderr.String())
	}
	if got, want := stdout.String(), "resumed\nalive\n"; got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %q", got, want, stderr.String())
	}
}
