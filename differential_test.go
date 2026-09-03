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

// requireBash skips a differential test when no bash is installed; the
// suite is expected to run unattended on machines without one.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not available: %v", err)
	}
}

// runBothShells runs one -c script under bash and gosh and returns both
// results, bash first.
func runBothShells(t *testing.T, script string, env []string, dir string) (bash, gosh differentialShellResult) {
	t.Helper()
	bash = runDifferentialShell(t, "bash", []string{"--noprofile", "--norc", "-c", script}, nil, env, dir)
	gosh = runDifferentialShell(t, testGoshBinary, []string{"gosh", "-c", script}, nil, env, dir)
	return bash, gosh
}

// assertSameScript requires gosh to match bash on stdout and exit status for
// script. Diagnostics on stderr are allowed to differ in wording, but a
// script bash completes successfully must stay silent under gosh too.
func assertSameScript(t *testing.T, script string, env []string, dir string) {
	t.Helper()
	want, got := runBothShells(t, script, env, dir)
	if got.code != want.code {
		t.Fatalf("exit code mismatch for %q: bash=%d gosh=%d\nbash stdout: %q\ngosh stdout: %q\ngosh stderr: %q",
			script, want.code, got.code, want.stdout, got.stdout, got.stderr)
	}
	if got.stdout != want.stdout {
		t.Fatalf("stdout mismatch for %q:\nbash: %q\ngosh: %q\ngosh stderr: %q", script, want.stdout, got.stdout, got.stderr)
	}
	if want.code == 0 && got.stderr != "" {
		t.Fatalf("gosh wrote to stderr for a successful script %q: %q", script, got.stderr)
	}
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
		{"single line", []string{"echo one"}},
		{"if", []string{"if true; then", "echo hi", "fi"}},
		{"pipeline", []string{"echo a |", "cat"}},
		{"and", []string{"echo a &&", "echo b"}},
		{"or", []string{"echo a ||", "echo b"}},
		{"pipe amp", []string{"echo a |&", "cat"}},
		{"pipeline trailing space", []string{"echo a | ", "cat"}},
		{"for", []string{"for x in 1; do", "echo x", "done"}},
		{"for in", []string{"for i", "in a b; do", "echo $i", "done"}},
		{"for do", []string{"for x", "do", "echo x", "done"}},
		{"for in no do", []string{"for x in", "a b"}},
		{"brace group", []string{"{", "echo a", "echo b", "}"}},
		{"brace blank", []string{"{", "", "echo a", "}"}},
		{"brace blank after word", []string{"{ echo a", "", "}"}},
		{"case", []string{"case $x in a)", "echo x;", ";;", "esac"}},
		{"case comment", []string{"case x in", "a) echo a", "# c", ";;", "esac"}},
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
		{"blank inside quotes", []string{"echo \"a", "", "b\""}},
		{"command substitution", []string{"x=$(echo a", "echo b)"}},
		{"quotes in substitution", []string{"x=$(a", "\"b", "c\")", "echo d"}},
		{"arithmetic expansion", []string{"echo $((1 +", "2))"}},
		{"comment dropped", []string{"if true; then", "# comment", "echo hi", "fi"}},
		{"two comments", []string{"if true; then", "# c1", "# c2", "echo hi", "fi"}},
		{"trailing comment", []string{"if true; then", "echo a # trailing", "fi"}},
		{"first line trailing comment", []string{"if true; then # c", "echo hi", "fi"}},
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

// TestDifferentialHistoryCmdhistKnownDivergences records multi-line commands
// whose history entry still differs from bash's. Every subtest is skipped:
// the comment states what bash records, and removing the t.Skip line turns
// the case into a live differential test once gosh matches.
func TestDifferentialHistoryCmdhistKnownDivergences(t *testing.T) {
	requireBash(t)
	cases := []struct {
		name   string
		reason string
		lines  []string
	}{
		{
			name: "arithmetic for",
			// bash: keeps reading until the loop is complete and records one
			// entry, "for ((i=0; i<2; i++)); do echo i; done".
			// gosh: its parser stops asking for continuation lines after the
			// first one, so four separate entries are recorded. Note that
			// formatHistoryEntries already produces bash's single entry for
			// these lines; the divergence is in the interactive reader's
			// "is this command complete?" check.
			reason: "gosh does not continue an unterminated arithmetic for header",
			lines:  []string{"for ((i=0", "i<2; i++)); do", "echo i", "done"},
		},
		{
			name: "unterminated parameter expansion",
			// bash: an unterminated ${...} continues onto the next line and
			// the merged entry is "x=${a; b}".
			// gosh: records "x=${a" and "b}" as two entries. Here too
			// formatHistoryEntries agrees with bash already.
			reason: "gosh does not continue an unterminated parameter expansion",
			lines:  []string{"x=${a", "b}"},
		},
		{
			name: "unterminated conditional command",
			// bash: `[[ a` is a syntax error on its own line rather than an
			// incomplete command, so bash records two entries, "[[ a" and
			// "&& b ]]".
			// gosh: treats the line as incomplete and merges both lines into
			// "[[ a\n&& b ]]" (which is what formatHistoryEntries returns).
			reason: "gosh continues an invalid conditional command instead of failing it",
			lines:  []string{"[[ a", "&& b ]]"},
		},
		{
			name: "heredoc trailing newline",
			// bash: a heredoc entry keeps its final newline, so its history
			// listing ends with an empty line before the next entry, and the
			// history file holds "cat <<EOF\nline1\nline2\nEOF\n\n".
			// gosh: history.Add trims the trailing newline, so the blank line
			// is missing. formatHistoryEntries keeps the newline and thus
			// already matches bash.
			reason: "gosh trims the trailing newline of a heredoc history entry",
			lines:  []string{"cat <<EOF", "line1", "line2", "EOF"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Skipf("known divergence: %s", tc.reason)
			script := append(append([]string{}, tc.lines...), "history", "exit")
			bashFile := filepath.Join(t.TempDir(), "bash-history")
			goshFile := filepath.Join(t.TempDir(), "gosh-history")
			want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, bashFile))
			got := runInteractiveHistory(t, testGoshBinary, "gosh", []string{"-i", "--norc"}, script, historyEnv(t, goshFile))
			if got.stdout != want.stdout {
				t.Fatalf("history output mismatch:\nbash: %q\ngosh: %q", want.stdout, got.stdout)
			}
		})
	}
}

