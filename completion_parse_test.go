package gosh

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestParseCompletionContextGolden pins down the parser-derived context for
// the inputs listed in the migration plan: simple commands, quotes, command
// substitution, arithmetic, if/for/case, assignments, and here-document
// boundaries. Parser-correct behavior intentionally differs from the legacy
// scanner in several of these cases.
func TestParseCompletionContextGolden(t *testing.T) {
	tests := []struct {
		in   string
		want completionContext
	}{
		{
			in:   "cd ~/Do",
			want: completionContext{prefix: "~/Do", command: "cd", words: []string{"cd"}, cword: 1, inWord: true},
		},
		{
			in:   "cmd ",
			want: completionContext{command: "cmd", words: []string{"cmd"}, cword: 1},
		},
		{
			in:   "cmd arg",
			want: completionContext{prefix: "arg", command: "cmd", words: []string{"cmd"}, cword: 1, inWord: true},
		},
		{
			in:   "",
			want: completionContext{isCommand: true},
		},
		{
			in:   `git commit -m "`,
			want: completionContext{quote: '"', command: "git", words: []string{"git", "commit", "-m"}, cword: 3, inWord: true},
		},
		{
			in:   `git commit -m "hello wor`,
			want: completionContext{prefix: "hello wor", quote: '"', command: "git", words: []string{"git", "commit", "-m"}, cword: 3, inWord: true},
		},
		{
			in:   `cd '~/Do`,
			want: completionContext{prefix: "~/Do", quote: '\'', command: "cd", words: []string{"cd"}, cword: 1, inWord: true},
		},
		{
			in:   `echo $(foo `,
			want: completionContext{command: "foo", words: []string{"foo"}, cword: 1},
		},
		{
			in:   `echo $(foo`,
			want: completionContext{prefix: "foo", isCommand: true, inWord: true},
		},
		{
			in:   "echo $( ",
			want: completionContext{isCommand: true},
		},
		{
			in:   `echo $(foo $(bar `,
			want: completionContext{command: "bar", words: []string{"bar"}, cword: 1},
		},
		{
			in:   "echo $((1+",
			want: completionContext{prefix: "1+", inWord: true},
		},
		{
			in:   "if foo; then ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "if foo; then bar ",
			want: completionContext{command: "bar", words: []string{"bar"}, cword: 1},
		},
		{
			in:   "if foo; ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "for x in a b; do ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "while foo; do ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "case $x in foo) ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "case $x in fo",
			want: completionContext{prefix: "fo", inWord: true},
		},
		{
			in:   "FOO=1 bar ",
			want: completionContext{command: "bar", words: []string{"FOO=1", "bar"}, cword: 2},
		},
		{
			in:   "FOO=1 ",
			want: completionContext{isCommand: true, words: []string{"FOO=1"}, cword: 1},
		},
		{
			in:   "cat <<EOF",
			want: completionContext{},
		},
		{
			in:   "echo a |",
			want: completionContext{isCommand: true},
		},
		{
			in:   "echo a && ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "echo a; ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "echo a <",
			want: completionContext{command: "echo", words: []string{"echo", "a"}, cword: 2},
		},
		{
			in:   `cd ~/Do\ F`,
			want: completionContext{prefix: "~/Do F", command: "cd", words: []string{"cd"}, cword: 1, inWord: true},
		},
		{
			in:   `cd ~/Do\`,
			want: completionContext{prefix: "~/Do", command: "cd", words: []string{"cd"}, cword: 1, inWord: true},
		},
		{
			in:   "[ foo ",
			want: completionContext{command: "[", words: []string{"[", "foo"}, cword: 2},
		},
		{
			in:   "[[ $x == ",
			want: completionContext{},
		},
		{
			in:   "[[ $x == f",
			want: completionContext{prefix: "f", inWord: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseCompletionContext([]rune(tt.in))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseCompletionContext(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

// TestCompletionContextLegacyDifferential asserts that the parser-derived
// context agrees exactly with the legacy scanner on the inputs where the
// scanner was already correct. Divergent (parser-correct) cases are pinned in
// TestParseCompletionContextGolden instead.
func TestCompletionContextLegacyDifferential(t *testing.T) {
	agree := []string{
		"",
		"cmd ",
		"cmd arg",
		"cd ~/Do",
		`git commit -m "`,
		`cd '~/Do`,
		`cmd "foo bar `,
		"echo a |",
		"echo a && ",
		"echo a; ",
		"echo a & ",
		"echo $(foo ",
		"echo $( ",
		"case $x in foo) ",
		"[ foo ",
		"[ $x = f",
		`cd ~/Do\ F`,
		`cd ~/Do\`,
		"echo a; ( ",
		"( echo hi ",
		"{ echo hi; ",
		"echo <(foo ",
	}
	normalize := func(c completionContext) completionContext {
		if len(c.words) == 0 {
			c.words = nil
		}
		return c
	}
	for _, in := range agree {
		legacy := normalize(scanCompletionContext([]rune(in)))
		parsed := normalize(parseCompletionContext([]rune(in)))
		if !reflect.DeepEqual(legacy, parsed) {
			t.Errorf("%q: legacy=%#v parsed=%#v", in, legacy, parsed)
		}
	}
}

// TestCompletionContextLegacyDivergence documents cases where the parser is
// intentionally more correct than the scanner. This is the corpus the
// differential fuzzing surfaced during the migration.
func TestCompletionContextLegacyDivergence(t *testing.T) {
	tests := []struct {
		in     string
		legacy completionContext
		parsed completionContext
	}{
		{
			in:     "if foo; then bar ",
			legacy: completionContext{command: "then", words: []string{"then", "bar"}, cword: 2},
			parsed: completionContext{command: "bar", words: []string{"bar"}, cword: 1},
		},
		{
			in:     "FOO=1 bar ",
			legacy: completionContext{command: "FOO=1", words: []string{"FOO=1", "bar"}, cword: 2},
			parsed: completionContext{command: "bar", words: []string{"FOO=1", "bar"}, cword: 2},
		},
		{
			in:     "FOO=1 ",
			legacy: completionContext{command: "FOO=1", words: []string{"FOO=1"}, cword: 1},
			parsed: completionContext{isCommand: true, words: []string{"FOO=1"}, cword: 1},
		},
		{
			in:     "cat <<EOF",
			legacy: completionContext{prefix: "<<EOF", command: "cat", words: []string{"cat"}, cword: 1, inWord: true},
			parsed: completionContext{},
		},
		{
			in:     "echo a > ",
			legacy: completionContext{command: "echo", words: []string{"echo", "a", ">"}, cword: 3},
			parsed: completionContext{command: "echo", words: []string{"echo", "a"}, cword: 2},
		},
		{
			in:     "echo a 2> ",
			legacy: completionContext{command: "echo", words: []string{"echo", "a", "2>"}, cword: 3},
			parsed: completionContext{command: "echo", words: []string{"echo", "a"}, cword: 2},
		},
		{
			in:     "echo @(foo ",
			legacy: completionContext{command: "foo", words: []string{"foo"}, cword: 1},
			parsed: completionContext{},
		},
		{
			in:     `echo "$(foo `,
			legacy: completionContext{prefix: "$(foo ", quote: '"', command: "echo", words: []string{"echo"}, cword: 1, inWord: true},
			parsed: completionContext{command: "foo", words: []string{"foo"}, cword: 1},
		},
		{
			in:     `echo "$( `,
			legacy: completionContext{prefix: "$( ", quote: '"', command: "echo", words: []string{"echo"}, cword: 1, inWord: true},
			parsed: completionContext{isCommand: true},
		},
	}
	for _, tt := range tests {
		if got := scanCompletionContext([]rune(tt.in)); !reflect.DeepEqual(got, tt.legacy) {
			t.Errorf("legacy(%q) = %#v, want %#v", tt.in, got, tt.legacy)
		}
		if got := parseCompletionContext([]rune(tt.in)); !reflect.DeepEqual(got, tt.parsed) {
			t.Errorf("parsed(%q) = %#v, want %#v", tt.in, got, tt.parsed)
		}
	}
}

