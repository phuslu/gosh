package gosh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if got := resolveHistoryLimit(); got != 500 {
		t.Fatalf("default history limit = %d, want 500", got)
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

func TestOpenScriptSourcePipe(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("echo pipe-input\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	defer reader.Close()

	src, err := openScriptSource(reader)
	if err != nil {
		t.Fatalf("openScriptSource(pipe): %v", err)
	}
	defer src.Close()
	if got, want := string(src.Data()), "echo pipe-input\n"; got != want {
		t.Fatalf("pipe data = %q, want %q", got, want)
	}
}

func TestOpenScriptSourceLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.sh")
	content := strings.Repeat("echo large\n", scriptSourceThreshold/10)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	src, err := openScriptSource(file)
	if err != nil {
		t.Fatalf("openScriptSource(large file): %v", err)
	}
	defer src.Close()
	if got := string(src.Data()); got != content {
		t.Fatalf("large file data mismatch: got %d bytes, want %d", len(got), len(content))
	}
	stdin, err := src.StdinFile()
	if err != nil {
		t.Fatalf("StdinFile: %v", err)
	}
	readBack, err := io.ReadAll(stdin)
	if err != nil {
		t.Fatalf("read StdinFile: %v", err)
	}
	if string(readBack) != content {
		t.Fatalf("StdinFile data mismatch")
	}
}

func TestPromptTemplateCache(t *testing.T) {
	cache := newPromptCache()
	first := cache.get(`\u@\h:\w`)
	second := cache.get(`\u@\h:\w`)
	if first != second {
		t.Fatal("prompt cache did not reuse an identical template")
	}
	state := &promptState{
		vars:      map[string]string{"USER": "alice", "HOME": "/home/alice"},
		dir:       "/home/alice",
		host:      "host.example",
		shortHost: "host",
	}
	if got := first.render(state); got != "alice@host:~" {
		t.Fatalf("cached prompt rendered %q", got)
	}
}

func TestHistoryAppendErrorIsReported(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-dir", "history")
	var reported error
	h := &history{cfg: historyConfig{inMemoryLimit: 10, file: missing}, onError: func(err error) { reported = err }}
	h.Add("echo lost")
	if reported == nil {
		t.Fatal("history onError hook was not invoked")
	}
}
