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

	baseCtx context.Context

	parser      *syntax.Parser
	runner      *interp.Runner
	history     *history
	bindings    *keyBindingManager
	completion  *completionRegistry
	opts        *shellOptions
	promptCache *promptCache
	rl          *readline.Instance

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
	hasCommand := command != nil && command.isCommand
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
		interp.Interactive(true),
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
	s.history = &history{limit: resolveHistoryLimit()}
	s.bindings = &keyBindingManager{entries: make(map[string]*goKeyBindingEntry)}
	s.completion = newCompletionRegistry()
	s.opts = newShellOptions(s.interactive)
	s.promptCache = newPromptCache()

	deps := callDeps{
		runner:     func() *interp.Runner { return s.runner },
		history:    s.history,
		bindings:   s.bindings,
		completion: s.completion,
		opts:       s.opts,
	}
	opts = append(opts, interp.CallHandler(callHandler(deps)))
	opts = append(opts, interp.ExecHandlers(execHandler(deps.runner, s.opts, backendExec(s.cfg.Backend))))

	runner, err := interp.New(opts...)
	if err != nil {
		return err
	}
	s.runner = runner
	installShellOptionVariable(runner, s.version)

	prog, err := s.parser.Parse(strings.NewReader(`
		bind '"\e[1~": beginning-of-line'
		bind '"\e[4~": end-of-line'
		bind '"\e[5~": previous-screen'
		bind '"\e[6~": next-screen'
		bind '"\e[F": end-of-line'
		bind '"\e[H": beginning-of-line'
		bind '"\eOF": end-of-line'
		bind '"\eOH": beginning-of-line'
	`), "")
	if err != nil {
		return err
	}
	if err := runner.Run(s.baseCtx, prog); err != nil {
		return err
	}

	// Source the interactive init file.
	if s.interactive && !s.command.noRC {
		file := s.command.rcFile
		if file == "" {
			file = resolveInitFile(s.env, true)
		} else {
			file = expandEnv(s.env, file)
		}
		fileHandle, err := os.Open(file)
		if err == nil {
			prog, err := s.parser.Parse(fileHandle, fileHandle.Name())
			if err != nil {
				fmt.Fprintln(s.stderr, "failed to parse", fileHandle.Name(), ":", err)
			} else if err := runner.Run(s.baseCtx, prog); err != nil {
				fmt.Fprintln(s.stderr, "failed to run", fileHandle.Name(), ":", err)
			}
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
	ctx, cancel := s.runContext(ctx)
	defer cancel()
	if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
		s.cfg.OnPromptReset(ctx)
	}
	switch {
	case s.command.isCommand:
		return s.runCommand(ctx)
	case s.interactive:
		return s.runInteractive(ctx)
	default:
		s.runner.Reset()
		s.opts.reset(s.interactive)
		if len(s.command.params) != 0 {
			s.runner.Params = append([]string(nil), s.command.params...)
		} else {
			s.runner.Params = nil
		}
		return runNonInteractiveStream(ctx, s.stdin, s.runner, s.stdout, s.stderr)
	}
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
	return s.runner.Run(ctx, prog)
}

// Interactive starts the readline-driven interactive frontend on the
// Shell's current interpreter state.
func (s *Shell) Interactive(ctx context.Context) error {
	if s.noop {
		return fmt.Errorf("gosh: shell was created for a metadata-only invocation")
	}
	ctx, cancel := s.runContext(ctx)
	defer cancel()
	if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
		s.cfg.OnPromptReset(ctx)
	}
	return s.runInteractive(ctx)
}

func (s *Shell) runCommand(ctx context.Context) error {
	script := s.command.script
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	prog, err := s.parser.Parse(strings.NewReader(script), s.command.argv0)
	if err != nil {
		return err
	}
	s.runner.Reset()
	s.opts.reset(s.interactive)
	if len(s.command.params) != 0 {
		s.runner.Params = append([]string(nil), s.command.params...)
	} else {
		s.runner.Params = nil
	}
	return s.runner.Run(ctx, prog)
}

