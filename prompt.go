package gosh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func defaultPrompt(version string) string {
	symbol := "$"
	if os.Geteuid() == 0 {
		symbol = "#"
	}
	return "sh-" + shortVersion(version) + symbol + " "
}

// subshellStdin chooses an stdin source for prompt and completion subshells.
// os.File reads are safe to share with the shell and preserve terminal
// behavior; arbitrary readers are not, and passing them to two interp.StdIO
// copy goroutines at once would race, so they get an empty stdin instead.
func subshellStdin(stdin io.Reader) io.Reader {
	if stdin == nil {
		return strings.NewReader("")
	}
	if _, ok := stdin.(*os.File); ok {
		return stdin
	}
	return strings.NewReader("")
}

func shortVersion(version string) string {
	if len(version) > 3 {
		return version[:3]
	}
	if version == "" {
		return "0.0"
	}
	return version
}

// promptEnv holds everything prompt rendering needs for the lifetime of an
// interactive session: the interpreter to read variables from and to run
// command substitutions in, the option state gating promptvars, the history
// behind \!, the host name behind \h and \H, the fallbacks for \u and \w,
// and the parsed-template cache. The interactive loop builds one and renders
// every prompt through it.
type promptEnv struct {
	runner       *interp.Runner
	opts         *shellOptions
	history      *history
	stdin        io.Reader
	stderr       io.Writer
	host         string
	shortHost    string
	homeFallback string
	userFallback string
	cache        *promptCache
}

func (s *Shell) newPromptEnv() *promptEnv {
	host := s.hostname
	if host == "" {
		host = "localhost"
	}
	short := host
	if idx := strings.IndexByte(host, '.'); idx >= 0 {
		short = host[:idx]
	}
	return &promptEnv{
		runner:       s.runner,
		opts:         s.opts,
		history:      s.history,
		stdin:        subshellStdin(s.stdin),
		stderr:       s.stderr,
		host:         host,
		shortHost:    short,
		homeFallback: s.homeFallback,
		userFallback: s.userFallback,
		cache:        s.promptCache,
	}
}

// render expands the prompt variable name (PS1, PS2) for command number seq,
// falling back to fallback when it is unset or empty.
func (e *promptEnv) render(ctx context.Context, name, fallback string, seq int) promptParts {
	if e == nil || e.runner == nil {
		return promptParts{prompt: fallback}
	}
	src, _ := runnerStringVar(e.runner, name)
	if src == "" {
		return promptParts{prompt: fallback}
	}
	state := &promptState{
		env:       e,
		ctx:       ctx,
		dir:       e.runner.Dir,
		host:      e.host,
		shortHost: e.shortHost,
		seq:       seq,
		now:       time.Now(),
	}
	return splitPromptLines((&promptRenderer{src: src, state: state, cache: e.cache}).render())
}

type promptParts struct {
	prefix string
	prompt string
}

type promptPrinter struct {
	mu     sync.RWMutex
	prefix string
}

func (p *promptPrinter) Print(w io.Writer, prefix string) {
	p.mu.Lock()
	p.prefix = prefix
	p.mu.Unlock()
	if prefix == "" || w == nil {
		return
	}
	fmt.Fprint(w, prefix)
}

func (p *promptPrinter) Prefix() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.prefix
}

func splitPromptLines(val string) promptParts {
	idx := strings.LastIndexByte(val, '\n')
	if idx < 0 {
		return promptParts{prompt: val}
	}
	return promptParts{
		prefix: val[:idx+1],
		prompt: val[idx+1:],
	}
}

// promptState is one prompt expansion: the snapshot of the shell state that
// every escape in a single draw must agree on (clock, command number, working
// directory) plus a memo of the variables it has already read.
type promptState struct {
	env       *promptEnv
	ctx       context.Context
	vars      map[string]string
	dir       string
	host      string
	shortHost string
	seq       int
	now       time.Time
}

