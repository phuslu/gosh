package gosh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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

// historyEntryCase is a multi-line command plus the entries gosh's
// formatHistoryEntries merges it into with cmdhist on and lithist off.
//
// Every golden below has been checked against GNU bash 5.3: the same lines
// are typed into `bash --noprofile --norc -i` and the entries its history
// builtin prints are compared with the golden (see
// TestFormatHistoryEntriesMatchBash). bashSkip marks the few cases where
// that comparison is not possible or where bash is known to disagree, and
// its text says what bash actually does.
type historyEntryCase struct {
	name     string
	lines    []string
	want     []string
	bashSkip string
}

var historyEntryCases = []historyEntryCase{
	{name: "single", lines: []string{"echo one"}, want: []string{"echo one"}},
	{name: "if", lines: []string{"if true; then", "echo hi", "fi"}, want: []string{"if true; then echo hi; fi"}},
	{name: "pipeline", lines: []string{"echo a |", "cat"}, want: []string{"echo a | cat"}},
	{name: "pipeline trailing space", lines: []string{"echo a | ", "cat"}, want: []string{"echo a |  cat"}},
	{name: "for", lines: []string{"for x in 1; do", "echo x", "done"}, want: []string{"for x in 1; do echo x; done"}},
	{name: "for in", lines: []string{"for i", "in a b; do", "echo $i", "done"}, want: []string{"for i in a b; do echo $i; done"}},
	{name: "for do", lines: []string{"for x", "do", "echo x", "done"}, want: []string{"for x;do echo x; done"}},
	{name: "for in no do", lines: []string{"for x in", "a b"}, want: []string{"for x in a b"}},
	{name: "brace", lines: []string{"{", "echo a", "echo b", "}"}, want: []string{"{ echo a; echo b; }"}},
	{name: "brace blank", lines: []string{"{", "", "echo a", "}"}, want: []string{"{ echo a; }"}},
	{name: "brace blank after word", lines: []string{"{ echo a", "", "}"}, want: []string{"{ echo a;  }"}},
	{name: "while", lines: []string{"while true; do", "echo x", "break", "done"}, want: []string{"while true; do echo x; break; done"}},
	{name: "if elif else", lines: []string{"if true; then", "echo a", "elif false; then", "echo b", "else", "echo c", "fi"},
		want: []string{"if true; then echo a; elif false; then echo b; else echo c; fi"}},
	{name: "case", lines: []string{"case $x in a)", "echo x;", ";;", "esac"}, want: []string{"case $x in a) echo x; ;; esac"}},
	{name: "case comment", lines: []string{"case x in", "a) echo a", "# c", ";;", "esac"}, want: []string{"case x in a) echo a\n;; esac"}},
	{name: "function braces", lines: []string{"f() {", "echo hi", "}"}, want: []string{"f() { echo hi; }"}},
	{name: "function name", lines: []string{"function f", "echo hi"}, want: []string{"function f echo hi"}},
	{name: "subshell", lines: []string{"(echo a", "echo b)"}, want: []string{"(echo a; echo b)"}},
	{name: "compound assignment", lines: []string{"arr=(", "one two", "three", ")"}, want: []string{"arr=( one two three )"}},
	// bash merges these lines the same way even though its parser rejects
	// the resulting arithmetic header; gosh's interactive reader does not
	// continue the command at all (see
	// TestDifferentialHistoryCmdhistKnownDivergences).
	{name: "arithmetic for", lines: []string{"for ((i=0", "i<2; i++)); do", "echo i", "done"}, want: []string{"for ((i=0; i<2; i++)); do echo i; done"}},
	{name: "select", lines: []string{"select x in a b; do", "echo x", "done"}, want: []string{"select x in a b; do echo x; done"},
		bashSkip: "select reads its answer from the same pipe that feeds the shell, so the rest of the script is swallowed"},
	{name: "backslash continuation", lines: []string{"echo a \\", "echo b"}, want: []string{"echo a echo b"}},
	{name: "single quote", lines: []string{"echo 'a", "b'"}, want: []string{"echo 'a\nb'"}},
	{name: "double quote", lines: []string{"echo \"c", "d\""}, want: []string{"echo \"c\nd\""}},
	{name: "blank inside quotes", lines: []string{"echo \"a", "", "b\""}, want: []string{"echo \"a\n\nb\""}},
	{name: "dollar paren", lines: []string{"x=$(echo a", "echo b)"}, want: []string{"x=$(echo a\necho b)"}},
	// As with "arithmetic for", the golden matches bash but gosh's
	// interactive reader does not ask for the continuation line.
	{name: "dollar brace", lines: []string{"x=${a", "b}"}, want: []string{"x=${a; b}"}},
	{name: "arithmetic expansion", lines: []string{"echo $((1 +", "2))"}, want: []string{"echo $((1 +\n2))"}},
	{name: "and", lines: []string{"echo a &&", "echo b"}, want: []string{"echo a && echo b"}},
	{name: "or", lines: []string{"echo a ||", "echo b"}, want: []string{"echo a || echo b"}},
	{name: "pipe amp quirk", lines: []string{"echo a |&", "cat"}, want: []string{"echo a |&; cat"}},
	{name: "comment dropped", lines: []string{"if true; then", "# comment", "echo hi", "fi"}, want: []string{"if true; then\necho hi; fi"}},
	{name: "two comments", lines: []string{"if true; then", "# c1", "# c2", "echo hi", "fi"}, want: []string{"if true; then\necho hi; fi"}},
	{name: "trailing comment", lines: []string{"if true; then", "echo a # trailing", "fi"}, want: []string{"if true; then echo a # trailing\nfi"}},
	{name: "first line trailing comment", lines: []string{"if true; then # c", "echo hi", "fi"}, want: []string{"if true; then # c\necho hi; fi"}},
	// The trailing newline is deliberate: bash keeps it, so its history
	// listing shows an empty line after the delimiter.
	{name: "heredoc", lines: []string{"cat <<EOF", "line1", "line2", "EOF"}, want: []string{"cat <<EOF\nline1\nline2\nEOF\n"}},
	{name: "heredoc in compound", lines: []string{"{ cat <<EOF", "x", "EOF", "echo done", "}"}, want: []string{"{ cat <<EOF\nx\nEOF\n echo done; }"}},
	{name: "heredoc tab", lines: []string{"cat <<-EOF", "\tline1", "\tEOF"}, want: []string{"cat <<-EOF\n\tline1\n\tEOF\n"},
		bashSkip: "a literal tab triggers completion in readline before it reaches the parser, so bash never sees this input over a pipe"},
	// Divergence: bash treats `[[ a` as a syntax error rather than an
	// incomplete command and records two entries, "[[ a" and "&& b ]]",
	// while gosh merges them.
	{name: "cond command", lines: []string{"[[ a", "&& b ]]"}, want: []string{"[[ a\n&& b ]]"},
		bashSkip: `bash does not continue an invalid conditional command; it records "[[ a" and "&& b ]]" separately`},
}

