package gosh

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// completionSpec is the gosh equivalent of a Bash "complete" specification.
// The supported subset covers function and word-list specifications plus the
// commonly used display options; it is deliberately narrow so behavior stays
// predictable and testable.
type completionSpec struct {
	funcName string
	words    []string
	actions  []string
	prefix   string
	suffix   string
	options  completionSpecOptions
}

type completionSpecOptions struct {
	nospace     bool
	dirnames    bool
	filenames   bool
	plusdirs    bool
	bashdefault bool
	defaulted   bool
}

type completionInvocation struct {
	spec    *completionSpec
	command string
	words   []string
	cword   int
}

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

func (r *completionRegistry) spec(command string) *completionSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[command]
}

func (r *completionRegistry) sortedSpecs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	commands := make([]string, 0, len(r.specs))
	for command := range r.specs {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

func (r *completionRegistry) begin(spec *completionSpec, command string, words []string, cword int) {
	r.mu.Lock()
	r.active = &completionInvocation{spec: spec, command: command, words: words, cword: cword}
	r.mu.Unlock()
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

func setCompletionOption(spec *completionSpec, name string, enabled bool) error {
	switch name {
	case "nospace":
		spec.options.nospace = enabled
	case "dirnames":
		spec.options.dirnames = enabled
	case "filenames":
		spec.options.filenames = enabled
	case "plusdirs":
		spec.options.plusdirs = enabled
	case "bashdefault":
		spec.options.bashdefault = enabled
	case "default":
		spec.options.defaulted = enabled
	default:
		return fmt.Errorf("unsupported completion option %q", name)
	}
	return nil
}

func completionOptionNames(opts completionSpecOptions) []string {
	var names []string
	if opts.nospace {
		names = append(names, "nospace")
	}
	if opts.dirnames {
		names = append(names, "dirnames")
	}
	if opts.filenames {
		names = append(names, "filenames")
	}
	if opts.plusdirs {
		names = append(names, "plusdirs")
	}
	if opts.bashdefault {
		names = append(names, "bashdefault")
	}
	if opts.defaulted {
		names = append(names, "default")
	}
	return names
}

func parseCompleteArgs(args []string) (remove, list bool, command string, spec *completionSpec, err error) {
	spec = &completionSpec{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-r":
			remove = true
		case "-p":
			list = true
		case "-F":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -F requires a function name")
			}
			i++
			spec.funcName = args[i]
		case "-W":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -W requires a word list")
			}
			i++
			spec.words = splitCompletionWordList(args[i])
		case "-A":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -A requires an action")
			}
			i++
			spec.actions = append(spec.actions, args[i])
		case "-P":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -P requires a prefix")
			}
			i++
			spec.prefix = args[i]
		case "-S":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -S requires a suffix")
			}
			i++
			spec.suffix = args[i]
		case "-o":
			if i+1 >= len(args) {
				return false, false, "", nil, fmt.Errorf("complete: -o requires an option")
			}
			i++
			if err := setCompletionOption(spec, args[i], true); err != nil {
				return false, false, "", nil, err
			}
		case "-C", "-G", "-X", "-D", "-E":
			return false, false, "", nil, fmt.Errorf("complete: unsupported flag %q", arg)
		default:
			if strings.HasPrefix(arg, "-") {
				return false, false, "", nil, fmt.Errorf("complete: invalid option %q", arg)
			}
			if command != "" {
				return false, false, "", nil, fmt.Errorf("complete: too many command names")
			}
			command = arg
		}
	}
	return remove, list, command, spec, nil
}

func splitCompletionWordList(list string) []string {
	// Bash performs normal word splitting on -W lists. Fields handles the
	// overwhelmingly common case; shell quoting of individual entries is not
	// re-parsed here.
	return strings.Fields(list)
}

func printCompletionSpec(w io.Writer, command string, spec *completionSpec) {
	fmt.Fprint(w, "complete")
	for _, opt := range completionOptionNames(spec.options) {
		fmt.Fprintf(w, " -o %s", opt)
	}
	if spec.funcName != "" {
		fmt.Fprintf(w, " -F %s", spec.funcName)
	}
	if len(spec.words) > 0 {
		fmt.Fprintf(w, " -W '%s'", strings.Join(spec.words, " "))
	}
	for _, action := range spec.actions {
		fmt.Fprintf(w, " -A %s", action)
	}
	if spec.prefix != "" {
		fmt.Fprintf(w, " -P '%s'", spec.prefix)
	}
	if spec.suffix != "" {
		fmt.Fprintf(w, " -S '%s'", spec.suffix)
	}
	fmt.Fprintf(w, " %s\n", command)
}

