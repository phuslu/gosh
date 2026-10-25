// Package gosh is a compact Bash-style shell and embeddable shell runtime.
//
// It combines mvdan.cc/sh/v3 for shell parsing and interpretation with an
// in-tree readline implementation for interactive use, adding the pieces
// that make it usable as a Bash-flavored interactive shell: history,
// prompts, completion, and key bindings.
//
// Run executes one shell invocation. New returns a reusable Shell which can
// evaluate scripts and drive the interactive frontend independently; see
// Config and Shell for the available options.
package gosh

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/phuslu/gosh/internal/readline"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Config describes one gosh shell invocation.
type Config struct {
	Version       string
	Context       context.Context
	Args          []string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	Env           []string
	Dir           string
	NotifySignals bool
	IsTerminal    bool
	OnPromptReset func(context.Context)
	Backend       Backend
}

// Backend replaces the interpreter's process-execution and filesystem
// capabilities. A nil Config.Backend keeps gosh's host-process defaults.
// Implementations can provide a sandbox, mock filesystem, remote executor,
// or audit layer without changing shell semantics.
type Backend interface {
	Exec(ctx context.Context, args []string) error
	Open(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error)
	Stat(ctx context.Context, name string, followSymlinks bool) (fs.FileInfo, error)
	ReadDir(ctx context.Context, path string) ([]fs.DirEntry, error)
	Access(ctx context.Context, path string, mode interp.AccessMode) error
}

// Run executes one shell invocation. It is a convenience wrapper around New
// followed by Shell.Run, for callers that only need a single-shot CLI.
func Run(c Config) error {
	s, err := New(c)
	if err != nil {
		return err
	}
	return s.Run(c.Context)
}

// Shell is a reusable shell runtime. One Shell owns one interpreter, its
// option state, history, key bindings, and (when interactive) the readline
// frontend, so embedders can evaluate scripts and then attach a user
// interface without losing shell state.
type Shell struct {
	cfg         Config
	version     string
	args        []string
	stdin       io.Reader
	runnerStdin io.Reader
	stdout      io.Writer
	stderr      io.Writer
	env         []string
	dir         string
	command     *commandSpec
	interactive bool
	noop        bool
	// initExit is set when the startup file ran "exit"; the shell then ends
	// with initExitErr instead of starting.
	initExit    bool
	initExitErr error

	baseCtx context.Context

	parser      *syntax.Parser
	runner      *interp.Runner
	history     *history
	bindings    *keyBindingManager
	completion  *completionRegistry
	opts        *shellOptions
	aliases     *aliasTable
	exitTrap    *exitTrap
	promptCache *promptCache
	rl          *readline.Instance

	// scriptName names the script or startup file running, for errors.
	scriptName string

	hostname     string
	homeFallback string
	userFallback string
}

