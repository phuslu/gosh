package gosh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func TestShellEvalKeepsState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	s, err := New(Config{
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := s.Eval(context.Background(), "greeting=hello"); err != nil {
		t.Fatalf("Eval assignment failed: %v", err)
	}
	if err := s.Eval(context.Background(), `printf '%s\n' "$greeting"`); err != nil {
		t.Fatalf("Eval print failed: %v", err)
	}
	if got, want := stdout.String(), "hello\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestShellStateDoesNotLeakHostEnvironment(t *testing.T) {
	t.Setenv("GOSH_LEAK_HOST_ONLY", "secret")
	t.Setenv("HISTCONTROL", "ignoredups")
	t.Setenv("HISTFILE", "/host/history")

	runner, err := interp.New(
		interp.StdIO(strings.NewReader(""), io.Discard, io.Discard),
		interp.Env(expand.ListEnviron("PATH=/bin")),
	)
	if err != nil {
		t.Fatal(err)
	}
	completer := &autoCompleter{
		ctx:    context.Background(),
		runner: runner,
		stdin:  strings.NewReader(""),
		stderr: io.Discard,
	}
	if got := completer.shellVar("GOSH_LEAK_HOST_ONLY"); got != "" {
		t.Fatalf("completion leaked host variable: %q", got)
	}
	if got := resolveShellHistoryControl(runner); got != (historyControl{}) {
		t.Fatalf("history control leaked host HISTCONTROL: %#v", got)
	}
	if got := resolveShellHistoryFile(runner); got != "" {
		t.Fatalf("history file leaked host HISTFILE: %q", got)
	}
	if got := resolveHistoryLimit(); got != 1000 {
		t.Fatalf("default history limit = %d, want 1000", got)
	}
}

func TestInteractiveContextCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	promptReset := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(Config{
			Args:       []string{"gosh"},
			Context:    ctx,
			Stdin:      pr,
			Stdout:     io.Discard,
			Stderr:     io.Discard,
			Env:        testEnv(t),
			IsTerminal: true,
			OnPromptReset: func(context.Context) {
				select {
				case promptReset <- struct{}{}:
				default:
				}
			},
		})
	}()

	select {
	case <-promptReset:
	case <-time.After(5 * time.Second):
		t.Fatal("interactive session never reached the prompt")
	}
	// Give readline a moment to enter its blocking read, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled interactive Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellation did not interrupt readline")
	}
}