// shellVar reads a plain shell variable, memoized so that a prompt naming the
// same variable twice reads it once. The read goes straight to the
// interpreter's variables; no subshell is involved.
func (p *promptState) shellVar(name string) string {
	if p.vars == nil {
		p.vars = make(map[string]string)
	}
	if val, ok := p.vars[name]; ok {
		return val
	}
	var val string
	if p.env != nil {
		val, _ = runnerStringVar(p.env.runner, name)
	}
	p.vars[name] = val
	return val
}

func (p *promptState) user() string {
	if val := p.shellVar("USER"); val != "" {
		return val
	}
	if p.env != nil && p.env.userFallback != "" {
		return p.env.userFallback
	}
	return fmt.Sprintf("%d", os.Getuid())
}

func (p *promptState) home() string {
	if val := p.shellVar("HOME"); val != "" {
		return val
	}
	if p.env == nil {
		return ""
	}
	return p.env.homeFallback
}

func (p *promptState) pwd() string {
	if p.dir == "" {
		p.dir = p.shellVar("PWD")
	}
	if p.dir == "" {
		p.dir = "."
	}
	return p.dir
}

func (p *promptState) promptSymbol() string {
	if os.Geteuid() == 0 {
		return "#"
	}
	return "$"
}

// promptvars reports whether parameter expansion and command substitution
// should be performed on the prompt string, per the Bash promptvars option.
func (p *promptState) promptvars() bool {
	if p.env == nil || p.env.opts == nil {
		return true
	}
	return shoptEnabled(p.env.opts, "promptvars")
}

// historyNumber returns the number Bash assigns to the command about to be
// entered: one past the current in-memory history count.
func (p *promptState) historyNumber() string {
	if p.env == nil || p.env.history == nil {
		return "0"
	}
	return strconv.Itoa(p.env.history.Len() + 1)
}

type promptRenderer struct {
	src      string
	state    *promptState
	cache    *promptCache
	template *promptTemplate
	once     sync.Once
}

func (r *promptRenderer) render() string {
	return r.templateFor().render(r.state)
}

func (r *promptRenderer) templateFor() *promptTemplate {
	r.once.Do(func() {
		if r.cache != nil {
			r.template = r.cache.get(r.src)
			return
		}
		r.template = parsePromptTemplate(r.src)
	})
	return r.template
}

type promptCache struct {
	mu      sync.Mutex
	entries map[string]*promptTemplate
}

func newPromptCache() *promptCache {
	return &promptCache{entries: make(map[string]*promptTemplate)}
}

