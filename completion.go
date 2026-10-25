package gosh

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/phuslu/gosh/internal/readline"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

type autoCompleter struct {
	ctx           context.Context
	runner        *interp.Runner
	opts          *shellOptions
	completion    *completionRegistry
	aliases       *aliasTable
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	promptPrinter *promptPrinter
	defaultHome   string

	rlMu sync.Mutex
	rl   *readline.Instance

	mu              sync.Mutex
	cachedPath      string
	cachedPathExt   string
	cachedFuncStamp string
	cachedHome      string
	cachedCommands  []string
}

var defaultCommandNames = []string{
	"alias",
	"bg",
	"bind",
	"break",
	"builtin",
	"caller",
	"cd",
	"command",
	"compgen",
	"complete",
	"compopt",
	"continue",
	"declare",
	"dirs",
	"disown",
	"echo",
	"enable",
	"eval",
	"exec",
	"exit",
	"export",
	"false",
	"fc",
	"fg",
	"getopts",
	"hash",
	"help",
	"history",
	"jobs",
	"kill",
	"let",
	"local",
	"logout",
	"mapfile",
	"popd",
	"printf",
	"pushd",
	"pwd",
	"read",
	"readarray",
	"readonly",
	"return",
	"set",
	"shift",
	"shopt",
	"source",
	"suspend",
	"test",
	"times",
	"trap",
	"true",
	"type",
	"typeset",
	"ulimit",
	"umask",
	"unalias",
	"unset",
	"wait",
	":",
	".",
	"[",
}

var defaultKeywordNames = []string{
	"case",
	"coproc",
	"do",
	"done",
	"elif",
	"else",
	"esac",
	"fi",
	"for",
	"function",
	"if",
	"in",
	"select",
	"then",
	"time",
	"until",
	"while",
}

func (c *autoCompleter) attach(rl *readline.Instance) {
	c.rlMu.Lock()
	c.rl = rl
	c.rlMu.Unlock()
}

func (c *autoCompleter) Do(line []rune, pos int) ([][]rune, int) {
	ctx := c.completionContext(line, pos)
	if additions, ok := c.variableCompletion(line, pos, ctx); ok {
		return additions, 0
	}
	if programmable := c.programmableCompletion(ctx, line, pos); programmable.handled {
		return c.applyCompletionOptions(programmable.candidates, ctx, completionStyle{
			noSpace:  programmable.options.nospace,
			noQuote:  programmable.options.noquote,
			markDirs: programmable.files,
		})
	}
	var options []string
	if !ctx.isCommand && shoptEnabled(c.opts, "hostcomplete") {
		options = c.hostCandidates(ctx.prefix)
	}
	if len(options) == 0 && ctx.isCommand && !strings.ContainsAny(ctx.prefix, "/\\") {
		options = c.commandCandidates(ctx.prefix)
	} else if len(options) == 0 {
		onlyDirs := !ctx.isCommand && strings.EqualFold(ctx.command, "cd")
		options = c.pathCandidates(ctx.prefix, onlyDirs)
	}
	if len(options) == 0 {
		return nil, 0
	}
	return c.applyCompletionOptions(options, ctx, completionStyle{markDirs: true})
}

// completionStyle is how matches go on the line: noSpace leaves out the
// space after a single match, noQuote its quoting, and markDirs appends a
// slash to a match naming a directory.
type completionStyle struct {
	noSpace  bool
	noQuote  bool
	markDirs bool
}

