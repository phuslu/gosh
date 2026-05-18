package gosh

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/chzyer/readline"
)

type historySearch struct {
	history *history
	rl      *readline.Instance

	mu           sync.Mutex
	line         []rune
	pos          int
	searchActive bool
	searchPrefix string
	searchPos    int
	searchEmpty  bool
	searchIndex  int
	historySize  int
	setBuffer    func(*readline.Instance, []rune, int) bool
}

func (h *historySearch) Attach(rl *readline.Instance) {
	h.mu.Lock()
	h.rl = rl
	h.mu.Unlock()
}

func (h *historySearch) Search(action rune) bool {
	if h.applySearch(action) {
		return true
	}
	h.emitBell()
	return true
}

func (h *historySearch) OnChange(line []rune, pos int, _ rune) (newLine []rune, newPos int, ok bool) {
	h.mu.Lock()
	h.line = append(h.line[:0], line...)
	if pos < 0 {
		pos = 0
	} else if pos > len(line) {
		pos = len(line)
	}
	h.pos = pos
	h.resetSearchLocked()
	h.mu.Unlock()
	return nil, 0, false
}

func (h *historySearch) resetSearch() {
	h.mu.Lock()
	h.resetSearchLocked()
	h.mu.Unlock()
}

func (h *historySearch) resetSearchLocked() {
	h.searchActive = false
	h.searchPrefix = ""
	h.searchPos = 0
	h.searchEmpty = false
	h.historySize = 0
	h.searchIndex = -1
}

func (h *historySearch) applySearch(action rune) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.history == nil || h.rl == nil && h.setBuffer == nil {
		return false
	}
	entries := h.history.Entries()
	if !h.searchActive || len(entries) != h.historySize {
		h.searchPrefix = h.currentPrefixLocked()
		h.searchPos = h.pos
		h.searchEmpty = h.searchPos == 0 && h.searchPrefix == "" && len(h.line) == 0
		h.searchActive = true
		h.historySize = len(entries)
		if action == keyActionHistorySearchBackward {
			h.searchIndex = len(entries)
		} else {
			h.searchIndex = -1
		}
	}
	if len(entries) == 0 {
		return false
	}
	start := h.searchIndex
	var candidate string
	if action == keyActionHistorySearchBackward {
		for idx := start - 1; idx >= 0; idx-- {
			if strings.HasPrefix(entries[idx], h.searchPrefix) {
				candidate = entries[idx]
				h.searchIndex = idx
				break
			}
		}
	} else {
		for idx := start + 1; idx < len(entries); idx++ {
			if strings.HasPrefix(entries[idx], h.searchPrefix) {
				candidate = entries[idx]
				h.searchIndex = idx
				break
			}
		}
	}
	if candidate == "" {
		return false
	}
	runes := []rune(candidate)
	pos := h.searchPos
	if h.searchEmpty {
		pos = len(runes)
	} else if pos < 0 {
		pos = 0
	} else if pos > len(runes) {
		pos = len(runes)
	}
	if !h.setReadlineBuffer(runes, pos) {
		return false
	}
	h.line = append(h.line[:0], runes...)
	h.pos = pos
	return true
}

func (h *historySearch) setReadlineBuffer(line []rune, pos int) bool {
	if h.setBuffer != nil {
		return h.setBuffer(h.rl, line, pos)
	}
	return setReadlineBuffer(h.rl, line, pos)
}

