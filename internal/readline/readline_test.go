package readline

import (
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