// applyCompletionOptions turns a candidate list into the readline addition
// and printed matches, in the given style.
func (c *autoCompleter) applyCompletionOptions(options []string, ctx completionContext, style completionStyle) ([][]rune, int) {
	if len(options) == 0 {
		return nil, 0
	}
	prefixRunes := []rune(ctx.prefix)
	prefixLen := len(prefixRunes)
	common := longestCommonPrefix(options)
	commonRunes := []rune(common)
	if !runesHavePrefix(commonRunes, prefixRunes) {
		// The candidates do not extend what is already on the line, which
		// programmable completion is free to do. Inserting the tail of the
		// common prefix here would splice unrelated text into the word, so
		// only show the matches.
		c.printMatches(options)
		return nil, 0
	}
	addition := []rune{}
	if len(commonRunes) > prefixLen {
		tail := string(commonRunes[prefixLen:])
		if !style.noQuote {
			tail = escapeCompletionForContext(tail, ctx.quote)
		}
		addition = append(addition, []rune(tail)...)
	}
	if len(options) == 1 {
		// What follows the match: a directory's separator, or a space
		// unless the completion function asked for none.
		var sep []rune
		if hasTrailingPathSeparator(options[0]) {
			if n := len(addition); n > 0 && (addition[n-1] == '/' || addition[n-1] == '\\') {
				sep, addition = []rune{addition[n-1]}, addition[:n-1]
			}
		} else if style.markDirs && c.completionOptionIsDir(ctx, options[0]) {
			sep = []rune{os.PathSeparator}
		} else if !style.noSpace {
			sep = []rune{' '}
		}
		if ctx.quote != 0 && len(sep) > 0 {
			// Like readline, close the quote the word started.
			addition = append(addition, ctx.quote)
		}
		addition = append(addition, sep...)
	}
	if len(addition) > 0 {
		return [][]rune{addition}, prefixLen
	}
	if len(options) == 1 {
		return nil, 0
	}
	c.printMatches(options)
	return nil, 0
}

// variableCompletion completes a variable reference before the cursor,
// "$NAME" or "${NAME", with the names of the shell's variables, as Bash
// does: a single match gets its closing brace and quote, then a slash when
// the variable names a directory and a space otherwise, and several are
// listed. It reports whether the cursor is in such a reference.
func (c *autoCompleter) variableCompletion(line []rune, pos int, ctx completionContext) ([][]rune, bool) {
	if ctx.quote == '\'' {
		return nil, false
	}
	text := string(line[:min(max(pos, 0), len(line))])
	name, braced, ok := variableReferenceAt(text)
	if !ok {
		return nil, false
	}
	var matches []string
	for _, v := range completionVariables(c.runner, func(expand.Variable) bool { return true }) {
		if strings.HasPrefix(v, name) {
			matches = append(matches, v)
		}
	}
	if len(matches) == 0 {
		return nil, true
	}
	common := longestCommonPrefix(matches)
	addition := common[len(name):]
	if len(matches) == 1 {
		if braced {
			addition += "}"
		}
		if ctx.quote == '"' {
			addition += `"`
		}
		value, _ := runnerStringVar(c.runner, matches[0])
		if info, err := os.Stat(value); err == nil && info.IsDir() && value != "" {
			addition += "/"
		} else {
			addition += " "
		}
		return [][]rune{[]rune(addition)}, true
	}
	if addition != "" {
		return [][]rune{[]rune(addition)}, true
	}
	display := make([]string, len(matches))
	for i, m := range matches {
		if braced {
			display[i] = "${" + m
		} else {
			display[i] = "$" + m
		}
	}
	c.printList(display)
	return nil, true
}

// variableReferenceAt finds the variable reference text ends in, "$NAME"
// or "${NAME" with NAME possibly empty, not escaped with a backslash, and
// returns the name so far and whether it is braced.
func variableReferenceAt(text string) (string, bool, bool) {
	start := len(text)
	for start > 0 && isNameByte(text[start-1]) {
		start--
	}
	name := text[start:]
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		return "", false, false
	}
	braced := false
	if start > 0 && text[start-1] == '{' {
		braced = true
		start--
	}
	if start == 0 || text[start-1] != '$' {
		return "", false, false
	}
	// An odd number of backslashes before the "$" escapes it.
	backslashes := 0
	for k := start - 2; k >= 0 && text[k] == '\\'; k-- {
		backslashes++
	}
	if backslashes%2 == 1 {
		return "", false, false
	}
	return name, braced, true
}

func isNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

type completionContext struct {
	prefix    string
	isCommand bool
	command   string
	quote     rune
	words     []string
	cword     int
	inWord    bool
}

func (c *autoCompleter) completionContext(line []rune, pos int) completionContext {
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	return parseCompletionContext(line[:pos])
}

func (c *autoCompleter) commandCandidates(prefix string) []string {
	path := c.shellVar("PATH")
	pathExt := c.shellVar("PATHEXT")
	funcStamp := c.functionStamp()
	home := c.userHome()
	commands := c.commandIndex(path, pathExt, funcStamp, home)
	if len(commands) == 0 {
		return nil
	}
	matches := make([]string, 0, len(commands))
	for _, name := range commands {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
		}
	}
	return matches
}

func (c *autoCompleter) hostCandidates(prefix string) []string {
	return hostCandidatesFrom(prefix, systemHostNames())
}

func hostCandidatesFrom(prefix string, hosts []string) []string {
	at := strings.LastIndex(prefix, "@")
	if at < 0 || at == len(prefix)-1 {
		return nil
	}
	hostPrefix := prefix[:at+1]
	hostPart := prefix[at+1:]
	if strings.ContainsAny(hostPart, `/\`) {
		return nil
	}
	seen := make(map[string]struct{})
	matches := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if !strings.HasPrefix(host, hostPart) {
			continue
		}
		candidate := hostPrefix + host
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		matches = append(matches, candidate)
	}
	slices.Sort(matches)
	return matches
}

func systemHostNames() []string {
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var hosts []string
	for _, line := range strings.Split(string(data), "\n") {
		if before, _, ok := strings.Cut(line, "#"); ok {
			line = before
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, host := range fields[1:] {
			if host == "" || strings.ContainsAny(host, " \t/\\") {
				continue
			}
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	return hosts
}

func (c *autoCompleter) commandIndex(path, pathExt, funcStamp, home string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if path == c.cachedPath && pathExt == c.cachedPathExt && funcStamp == c.cachedFuncStamp && home == c.cachedHome && len(c.cachedCommands) > 0 {
		return c.cachedCommands
	}
	cmds := c.buildCommandIndexLocked(path, pathExt, home)
	c.cachedPath = path
	c.cachedPathExt = pathExt
	c.cachedFuncStamp = funcStamp
	c.cachedHome = home
	c.cachedCommands = cmds
	return cmds
}

func (c *autoCompleter) buildCommandIndexLocked(path, pathExt, home string) []string {
	seen := make(map[string]struct{})
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
	}
	for _, name := range defaultCommandNames {
		add(name)
	}
	for name := range c.runner.Funcs {
		add(name)
	}
	for _, keyword := range defaultKeywordNames {
		add(keyword)
	}
	if path != "" {
		parts := strings.Split(path, string(os.PathListSeparator))
		for _, dir := range parts {
			if dir == "" {
				dir = "."
			}
			if expanded, ok := expandTilde(dir, home); ok {
				dir = expanded
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(c.runner.Dir, dir)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				for _, name := range commandEntryNames(dir, entry, runtime.GOOS, pathExt) {
					add(name)
				}
			}
		}
	}
	cmds := make([]string, 0, len(seen))
	for name := range seen {
		cmds = append(cmds, name)
	}
	slices.Sort(cmds)
	return cmds
}

func commandEntryNames(dir string, entry os.DirEntry, goos, pathExt string) []string {
	if entry == nil || entry.IsDir() {
		return nil
	}
	name := entry.Name()
	if goos == "windows" {
		ext := strings.ToLower(filepath.Ext(name))
		if ext == "" || !slices.Contains(pathExts(pathExt), ext) {
			return nil
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if stem == "" || strings.EqualFold(stem, name) {
			return []string{name}
		}
		return []string{stem, name}
	}
	info, err := entry.Info()
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		info, err = os.Stat(filepath.Join(dir, name))
	}
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil
	}
	return []string{name}
}

func pathExts(pathExt string) []string {
	if pathExt == "" {
		pathExt = ".com;.exe;.bat;.cmd"
	}
	var exts []string
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(strings.ToLower(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		exts = append(exts, ext)
	}
	return exts
}

func (c *autoCompleter) pathCandidates(prefix string, dirsOnly bool) []string {
	dirPart, base := splitPathPrefix(prefix)
	search := dirPart
	if search == "" {
		search = "."
	}
	home := c.userHome()
	if expanded, ok := expandTilde(search, home); ok {
		search = expanded
	}
	clean := filepath.Clean(search)
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(c.runner.Dir, clean)
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil
	}
	matches := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		isDir := entry.IsDir()
		if !isDir {
			needStat := dirsOnly || entry.Type()&fs.ModeSymlink != 0
			if needStat {
				if info, err := os.Stat(filepath.Join(clean, name)); err == nil {
					isDir = info.IsDir()
				}
			}
		}
		if dirsOnly && !isDir {
			continue
		}
		candidate := dirPart + name
		if isDir {
			candidate += string(os.PathSeparator)
		}
		matches = append(matches, candidate)
	}
	slices.Sort(matches)
	return matches
}

func (c *autoCompleter) completionOptionIsDir(ctx completionContext, option string) bool {
	if option == "" {
		return false
	}
	if hasTrailingPathSeparator(option) {
		return true
	}
	if ctx.isCommand && !strings.ContainsAny(ctx.prefix, "/\\") {
		return false
	}
	resolved := option
	if expanded, ok := expandTilde(resolved, c.userHome()); ok {
		resolved = expanded
	}
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(c.runner.Dir, resolved)
	}
	if info, err := os.Stat(resolved); err == nil {
		return info.IsDir()
	}
	return false
}

func (c *autoCompleter) printMatches(options []string) {
	display := make([]string, len(options))
	for i, option := range options {
		display[i] = completionDisplayName(option)
	}
	c.printList(display)
}

// printList lists the matches as they are shown, in columns.
func (c *autoCompleter) printList(display []string) {
	if len(display) == 0 || c.stdout == nil {
		return
	}
	options := display
	width := c.screenWidth()
	if width <= 0 {
		width = 80
	}
	maxLen := 0
	for _, entry := range display {
		if size := utf8.RuneCountInString(entry); size > maxLen {
			maxLen = size
		}
	}
	if maxLen == 0 {
		maxLen = 1
	}
	colWidth := maxLen + 2
	if colWidth > width {
		colWidth = maxLen
	}
	cols := 1
	if colWidth > 0 {
		cols = width / colWidth
	}
	if cols < 1 {
		cols = 1
	}
	rows := (len(options) + cols - 1) / cols
	fmt.Fprint(c.stdout, "\r\n")
	for r := 0; r < rows; r++ {
		printed := false
		for cidx := 0; cidx < cols; cidx++ {
			idx := cidx*rows + r
			if idx >= len(options) {
				break
			}
			entry := display[idx]
			printed = true
			fmt.Fprint(c.stdout, entry)
			nextIdx := (cidx+1)*rows + r
			if cidx == cols-1 || nextIdx >= len(options) {
				continue
			}
			pad := colWidth - utf8.RuneCountInString(entry)
			if pad < 1 {
				pad = 1
			}
			fmt.Fprint(c.stdout, strings.Repeat(" ", pad))
		}
		if printed {
			fmt.Fprint(c.stdout, "\r\n")
		}
	}
	c.printPromptPrefix()
	if rl := c.readline(); rl != nil {
		rl.Refresh()
	}
}

func (c *autoCompleter) readline() *readline.Instance {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	return c.rl
}

func (c *autoCompleter) printPromptPrefix() {
	if c.promptPrinter == nil || c.stdout == nil {
		return
	}
	if prefix := c.promptPrinter.Prefix(); prefix != "" {
		fmt.Fprint(c.stdout, prefix)
	}
}

func (c *autoCompleter) screenWidth() int {
	if rl := c.readline(); rl != nil {
		if cfg := rl.GetConfig(); cfg != nil && cfg.FuncGetSize != nil {
			if width, _ := cfg.FuncGetSize(); width > 0 {
				return width
			}
		}
	}
	return 80
}

func completionDisplayName(opt string) string {
	trimmed := strings.TrimRight(opt, "/\\")
	if trimmed == "" {
		return escapeCompletion(opt)
	}
	idx := strings.LastIndexAny(trimmed, "/\\")
	base := trimmed
	if idx >= 0 {
		base = trimmed[idx+1:]
	}
	if strings.HasSuffix(opt, "/") || strings.HasSuffix(opt, "\\") {
		base += "/"
	}
	return escapeCompletion(base)
}

func escapeCompletion(val string) string {
	return escapeCompletionForContext(val, 0)
}

func escapeCompletionForContext(val string, quote rune) string {
	if val == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range val {
		if shouldEscapeCompletionRune(r, quote) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shouldEscapeCompletionRune(r rune, quote rune) bool {
	switch quote {
	case '\'':
		return r == '\''
	case '"':
		switch r {
		case '\\', '"', '`', '$':
			return true
		default:
			return false
		}
	default:
		switch r {
		case ' ', '\t', '\n', '\\', '"', '\'', '`', '$', '&', '|', ';', '<', '>', '(', ')', '{', '}', '[', ']', '!', '?', '*', '~', '^', '#', '%', '=', ':', ',', '+':
			return true
		default:
			return false
		}
	}
}

