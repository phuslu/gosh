package gosh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chzyer/readline"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func testEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"HISTFILE=" + os.DevNull,
	}
}

func TestRunCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:    []string{"gosh", "-c", `printf '%s:%s' "$0" "$1"`, "argv0", "param1"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run -c failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "argv0:param1"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunNonInteractiveStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:    []string{"gosh"},
		Stdin:   strings.NewReader("read value; printf '<%s>' \"$value\"\nfrom-stdin\n"),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run stdin failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "<from-stdin>"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunCommandDoesNotRenderPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh", "-c", `echo ok`},
		Stdout: &stdout,
		Stderr: &stderr,
		Env: append(testEnv(t),
			`PS1=$(__gosh_missing_prompt_helper)`,
		),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run -c failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "ok\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNonInteractiveDoesNotSourceInitOrRenderPrompt(t *testing.T) {
	dir := t.TempDir()
	initFile := filepath.Join(dir, "goshrc")
	if err := os.WriteFile(initFile, []byte("echo init-ran\nPS1='$(__gosh_missing_prompt_helper)'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh"},
		Stdin:  strings.NewReader("echo stdin\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Env: append(testEnv(t),
			"GOSH_ENV="+initFile,
			`PS1=$(__gosh_missing_prompt_helper)`,
		),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run stdin failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "stdin\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNonInteractiveQuotedNewline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   []string{"gosh"},
		Stdin:  strings.NewReader("printf \"hello \n\"\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	if err != nil {
		t.Fatalf("Run quoted newline failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "hello \n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
	status := interp.ExitStatus(42)
	if !IsExitStatus(status) {
		t.Fatalf("IsExitStatus(interp.ExitStatus) = false")
	}
	if got := ExitCode(status); got != 42 {
		t.Fatalf("ExitCode(interp.ExitStatus) = %d, want 42", got)
	}
	if got := ExitCode(context.Canceled); got != 130 {
		t.Fatalf("ExitCode(context.Canceled) = %d, want 130", got)
	}
	if got := ExitCode(os.ErrInvalid); got != 127 {
		t.Fatalf("ExitCode(other) = %d, want 127", got)
	}
}

func TestSetEnv(t *testing.T) {
	env := []string{"A=1", "B=2", "A=3"}
	env = SetEnv(env, "A", "4")
	if want := []string{"A=1", "B=2", "A=4"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("SetEnv update = %#v, want %#v", env, want)
	}
	env = SetEnv(env, "C", "5")
	if want := []string{"A=1", "B=2", "A=4", "C=5"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("SetEnv append = %#v, want %#v", env, want)
	}
}

func TestHistoryEncodingAndControl(t *testing.T) {
	for _, line := range []string{
		"echo plain",
		"echo one\necho two",
		historyEncodedPrefix + "literal",
	} {
		if got := decodeHistoryLine(encodeHistoryLine(line)); got != line {
			t.Fatalf("history roundtrip = %q, want %q", got, line)
		}
	}

	history := &history{limit: 10, control: parseHistoryControl("ignoreboth")}
	if history.Add(" leading-space") {
		t.Fatalf("history saved ignorespace entry")
	}
	if !history.Add("echo ok") {
		t.Fatalf("history did not save first entry")
	}
	if history.Add("echo ok") {
		t.Fatalf("history saved duplicate entry")
	}
	if got, want := history.Entries(), []string{"echo ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("history entries = %#v, want %#v", got, want)
	}
}

func TestBindParser(t *testing.T) {
	key, action, err := parseBindArgs([]string{`"\e[A": history-search-backward`})
	if err != nil {
		t.Fatalf("parseBindArgs failed: %v", err)
	}
	if key != `"\e[A"` || action != "history-search-backward" {
		t.Fatalf("bind args = %q, %q", key, action)
	}
	seq, err := parseKeySequence(key)
	if err != nil {
		t.Fatalf("parseKeySequence failed: %v", err)
	}
	if want := []byte{0x1b, '[', 'A'}; !reflect.DeepEqual(seq, want) {
		t.Fatalf("key sequence = %#v, want %#v", seq, want)
	}
	if got, ok := lookupBindAction(action); !ok || got != keyActionHistorySearchBackward {
		t.Fatalf("bind action = %v, %v", got, ok)
	}
}

func TestHistorySearchKeepsCursorAtSearchPosition(t *testing.T) {
	const long = `sed -i -E 's/const bufWriterPoolBufferSize = .+/var bufWriterPoolBufferSize = func() int { n, _ := strconv.Atoi(os.Getenv("HTTP2_WRITER_POOL_BUFFER_SIZE")); return max(n, 32768) }()/' /Users/xiangyu.lu/go/pkg/mod/golang.org/x/net@v0.54.0/http2/http2.go`
	const short = `sed -n '1p' http2.go`

	history := &history{limit: 10}
	history.append("echo ignored")
	history.append(short)
	history.append(long)

	var gotLine []rune
	gotPos := -1
	search := &historySearch{
		history: history,
		setBuffer: func(_ *readline.Instance, line []rune, pos int) bool {
			gotLine = append([]rune(nil), line...)
			gotPos = pos
			return true
		},
		searchIndex: -1,
	}

	search.OnChange([]rune("sed"), 3, 0)
	if !search.applySearch(keyActionHistorySearchBackward) {
		t.Fatalf("first history search failed")
	}
	if got := string(gotLine); got != long {
		t.Fatalf("first match = %q, want %q", got, long)
	}
	if gotPos != len([]rune("sed")) {
		t.Fatalf("first cursor position = %d, want %d", gotPos, len([]rune("sed")))
	}

	if !search.applySearch(keyActionHistorySearchBackward) {
		t.Fatalf("second history search failed")
	}
	if got := string(gotLine); got != short {
		t.Fatalf("second match = %q, want %q", got, short)
	}
	if gotPos != len([]rune("sed")) {
		t.Fatalf("second cursor position = %d, want %d", gotPos, len([]rune("sed")))
	}
}

func TestHistorySearchCursorMoveUsesAbsolutePositioning(t *testing.T) {
	line := []rune(strings.Repeat("a", 160))
	got := string(cursorMoveFromEndSequence(2, line, 3, 80))
	if want := "\x1b[2A\r\x1b[5C"; got != want {
		t.Fatalf("cursor move sequence = %q, want %q", got, want)
	}
	if strings.Contains(got, "\b") {
		t.Fatalf("cursor move sequence should not use backspace: %q", got)
	}
}

func TestHistorySearchBellWritesDirectly(t *testing.T) {
	var stdout bytes.Buffer
	search := &historySearch{
		rl: &readline.Instance{
			Config: &readline.Config{Stdout: &stdout},
		},
	}
	search.emitBell()
	if got, want := stdout.Bytes(), []byte{0x07}; !bytes.Equal(got, want) {
		t.Fatalf("bell output = %#v, want %#v", got, want)
	}
}

func TestCompletionHelpers(t *testing.T) {
	ctx := scanCompletionContext([]rune("cd ~/Do"))
	if ctx.isCommand || ctx.command != "cd" || ctx.prefix != "~/Do" {
		t.Fatalf("completion context = %#v", ctx)
	}
	if got, want := escapeCompletionForContext("a b$", 0), `a\ b\$`; got != want {
		t.Fatalf("escaped completion = %q, want %q", got, want)
	}
	if got, want := escapeCompletionForContext(`a"$`, '"'), `a\"\$`; got != want {
		t.Fatalf("double-quoted completion = %q, want %q", got, want)
	}
	if got, want := longestCommonPrefix([]string{"alpha", "alpine"}), "alp"; got != want {
		t.Fatalf("longest common prefix = %q, want %q", got, want)
	}
	expanded, ok := expandTilde("~/src", "/home/tester")
	if !ok || expanded != filepath.Join("/home/tester", "src") {
		t.Fatalf("expandTilde = %q, %v", expanded, ok)
	}
}

func TestPromptRenderer(t *testing.T) {
	state := &promptState{
		vars:      map[string]string{"USER": "alice", "HOME": "/home/alice"},
		dir:       "/home/alice/project",
		host:      "host.example",
		shortHost: "host",
		seq:       3,
		now:       time.Date(2026, 5, 15, 9, 8, 7, 0, time.UTC),
	}
	got := (&promptRenderer{src: `\u@\h:\w \D{%F} \# \$`, state: state}).render()
	want := "alice@host:~/project 2026-05-15 3 " + state.promptSymbol()
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	if got := defaultPrompt(""); !strings.HasPrefix(got, "sh-0.0") {
		t.Fatalf("empty-version prompt = %q", got)
	}
}

func TestPromptCommandSubstitutionKeepsOutputOnExitStatus(t *testing.T) {
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	runner, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(strings.NewReader(""), &stdout, &stderr),
		interp.Env(expand.ListEnviron(testEnv(t)...)),
	)
	if err != nil {
		t.Fatalf("interp.New failed: %v", err)
	}
	run := func(script string) error {
		prog, err := syntax.NewParser().Parse(strings.NewReader(script), "")
		if err != nil {
			return err
		}
		return runner.Run(ctx, prog)
	}
	if err := run(`__git_ps1() { printf "$1" master; return 7; }`); err != nil {
		t.Fatalf("define __git_ps1 failed: %v", err)
	}
	if err := run(`false`); !IsExitStatus(err) {
		t.Fatalf("false status = %v, want ExitStatus", err)
	}

	state := &promptState{
		ctx:       ctx,
		runner:    runner,
		stdin:     strings.NewReader(""),
		stderr:    &stderr,
		vars:      map[string]string{"USER": "alice", "HOME": "/home/alice"},
		dir:       "/home/alice/project",
		host:      "host.example",
		shortHost: "host",
		now:       time.Date(2026, 5, 15, 9, 8, 7, 0, time.UTC),
	}
	got := (&promptRenderer{src: `\w$(__git_ps1 " (%s)")!`, state: state}).render()
	want := "~/project (master)!"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

func TestShellOptionVersion(t *testing.T) {
	env := &shellEnviron{
		base:    expand.ListEnviron("X=1"),
		flags:   func() string { return "hBs" },
		version: "1.2.3",
	}
	if got := env.Get("-").String(); got != "hBs" {
		t.Fatalf("$- = %q, want hBs", got)
	}
	if got := env.Get("BASH_VERSION").String(); got != "1.2.3(1)-gosh" {
		t.Fatalf("BASH_VERSION = %q", got)
	}
}

func TestShoptQuiet(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			if shopt -q extglob; then echo bad-extglob-default; else echo extglob-off; fi
			shopt -s extglob
			if shopt -q extglob; then echo extglob-on; else echo bad-extglob-set; fi
			if builtin shopt -q extglob; then echo builtin-extglob-on; else echo bad-builtin-extglob; fi
			if command shopt -q extglob; then echo command-extglob-on; else echo bad-command-extglob; fi
			if shopt -q extglob nullglob; then echo bad-mixed; else echo mixed-off; fi
			if shopt -q -o errexit; then echo bad-errexit-default; else echo errexit-off; fi
			shopt -q -s nullglob
			if shopt -q nullglob; then echo nullglob-on; else echo bad-nullglob-set; fi
			shopt -qu nullglob
			if shopt -q nullglob; then echo bad-nullglob-unset; else echo nullglob-off; fi
		`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run shopt -q failed: %v\nstderr: %s", err, stderr.String())
	}
	want := strings.Join([]string{
		"extglob-off",
		"extglob-on",
		"builtin-extglob-on",
		"command-extglob-on",
		"mixed-off",
		"errexit-off",
		"nullglob-on",
		"nullglob-off",
		"",
	}, "\n")
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestShoptCheckwinsize(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			if shopt -q checkwinsize; then echo default-on; else echo bad-default; fi
			shopt -u checkwinsize
			if shopt -q checkwinsize; then echo bad-unset; else echo unset; fi
			shopt -s checkwinsize
			if shopt -q checkwinsize; then echo set; else echo bad-set; fi
			builtin shopt -u checkwinsize
			if command shopt -q checkwinsize; then echo bad-command-query; else echo command-query-unset; fi
			command shopt -s checkwinsize
			shopt checkwinsize
		`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run checkwinsize shopt failed: %v\nstderr: %s", err, stderr.String())
	}
	want := strings.Join([]string{
		"default-on",
		"unset",
		"set",
		"command-query-unset",
		"checkwinsize\ton",
		"",
	}, "\n")
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestPrintfDashDash(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args: []string{"gosh", "-c", `
			printf -- '%s\n' direct
			builtin printf -- '%s\n' builtin
			command printf -- '%s\n' command
			printf() { echo "$1:$2"; }
			printf -- function
		`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     testEnv(t),
		Version: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run printf -- failed: %v\nstderr: %s", err, stderr.String())
	}
	want := strings.Join([]string{
		"direct",
		"builtin",
		"command",
		"--:function",
		"",
	}, "\n")
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q\nstderr: %s", got, want, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}
