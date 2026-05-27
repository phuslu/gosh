package gosh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chzyer/readline"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func runInteractiveParser(parser *syntax.Parser, r io.Reader, run func([]*syntax.Stmt) bool, handleError func(error) bool) error {
	for {
		err := parser.Interactive(r, run)
		if err == nil {
			return nil
		}
		if handleError == nil || !handleError(err) {
			return err
		}
	}
}

func runNonInteractiveStream(ctx context.Context, r io.Reader, runner *interp.Runner, stdout, stderr io.Writer) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var runErr error
	var lastStatus error
	for offset := 0; offset < len(data); {
		stmts, next, err := parseNextStatements(data, offset)
		if err != nil {
			return err
		}
		if next <= offset {
			break
		}
		stdin, err := remainingStdinFile(data[next:])
		if err != nil {
			return err
		}
		if err := interp.StdIO(stdin, stdout, stderr)(runner); err != nil {
			stdin.Close()
			os.Remove(stdin.Name())
			return err
		}
		for _, stmt := range stmts {
			err := runner.Run(ctx, stmt)
			if err == nil {
				lastStatus = nil
			} else {
				var status interp.ExitStatus
				if errors.As(err, &status) {
					lastStatus = err
				} else {
					runErr = err
					break
				}
			}
			if runner.Exited() {
				break
			}
		}
		pos, seekErr := stdin.Seek(0, io.SeekCurrent)
		stdin.Close()
		os.Remove(stdin.Name())
		if seekErr != nil {
			return seekErr
		}
		offset = next + int(pos)
		if runErr != nil {
			return runErr
		}
		if runner.Exited() {
			return lastStatus
		}
	}
	return lastStatus
}

func runInteractiveStatements(ctx context.Context, runner *interp.Runner, stmts []*syntax.Stmt, stderr io.Writer) (bool, error) {
	for _, stmt := range stmts {
		err := runner.Run(ctx, stmt)
		if runner.Exited() {
			return false, err
		}
		if err != nil {
			var status interp.ExitStatus
			if errors.As(err, &status) {
				continue
			}
			fmt.Fprintln(stderr, err.Error())
		}
	}
	return true, nil
}

func parseNextStatements(data []byte, offset int) ([]*syntax.Stmt, int, error) {
	for next := offset; next < len(data); {
		if idx := bytes.IndexByte(data[next:], '\n'); idx >= 0 {
			next += idx + 1
		} else {
			next = len(data)
		}
		var out []*syntax.Stmt
		parser := syntax.NewParser()
		err := parser.Interactive(bytes.NewReader(data[offset:next]), func(stmts []*syntax.Stmt) bool {
			if parser.Incomplete() {
				return true
			}
			out = append(out, stmts...)
			return false
		})
		if err != nil {
			if parser.Incomplete() || next < len(data) {
				continue
			}
			return nil, next, err
		}
		if !parser.Incomplete() {
			return out, next, nil
		}
	}
	return nil, len(data), io.ErrUnexpectedEOF
}

func remainingStdinFile(data []byte) (*os.File, error) {
	file, err := os.CreateTemp("", "gosh-stdin-*")
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

// reader adapts *readline.Instance to the io.Reader interface expected by
// parser.Interactive. The parser calls Read whenever it needs more input.
type reader struct {
	rl                  *readline.Instance
	buf                 []byte // leftover bytes from the previous Readline call
	history             *history
	pendingHistoryLines []string
}

func (r *reader) Read(p []byte) (int, error) {
	// Drain any bytes that did not fit into p on the previous call.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	line, err := r.rl.Readline()
	if err != nil {
		if err == readline.ErrInterrupt {
			// Ctrl-C: feed a bare newline so the parser discards the current
			// incomplete statement and returns to a clean state.
			r.pendingHistoryLines = r.pendingHistoryLines[:0]
			p[0] = '\n'
			return 1, nil
		}
		// Ctrl-D or any other error: signal EOF to shut down the parser loop.
		return 0, io.EOF
	}

	r.pendingHistoryLines = append(r.pendingHistoryLines, line)
	data := []byte(line + "\n")
	n := copy(p, data)
	if n < len(data) {
		r.buf = data[n:] // stash the remainder for the next Read call
	}
	return n, nil
}

func (r *reader) savePendingHistory() {
	if len(r.pendingHistoryLines) == 0 {
		return
	}
	line := historyLine(r.pendingHistoryLines)
	r.pendingHistoryLines = r.pendingHistoryLines[:0]
	r.saveHistoryLine(line)
}

func (r *reader) saveHistoryLine(line string) {
	if r.rl == nil {
		if r.history != nil {
			r.history.Add(line)
		}
		return
	}
	if r.history == nil {
		_ = r.rl.SaveHistory(line)
		return
	}
	if r.history.Add(line) {
		_ = r.rl.SaveHistory(line)
		return
	}
	_ = r.rl.SaveHistory("")
}

func historyLine(lines []string) string {
	return strings.Join(lines, "\n")
}
