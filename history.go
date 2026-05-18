package gosh

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"mvdan.cc/sh/v3/interp"
)

type history struct {
	limit   int
	control historyControl
	file    string
	mu      sync.Mutex
	entries []string
}

type historyControl struct {
	ignoreDups  bool
	ignoreSpace bool
}

func resolveHistoryLimit() int {
	val, _ := os.LookupEnv("HISTSIZE")
	if n := parseHistoryLimit(val); n > 0 {
		return n
	}
	return 1000
}

func resolveShellHistoryLimit(runner *interp.Runner) int {
	if val, ok := runnerStringVar(runner, "HISTSIZE"); ok {
		if n := parseHistoryLimit(val); n > 0 {
			return n
		}
	}
	return resolveHistoryLimit()
}

func parseHistoryLimit(val string) int {
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func resolveShellHistoryControl(runner *interp.Runner) historyControl {
	if val, ok := runnerStringVar(runner, "HISTCONTROL"); ok {
		return parseHistoryControl(val)
	}
	return parseHistoryControl(os.Getenv("HISTCONTROL"))
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

func resolveShellHistoryFile(runner *interp.Runner) string {
	histFile, ok := runnerStringVar(runner, "HISTFILE")
	if !ok {
		histFile, ok = os.LookupEnv("HISTFILE")
	}
	if !ok || histFile == os.DevNull || histFile == "/dev/null" {
		return ""
	}
	return histFile
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
	if h.control.ignoreSpace && strings.HasPrefix(line, " ") {
		h.mu.Unlock()
		return false
	}
	if h.control.ignoreDups && len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		h.mu.Unlock()
		return false
	}
	h.appendLocked(line)
	h.mu.Unlock()
	h.appendFile(line)
	return true
}

func (h *history) append(line string) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendLocked(line)
}

func (h *history) appendLocked(line string) {
	h.entries = append(h.entries, line)
	if h.limit > 0 && len(h.entries) > h.limit {
		h.entries = h.entries[len(h.entries)-h.limit:]
	}
}

func (h *history) Entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	copy(out, h.entries)
	return out
}

func (h *history) appendFile(line string) {
	if h == nil || h.file == "" {
		return
	}
	file, err := os.OpenFile(h.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, encodeHistoryLine(line))
	_ = file.Close()
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
