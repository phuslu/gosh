package gosh

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// exitTrap holds a script's EXIT trap while the script runs.
//
// A script runs one complete command at a time, each as its own File so that
// $0 is the script name in the commands, in the files they source and in
// their traps. interp treats every File as a whole script and runs the EXIT
// trap after it, so while a script runs, gosh keeps the main shell's EXIT
// trap out of the interpreter and hands it back only when the script ends;
// see Shell.finishScript. Traps set in subshells still go to the subshell's
// interpreter.
type exitTrap struct {
	mu       sync.Mutex
	holding  bool // a script is running and EXIT traps are held here
	callback string
}

// hold starts or stops holding the EXIT trap, dropping any held callback.
func (t *exitTrap) hold(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.holding, t.callback = on, ""
}

// release stops holding the EXIT trap and returns the held callback.
func (t *exitTrap) release() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	callback := t.callback
	t.holding, t.callback = false, ""
	return callback
}

// rewriteTrap takes the EXIT trap out of a trap call made by the main shell
// while a script runs, following interp's own trap builtin: "trap" lists the
// traps, "trap SIG" resets SIG, and "trap ARG SIG..." sets ARG for each SIG,
// where "-" resets it.
func (d callDeps) rewriteTrap(ctx context.Context, argv []string) ([]string, bool) {
	t := d.exitTrap
	if t == nil || !d.isMainRunner(ctx) {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.holding {
		return nil, false
	}
	args := argv[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		// An option; interp reports it.
		return nil, false
	}
	switch len(args) {
	case 0:
		if t.callback != "" {
			fmt.Fprintf(interp.HandlerCtx(ctx).Stdout, "trap -- %q EXIT\n", t.callback)
		}
		return nil, false
	case 1:
		if args[0] != "EXIT" {
			return nil, false
		}
		t.callback = ""
		return []string{":"}, true
	}
	callback, signals := args[0], args[1:]
	rest := slices.DeleteFunc(slices.Clone(signals), func(sig string) bool { return sig == "EXIT" })
	if len(rest) == len(signals) {
		return nil, false
	}
	if callback == "-" {
		callback = ""
	}
	t.callback = callback
	if len(rest) == 0 {
		return []string{":"}, true
	}
	return append([]string{argv[0], "--", args[0]}, rest...), true
}

// isMainRunner reports whether the call in ctx comes from the shell's own
// interpreter rather than from a subshell's copy of it. interp does not
// expose the runner behind a HandlerContext, so it is read by reflection;
// without it every call counts as the main shell's.
func (d callDeps) isMainRunner(ctx context.Context) bool {
	main := d.runner()
	if main == nil {
		return false
	}
	field := reflect.ValueOf(interp.HandlerCtx(ctx)).FieldByName("runner")
	if !field.IsValid() || field.Kind() != reflect.Pointer {
		return true
	}
	return field.Pointer() == reflect.ValueOf(main).Pointer()
}

// setTrapStmt returns a statement which sets callback as the EXIT trap.
// Closing stderr keeps xtrace from showing it.
func setTrapStmt(callback string) *syntax.Stmt {
	quoted, err := syntax.Quote(callback, syntax.LangBash)
	if err != nil {
		// Only strings with NUL bytes cannot be quoted, and the shell
		// cannot hold those either.
		quoted = "''"
	}
	return parseStmt("trap -- " + quoted + " EXIT 2>&-")
}

// statusStmt returns a statement which sets $? to status. Like a command in
// an && list, it fails without running the ERR trap or triggering errexit,
// and closing stderr keeps xtrace from showing it.
func statusStmt(status int) *syntax.Stmt {
	return parseStmt(fmt.Sprintf("(exit %d) 2>&- && : 2>&-", status))
}

func parseStmt(src string) *syntax.Stmt {
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		panic(err)
	}
	return prog.Stmts[0]
}
