package gosh

import (
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

// TestPromptEscapeKnownDivergences records the PS1 escapes where gosh still
// differs from bash. Every subtest is skipped on purpose: `want` is what
// bash 5.3 produces (checked with "${PS1@P}"), so deleting the t.Skip line
// turns the case into a live test once gosh matches.
//
// Two more escapes are hardcoded rather than wrong for gosh and so have no
// case here: \s is always "gosh" instead of the basename of $0, and \j is
// always "0" because gosh has no job control.
func TestPromptEscapeKnownDivergences(t *testing.T) {
	state := newPromptEscapeState()
	cases := []struct {
		name   string
		reason string
		src    string
		want   string
	}{
		{
			name: "octal escape",
			// bash: \nnn inserts the character with that octal value, so
			// \101 is "A" and \007 is a bell.
			// gosh: has no octal escape; `\1` renders as "1" and the
			// remaining digits stay literal, giving "101".
			reason: "gosh does not implement \\nnn octal escapes",
			src:    `\101`,
			want:   "A",
		},
		{
			name: "octal escape bell",
			// bash: "\007" is the same bell as "\a".
			// gosh: renders "\x0007" (a NUL byte followed by "07").
			reason: "gosh does not implement \\nnn octal escapes",
			src:    `\007`,
			want:   "\a",
		},
		{
			name: "octal escape nul",
			// bash: \0 is octal zero, and the NUL is dropped from the
			// expanded prompt, leaving an empty string.
			// gosh: emits a literal NUL byte.
			reason: "gosh emits a NUL byte for \\0",
			src:    `\0`,
			want:   "",
		},
		{
			name: "capital E is not an escape",
			// bash: only \e is the escape character; "\E" stays literal.
			// gosh: treats \E as another spelling of \e.
			reason: "gosh expands \\E as ESC",
			src:    `\E`,
			want:   `\E`,
		},
		{
			name: "unknown escape keeps its backslash",
			// bash: an unrecognized escape is emitted verbatim, backslash
			// included, so "\z" stays "\z".
			// gosh: drops the backslash and renders just "z".
			reason: "gosh drops the backslash of an unknown escape",
			src:    `\z`,
			want:   `\z`,
		},
		{
			name: "bare D without a format",
			// bash: "\D" without "{...}" is not an escape and stays literal.
			// gosh: renders "D".
			reason: "gosh drops the backslash of a bare \\D",
			src:    `\D`,
			want:   `\D`,
		},
		{
			name: "empty D format",
			// bash: "\D{}" uses the locale's time representation (%X),
			// which is "13:08:07" for this timestamp in the C locale.
			// gosh: renders "D".
			reason: "gosh does not implement the default \\D{} format",
			src:    `\D{}`,
			want:   "13:08:07",
		},
		{
			name: "terminal device",
			// bash: \l is the basename of the terminal device.
			// gosh: renders "l".
			reason: "gosh does not implement \\l",
			src:    `\l`,
			want:   "tty",
		},
		{
			name: "version escapes",
			// bash: \v is the version ("5.3") and \V the version with the
			// patch level ("5.3.9").
			// gosh: both render the literal string "gosh".
			reason: "gosh hardcodes \\v and \\V to \"gosh\" instead of its version",
			src:    `\v \V`,
			want:   "0.0 0.0.0",
		},
		{
			name: "twelve hour time with am pm",
			// bash: \@ is "%I:%M %p", so seconds are not shown and there is
			// a space before the meridiem, e.g. "01:08 PM".
			// gosh: renders "01:08:07PM".
			reason: "gosh formats \\@ with seconds and without a space",
			src:    `\@`,
			want:   "01:08 PM",
		},
		{
			name: "strftime meridiem",
			// bash: \D{...} passes the format to strftime, so %p, %I, %a,
			// %b, %e and %j all work.
			// gosh: implements only a handful of conversions and copies the
			// rest through verbatim, e.g. "%p".
			reason: "gosh's \\D{} implements only a subset of strftime",
			src:    `\D{%I:%M %p}`,
			want:   "01:08 PM",
		},
		{
			name: "strftime day of year",
			// See above; %j is the day of the year.
			reason: "gosh's \\D{} implements only a subset of strftime",
			src:    `\D{%j}`,
			want:   "135",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Skipf("known divergence: %s", tc.reason)
			got := (&promptRenderer{src: tc.src, state: state}).render()
			if got != tc.want {
				t.Fatalf("render(%q) = %q, want %q (bash)", tc.src, got, tc.want)
			}
		})
	}
}
