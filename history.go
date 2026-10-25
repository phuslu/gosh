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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/pattern"
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
	// ignore is HISTIGNORE, a colon-separated list of patterns for lines
	// not to remember.
	ignore string
	// timestamps reports whether HISTTIMEFORMAT is set, in which case the
	// history file records when each entry was added and "history" shows
	// it formatted with timeFormat.
	timestamps bool
	timeFormat string
}

type history struct {
	cfg         historyConfig
	appendOnAdd func() bool
	resync      func()
	onError     func(error)
	mu          sync.Mutex
	entries     []historyEntry
	// base is the number of entries dropped from the front since the list
	// was last cleared, so that like Bash's history_base, entry i of
	// entries keeps the number base+i+1 when older entries are dropped.
	base int
	// fileLines counts the lines of the history file this shell has read
	// or written, so "history -n" reads only what other shells appended.
	fileLines int
	// lastAdded reports that the newest entry is a command line the shell
	// added, which "history -s" replaces.
	lastAdded bool
	expander  historyExpander
	// unsaved counts the entries added since the history file was last
	// written, like Bash's history_lines_this_session: "history -a" and
	// the end of the session append that many of the newest entries.
	unsaved int
}

// historyEntry is one remembered command line and when it was added, in
// seconds since the epoch.
type historyEntry struct {
	line string
	time int64
}

func historyLines(entries []historyEntry) []string {
	lines := make([]string, len(entries))
	for i, entry := range entries {
		lines[i] = entry.line
	}
	return lines
}

