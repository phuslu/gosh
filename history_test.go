package gosh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func TestHistorySizeParsing(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"500", 500},
		{"0", 0},
		{"-1", -1},
		{"-100", -1},
		{"", -1},
		{"abc", -1},
		{"12x", -1},
		{" 7 ", 7},
	}
	for _, tc := range cases {
		if got := parseHistorySize(tc.val); got != tc.want {
			t.Errorf("parseHistorySize(%q) = %d, want %d", tc.val, got, tc.want)
		}
	}
	if got := resolveHistoryLimit(); got != defaultHistoryLimit {
		t.Errorf("resolveHistoryLimit() = %d, want %d", got, defaultHistoryLimit)
	}
}

func TestHistoryFileLimitParsing(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"500", 500},
		{"0", 0},
		{"-1", -1},
		{"", -1},
		{"abc", -1},
		{"3.5", -1},
	}
	for _, tc := range cases {
		if got := parseHistoryFileLimit(tc.val); got != tc.want {
			t.Errorf("parseHistoryFileLimit(%q) = %d, want %d", tc.val, got, tc.want)
		}
	}
}

func newTestRunner(t *testing.T, env []string) *interp.Runner {
	t.Helper()
	runner, err := interp.New(
		interp.StdIO(strings.NewReader(""), io.Discard, io.Discard),
		interp.Env(expand.ListEnviron(env...)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestShellHistoryFileResolution(t *testing.T) {
	t.Run("explicit file", func(t *testing.T) {
		runner := newTestRunner(t, []string{"HISTFILE=/tmp/explicit", "HOME=/home/a"})
		if got, want := resolveShellHistoryFile(runner), "/tmp/explicit"; got != want {
			t.Fatalf("resolveShellHistoryFile = %q, want %q", got, want)
		}
	})
	t.Run("dev null", func(t *testing.T) {
		runner := newTestRunner(t, []string{"HISTFILE=/dev/null", "HOME=/home/a"})
		if got := resolveShellHistoryFile(runner); got != "" {
			t.Fatalf("resolveShellHistoryFile = %q, want empty", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		runner := newTestRunner(t, []string{"HISTFILE=", "HOME=/home/a"})
		if got := resolveShellHistoryFile(runner); got != "" {
			t.Fatalf("resolveShellHistoryFile = %q, want empty", got)
		}
	})
	t.Run("default", func(t *testing.T) {
		runner := newTestRunner(t, []string{"HOME=/home/a"})
		if got, want := resolveShellHistoryFile(runner), filepath.Join("/home/a", ".gosh_history"); got != want {
			t.Fatalf("resolveShellHistoryFile = %q, want %q", got, want)
		}
	})
	t.Run("no home", func(t *testing.T) {
		runner := newTestRunner(t, []string{"PATH=/bin"})
		if got := resolveShellHistoryFile(runner); got != "" {
			t.Fatalf("resolveShellHistoryFile = %q, want empty", got)
		}
	})
}

func TestShellHistoryLimitsFromRunner(t *testing.T) {
	runner := newTestRunner(t, []string{"HISTSIZE=7", "HISTFILESIZE=9"})
	if got := resolveShellHistoryLimit(runner); got != 7 {
		t.Fatalf("resolveShellHistoryLimit = %d, want 7", got)
	}
	if got := resolveShellHistoryFileLimit(runner); got != 9 {
		t.Fatalf("resolveShellHistoryFileLimit = %d, want 9", got)
	}

	runner = newTestRunner(t, []string{"HISTSIZE=-2", "HISTFILESIZE=abc"})
	if got := resolveShellHistoryLimit(runner); got != -1 {
		t.Fatalf("resolveShellHistoryLimit(-2) = %d, want -1", got)
	}
	if got := resolveShellHistoryFileLimit(runner); got != -1 {
		t.Fatalf("resolveShellHistoryFileLimit(abc) = %d, want -1", got)
	}

	runner = newTestRunner(t, []string{})
	if got := resolveShellHistoryLimit(runner); got != defaultHistoryLimit {
		t.Fatalf("unset HISTSIZE limit = %d, want %d", got, defaultHistoryLimit)
	}
	if got := resolveShellHistoryFileLimit(runner); got != -1 {
		t.Fatalf("unset HISTFILESIZE limit = %d, want -1", got)
	}
}

func TestHistoryInMemoryLimitSemantics(t *testing.T) {
	t.Run("unlimited", func(t *testing.T) {
		h := &history{cfg: historyConfig{inMemoryLimit: -1}}
		for i := 0; i < 5; i++ {
			h.append(string(rune('a' + i)))
		}
		if got, want := h.Entries(), []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("entries = %#v, want %#v", got, want)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		h := &history{cfg: historyConfig{inMemoryLimit: 0}}
		if h.Add("echo x") {
			t.Fatal("disabled history accepted an entry")
		}
		h.append("echo y")
		if got := h.Entries(); len(got) != 0 {
			t.Fatalf("disabled history entries = %#v, want empty", got)
		}
	})
	t.Run("limited", func(t *testing.T) {
		h := &history{cfg: historyConfig{inMemoryLimit: 3}}
		for _, line := range []string{"a", "b", "c", "d"} {
			h.append(line)
		}
		if got, want := h.Entries(), []string{"b", "c", "d"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("entries = %#v, want %#v", got, want)
		}
	})
}

func TestHistoryFileTruncation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "history")
	content := strings.Join([]string{
		"echo 1", "echo 2", "echo 3", "echo 4", "echo 5",
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}

	if err := truncateHistoryFile(file, 2); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "echo 4\necho 5\n"; got != want {
		t.Fatalf("truncated file = %q, want %q", got, want)
	}

	if err := truncateHistoryFile(file, 0); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("HISTFILESIZE=0 file = %q, want empty", data)
	}

	if err := truncateHistoryFile(file, -1); err != nil {
		t.Fatal(err)
	}
	if err := truncateHistoryFile(filepath.Join(t.TempDir(), "missing"), 2); err != nil {
		t.Fatalf("missing file truncation should be a no-op: %v", err)
	}
}

func TestHistoryHistappendAppend(t *testing.T) {
	file := filepath.Join(t.TempDir(), "history")
	h := &history{
		cfg:         historyConfig{inMemoryLimit: 10, file: file},
		appendOnAdd: func() bool { return true },
	}
	if !h.Add("echo one") || !h.Add("echo two") {
		t.Fatal("history did not accept entries")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "echo one\necho two\n"; got != want {
		t.Fatalf("appended history = %q, want %q", got, want)
	}
}

func TestFormatHistoryEntries(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"single", []string{"echo one"}, []string{"echo one"}},
		{"if", []string{"if true; then", "echo hi", "fi"}, []string{"if true; then echo hi; fi"}},
		{"pipeline", []string{"echo a |", "cat"}, []string{"echo a | cat"}},
		{"pipeline trailing space", []string{"echo a | ", "cat"}, []string{"echo a |  cat"}},
		{"for", []string{"for x in 1; do", "echo x", "done"}, []string{"for x in 1; do echo x; done"}},
		{"for in", []string{"for i", "in a b; do", "echo $i", "done"}, []string{"for i in a b; do echo $i; done"}},
		{"for do", []string{"for x", "do", "echo x", "done"}, []string{"for x;do echo x; done"}},
		{"for in no do", []string{"for x in", "a b"}, []string{"for x in a b"}},
		{"brace", []string{"{", "echo a", "echo b", "}"}, []string{"{ echo a; echo b; }"}},
		{"brace blank", []string{"{", "", "echo a", "}"}, []string{"{ echo a; }"}},
		{"brace blank after word", []string{"{ echo a", "", "}"}, []string{"{ echo a;  }"}},
		{"while", []string{"while true; do", "echo x", "break", "done"}, []string{"while true; do echo x; break; done"}},
		{"if elif else", []string{"if true; then", "echo a", "elif false; then", "echo b", "else", "echo c", "fi"},
			[]string{"if true; then echo a; elif false; then echo b; else echo c; fi"}},
		{"case", []string{"case $x in a)", "echo x;", ";;", "esac"}, []string{"case $x in a) echo x; ;; esac"}},
		{"case comment", []string{"case x in", "a) echo a", "# c", ";;", "esac"}, []string{"case x in a) echo a\n;; esac"}},
		{"function braces", []string{"f() {", "echo hi", "}"}, []string{"f() { echo hi; }"}},
		{"function name", []string{"function f", "echo hi"}, []string{"function f echo hi"}},
		{"subshell", []string{"(echo a", "echo b)"}, []string{"(echo a; echo b)"}},
		{"compound assignment", []string{"arr=(", "one two", "three", ")"}, []string{"arr=( one two three )"}},
		{"arithmetic for", []string{"for ((i=0", "i<2; i++)); do", "echo i", "done"}, []string{"for ((i=0; i<2; i++)); do echo i; done"}},
		{"select", []string{"select x in a b; do", "echo x", "done"}, []string{"select x in a b; do echo x; done"}},
		{"backslash continuation", []string{"echo a \\", "echo b"}, []string{"echo a echo b"}},
		{"single quote", []string{"echo 'a", "b'"}, []string{"echo 'a\nb'"}},
		{"double quote", []string{"echo \"c", "d\""}, []string{"echo \"c\nd\""}},
		{"blank inside quotes", []string{"echo \"a", "", "b\""}, []string{"echo \"a\n\nb\""}},
		{"dollar paren", []string{"x=$(echo a", "echo b)"}, []string{"x=$(echo a\necho b)"}},
		{"dollar brace", []string{"x=${a", "b}"}, []string{"x=${a; b}"}},
		{"arithmetic expansion", []string{"echo $((1 +", "2))"}, []string{"echo $((1 +\n2))"}},
		{"and", []string{"echo a &&", "echo b"}, []string{"echo a && echo b"}},
		{"or", []string{"echo a ||", "echo b"}, []string{"echo a || echo b"}},
		{"pipe amp quirk", []string{"echo a |&", "cat"}, []string{"echo a |&; cat"}},
		{"comment dropped", []string{"if true; then", "# comment", "echo hi", "fi"}, []string{"if true; then\necho hi; fi"}},
		{"two comments", []string{"if true; then", "# c1", "# c2", "echo hi", "fi"}, []string{"if true; then\necho hi; fi"}},
		{"trailing comment", []string{"if true; then", "echo a # trailing", "fi"}, []string{"if true; then echo a # trailing\nfi"}},
		{"first line trailing comment", []string{"if true; then # c", "echo hi", "fi"}, []string{"if true; then # c\necho hi; fi"}},
		{"heredoc", []string{"cat <<EOF", "line1", "line2", "EOF"}, []string{"cat <<EOF\nline1\nline2\nEOF\n"}},
		{"heredoc in compound", []string{"{ cat <<EOF", "x", "EOF", "echo done", "}"}, []string{"{ cat <<EOF\nx\nEOF\n echo done; }"}},
		{"heredoc tab", []string{"cat <<-EOF", "\tline1", "\tEOF"}, []string{"cat <<-EOF\n\tline1\n\tEOF\n"}},
		{"cond command", []string{"[[ a", "&& b ]]"}, []string{"[[ a\n&& b ]]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatHistoryEntries(tc.lines, true, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("formatHistoryEntries(%#v) = %#v, want %#v", tc.lines, got, tc.want)
			}
		})
	}
}

func TestFormatHistoryEntriesCmdhistOff(t *testing.T) {
	lines := []string{"if true; then", "echo hi", "fi"}
	got := formatHistoryEntries(lines, false, false)
	if want := []string{"if true; then", "echo hi", "fi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cmdhist off = %#v, want %#v", got, want)
	}
}

func TestFormatHistoryEntriesLithist(t *testing.T) {
	lines := []string{"if true; then", "echo hi", "fi"}
	got := formatHistoryEntries(lines, true, true)
	if want := []string{"if true; then\necho hi\nfi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lithist on = %#v, want %#v", got, want)
	}
	lines = []string{"echo 'a", "b'"}
	got = formatHistoryEntries(lines, true, true)
	if want := []string{"echo 'a\nb'"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lithist quoted = %#v, want %#v", got, want)
	}
}

