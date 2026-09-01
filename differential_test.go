package gosh

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testGoshBinary is built once for the whole test run so that the
// differential suite exercises the real gosh command rather than the
// in-process library. This matches how gosh behaves when embedded behind a
// CLI frontend.
var testGoshBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gosh-test-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
		os.Exit(1)
	}
	testGoshBinary = filepath.Join(dir, "gosh")
	if runtime.GOOS == "windows" {
		testGoshBinary += ".exe"
	}

	absBinary, err := filepath.Abs(testGoshBinary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve binary path: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	testGoshBinary = absBinary
	cmd := exec.Command("go", "build", "-C", "./cmd/gosh", "-o", testGoshBinary, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build gosh test binary: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type differentialShellResult struct {
	stdout string
	stderr string
	code   int
}

func runDifferentialShell(t *testing.T, binary string, args []string, stdin, env []string, dir string) differentialShellResult {
	t.Helper()
	cmd := exec.Command(binary, args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(strings.Join(stdin, "\n"))
	}
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = testEnv(t)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := differentialShellResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		res.code = 0
	} else if exit, ok := err.(*exec.ExitError); ok {
		res.code = exit.ExitCode()
	} else {
		t.Fatalf("run %s: %v\nstderr: %s", binary, err, stderr.String())
	}
	return res
}

func TestDifferentialCoreScripts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt", "binary"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(testEnv(t), "GREETING=hello")

	cases := []struct {
		name   string
		script string
	}{
		{"printf", `printf '%s:%d\n' hello 42`},
		{"quoting", `printf '<%s>\n' 'a b' "c d" a\ b`},
		{"variable-expansion", `x=world; printf '%s\n' "hello ${x}" "${x:-fallback}"`},
		{"parameter-default", `printf '%s\n' "${UNSET_VAR-default}" "${GREETING-default}"`},
		{"arithmetic", `printf '%d %d\n' $((3*4+1)) $((1<<4))`},
		{"command-substitution", `printf '%s\n' "$(printf 'a\nb\n')"`},
		{"pipeline", `printf 'b\na\nc\n' | sort | tr a-z A-Z`},
		{"redirect", `printf 'to-file\n' >redirect.out; cat redirect.out`},
		{"glob", `printf '%s\n' *.txt`},
		{"function", `greet() { printf 'hi %s\n' "$1"; }; greet world`},
		{"if-statement", `if [ 1 -eq 1 ]; then echo yes; else echo no; fi`},
		{"case-statement", `case foo in foo) echo foo;; *) echo other;; esac`},
		{"for-loop", `for x in a b c; do printf '%s\n' "$x"; done`},
		{"while-loop", `i=0; while [ "$i" -lt 3 ]; do echo "$i"; i=$((i+1)); done`},
		{"arrays", `a=(one two three); printf '%s\n' "${a[1]}" "${#a[@]}"`},
		{"exit-status", `false; echo "$?"; true; echo "$?"`},
		{"conditional-and-or", `false && echo bad || echo recovered`},
		{"read", `read line < /dev/null; printf '<%s>\n' "$line"`},
		{"heredoc", `cat <<EOF
one
two
EOF`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bashArgs := []string{"--noprofile", "--norc", "-c", tc.script}
			want := runDifferentialShell(t, "bash", bashArgs, nil, env, dir)
			got := runDifferentialShell(t, testGoshBinary, []string{"gosh", "-c", tc.script}, nil, env, dir)
			if got.code != want.code {
				t.Fatalf("exit code mismatch: bash=%d gosh=%d\nbash stdout: %q\ngosh stdout: %q\ngosh stderr: %q",
					want.code, got.code, want.stdout, got.stdout, got.stderr)
			}
			if got.stdout != want.stdout {
				t.Fatalf("stdout mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", want.stdout, got.stdout, got.stderr)
			}
			// Error text can legitimately differ between implementations, but
			// successful scripts must not print anything to stderr.
			if want.code == 0 && got.stderr != "" {
				t.Fatalf("gosh wrote to stderr for a successful script: %q", got.stderr)
			}
		})
	}
}

func TestDifferentialNonInteractiveStdin(t *testing.T) {
	script := []string{
		"read value",
		`printf 'read:<%s>\n' "$value"`,
		"value=set-here",
		`printf 'var:<%s>\n' "$value"`,
		"exit 0",
	}
	env := testEnv(t)

	bash := runDifferentialShell(t, "bash", []string{"--noprofile", "--norc"}, script, env, "")
	gosh := runDifferentialShell(t, testGoshBinary, []string{"gosh"}, script, env, "")
	if gosh.code != bash.code {
		t.Fatalf("exit code mismatch: bash=%d gosh=%d", bash.code, gosh.code)
	}
	if gosh.stdout != bash.stdout {
		t.Fatalf("stdout mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", bash.stdout, gosh.stdout, gosh.stderr)
	}
}
