package gosh

import (
	"bufio"
	"errors"
	"io"
	"os"
)

// scriptSourceThreshold is the largest script kept entirely in memory before
// gosh spills the input to a seekable temporary file. Small interactive-sized
// scripts avoid disk work; large piped scripts avoid holding the whole input
// twice.
const scriptSourceThreshold = 1 << 20

// scriptSource abstracts the seekable byte source used by the non-interactive
// stream runner, along with the *os.File which the interpreter uses as stdin.
// Small sources stay in memory; large ones are mmapped from a temp file on
// supported platforms.
type scriptSource interface {
	Data() []byte
	StdinFile() (*os.File, error)
	Close() error
}

type memorySource struct {
	data []byte
	file *os.File
}

func (s *memorySource) Data() []byte { return s.data }

func (s *memorySource) StdinFile() (*os.File, error) {
	if s.file != nil {
		return s.file, nil
	}
	file, err := newStdinFile(s.data)
	if err != nil {
		return nil, err
	}
	s.file = file
	return file, nil
}

func (s *memorySource) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	closeErr := s.file.Close()
	removeErr := os.Remove(name)
	return errors.Join(closeErr, removeErr)
}

type fileSource struct {
	file          *os.File
	data          []byte
	unmap         func() error
	removeOnClose bool
}

func (s *fileSource) Data() []byte { return s.data }

func (s *fileSource) StdinFile() (*os.File, error) { return s.file, nil }

func (s *fileSource) Close() error {
	var errs []error
	if s.unmap != nil {
		errs = append(errs, s.unmap())
	}
	errs = append(errs, s.file.Close())
	if s.removeOnClose {
		errs = append(errs, os.Remove(s.file.Name()))
	}
	return errors.Join(errs...)
}

// openScriptSource consumes r into either a memory-backed or file-backed
// source. It special-cases regular *os.File inputs so large stdin scripts do
// not get copied a second time.
func openScriptSource(r io.Reader) (scriptSource, error) {
	if file, ok := r.(*os.File); ok {
		info, err := file.Stat()
		if err == nil && info.Mode().IsRegular() {
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			if info.Size() > scriptSourceThreshold {
				data, unmap, err := mapScriptFile(file, info.Size())
				if err == nil {
					return &fileSource{file: file, data: data, unmap: unmap}, nil
				}
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
			}
		}
	}

	br := bufio.NewReader(r)
	buf := make([]byte, scriptSourceThreshold+1)
	n, err := io.ReadFull(br, buf)
	switch err {
	case nil:
		// Read at least the threshold plus one byte; keep spooling below.
	case io.EOF, io.ErrUnexpectedEOF:
		return &memorySource{data: buf[:n]}, nil
	default:
		return nil, err
	}

	file, err := os.CreateTemp("", "gosh-script-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		file.Close()
		os.Remove(file.Name())
	}
	if _, err := file.Write(buf[:n]); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := io.Copy(file, br); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		cleanup()
		return nil, err
	}
	data, unmap, err := mapScriptFile(file, info.Size())
	if err != nil {
		cleanup()
		return nil, err
	}
	return &fileSource{file: file, data: data, unmap: unmap, removeOnClose: true}, nil
}
