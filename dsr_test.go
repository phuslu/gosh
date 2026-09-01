package gosh

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDSRReader struct {
	mu       sync.Mutex
	chunks   [][]byte
	deadline time.Time
}

func (r *fakeDSRReader) SetReadDeadline(t time.Time) error {
	r.mu.Lock()
	r.deadline = t
	r.mu.Unlock()
	return nil
}

func (r *fakeDSRReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		if !r.deadline.IsZero() {
			return 0, os.ErrDeadlineExceeded
		}
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

func newTestKeyBindingInput(src io.Reader, probe *dsrProbe) *keyBindingInput {
	return &keyBindingInput{
		src:   src,
		mgr:   &keyBindingManager{entries: make(map[string]*goKeyBindingEntry)},
		probe: probe,
	}
}

func TestDSRWriterArmsProbeAcrossWrites(t *testing.T) {
	probe := &dsrProbe{}
	var buf bytes.Buffer
	w := newDSRWriter(&buf, probe)

	w.Write([]byte("\x1b["))
	if probe.disarm() {
		t.Fatalf("probe armed before the full sequence arrived")
	}
	w.Write([]byte("6n"))
	if !probe.disarm() {
		t.Fatalf("probe was not armed after the full sequence")
	}
	if got, want := buf.String(), "\x1b[6n"; got != want {
		t.Fatalf("passthrough = %q, want %q", got, want)
	}
}

func TestKeyBindingInputAnswersDSRProbeOnTimeout(t *testing.T) {
	probe := &dsrProbe{}
	probe.arm()
	input := newTestKeyBindingInput(&fakeDSRReader{}, probe)

	buf := make([]byte, 32)
	n, err := input.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got, want := string(buf[:n]), "\x1b[1;1R"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if probe.disarm() {
		t.Fatalf("probe left armed after being answered")
	}
}

func TestKeyBindingInputPrefersRealDSRResponse(t *testing.T) {
	probe := &dsrProbe{}
	probe.arm()
	src := &fakeDSRReader{chunks: [][]byte{[]byte("\x1b[2;7R")}}
	input := newTestKeyBindingInput(src, probe)

	buf := make([]byte, 32)
	n, err := input.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got, want := string(buf[:n]), "\x1b[2;7R"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if probe.disarm() {
		t.Fatalf("probe left armed after the real response")
	}
}

func TestKeyBindingInputAnswersProbeAfterKeystrokes(t *testing.T) {
	probe := &dsrProbe{}
	probe.arm()
	src := &fakeDSRReader{chunks: [][]byte{[]byte("ls")}}
	input := newTestKeyBindingInput(src, probe)

	buf := make([]byte, 32)
	n, err := input.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got, want := string(buf[:n]), "ls\x1b[1;1R"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if probe.disarm() {
		t.Fatalf("probe left armed after being answered")
	}
}

func TestProbeableStdinBridgesNonFileReader(t *testing.T) {
	r := probeableStdin(strings.NewReader("hello"))
	if _, ok := r.(readDeadliner); !ok {
		t.Fatalf("bridged reader does not support deadlines")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if got, want := string(data), "hello"; got != want {
		t.Fatalf("data = %q, want %q", got, want)
	}
}

func TestProbeableStdinKeepsPollableFile(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	if got := probeableStdin(pr); got != pr {
		t.Fatalf("pollable file should be passed through unchanged")
	}
}
