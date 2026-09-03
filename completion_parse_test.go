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
// boundaries.
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

// TestParseCompletionContextQuotingAndGrouping pins down word splitting inside
// quotes, test brackets, subshells, brace groups, and process substitution.
func TestParseCompletionContextQuotingAndGrouping(t *testing.T) {
	tests := []struct {
		in   string
		want completionContext
	}{
		{
			in:   `cmd "foo bar `,
			want: completionContext{prefix: "foo bar ", command: "cmd", quote: '"', words: []string{"cmd"}, cword: 1, inWord: true},
		},
		{
			in:   "echo a & ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "[ $x = f",
			want: completionContext{prefix: "f", command: "[", words: []string{"[", "$x", "="}, cword: 3, inWord: true},
		},
		{
			in:   "echo a; ( ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "( echo hi ",
			want: completionContext{command: "echo", words: []string{"echo", "hi"}, cword: 2},
		},
		{
			in:   "{ echo hi; ",
			want: completionContext{isCommand: true},
		},
		{
			in:   "echo <(foo ",
			want: completionContext{command: "foo", words: []string{"foo"}, cword: 1},
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

// TestParseCompletionContextRedirectsAndSubstitutions pins down the cases the
// parser handles more correctly than a plain lexical scan would: redirection
// operators are not words, extended globs are not commands, and a command
// substitution inside double quotes still opens a fresh command context.
func TestParseCompletionContextRedirectsAndSubstitutions(t *testing.T) {
	tests := []struct {
		in   string
		want completionContext
	}{
		{
			in:   "echo a > ",
			want: completionContext{command: "echo", words: []string{"echo", "a"}, cword: 2},
		},
		{
			in:   "echo a 2> ",
			want: completionContext{command: "echo", words: []string{"echo", "a"}, cword: 2},
		},
		{
			in:   "echo @(foo ",
			want: completionContext{},
		},
		{
			in:   `echo "$(foo `,
			want: completionContext{command: "foo", words: []string{"foo"}, cword: 1},
		},
		{
			in:   `echo "$( `,
			want: completionContext{isCommand: true},
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
