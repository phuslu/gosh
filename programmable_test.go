package gosh

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func newProgrammableCompleter(t *testing.T, functions string) *autoCompleter {
	t.Helper()
	var stdout, stderr bytes.Buffer
	registry := newCompletionRegistry()
	var runner *interp.Runner
	deps := callDeps{
		runner:     func() *interp.Runner { return runner },
		completion: registry,
	}
	runner, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.Env(expand.ListEnviron(testEnv(t)...)),
		interp.CallHandler(callHandler(deps)),
		interp.ExecHandlers(execHandler(deps, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if functions != "" {
		prog, err := syntax.NewParser().Parse(strings.NewReader(functions), "")
		if err != nil {
			t.Fatalf("parse functions: %v", err)
		}
		if err := runner.Run(context.Background(), prog); err != nil {
			t.Fatalf("define functions: %v\nstderr: %s", err, stderr.String())
		}
	}
	return &autoCompleter{
		ctx:        context.Background(),
		runner:     runner,
		opts:       newShellOptions(false),
		completion: registry,
		stdin:      strings.NewReader(""),
		stderr:     &stderr,
	}
}

func TestProgrammableCompletionFunction(t *testing.T) {
	c := newProgrammableCompleter(t, `
		_complete_cmd() { COMPREPLY=(alpha beta); }
	`)
	c.completion.set("cmd", &completionSpec{funcName: "_complete_cmd"})

	ctx := parseCompletionContext([]rune("cmd "))
	result := c.programmableCompletion(ctx, []rune("cmd "), 4)
	if !result.handled {
		t.Fatal("programmable completion was not handled")
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(result.candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", result.candidates, want)
	}
}

func TestProgrammableCompletionFunctionVariables(t *testing.T) {
	c := newProgrammableCompleter(t, `
		_complete_cmd() { COMPREPLY=("${COMP_WORDS[1]}" "${COMP_CWORD}"); }
	`)
	c.completion.set("cmd", &completionSpec{funcName: "_complete_cmd"})

	ctx := parseCompletionContext([]rune("cmd arg"))
	result := c.programmableCompletion(ctx, []rune("cmd arg"), 7)
	if !result.handled {
		t.Fatal("programmable completion was not handled")
	}
	if want := []string{"arg", "1"}; !reflect.DeepEqual(result.candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", result.candidates, want)
	}
}

func TestProgrammableCompletionWordList(t *testing.T) {
	c := newProgrammableCompleter(t, "")
	c.completion.set("greet", &completionSpec{words: []string{"help", "other"}})

	ctx := parseCompletionContext([]rune("greet he"))
	result := c.programmableCompletion(ctx, []rune("greet he"), 8)
	if !result.handled {
		t.Fatal("programmable completion was not handled")
	}
	if want := []string{"help"}; !reflect.DeepEqual(result.candidates, want) {
		t.Fatalf("candidates = %#v, want %#v", result.candidates, want)
	}
}

func TestProgrammableCompletionCompopt(t *testing.T) {
	c := newProgrammableCompleter(t, `
		_complete_cmd() { compopt -o nospace; COMPREPLY=(only); }
	`)
	c.completion.set("cmd", &completionSpec{funcName: "_complete_cmd"})

	ctx := parseCompletionContext([]rune("cmd "))
	result := c.programmableCompletion(ctx, []rune("cmd "), 4)
	if !result.handled {
		t.Fatal("programmable completion was not handled")
	}
	if !result.noSpace {
		t.Fatalf("compopt -o nospace did not reach the completion invocation\nstderr: %s", c.stderr)
	}
}

func TestRunCompgen(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh", "-c", `compgen -W 'alpha beta' al`},
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil {
		t.Fatalf("Run compgen failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "alpha\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunCompleteListing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			complete -W 'one two' greet
			complete -p greet
		`},
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil {
		t.Fatalf("Run complete failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "complete -W 'one two' greet\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestParseCompleteArgsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-F"},
		{"-o", "unknown", "cmd"},
		{"-C", "echo", "cmd"},
		{"one", "two"},
	} {
		if _, _, _, _, err := parseCompleteArgs(args); err == nil {
			t.Fatalf("parseCompleteArgs(%q) = nil error, want error", args)
		}
	}
}

func TestBuiltinCompoptOutsideCompletion(t *testing.T) {
	deps := callDeps{completion: newCompletionRegistry()}
	if err := builtinCompopt(deps, []string{"-o", "nospace"}, io.Discard); err == nil {
		t.Fatal("compopt outside a completion function should fail")
	}
}

// TestApplyCompletionOptionsUnrelatedCandidates checks that candidates which
// do not extend the word under the cursor never splice text into the line.
// Programmable completion functions are free to return words unrelated to
// $cur, and bash only lists those instead of inserting them.
func TestApplyCompletionOptionsUnrelatedCandidates(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		options []string
		want    [][]rune
		wantLen int
		listed  bool
	}{
		{
			name:    "unrelated single candidate",
			prefix:  "ab",
			options: []string{"xyzzy"},
			listed:  true,
		},
		{
			name:    "unrelated candidates",
			prefix:  "ab",
			options: []string{"xyzzy", "xyzzz"},
			listed:  true,
		},
		{
			name:    "common prefix shorter than word",
			prefix:  "abc",
			options: []string{"abd", "abe"},
			listed:  true,
		},
		{
			name:    "candidate extends the word",
			prefix:  "ab",
			options: []string{"abcd", "abce"},
			want:    [][]rune{[]rune("c")},
			wantLen: 2,
		},
		{
			name:    "single candidate extends the word",
			prefix:  "ab",
			options: []string{"abc"},
			want:    [][]rune{[]rune("c ")},
			wantLen: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := &autoCompleter{
				ctx:    context.Background(),
				opts:   newShellOptions(false),
				stdin:  strings.NewReader(""),
				stdout: &stdout,
				stderr: io.Discard,
			}
			ctx := completionContext{prefix: test.prefix, isCommand: true, inWord: true}
			got, gotLen := c.applyCompletionOptions(test.options, ctx, false)
			if !reflect.DeepEqual(got, test.want) || gotLen != test.wantLen {
				t.Fatalf("applyCompletionOptions(%q, prefix %q) = %q, %d, want %q, %d",
					test.options, test.prefix, got, gotLen, test.want, test.wantLen)
			}
			if listed := stdout.Len() > 0; listed != test.listed {
				t.Fatalf("candidates listed = %v, want %v (output %q)", listed, test.listed, stdout.String())
			}
		})
	}
}
