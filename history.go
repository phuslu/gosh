package gosh

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"mvdan.cc/sh/v3/expand"
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
	// base is the number of entries dropped from the front since the list
	// was last cleared, so that like Bash's history_base, entry i of
	// entries keeps the number base+i+1 when older entries are dropped.
	base int
	// fileLines counts the lines of the history file this shell has read
	// or written, so "history -n" reads only what other shells appended.
	fileLines int
	// unsaved counts the entries added since the history file was last
	// written, like Bash's history_lines_this_session: "history -a" and
	// the end of the session append that many of the newest entries.
	unsaved int
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
		return math.MaxInt32
	default:
		return 1
	}
}

// installHistoryDefaults gives the history variables that the startup files
// left unset Bash's defaults: HISTFILE names the default history file and
// HISTSIZE is 500. Once they are shell variables, unsetting one means what
// it does in Bash: no history file, or no limit. Unlike Bash, HISTFILESIZE
// stays unset, so an existing history file is never cut to HISTSIZE.
func installHistoryDefaults(runner *interp.Runner) {
	env, ok := runner.Env.(*shellEnviron)
	if !ok {
		return
	}
	set := func(name, value string) {
		_ = env.Set(name, expand.Variable{Set: true, Kind: expand.String, Str: value})
	}
	if _, ok := runnerStringVar(runner, "HISTFILE"); !ok {
		if file := defaultShellHistoryFile(runner); file != "" {
			set("HISTFILE", file)
		}
	}
	if _, ok := runnerStringVar(runner, "HISTSIZE"); !ok {
		set("HISTSIZE", strconv.Itoa(defaultHistoryLimit))
	}
}

// shellHistoryConfig reads the history settings from the shell variables as
// they are now. Unlike at startup, an unset HISTSIZE or HISTFILESIZE means
// no limit and an unset HISTFILE means no history file.
func shellHistoryConfig(runner *interp.Runner) historyConfig {
	size, _ := runnerStringVar(runner, "HISTSIZE")
	fileSize, _ := runnerStringVar(runner, "HISTFILESIZE")
	control, _ := runnerStringVar(runner, "HISTCONTROL")
	file, _ := runnerStringVar(runner, "HISTFILE")
	if file == os.DevNull || file == "/dev/null" {
		file = ""
	}
	return historyConfig{
		inMemoryLimit: parseHistoryLimit(size),
		fileLimit:     parseHistoryLimit(fileSize),
		control:       parseHistoryControl(control),
		file:          file,
	}
}

// configure applies cfg as Bash does when a history variable changes: a
// smaller HISTSIZE drops the oldest entries at once and a smaller
// HISTFILESIZE truncates the history file.
func (h *history) configure(cfg historyConfig) {
	h.mu.Lock()
	old := h.cfg
	h.cfg = cfg
	trimmed := false
	if limit := cfg.inMemoryLimit; limit >= 0 && len(h.entries) > limit {
		n := len(h.entries) - limit
		h.dropLocked(n)
		// Bash's stifle_history sets history_base to the number of entries
		// it dropped rather than adding to it, so the remaining entries are
		// numbered one lower than before; match its numbering.
		h.base = n - 1
		trimmed = true
	}
	h.mu.Unlock()
	if trimmed && h.resync != nil {
		h.resync()
	}
	if cfg.fileLimit != old.fileLimit || cfg.file != old.file {
		if err := h.TruncateFile(); err != nil && h.onError != nil {
			h.onError(err)
		}
	}
}