// New creates a Shell from cfg. The interpreter, default bindings, and (for
// interactive shells) the init file are prepared eagerly so that Eval and
// Interactive can share the same state.
func New(c Config) (*Shell, error) {
	s := &Shell{cfg: c, dir: c.Dir}
	s.version = c.Version
	if s.version == "" {
		s.version = "0.0.0"
	}

	s.args = c.Args
	if len(s.args) == 0 {
		s.args = []string{"gosh"}
	}
	s.stdin = c.Stdin
	if s.stdin == nil {
		s.stdin = strings.NewReader("")
	}
	s.runnerStdin = s.stdin
	s.stdout = c.Stdout
	if s.stdout == nil {
		s.stdout = io.Discard
	}
	s.stderr = c.Stderr
	if s.stderr == nil {
		s.stderr = io.Discard
	}
	s.env = environWithDefaultShell(c.Env)

	command, err := parseCommand(s.args)
	if err != nil {
		return nil, err
	}
	s.command = command
	// A command string or script file is never interactive, like Bash.
	hasCommand := command.isCommand || command.scriptFile != ""
	s.interactive = (c.IsTerminal || command.interactive) && !hasCommand
	if command.showVersion {
		fmt.Fprintf(s.stdout, "gosh %s\n", s.version)
		s.noop = true
		return s, nil
	}
	if command.showHelp {
		fmt.Fprint(s.stdout, goshUsage)
		s.noop = true
		return s, nil
	}
	if s.interactive {
		s.env = SetEnv(s.env, "GOSH_INTERACTIVE", "1")
		if _, ok := s.stdin.(*os.File); !ok {
			// Only readline consumes an arbitrary interactive reader. Sharing
			// it with interp.StdIO would start a second copy goroutine over
			// a source which need not be concurrency-safe.
			s.runnerStdin = strings.NewReader("")
		}
	}
	if !hasCommand && !s.interactive {
		s.runnerStdin = strings.NewReader("")
	}

	s.baseCtx = c.Context
	if s.baseCtx == nil {
		s.baseCtx = context.Background()
	}
	s.hostname, _ = lookupEnv(s.env, "HOSTNAME")
	if s.hostname == "" {
		s.hostname, _ = os.Hostname()
	}
	if s.hostname == "" {
		s.hostname = "localhost"
	}
	s.homeFallback, _ = lookupEnv(s.env, "HOME")
	if s.homeFallback == "" {
		s.homeFallback, _ = os.UserHomeDir()
	}
	s.userFallback, _ = lookupEnv(s.env, "USER")
	if s.userFallback == "" {
		s.userFallback = strconv.Itoa(os.Getuid())
	}

	if err := s.initialize(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Shell) initialize() error {
	opts := []interp.RunnerOption{
		// Like Bash, only an interactive shell expands aliases by default;
		// scripts can still opt in with "shopt -s expand_aliases".
		interp.Interactive(s.interactive),
		interp.StdIO(s.runnerStdin, s.stdout, s.stderr),
		interp.Env(expand.ListEnviron(s.env...)),
	}
	if s.dir != "" {
		opts = append(opts, interp.Dir(s.dir))
	}
	if backend := s.cfg.Backend; backend != nil {
		opts = append(opts,
			interp.OpenHandler(backend.Open),
			interp.ReadDirHandler2(backend.ReadDir),
			interp.StatHandler(backend.Stat),
			interp.AccessHandler(backend.Access),
		)
	}

	s.parser = syntax.NewParser()
	s.history = &history{cfg: historyConfig{inMemoryLimit: defaultHistoryLimit}}
	s.bindings = &keyBindingManager{entries: make(map[string]*goKeyBindingEntry)}
	s.completion = newCompletionRegistry()
	s.opts = newShellOptions(s.interactive)
	s.aliases = newAliasTable()
	s.exitTrap = &exitTrap{}
	s.promptCache = newPromptCache()

	deps := callDeps{
		runner:      func() *interp.Runner { return s.runner },
		history:     s.history,
		bindings:    s.bindings,
		completion:  s.completion,
		opts:        s.opts,
		aliases:     s.aliases,
		exitTrap:    s.exitTrap,
		hasBackend:  s.cfg.Backend != nil,
		start:       time.Now(),
		errorPrefix: s.errorPrefix,
	}
	opts = append(opts, interp.CallHandler(callHandler(deps)))
	opts = append(opts, interp.ExecHandlers(execHandler(deps, backendExec(s.cfg.Backend))))

	runner, err := interp.New(opts...)
	if err != nil {
		return err
	}
	s.runner = runner
	installShellOptionVariable(runner, s.version)
	if s.interactive {
		// Non-interactive runs apply these after resetting the runner; an
		// interactive shell never resets, so the startup file sees them.
		if err := s.applySetArgs(); err != nil {
			return err
		}
	}

	// Default key bindings. Home/End in their various encodings (\e[1~, \e[4~,
	// \e[H, \e[F, \eOH, \eOF) are already decoded by the readline terminal, so
	// only PageUp/PageDown need a binding here.
	for _, binding := range []string{
		`"\e[5~": previous-history`,
		`"\e[6~": next-history`,
	} {
		if err := s.bindings.handleBind([]string{binding}, io.Discard); err != nil {
			return err
		}
	}

	// Source the interactive init file.
	if s.interactive && !s.command.noRC {
		file := s.command.rcFile
		if file == "" {
			file = resolveInitFile(s.env, true)
		} else {
			file = expandEnv(s.env, file)
		}
		if fileHandle, err := os.Open(file); err == nil {
			s.runInitFile(s.baseCtx, fileHandle)
			fileHandle.Close()
		}
	}
	return nil
}

func (s *Shell) runContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = s.baseCtx
	}
	if !s.cfgNotifySignals() {
		return ctx, func() {}
	}
	signals := []os.Signal{syscall.SIGTERM}
	if !s.interactive {
		signals = append(signals, os.Interrupt)
	}
	return signal.NotifyContext(ctx, signals...)
}

