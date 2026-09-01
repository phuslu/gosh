package readline

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/phuslu/gosh/internal/readline/internal/ansi"
	"github.com/phuslu/gosh/internal/readline/internal/platform"
)

const (
	maxAnsiLen = 32
)

var (
	deadlineExceeded = errors.New("deadline exceeded")
)

/*
terminal manages terminal input. The design constraints here are somewhat complex:

1. Calls to (*Instance).Readline() must always be preemptible by (*Instance).Close.
   This could be handled at the Operation layer instead; however, it's cleaner
   to provide an API in terminal itself that can interrupt attempts to read.
2. In between calls to Readline(), or *after* a call to (*Instance).Close(),
   stdin must be available for code outside of this library to read from. The
   problem is that reads from stdin in Go are not preemptible (see, for example,
   https://github.com/golang/go/issues/24842 ). In the worst case, an
   interrupted read will leave (*terminal).ioloop() running, and it will
   consume one more user keystroke before it exits. However, it is a design goal
   to read as little as possible at a time.
Accordingly, the concurrency design is as follows:

1. ioloop() runs asynchronously. It operates in lockstep with the read methods:
   each synchronous receive from kickChan is matched with a synchronous send to
   outChan. It does blocking reads from stdin, reading as little as possible at
   a time, and passing the results back over outChan.
2. Close() can be called asynchronously. It interrupts ioloop() (unless ioloop()
   is actually reading from stdin, in which case it interrupts it after the next
   keystroke), and also interrupts any in-progress GetRune() call. It is
   idempotent and can be called multiple times.
*/

type terminal struct {
	cfg        atomic.Pointer[Config]
	dimensions atomic.Pointer[termDimensions]
	closeOnce  sync.Once
	closeErr   error
	outChan    chan readResult
	kickChan   chan struct{}
	stopChan   chan struct{}
	inFlight   bool // tracks whether we initiated a read and then gave up waiting
	sleeping   int32
}

// termDimensions stores the terminal width and height (-1 means unknown)
type termDimensions struct {
	width  int
	height int
}

// readResult represents the result of a single "read operation" from the
// perspective of terminal. it may be a pure no-op. the consumer needs to
// read again if it didn't get what it wanted
type readResult struct {
	r  rune
	ok bool // is `r` valid user input? if not, we may need to read again
}

func newTerminal(cfg *Config) (*terminal, error) {
	if cfg.isInteractive {
		if ansiErr := ansi.EnableANSI(); ansiErr != nil {
			return nil, fmt.Errorf("Could not enable ANSI escapes: %w", ansiErr)
		}
	}
	t := &terminal{
		kickChan: make(chan struct{}),
		outChan:  make(chan readResult),
		stopChan: make(chan struct{}),
	}
	t.SetConfig(cfg)
	// Get and cache the current terminal size.
	t.OnSizeChange()

	go t.ioloop()
	return t, nil
}

// SleepToResume will sleep myself, and return only if I'm resumed.
func (t *terminal) SleepToResume() {
	if !atomic.CompareAndSwapInt32(&t.sleeping, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&t.sleeping, 0)

	t.ExitRawMode()
	platform.SuspendProcess()
	t.EnterRawMode()
}

func (t *terminal) EnterRawMode() (err error) {
	return t.GetConfig().FuncMakeRaw()
}

func (t *terminal) ExitRawMode() (err error) {
	return t.GetConfig().FuncExitRaw()
}

func (t *terminal) Write(b []byte) (int, error) {
	return t.GetConfig().Stdout.Write(b)
}

func (t *terminal) GetRune(deadline chan struct{}) (rune, error) {
	return t.getRuneFromStdin(deadline)
}

func (t *terminal) getRuneFromStdin(deadline chan struct{}) (rune, error) {
	for {
		result, err := t.readFromStdin(deadline)
		if err != nil {
			return 0, err
		} else if result.ok {
			return result.r, nil
		} // else: something we didn't understand, read again
	}
}

func (t *terminal) readFromStdin(deadline chan struct{}) (result readResult, err error) {
	// we may have sent a kick previously and given up on the response;
	// if so, don't kick again (we will try again to read the pending response)
	if !t.inFlight {
		select {
		case t.kickChan <- struct{}{}:
			t.inFlight = true
		case <-t.stopChan:
			return result, io.EOF
		case <-deadline:
			return result, deadlineExceeded
		}
	}

	select {
	case result = <-t.outChan:
		t.inFlight = false
		return result, nil
	case <-t.stopChan:
		return result, io.EOF
	case <-deadline:
		return result, deadlineExceeded
	}
}

func (t *terminal) ioloop() {
	// ensure close if we get an error from stdio
	defer t.Close()

	buf := bufio.NewReader(t.GetConfig().Stdin)
	var ansiBuf bytes.Buffer

	for {
		select {
		case <-t.kickChan:
		case <-t.stopChan:
			return
		}

		r, _, err := buf.ReadRune()
		if err != nil {
			return
		}

		var result readResult
		if r == '\x1b' {
			// we're starting an ANSI escape sequence:
			// keep reading until we reach the end of the sequence
			result, err = t.consumeANSIEscape(buf, &ansiBuf)
			if err != nil {
				return
			}
		} else {
			result = readResult{r: r, ok: true}
		}

		select {
		case t.outChan <- result:
		case <-t.stopChan:
			return
		}
	}
}

