package gosh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phuslu/gosh/internal/readline"
	"mvdan.cc/sh/v3/interp"
)

type keyBindingManager struct {
	mu      sync.RWMutex
	entries map[string]*goKeyBindingEntry
	actions map[rune]func(rune) bool
	// runCommand runs the shell command of a "bind -x" binding; see
	// Shell.runBindCommand.
	runCommand func(command string)
}

type goKeyBindingEntry struct {
	seq    []byte
	action rune
	// command is the shell command a "bind -x" binding runs, with action
	// keyActionShellCommand.
	command string
}

const (
	keyActionHistorySearchBackward = 0x90
	keyActionHistorySearchForward  = 0x91
)

const bindUsage = "bind: usage: bind [-lpsvPSVX] [-m keymap] [-f filename] [-q name] [-u name] [-r keyseq] [-x keyseq:shell-command] [keyseq:readline-function or readline-command]"

// handleBind implements the bind builtin: -l lists the functions, -p and
// -P the bindings as input or as sentences, -q asks for one function's
// keys, -u unbinds a function, -r a key sequence, -f reads bindings from a
// file, and each other argument binds a key sequence, "keyseq: function",
// or sets a readline variable, "set name value", which gosh accepts and
// ignores. With no arguments it lists the bindings like -p.
func (m *keyBindingManager) handleBind(args []string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if len(args) == 0 {
		m.printBindings(out)
		return nil
	}
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		arg := args[0]
		args = args[1:]
		if arg == "--" {
			break
		}
		for k := 1; k < len(arg); k++ {
			opt := arg[k]
			// value takes the option's argument, the rest of this word or
			// the next one.
			value := func() (string, error) {
				if k+1 < len(arg) {
					v := arg[k+1:]
					k = len(arg)
					return v, nil
				}
				if len(args) == 0 {
					return "", builtinUsagef("bind: -%c: option requires an argument\n%s", opt, bindUsage)
				}
				v := args[0]
				args = args[1:]
				return v, nil
			}
			switch opt {
			case 'l':
				for _, entry := range bindActions {
					fmt.Fprintln(out, entry.names[0])
				}
			case 'p':
				m.printBindings(out)
			case 'X':
				m.printCommandBindings(out)
			case 'P':
				for _, entry := range bindActions {
					m.printFunctionKeys(out, entry.names[0], "%s can be found on %s.\n")
				}
			case 's', 'S', 'v', 'V':
				// Macros and readline variables: gosh has none to list.
			case 'm':
				if _, err := value(); err != nil {
					return err
				}
			case 'q':
				name, err := value()
				if err != nil {
					return err
				}
				if _, ok := lookupBindAction(name); !ok {
					return fmt.Errorf("bind: `%s': unknown function name", name)
				}
				if !m.printFunctionKeys(out, name, "%s can be invoked via %s.\n") {
					return interp.ExitStatus(1)
				}
			case 'u':
				name, err := value()
				if err != nil {
					return err
				}
				action, ok := lookupBindAction(name)
				if !ok {
					return fmt.Errorf("bind: `%s': unknown function name", name)
				}
				m.unbindAction(action)
			case 'r':
				spec, err := value()
				if err != nil {
					return err
				}
				seq, err := parseKeySequence(spec)
				if err != nil {
					return err
				}
				m.mu.Lock()
				delete(m.entries, string(seq))
				m.mu.Unlock()
			case 'f':
				name, err := value()
				if err != nil {
					return err
				}
				if err := m.bindFile(name); err != nil {
					return err
				}
			case 'x':
				spec, err := value()
				if err != nil {
					return err
				}
				if err := m.bindCommand(spec); err != nil {
					return err
				}
			default:
				return builtinUsagef("bind: -%c: invalid option\n%s", opt, bindUsage)
			}
		}
	}
	for _, arg := range args {
		if err := m.bindLine(arg); err != nil {
			return err
		}
	}
	return nil
}

