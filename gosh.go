package gosh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/chzyer/readline"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Config describes one gosh shell invocation.
type Config struct {
	Version               string
	Context               context.Context
	Args                  []string
	Stdin                 io.Reader
	Stdout                io.Writer
	Stderr                io.Writer
	Env                   []string
	Dir                   string
	NotifySignals         bool
	IsTerminal            bool
	EnableVirtualTerminal func(stdin, stdout, stderr bool) error
}

func Run(c Config) error {
	version := c.Version
	if version == "" {
		version = "0.0.0"
	}

	args := c.Args
	if len(args) == 0 {
		args = []string{"gosh"}
	}
	stdin := c.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stdout := c.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	env := environWithDefaultShell(c.Env)

	command, err := parseCommand(args)
	if err != nil {
		return err
	}
	interactive := c.IsTerminal && command == nil
	stdinForRunner := stdin
	if command == nil && !interactive {
		stdinForRunner = strings.NewReader("")
	}

	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if c.NotifySignals {
		signals := []os.Signal{syscall.SIGTERM}
		if !interactive {
			signals = append(signals, os.Interrupt)
		}

		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(ctx, signals...)
		defer cancel()
	}

	if c.IsTerminal && c.EnableVirtualTerminal != nil {
		_ = c.EnableVirtualTerminal(true, false, false)
	}
	getScreenWidth := func() int {
		return readline.GetScreenWidth()
	}
	readlineWidth := func() int {
		if w := getScreenWidth(); w > 0 {
			return w
		}
		return 80
	}

	opts := []interp.RunnerOption{
		interp.Interactive(true),
		interp.StdIO(stdinForRunner, stdout, stderr),
		interp.Env(expand.ListEnviron(env...)),
	}
	if c.Dir != "" {
		opts = append(opts, interp.Dir(c.Dir))
	}

	parser := syntax.NewParser()
	history := &history{limit: resolveHistoryLimit()}
	bindings := &keyBindingManager{entries: make(map[string]*goKeyBindingEntry)}
	var runner *interp.Runner
	opts = append(opts, interp.CallHandler(callHandler(func() *interp.Runner { return runner }, history, bindings)))
	opts = append(opts, interp.ExecHandlers(execHandler(func() *interp.Runner { return runner })))
	runner, err = interp.New(opts...)
	if err != nil {
		return err
	}
	installShellOptionVariable(runner, interactive, command == nil, version)

	runner.Run(ctx, func() *syntax.File {
		prog, err := parser.Parse(strings.NewReader(`
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
			panic(err)
		}
		return prog
	}())

	// Source the interactive init file.
	if interactive {
		file, err := os.Open(resolveInitFile(env, interactive))
		if err == nil {
			prog, err := parser.Parse(file, file.Name())
			if err != nil {
				fmt.Fprintln(stderr, "failed to parse", file.Name(), ":", err)
			} else {
				if err := runner.Run(ctx, prog); err != nil {
					fmt.Fprintln(stderr, "failed to run", file.Name(), ":", err)
				}
			}
			file.Close()
		}
	}

	if command != nil {
		script := command.script
		if !strings.HasSuffix(script, "\n") {
			script += "\n"
		}
		prog, err := parser.Parse(strings.NewReader(script), command.argv0)
		if err != nil {
			return err
		}
		runner.Reset()
		if len(command.params) != 0 {
			runner.Params = append([]string(nil), command.params...)
		} else {
			runner.Params = nil
		}
		return runner.Run(ctx, prog)
	}

	// Non-interactive: parse stdin as a script and run it directly.
	if !interactive {
		runner.Reset()
		return runNonInteractiveStream(ctx, stdin, runner, stdout, stderr)
	}

	updateCheckwinsizeColumns(runner, getScreenWidth)
	promptFallback := defaultPrompt(version)
	promptSeq := 1
	currentPrompt := promptString(ctx, runner, stdin, stderr, "PS1", promptFallback, promptSeq)
	promptSeq++

	// export HISTFILE=""
	history.limit = resolveShellHistoryLimit(runner)
	history.control = resolveShellHistoryControl(runner)
	histFile := resolveShellHistoryFile(runner)
	history.file = histFile

	conWriter := ptyNewConsoleANSIWriter(stderr)
	boundStdin := &keyBindingInput{src: stdin, mgr: bindings}
	promptPrinter := &promptPrinter{}
	completer := &autoCompleter{ctx: ctx, runner: runner, stdin: stdin, stdout: conWriter, stderr: conWriter, promptPrinter: promptPrinter}
	historySearch := &historySearch{history: history, searchIndex: -1}
	bindings.registerActionHandler(keyActionHistorySearchBackward, historySearch.Search)
	bindings.registerActionHandler(keyActionHistorySearchForward, historySearch.Search)
	rl, err := readline.NewEx(&readline.Config{
		Prompt:                 currentPrompt.prompt,
		HistoryLimit:           history.limit,
		DisableAutoSaveHistory: true,
		InterruptPrompt:        "^C",
		EOFPrompt:              "exit",
		Stdin:                  readline.NewCancelableStdin(boundStdin),
		Stdout:                 conWriter,
		Stderr:                 conWriter,
		AutoComplete:           completer,
		Listener:               historySearch,
		FuncGetWidth: func() int {
			return readlineWidth()
		},
	})
	if err != nil {
		return err
	}
	_ = history.LoadFile(histFile)
	for _, entry := range history.Entries() {
		_ = rl.SaveHistory(entry)
	}
	completer.attach(rl)
	historySearch.Attach(rl)
	defer rl.Close()
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
		if c.IsTerminal && c.EnableVirtualTerminal != nil {
			// Windows consoles may lose VT mode after programs exit.
			_ = c.EnableVirtualTerminal(true, false, false)
		}
		updateCheckwinsizeColumns(runner, getScreenWidth)
		setPrompt(promptString(ctx, runner, stdin, stderr, "PS1", defaultPrompt(version), promptSeq))
		promptSeq++
		flushPrefix()
	}

	// reader wraps readline so parser.Interactive can consume it as an
	// io.Reader. Each call to Read invokes Readline() to fetch one line.
	// Ctrl-C (ErrInterrupt) injects a newline to abandon the current
	// incomplete statement. Ctrl-D / EOF returns io.EOF to end the session.
	rdr := &reader{rl: rl, history: history}

	return runInteractiveParser(parser, rdr, func(stmts []*syntax.Stmt) bool {
		// parser.Incomplete() returns true when the parser has consumed a
		// partial statement and is waiting for more input (e.g. open quotes,
		// unclosed if/for blocks). Switch to the continuation prompt and keep
		// reading without executing anything yet.
		if parser.Incomplete() {
			setPrompt(promptString(ctx, runner, stdin, stderr, "PS2", "> ", promptSeq))
			flushPrefix()
			return true
		}

		rdr.savePendingHistory()
		for _, stmt := range stmts {
			if err := runner.Run(ctx, stmt); err != nil {
				var status interp.ExitStatus
				if errors.As(err, &status) {
					continue
				}
				fmt.Fprintln(rl.Stderr(), err.Error())
			}
			if runner.Exited() {
				return false
			}
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
}