// TestDifferentialHistoryCmdhistUntestable documents multi-line cases that
// cannot be compared by piping input into an interactive shell.
func TestDifferentialHistoryCmdhistUntestable(t *testing.T) {
	t.Skip(`select reads its menu answer from stdin, which is the same pipe that
feeds the shell its commands, so the remaining script lines are swallowed;
and a literal tab in a <<-EOF body triggers completion in both readline
implementations before it ever reaches the parser. Both need a pty harness.`)
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

// TestDifferentialCompletionBuiltins pins the compgen/complete/compopt
// behavior that gosh already shares with bash. The upcoming refactor of
// programmable.go has to keep every one of these cases identical.
func TestDifferentialCompletionBuiltins(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	env := testEnv(t)

	cases := []struct {
		name   string
		script string
	}{
		{"compgen word list prefix", `compgen -W "aa ab bb" a`},
		{"compgen word list all", `compgen -W "aa ab bb"`},
		{"compgen word list exact", `compgen -W "aa ab bb" aa`},
		{"compgen prefix suffix", `compgen -W "aa ab" -P "<" -S ">" a`},
		{"compgen no options", `compgen`},
		{"compgen action function", `f() { :; }; g() { :; }; compgen -A function`},
		{"compgen action function prefix", `f() { :; }; ff() { :; }; g() { :; }; compgen -A function f`},
		{"compgen action variable", `compgen -A variable PATH`},
		{"compgen action variable prefix", `compgen -A variable HOM`},
		{"compgen action builtin prefix", `compgen -A builtin ec`},
		{"compgen action builtin prefix pwd", `compgen -A builtin pw`},
		{"complete then print", `complete -W 'one two' greet; complete -p greet`},
		{"complete option nospace", `complete -o nospace -W 'one two' greet; complete -p greet`},
		{"complete prefix suffix", `complete -P '[' -S ']' -W one greet; complete -p greet`},
		{"complete function spec", `complete -F _greet greet; complete -p greet`},
		{"complete replaces spec", `complete -W one greet; complete -W two greet; complete -p greet`},
		{"complete print several", `complete -W one alpha; complete -W two beta; complete -p | sort`},
		{"complete print unknown command", `complete -p nosuch`},
		{"complete removes spec", `complete -W one greet; complete -r greet; complete -p greet`},
		{"compopt outside completion", `compopt -o nospace`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSameScript(t, tc.script, env, dir)
		})
	}
}