// bindLine applies one line of readline init syntax: a binding of a key
// sequence to a function, or a "set" of a readline variable, which gosh
// accepts and ignores.
func (m *keyBindingManager) bindLine(line string) error {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "set ") || strings.HasPrefix(line, "$") {
		return nil
	}
	keySpec, actionSpec, err := parseBindArgs([]string{line})
	if err != nil {
		return err
	}
	seq, err := parseKeySequence(keySpec)
	if err != nil {
		return err
	}
	if len(seq) == 0 {
		return fmt.Errorf("bind: empty key sequence")
	}
	actionRune, ok := lookupBindAction(actionSpec)
	if !ok {
		return fmt.Errorf("bind: `%s': unknown function name", trimOuterQuotes(actionSpec))
	}
	m.store(seq, actionRune)
	return nil
}

// bindCommand binds a key sequence to a shell command, given as
// "keyseq: command", for "bind -x".
func (m *keyBindingManager) bindCommand(spec string) error {
	keySpec, command, err := parseBindArgs([]string{spec})
	if err != nil {
		return fmt.Errorf("bind: %s: missing colon separator", spec)
	}
	seq, err := parseKeySequence(keySpec)
	if err != nil {
		return err
	}
	if len(seq) == 0 {
		return fmt.Errorf("bind: empty key sequence")
	}
	m.mu.Lock()
	m.entries[string(seq)] = &goKeyBindingEntry{seq: seq, action: keyActionShellCommand, command: trimOuterQuotes(command)}
	m.mu.Unlock()
	return nil
}

// printCommandBindings lists the "bind -x" bindings as "bind -X" does.
func (m *keyBindingManager) printCommandBindings(out io.Writer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var lines []string
	for _, entry := range m.entries {
		if entry.action == keyActionShellCommand {
			command := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(entry.command)
			lines = append(lines, fmt.Sprintf("\"%s\": \"%s\"", keySequenceSpec(entry.seq), command))
		}
	}
	slices.Sort(lines)
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
}

// bindFile applies the lines of a readline init file, as "bind -f" does.
func (m *keyBindingManager) bindFile(name string) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return fmt.Errorf("bind: %s: %v", name, strerror(err))
	}
	for _, line := range strings.Split(string(data), "\n") {
		if err := m.bindLine(line); err != nil {
			return err
		}
	}
	return nil
}

// printBindings lists the key sequences bound with bind, in readline init
// syntax, sorted.
func (m *keyBindingManager) printBindings(out io.Writer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries := make([]*goKeyBindingEntry, 0, len(m.entries))
	for _, entry := range m.entries {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b *goKeyBindingEntry) int {
		return strings.Compare(keySequenceSpec(a.seq), keySequenceSpec(b.seq))
	})
	for _, entry := range entries {
		if name, ok := bindActionNames[entry.action]; ok {
			fmt.Fprintf(out, "\"%s\": %s\n", keySequenceSpec(entry.seq), name)
		}
	}
}

// printFunctionKeys prints with format the keys that invoke the function
// name, the ones bound with bind and readline's own, or that it is not bound
// to any keys, and reports whether it is bound.
func (m *keyBindingManager) printFunctionKeys(out io.Writer, name, format string) bool {
	action, _ := lookupBindAction(name)
	keys := m.functionKeys(action)
	if len(keys) == 0 {
		fmt.Fprintf(out, "%s is not bound to any keys.\n", name)
		return false
	}
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = `"` + key + `"`
	}
	fmt.Fprintf(out, format, name, strings.Join(quoted, ", "))
	return true
}

// functionKeys returns the key sequences that invoke action: readline's
// defaults that no binding took over, then the ones bound with bind.
func (m *keyBindingManager) functionKeys(action rune) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var keys []string
	for _, entry := range bindActions {
		if entry.action != action {
			continue
		}
		for _, key := range entry.defaults {
			seq, err := parseKeySequence(key)
			if err != nil {
				continue
			}
			if bound, ok := m.entries[string(seq)]; ok && bound.action != action {
				continue
			}
			keys = append(keys, key)
		}
	}
	var bound []string
	for _, entry := range m.entries {
		spec := keySequenceSpec(entry.seq)
		if entry.action == action && !slices.Contains(keys, spec) {
			bound = append(bound, spec)
		}
	}
	slices.Sort(bound)
	return append(keys, bound...)
}

