package gosh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/pattern"
)

// completionSpec is a Bash "complete" specification: how to generate the
// matches for the arguments of a command.
type completionSpec struct {
	actions  []string // completionActions names, from -A or a flag like -f
	glob     string   // -G
	wordList string   // -W, expanded and split when completing
	funcName string   // -F
	command  string   // -C
	filter   string   // -X
	prefix   string   // -P
	suffix   string   // -S
	options  completionSpecOptions
}

// completionSpecOptions holds the -o options of a specification.
type completionSpecOptions struct {
	bashdefault bool
	defaulted   bool // "default"
	dirnames    bool
	filenames   bool
	fullquote   bool
	noquote     bool
	nosort      bool
	nospace     bool
	plusdirs    bool
}

// completionOptions lists the -o options in Bash's order, with the field
// each one sets.
var completionOptions = []struct {
	name  string
	field func(*completionSpecOptions) *bool
}{
	{"bashdefault", func(o *completionSpecOptions) *bool { return &o.bashdefault }},
	{"default", func(o *completionSpecOptions) *bool { return &o.defaulted }},
	{"dirnames", func(o *completionSpecOptions) *bool { return &o.dirnames }},
	{"filenames", func(o *completionSpecOptions) *bool { return &o.filenames }},
	{"fullquote", func(o *completionSpecOptions) *bool { return &o.fullquote }},
	{"noquote", func(o *completionSpecOptions) *bool { return &o.noquote }},
	{"nosort", func(o *completionSpecOptions) *bool { return &o.nosort }},
	{"nospace", func(o *completionSpecOptions) *bool { return &o.nospace }},
	{"plusdirs", func(o *completionSpecOptions) *bool { return &o.plusdirs }},
}

// completionAction is a kind of name "complete -A" generates, with the flag
// that stands for it, if any.
type completionAction struct {
	name string
	flag byte
}

// completionActions lists the actions, the ones with a flag first in the
// order "complete -p" prints the flags.
var completionActions = []completionAction{
	{"alias", 'a'}, {"builtin", 'b'}, {"command", 'c'}, {"directory", 'd'},
	{"export", 'e'}, {"file", 'f'}, {"group", 'g'}, {"job", 'j'},
	{"keyword", 'k'}, {"service", 's'}, {"user", 'u'}, {"variable", 'v'},
	{"arrayvar", 0}, {"binding", 0}, {"disabled", 0}, {"enabled", 0},
	{"function", 0}, {"helptopic", 0}, {"hostname", 0}, {"running", 0},
	{"setopt", 0}, {"shopt", 0}, {"signal", 0}, {"stopped", 0},
}

func completionActionByFlag(flag byte) (string, bool) {
	for _, action := range completionActions {
		if action.flag == flag && flag != 0 {
			return action.name, true
		}
	}
	return "", false
}

func isCompletionAction(name string) bool {
	return slices.ContainsFunc(completionActions, func(a completionAction) bool { return a.name == name })
}

func (s *completionSpec) hasAction(name string) bool {
	return slices.Contains(s.actions, name)
}

// The keys of the specifications "complete -D", "-E" and "-I" set: for
// commands without one of their own, for an empty line, and for the command
// word. They start with a NUL byte, so that no command name can clash.
const (
	completionDefaultSpec = "\x00D"
	completionEmptySpec   = "\x00E"
	completionInitialSpec = "\x00I"
)

func isSpecialCompletionName(name string) bool {
	return name == completionDefaultSpec || name == completionEmptySpec || name == completionInitialSpec
}

// completionSpecName is how complete -p shows the key name: "-D" for
// completionDefaultSpec and so on.
func completionSpecName(name string) string {
	if isSpecialCompletionName(name) {
		return "-" + name[1:]
	}
	return name
}

type completionInvocation struct {
	spec    *completionSpec
	command string
	words   []string
	cword   int
}

// completionRegistry holds the specifications "complete" defines, keyed by
// command name or by one of the special names above.
type completionRegistry struct {
	mu     sync.Mutex
	specs  map[string]*completionSpec
	active *completionInvocation
}

func newCompletionRegistry() *completionRegistry {
	return &completionRegistry{specs: make(map[string]*completionSpec)}
}

func (r *completionRegistry) set(command string, spec *completionSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[command] = spec
}

