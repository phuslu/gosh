package gosh

import (
	"regexp"
	"slices"
	"testing"
)

// TestDifferentialPromptCommand compares PROMPT_COMMAND and PS0 with Bash,
// including what goes to stderr along with the prompts.
func TestDifferentialPromptCommand(t *testing.T) {
	requireBash(t)
	ps0Lines := regexp.MustCompile(`ps0:[0-9]+`)
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{name: "keeps status", lines: []string{`PROMPT_COMMAND='echo pc:$?; false'`, "(exit 3)", "echo st=$?", "", "echo st=$?"}},
		{name: "array", lines: []string{`PROMPT_COMMAND=("echo one" "echo two; x=1")`, "echo x=$x", "unset PROMPT_COMMAND", "echo done"}},
		{name: "sets prompt", lines: []string{`PROMPT_COMMAND='PS1="[$?] "'`, "false", "true"}},
		{name: "exit", lines: []string{`PROMPT_COMMAND='exit 4'`, "echo never"}},
		{name: "PS0", lines: []string{`PS0='ps0:\#\n'`, "echo run", "", "unset PS0", "echo run"}},
		{name: "syntax error status", lines: []string{"echo (", "echo st=$?"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := append(append([]string{}, tc.lines...), "exit")
			env := append(historyEnv(t, "/dev/null"), "PS1=$ ", "PS2=> ")
			want := runInteractiveHistory(t, "bash", "", []string{"--noprofile", "--norc", "-i"}, script, env)
			got := runInteractiveHistory(t, testGoshBinary, "", []string{"-i", "--norc"}, script, env)
			if got.stdout != want.stdout || got.code != want.code {
				t.Fatalf("stdout mismatch:\nbash: rc=%d %q\ngosh: rc=%d %q\ngosh stderr: %q", want.code, want.stdout, got.code, got.stdout, got.stderr)
			}
			// Bash echoes the prompts and input lines it reads from a pipe,
			// and readline does not, so only PS0 can be compared.
			if wantPS0, gotPS0 := ps0Lines.FindAllString(want.stderr, -1), ps0Lines.FindAllString(got.stderr, -1); !slices.Equal(gotPS0, wantPS0) {
				t.Fatalf("PS0 mismatch:\nbash: %q\ngosh: %q", wantPS0, gotPS0)
			}
		})
	}
}
