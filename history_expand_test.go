package gosh

import (
	"testing"
)

// TestDifferentialHistoryExpansion compares history expansion with Bash's
// through "history -p", over a fixed history.
func TestDifferentialHistoryExpansion(t *testing.T) {
	requireBash(t)
	const seed = `history -s 'echo a b c'
history -s 'ls -l /usr/lib/x.tar.gz | grep "two words" > out.txt'
history -s 'cd /tmp; echo it'
history -s 'echo /usr/lib/x foo.c a.b/c .bashrc'
history -p "$@"
echo "rc=$?"
`
	for _, expr := range []string{
		"!!", "!-1", "!-3", "!1", "!2:0", "!e", "!ls:3", "!?grep?", "!?two w?:%", "!x",
		"!$", "!^", "!*", "!:0", "!!:2-3", "!!:2*", "!!:1-", "!!:-2", "!ls:$", "!ls:5", "!1:9",
		"!2:3:h", "!2:3:t", "!2:3:r", "!2:3:e", "!2:3:t:r", "!1:q", "!1:x", "!1:p",
		"!1:s/a/X/", "!1:gs/ /_/", "!1:s/zz/y/", "!1:s/b/[&]/", "^it^that^", "^zz^y",
		"!1 and !-1", "a!=b", "echo $! ${!x} [!a]", "'!!' \"!!\" \\!!", "!!:y",
		"x!", "!#", "!1:s;b;B;", "!!:1:e", "!!:2:e", "!!:3:e", "!!:4:e", "!!:4:r", "!!:3:r", "!!:1:h", "!!:1:t",
		"!1:s/a/X/ !!:&", "!1:s/b/Y/ !-2:g&", "!ls:2*:s/l/L/",
	} {
		t.Run(expr, func(t *testing.T) {
			want := runDifferentialShell(t, "bash", []string{"--norc", "-c", "set -o history\n" + seed, "sh", expr}, nil, nil, "")
			got := runDifferentialShell(t, testGoshBinary, []string{"-c", seed, "sh", expr}, nil, nil, "")
			if got.stdout != want.stdout {
				t.Fatalf("history -p %q:\nbash: %q (stderr %q)\ngosh: %q (stderr %q)", expr, want.stdout, want.stderr, got.stdout, got.stderr)
			}
		})
	}
}

// TestDifferentialInteractiveHistoryExpansion compares history expansion of
// interactive input with Bash: what runs, what is remembered, and that a
// failed expansion drops the line.
func TestDifferentialInteractiveHistoryExpansion(t *testing.T) {
	requireBash(t)
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{name: "basic", lines: []string{"echo a b", "!!", "echo !$ !^", "^a^x^", "history"}},
		{name: "not found", lines: []string{"echo a", "!nope", "echo b", "history"}},
		{name: "print only", lines: []string{"echo a", "!!:s/a/b/:p", "history"}},
		{name: "quotes", lines: []string{"echo a", `echo '!!' "x!!" \!!`, "history"}},
		{name: "off", lines: []string{"echo a", "set +H", "echo !!", "set -o histexpand", "echo !!", "history"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := append(append([]string{}, tc.lines...), "exit")
			want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, historyEnv(t, "/dev/null"))
			got := runInteractiveHistory(t, testGoshBinary, "", []string{"-i", "--norc"}, script, historyEnv(t, "/dev/null"))
			if got.stdout != want.stdout {
				t.Fatalf("output mismatch:\nbash: %q\ngosh: %q\ngosh stderr: %q", want.stdout, got.stdout, got.stderr)
			}
		})
	}
}
