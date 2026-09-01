package readline

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRace(t *testing.T) {
	rl, err := NewFromConfig(&Config{})
	if err != nil {
		t.Fatal(err)
		return
	}

	go func() {
		for range time.Tick(time.Millisecond) {
			rl.SetPrompt("hello")
		}
	}()

	go func() {
		time.Sleep(100 * time.Millisecond)
		rl.Close()
	}()

	rl.Readline()
}

func TestSetBufferWithCursor(t *testing.T) {
	rl, err := NewFromConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	rl.SetBufferWithCursor("héllo", 2)
	if got := string(rl.operation.buf.buf); got != "héllo" {
		t.Fatalf("buffer = %q, want %q", got, "héllo")
	}
	if idx := rl.operation.buf.idx; idx != 2 {
		t.Fatalf("cursor = %d, want 2", idx)
	}

	rl.SetBufferWithCursor("abc", -1)
	if idx := rl.operation.buf.idx; idx != 0 {
		t.Fatalf("negative pos clamped to %d, want 0", idx)
	}

	rl.SetBufferWithCursor("abc", 100)
	if idx := rl.operation.buf.idx; idx != 3 {
		t.Fatalf("oversized pos clamped to %d, want 3", idx)
	}
}

func TestEscapeBackspaceAndCtrlModifierMapping(t *testing.T) {
	for _, seq := range []string{"\x1b\x08", "\x1b\x7f"} {
		buf := bufio.NewReader(strings.NewReader(seq))
		if r, _, err := buf.ReadRune(); err != nil || r != '\x1b' {
			t.Fatalf("could not consume ESC for %q: %v", seq, err)
		}
		result, err := (&terminal{}).consumeANSIEscape(buf, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("consumeANSIEscape(%q) failed: %v", seq, err)
		}
		if !result.ok || result.r != MetaBackspace {
			t.Fatalf("consumeANSIEscape(%q) = %#v, want MetaBackspace", seq, result)
		}
	}

	for payload, want := range map[string]bool{
		"1;2": false,
		"1;3": true,
		"1;5": true,
		"1;7": true,
	} {
		if got := altModifierEnabled([]byte(payload)); got != want {
			t.Fatalf("altModifierEnabled(%q) = %v, want %v", payload, got, want)
		}
	}
}