// parseHistoryLimit follows Bash's startup behavior for HISTSIZE and
// HISTFILESIZE: a missing value means the default (handled by the caller),
// while a negative, empty, or non-numeric value means "no limit" - unlimited
// history for HISTSIZE, no truncation for HISTFILESIZE.
func parseHistoryLimit(val string) int {
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

func defaultShellHistoryFile(runner *interp.Runner) string {
	home, ok := runnerStringVar(runner, "HOME")
	if !ok || home == "" {
		return ""
	}
	return filepath.Join(home, ".gosh_history")
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
	n, err := h.load(file, 0)
	if name == h.cfg.file {
		h.mu.Lock()
		h.fileLines = n
		h.mu.Unlock()
	}
	return err
}

func (h *history) Load(r io.Reader) error {
	_, err := h.load(r, 0)
	return err
}

// load appends the entries of a history file after its first skip lines and
// returns the number of lines it has.
func (h *history) load(r io.Reader, skip int) (int, error) {
	br := bufio.NewReader(r)
	n := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			n++
			if n > skip {
				h.append(decodeHistoryLine(line))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
	}
}

// ReadNew appends the lines that were added to the history file since this
// shell last read or wrote it, as "history -n" does.
func (h *history) ReadNew() error {
	if h == nil || h.cfg.file == "" {
		return fmt.Errorf("history: no history file")
	}
	file, err := os.Open(h.cfg.file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	h.mu.Lock()
	skip := h.fileLines
	h.mu.Unlock()
	n, err := h.load(file, skip)
	h.mu.Lock()
	h.fileLines = max(n, skip)
	// Like Bash, the lines read count as this session's and are appended
	// again at its end.
	h.unsaved += max(n-skip, 0)
	h.mu.Unlock()
	if h.resync != nil {
		h.resync()
	}
	return err
}

// AppendNew appends the entries added since the history file was last
// written to it, as "history -a" does.
func (h *history) AppendNew() error {
	if h == nil || h.cfg.file == "" {
		return fmt.Errorf("history: no history file")
	}
	h.mu.Lock()
	n := min(h.unsaved, len(h.entries))
	lines := slices.Clone(h.entries[len(h.entries)-n:])
	h.mu.Unlock()
	if err := h.appendFile(lines...); err != nil {
		return err
	}
	h.mu.Lock()
	h.unsaved = 0
	h.mu.Unlock()
	return nil
}

// ReplaceLast replaces the newest entry, which is the "history -s" command
// running, with line, as "history -s" does.
func (h *history) ReplaceLast(line string) {
	h.mu.Lock()
	if len(h.entries) > 0 {
		h.entries = h.entries[:len(h.entries)-1]
		h.unsaved = max(h.unsaved-1, 0)
	}
	if h.appendLocked(line) {
		h.unsaved++
	}
	h.mu.Unlock()
	if h.resync != nil {
		h.resync()
	}
}

func (h *history) Add(line string) bool {
	line, ok := normalizeHistoryLine(line)
	if !ok {
		return false
	}
	h.mu.Lock()
	if !h.controlAllowsLocked(line) || !h.appendLocked(line) {
		h.mu.Unlock()
		return false
	}
	appendNow := h.shouldAppendOnAdd()
	if !appendNow {
		h.unsaved++
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
	line, ok := normalizeHistoryLine(line)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendLocked(line)
}

// normalizeHistoryLine strips the trailing newline from line and reports
// whether what is left is worth remembering.
func normalizeHistoryLine(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	return line, strings.TrimSpace(line) != ""
}

// controlAllowsLocked reports whether HISTCONTROL lets line be remembered.
// h.mu must be held.
func (h *history) controlAllowsLocked(line string) bool {
	if h.cfg.control.ignoreSpace && strings.HasPrefix(line, " ") {
		return false
	}
	if h.cfg.control.ignoreDups && len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		return false
	}
	return true
}

// appendLocked stores line, honoring HISTSIZE, and reports whether it was
// stored. h.mu must be held.
func (h *history) appendLocked(line string) bool {
	if h.cfg.inMemoryLimit == 0 {
		return false
	}
	h.entries = append(h.entries, line)
	if h.cfg.inMemoryLimit > 0 && len(h.entries) > h.cfg.inMemoryLimit {
		h.dropLocked(len(h.entries) - h.cfg.inMemoryLimit)
	}
	return true
}

// dropLocked forgets the n oldest entries. h.mu must be held.
func (h *history) dropLocked(n int) {
	h.entries = append(h.entries[:0], h.entries[n:]...)
	h.base += n
}

func (h *history) Entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	copy(out, h.entries)
	return out
}

// Numbered returns the in-memory entries and the number of the first one.
func (h *history) Numbered() (first int, entries []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base + 1, slices.Clone(h.entries)
}

// Next returns the number the next entry will get, as shown by \! in
// prompts.
func (h *history) Next() int {
	if h == nil {
		return 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base + len(h.entries) + 1
}

// Clear forgets all in-memory entries without touching the history file.
func (h *history) Clear() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.entries = h.entries[:0]
	h.base = 0
	h.unsaved = 0
	h.mu.Unlock()
	if h.resync != nil {
		h.resync()
	}
}

// Delete removes the entry numbered pos (negative counts from the end) and
// reports whether pos was in range. Like Bash, the history file keeps the
// entry, and one fewer new entry is appended to it.
func (h *history) Delete(pos int) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if pos < 0 {
		pos = len(h.entries) + pos + 1
	} else {
		pos -= h.base
	}
	if pos < 1 || pos > len(h.entries) {
		return false
	}
	h.entries = append(h.entries[:pos-1], h.entries[pos:]...)
	h.unsaved = max(h.unsaved-1, 0)
	return true
}

func (h *history) appendFile(lines ...string) error {
	if h == nil || h.cfg.file == "" || len(lines) == 0 {
		return nil
	}
	if err := appendHistoryLines(h.cfg.file, lines...); err != nil {
		return err
	}
	h.mu.Lock()
	h.fileLines += len(lines)
	h.mu.Unlock()
	return nil
}

// appendHistoryLines appends entries to the history file name. They go out
// in a single write, so the lines of shells sharing the file cannot
// interleave.
func appendHistoryLines(name string, entries ...string) error {
	var buf bytes.Buffer
	if err := writeHistoryLines(&buf, entries...); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := file.Write(buf.Bytes()); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// writeHistoryLines writes entries to w in history-file format, one encoded
// entry per line. It is shared by the append and rewrite paths.
func writeHistoryLines(w io.Writer, entries ...string) error {
	for _, entry := range entries {
		if _, err := fmt.Fprintln(w, encodeHistoryLine(entry)); err != nil {
			return err
		}
	}
	return nil
}

func (h *history) shouldAppendOnAdd() bool {
	if h == nil || h.appendOnAdd == nil {
		return true
	}
	return h.appendOnAdd()
}

// SaveOnExit saves the history at the end of the session as Bash does: it
// appends the entries added during the session, so that the lines other
// shells appended meanwhile are kept, unless HISTSIZE dropped some of them,
// in which case the file is rewritten with the list.
func (h *history) SaveOnExit() error {
	if h == nil || h.cfg.file == "" {
		return nil
	}
	h.mu.Lock()
	n, stifled := h.unsaved, h.unsaved > len(h.entries)
	h.mu.Unlock()
	switch {
	case n == 0:
		return nil
	case stifled:
		return h.RewriteFile()
	default:
		return h.AppendNew()
	}
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
		return writeHistoryLines(w, entries...)
	})
	if err != nil {
		return err
	}
	if name == h.cfg.file {
		h.mu.Lock()
		h.fileLines = len(entries)
		h.unsaved = 0
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
	if err := truncateHistoryFile(h.cfg.file, h.cfg.fileLimit); err != nil {
		return err
	}
	h.mu.Lock()
	h.fileLines = min(h.fileLines, h.cfg.fileLimit)
	h.mu.Unlock()
	return nil
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
