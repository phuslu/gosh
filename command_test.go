package gosh

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"gosh", "-c"},
		{"gosh", "--rcfile"},
		{"gosh", "--unknown"},
	} {
		if _, err := parseCommand(args); err == nil {
			t.Fatalf("parseCommand(%q) = nil error, want error", args)
		}
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	var versionOut, helpOut bytes.Buffer
	if err := Run(Config{Args: []string{"gosh", "--version"}, Stdout: &versionOut, Stderr: &helpOut, Env: testEnv(t), Version: "1.2.3"}); err != nil {
		t.Fatalf("Run --version failed: %v", err)
	}
	if got, want := versionOut.String(), "gosh 1.2.3\n"; got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}

	if err := Run(Config{Args: []string{"gosh", "--help"}, Stdout: &helpOut, Env: testEnv(t)}); err != nil {
		t.Fatalf("Run --help failed: %v", err)
	}
	if !strings.Contains(helpOut.String(), "Usage: gosh") {
		t.Fatalf("--help output = %q", helpOut.String())
	}
}

func TestRunReadStdinWithParams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh", "-s", "one", "two"},
		Stdin:  strings.NewReader("printf '%s:%s\\n' \"$1\" \"$2\"\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil {
		t.Fatalf("Run -s failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "one:two\n"; got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
}

func TestRunForcedInteractiveFlag(t *testing.T) {
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(stdinPath, []byte("printf 'interactive=<%s>\\n' \"${GOSH_INTERACTIVE-}\"\nexit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	err = Run(Config{
		Args:   []string{"gosh", "-i"},
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil {
		t.Fatalf("Run -i failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "interactive=<1>\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunRcfileAndNorc(t *testing.T) {
	rcFile := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rcFile, []byte("printf 'rc-ran\n'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin := strings.NewReader("exit\n")

	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:       []string{"gosh", "--rcfile", rcFile},
		Stdin:      stdin,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Env:        testEnv(t),
		IsTerminal: true,
	})
	if err != nil {
		t.Fatalf("Run --rcfile failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "rc-ran\n"; got != want {
		t.Fatalf("rcfile stdout = %q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	err = Run(Config{
		Args:       []string{"gosh", "--norc", "--rcfile", rcFile},
		Stdin:      strings.NewReader("exit\n"),
		Stdout:     &stdout,
		Stderr:     &stderr,
		Env:        testEnv(t),
		IsTerminal: true,
	})
	if err != nil {
		t.Fatalf("Run --norc failed: %v\nstderr: %s", err, stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("--norc stdout = %q, want empty", got)
	}
}