func (t *terminal) consumeANSIEscape(buf *bufio.Reader, ansiBuf *bytes.Buffer) (result readResult, err error) {
	ansiBuf.Reset()
	initial, _, err := buf.ReadRune()
	if err != nil {
		return
	}
	// we already read one \x1b. this can indicate either the start of an ANSI
	// escape sequence, or a keychord with Alt (e.g. Alt+f produces `\x1bf` in
	// a typical xterm).
	switch initial {
	case 'f':
		// Alt-f in xterm, or Option+RightArrow in iTerm2 with "Natural text editing"
		return readResult{r: MetaForward, ok: true}, nil // Alt-f
	case 'b':
		// Alt-b in xterm, or Option+LeftArrow in iTerm2 with "Natural text editing"
		return readResult{r: MetaBackward, ok: true}, nil // Alt-b
	case '\b', 0x7f:
		// Alt-Backspace in many terminals is sent as ESC + Backspace or ESC + DEL.
		// Treat this as word-wise backspace.
		return readResult{r: MetaBackspace, ok: true}, nil
	case '[', 'O':
		// this is a real ANSI escape sequence, read the rest of the sequence below:
	case '\x1b':
		// Alt plus a real ANSI escape sequence. Handle this specially since
		// right now the only cases we want to handle are the arrow keys:
		return consumeAltSequence(buf)
	default:
		return // invalid, ignore
	}

	// data consists of ; and 0-9 , anything else terminates the sequence
	var type_ rune
	for {
		r, _, err := buf.ReadRune()
		if err != nil {
			return result, err
		}
		if r == ';' || ('0' <= r && r <= '9') {
			ansiBuf.WriteRune(r)
		} else {
			type_ = r
			break
		}
	}

	var r rune
	switch type_ {
	case 'D':
		if altModifierEnabled(ansiBuf.Bytes()) {
			r = MetaBackward
		} else {
			r = CharBackward
		}
	case 'C':
		if altModifierEnabled(ansiBuf.Bytes()) {
			r = MetaForward
		} else {
			r = CharForward
		}
	case 'A':
		r = CharPrev
	case 'B':
		r = CharNext
	case 'H':
		r = CharLineStart
	case 'F':
		r = CharLineEnd
	case '~':
		if initial == '[' {
			switch string(ansiBuf.Bytes()) {
			case "3":
				r = MetaDeleteKey // this is the key typically labeled "Delete"
			case "1", "7":
				r = CharLineStart // "Home" key
			case "4", "8":
				r = CharLineEnd // "End" key
			}
		}
	case 'Z':
		if initial == '[' {
			r = MetaShiftTab
		}
	}

	if r != 0 {
		return readResult{r: r, ok: true}, nil
	}
	return // default: no interpretable rune value
}

func consumeAltSequence(buf *bufio.Reader) (result readResult, err error) {
	initial, _, err := buf.ReadRune()
	if err != nil {
		return
	}
	if initial != '[' {
		return
	}
	second, _, err := buf.ReadRune()
	if err != nil {
		return
	}
	switch second {
	case 'D':
		return readResult{r: MetaBackward, ok: true}, nil
	case 'C':
		return readResult{r: MetaForward, ok: true}, nil
	default:
		return
	}
}

func altModifierEnabled(payload []byte) bool {
	// https://www.xfree86.org/current/ctlseqs.html ; modifier keycodes
	// go after the semicolon, e.g. Alt-LeftArrow is `\x1b[1;3D` in VTE
	// terminals, where 3 indicates Alt. Use the same word-wise behavior for
	// Ctrl-Left/Right (modifier 5) and Alt+Ctrl (modifier 7), since readline
	// doesn't distinguish them at a higher level.
	if semicolonIdx := bytes.IndexByte(payload, ';'); semicolonIdx != -1 {
		switch string(payload[semicolonIdx+1:]) {
		case "3", "5", "7":
			return true
		}
	}
	return false
}

func (t *terminal) Bell() {
	t.Write([]byte{CharBell})
}

func (t *terminal) Close() error {
	t.closeOnce.Do(func() {
		close(t.stopChan)
		// don't close outChan; outChan results should always be valid.
		// instead we always select on both outChan and stopChan
		t.closeErr = t.ExitRawMode()
	})
	return t.closeErr
}

func (t *terminal) SetConfig(c *Config) error {
	t.cfg.Store(c)
	return nil
}

func (t *terminal) GetConfig() *Config {
	return t.cfg.Load()
}

// OnSizeChange gets the current terminal size and caches it
func (t *terminal) OnSizeChange() {
	cfg := t.GetConfig()
	width, height := cfg.FuncGetSize()
	t.dimensions.Store(&termDimensions{
		width:  width,
		height: height,
	})
}

// GetWidthHeight returns the cached width, height values from the terminal
func (t *terminal) GetWidthHeight() (width, height int) {
	dimensions := t.dimensions.Load()
	return dimensions.width, dimensions.height
}