func (c *promptCache) get(src string) *promptTemplate {
	if c == nil {
		return parsePromptTemplate(src)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if template := c.entries[src]; template != nil {
		return template
	}
	template := parsePromptTemplate(src)
	c.entries[src] = template
	return template
}

type promptTokenKind uint8

const (
	promptTokenLiteral promptTokenKind = iota
	promptTokenNonPrinting
	promptTokenEscape
	promptTokenParam
	promptTokenCommand
	promptTokenArithmetic
)

type promptToken struct {
	kind     promptTokenKind
	text     string
	escape   byte
	format   string
	expr     string
	raw      string
	template *promptTemplate
}

type promptTemplate struct {
	src    string
	tokens []promptToken
}

func parsePromptTemplate(src string) *promptTemplate {
	template := &promptTemplate{src: src}
	template.parse()
	return template
}

func (t *promptTemplate) parse() {
	for i := 0; i < len(t.src); {
		switch t.src[i] {
		case '\\':
			if i+1 >= len(t.src) {
				t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "\\"})
				i++
				continue
			}
			next := t.src[i+1]
			if next == '[' {
				inner, pos := scanPromptNonPrinting(t.src, i+2)
				if pos == -1 {
					i += 2
					continue
				}
				t.tokens = append(t.tokens, promptToken{kind: promptTokenNonPrinting, template: parsePromptTemplate(inner)})
				i = pos
				continue
			}
			if next == ']' {
				i += 2
				continue
			}
			token := promptToken{kind: promptTokenEscape, escape: next}
			if next == 'D' && i+2 < len(t.src) && t.src[i+2] == '{' {
				if end := strings.IndexByte(t.src[i+3:], '}'); end >= 0 {
					token.format = t.src[i+3 : i+3+end]
					i = i + 3 + end + 1
				} else {
					i += 2
				}
			} else {
				i += 2
			}
			t.tokens = append(t.tokens, token)
		case '$':
			start := i
			if i+1 >= len(t.src) {
				t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "$"})
				i++
				continue
			}
			switch t.src[i+1] {
			case '(':
				if i+2 < len(t.src) && t.src[i+2] == '(' {
					expr, end, ok := scanPromptArithmetic(t.src, i+3)
					if !ok {
						t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "$"})
						i++
						continue
					}
					t.tokens = append(t.tokens, promptToken{kind: promptTokenArithmetic, expr: expr, raw: t.src[start:end]})
					i = end
					continue
				}
				body, end, ok := scanPromptDelimited(t.src, i+2, '(', ')')
				if !ok {
					t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "$"})
					i++
					continue
				}
				t.tokens = append(t.tokens, promptToken{kind: promptTokenCommand, expr: body, raw: t.src[start:end]})
				i = end
			case '{':
				body, end, ok := scanPromptDelimited(t.src, i+2, '{', '}')
				if !ok {
					t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "$"})
					i++
					continue
				}
				t.tokens = append(t.tokens, promptToken{kind: promptTokenParam, expr: "${" + body + "}", raw: t.src[start:end]})
				i = end
			default:
				name, end := scanPromptName(t.src, i+1)
				if end == i+1 {
					t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: "$"})
					i++
					continue
				}
				t.tokens = append(t.tokens, promptToken{kind: promptTokenParam, expr: "${" + name + "-}", raw: t.src[start:end]})
				i = end
			}
		default:
			t.tokens = append(t.tokens, promptToken{kind: promptTokenLiteral, text: t.src[i : i+1]})
			i++
		}
	}
}

func (t *promptTemplate) render(state *promptState) string {
	var b strings.Builder
	for _, token := range t.tokens {
		switch token.kind {
		case promptTokenLiteral:
			b.WriteString(token.text)
		case promptTokenNonPrinting:
			b.WriteString(token.template.render(state))
		case promptTokenEscape:
			b.WriteString(renderPromptEscape(state, token.escape, token.format))
		case promptTokenParam:
			if !state.promptvars() {
				b.WriteString(token.raw)
				continue
			}
			b.WriteString(state.runParam(token.expr))
		case promptTokenCommand:
			if !state.promptvars() {
				b.WriteString(token.raw)
				continue
			}
			b.WriteString(state.runCommand(token.expr))
		case promptTokenArithmetic:
			if !state.promptvars() {
				b.WriteString(token.raw)
				continue
			}
			b.WriteString(state.runArithmetic(token.expr))
		}
	}
	return b.String()
}

func scanPromptNonPrinting(src string, start int) (string, int) {
	for i := start; i < len(src)-1; i++ {
		if src[i] == '\\' && src[i+1] == ']' {
			return src[start:i], i + 2
		}
	}
	return "", -1
}

func renderPromptEscape(state *promptState, c byte, format string) string {
	switch c {
	case 'a':
		return "\a"
	case 'e', 'E':
		return "\x1b"
	case 'n':
		return "\n"
	case 'r':
		return "\r"
	case 't':
		return state.now.Format("15:04:05")
	case 'T':
		return state.now.Format("03:04:05")
	case '@':
		return state.now.Format("03:04:05PM")
	case 'A':
		return state.now.Format("15:04")
	case 'd':
		return state.now.Format("Mon Jan 02")
	case 's':
		return "gosh"
	case 'u':
		return state.user()
	case 'h':
		return state.shortHost
	case 'H':
		return state.host
	case 'w':
		return state.displayPwd()
	case 'W':
		return filepath.Base(state.displayPwd())
	case 'D':
		if format != "" {
			return state.formatTime(format)
		}
	case '#':
		return fmt.Sprintf("%d", state.seq)
	case '!':
		return state.historyNumber()
	case '\\':
		return "\\"
	case '$':
		return state.promptSymbol()
	case '0':
		return "\000"
	case 'j':
		return "0"
	case 'v', 'V':
		return "gosh"
	}
	return string(c)
}