type historyControl struct {
	ignoreDups  bool
	ignoreSpace bool
	eraseDups   bool
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
	ignore, _ := runnerStringVar(runner, "HISTIGNORE")
	timeFormat, timestamps := runnerStringVar(runner, "HISTTIMEFORMAT")
	file, _ := runnerStringVar(runner, "HISTFILE")
	if file == os.DevNull || file == "/dev/null" {
		file = ""
	}
	return historyConfig{
		inMemoryLimit: parseHistoryLimit(size),
		fileLimit:     parseHistoryLimit(fileSize),
		control:       parseHistoryControl(control),
		file:          file,
		ignore:        ignore,
		timestamps:    timestamps,
		timeFormat:    timeFormat,
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
		case "erasedups":
			control.eraseDups = true
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
// returns the number of lines it has. Like Bash, a "#" line with only digits
// after it gives the time of the entry that follows; entries without one
// get the current time.
func (h *history) load(r io.Reader, skip int) (int, error) {
	br := bufio.NewReader(r)
	n := 0
	stamp := int64(-1)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			n++
			if n > skip {
				if t, ok := historyTimestamp(line); ok {
					stamp = t
				} else {
					if stamp < 0 {
						stamp = time.Now().Unix()
					}
					h.appendEntry(historyEntry{decodeHistoryLine(line), stamp})
					stamp = -1
				}
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

// historyTimestamp parses a history file line holding a timestamp, "#"
// followed by digits.
func historyTimestamp(line string) (int64, bool) {
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 2 || line[0] != '#' || line[1] < '0' || line[1] > '9' {
		return 0, false
	}
	t, err := strconv.ParseInt(line[1:], 10, 64)
	return t, err == nil
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
	entries := slices.Clone(h.entries[len(h.entries)-n:])
	h.mu.Unlock()
	if err := h.appendFile(entries...); err != nil {
		return err
	}
	h.mu.Lock()
	h.unsaved = 0
	h.mu.Unlock()
	return nil
}

// ReplaceLast stores line in place of the newest entry when that is the
// "history -s" command running, as "history -s" does, and adds it otherwise.
func (h *history) ReplaceLast(line string) {
	h.mu.Lock()
	if h.lastAdded && len(h.entries) > 0 {
		h.entries = h.entries[:len(h.entries)-1]
		h.unsaved = max(h.unsaved-1, 0)
	}
	h.lastAdded = false
	if h.appendLocked(historyEntry{line, time.Now().Unix()}) {
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
	if !h.controlAllowsLocked(line) {
		h.mu.Unlock()
		return false
	}
	if h.eraseDupsLocked(line) && h.resync != nil {
		// Readline must forget the erased entries too; it learns about
		// line from the caller.
		h.mu.Unlock()
		h.resync()
		h.mu.Lock()
	}
	entry := historyEntry{line, time.Now().Unix()}
	if !h.appendLocked(entry) {
		h.mu.Unlock()
		return false
	}
	h.lastAdded = true
	appendNow := h.shouldAppendOnAdd()
	if !appendNow {
		h.unsaved++
	}
	h.mu.Unlock()
	if appendNow {
		if err := h.appendFile(entry); err != nil && h.onError != nil {
			h.onError(err)
		}
	}
	return true
}

// append stores line as if it had been added now, without HISTCONTROL and
// without writing the history file.
func (h *history) append(line string) {
	h.appendEntry(historyEntry{line, time.Now().Unix()})
}

// appendEntry stores entry, read from a history file.
func (h *history) appendEntry(entry historyEntry) {
	line, ok := normalizeHistoryLine(entry.line)
	if !ok {
		return
	}
	entry.line = line
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendLocked(entry)
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
	prev := ""
	if len(h.entries) > 0 {
		prev = h.entries[len(h.entries)-1].line
	}
	if h.cfg.control.ignoreDups && len(h.entries) > 0 && prev == line {
		return false
	}
	return !historyIgnored(h.cfg.ignore, line, prev)
}

// historyIgnored reports whether line matches one of the HISTIGNORE
// patterns in ignore. Patterns are separated by colons, which a backslash
// escapes, must match the whole line, and "&" stands for prev, the previous
// history entry.
func historyIgnored(ignore, line, prev string) bool {
	if ignore == "" {
		return false
	}
	var pat strings.Builder
	check := func() bool {
		p := pat.String()
		pat.Reset()
		switch p {
		case "":
			return false
		case "&":
			return line == prev
		}
		expr, err := pattern.Regexp(p, pattern.EntireString)
		if err != nil {
			return false
		}
		rx, err := regexp.Compile(expr)
		return err == nil && rx.MatchString(line)
	}
	for i := 0; i < len(ignore); i++ {
		switch c := ignore[i]; {
		case c == '\\' && i+1 < len(ignore) && ignore[i+1] == ':':
			pat.WriteString(`\:`)
			i++
		case c == ':':
			if check() {
				return true
			}
		default:
			pat.WriteByte(c)
		}
	}
	return check()
}

// eraseDupsLocked removes the entries equal to line, for
// HISTCONTROL=erasedups, and reports whether it removed any. Like Bash, it
// leaves the count of the session's entries alone, so that the history file
// is rewritten at exit once more entries were erased than remain.
// h.mu must be held.
func (h *history) eraseDupsLocked(line string) bool {
	if !h.cfg.control.eraseDups {
		return false
	}
	n := len(h.entries)
	h.entries = slices.DeleteFunc(h.entries, func(entry historyEntry) bool { return entry.line == line })
	return len(h.entries) != n
}

// appendLocked stores line, honoring HISTSIZE, and reports whether it was
// stored. h.mu must be held.
func (h *history) appendLocked(entry historyEntry) bool {
	if h.cfg.inMemoryLimit == 0 {
		return false
	}
	h.entries = append(h.entries, entry)
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
	return historyLines(h.entries)
}

// Numbered returns the in-memory entries and the number of the first one.
func (h *history) Numbered() (first int, entries []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base + 1, historyLines(h.entries)
}

// Expand applies history expansion to line; see historyExpander.
func (h *history) Expand(line string) (out string, changed, printOnly bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expander.expand(line, historyLines(h.entries))
}

// Timed returns the in-memory entries with their times, the number of the
// first one and the HISTTIMEFORMAT to show the times with, if any.
func (h *history) Timed() (first int, entries []historyEntry, timeFormat string, timestamps bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base + 1, slices.Clone(h.entries), h.cfg.timeFormat, h.cfg.timestamps
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
	if pos < 0 {
		pos = len(h.entries) + pos + 1
	} else {
		pos -= h.base
	}
	if pos < 1 || pos > len(h.entries) {
		h.mu.Unlock()
		return false
	}
	if pos == len(h.entries) {
		h.lastAdded = false
	}
	h.entries = append(h.entries[:pos-1], h.entries[pos:]...)
	h.unsaved = max(h.unsaved-1, 0)
	h.mu.Unlock()
	// The up-arrow history must not bring the entry back.
	if h.resync != nil {
		h.resync()
	}
	return true
}

func (h *history) appendFile(entries ...historyEntry) error {
	if h == nil || h.cfg.file == "" || len(entries) == 0 {
		return nil
	}
	n, err := appendHistoryLines(h.cfg.file, h.cfg.timestamps, entries...)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.fileLines += n
	h.mu.Unlock()
	return nil
}

// appendHistoryLines appends entries to the history file name and returns
// the number of lines written. They go out in a single write, so the lines
// of shells sharing the file cannot interleave.
func appendHistoryLines(name string, timestamps bool, entries ...historyEntry) (int, error) {
	var buf bytes.Buffer
	n, err := writeHistoryLines(&buf, timestamps, entries...)
	if err != nil {
		return 0, err
	}
	file, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return 0, err
	}
	if _, err := file.Write(buf.Bytes()); err != nil {
		file.Close()
		return 0, err
	}
	return n, file.Close()
}

// writeHistoryLines writes entries to w in history-file format, one encoded
// entry per line, each after a "#seconds" line with its time when
// timestamps is set, and returns the number of lines written. It is shared
// by the append and rewrite paths.
func writeHistoryLines(w io.Writer, timestamps bool, entries ...historyEntry) (int, error) {
	n := 0
	for _, entry := range entries {
		if timestamps {
			if _, err := fmt.Fprintf(w, "#%d\n", entry.time); err != nil {
				return n, err
			}
			n++
		}
		if _, err := fmt.Fprintln(w, encodeHistoryLine(entry.line)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
	h.mu.Lock()
	entries := slices.Clone(h.entries)
	timestamps := h.cfg.timestamps
	h.mu.Unlock()
	lines := 0
	err := writeFileAtomic(name, func(w io.Writer) error {
		var err error
		lines, err = writeHistoryLines(w, timestamps, entries...)
		return err
	})
	if err != nil {
		return err
	}
	if name == h.cfg.file {
		h.mu.Lock()
		h.fileLines = lines
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
	lines, err := truncateHistoryFile(h.cfg.file, h.cfg.fileLimit)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.fileLines = min(h.fileLines, lines)
	h.mu.Unlock()
	return nil
}

// truncateHistoryFile keeps the last limit entries of the history file name
// and returns the number of lines left. A timestamp line stays with the
// entry after it.
func truncateHistoryFile(name string, limit int) (int, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if limit < 0 {
		return len(lines), nil
	}
	// Walk back over limit entries and their timestamps.
	start, entries := len(lines), 0
	for start > 0 && entries < limit {
		start--
		if _, ok := historyTimestamp(lines[start]); !ok {
			entries++
		}
	}
	for start > 0 && entries > 0 {
		if _, ok := historyTimestamp(lines[start-1]); !ok {
			break
		}
		start--
	}
	if start == 0 {
		return len(lines), nil
	}
	keep := lines[start:]
	out := strings.Join(keep, "\n")
	if len(keep) > 0 {
		out += "\n"
	}
	err = writeFileAtomic(name, func(w io.Writer) error {
		_, err := io.WriteString(w, out)
		return err
	})
	return len(keep), err
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