func builtinComplete(deps callDeps, args []string, out io.Writer) error {
	remove, list, command, spec, err := parseCompleteArgs(args)
	if err != nil {
		return err
	}
	if remove {
		if command == "" {
			return fmt.Errorf("complete: -r requires a command name")
		}
		if !deps.completion.remove(command) {
			return fmt.Errorf("complete: no completion specification for %q", command)
		}
		return nil
	}
	if list {
		if command != "" {
			current := deps.completion.spec(command)
			if current == nil {
				return fmt.Errorf("complete: no completion specification for %q", command)
			}
			printCompletionSpec(out, command, current)
			return nil
		}
		for _, name := range deps.completion.sortedSpecs() {
			printCompletionSpec(out, name, deps.completion.spec(name))
		}
		return nil
	}
	if command == "" {
		return fmt.Errorf("complete: missing command name")
	}
	deps.completion.set(command, spec)
	return nil
}

func builtinCompgen(deps callDeps, ctx context.Context, args []string, out io.Writer) error {
	var (
		wordList, action, prefix, suffix, word string
		seenWord                               bool
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-W":
			if i+1 >= len(args) {
				return fmt.Errorf("compgen: -W requires a word list")
			}
			i++
			wordList = args[i]
		case "-A":
			if i+1 >= len(args) {
				return fmt.Errorf("compgen: -A requires an action")
			}
			i++
			action = args[i]
		case "-P":
			if i+1 >= len(args) {
				return fmt.Errorf("compgen: -P requires a prefix")
			}
			i++
			prefix = args[i]
		case "-S":
			if i+1 >= len(args) {
				return fmt.Errorf("compgen: -S requires a suffix")
			}
			i++
			suffix = args[i]
		case "-C", "-G", "-X":
			return fmt.Errorf("compgen: unsupported flag %q", arg)
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("compgen: invalid option %q", arg)
			}
			if seenWord {
				return fmt.Errorf("compgen: too many word arguments")
			}
			word = arg
			seenWord = true
		}
	}

	var candidates []string
	switch {
	case wordList != "":
		candidates = splitCompletionWordList(wordList)
	case action != "":
		candidates = compgenActionCandidates(deps, ctx, action)
	default:
		return nil
	}
	for _, candidate := range candidates {
		if word != "" && !strings.HasPrefix(candidate, word) {
			continue
		}
		fmt.Fprintf(out, "%s%s%s\n", prefix, candidate, suffix)
	}
	return nil
}

func compgenActionCandidates(deps callDeps, ctx context.Context, action string) []string {
	runner := deps.runner()
	hc := interp.HandlerCtx(ctx)
	completer := &autoCompleter{ctx: ctx, runner: runner, stdin: hc.Stdin, stderr: hc.Stderr}
	switch action {
	case "command":
		return completer.commandCandidates("")
	case "file":
		return completer.pathCandidates("", false)
	case "directory":
		return completer.pathCandidates("", true)
	case "variable":
		var names []string
		if runner != nil && runner.Env != nil {
			runner.Env.Each(func(name string, _ expand.Variable) bool {
				names = append(names, name)
				return true
			})
		}
		slices.Sort(names)
		return names
	case "function":
		if runner == nil {
			return nil
		}
		names := make([]string, 0, len(runner.Funcs))
		for name := range runner.Funcs {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	case "hostname":
		return systemHostNames()
	case "builtin":
		return slices.Clone(defaultCommandNames)
	case "alias":
		return nil
	default:
		return nil
	}
}

func builtinCompopt(deps callDeps, args []string, out io.Writer) error {
	spec := deps.completion.activeSpec()
	if spec == nil {
		return fmt.Errorf("compopt: can only be called from a completion function")
	}
	if len(args) == 0 {
		names := completionOptionNames(spec.options)
		fmt.Fprintln(out, strings.Join(names, " "))
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-o" || arg == "+o" {
			if i+1 >= len(args) {
				return fmt.Errorf("compopt: %s requires an option name", arg)
			}
			i++
			if err := setCompletionOption(spec, args[i], arg[0] == '-'); err != nil {
				return err
			}
			continue
		}
		if len(arg) > 2 && (arg[0] == '-' || arg[0] == '+') && arg[1] == 'o' {
			if err := setCompletionOption(spec, arg[2:], arg[0] == '-'); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("compopt: invalid option %q", arg)
	}
	return nil
}

// shellSingleQuote renders value as a safely quotable single argument.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func completionFunctionScript(spec *completionSpec, command string, words []string, cword int, line string, point int) string {
	var b strings.Builder
	b.WriteString("COMP_WORDS=(")
	for _, word := range words {
		b.WriteString(shellSingleQuote(word))
		b.WriteByte(' ')
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "COMP_CWORD=%d\nCOMP_LINE=%s\nCOMP_POINT=%d\n", cword, shellSingleQuote(line), point)
	fmt.Fprintf(&b, "COMPREPLY=()\n%s", shellSingleQuote(spec.funcName))
	for _, word := range words {
		b.WriteByte(' ')
		b.WriteString(shellSingleQuote(word))
	}
	fmt.Fprintf(&b, "\nprintf '%s'\n", programmableReplyMarker)
	b.WriteString(`printf '%s\n' "${COMPREPLY[@]-}"` + "\n")
	return b.String()
}