func scanPromptName(src string, start int) (string, int) {
	if start >= len(src) {
		return "", start
	}
	ch := src[start]
	if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
		j := start + 1
		for j < len(src) {
			c := src[j]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
				j++
				continue
			}
			break
		}
		return src[start:j], j
	}
	if strings.ContainsRune("@*#?$!-0123456789", rune(ch)) {
		return src[start : start+1], start + 1
	}
	return "", start
}

func scanPromptDelimited(src string, start int, open, close byte) (string, int, bool) {
	depth := 1
	for i := start; i < len(src); i++ {
		s := src[i]
		switch s {
		case '\\':
			i++
			continue
		case '\'':
			j := i + 1
			for j < len(src) && src[j] != '\'' {
				j++
			}
			if j >= len(src) {
				return "", len(src), false
			}
			i = j
			continue
		case '"':
			j := i + 1
			for j < len(src) {
				if src[j] == '\\' && j+1 < len(src) {
					j += 2
					continue
				}
				if src[j] == '"' {
					break
				}
				j++
			}
			if j >= len(src) {
				return "", len(src), false
			}
			i = j
			continue
		case '$':
			if open == '(' && close == ')' && i+2 < len(src) && src[i+1] == '(' && src[i+2] == '(' {
				next, ok := skipPromptArithmetic(src, i+3)
				if !ok {
					return "", len(src), false
				}
				i = next - 1
				continue
			}
		}
		if s == open {
			depth++
			continue
		}
		if s == close {
			depth--
			if depth == 0 {
				return src[start:i], i + 1, true
			}
		}
	}
	return "", len(src), false
}

func scanPromptArithmetic(src string, start int) (string, int, bool) {
	body, end, ok := scanPromptDelimited(src, start, '(', ')')
	if !ok || end >= len(src) {
		return "", len(src), false
	}
	return body, end + 1, true
}

func skipPromptArithmetic(src string, start int) (int, bool) {
	_, end, ok := scanPromptDelimited(src, start, '(', ')')
	if !ok || end >= len(src) || src[end] != ')' {
		return len(src), false
	}
	return end + 1, true
}

