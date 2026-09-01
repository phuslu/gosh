package gosh

import (
	"bytes"
	"io"
	"os"
	"sync"
	"time"
)

// ergochat/readline sends a DSR-CPR request ("\x1b[6n") to the terminal
// before printing the first prompt of every Readline call and blocks until
// the terminal replies with "\x1b[row;colR". Terminals that never answer
// (dumb terminals, `script` without a live terminal, some multiplexers)
// would therefore hang the session.
//
// gosh watches the stdout stream for the probe and answers it on the input
// side with a synthetic offset only when the terminal has not replied within
// dsrProbeTimeout. Real terminal responses always win, preserving the exact
// cursor offset where available; the fallback restores the pre-CPR behavior
// of assuming the prompt starts at column 1. The timeout doubles as the idle
// read-poll interval, so it bounds both the per-prompt delay in terminals
// that never answer and the wakeup rate while waiting for input. 50ms keeps
// dumb-terminal prompt latency low while still letting most real terminals
// (local and low-latency links) answer with their exact cursor position.
const dsrProbeTimeout = 50 * time.Millisecond

var (
	dsrProbeSequence = []byte("\x1b[6n")
	dsrProbeResponse = []byte("\x1b[1;1R")
)

// dsrProbe coordinates the DSR writer and the key binding input: the writer
// arms the probe when it sees the request, and the reader disarms it when it
// answers.
type dsrProbe struct {
	mu    sync.Mutex
	armed bool
}

func (p *dsrProbe) arm() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

func (p *dsrProbe) disarm() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	armed := p.armed
	p.armed = false
	return armed
}

// dsrWriter passes writes through and arms the probe when the DSR-CPR
// request sequence passes through, matching across Write boundaries.
type dsrWriter struct {
	dst   io.Writer
	probe *dsrProbe
	tail  []byte
}

func newDSRWriter(dst io.Writer, probe *dsrProbe) io.Writer {
	if dst == nil || probe == nil {
		return dst
	}
	return &dsrWriter{dst: dst, probe: probe}
}

func (w *dsrWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.scan(p[:n])
	}
	return n, err
}

func (w *dsrWriter) scan(p []byte) {
	w.tail = append(w.tail, p...)
	for len(w.tail) >= len(dsrProbeSequence) {
		if bytes.Equal(w.tail[:len(dsrProbeSequence)], dsrProbeSequence) {
			w.probe.arm()
		}
		w.tail = w.tail[1:]
	}
}

// containsCPRResponse reports whether b holds a DSR-CPR response
// ("\x1b[row;colR"). A loose match is intentional: a false negative only
// produces a redundant synthetic response, while a false positive would
// leave a non-responding terminal waiting forever.
func containsCPRResponse(b []byte) bool {
	for i := 0; i+2 < len(b); i++ {
		if b[i] != '\x1b' || b[i+1] != '[' {
			continue
		}
		hasSemi := false
		for j := i + 2; j < len(b); j++ {
			c := b[j]
			if c == ';' {
				hasSemi = true
				continue
			}
			if c == 'R' && hasSemi {
				return true
			}
			if c < '0' || c > '9' {
				break
			}
		}
	}
	return false
}

// probeableStdin returns an input stream whose reads respect deadlines. Go's
// os.File deadlines are not supported on terminal devices, so a
// terminal-backed stdin is bridged through an os.Pipe fed by a pump
// goroutine; the pipe read end then honors the deadlines used to answer DSR
// probes and disambiguate key prefixes.
func probeableStdin(stdin io.Reader) io.Reader {
	if f, ok := stdin.(*os.File); ok {
		if err := f.SetReadDeadline(time.Now()); err == nil {
			_ = f.SetReadDeadline(time.Time{})
			return stdin
		}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return stdin
	}
	go func() {
		_, _ = io.Copy(pw, stdin)
		_ = pw.Close()
	}()
	return pr
}