func (s *Shell) cfgNotifySignals() bool {
	return s.cfg.NotifySignals
}

// Run executes the shell invocation described by Config. When ctx is nil,
// Config.Context (or context.Background) is used.
func (s *Shell) Run(ctx context.Context) error {
	if s.noop {
		return nil
	}
	if s.initExit {
		return s.initExitErr
	}
	ctx, cancel := s.runContext(ctx)
	defer cancel()
	if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
		s.cfg.OnPromptReset(ctx)
	}
	switch {
	case s.command.isCommand:
		return s.runCommand(ctx)
	case s.command.scriptFile != "":
		return s.runScriptFile(ctx)
	case s.interactive:
		return s.runInteractive(ctx)
	default:
		return s.runStdinScript(ctx)
	}
}

// resetForInvocation prepares the runner for one non-interactive
// invocation: the command-line shell options and positional parameters
// apply on top of a freshly reset interpreter.
func (s *Shell) resetForInvocation() error {
	s.runner.Reset()
	s.opts.reset(s.interactive)
	s.aliases.reset()
	if err := s.applySetArgs(); err != nil {
		return err
	}
	if len(s.command.params) != 0 {
		s.runner.Params = append([]string(nil), s.command.params...)
	} else {
		s.runner.Params = nil
	}
	return nil
}

// applySetArgs applies the "set"-style options from the command line, such
// as -e or -o pipefail, and mirrors them into the shopt/$- option state.
func (s *Shell) applySetArgs() error {
	if len(s.command.setArgs) == 0 {
		return nil
	}
	if err := interp.Params(s.command.setArgs...)(s.runner); err != nil {
		return &usageError{msg: "gosh: " + err.Error()}
	}
	s.opts.recordSet(s.command.setArgs)
	return nil
}

// Eval parses and evaluates script in the Shell's interpreter, preserving
// variables, functions, options, and working directory for later calls.
func (s *Shell) Eval(ctx context.Context, script string) error {
	if s.noop {
		return fmt.Errorf("gosh: shell was created for a metadata-only invocation")
	}
	if ctx == nil {
		ctx = s.baseCtx
	}
	prog, err := s.parser.Parse(strings.NewReader(script), "")
	if err != nil {
		return err
	}
	s.expandAliases(prog.Stmts...)
	return s.runStatements(ctx, prog.Stmts)
}

// runStatements runs stmts and returns the result of the last one run,
// stopping once the shell exits. Unlike running them as a File, which interp
// treats as a whole script, this does not run the EXIT trap, so more code
// can run in the shell afterwards.
func (s *Shell) runStatements(ctx context.Context, stmts []*syntax.Stmt) error {
	var err error
	for _, stmt := range stmts {
		err = s.runner.Run(ctx, stmt)
		if s.runner.Exited() {
			break
		}
	}
	return err
}

// syncHistoryConfig applies changes to the history variables made by the
// commands just run, as Bash does when they are assigned.
func (s *Shell) syncHistoryConfig() {
	cfg := shellHistoryConfig(s.runner)
	if s.rl != nil {
		rlCfg := s.rl.GetConfig()
		if limit := readlineHistoryLimit(cfg.inMemoryLimit); rlCfg.HistoryLimit != limit {
			rlCfg.HistoryLimit = limit
			_ = s.rl.SetConfig(rlCfg)
		}
	}
	s.history.configure(cfg)
}