// TestDifferentialCompletionKnownDivergences records where gosh's completion
// builtins still differ from bash. Every subtest is skipped on purpose: the
// comment states the bash behavior gosh should eventually grow, and dropping
// the t.Skip line turns the case into a live differential test.
func TestDifferentialCompletionKnownDivergences(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	env := testEnv(t)

	cases := []struct {
		name   string
		reason string
		script string
	}{
		{
			name: "compgen double dash separator",
			// bash: `compgen -W "aa ab bb" -- a` prints "aa\nab"; the `--`
			// ends option parsing so the word can start with a dash.
			// gosh: rejects it with `compgen: invalid option "--"` (exit 1).
			reason: "gosh does not accept the -- option terminator",
			script: `compgen -W "aa ab bb" -- a`,
		},
		{
			name: "compgen no match exit status",
			// bash: compgen exits 1 when nothing matched, which is what
			// completion scripts test with `if compgen ...; then`.
			// gosh: always exits 0 when no error occurred.
			reason: "gosh returns 0 instead of 1 when no candidate matches",
			script: `compgen -W "aa ab bb" zz; echo "rc=$?"`,
		},
		{
			name: "compgen action no match exit status",
			// bash: exits 1 for an action that produced nothing.
			// gosh: exits 0.
			reason: "gosh returns 0 instead of 1 for an empty action result",
			script: `compgen -A function; echo "rc=$?"`,
		},
		{
			name: "complete without arguments",
			// bash: prints every registered specification and exits 0.
			// gosh: fails with `complete: missing command name` (exit 1).
			reason: "gosh requires a command name instead of listing all specs",
			script: `complete -W one greet; complete`,
		},
		{
			name: "compgen action variable sees shell variables",
			// bash: lists shell variables, including ones assigned by the
			// script, so `myvar=1; compgen -A variable myvar` prints myvar.
			// gosh: only walks the inherited environment, so variables
			// created (even exported) during the run are invisible.
			reason: "gosh only completes variables from the initial environment",
			script: `myvar=1; export myvar2=2; compgen -A variable myvar`,
		},
		{
			name: "compgen shorthand action flags",
			// bash: -b/-v/-c/-f/-d are shorthands for -A builtin/variable/
			// command/file/directory, e.g. `compgen -b ec` prints echo.
			// gosh: rejects them as invalid options.
			reason: "gosh only implements the -A form of the action flags",
			script: `compgen -b ec; compgen -v PAT`,
		},
		{
			name: "compgen extra word arguments",
			// bash: ignores everything after the first word, so
			// `compgen -W "aa ab" a b` still completes "a".
			// gosh: fails with `compgen: too many word arguments`.
			reason: "gosh rejects extra word arguments instead of ignoring them",
			script: `compgen -W "aa ab" a b`,
		},
		{
			name: "invalid option exit status",
			// bash: an unknown option is a usage error, printed with the
			// builtin's usage line and exiting 2.
			// gosh: prints its own message and exits 1.
			reason: "gosh exits 1 rather than 2 for a usage error",
			script: `compgen -Z a; echo "rc=$?"; complete -Z greet; echo "rc=$?"`,
		},
		{
			name: "complete -r for unknown command",
			// bash: removing a specification that does not exist is silent
			// and exits 0.
			// gosh: fails with `complete: no completion specification`.
			reason: "gosh fails instead of ignoring a missing specification",
			script: `complete -r nosuch; echo "rc=$?"`,
		},
		{
			name: "complete -p option ordering",
			// bash: prints -o options in its own canonical order and puts
			// -A before -W, e.g.
			//   complete -o filenames -o nospace -W 'one' greet
			//   complete -A function -W 'one' greet
			// gosh: prints them in the order its own struct declares.
			reason: "gosh prints complete -p fields in a different order",
			script: `complete -o nospace -o filenames -W one greet; complete -p greet
complete -A function -W one other; complete -p other`,
		},
		{
			name: "compgen action builtin list",
			// bash: the builtin list is sorted in C order (".", ":", "["
			// come first) and contains only real bash builtins.
			// gosh: emits defaultCommandNames unsorted, with ":", "." and
			// "[" last, and includes non-bash entries such as newgrp.
			reason: "gosh's builtin list differs in order and membership",
			script: `compgen -A builtin`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Skipf("known divergence: %s", tc.reason)
			assertSameScript(t, tc.script, env, dir)
		})
	}
}

