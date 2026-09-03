package gosh

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// callDeps carries the shell-owned state that compatibility interceptors
// need. Keeping it behind one value makes the middleware chain independent
// of gosh.Run's wiring and easy to extend for future backends.
type callDeps struct {
	runner     func() *interp.Runner
	history    *history
	bindings   *keyBindingManager
	completion *completionRegistry
	opts       *shellOptions
}

// Sentinel command names. The call middleware rewrites an intercepted builtin
// into one of these, and the exec middleware below runs the matching gosh
// implementation. Going through the exec handler (rather than doing the work
// inside the call handler) is what lets a gosh builtin report an arbitrary
// interp.ExitStatus instead of only success or failure. The "__gosh_" prefix
// keeps the names out of reach of real commands; they never appear in the
// completion command index either, which is built from defaultCommandNames,
// the runner's functions and $PATH.
const (
	shoptCommand    = "__gosh_shopt"
	historyCommand  = "__gosh_history"
	fcCommand       = "__gosh_fc"
	bindCommand     = "__gosh_bind"
	completeCommand = "__gosh_complete"
	compgenCommand  = "__gosh_compgen"
	compoptCommand  = "__gosh_compopt"
)

// compatBuiltin is one Bash builtin that gosh rewrites before the
// mvdan.cc/sh interpreter sees it. rewrite receives the call with any
// "builtin"/"command" prefix already stripped and returns handled=false to
// let the call through unchanged.
type compatBuiltin struct {
	// shadowable reports whether a user-defined function of the same name
	// wins over the rewrite. Only bare calls consult it, since Bash's
	// "builtin foo" forces the builtin and "command foo" bypasses functions.
	shadowable bool
	rewrite    func(d callDeps, ctx context.Context, argv []string) ([]string, bool)
}

// compatBuiltins is the single interception table. Every entry is reached
// through the same prefix normalization, so "shopt", "builtin shopt" and
// "command -p shopt" all share one implementation.
var compatBuiltins = map[string]compatBuiltin{
	"printf":   {shadowable: true, rewrite: callDeps.rewritePrintf},
	"shopt":    {shadowable: true, rewrite: callDeps.rewriteShopt},
	"complete": {shadowable: true, rewrite: callDeps.rewriteComplete},
	"compgen":  {shadowable: true, rewrite: callDeps.rewriteCompgen},
	"compopt":  {shadowable: true, rewrite: callDeps.rewriteCompopt},
	"set":      {rewrite: callDeps.rewriteSet},
	"history":  {rewrite: callDeps.rewriteHistory},
	"fc":       {rewrite: callDeps.rewriteFc},
	"bind":     {rewrite: callDeps.rewriteBind},
}

// compatCommands rewrites external commands, which have no "builtin"/
// "command" prefix handling because they are not builtins in the first place.
var compatCommands = map[string]func(d callDeps, ctx context.Context, argv []string) ([]string, bool){
	"wget":   callDeps.rewriteWget,
	"kill":   callDeps.rewriteKillNewgrp,
	"newgrp": callDeps.rewriteKillNewgrp,
}

// callHandler builds the compatibility middleware which sits in front of the
// mvdan.cc/sh interpreter. New Bash compatibility concerns become additional
// table entries instead of new branches in one giant switch.
func callHandler(deps callDeps) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 0 {
			return args, nil
		}
		prefix, argv := deps.splitCommandPrefix(args)
		if len(prefix) == 0 {
			if rewrite, ok := compatCommands[argv[0]]; ok {
				if next, handled := rewrite(deps, ctx, argv); handled {
					return next, nil
				}
				return args, nil
			}
		}
		builtin, ok := compatBuiltins[argv[0]]
		if !ok {
			return args, nil
		}
		if len(prefix) == 0 && builtin.shadowable && deps.functionDefined(argv[0]) {
			return args, nil
		}
		next, handled := builtin.rewrite(deps, ctx, argv)
		if !handled {
			return args, nil
		}
		// Put the prefix back only when the rewrite still names the same
		// builtin; ":" and the sentinels replace the whole call instead.
		if len(prefix) > 0 && next[0] == argv[0] {
			next = append(slices.Clone(prefix), next...)
		}
		return next, nil
	}
}

// splitCommandPrefix peels off the leading "builtin" and "command" words so
// the interception table only has to know each builtin's own name.
func (d callDeps) splitCommandPrefix(args []string) (prefix, argv []string) {
	i := 0
	for i < len(args)-1 {
		n := d.commandPrefixLen(args[i:])
		if n == 0 {
			break
		}
		i += n
	}
	return args[:i], args[i:]
}

// commandPrefixLen reports how many leading words of args, which holds at
// least two elements, form a strippable prefix.
func (d callDeps) commandPrefixLen(args []string) int {
	switch args[0] {
	case "builtin":
		if d.functionDefined("builtin") {
			return 0
		}
		return 1
	case "command":
		if d.functionDefined("command") {
			return 0
		}
		// "command -p" only swaps in a default $PATH for the lookup, so the
		// call still runs. "command -v" and "-V" describe the command rather
		// than running it, so they stay with the interpreter's own builtin,
		// as does any other flag we do not model.
		if args[1] == "-p" {
			if len(args) < 3 {
				return 0
			}
			return 2
		}
		if strings.HasPrefix(args[1], "-") {
			return 0
		}
		return 1
	}
	return 0
}

