package gosh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// scriptReader feeds a script to parser.InteractiveSeq, which parses and
// yields a line as soon as Read returns it, so each Read returns at most one
// line.
//
// When the script is also the stdin of its commands, as for "gosh < file",
// the reader must not consume input past the command being run, so that a
// command like "read" sees the lines after it, as in Bash. A seekable file
// is read in chunks and seeked back to the end of the parsed input before
// commands run; any other file is read one byte at a time.
type scriptReader struct {
	src      io.Reader
	shared   *os.File // the commands' stdin, when it is also the script
	bytewise bool
	buf      []byte
	start    int
	end      int
	midLine  bool // the last byte returned was not a newline

	synced   bool  // shared was seeked back to the parsed input by sync
	syncedAt int64 // the offset sync left shared at
}

const scriptReaderBufSize = 64 << 10

func newScriptReader(src io.Reader) *scriptReader {
	return &scriptReader{src: src, buf: make([]byte, scriptReaderBufSize)}
}

// newSharedScriptReader reads a script from file, which the script's
// commands also use as their stdin.
func newSharedScriptReader(file *os.File) *scriptReader {
	r := &scriptReader{src: file, shared: file}
	if info, err := file.Stat(); err == nil && info.Mode().IsRegular() {
		r.buf = make([]byte, scriptReaderBufSize)
	} else {
		r.bytewise = true
	}
	return r
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.bytewise {
		n := 0
		for n < len(p) {
			m, err := r.src.Read(p[n : n+1])
			n += m
			if m == 1 && p[n-1] == '\n' {
				break
			}
			if err != nil {
				if n > 0 {
					break
				}
				return r.finish(p, err)
			}
		}
		r.midLine = p[n-1] != '\n'
		return n, nil
	}
	if r.synced {
		if err := r.resume(); err != nil {
			return 0, err
		}
	}
	for r.start == r.end {
		n, err := r.src.Read(r.buf)
		r.start, r.end = 0, n
		if n == 0 && err != nil {
			return r.finish(p, err)
		}
	}
	chunk := r.buf[r.start:r.end]
	if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
		chunk = chunk[:i+1]
	}
	n := copy(p, chunk)
	r.start += n
	r.midLine = p[n-1] != '\n'
	return n, nil
}

// finish ends the input. A final line without a newline still ends a
// command at EOF, as in Bash, but the parser only completes a line once it
// sees the newline, so one is supplied.
func (r *scriptReader) finish(p []byte, err error) (int, error) {
	if err == io.EOF && r.midLine {
		r.midLine = false
		p[0] = '\n'
		return 1, nil
	}
	return 0, err
}

// sync positions the shared stdin right after the input parsed so far, so
// the commands about to run read what follows them in the script.
func (r *scriptReader) sync() error {
	if r.shared == nil || r.bytewise || r.start == r.end {
		return nil
	}
	pos, err := r.shared.Seek(-int64(r.end-r.start), io.SeekCurrent)
	if err != nil {
		return err
	}
	r.synced, r.syncedAt = true, pos
	return nil
}

// resume continues reading after the commands run since sync. If they left
// the file where sync did, the buffered input is still next and the file
// goes back to the end of the buffer; otherwise reading continues from
// wherever they stopped.
func (r *scriptReader) resume() error {
	r.synced = false
	pos, err := r.shared.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if pos != r.syncedAt {
		r.start, r.end = 0, 0
		return nil
	}
	_, err = r.shared.Seek(int64(r.end-r.start), io.SeekCurrent)
	return err
}

// scriptStdin returns stdin as an *os.File the script and its commands can
// share. Any other reader is copied into a pipe, which the returned
// function closes.
func scriptStdin(stdin io.Reader) (*os.File, func(), error) {
	if file, ok := stdin.(*os.File); ok {
		return file, func() {}, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	go func() {
		io.Copy(pw, stdin)
		pw.Close()
	}()
	// Closing the read end also stops the copy if the script ends before
	// its input does.
	return pr, func() { pr.Close() }, nil
}

// runScript parses and runs a script one complete command at a time, as
// Bash does: the commands before a syntax error still run, and a command
// defined or changed by one line affects how the following lines run. name
// is used in syntax errors.
func (s *Shell) runScript(ctx context.Context, r *scriptReader, name string) error {
	parser := syntax.NewParser()
	var lastStatus error
	for stmts, err := range parser.InteractiveSeq(r) {
		if err != nil {
			return withParseFilename(err, name)
		}
		if parser.Incomplete() || len(stmts) == 0 {
			continue
		}
		if err := r.sync(); err != nil {
			return err
		}
		// Running a File rather than bare statements keeps $0 set to name.
		err := s.runner.Run(ctx, &syntax.File{Name: name, Stmts: stmts})
		var status interp.ExitStatus
		switch {
		case err == nil:
			lastStatus = nil
		case errors.As(err, &status):
			lastStatus = err
		default:
			return err
		}
		if s.runner.Exited() {
			return lastStatus
		}
	}
	return lastStatus
}

// withParseFilename names the script in a syntax error; the interactive
// parser has no file name of its own.
func withParseFilename(err error, name string) error {
	switch e := err.(type) {
	case syntax.ParseError:
		if e.Filename == "" {
			e.Filename = name
		}
		return e
	case syntax.LangError:
		if e.Filename == "" {
			e.Filename = name
		}
		return e
	}
	return err
}