func (r *completionRegistry) remove(command string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.specs[command]; !ok {
		return false
	}
	delete(r.specs, command)
	return true
}

func (r *completionRegistry) removeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.specs)
}

func (r *completionRegistry) spec(command string) *completionSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[command]
}

// lookup returns the specification for the arguments of command: its own,
// the one for its base name, or the default one.
func (r *completionRegistry) lookup(command string) *completionSpec {
	if spec := r.spec(command); spec != nil {
		return spec
	}
	if base := filepath.Base(command); base != command {
		if spec := r.spec(base); spec != nil {
			return spec
		}
	}
	return r.spec(completionDefaultSpec)
}

// sortedSpecs returns the names with a specification, the command names
// sorted and then the special ones, as "complete -p" lists them.
func (r *completionRegistry) sortedSpecs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for command := range r.specs {
		if !isSpecialCompletionName(command) {
			names = append(names, command)
		}
	}
	sort.Strings(names)
	for _, name := range []string{completionInitialSpec, completionDefaultSpec, completionEmptySpec} {
		if _, ok := r.specs[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// begin starts one completion attempt and returns the spec it runs with: a
// copy of the registered one, so that compopt inside a completion function
// only changes the options for this attempt, as in Bash.
func (r *completionRegistry) begin(spec *completionSpec, command string, words []string, cword int) *completionSpec {
	active := *spec
	r.mu.Lock()
	r.active = &completionInvocation{spec: &active, command: command, words: words, cword: cword}
	r.mu.Unlock()
	return &active
}

func (r *completionRegistry) end() {
	r.mu.Lock()
	r.active = nil
	r.mu.Unlock()
}

func (r *completionRegistry) activeSpec() *completionSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		return nil
	}
	return r.active.spec
}

func setCompletionOption(spec *completionSpec, name string, enabled bool) bool {
	for _, option := range completionOptions {
		if option.name == name {
			*option.field(&spec.options) = enabled
			return true
		}
	}
	return false
}

// completeArgs is what parseCompleteArgs read from the arguments of
// complete or compgen.
type completeArgs struct {
	spec *completionSpec
	// specGiven reports that an option said how to complete.
	specGiven bool
	names     []string // command names, and the special -D, -E and -I
	remove    bool     // -r
	list      bool     // -p
	varName   string   // compgen -V
}

// parseCompleteArgs reads the arguments of complete, or of compgen when
// builtin is "compgen", where the one name is the word to complete.
func parseCompleteArgs(builtin string, args []string) (*completeArgs, error) {
	usage := func(format string, a ...any) error {
		if builtin == "compgen" {
			return compgenUsagef(format, a...)
		}
		return completeUsagef(format, a...)
	}
	parsed := &completeArgs{spec: &completionSpec{}}
	spec := parsed.spec
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			args = args[1:]
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		args = args[1:]
		for k := 1; k < len(arg); k++ {
			opt := arg[k]
			value := func() (string, error) {
				if k+1 < len(arg) {
					v := arg[k+1:]
					k = len(arg)
					return v, nil
				}
				if len(args) == 0 {
					return "", usage("-%c: option requires an argument", opt)
				}
				v := args[0]
				args = args[1:]
				return v, nil
			}
			if action, ok := completionActionByFlag(opt); ok {
				if !spec.hasAction(action) {
					spec.actions = append(spec.actions, action)
				}
				parsed.specGiven = true
				continue
			}
			var v string
			if strings.IndexByte("oAGWFCXPSV", opt) >= 0 {
				var err error
				if v, err = value(); err != nil {
					return nil, err
				}
				parsed.specGiven = parsed.specGiven || opt != 'V'
			}
			switch opt {
			case 'r':
				parsed.remove = true
			case 'p':
				parsed.list = true
			case 'D', 'E', 'I':
				if builtin == "compgen" {
					return nil, usage("-%c: invalid option", opt)
				}
				parsed.names = append(parsed.names, "\x00"+string(opt))
			case 'o':
				if !setCompletionOption(spec, v, true) {
					return nil, builtinUsagef("%s: %s: invalid option name", builtin, v)
				}
			case 'A':
				if !isCompletionAction(v) {
					return nil, builtinUsagef("%s: %s: invalid action name", builtin, v)
				}
				if !spec.hasAction(v) {
					spec.actions = append(spec.actions, v)
				}
			case 'G':
				spec.glob = v
			case 'W':
				spec.wordList = v
			case 'F':
				spec.funcName = v
			case 'C':
				spec.command = v
			case 'X':
				spec.filter = v
			case 'P':
				spec.prefix = v
			case 'S':
				spec.suffix = v
			case 'V':
				if builtin != "compgen" {
					return nil, usage("-%c: invalid option", opt)
				}
				parsed.varName = v
			default:
				return nil, usage("-%c: invalid option", opt)
			}
		}
	}
	if builtin == "compgen" && len(args) > 1 {
		// Like Bash, compgen ignores the words after the first.
		args = args[:1]
	}
	parsed.names = append(parsed.names, args...)
	return parsed, nil
}