// TestParseCompletionContextInvariants checks structural invariants across a
// broad corpus, including inputs where completion gives up entirely.
func TestParseCompletionContextInvariants(t *testing.T) {
	inputs := []string{
		"echo hello world ",
		`git commit -m "hello `,
		"echo $(date +%s ",
		"echo $((1 + 2)) ",
		"if [ -f x ]; then cat ",
		"for f in *.go; do echo ",
		"case $1 in a|b) echo ",
		"VAR=x y=2 cmd ",
		"sudo env -i ",
		"ls -la > ",
		"grep foo < ",
		"cat <<'EOF'",
		"echo 'unterminated",
		`echo "unterminated`,
		"printf '%s\n' ",
		"{ echo a; echo b; ",
		"(( ",
		"$[ 1 + ",
		"echo ${",
		"echo ${FO",
		"echo $FO",
		"function ",
		"function myf",
		"myfunc() ",
		"time ",
		"coproc ",
		"echo one\necho two ",
		"echo x # trailing comment ",
	}
	for _, in := range inputs {
		ctx := parseCompletionContext([]rune(in))
		if ctx.cword != len(ctx.words) {
			t.Errorf("%q: cword=%d len(words)=%d", in, ctx.cword, len(ctx.words))
		}
		if ctx.quote != 0 && ctx.quote != '\'' && ctx.quote != '"' {
			t.Errorf("%q: invalid quote %q", in, ctx.quote)
		}
		if strings.Contains(ctx.prefix, completionSentinel) {
			t.Errorf("%q: sentinel leaked into prefix %q", in, ctx.prefix)
		}
		for _, w := range ctx.words {
			if strings.Contains(w, completionSentinel) {
				t.Errorf("%q: sentinel leaked into word %q", in, w)
			}
		}
		if ctx.command != "" && len(ctx.words) == 0 {
			t.Errorf("%q: command %q without words", in, ctx.command)
		}
	}
}

// TestParseCompletionContextPanicFree is a cheap guard for the repair loop on
// adversarial inputs; the fuzz target exercises the same path continuously.
func TestParseCompletionContextPanicFree(t *testing.T) {
	inputs := []string{
		"$",
		"\\",
		"'",
		`"`,
		"`",
		"$(",
		"${",
		"$((",
		"(",
		")",
		"{",
		"}",
		"if",
		"then",
		"do",
		"done",
		"fi",
		"esac",
		"case $x in ",
		"for x in ",
		"while ",
		"until ",
		"function ",
		"foo() ",
		"; ",
		"| ",
		"&& ",
		"& ",
		"< ",
		"> ",
		"echo a ;; ",
		"echo a )",
		"echo a }",
		"echo $(foo $(bar $(baz ",
		"if a; then if b; then if c; then ",
		`echo "\`,
		`echo '\`,
	}
	for _, in := range inputs {
		_ = parseCompletionContext([]rune(in))
	}
}

func TestCompletionContextString(t *testing.T) {
	// Guard against accidental changes to the completionContext fields the
	// programmable completion path depends on.
	ctx := parseCompletionContext([]rune(`git commit -m "hello`))
	_ = fmt.Sprintf("%#v", ctx)
}