// TestInteractiveHistoryFileResolution drives the real gosh binary through
// forced-interactive sessions to exercise the HISTFILE resolution rules:
// explicit file, /dev/null, the $HOME/.gosh_history default, and no HOME.
func TestInteractiveHistoryFileResolution(t *testing.T) {
	run := func(env []string, stdin string) error {
		cmd := exec.Command(testGoshBinary, "gosh", "-i", "--norc")
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%v: %s", err, stderr.String())
		}
		return nil
	}
	baseEnv := []string{"PATH=" + os.Getenv("PATH")}

	t.Run("default home file", func(t *testing.T) {
		home := t.TempDir()
		if err := run(append(baseEnv, "HOME="+home), "echo hello\nexit\n"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(home, ".gosh_history"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), "echo hello\nexit\n"; got != want {
			t.Fatalf("default history = %q, want %q", got, want)
		}
	})

	t.Run("explicit histfile", func(t *testing.T) {
		home := t.TempDir()
		file := filepath.Join(t.TempDir(), "explicit-history")
		if err := run(append(baseEnv, "HOME="+home, "HISTFILE="+file), "echo hi\nexit\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("explicit HISTFILE was not written: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".gosh_history")); !os.IsNotExist(err) {
			t.Fatalf("default history file created despite explicit HISTFILE: %v", err)
		}
	})

	t.Run("dev null", func(t *testing.T) {
		home := t.TempDir()
		if err := run(append(baseEnv, "HOME="+home, "HISTFILE="+os.DevNull), "echo hi\nexit\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(home, ".gosh_history")); !os.IsNotExist(err) {
			t.Fatalf("default history file created for HISTFILE=/dev/null: %v", err)
		}
	})

	t.Run("no home", func(t *testing.T) {
		if err := run(baseEnv, "echo hi\nexit\n"); err != nil {
			t.Fatal(err)
		}
	})
}
