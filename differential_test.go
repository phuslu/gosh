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

// runInteractiveHistory runs an interactive shell on piped input with a
// private history file and returns everything it printed to stdout.
func runInteractiveHistory(t *testing.T, binary, argv0 string, extraArgs []string, lines []string, env []string) differentialShellResult {
	t.Helper()
	args := extraArgs
	if argv0 != "" {
		args = append([]string{argv0}, extraArgs...)
	}
	return runDifferentialShell(t, binary, args, lines, env, "")
}

// historyEnv builds the environment shared by the interactive history
// differential tests, with host history settings stripped out.
func historyEnv(t *testing.T, histFile string) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"HISTFILE=" + histFile,
		"HISTSIZE=",
		"HISTFILESIZE=",
		"HISTCONTROL=",
		"GOSH_ENV=",
	}
}

func TestDifferentialHistoryCmdhist(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"if", []string{"if true; then", "echo hi", "fi"}},
		{"pipeline", []string{"echo a |", "cat"}},
		{"pipeline trailing space", []string{"echo a | ", "cat"}},
		{"for", []string{"for x in 1; do", "echo x", "done"}},
		{"for in", []string{"for i", "in a b; do", "echo $i", "done"}},
		{"for do", []string{"for x", "do", "echo x", "done"}},
		{"brace group", []string{"{", "echo a", "echo b", "}"}},
		{"brace blank", []string{"{", "", "echo a", "}"}},
		{"brace blank after word", []string{"{ echo a", "", "}"}},
		{"case", []string{"case $x in a)", "echo x;", ";;", "esac"}},
		{"while", []string{"while true; do", "echo x", "break", "done"}},
		{"until", []string{"until false; do", "echo x", "break", "done"}},
		{"nested for", []string{"for x in a; do", "for y in b; do", "echo $x $y", "done", "done"}},
		{"if elif else", []string{"if true; then", "echo a", "elif false; then", "echo b", "else", "echo c", "fi"}},
		{"function braces", []string{"f() {", "echo hi", "}"}},
		{"function name", []string{"function f", "echo hi"}},
		{"subshell", []string{"(echo a", "echo b)"}},
		{"compound assignment", []string{"arr=(", "one two", "three", ")"}},
		{"backslash continuation", []string{"echo a \\", "echo b"}},
		{"single quote", []string{"echo 'a", "b'"}},
		{"double quote", []string{"echo \"c", "d\""}},
		{"command substitution", []string{"x=$(echo a", "echo b)"}},
		{"quotes in substitution", []string{"x=$(a", "\"b", "c\")", "echo d"}},
		{"arithmetic expansion", []string{"echo $((1 +", "2))"}},
		{"comment dropped", []string{"if true; then", "# comment", "echo hi", "fi"}},
		{"trailing comment", []string{"if true; then", "echo a # trailing", "fi"}},
		{"heredoc in compound", []string{"{ cat <<EOF", "x", "EOF", "echo done", "}"}},
		{"two heredocs", []string{"{ cat <<A", "a1", "A", "cat <<B", "b1", "B", "}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := append(append([]string{}, tc.lines...), "history", "exit")
			bashFile := filepath.Join(t.TempDir(), "bash-history")
			goshFile := filepath.Join(t.TempDir(), "gosh-history")
			want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, bashFile))
			got := runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, historyEnv(t, goshFile))
			if got.stdout != want.stdout {
				t.Fatalf("history output mismatch:\nbash: %q (stderr %q)\ngosh: %q (stderr %q)", want.stdout, want.stderr, got.stdout, got.stderr)
			}
		})
	}
}

func TestDifferentialHistoryCmdhistOff(t *testing.T) {
	script := []string{"shopt -u cmdhist", "if true; then", "echo hi", "fi", "history", "exit"}
	bashFile := filepath.Join(t.TempDir(), "bash-history")
	goshFile := filepath.Join(t.TempDir(), "gosh-history")
	want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, bashFile))
	got := runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, historyEnv(t, goshFile))
	if got.stdout != want.stdout {
		t.Fatalf("history output mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", want.stdout, got.stdout, got.stderr)
	}
}