// printCompletionSpec prints spec for name as the complete command that
// defines it, like "complete -p".
func printCompletionSpec(w io.Writer, name string, spec *completionSpec) {
	var b strings.Builder
	b.WriteString("complete")
	for _, option := range completionOptions {
		if *option.field(&spec.options) {
			b.WriteString(" -o " + option.name)
		}
	}
	for _, action := range completionActions {
		if action.flag != 0 && spec.hasAction(action.name) {
			b.WriteString(" -" + string(action.flag))
		}
	}
	for _, action := range completionActions {
		if action.flag == 0 && spec.hasAction(action.name) {
			b.WriteString(" -A " + action.name)
		}
	}
	for _, field := range []struct{ flag, value string }{
		{"-G", spec.glob}, {"-W", spec.wordList}, {"-P", spec.prefix},
		{"-S", spec.suffix}, {"-X", spec.filter}, {"-C", spec.command},
	} {
		if field.value != "" {
			b.WriteString(" " + field.flag + " " + shellSingleQuote(field.value))
		}
	}
	if spec.funcName != "" {
		b.WriteString(" -F " + spec.funcName)
	}
	b.WriteString(" " + completionSpecName(name))
	fmt.Fprintln(w, b.String())
}

// builtinComplete implements complete: define a specification for each
// name, remove them with -r, or print them with -p or without any option
// saying how to complete.
func builtinComplete(deps callDeps, args []string, out, errOut io.Writer) error {
	parsed, err := parseCompleteArgs("complete", args)
	if err != nil {
		return err
	}
	reg := deps.completion
	if parsed.remove {
		if len(parsed.names) == 0 {
			reg.removeAll()
			return nil
		}
		// Like Bash, removing a specification that does not exist is no
		// error.
		for _, name := range parsed.names {
			reg.remove(name)
		}
		return nil
	}
	if parsed.list || !parsed.specGiven {
		names := parsed.names
		if len(names) == 0 {
			names = reg.sortedSpecs()
		}
		var failed error
		for _, name := range names {
			spec := reg.spec(name)
			if spec == nil {
				fmt.Fprintf(errOut, "complete: %s: no completion specification\n", completionSpecName(name))
				failed = interp.ExitStatus(1)
				continue
			}
			printCompletionSpec(out, name, spec)
		}
		return failed
	}
	for _, name := range parsed.names {
		spec := *parsed.spec
		spec.actions = slices.Clone(parsed.spec.actions)
		reg.set(name, &spec)
	}
	return nil
}

// completionGenerator generates the matches of a specification as Bash's
// gen_compspec_completions does, for completion and for compgen.
type completionGenerator struct {
	ctx    context.Context
	runner *interp.Runner
	// env is the shell's variables, when running inside a builtin, which
	// sees them more recently than runner.Vars.
	env      expand.Environ
	stdin    io.Reader
	stderr   io.Writer
	complete *autoCompleter
	aliases  *aliasTable
}

// completionRequest is the line being completed: the command, its words,
// the index of the word under the cursor and that word, and the line and
// cursor offset for COMP_LINE and COMP_POINT.
type completionRequest struct {
	command string
	words   []string
	cword   int
	word    string
	line    string
	point   int
}