const programmableReplyMarker = "\x01gosh-completion-reply\x02"

func runCompletionFunction(ctx context.Context, runner *interp.Runner, stdin io.Reader, stderr io.Writer, spec *completionSpec, command string, words []string, cword int, line string, point int) []string {
	script := completionFunctionScript(spec, command, words, cword, line, point)
	out, err := runSubshell(ctx, runner, stdin, stderr, script)
	if err != nil {
		return nil
	}
	before, after, ok := strings.Cut(out, programmableReplyMarker)
	if !ok {
		return nil
	}
	_ = before
	var replies []string
	for _, reply := range strings.Split(after, "\n") {
		if reply != "" {
			replies = append(replies, reply)
		}
	}
	return replies
}

type programmableResult struct {
	candidates []string
	handled    bool
	noSpace    bool
}

func (c *autoCompleter) programmableCompletion(ctx completionContext, line []rune, pos int) programmableResult {
	reg := c.completion
	if reg == nil || c.opts == nil || !shoptEnabled(c.opts, "progcomp") {
		return programmableResult{}
	}
	var spec *completionSpec
	if ctx.isCommand {
		spec = reg.spec(ctx.prefix)
	} else {
		spec = reg.spec(ctx.command)
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
	cword := len(words) - 1
	if cword < 0 {
		cword = 0
	}
	reg.begin(spec, ctx.command, words, cword)
	defer reg.end()

	var candidates []string
	if spec.funcName != "" {
		candidates = runCompletionFunction(c.ctx, c.runner, c.stdin, c.stderr, spec, ctx.command, words, cword, string(line), pos)
	}
	if len(candidates) == 0 {
		for _, action := range spec.actions {
			candidates = append(candidates, c.actionCandidates(action, ctx.prefix)...)
		}
		var wordCandidates []string
		for _, word := range spec.words {
			if strings.HasPrefix(word, ctx.prefix) {
				wordCandidates = append(wordCandidates, word)
			}
		}
		slices.Sort(wordCandidates)
		candidates = append(candidates, wordCandidates...)
	}
	if spec.options.plusdirs {
		candidates = append(candidates, c.pathCandidates(ctx.prefix, true)...)
	}
	if len(candidates) == 0 {
		if spec.options.defaulted || spec.options.bashdefault {
			return programmableResult{}
		}
		return programmableResult{handled: true, noSpace: spec.options.nospace}
	}
	for i, candidate := range candidates {
		candidates[i] = spec.prefix + candidate + spec.suffix
	}
	return programmableResult{candidates: candidates, handled: true, noSpace: spec.options.nospace}
}

func (c *autoCompleter) actionCandidates(action, prefix string) []string {
	filter := func(values []string) []string {
		var out []string
		for _, value := range values {
			if strings.HasPrefix(value, prefix) {
				out = append(out, value)
			}
		}
		slices.Sort(out)
		return out
	}
	switch action {
	case "command":
		return c.commandCandidates(prefix)
	case "file":
		return c.pathCandidates(prefix, false)
	case "directory":
		return c.pathCandidates(prefix, true)
	case "variable":
		var names []string
		if c.runner != nil && c.runner.Env != nil {
			c.runner.Env.Each(func(name string, _ expand.Variable) bool {
				names = append(names, name)
				return true
			})
		}
		return filter(names)
	case "function":
		if c.runner == nil {
			return nil
		}
		names := make([]string, 0, len(c.runner.Funcs))
		for name := range c.runner.Funcs {
			names = append(names, name)
		}
		return filter(names)
	case "hostname":
		return c.hostCandidates(prefix)
	case "builtin":
		return filter(defaultCommandNames)
	default:
		return nil
	}
}