func TestDifferentialHistoryLithist(t *testing.T) {
	script := []string{"shopt -s lithist", "if true; then", "echo hi", "fi", "history", "exit"}
	bashFile := filepath.Join(t.TempDir(), "bash-history")
	goshFile := filepath.Join(t.TempDir(), "gosh-history")
	want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, bashFile))
	got := runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, historyEnv(t, goshFile))
	if got.stdout != want.stdout {
		t.Fatalf("history output mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", want.stdout, got.stdout, got.stderr)
	}
}

func TestDifferentialHistoryLimits(t *testing.T) {
	t.Run("HISTSIZE", func(t *testing.T) {
		script := []string{"echo 1", "echo 2", "echo 3"}
		bashFile := filepath.Join(t.TempDir(), "bash-history")
		goshFile := filepath.Join(t.TempDir(), "gosh-history")
		bashEnv := append(historyEnv(t, bashFile), "HISTSIZE=2")
		goshEnv := append(historyEnv(t, goshFile), "HISTSIZE=2")
		runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, bashEnv)
		runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, goshEnv)
		bashData, err := os.ReadFile(bashFile)
		if err != nil {
			t.Fatal(err)
		}
		goshData, err := os.ReadFile(goshFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(goshData) != string(bashData) {
			t.Fatalf("HISTSIZE=2 history file mismatch:\nbash: %q\ngosh: %q", bashData, goshData)
		}
	})

	t.Run("HISTFILESIZE", func(t *testing.T) {
		seed := strings.Join([]string{
			"echo 1", "echo 2", "echo 3", "echo 4", "echo 5",
			"echo 6", "echo 7", "echo 8", "echo 9", "echo 10",
		}, "\n") + "\n"
		bashFile := filepath.Join(t.TempDir(), "bash-history")
		goshFile := filepath.Join(t.TempDir(), "gosh-history")
		if err := os.WriteFile(bashFile, []byte(seed), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goshFile, []byte(seed), 0o666); err != nil {
			t.Fatal(err)
		}
		script := []string{"echo new"}
		bashEnv := append(historyEnv(t, bashFile), "HISTFILESIZE=3")
		goshEnv := append(historyEnv(t, goshFile), "HISTFILESIZE=3")
		runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, bashEnv)
		runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, goshEnv)
		bashData, err := os.ReadFile(bashFile)
		if err != nil {
			t.Fatal(err)
		}
		goshData, err := os.ReadFile(goshFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(goshData) != string(bashData) {
			t.Fatalf("HISTFILESIZE=3 history file mismatch:\nbash: %q\ngosh: %q", bashData, goshData)
		}
	})

	t.Run("HISTFILESIZE zero", func(t *testing.T) {
		bashFile := filepath.Join(t.TempDir(), "bash-history")
		goshFile := filepath.Join(t.TempDir(), "gosh-history")
		seed := "echo 1\necho 2\n"
		if err := os.WriteFile(bashFile, []byte(seed), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goshFile, []byte(seed), 0o666); err != nil {
			t.Fatal(err)
		}
		script := []string{"echo new"}
		bashEnv := append(historyEnv(t, bashFile), "HISTFILESIZE=0")
		goshEnv := append(historyEnv(t, goshFile), "HISTFILESIZE=0")
		runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, bashEnv)
		runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, goshEnv)
		bashData, err := os.ReadFile(bashFile)
		if err != nil {
			t.Fatal(err)
		}
		goshData, err := os.ReadFile(goshFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(goshData) != string(bashData) {
			t.Fatalf("HISTFILESIZE=0 history file mismatch:\nbash: %q\ngosh: %q", bashData, goshData)
		}
	})
}