// unbindAction removes every binding of action made with bind.
func (m *keyBindingManager) unbindAction(action rune) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, entry := range m.entries {
		if entry.action == action {
			delete(m.entries, key)
		}
	}
}

// Actions without a key of their own in readline's input: they are handed
// to readline as the escape sequences it decodes into them.
const (
	keyActionBackwardWord = 0x92 + iota
	keyActionForwardWord
	keyActionBackwardKillWord
	keyActionDeleteChar
	// keyActionShellCommand runs the command of a "bind -x" binding.
	keyActionShellCommand
)

// bindActions is the single source of truth for the functions bind
// understands. The first name of an entry is the canonical one printed by
// bind; the rest are accepted aliases. emit is what readline is fed to run
// the function, the action's own byte when empty, and defaults are the keys
// readline binds to it itself.
var bindActions = []struct {
	names    []string
	action   rune
	emit     string
	defaults []string
}{
	{names: []string{"accept-line"}, action: readline.CharEnter, defaults: []string{`\C-j`, `\C-m`}},
	{names: []string{"backward-char"}, action: readline.CharBackward, defaults: []string{`\C-b`, `\e[D`}},
	{names: []string{"backward-delete-char"}, action: readline.CharBackspace, defaults: []string{`\C-h`, `\C-?`}},
	{names: []string{"backward-kill-word"}, action: keyActionBackwardKillWord, emit: "\x1b\x7f", defaults: []string{`\e\C-h`, `\e\C-?`}},
	{names: []string{"backward-word"}, action: keyActionBackwardWord, emit: "\x1bb", defaults: []string{`\eb`}},
	{names: []string{"beginning-of-line", "start-of-line", "home"}, action: readline.CharLineStart, defaults: []string{`\C-a`}},
	{names: []string{"clear-screen"}, action: readline.CharCtrlL, defaults: []string{`\C-l`}},
	{names: []string{"complete"}, action: readline.CharTab, defaults: []string{`\C-i`}},
	{names: []string{"delete-char"}, action: keyActionDeleteChar, emit: "\x1b[3~", defaults: []string{`\C-d`, `\e[3~`}},
	{names: []string{"end-of-line", "cursor-end", "end"}, action: readline.CharLineEnd, defaults: []string{`\C-e`}},
	{names: []string{"forward-char"}, action: readline.CharForward, defaults: []string{`\C-f`, `\e[C`}},
	{names: []string{"forward-search-history"}, action: readline.CharFwdSearch, defaults: []string{`\C-s`}},
	{names: []string{"forward-word"}, action: keyActionForwardWord, emit: "\x1bf", defaults: []string{`\ef`}},
	{names: []string{"history-search-backward"}, action: keyActionHistorySearchBackward},
	{names: []string{"history-search-forward"}, action: keyActionHistorySearchForward},
	{names: []string{"kill-line"}, action: readline.CharKill, defaults: []string{`\C-k`}},
	{names: []string{"next-history", "next-screen"}, action: readline.CharNext, defaults: []string{`\C-n`, `\e[B`}},
	{names: []string{"previous-history", "previous-screen"}, action: readline.CharPrev, defaults: []string{`\C-p`, `\e[A`}},
	{names: []string{"reverse-search-history"}, action: readline.CharBckSearch, defaults: []string{`\C-r`}},
	{names: []string{"transpose-chars"}, action: readline.CharTranspose, defaults: []string{`\C-t`}},
	{names: []string{"undo"}, action: readline.CharCtrl_, defaults: []string{`\C-_`}},
	{names: []string{"unix-line-discard"}, action: readline.CharCtrlU, defaults: []string{`\C-u`}},
	{names: []string{"unix-word-rubout"}, action: readline.CharCtrlW, defaults: []string{`\C-w`}},
	{names: []string{"yank"}, action: readline.CharCtrlY, defaults: []string{`\C-y`}},
}

// bindActionNames maps action runes back to their canonical function
// names, for listings.
var bindActionNames = func() map[rune]string {
	names := make(map[rune]string, len(bindActions))
	for _, entry := range bindActions {
		names[entry.action] = entry.names[0]
	}
	return names
}()

