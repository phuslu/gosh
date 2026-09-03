package gosh

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"mvdan.cc/sh/v3/interp"
)

// historyConfig groups the settings derived from the history-related shell
// variables for one history store. cmdhist and lithist are shopt options and
// are read from the shell option state instead of living here.
type historyConfig struct {
	// inMemoryLimit is HISTSIZE: negative means unlimited, zero disables
	// history, and a positive value keeps only that many recent entries.
	inMemoryLimit int
	// fileLimit is HISTFILESIZE: a non-negative value truncates the history
	// file to that many entries; a negative value means "no truncation".
	fileLimit int
	control   historyControl
	file      string
}

type history struct {
	cfg         historyConfig
	appendOnAdd func() bool
	resync      func()
	onError     func(error)
	mu          sync.Mutex
	entries     []string
	dirtyFile   bool
}

type historyControl struct {
	ignoreDups  bool
	ignoreSpace bool
}

// defaultHistoryLimit mirrors Bash's HISTSIZE default.
const defaultHistoryLimit = 500

// readlineHistoryLimit adapts gosh's in-memory limit to the in-tree readline
// configuration, whose zero value means "use its default cap". The gosh
// history store remains authoritative for search and the history builtins.
func readlineHistoryLimit(limit int) int {
	switch {
	case limit > 0:
		return limit
	case limit < 0:
		return 0
	default:
		return 1
	}
}

func resolveHistoryLimit() int {
	return defaultHistoryLimit
}

func resolveShellHistoryLimit(runner *interp.Runner) int {
	if val, ok := runnerStringVar(runner, "HISTSIZE"); ok {
		return parseHistorySize(val)
	}
	return resolveHistoryLimit()
}

// parseHistorySize follows Bash's startup behavior for HISTSIZE: a missing
// value means the default (handled by the caller), while a negative, empty,
// or non-numeric value means unlimited history.
func parseHistorySize(val string) int {
	val = strings.TrimSpace(val)
	if val == "" {
		return -1
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// parseHistoryFileLimit parses HISTFILESIZE. Any value that is not a plain
// non-negative integer means "do not truncate the history file".
func parseHistoryFileLimit(val string) int {
	val = strings.TrimSpace(val)
	if val == "" {
		return -1
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func resolveShellHistoryFileLimit(runner *interp.Runner) int {
	if val, ok := runnerStringVar(runner, "HISTFILESIZE"); ok {
		return parseHistoryFileLimit(val)
	}
	return -1
}

func resolveShellHistoryControl(runner *interp.Runner) historyControl {
	if val, ok := runnerStringVar(runner, "HISTCONTROL"); ok {
		return parseHistoryControl(val)
	}
	return historyControl{}
}

func parseHistoryControl(val string) historyControl {
	var control historyControl
	for _, part := range strings.FieldsFunc(val, func(r rune) bool {
		return r == ':' || r == ',' || unicode.IsSpace(r)
	}) {
		switch part {
		case "ignoredups":
			control.ignoreDups = true
		case "ignorespace":
			control.ignoreSpace = true
		case "ignoreboth":
			control.ignoreDups = true
			control.ignoreSpace = true
		}
	}
	return control
}

// resolveShellHistoryFile resolves HISTFILE. An explicit non-empty value
// wins, /dev/null disables file storage, and an unset HISTFILE falls back to
// $HOME/.gosh_history when HOME exists.
func resolveShellHistoryFile(runner *interp.Runner) string {
	histFile, ok := runnerStringVar(runner, "HISTFILE")
	if !ok {
		return defaultShellHistoryFile(runner)
	}
	if histFile == "" || histFile == os.DevNull || histFile == "/dev/null" {
		return ""
	}
	return histFile
}

func defaultShellHistoryFile(runner *interp.Runner) string {
	home, ok := runnerStringVar(runner, "HOME")
	if !ok || home == "" {
		return ""
	}
	return filepath.Join(home, ".gosh_history")
}

func runnerStringVar(runner *interp.Runner, name string) (string, bool) {
	if runner != nil && runner.Vars != nil {
		if vr, ok := runner.Vars[name]; ok && vr.IsSet() {
			return vr.String(), true
		}
	}
	if runner != nil && runner.Env != nil {
		if vr := runner.Env.Get(name); vr.IsSet() {
			return vr.String(), true
		}
	}
	return "", false
}

func (h *history) LoadFile(name string) error {
	if h == nil || name == "" {
		return nil
	}
	file, err := os.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	return h.Load(file)
}

func (h *history) Load(r io.Reader) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			h.append(decodeHistoryLine(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (h *history) Add(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return false
	}
	h.mu.Lock()
	if h.cfg.inMemoryLimit == 0 {
		h.mu.Unlock()
		return false
	}
	if h.cfg.control.ignoreSpace && strings.HasPrefix(line, " ") {
		h.mu.Unlock()
		return false
	}
	if h.cfg.control.ignoreDups && len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		h.mu.Unlock()
		return false
	}
	h.appendLocked(line)
	appendNow := h.shouldAppendOnAdd()
	if !appendNow {
		h.dirtyFile = true
	}
	h.mu.Unlock()
	if appendNow {
		if err := h.appendFile(line); err != nil && h.onError != nil {
			h.onError(err)
		}
	}
	return true
}

func (h *history) append(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.inMemoryLimit == 0 {
		return
	}
	h.appendLocked(line)
}

func (h *history) appendLocked(line string) {
	h.entries = append(h.entries, line)
	if h.cfg.inMemoryLimit > 0 && len(h.entries) > h.cfg.inMemoryLimit {
		h.entries = h.entries[len(h.entries)-h.cfg.inMemoryLimit:]
	}
}

func (h *history) Entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	copy(out, h.entries)
	return out
}

// Len returns the number of in-memory history entries.
func (h *history) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}

// Clear forgets all in-memory entries without touching the history file.
func (h *history) Clear() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.entries = h.entries[:0]
	h.dirtyFile = false
	h.mu.Unlock()
	if h.resync != nil {
		h.resync()
	}
}

// Delete removes the entry at pos (1-based; negative counts from the end)
// and reports whether pos was in range. The removal is persisted on the next
// rewrite of the history file.
func (h *history) Delete(pos int) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if pos < 0 {
		pos = len(h.entries) + pos + 1
	}
	if pos < 1 || pos > len(h.entries) {
		return false
	}
	h.entries = append(h.entries[:pos-1], h.entries[pos:]...)
	h.dirtyFile = true
	return true
}