func TestFormatHistoryEntries(t *testing.T) {
	for _, tc := range historyEntryCases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatHistoryEntries(tc.lines, true, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("formatHistoryEntries(%#v) = %#v, want %#v", tc.lines, got, tc.want)
			}
		})
	}
}

// historyListingEntry matches the "%5d  " prefix the history builtin prints
// in front of each entry; everything else is a continuation line.
var historyListingEntry = regexp.MustCompile(`^ *[0-9]+  `)

// parseHistoryListing splits the output of the history builtin back into
// entries, re-joining the continuation lines of multi-line entries.
func parseHistoryListing(t *testing.T, listing string) []string {
	t.Helper()
	var entries []string
	for _, line := range strings.Split(strings.TrimSuffix(listing, "\n"), "\n") {
		if loc := historyListingEntry.FindStringIndex(line); loc != nil {
			entries = append(entries, line[loc[1]:])
			continue
		}
		if len(entries) == 0 {
			t.Fatalf("history listing starts with a continuation line: %q", listing)
		}
		entries[len(entries)-1] += "\n" + line
	}
	return entries
}

// bashHistoryEntries types lines into an interactive bash and returns the
// history entries bash merged them into, as its history builtin prints them.
func bashHistoryEntries(t *testing.T, lines []string) []string {
	t.Helper()
	const marker = "gosh-history-marker"
	// The marker separates the output of the commands themselves from the
	// history listing; the marker echo and the history call are the last two
	// entries and are dropped again.
	script := append(append([]string{}, lines...), "echo "+marker, "history", "exit")
	file := filepath.Join(t.TempDir(), "bash-history")
	res := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, file))
	_, listing, ok := strings.Cut(res.stdout, marker+"\n")
	if !ok {
		t.Fatalf("bash did not echo the marker\nstdout: %q\nstderr: %q", res.stdout, res.stderr)
	}
	entries := parseHistoryListing(t, listing)
	if len(entries) < 2 {
		t.Fatalf("bash printed %d history entries, want at least 2\nlisting: %q", len(entries), listing)
	}
	return entries[:len(entries)-2]
}

// TestFormatHistoryEntriesMatchBash validates the goldens above against a
// real bash instead of against gosh's own implementation, so the refactor of
// history_format.go has an external reference to keep matching.
func TestFormatHistoryEntriesMatchBash(t *testing.T) {
	requireBash(t)
	for _, tc := range historyEntryCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.bashSkip != "" {
				t.Skipf("not compared against bash: %s", tc.bashSkip)
			}
			want := bashHistoryEntries(t, tc.lines)
			if !reflect.DeepEqual(tc.want, want) {
				t.Fatalf("golden entries for %#v are %#v, but bash records %#v", tc.lines, tc.want, want)
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