// generate returns the matches of spec for req.word, in Bash's order, and
// the exit status of its completion function, if it ran one.
func (g *completionGenerator) generate(spec *completionSpec, req completionRequest) ([]string, int) {
	var matches []string
	for _, action := range spec.actions {
		matches = append(matches, g.actionMatches(action, req.word)...)
	}
	if spec.glob != "" {
		matches = append(matches, g.expandList("shopt -s nullglob; __gosh_w=( "+spec.glob+" )")...)
	}
	if spec.wordList != "" {
		for _, word := range g.expandList("set -f; __gosh_w=( " + spec.wordList + " )") {
			if strings.HasPrefix(word, req.word) {
				matches = append(matches, word)
			}
		}
	}
	status := 0
	if spec.funcName != "" {
		var replies []string
		replies, status = g.runFunction(spec, req)
		matches = append(matches, replies...)
	}
	if spec.command != "" {
		matches = append(matches, g.runCommand(spec, req)...)
	}
	if spec.filter != "" {
		matches = filterCompletionMatches(matches, spec.filter, req.word)
	}
	for i, match := range matches {
		matches[i] = spec.prefix + match + spec.suffix
	}
	if len(matches) == 0 && spec.options.dirnames {
		matches = trimDirSlashes(g.complete.pathCandidates(req.word, true))
	} else if spec.options.plusdirs {
		matches = append(matches, trimDirSlashes(g.complete.pathCandidates(req.word, true))...)
	}
	return matches, status
}

// expandList runs script, which assigns words to the array __gosh_w, in a
// subshell and returns them.
func (g *completionGenerator) expandList(script string) []string {
	out, _ := runSubshell(g.ctx, g.runner, g.stdin, g.stderr, script+"\nprintf '%s\\0' \"${__gosh_w[@]}\"")
	var words []string
	for _, word := range strings.Split(out, "\x00") {
		if word != "" {
			words = append(words, word)
		}
	}
	return words
}

// filterCompletionMatches removes the matches that match filter, in which
// "&" stands for word; a leading "!" keeps only those instead.
func filterCompletionMatches(matches []string, filter, word string) []string {
	keep := false
	if strings.HasPrefix(filter, "!") {
		keep, filter = true, filter[1:]
	}
	var pat strings.Builder
	for i := 0; i < len(filter); i++ {
		switch {
		case filter[i] == '\\' && i+1 < len(filter) && filter[i+1] == '&':
			pat.WriteString(`\&`)
			i++
		case filter[i] == '&':
			pat.WriteString(pattern.QuoteMeta(word, 0))
		default:
			pat.WriteByte(filter[i])
		}
	}
	expr, err := pattern.Regexp(pat.String(), pattern.EntireString|pattern.ExtendedOperators)
	if err != nil {
		return matches
	}
	rx, err := regexp.Compile(expr)
	if err != nil {
		return matches
	}
	return slices.DeleteFunc(matches, func(m string) bool { return rx.MatchString(m) != keep })
}

// trimDirSlashes drops the slash pathCandidates puts after directories:
// like Bash's, the matches are plain names, which get the slash when they go
// on the line.
func trimDirSlashes(matches []string) []string {
	for i, match := range matches {
		if len(match) > 1 {
			matches[i] = strings.TrimRight(match, "/")
		}
	}
	return matches
}

// variables returns the names of the shell's variables for which keep
// reports true.
func (g *completionGenerator) variables(keep func(expand.Variable) bool) []string {
	if g.env != nil {
		var names []string
		g.env.Each(func(name string, vr expand.Variable) bool {
			if vr.IsSet() && keep(vr) {
				names = append(names, name)
			}
			return true
		})
		slices.Sort(names)
		return slices.Compact(names)
	}
	return completionVariables(g.runner, keep)
}