// runBindCommand runs the shell command of a "bind -x" binding as Bash
// does: off the line being edited, with the terminal out of raw mode, and
// with READLINE_LINE and READLINE_POINT holding the line and the cursor's
// byte offset, which the command may change. It runs while readline waits
// for input, so the interpreter is otherwise idle.
func (s *Shell) runBindCommand(ctx context.Context, rl *readline.Instance, edit *historySearch, command string) {
	prog, err := syntax.NewParser().Parse(strings.NewReader(command), "bind -x")
	if err != nil {
		fmt.Fprintln(rl.Stderr(), "gosh:", err)
		return
	}
	line, pos := edit.current()
	pos = min(max(pos, 0), len(line))
	cfg := rl.GetConfig()
	_ = cfg.FuncExitRaw()
	// Clear the line on the terminal itself: rl.Stdout would redraw the
	// prompt after it.
	fmt.Fprint(cfg.Stdout, "\r\x1b[K")
	quoted, _ := syntax.Quote(string(line), syntax.LangBash)
	_ = s.runner.Run(ctx, parseStmt(fmt.Sprintf("READLINE_LINE=%s READLINE_POINT=%d 2>&-", quoted, len(string(line[:pos])))))
	s.expandAliases(prog.Stmts...)
	if err := s.runStatements(ctx, prog.Stmts); err != nil && !IsExitStatus(err) {
		fmt.Fprintln(rl.Stderr(), err)
	}
	newLine, _ := runnerStringVar(s.runner, "READLINE_LINE")
	point, _ := runnerStringVar(s.runner, "READLINE_POINT")
	_ = s.runner.Run(ctx, parseStmt("unset READLINE_LINE READLINE_POINT 2>&-"))
	_ = cfg.FuncMakeRaw()
	offset, err := strconv.Atoi(point)
	if err != nil || offset < 0 {
		offset = 0
	}
	offset = min(offset, len(newLine))
	edit.replace([]rune(newLine), utf8.RuneCountInString(newLine[:offset]))
}

// errorPrefix is what Bash puts before an error at pos: "gosh: " in an
// interactive shell, and the script's name and the line otherwise.
func (s *Shell) errorPrefix(pos syntax.Pos) string {
	if s.rl != nil {
		return "gosh: "
	}
	name := s.scriptName
	if name == "" {
		name = "gosh"
	}
	if line := pos.Line(); line > 0 {
		return fmt.Sprintf("%s: line %d: ", name, line)
	}
	return name + ": "
}

// runPromptCommand runs PROMPT_COMMAND before a primary prompt as Bash does:
// in the shell itself, each element in turn when it is an array, and with
// $? left at status, the status of the last command line. It reports
// whether the commands made the shell exit.
func (s *Shell) runPromptCommand(ctx context.Context, status int) (bool, error) {
	vr, ok := lookupRunnerVar(s.runner, "PROMPT_COMMAND")
	if !ok || !vr.IsSet() {
		return false, nil
	}
	cmds := []string{vr.String()}
	if vr.Kind == expand.Indexed {
		cmds = vr.List
	}
	ran := false
	for _, cmd := range cmds {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "PROMPT_COMMAND")
		if err != nil {
			fmt.Fprintln(s.stderr, "gosh:", err)
			continue
		}
		s.expandAliases(prog.Stmts...)
		ran = true
		err = s.runStatements(ctx, prog.Stmts)
		if s.runner.Exited() {
			return true, err
		}
		if err != nil && !IsExitStatus(err) {
			fmt.Fprintln(s.stderr, err)
		}
	}
	if ran {
		_ = s.runner.Run(ctx, statusStmt(status))
	}
	return false, nil
}

// expandAliases applies Bash's recursive alias expansion to freshly parsed
// code when alias expansion is on; see aliasTable.
func (s *Shell) expandAliases(stmts ...*syntax.Stmt) {
	if on, _ := s.opts.enabled(false, "expand_aliases"); !on {
		return
	}
	for _, stmt := range stmts {
		s.aliases.expand(stmt)
	}
}

// Interactive starts the readline-driven interactive frontend on the
// Shell's current interpreter state.
func (s *Shell) Interactive(ctx context.Context) error {
	if s.noop {
		return fmt.Errorf("gosh: shell was created for a metadata-only invocation")
	}
	if s.initExit {
		return s.initExitErr
	}
	ctx, cancel := s.runContext(ctx)
	defer cancel()
	if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
		s.cfg.OnPromptReset(ctx)
	}
	if !s.interactive {
		// A shell created for scripts starts with alias expansion off;
		// attaching the interactive frontend turns it on, as for a shell
		// that was interactive from the start.
		if err := interp.Interactive(true)(s.runner); err != nil {
			return err
		}
		s.opts.set(false, "expand_aliases", true)
		s.opts.setHistExpand(true)
	}
	return s.runInteractive(ctx)
}

func (s *Shell) runCommand(ctx context.Context) error {
	if err := s.resetForInvocation(); err != nil {
		return err
	}
	return s.runScript(ctx, newScriptReader(strings.NewReader(s.command.script)), s.command.argv0)
}

// runScriptFile runs "gosh script [args...]": $0 is the script path and the
// shell's own stdin stays available to the script's commands.
func (s *Shell) runScriptFile(ctx context.Context) error {
	file, err := os.Open(s.command.scriptFile)
	if err != nil {
		return fmt.Errorf("gosh: %w", err)
	}
	defer file.Close()
	if err := s.resetForInvocation(); err != nil {
		return err
	}
	return s.runScript(ctx, newScriptReader(file), s.command.scriptFile)
}

