package gosh

import (
	"path"
	"strings"
	"testing"
	"time"
)

// newPromptEscapeState returns a prompt state with everything pinned, so the
// escape expansions below are reproducible: user alice, home /home/alice,
// working directory /home/alice/project, host host.example, and a fixed
// timestamp of Friday 2026-05-15 13:08:07.
func newPromptEscapeState() *promptState {
	return &promptState{
		vars:      map[string]string{"USER": "alice", "HOME": "/home/alice"},
		dir:       "/home/alice/project",
		host:      "host.example",
		shortHost: "host",
		seq:       3,
		now:       time.Date(2026, 5, 15, 13, 8, 7, 0, time.UTC),
	}
}

// TestPromptEscapes pins the PS1 escapes gosh already expands like bash.
//
// The expected values come from the Bash Reference Manual ("Controlling the
// Prompt") and were confirmed against GNU bash 5.3 with its prompt-expansion
// operator, `PS1='<escape>'; printf '%s\n' "${PS1@P}"`.
func TestPromptEscapes(t *testing.T) {
	state := newPromptEscapeState()
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"user", `\u`, "alice"},
		{"short host", `\h`, "host"},
		{"full host", `\H`, "host.example"},
		{"working directory", `\w`, "~/project"},
		{"basename of working directory", `\W`, "project"},
		{"time 24 hour", `\t`, "13:08:07"},
		{"time 12 hour", `\T`, "01:08:07"},
		{"time hours and minutes", `\A`, "13:08"},
		{"date", `\d`, "Fri May 15"},
		{"jobs", `\j`, "0"},
		{"command number", `\#`, "3"},
		{"prompt symbol", `\$`, state.promptSymbol()},
		{"backslash", `\\`, "\\"},
		{"trailing backslash", `\`, "\\"},
		{"bell", `\a`, "\a"},
		{"escape", `\e`, "\x1b"},
		{"newline", `\n`, "\n"},
		{"carriage return", `\r`, "\r"},
		{"octal escape", `\033[1mX`, "\x1b[1mX"},
		{"octal escape in non printing", `\[\033[0m\]$`, "\x1b[0m$"},
		{"octal escape stops after three digits", `\1234`, "S4"},
		{"octal escape truncated to a byte", `\541`, "a"},
		{"octal escape at the end may be short", `\41`, "!"},
		{"short octal escape keeps its backslash", `\33x\1b`, "\\33x\\1b"},
		{"octal nul is dropped", `a\000b\400c\0`, "abc"},
		// \[ and \] delimit non-printing sequences; the characters between
		// them are kept, only the markers themselves disappear.
		{"non printing markers", `a\[b\]c`, "abc"},
		{"non printing escape", `\[\e[1m\]x\[\e[0m\]`, "\x1b[1mx\x1b[0m"},
		{"strftime date", `\D{%Y-%m-%d}`, "2026-05-15"},
		{"strftime iso", `\D{%F %T}`, "2026-05-15 13:08:07"},
		{"strftime clock", `\D{%H:%M}`, "13:08"},
		{"strftime percent", `\D{%%}`, "%"},
		{"strftime literal text", `\D{[%H]}`, "[13]"},
		{"combined", `\u@\h:\w\$ `, "alice@host:~/project" + state.promptSymbol() + " "},
		{"literal text", `plain$ `, "plain$ "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&promptRenderer{src: tc.src, state: state}).render()
			if got != tc.want {
				t.Fatalf("render(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestPromptWorkingDirectoryEscapes covers the ~ abbreviation \w and \W
// apply, which bash documents as "with $HOME abbreviated with a tilde".
func TestPromptWorkingDirectoryEscapes(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		home    string
		wantW   string
		wantCap string
	}{
		{"inside home", "/home/alice/project", "/home/alice", "~/project", "project"},
		{"home itself", "/home/alice", "/home/alice", "~", "~"},
		{"outside home", "/var/log", "/home/alice", "/var/log", "log"},
		{"root", "/", "/home/alice", "/", "/"},
		{"prefix is not a path component", "/home/alice-2/x", "/home/alice", "/home/alice-2/x", "x"},
		{"no home", "/var/log", "", "/var/log", "log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &promptState{vars: map[string]string{"HOME": tc.home}, dir: tc.dir}
			if got := (&promptRenderer{src: `\w`, state: state}).render(); got != tc.wantW {
				t.Fatalf(`render("\\w") with dir %q home %q = %q, want %q`, tc.dir, tc.home, got, tc.wantW)
			}
			if got := (&promptRenderer{src: `\W`, state: state}).render(); got != tc.wantCap {
				t.Fatalf(`render("\\W") with dir %q home %q = %q, want %q`, tc.dir, tc.home, got, tc.wantCap)
			}
		})
	}
}

// TestPromptEscapesLikeBash pins more escapes against what bash 5.3
// produces for the same state, checked with "${PS1@P}". \s is always "gosh"
// rather than the basename of $0.
func TestPromptEscapesLikeBash(t *testing.T) {
	state := newPromptEscapeState()
	defer func(orig func() string) { promptTerminalName = orig }(promptTerminalName)
	promptTerminalName = func() string { return "tty" }
	for _, tc := range []struct{ name, src, want string }{
		{"octal escape", `\101`, "A"},
		{"octal escape bell", `\007`, "\a"},
		{"octal NUL is dropped", `\0`, ""},
		{"capital E is not an escape", `\E`, `\E`},
		{"unknown escape keeps its backslash", `\z`, `\z`},
		{"bare D without a format", `\D`, `\D`},
		{"empty D format is the time", `\D{}`, "13:08:07"},
		{"terminal device without one", `\l`, "tty"},
		{"version escapes", `\v \V`, "0.0 0.0.0"},
		{"twelve hour time", `\@ \T`, "01:08 PM 01:08:07"},
		{"date", `\d`, "Fri May 15"},
		{"strftime meridiem", `\D{%I:%M %p}`, "01:08 PM"},
		{"strftime day of year", `\D{%j}`, "135"},
		{"no jobs", `\j`, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&promptRenderer{src: tc.src, state: state}).render(); got != tc.want {
				t.Fatalf("render(%q) = %q, want %q (bash)", tc.src, got, tc.want)
			}
		})
	}
}

// TestPromptDirtrim checks \w with PROMPT_DIRTRIM against bash 5.3.
func TestPromptDirtrim(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"/usr/local/lib/x", ".../lib/x"},
		{"/usr/local/lib", ".../local/lib"},
		{"/a/b", "/a/b"},
		{"/home/alice/a/b/c", "~/a/b/c"},
		{"/home/alice/a/b/c/d", "~/a/b/c/d"},
		{"/home/alice/aa/b/c/d", "~/.../c/d"},
		{"/home/alice", "~"},
	} {
		state := &promptState{vars: map[string]string{"HOME": "/home/alice", "PROMPT_DIRTRIM": "2"}, dir: tc.dir}
		if got := (&promptRenderer{src: `\w|\W`, state: state}).render(); got != tc.want+"|"+path.Base(strings.Replace(tc.dir, "/home/alice", "~", 1)) {
			t.Errorf("render(\\w|\\W) in %q = %q, want %q", tc.dir, got, tc.want)
		}
	}
}