// bindActionEmit maps each action readline runs itself to what readline is
// fed for it.
var bindActionEmit = func() map[rune]string {
	emit := make(map[rune]string, len(bindActions))
	for _, entry := range bindActions {
		switch {
		case entry.emit != "":
			emit[entry.action] = entry.emit
		case entry.action > 0 && entry.action < 0x80:
			emit[entry.action] = string(byte(entry.action))
		}
	}
	return emit
}()

// bindActionByName resolves every accepted name and alias to its action rune.
var bindActionByName = func() map[string]rune {
	actions := make(map[string]rune, len(bindActions))
	for _, entry := range bindActions {
		for _, name := range entry.names {
			actions[name] = entry.action
		}
	}
	return actions
}()

// keySequenceSpec renders a bound key sequence back in bind syntax, e.g.
// "\e[A" or "\C-a".
func keySequenceSpec(seq []byte) string {
	var b strings.Builder
	for _, c := range seq {
		switch {
		case c == 0x1b:
			b.WriteString(`\e`)
		case c == 0x7f:
			b.WriteString(`\C-?`)
		case c < 0x20:
			if c == 0 {
				b.WriteString(`\C-@`)
			} else {
				fmt.Fprintf(&b, `\C-%c`, c+0x60)
			}
		case c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

func (m *keyBindingManager) store(seq []byte, action rune) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := string(seq)
	entry := &goKeyBindingEntry{seq: append([]byte(nil), seq...), action: action}
	m.entries[key] = entry
}

func (m *keyBindingManager) registerActionHandler(action rune, handler func(rune) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handler == nil {
		if m.actions != nil {
			delete(m.actions, action)
		}
		return
	}
	if m.actions == nil {
		m.actions = make(map[rune]func(rune) bool)
	}
	m.actions[action] = handler
}

func (m *keyBindingManager) invokeAction(action rune) bool {
	m.mu.RLock()
	handler := m.actions[action]
	m.mu.RUnlock()
	if handler == nil {
		return false
	}
	return handler(action)
}

// match returns the binding whose key sequence starts buf, the longest one,
// or reports that buf could still become the start of one.
func (m *keyBindingManager) match(buf []byte) (*goKeyBindingEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	needMore := false
	var matched *goKeyBindingEntry
	for _, entry := range m.entries {
		seq := entry.seq
		switch {
		case len(buf) >= len(seq) && bytes.Equal(buf[:len(seq)], seq):
			if matched == nil || len(seq) > len(matched.seq) {
				matched = entry
			}
		case len(buf) < len(seq) && bytes.Equal(seq[:len(buf)], buf):
			needMore = true
		}
	}
	if matched != nil {
		return matched, false
	}
	return nil, needMore
}

func parseBindArgs(args []string) (string, string, error) {
	if len(args) == 0 {
		return "", "", fmt.Errorf("bind: missing arguments")
	}
	if len(args) == 1 {
		parts := strings.SplitN(args[0], ":", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("bind: invalid format, expected key: action")
		}
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
	}
	key := args[0]
	if strings.HasSuffix(key, ":") {
		key = key[:len(key)-1]
	}
	action := strings.Join(args[1:], " ")
	return strings.TrimSpace(key), strings.TrimSpace(action), nil
}

func parseKeySequence(spec string) ([]byte, error) {
	s := trimOuterQuotes(strings.TrimSpace(spec))
	var out []byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch != '\\' {
			out = append(out, ch)
			continue
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("bind: trailing escape in %q", spec)
		}
		switch s[i] {
		case 'e', 'E':
			out = append(out, 0x1b)
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '\\':
			out = append(out, '\\')
		case '\'':
			out = append(out, '\'')
		case '"':
			out = append(out, '"')
		case 'x', 'X':
			if i+2 >= len(s) {
				return nil, fmt.Errorf("bind: incomplete hex escape in %q", spec)
			}
			val, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("bind: invalid hex escape in %q", spec)
			}
			out = append(out, byte(val))
			i += 2
		case 'C', 'c':
			if i+1 >= len(s) || s[i+1] != '-' {
				out = append(out, s[i])
				continue
			}
			i += 2
			if i >= len(s) {
				return nil, fmt.Errorf("bind: malformed control sequence in %q", spec)
			}
			if s[i] == '?' {
				out = append(out, 0x7f)
			} else {
				out = append(out, s[i]&0x1f)
			}
		case 'M', 'm':
			if i+1 >= len(s) || s[i+1] != '-' {
				out = append(out, s[i])
				continue
			}
			i += 2
			if i >= len(s) {
				return nil, fmt.Errorf("bind: malformed meta sequence in %q", spec)
			}
			out = append(out, 0x80|s[i])
		default:
			out = append(out, s[i])
		}
	}
	return out, nil
}

