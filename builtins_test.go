package gosh

import (
	"bytes"
	"regexp"
	"runtime"
	"testing"
)

// TestDifferentialBuiltins compares builtins gosh implements itself with
// Bash, output and exit status. Error messages are compared without Bash's
// "bash: line N: " prefix.
func TestDifferentialBuiltins(t *testing.T) {
	requireBash(t)
	if runtime.GOOS == "windows" {
		t.Skip("no umask or ulimit on Windows")
	}
	for _, script := range []string{
		`umask; umask -S; umask -p; umask -pS; umask 027; umask; umask g+w,o=r; umask -S; umask a-x; umask -S`,
		`umask 9; echo rc=$?; umask u=xyz; echo rc=$?; umask -z; echo rc=$?`,
		`umask 077; : > f; ls -l f | cut -c1-10; umask 022; : > g; ls -l g | cut -c1-10`,
		`ulimit; ulimit -a; ulimit -n; ulimit -Sn; ulimit -Hn; ulimit -H -c -n`,
		`ulimit -n 512; ulimit -n; ulimit -S -n 256; ulimit -Sn; ulimit -Hn; ulimit -n soft; ulimit -Hn; sh -c 'ulimit -n'`,
		`ulimit -p 1; echo rc=$?; ulimit -z; echo rc=$?; ulimit -n abc; echo rc=$?; ulimit -t unlimited; ulimit -t`,
	} {
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			want := runDifferentialShell(t, "bash", []string{"--norc", "-c", script}, nil, nil, dir)
			got := runDifferentialShell(t, testGoshBinary, []string{"-c", script}, nil, nil, t.TempDir())
			if got.stdout != want.stdout || got.code != want.code {
				t.Fatalf("mismatch:\nbash: rc=%d %q\ngosh: rc=%d %q (stderr %q)", want.code, want.stdout, got.code, got.stdout, got.stderr)
			}
			if wantErr := bashErrorPrefix.ReplaceAllString(want.stderr, ""); got.stderr != wantErr {
				t.Fatalf("stderr mismatch:\nbash: %q\ngosh: %q", wantErr, got.stderr)
			}
		})
	}
}

// bashErrorPrefix matches the "bash: line N: " Bash puts before errors in
// scripts.
var bashErrorPrefix = regexp.MustCompile(`(?m)^bash: line [0-9]+: `)

func TestDynamicVariables(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			[ "$SECONDS" -lt 5 ] && echo seconds
			[ "$EPOCHSECONDS" -gt 1700000000 ] && echo epoch
			[[ $EPOCHREALTIME =~ ^[0-9]+\.[0-9]{6}$ ]] && echo realtime
			[ "$BASHPID" = "$$" ] && echo bashpid
			SECONDS=500; echo "$SECONDS"
			unset EPOCHSECONDS; echo "${EPOCHSECONDS-unset}"
			env | grep -c -e ^SECONDS= -e ^BASHPID= -e ^EPOCH
		`},
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil && ExitCode(err) != 1 {
		t.Fatalf("Run failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "seconds\nepoch\nrealtime\nbashpid\n500\nunset\n0\n"; got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
}