func setReadlineBuffer(rl *readline.Instance, line []rune, pos int) bool {
	if rl == nil || rl.Operation == nil {
		return false
	}
	if pos < 0 {
		pos = 0
	} else if pos > len(line) {
		pos = len(line)
	}
	op := reflect.ValueOf(rl.Operation)
	if !op.IsValid() || op.Kind() != reflect.Pointer || op.IsNil() {
		return false
	}
	bufField := op.Elem().FieldByName("buf")
	if !bufField.IsValid() || !bufField.CanAddr() || bufField.Kind() != reflect.Pointer || bufField.IsNil() {
		return false
	}
	buf, ok := reflect.NewAt(bufField.Type(), unsafe.Pointer(bufField.UnsafeAddr())).Elem().Interface().(*readline.RuneBuffer)
	if !ok || buf == nil {
		return false
	}
	line = append([]rune(nil), line...)
	if pos == len(line) || !readlineBufferInteractive(buf) {
		buf.SetWithIdx(pos, line)
		return true
	}
	width := readlineBufferWidth(buf)
	if width <= 0 {
		buf.SetWithIdx(pos, line)
		return true
	}
	// chzyer/readline moves back from the end with one backspace per rune.
	// Across wrapped long lines that can overrun on some terminals, so draw the
	// full match at the end and then reposition by row and column.
	promptWidth := buf.PromptLen()
	buf.SetWithIdx(len(line), line)
	if seq := cursorMoveFromEndSequence(promptWidth, line, pos, width); len(seq) > 0 && rl.Terminal != nil {
		_, _ = rl.Terminal.Write(seq)
	}
	buf.Lock()
	ok = setReadlineBufferIndexLocked(buf, pos)
	buf.Unlock()
	if !ok {
		buf.SetWithIdx(pos, line)
	}
	return true
}

func readlineBufferInteractive(buf *readline.RuneBuffer) bool {
	v := reflect.ValueOf(buf)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return false
	}
	field := v.Elem().FieldByName("interactive")
	return field.IsValid() && field.Kind() == reflect.Bool && field.Bool()
}

func readlineBufferWidth(buf *readline.RuneBuffer) int {
	v := reflect.ValueOf(buf)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return 0
	}
	field := v.Elem().FieldByName("width")
	if !field.IsValid() || field.Kind() != reflect.Int {
		return 0
	}
	return int(field.Int())
}

func setReadlineBufferIndexLocked(buf *readline.RuneBuffer, pos int) bool {
	v := reflect.ValueOf(buf)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return false
	}
	field := v.Elem().FieldByName("idx")
	if !field.IsValid() || !field.CanAddr() || field.Kind() != reflect.Int {
		return false
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().SetInt(int64(pos))
	return true
}

func cursorMoveFromEndSequence(promptWidth int, line []rune, pos, width int) []byte {
	if width <= 0 || pos >= len(line) {
		return nil
	}
	if pos < 0 {
		pos = 0
	}
	runes := readline.Runes{}
	targetRow, targetCol := terminalRowCol(promptWidth+runes.WidthAll(line[:pos]), width)
	endRow, _ := terminalRowCol(promptWidth+runes.WidthAll(line), width)
	if endRow < targetRow {
		return nil
	}
	var buf bytes.Buffer
	if rows := endRow - targetRow; rows > 0 {
		fmt.Fprintf(&buf, "\x1b[%dA", rows)
	}
	buf.WriteByte('\r')
	if targetCol > 0 {
		fmt.Fprintf(&buf, "\x1b[%dC", targetCol)
	}
	return buf.Bytes()
}

func terminalRowCol(cells, width int) (int, int) {
	if width <= 0 {
		return 0, cells
	}
	return cells / width, cells % width
}

func (h *historySearch) currentPrefixLocked() string {
	line := h.line
	pos := h.pos
	if pos < 0 {
		pos = 0
	} else if pos > len(line) {
		pos = len(line)
	}
	return string(line[:pos])
}

func (h *historySearch) emitBell() {
	h.mu.Lock()
	rl := h.rl
	h.mu.Unlock()
	if rl == nil {
		return
	}
	if rl.Terminal != nil {
		_, _ = rl.Terminal.Write([]byte{0x07})
		return
	}
	if rl.Config != nil && rl.Config.Stdout != nil {
		_, _ = rl.Config.Stdout.Write([]byte{0x07})
	}
}