func (h *history) appendFile(line string) error {
	if h == nil || h.cfg.file == "" {
		return nil
	}
	file, err := os.OpenFile(h.cfg.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(file, encodeHistoryLine(line)); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (h *history) shouldAppendOnAdd() bool {
	if h == nil || h.appendOnAdd == nil {
		return true
	}
	return h.appendOnAdd()
}

func (h *history) fileDirty() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dirtyFile
}

func (h *history) RewriteFile() error {
	if h == nil || h.cfg.file == "" {
		return nil
	}
	return h.WriteFile(h.cfg.file)
}

// WriteFile writes all entries to name (or the configured history file when
// name is empty), replacing its previous contents atomically.
func (h *history) WriteFile(name string) error {
	if h == nil {
		return nil
	}
	if name == "" {
		name = h.cfg.file
	}
	if name == "" {
		return fmt.Errorf("history: no history file")
	}
	entries := h.Entries()
	err := writeFileAtomic(name, func(w io.Writer) error {
		for _, entry := range entries {
			if _, err := fmt.Fprintln(w, encodeHistoryLine(entry)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if name == h.cfg.file {
		h.mu.Lock()
		h.dirtyFile = false
		h.mu.Unlock()
	}
	return nil
}

// ReadFile appends the entries from name (or the configured history file
// when name is empty) to the in-memory list and re-synchronizes the readline
// history.
func (h *history) ReadFile(name string) error {
	if h == nil {
		return nil
	}
	if name == "" {
		name = h.cfg.file
	}
	if name == "" {
		return fmt.Errorf("history: no history file")
	}
	if err := h.LoadFile(name); err != nil {
		return err
	}
	if h.resync != nil {
		h.resync()
	}
	return nil
}

// TruncateFile truncates the configured history file to at most HISTFILESIZE
// entries. Entries are stored one per physical line, so the entry count is
// the line count. Negative limits disable truncation.
func (h *history) TruncateFile() error {
	if h == nil || h.cfg.file == "" || h.cfg.fileLimit < 0 {
		return nil
	}
	return truncateHistoryFile(h.cfg.file, h.cfg.fileLimit)
}

func truncateHistoryFile(name string, limit int) error {
	if limit < 0 {
		return nil
	}
	data, err := os.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) <= limit {
		return nil
	}
	keep := lines[len(lines)-limit:]
	if limit == 0 {
		keep = nil
	}
	out := strings.Join(keep, "\n")
	if len(keep) > 0 {
		out += "\n"
	}
	return writeFileAtomic(name, func(w io.Writer) error {
		_, err := io.WriteString(w, out)
		return err
	})
}

// writeFileAtomic replaces name with the bytes produced by write. The content
// is staged in a temporary file in the same directory and then renamed over
// name, so an interrupted write never leaves a truncated or partial history
// file behind. The permissions of an existing file are preserved.
func writeFileAtomic(name string, write func(io.Writer) error) error {
	perm := fs.FileMode(0)
	if info, err := os.Stat(name); err == nil {
		perm = info.Mode().Perm()
	}
	dir, base := filepath.Split(name)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()
	buf := bufio.NewWriter(tmp)
	if err := write(buf); err != nil {
		tmp.Close()
		return err
	}
	if err := buf.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp always uses 0600; restore the mode of the file being
	// replaced. A history file created from scratch keeps 0600, which is
	// what bash uses as well.
	if perm != 0 {
		if err := os.Chmod(tmpName, perm); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, name); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

const historyEncodedPrefix = "# gosh-history-v1 "

func encodeHistoryLine(line string) string {
	if strings.ContainsAny(line, "\r\n") || strings.HasPrefix(line, historyEncodedPrefix) {
		return historyEncodedPrefix + base64.StdEncoding.EncodeToString([]byte(line))
	}
	return line
}

func decodeHistoryLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, historyEncodedPrefix) {
		return line
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len(historyEncodedPrefix):]))
	if err != nil {
		return line
	}
	return string(data)
}