// TestDifferentialHistoryBuiltin covers the history builtin in script mode,
// where bash keeps history disabled unless it is switched on explicitly.
func TestDifferentialHistoryBuiltin(t *testing.T) {
	requireBash(t)
	env := testEnv(t)

	cases := []struct {
		name   string
		script string
	}{
		{"empty history", `history`},
		{"clear", `history -c; echo "rc=$?"`},
		{"clear then list", `history -c; history`},
		{"delete out of range", `history -d 1; echo "rc=$?"`},
		{"read file then list", `printf 'a\nb\n' > hist.in; history -r hist.in; history`},
		{"read then delete", `printf 'a\nb\nc\n' > hist.in; history -r hist.in; history -d 2; history`},
		{"read then clear", `printf 'a\nb\n' > hist.in; history -r hist.in; history -c; history`},
		{"write file", `printf 'a\nb\n' > hist.in; history -r hist.in; history -w hist.out; cat hist.out`},
		// Commands run from -c scripts are never entered into history, in
		// either shell, so the listing stays empty here.
		{"script commands are not recorded", `echo one; echo two; history`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each shell gets its own directory so the temporary history
			// files written by the script cannot leak between them.
			bashDir := t.TempDir()
			goshDir := t.TempDir()
			want := runDifferentialShell(t, "bash", []string{"--noprofile", "--norc", "-c", tc.script}, nil, env, bashDir)
			got := runDifferentialShell(t, testGoshBinary, []string{"gosh", "-c", tc.script}, nil, env, goshDir)
			if got.code != want.code {
				t.Fatalf("exit code mismatch: bash=%d gosh=%d\nbash stdout: %q\ngosh stdout: %q\ngosh stderr: %q",
					want.code, got.code, want.stdout, got.stdout, got.stderr)
			}
			if got.stdout != want.stdout {
				t.Fatalf("stdout mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", want.stdout, got.stdout, got.stderr)
			}
		})
	}
}

// TestDifferentialHistoryBuiltinKnownDivergences records the history builtin
// features gosh does not implement yet. All subtests are skipped; the
// comments describe what bash does.
func TestDifferentialHistoryBuiltinKnownDivergences(t *testing.T) {
	requireBash(t)
	env := testEnv(t)

	cases := []struct {
		name   string
		reason string
		script string
	}{
		{
			name: "set -o history",
			// bash: history is a POSIX shell option; `set -o history` and
			// `set +o history` toggle it and succeed even non-interactively.
			// gosh: prints `set: invalid option: "history"` on stderr.
			reason: "gosh does not implement the history shell option",
			script: `set -o history; echo one; history`,
		},
		{
			name: "history -s",
			// bash: `history -s "echo added"` appends the argument to the
			// history list without executing it; the listing then shows it.
			// gosh: fails with `history: unsupported argument "-s"`.
			reason: "gosh does not implement history -s",
			script: `history -s "echo added"; history`,
		},
		{
			name: "history count argument",
			// bash: `history 5` lists at most the last five entries.
			// gosh: fails with `history: unsupported argument "5"`.
			reason: "gosh does not implement the numeric history argument",
			script: `printf 'a\nb\nc\n' > hist.in; history -r hist.in; history 2`,
		},
		{
			name: "history -p",
			// bash: `history -p foo` performs history expansion on its
			// arguments and prints the result ("foo").
			// gosh: fails with `history: unsupported argument "-p"`.
			reason: "gosh does not implement history -p",
			script: `history -p foo`,
		},
		{
			name: "history -a and -n",
			// bash: -a appends the new entries to the history file and -n
			// reads the entries not yet read from it.
			// gosh: both are rejected as unsupported arguments.
			reason: "gosh does not implement history -a/-n",
			script: `history -a hist.out; history -n hist.out`,
		},
		{
			name: "invalid option exit status",
			// bash: prints its usage line and exits 2.
			// gosh: prints its own message and exits 1.
			reason: "gosh exits 1 rather than 2 for a usage error",
			script: `history -z; echo "rc=$?"`,
		},
		{
			name: "history -r on an empty file",
			// bash: reading /dev/null yields no entries and exits 1.
			// gosh: exits 0.
			reason: "gosh returns 0 when history -r reads nothing",
			script: `history -r /dev/null; echo "rc=$?"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Skipf("known divergence: %s", tc.reason)
			bashDir := t.TempDir()
			goshDir := t.TempDir()
			want := runDifferentialShell(t, "bash", []string{"--noprofile", "--norc", "-c", tc.script}, nil, env, bashDir)
			got := runDifferentialShell(t, testGoshBinary, []string{"gosh", "-c", tc.script}, nil, env, goshDir)
			if got.code != want.code || got.stdout != want.stdout {
				t.Fatalf("mismatch:\nbash: rc=%d stdout=%q\ngosh: rc=%d stdout=%q stderr=%q",
					want.code, want.stdout, got.code, got.stdout, got.stderr)
			}
		})
	}
}