// completionVariables returns the names of the shell's variables for which
// keep reports true.
func completionVariables(runner *interp.Runner, keep func(expand.Variable) bool) []string {
	if runner == nil {
		return nil
	}
	seen := make(map[string]bool)
	if runner.Env != nil {
		runner.Env.Each(func(name string, vr expand.Variable) bool {
			if vr.IsSet() && keep(vr) {
				seen[name] = true
			}
			return true
		})
	}
	// Runner.Vars holds the variables the shell set or unset since.
	for name, vr := range runner.Vars {
		if vr.IsSet() && keep(vr) {
			seen[name] = true
		} else {
			delete(seen, name)
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// actionMatches returns the names of kind action that start with word.
func (g *completionGenerator) actionMatches(action, word string) []string {
	var names []string
	switch action {
	case "command":
		return g.complete.commandCandidates(word)
	case "file":
		return trimDirSlashes(g.complete.pathCandidates(word, false))
	case "directory":
		return trimDirSlashes(g.complete.pathCandidates(word, true))
	case "hostname":
		return g.complete.hostCandidates(word)
	case "alias":
		if g.aliases != nil {
			names = g.aliases.names()
		}
	case "builtin", "enabled", "helptopic":
		names = slices.Sorted(slices.Values(defaultCommandNames))
	case "keyword":
		names = defaultKeywordNames
	case "function":
		if g.runner != nil {
			names = slices.Sorted(maps.Keys(g.runner.Funcs))
		}
	case "variable":
		names = g.variables(func(expand.Variable) bool { return true })
	case "export":
		names = g.variables(func(vr expand.Variable) bool { return vr.Exported })
	case "arrayvar":
		names = g.variables(func(vr expand.Variable) bool {
			return vr.Kind == expand.Indexed || vr.Kind == expand.Associative
		})
	case "setopt":
		names = append(shellOptionNames(true), "histexpand", "history")
		slices.Sort(names)
	case "shopt":
		names = slices.Sorted(slices.Values(shellOptionNames(false)))
	case "signal":
		names = completionSignalNames
	case "binding":
		for _, entry := range bindActions {
			names = append(names, entry.names[0])
		}
	case "user":
		names = systemNames("/etc/passwd")
	case "group":
		names = systemNames("/etc/group")
	}
	var matches []string
	for _, name := range names {
		if strings.HasPrefix(name, word) {
			matches = append(matches, name)
		}
	}
	return matches
}

// completionSignalNames are the signals "complete -A signal" offers.
var completionSignalNames = []string{
	"SIGABRT", "SIGALRM", "SIGBUS", "SIGCHLD", "SIGCONT", "SIGFPE", "SIGHUP",
	"SIGILL", "SIGINT", "SIGIO", "SIGKILL", "SIGPIPE", "SIGPROF", "SIGPWR",
	"SIGQUIT", "SIGSEGV", "SIGSTKFLT", "SIGSTOP", "SIGSYS", "SIGTERM",
	"SIGTRAP", "SIGTSTP", "SIGTTIN", "SIGTTOU", "SIGURG", "SIGUSR1",
	"SIGUSR2", "SIGVTALRM", "SIGWINCH", "SIGXCPU", "SIGXFSZ",
}

// systemNames returns the names in the first field of a colon-separated
// system database such as /etc/passwd, sorted.
func systemNames(path string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, ":"); ok && name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// runFunction runs the completion function of spec with COMP_WORDS,
// COMP_CWORD, COMP_LINE and COMP_POINT set, and returns COMPREPLY and the
// function's exit status.
func (g *completionGenerator) runFunction(spec *completionSpec, req completionRequest) ([]string, int) {
	var b strings.Builder
	b.WriteString("COMP_WORDS=(")
	for _, word := range req.words {
		b.WriteString(shellSingleQuote(word))
		b.WriteByte(' ')
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "COMP_CWORD=%d\nCOMP_LINE=%s\nCOMP_POINT=%d\nCOMP_TYPE=9\nCOMP_KEY=9\n", req.cword, shellSingleQuote(req.line), req.point)
	// Like Bash, the function gets the command name, the word being
	// completed and the word before it.
	fmt.Fprintf(&b, "COMPREPLY=()\n%s %s %s %s\n", shellSingleQuote(spec.funcName),
		shellSingleQuote(req.command), shellSingleQuote(req.word), shellSingleQuote(previousWord(req)))
	b.WriteString("__gosh_status=$?\n")
	fmt.Fprintf(&b, "printf '%s%%s\\n' \"$__gosh_status\"\n", programmableReplyMarker)
	b.WriteString(`printf '%s\n' "${COMPREPLY[@]-}"` + "\n")
	out, _ := runSubshell(g.ctx, g.runner, g.stdin, g.stderr, b.String())
	_, after, ok := strings.Cut(out, programmableReplyMarker)
	if !ok {
		return nil, 1
	}
	statusLine, rest, _ := strings.Cut(after, "\n")
	status, _ := strconv.Atoi(statusLine)
	var replies []string
	for _, reply := range strings.Split(rest, "\n") {
		if reply != "" {
			replies = append(replies, reply)
		}
	}
	return replies, status
}

// runCommand runs the -C command of spec with the command name, the word
// and the word before it as arguments and COMP_LINE and COMP_POINT in its
// environment, and returns the lines it prints.
func (g *completionGenerator) runCommand(spec *completionSpec, req completionRequest) []string {
	script := fmt.Sprintf("COMP_LINE=%s COMP_POINT=%d COMP_TYPE=9 COMP_KEY=9 %s %s %s %s",
		shellSingleQuote(req.line), req.point, spec.command,
		shellSingleQuote(req.command), shellSingleQuote(req.word), shellSingleQuote(previousWord(req)))
	out, _ := runSubshell(g.ctx, g.runner, g.stdin, g.stderr, script)
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func previousWord(req completionRequest) string {
	if req.cword > 0 && req.cword <= len(req.words) {
		return req.words[req.cword-1]
	}
	return ""
}

const programmableReplyMarker = "\x01gosh-completion-reply\x02"

// shellSingleQuote renders value as a safely quotable single argument.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func (c *autoCompleter) generator() *completionGenerator {
	return &completionGenerator{ctx: c.ctx, runner: c.runner, stdin: c.stdin, stderr: c.stderr, complete: c, aliases: c.aliases}
}

type programmableResult struct {
	candidates []string
	handled    bool
	options    completionSpecOptions
	// files reports that the matches are file names, which get a slash
	// when they name a directory.
	files bool
}

// programmableCompletion completes the line with a specification, as Bash
// does: the one for an empty line, the one for the command word, or the one
// for the command being given arguments, falling back to the default one.
// A completion function returning 124 after defining a specification for
// the command, as bash-completion's loader does, is retried with it.
func (c *autoCompleter) programmableCompletion(ctx completionContext, line []rune, pos int) programmableResult {
	reg := c.completion
	if reg == nil || c.opts == nil || !shoptEnabled(c.opts, "progcomp") {
		return programmableResult{}
	}
	var spec *completionSpec
	switch {
	case ctx.isCommand && ctx.prefix == "" && strings.TrimSpace(string(line[:pos])) == "":
		spec = reg.spec(completionEmptySpec)
	case ctx.isCommand:
		spec = reg.spec(completionInitialSpec)
	default:
		spec = reg.lookup(ctx.command)
	}
	if spec == nil {
		return programmableResult{}
	}

	words := append([]string(nil), ctx.words...)
	if ctx.inWord {
		words = append(words, ctx.prefix)
	} else {
		words = append(words, "")
	}
	cword := max(len(words)-1, 0)
	command := ctx.command
	if command == "" && len(words) > 0 {
		command = words[0]
	}
	req := completionRequest{command: command, words: words, cword: cword, word: ctx.prefix, line: string(line), point: len(string(line[:pos]))}

	var candidates []string
	for retried := false; ; retried = true {
		active := reg.begin(spec, ctx.command, words, cword)
		var status int
		candidates, status = c.generator().generate(active, req)
		reg.end()
		spec = active
		if status != 124 || retried || ctx.isCommand {
			break
		}
		next := reg.lookup(ctx.command)
		if next == nil {
			break
		}
		spec = next
	}
	if len(candidates) == 0 && (spec.options.defaulted || spec.options.bashdefault) {
		return programmableResult{}
	}
	if !spec.options.nosort {
		slices.Sort(candidates)
		candidates = slices.Compact(candidates)
	}
	files := spec.options.filenames || spec.options.dirnames || spec.options.plusdirs ||
		spec.hasAction("file") || spec.hasAction("directory")
	return programmableResult{candidates: candidates, handled: true, options: spec.options, files: files}
}

// compgenMatches generates the matches compgen prints for the
// specification and word its arguments give.
func compgenMatches(deps callDeps, ctx context.Context, parsed *completeArgs) []string {
	word := ""
	if len(parsed.names) > 0 {
		word = parsed.names[0]
	}
	hc := interp.HandlerCtx(ctx)
	if parsed.spec.command != "" {
		fmt.Fprintln(hc.Stderr, "compgen: warning: -C option may not work as you expect")
	}
	if parsed.spec.funcName != "" {
		fmt.Fprintln(hc.Stderr, "compgen: warning: -F option may not work as you expect")
	}
	completer := &autoCompleter{ctx: ctx, runner: deps.runner(), stdin: hc.Stdin, stderr: hc.Stderr, aliases: deps.aliases}
	gen := completer.generator()
	gen.env = hc.Env
	matches, _ := gen.generate(parsed.spec, completionRequest{words: []string{word}, word: word})
	return matches
}

// builtinCompgen implements compgen: print the matches of the
// specification its options make for the word. It fails when there are
// none.
func builtinCompgen(deps callDeps, ctx context.Context, args []string, out io.Writer) error {
	parsed, err := parseCompleteArgs("compgen", args)
	if err != nil {
		return err
	}
	if !parsed.specGiven {
		// Like Bash, nothing to generate is no failure.
		return nil
	}
	matches := compgenMatches(deps, ctx, parsed)
	for _, match := range matches {
		fmt.Fprintln(out, match)
	}
	if len(matches) == 0 {
		return interp.ExitStatus(1)
	}
	return nil
}

// builtinCompopt implements compopt: change the -o options of the running
// completion, or of the specifications of the names given, or print them.
func builtinCompopt(deps callDeps, args []string, out io.Writer) error {
	type change struct {
		name string
		on   bool
	}
	var changes []change
	var names []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-o" || arg == "+o":
			if i+1 >= len(args) {
				return builtinUsagef("compopt: %s: option requires an argument\n%s", arg, compoptUsage)
			}
			i++
			changes = append(changes, change{args[i], arg[0] == '-'})
		case len(arg) > 2 && (arg[0] == '-' || arg[0] == '+') && arg[1] == 'o':
			changes = append(changes, change{arg[2:], arg[0] == '-'})
		case arg == "-D" || arg == "-E" || arg == "-I":
			names = append(names, "\x00"+arg[1:])
		case len(arg) > 1 && arg[0] == '-':
			return builtinUsagef("compopt: %s: invalid option\n%s", arg, compoptUsage)
		default:
			names = append(names, arg)
		}
	}
	for _, c := range changes {
		if !setCompletionOption(&completionSpec{}, c.name, true) {
			return builtinUsagef("compopt: %s: invalid option name", c.name)
		}
	}
	apply := func(spec *completionSpec) {
		for _, c := range changes {
			setCompletionOption(spec, c.name, c.on)
		}
	}
	show := func(name string, spec *completionSpec) {
		var b strings.Builder
		b.WriteString("compopt")
		for _, option := range completionOptions {
			sign := "+"
			if *option.field(&spec.options) {
				sign = "-"
			}
			b.WriteString(" " + sign + "o " + option.name)
		}
		if name != "" {
			b.WriteString(" " + name)
		}
		fmt.Fprintln(out, b.String())
	}
	if len(names) == 0 {
		spec := deps.completion.activeSpec()
		if spec == nil {
			return fmt.Errorf("compopt: not currently executing completion function")
		}
		if len(changes) == 0 {
			show("", spec)
		}
		apply(spec)
		return nil
	}
	var failed error
	for _, name := range names {
		spec := deps.completion.spec(name)
		if spec == nil {
			failed = fmt.Errorf("compopt: %s: no completion specification", completionSpecName(name))
			continue
		}
		if len(changes) == 0 {
			show(completionSpecName(name), spec)
		}
		apply(spec)
	}
	return failed
}

const (
	completeUsage = "complete: usage: complete [-abcdefgjksuv] [-pr] [-DEI] [-o option] [-A action] [-G globpat] [-W wordlist] [-F function] [-C command] [-X filterpat] [-P prefix] [-S suffix] [name ...]"
	compgenUsage  = "compgen: usage: compgen [-V varname] [-abcdefgjksuv] [-o option] [-A action] [-G globpat] [-W wordlist] [-F function] [-C command] [-X filterpat] [-P prefix] [-S suffix] [word]"
	compoptUsage  = "compopt: usage: compopt [-o|+o option] [-DEI] [name ...]"
)

func completeUsagef(format string, args ...any) error {
	return builtinUsagef("complete: "+format+"\n"+completeUsage, args...)
}

func compgenUsagef(format string, args ...any) error {
	return builtinUsagef("compgen: "+format+"\n"+compgenUsage, args...)
}