// runStdinScript runs the script read from stdin, which is also the stdin
// of the script's commands.
func (s *Shell) runStdinScript(ctx context.Context) error {
	stdin, closeStdin, err := scriptStdin(s.stdin)
	if err != nil {
		return err
	}
	defer closeStdin()
	if err := s.resetForInvocation(); err != nil {
		return err
	}
	if err := interp.StdIO(stdin, s.stdout, s.stderr)(s.runner); err != nil {
		return err
	}
	return s.runScript(ctx, newSharedScriptReader(stdin), s.command.argv0)
}

func (s *Shell) runInteractive(ctx context.Context) error {
	prompts := s.newPromptEnv()
	promptFallback := defaultPrompt(s.version)
	promptSeq := 1
	if exited, err := s.runPromptCommand(ctx, 0); exited {
		return err
	}
	currentPrompt := prompts.render(ctx, "PS1", promptFallback, promptSeq)

	installHistoryDefaults(s.runner)
	s.history.cfg = shellHistoryConfig(s.runner)
	s.history.onError = func(err error) {
		fmt.Fprintln(s.stderr, "history:", err)
	}
	histFile := s.history.cfg.file
	s.history.appendOnAdd = func() bool {
		return shoptEnabled(s.opts, "histappend")
	}

	conWriter := ptyNewConsoleANSIWriter(s.stderr)
	boundStdin := &keyBindingInput{src: s.stdin, mgr: s.bindings}
	promptPrinter := &promptPrinter{}
	completer := &autoCompleter{
		ctx:           ctx,
		runner:        s.runner,
		opts:          s.opts,
		completion:    s.completion,
		aliases:       s.aliases,
		stdin:         prompts.stdin,
		stdout:        conWriter,
		stderr:        conWriter,
		promptPrinter: promptPrinter,
		defaultHome:   s.homeFallback,
	}
	historySearch := &historySearch{history: s.history, searchIndex: -1}
	s.bindings.registerActionHandler(keyActionHistorySearchBackward, historySearch.Search)
	s.bindings.registerActionHandler(keyActionHistorySearchForward, historySearch.Search)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 currentPrompt.prompt,
		HistoryLimit:           readlineHistoryLimit(s.history.cfg.inMemoryLimit),
		DisableAutoSaveHistory: true,
		InterruptPrompt:        "^C",
		EOFPrompt:              "exit",
		Stdout:                 conWriter,
		Stderr:                 conWriter,
		AutoComplete:           completer,
		Listener:               historySearch.OnChange,
		Stdin:                  boundStdin,
		FuncGetSize:            terminalSize,
	})
	if err != nil {
		return err
	}
	s.rl = rl
	defer func() {
		_ = rl.Close()
		s.rl = nil
	}()

	s.history.resync = func() {
		rl.ResetHistory()
		for _, entry := range s.history.Entries() {
			_ = rl.SaveToHistory(entry)
		}
	}
	if err := s.history.LoadFile(histFile); err != nil {
		fmt.Fprintln(rl.Stderr(), "failed to load history:", err)
	}
	if truncateErr := s.history.TruncateFile(); truncateErr != nil {
		fmt.Fprintln(rl.Stderr(), "failed to truncate history:", truncateErr)
	}
	s.history.resync()
	completer.attach(rl)
	historySearch.Attach(rl)
	s.bindings.setCommandRunner(func(command string) {
		s.runBindCommand(ctx, rl, historySearch, command)
	})
	defer s.bindings.setCommandRunner(nil)
	readlineColumns := func() int {
		if width, _ := rl.GetConfig().FuncGetSize(); width > 0 {
			return width
		}
		return 0
	}
	updateCheckwinsizeColumns(s.opts, s.runner, readlineColumns)
	promptPrinter.Print(rl.Stdout(), currentPrompt.prefix)
	nextPrefix := ""
	setPrompt := func(parts promptParts) {
		rl.SetPrompt(parts.prompt)
		nextPrefix = parts.prefix
	}
	flushPrefix := func() {
		if nextPrefix == "" {
			return
		}
		promptPrinter.Print(rl.Stdout(), nextPrefix)
		nextPrefix = ""
	}
	// resetPrompt shows the primary prompt again. Like Bash's \#, the
	// command number advances only when a command ran.
	resetPrompt := func(ran bool) {
		if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
			// Windows consoles may lose VT mode after programs exit.
			s.cfg.OnPromptReset(ctx)
		}
		updateCheckwinsizeColumns(s.opts, s.runner, readlineColumns)
		if ran {
			promptSeq++
		}
		setPrompt(prompts.render(ctx, "PS1", defaultPrompt(s.version), promptSeq))
		flushPrefix()
	}

	// Context cancellation must interrupt a blocking Readline, otherwise an
	// embedder cannot stop an interactive session that is waiting at the
	// prompt. readline.Close is safe to call from another goroutine.
	interrupted := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rl.Close()
		case <-interrupted:
		}
	}()
	defer close(interrupted)

	// While a command runs the terminal is back in cooked mode, so ^C and
	// ^\ reach gosh as signals along with the foreground command. Like Bash,
	// SIGINT only interrupts the current line and SIGQUIT is ignored; neither
	// may end the session. Handlers, unlike signal.Ignore, are not inherited
	// by the commands gosh starts. ^Z must not stop the shell either; see
	// resumeStoppedCommands.
	var interrupts chan os.Signal
	if s.cfgNotifySignals() {
		interrupts = make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
		quits := make(chan os.Signal, 1)
		signal.Notify(quits, syscall.SIGQUIT)
		defer signal.Stop(quits)
		defer resumeStoppedCommands()()
	}

	// reader wraps readline so parser.InteractiveSeq can consume it as an
	// io.Reader. Each call to Read invokes Readline() to fetch one line.
	// Ctrl-C (ErrInterrupt) injects a newline to abandon the current
	// incomplete statement. Ctrl-D / EOF returns io.EOF to end the session.
	rdr := &reader{rl: rl, history: s.history, opts: s.opts}

	var exitErr error
	lastStatus := 0
	err = runInteractiveParser(s.parser, rdr, func(stmts []*syntax.Stmt) bool {
		// parser.Incomplete() returns true when the parser has consumed a
		// partial statement and is waiting for more input (e.g. open quotes,
		// unclosed if/for blocks). Switch to the continuation prompt and keep
		// reading without executing anything yet.
		if s.parser.Incomplete() {
			setPrompt(prompts.render(ctx, "PS2", "> ", promptSeq))
			flushPrefix()
			return true
		}

		rdr.savePendingHistory()
		s.expandAliases(stmts...)
		if len(stmts) > 0 {
			// Like Bash, PS0 shows after a command is read, before it runs.
			ps0 := prompts.render(ctx, "PS0", "", promptSeq)
			fmt.Fprint(rl.Stderr(), ps0.prefix+ps0.prompt)
		}
		cont, status, err := runInteractiveStatements(ctx, s.runner, stmts, rl.Stderr(), interrupts)
		if !cont {
			exitErr = err
			return false
		}
		if len(stmts) > 0 {
			lastStatus = status
		}
		s.syncHistoryConfig()
		if exited, err := s.runPromptCommand(ctx, lastStatus); exited {
			exitErr = err
			return false
		}

		// Restore the main prompt, updating it in case the effective UID
		// changed (e.g. via su).
		resetPrompt(len(stmts) > 0)
		return true
	}, func(err error) bool {
		rdr.savePendingHistory()
		fmt.Fprintln(rl.Stderr(), err.Error())
		// Like Bash, a syntax error sets $? to 2.
		lastStatus = 2
		_ = s.runner.Run(ctx, statusStmt(lastStatus))
		if exited, err := s.runPromptCommand(ctx, lastStatus); exited {
			exitErr = err
			return false
		}
		resetPrompt(true)
		return true
	})
	if err == nil && exitErr == nil && ctx.Err() == nil {
		// Like Bash, the end of input runs "exit": the shell runs its EXIT
		// trap and exits with the last command's status.
		exitErr = s.runner.Run(ctx, eofExitStmt)
	}
	if saveErr := s.history.SaveOnExit(); saveErr != nil {
		fmt.Fprintln(rl.Stderr(), "failed to save history:", saveErr)
	}
	if truncateErr := s.history.TruncateFile(); truncateErr != nil {
		fmt.Fprintln(rl.Stderr(), "failed to truncate history:", truncateErr)
	}
	if exitErr != nil {
		return exitErr
	}
	return err
}
