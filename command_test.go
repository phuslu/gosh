package gosh

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCommandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"gosh", "-c"},
		{"gosh", "--rcfile"},
		{"gosh", "--unknown"},
		{"gosh", "-z"},
		{"gosh", "-o"},
		{"gosh", "-o", "bogus"},
	} {
		_, err := parseCommand(args)
		if err == nil {
			t.Fatalf("parseCommand(%q) = nil error, want error", args)
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode(parseCommand(%q)) = %d, want 2", args, got)
		}
	}
}

func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want commandSpec
	}{
		{
			args: []string{"gosh", "-c", "echo", "zero", "one"},
			want: commandSpec{isCommand: true, script: "echo", argv0: "zero", params: []string{"one"}},
		},
		{
			args: []string{"gosh", "-ec", "echo"},
			want: commandSpec{isCommand: true, script: "echo", argv0: "gosh", setArgs: []string{"-e"}},
		},
		{
			args: []string{"gosh", "-e", "+x", "-o", "pipefail", "-v", "-c", "echo"},
			want: commandSpec{isCommand: true, script: "echo", argv0: "gosh", setArgs: []string{"-e", "+x", "-o", "pipefail"}},
		},
		{
			args: []string{"gosh", "script.sh", "-c", "one"},
			want: commandSpec{scriptFile: "script.sh", argv0: "script.sh", params: []string{"-c", "one"}},
		},
		{
			args: []string{"gosh", "--", "-script"},
			want: commandSpec{scriptFile: "-script", argv0: "-script", params: []string{}},
		},
		{
			args: []string{"gosh", "-s", "one", "-x"},
			want: commandSpec{readStdin: true, argv0: "gosh", params: []string{"one", "-x"}},
		},
		{
			args: []string{"gosh", "--norc", "-i"},
			want: commandSpec{noRC: true, interactive: true, argv0: "gosh", params: nil},
		},
	} {
		got, err := parseCommand(tc.args)
		if err != nil {
			t.Fatalf("parseCommand(%q) failed: %v", tc.args, err)
		}
		if len(got.params) == 0 && len(tc.want.params) == 0 {
			got.params, tc.want.params = nil, nil
		}
		if !reflect.DeepEqual(*got, tc.want) {
			t.Fatalf("parseCommand(%q) = %+v, want %+v", tc.args, *got, tc.want)
		}
	}
}

func TestRunScriptFile(t *testing.T) {
	script := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(script, []byte("read -r line\nprintf '%s|%s|%s|%s\\n' \"$0\" \"$1\" \"$2\" \"$line\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:       []string{"gosh", script, "one", "two"},
		Stdin:      strings.NewReader("from-stdin\n"),
		Stdout:     &stdout,
		Stderr:     &stderr,
		Env:        testEnv(t),
		IsTerminal: true,
	})
	if err != nil {
		t.Fatalf("Run script failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), script+"|one|two|from-stdin\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	err = Run(Config{Args: []string{"gosh", filepath.Join(t.TempDir(), "missing.sh")}, Env: testEnv(t)})
	if got := ExitCode(err); got != 127 {
		t.Fatalf("missing script exit code = %d, want 127 (err: %v)", got, err)
	}
}

func TestRunSetOptionsFromCommandLine(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		stdin    string
		wantOut  string
		wantCode int
	}{
		{args: []string{"gosh", "-e", "-c", "false; echo after"}, wantCode: 1},
		{args: []string{"gosh", "-o", "pipefail", "-c", "false | true"}, wantCode: 1},
		{args: []string{"gosh", "-eu", "-c", "echo $-"}, wantOut: "eu\n"},
		{args: []string{"gosh", "-e", "-s"}, stdin: "false\necho after\n", wantCode: 1},
		{args: []string{"gosh", "-c", "if"}, wantCode: 2},
	} {
		var stdout, stderr bytes.Buffer
		err := Run(Config{
			Args:   tc.args,
			Stdin:  strings.NewReader(tc.stdin),
			Stdout: &stdout,
			Stderr: &stderr,
			Env:    testEnv(t),
		})
		if got := ExitCode(err); got != tc.wantCode {
			t.Fatalf("%q: exit code = %d, want %d (err: %v, stderr: %s)", tc.args, got, tc.wantCode, err, stderr.String())
		}
		if got := stdout.String(); got != tc.wantOut {
			t.Fatalf("%q: stdout = %q, want %q", tc.args, got, tc.wantOut)
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