func (p *promptState) displayPwd() string {
	dir := p.pwd()
	home := p.home()
	if home != "" {
		if dir == home {
			return "~"
		}
		prefix := home
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if strings.HasPrefix(dir, prefix) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

func (p *promptState) formatTime(layout string) string {
	var b strings.Builder
	for i := 0; i < len(layout); i++ {
		if layout[i] != '%' {
			b.WriteByte(layout[i])
			continue
		}
		i++
		if i >= len(layout) {
			b.WriteByte('%')
			break
		}
		switch layout[i] {
		case '%':
			b.WriteByte('%')
		case 'H':
			b.WriteString(fmt.Sprintf("%02d", p.now.Hour()))
		case 'M':
			b.WriteString(fmt.Sprintf("%02d", p.now.Minute()))
		case 'S':
			b.WriteString(fmt.Sprintf("%02d", p.now.Second()))
		case 'Y':
			b.WriteString(fmt.Sprintf("%04d", p.now.Year()))
		case 'm':
			b.WriteString(fmt.Sprintf("%02d", int(p.now.Month())))
		case 'd':
			b.WriteString(fmt.Sprintf("%02d", p.now.Day()))
		case 'F':
			b.WriteString(p.now.Format("2006-01-02"))
		case 'T':
			b.WriteString(p.now.Format("15:04:05"))
		case 'R':
			b.WriteString(p.now.Format("15:04"))
		case 'z':
			b.WriteString(p.now.Format("-0700"))
		case 'Z':
			b.WriteString(p.now.Format("MST"))
		default:
			b.WriteByte('%')
			b.WriteByte(layout[i])
		}
	}
	return b.String()
}

func (p *promptState) runCommand(cmd string) string {
	out, err := p.runScript(cmd)
	if err != nil && !IsExitStatus(err) {
		return ""
	}
	return out
}

// runParam expands one ${...} from the prompt. A plain ${name} or ${name-},
// which is what a bare $name in a prompt parses to, is read directly from the
// interpreter; anything with an operator, an index, or a special parameter
// still goes through a subshell so that the interpreter does the expanding.
func (p *promptState) runParam(expr string) string {
	// An unset ${name} is left to the subshell: only there does set -u
	// report it, whereas ${name-} is defined to expand to the empty string.
	if name, defaulted := plainParamName(expr); name != "" && (defaulted || p.varIsSet(name)) {
		return p.shellVar(name)
	}
	script := fmt.Sprintf("printf %%s \"%s\"", p.escapeDouble(expr))
	out, err := p.runScript(script)
	if err != nil {
		return ""
	}
	return out
}

func (p *promptState) varIsSet(name string) bool {
	if p.env == nil {
		return false
	}
	_, ok := runnerStringVar(p.env.runner, name)
	return ok
}

// plainParamName returns the variable name of a ${name} or ${name-}
// expansion, which a direct variable read expands identically, and reports
// whether the "-" default was present. Anything else - an operator, an index,
// a special parameter, or one of the variables the interpreter computes on
// each lookup - yields "" and is left to a real expansion.
func plainParamName(expr string) (string, bool) {
	body, ok := strings.CutPrefix(expr, "${")
	if !ok {
		return "", false
	}
	body, ok = strings.CutSuffix(body, "}")
	if !ok {
		return "", false
	}
	name, defaulted := strings.CutSuffix(body, "-")
	if name == "" {
		return "", false
	}
	// Reject the special parameters ($?, $$, $1, ...) along with anything
	// that is not a bare name.
	if c := name[0]; !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && c != '_' {
		return "", false
	}
	if scanned, end := scanPromptName(name, 0); scanned != name || end != len(name) {
		return "", false
	}
	switch name {
	case "LINENO", "RANDOM", "SRANDOM", "PPID", "DIRSTACK":
		return "", false
	}
	return name, defaulted
}

func (p *promptState) runArithmetic(expr string) string {
	script := fmt.Sprintf("printf %%s \"$((%s))\"", p.escapeDouble(expr))
	out, err := p.runScript(script)
	if err != nil {
		return ""
	}
	return out
}

func (p *promptState) runScript(script string) (string, error) {
	if p.env == nil {
		return "", errors.New("gosh: prompt has no interpreter")
	}
	return runSubshell(p.ctx, p.env.runner, p.env.stdin, p.env.stderr, script)
}

// runSubshell runs script in a subshell of runner and returns its standard
// output with trailing newlines removed, the way command substitution does.
// Prompt and completion use it for the expansions that need the interpreter:
// command substitution, arithmetic, and programmable completion functions.
func runSubshell(ctx context.Context, runner *interp.Runner, stdin io.Reader, stderr io.Writer, script string) (string, error) {
	if runner == nil {
		return "", errors.New("gosh: prompt has no interpreter")
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return "", err
	}
	sub := runner.Subshell()
	var buf bytes.Buffer
	interp.StdIO(stdin, &buf, stderr)(sub)
	if err := sub.Run(ctx, prog); err != nil {
		return strings.TrimRight(buf.String(), "\n"), err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func (p *promptState) escapeDouble(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
