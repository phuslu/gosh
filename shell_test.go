package gosh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
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

func TestShellEvalDoesNotRunExitTrap(t *testing.T) {
	var stdout bytes.Buffer
	s, err := New(Config{Stdout: &stdout, Env: testEnv(t)})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	for _, script := range []string{"trap 'echo bye' EXIT", "echo a", "echo b"} {
		if err := s.Eval(context.Background(), script); err != nil {
			t.Fatalf("Eval(%q) failed: %v", script, err)
		}
	}
	if err := s.Eval(context.Background(), "exit 3"); !IsExitStatus(err) || ExitCode(err) != 3 {
		t.Fatalf("Eval(exit 3) = %v, want exit status 3", err)
	}
	if got, want := stdout.String(), "a\nb\nbye\n"; got != want {
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
	if got := resolveShellHistoryLimit(runner); got != 500 {
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

func TestInteractiveShellExpandsAliases(t *testing.T) {
	const script = "alias hi='echo alias'\nhi\nexit\n"
	for _, tc := range []struct {
		name  string
		shell func(t *testing.T, stdout, stderr *bytes.Buffer) error
	}{
		{"forced interactive", func(t *testing.T, stdout, stderr *bytes.Buffer) error {
			return Run(Config{
				Args:   []string{"gosh", "-i", "--norc"},
				Stdin:  strings.NewReader(script),
				Stdout: stdout,
				Stderr: stderr,
				Env:    testEnv(t),
			})
		}},
		{"frontend attached later", func(t *testing.T, stdout, stderr *bytes.Buffer) error {
			shell, err := New(Config{
				Stdin:  strings.NewReader(script),
				Stdout: stdout,
				Stderr: stderr,
				Env:    testEnv(t),
			})
			if err != nil {
				return err
			}
			return shell.Interactive(context.Background())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := tc.shell(t, &stdout, &stderr); err != nil {
				t.Fatalf("shell failed: %v\nstderr: %s", err, stderr.String())
			}
			if got := stdout.String(); got != "alias\n" {
				t.Fatalf("stdout = %q, want %q\nstderr: %s", got, "alias\n", stderr.String())
			}
		})
	}
}

func TestInteractiveEOFRunsExitTrap(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh", "-i", "--norc"},
		Stdin:  strings.NewReader("trap 'echo bye:$?' EXIT\nexit() { echo function; }\nfalse\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if got := ExitCode(err); got != 1 {
		t.Fatalf("exit code = %d, want 1 (err: %v)\nstderr: %s", got, err, stderr.String())
	}
	if got, want := stdout.String(), "bye:1\n"; got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
}

func TestWgetBuiltinSavesInShellDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "payload")
	}))
	defer server.Close()

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	s, err := New(Config{
		Stdout: &stdout,
		Stderr: &stderr,
		// No wget on this PATH, whatever the host has.
		Env: []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := "cd " + dir + " && wget " + server.URL + "/file.txt"
	if err := s.Eval(context.Background(), script); err != nil {
		t.Fatalf("wget failed: %v\nstderr: %s", err, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil || string(data) != "payload" {
		t.Fatalf("saved file = %q, %v; want %q in the shell's directory", data, err, "payload")
	}
}

func TestWgetBuiltinDefersToBackend(t *testing.T) {
	backend := &mockBackend{}
	s, err := New(Config{Env: []string{"PATH=" + t.TempDir()}, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Eval(context.Background(), "wget http://127.0.0.1:1/x"); err != nil {
		t.Fatalf("wget failed: %v", err)
	}
	if got, _, _, _, _ := backend.snapshot(); len(got) != 1 || got[0][0] != "wget" {
		t.Fatalf("backend execs = %q, want the wget call", got)
	}
}

func TestTerminalSizeFallsBackWithoutTerminal(t *testing.T) {
	if term.IsTerminal(int(os.Stdout.Fd())) || term.IsTerminal(int(os.Stderr.Fd())) {
		t.Skip("the test output is a terminal")
	}
	if width, height := terminalSize(); width != 80 || height != 24 {
		t.Fatalf("terminalSize() = %dx%d, want 80x24", width, height)
	}
}