func (d callDeps) functionDefined(name string) bool {
	r := d.runner()
	return r != nil && r.Funcs[name] != nil
}

func (d callDeps) rewritePrintf(ctx context.Context, argv []string) ([]string, bool) {
	return dropPrintfDashDash(argv)
}

func (d callDeps) rewriteSet(ctx context.Context, argv []string) ([]string, bool) {
	if d.opts != nil {
		d.opts.recordSet(argv[1:])
	}
	return handleSetVerboseOption(argv)
}

func (d callDeps) rewriteShopt(ctx context.Context, argv []string) ([]string, bool) {
	if !shouldHandleShopt(argv[1:]) {
		return nil, false
	}
	return sentinelArgs(shoptCommand, argv[1:]), true
}

func (d callDeps) rewriteHistory(ctx context.Context, argv []string) ([]string, bool) {
	if d.history == nil {
		return nil, false
	}
	return sentinelArgs(historyCommand, argv[1:]), true
}

func (d callDeps) rewriteFc(ctx context.Context, argv []string) ([]string, bool) {
	if d.history == nil {
		return nil, false
	}
	return sentinelArgs(fcCommand, argv[1:]), true
}

func (d callDeps) rewriteBind(ctx context.Context, argv []string) ([]string, bool) {
	if d.bindings == nil {
		return nil, false
	}
	return sentinelArgs(bindCommand, argv[1:]), true
}

func (d callDeps) rewriteComplete(ctx context.Context, argv []string) ([]string, bool) {
	if d.completion == nil {
		return nil, false
	}
	return sentinelArgs(completeCommand, argv[1:]), true
}

func (d callDeps) rewriteCompgen(ctx context.Context, argv []string) ([]string, bool) {
	if d.completion == nil {
		return nil, false
	}
	return sentinelArgs(compgenCommand, argv[1:]), true
}

func (d callDeps) rewriteCompopt(ctx context.Context, argv []string) ([]string, bool) {
	if d.completion == nil {
		return nil, false
	}
	return sentinelArgs(compoptCommand, argv[1:]), true
}

// rewriteWget stays a call-handler side effect rather than a sentinel: the
// gosh implementation only exists as a fallback for hosts without a real
// wget, so the LookPath probe that decides whether to intercept at all has to
// run here anyway, and there is no external builtin whose exit statuses we
// need to reproduce.
func (d callDeps) rewriteWget(ctx context.Context, argv []string) ([]string, bool) {
	if _, err := exec.LookPath(argv[0]); err == nil {
		return nil, false
	}
	hc := interp.HandlerCtx(ctx)
	file, err := builtinWget(ctx, argv[1:], hc.Stdout)
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	fmt.Fprintf(hc.Stdout, "Saved %s\n", file)
	return []string{":"}, true
}

func (d callDeps) rewriteKillNewgrp(ctx context.Context, argv []string) ([]string, bool) {
	if d.functionDefined(argv[0]) {
		return nil, false
	}
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, argv[0])
	if err != nil {
		return nil, false
	}
	next := slices.Clone(argv)
	next[0] = path
	return next, true
}

func sentinelArgs(name string, args []string) []string {
	next := make([]string, 1, len(args)+1)
	next[0] = name
	return append(next, args...)
}

// sentinelBuiltins runs the gosh builtins that the call middleware rewrote
// into sentinel commands. Returning an error is how a builtin reports a
// non-zero exit status; interp.ExitStatus values pass through unchanged.
var sentinelBuiltins = map[string]func(ctx context.Context, d callDeps, args []string) error{
	shoptCommand: func(ctx context.Context, d callDeps, args []string) error {
		var r *interp.Runner
		if d.runner != nil {
			r = d.runner()
		}
		return runShopt(ctx, r, d.opts, args)
	},
	historyCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, builtinHistory(d.history, args, interp.HandlerCtx(ctx).Stdout))
	},
	fcCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, builtinFc(d.history, args, interp.HandlerCtx(ctx).Stdout))
	},
	bindCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, d.bindings.handleBind(args, interp.HandlerCtx(ctx).Stdout))
	},
	completeCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, builtinComplete(d, args, interp.HandlerCtx(ctx).Stdout))
	},
	compgenCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, builtinCompgen(d, ctx, args, interp.HandlerCtx(ctx).Stdout))
	},
	compoptCommand: func(ctx context.Context, d callDeps, args []string) error {
		return builtinStatus(ctx, builtinCompopt(d, args, interp.HandlerCtx(ctx).Stdout))
	},
}

// builtinStatus turns a builtin's Go error into the exit status Bash would
// report. Usage errors still exit 1 rather than Bash's 2; aligning those is
// separate work.
func builtinStatus(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, err)
	return interp.ExitStatus(1)
}

func backendExec(backend Backend) interp.ExecHandlerFunc {
	if backend == nil {
		return nil
	}
	return backend.Exec
}

// execHandler dispatches the sentinel commands produced by callHandler and
// otherwise defers to the configured backend or the interpreter default.
func execHandler(deps callDeps, execBackend interp.ExecHandlerFunc) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) > 0 {
				if run, ok := sentinelBuiltins[args[0]]; ok {
					return run(ctx, deps, args[1:])
				}
			}
			if execBackend != nil {
				return execBackend(ctx, args)
			}
			return next(ctx, args)
		}
	}
}