func (s *Shell) runInteractive(ctx context.Context) error {
	promptFallback := defaultPrompt(s.version)
	promptSeq := 1
	currentPrompt := promptString(ctx, s.runner, s.opts, s.history, s.stdin, s.stderr, s.hostname, s.homeFallback, s.userFallback, "PS1", promptFallback, promptSeq, s.promptCache)
	promptSeq++

	// export HISTFILE=""
	s.history.limit = resolveShellHistoryLimit(s.runner)
	s.history.control = resolveShellHistoryControl(s.runner)
	s.history.onError = func(err error) {
		fmt.Fprintln(s.stderr, "history:", err)
	}
	histFile := resolveShellHistoryFile(s.runner)
	s.history.file = histFile
	s.history.appendOnAdd = func() bool {
		enabled, _ := shoptOptionEnabled(s.opts, false, "histappend")
		return enabled
	}

	conWriter := ptyNewConsoleANSIWriter(s.stderr)
	boundStdin := &keyBindingInput{src: s.stdin, mgr: s.bindings}
	promptPrinter := &promptPrinter{}
	completer := &autoCompleter{
		ctx:           ctx,
		runner:        s.runner,
		opts:          s.opts,
		completion:    s.completion,
		stdin:         s.stdin,
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
		HistoryLimit:           s.history.limit,
		DisableAutoSaveHistory: true,
		InterruptPrompt:        "^C",
		EOFPrompt:              "exit",
		Stdout:                 conWriter,
		Stderr:                 conWriter,
		AutoComplete:           completer,
		Listener:               historySearch.OnChange,
		Stdin:                  boundStdin,
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
	s.history.resync()
	completer.attach(rl)
	historySearch.Attach(rl)
	updateCheckwinsizeColumns(s.opts, s.runner, func() int {
		if width, _ := rl.GetConfig().FuncGetSize(); width > 0 {
			return width
		}
		return 0
	})
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
	resetPrompt := func() {
		if s.cfg.IsTerminal && s.cfg.OnPromptReset != nil {
			// Windows consoles may lose VT mode after programs exit.
			s.cfg.OnPromptReset(ctx)
		}
		updateCheckwinsizeColumns(s.opts, s.runner, func() int {
			if width, _ := rl.GetConfig().FuncGetSize(); width > 0 {
				return width
			}
			return 0
		})
		setPrompt(promptString(ctx, s.runner, s.opts, s.history, s.stdin, s.stderr, s.hostname, s.homeFallback, s.userFallback, "PS1", defaultPrompt(s.version), promptSeq, s.promptCache))
		promptSeq++
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

	// reader wraps readline so parser.Interactive can consume it as an
	// io.Reader. Each call to Read invokes Readline() to fetch one line.
	// Ctrl-C (ErrInterrupt) injects a newline to abandon the current
	// incomplete statement. Ctrl-D / EOF returns io.EOF to end the session.
	rdr := &reader{rl: rl, history: s.history}

	var exitErr error
	err = runInteractiveParser(s.parser, rdr, func(stmts []*syntax.Stmt) bool {
		// parser.Incomplete() returns true when the parser has consumed a
		// partial statement and is waiting for more input (e.g. open quotes,
		// unclosed if/for blocks). Switch to the continuation prompt and keep
		// reading without executing anything yet.
		if s.parser.Incomplete() {
			setPrompt(promptString(ctx, s.runner, s.opts, s.history, s.stdin, s.stderr, s.hostname, s.homeFallback, s.userFallback, "PS2", "> ", promptSeq, s.promptCache))
			flushPrefix()
			return true
		}

		rdr.savePendingHistory()
		cont, err := runInteractiveStatements(ctx, s.runner, stmts, rl.Stderr())
		if !cont {
			exitErr = err
			return false
		}

		// Restore the main prompt, updating it in case the effective UID
		// changed (e.g. via su).
		resetPrompt()
		return true
	}, func(err error) bool {
		rdr.savePendingHistory()
		fmt.Fprintln(rl.Stderr(), err.Error())
		resetPrompt()
		return true
	})
	if s.history.fileDirty() {
		if rewriteErr := s.history.RewriteFile(); rewriteErr != nil {
			fmt.Fprintln(rl.Stderr(), "failed to rewrite history:", rewriteErr)
		}
	}
	if exitErr != nil {
		return exitErr
	}
	return err
}
