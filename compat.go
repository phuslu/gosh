package gosh

import (
	"context"
	"fmt"
	"os/exec"
	"slices"

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

// callInterceptor is one compatibility middleware. A handler returns
// handled=false to pass the call through unchanged.
type callInterceptor struct {
	name    string
	rewrite func(ctx context.Context, args []string) ([]string, bool)
}

// callHandler builds the compatibility middleware chain which sits in front
// of the mvdan.cc/sh interpreter. New Bash compatibility concerns become
// additional interceptors instead of new branches in one giant switch.
func callHandler(deps callDeps) interp.CallHandlerFunc {
	interceptors := []callInterceptor{
		{name: "printf", rewrite: deps.rewritePrintf},
		{name: "shopt", rewrite: deps.rewriteShopt},
		{name: "set", rewrite: deps.rewriteSet},
		{name: "builtin", rewrite: deps.rewriteBuiltin},
		{name: "command", rewrite: deps.rewriteCommand},
		{name: "wget", rewrite: deps.rewriteWget},
		{name: "history", rewrite: deps.rewriteHistory},
		{name: "fc", rewrite: deps.rewriteFc},
		{name: "bind", rewrite: deps.rewriteBind},
		{name: "complete", rewrite: deps.rewriteComplete},
		{name: "compgen", rewrite: deps.rewriteCompgen},
		{name: "compopt", rewrite: deps.rewriteCompopt},
		{name: "kill", rewrite: deps.rewriteKillNewgrp},
		{name: "newgrp", rewrite: deps.rewriteKillNewgrp},
	}
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 0 {
			return args, nil
		}
		for _, interceptor := range interceptors {
			if interceptor.name != args[0] {
				continue
			}
			next, handled := interceptor.rewrite(ctx, args)
			if handled {
				return next, nil
			}
			break
		}
		return args, nil
	}
}

func (d callDeps) rewriteComplete(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if d.completion == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := builtinComplete(d, args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) rewriteCompgen(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if d.completion == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := builtinCompgen(d, ctx, args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) rewriteCompopt(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if d.completion == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := builtinCompopt(d, args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) functionDefined(name string) bool {
	r := d.runner()
	return r != nil && r.Funcs[name] != nil
}

func (d callDeps) rewritePrintf(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	return dropBuiltinPrintfDashDash(args)
}

func (d callDeps) rewriteShopt(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if shouldHandleShopt(args[1:]) {
		next := make([]string, 1, len(args))
		next[0] = shoptCommand
		next = append(next, args[1:]...)
		return next, true
	}
	return args, false
}

func (d callDeps) rewriteSet(ctx context.Context, args []string) ([]string, bool) {
	if d.opts != nil {
		d.opts.recordSet(args[1:])
	}
	return handleSetVerboseOption(args)
}

func (d callDeps) rewriteBuiltin(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if len(args) >= 2 && args[1] == "shopt" && shouldHandleShopt(args[2:]) {
		next := make([]string, 1, len(args)-1)
		next[0] = shoptCommand
		next = append(next, args[2:]...)
		return next, true
	}
	if len(args) >= 2 && args[1] == "set" {
		if d.opts != nil {
			d.opts.recordSet(args[2:])
		}
		if next, ok := handleSetVerboseOption(args[1:]); ok {
			if len(next) == 1 && next[0] == ":" {
				return next, true
			}
			return append([]string{args[0]}, next...), true
		}
	}
	if len(args) >= 2 && args[1] == "printf" {
		if next, ok := dropBuiltinPrintfDashDash(args[1:]); ok {
			return append([]string{args[0]}, next...), true
		}
	}
	return args, false
}

func (d callDeps) rewriteCommand(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	if shoptArgs, ok := commandShoptArgs(args[1:]); ok {
		next := make([]string, 1, len(shoptArgs)+1)
		next[0] = shoptCommand
		next = append(next, shoptArgs...)
		return next, true
	}
	if len(args) >= 2 && args[1] == "set" {
		if d.opts != nil {
			d.opts.recordSet(args[2:])
		}
		if next, ok := handleSetVerboseOption(args[1:]); ok {
			if len(next) == 1 && next[0] == ":" {
				return next, true
			}
			return append([]string{args[0]}, next...), true
		}
	}
	return dropCommandPrintfDashDash(args)
}

func (d callDeps) rewriteWget(ctx context.Context, args []string) ([]string, bool) {
	if _, err := exec.LookPath(args[0]); err == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	file, err := builtinWget(ctx, args[1:], hc.Stdout)
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	fmt.Fprintf(hc.Stdout, "Saved %s\n", file)
	return []string{":"}, true
}

func (d callDeps) rewriteHistory(ctx context.Context, args []string) ([]string, bool) {
	if d.history == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := builtinHistory(d.history, args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) rewriteFc(ctx context.Context, args []string) ([]string, bool) {
	if d.history == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := builtinFc(d.history, args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) rewriteBind(ctx context.Context, args []string) ([]string, bool) {
	if d.bindings == nil {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	if err := d.bindings.handleBind(args[1:], hc.Stdout); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return []string{"false"}, true
	}
	return []string{":"}, true
}

func (d callDeps) rewriteKillNewgrp(ctx context.Context, args []string) ([]string, bool) {
	if d.functionDefined(args[0]) {
		return args, false
	}
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		return args, false
	}
	next := slices.Clone(args)
	next[0] = path
	return next, true
}