// shellVar reads a plain shell variable straight from the interpreter, with
// no subshell: PATH, PATHEXT and HOME are consulted on every completion.
func (c *autoCompleter) shellVar(name string) string {
	val, _ := runnerStringVar(c.runner, name)
	return val
}

func (c *autoCompleter) userHome() string {
	if home := c.shellVar("HOME"); home != "" {
		return home
	}
	return c.defaultHome
}

func (c *autoCompleter) functionStamp() string {
	if len(c.runner.Funcs) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.runner.Funcs))
	for name := range c.runner.Funcs {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, "\x00")
}

func splitPathPrefix(prefix string) (string, string) {
	idx := strings.LastIndexAny(prefix, "/\\")
	if idx < 0 {
		return "", prefix
	}
	return prefix[:idx+1], prefix[idx+1:]
}

func hasTrailingPathSeparator(val string) bool {
	if val == "" {
		return false
	}
	return strings.HasSuffix(val, "/") || strings.HasSuffix(val, "\\")
}

func expandTilde(path, home string) (string, bool) {
	if !strings.HasPrefix(path, "~") {
		return path, true
	}
	if home == "" {
		return "", false
	}
	if len(path) == 1 {
		return home, true
	}
	next := rune(path[1])
	if next == '/' || next == '\\' {
		if len(path) == 2 {
			return home, true
		}
		return filepath.Join(home, path[2:]), true
	}
	return "", false
}

func sharedPrefix(a, b string) string {
	ar := []rune(a)
	br := []rune(b)
	limit := len(ar)
	if len(br) < limit {
		limit = len(br)
	}
	i := 0
	for i < limit && ar[i] == br[i] {
		i++
	}
	return string(ar[:i])
}

// runesHavePrefix reports whether r starts with prefix.
func runesHavePrefix(r, prefix []rune) bool {
	if len(r) < len(prefix) {
		return false
	}
	for i, c := range prefix {
		if r[i] != c {
			return false
		}
	}
	return true
}

func longestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, val := range values[1:] {
		prefix = sharedPrefix(prefix, val)
		if prefix == "" {
			break
		}
	}
	return prefix
}