func lookupBindAction(action string) (rune, bool) {
	rn, ok := bindActionByName[strings.ToLower(trimOuterQuotes(strings.TrimSpace(action)))]
	return rn, ok
}

func trimOuterQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

type keyBindingInput struct {
	src io.Reader
	mgr *keyBindingManager
	buf []byte
	out []byte
	tmp [64]byte

	needMore bool
	// flushFirst reports that processing stopped at a "bind -x" key to
	// hand readline the keys before it first.
	flushFirst bool
}

const keyBindingPrefixTimeout = 50 * time.Millisecond

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

func (r *keyBindingInput) Read(p []byte) (int, error) {
	if r.flushFirst && len(r.out) == 0 {
		r.flushFirst = false
		r.needMore = r.processBuffer()
	}
	for len(r.out) == 0 {
		var deadliner readDeadliner
		deadlineSet := false
		if r.needMore && len(r.buf) > 0 {
			if d, ok := r.src.(readDeadliner); ok {
				if err := d.SetReadDeadline(time.Now().Add(keyBindingPrefixTimeout)); err == nil {
					deadliner = d
					deadlineSet = true
				}
			}
		}
		n, err := r.src.Read(r.tmp[:])
		if deadlineSet {
			_ = deadliner.SetReadDeadline(time.Time{})
		}
		if n > 0 {
			r.buf = append(r.buf, r.tmp[:n]...)
			r.needMore = r.processBuffer()
		}
		if len(r.out) > 0 {
			break
		}
		if err != nil {
			if isReadTimeout(err) && len(r.buf) > 0 {
				r.out = append(r.out, r.buf[0])
				r.buf = r.buf[1:]
				r.needMore = r.processBuffer()
				continue
			}
			if err == io.EOF && len(r.buf) > 0 {
				r.out = append(r.out, r.buf...)
				r.buf = nil
				r.needMore = false
				continue
			}
			return 0, err
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func isReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeout interface {
		Timeout() bool
	}
	return errors.As(err, &timeout) && timeout.Timeout()
}

func (r *keyBindingInput) processBuffer() bool {
	for len(r.buf) > 0 {
		entry, needMore := r.mgr.match(r.buf)
		if entry != nil {
			if entry.action == keyActionShellCommand {
				if len(r.out) > 0 {
					// Readline has to see the keys before it first, so
					// that the command gets the line they make.
					r.flushFirst = true
					return false
				}
				r.buf = r.buf[len(entry.seq):]
				r.mgr.runShellCommand(entry.command)
				continue
			}
			handled := r.mgr.invokeAction(entry.action)
			r.buf = r.buf[len(entry.seq):]
			if !handled {
				// Hand readline what runs the function. Actions that only
				// exist as a registered handler have nothing to send.
				r.out = append(r.out, bindActionEmit[entry.action]...)
			}
			continue
		}
		if needMore {
			return true
		}
		r.out = append(r.out, r.buf[0])
		r.buf = r.buf[1:]
	}
	return false
}

// setCommandRunner sets what runs the commands of "bind -x" bindings.
func (m *keyBindingManager) setCommandRunner(run func(command string)) {
	m.mu.Lock()
	m.runCommand = run
	m.mu.Unlock()
}

func (m *keyBindingManager) runShellCommand(command string) {
	m.mu.RLock()
	run := m.runCommand
	m.mu.RUnlock()
	if run != nil {
		run(command)
	}
}
