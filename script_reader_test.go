package gosh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runScriptInput(t *testing.T, args []string, stdin io.Reader) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(Config{
		Args:   args,
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Env:    testEnv(t),
	})
	return stdout.String(), stderr.String(), err
}

func writeScriptFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openScriptFile(t *testing.T, content string) *os.File {
	t.Helper()
	file, err := os.Open(writeScriptFile(t, content))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

// Like Bash, every way of running a script executes each command as soon
// as it is complete, so a later syntax error does not stop earlier lines.
func TestRunScriptStopsAtSyntaxError(t *testing.T) {
	const script = "echo before\n(\necho never\n"
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{name: "stdin", args: []string{"gosh"}, stdin: strings.NewReader(script)},
		{name: "command", args: []string{"gosh", "-c", script}},
		{name: "file", args: []string{"gosh", writeScriptFile(t, script)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := runScriptInput(t, tc.args, tc.stdin)
			if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2 (err: %v)", got, err)
			}
			if stdout != "before\n" {
				t.Fatalf("stdout = %q, want %q", stdout, "before\n")
			}
		})
	}
}

func TestRunScriptFileNamesSyntaxError(t *testing.T) {
	script := writeScriptFile(t, "echo ok\nif\n")
	_, _, err := runScriptInput(t, []string{"gosh", script}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), script+":2:") {
		t.Fatalf("err = %v, want it to start with %q", err, script+":2:")
	}
}

func TestRunScriptFinalLineWithoutNewline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{name: "stdin", args: []string{"gosh"}, stdin: strings.NewReader("echo a\necho z")},
		{name: "command", args: []string{"gosh", "-c", "echo a\necho z"}},
		{name: "file", args: []string{"gosh", writeScriptFile(t, "echo a\necho z")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runScriptInput(t, tc.args, tc.stdin)
			if err != nil {
				t.Fatalf("Run failed: %v\nstderr: %s", err, stderr)
			}
			if stdout != "a\nz\n" {
				t.Fatalf("stdout = %q, want %q", stdout, "a\nz\n")
			}
		})
	}
}

// A script read from stdin shares it with its commands, which must see the
// input right after the command that reads it, whether stdin is a pipe, a
// regular file, or an arbitrary reader.
func TestRunStdinScriptSharesInput(t *testing.T) {
	const script = "read -r x; echo \"got<$x>\"\nline2\necho after\n"
	const want = "got<line2>\nafter\n"
	pipe := func() io.Reader {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			pw.WriteString(script)
			pw.Close()
		}()
		t.Cleanup(func() { pr.Close() })
		return pr
	}
	for _, tc := range []struct {
		name  string
		stdin func() io.Reader
	}{
		{name: "reader", stdin: func() io.Reader { return strings.NewReader(script) }},
		{name: "pipe", stdin: pipe},
		{name: "file", stdin: func() io.Reader { return openScriptFile(t, script) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runScriptInput(t, []string{"gosh"}, tc.stdin())
			if err != nil {
				t.Fatalf("Run failed: %v\nstderr: %s", err, stderr)
			}
			if stdout != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

// A seekable stdin is read from its current offset, not from the start.
func TestRunStdinScriptFromFileOffset(t *testing.T) {
	file := openScriptFile(t, "echo skipped\necho run\n")
	if _, err := file.Seek(int64(len("echo skipped\n")), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runScriptInput(t, []string{"gosh"}, file)
	if err != nil {
		t.Fatalf("Run failed: %v\nstderr: %s", err, stderr)
	}
	if stdout != "run\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "run\n")
	}
}

// Reads interleaved with a script larger than the reader's buffer exercise
// both keeping the buffered input and dropping it after a command moved the
// file offset, including lines which straddle a buffer boundary.
func TestRunStdinScriptLargeFileWithReads(t *testing.T) {
	var script, want strings.Builder
	pad := strings.Repeat("x", 97)
	for i := 0; script.Len() < 3*scriptReaderBufSize; i++ {
		if i%50 == 0 {
			fmt.Fprintf(&script, "read -r v; echo \"$v\"\ndata-%d\n", i)
			fmt.Fprintf(&want, "data-%d\n", i)
			continue
		}
		fmt.Fprintf(&script, "echo line-%d %s\n", i, pad[:1+i%(len(pad)-1)])
		fmt.Fprintf(&want, "line-%d %s\n", i, pad[:1+i%(len(pad)-1)])
	}
	stdout, stderr, err := runScriptInput(t, []string{"gosh"}, openScriptFile(t, script.String()))
	if err != nil {
		t.Fatalf("Run failed: %v\nstderr: %s", err, stderr)
	}
	if stdout != want.String() {
		t.Fatalf("stdout mismatch: got %d bytes, want %d bytes", len(stdout), want.Len())
	}
}

// A single command spanning many lines used to be re-parsed from its first
// line for every line added, which took seconds for a few thousand lines.
func TestRunStdinScriptLongCommand(t *testing.T) {
	var script strings.Builder
	script.WriteString("f() {\n")
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&script, "  x=%d\n", i)
	}
	script.WriteString("}\nf; echo $x\nn=0; while read -r l; do n=$((n+1)); done <<EOF\n")
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&script, "%d\n", i)
	}
	script.WriteString("EOF\necho $n\n")
	stdout, stderr, err := runScriptInput(t, []string{"gosh"}, strings.NewReader(script.String()))
	if err != nil {
		t.Fatalf("Run failed: %v\nstderr: %s", err, stderr)
	}
	if got := strings.Fields(stdout); len(got) != 2 || got[0] != "19999" || got[1] != "20000" {
		t.Fatalf("stdout = %q, want 19999 and 20000", stdout)
	}
}
