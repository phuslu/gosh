package gosh

import (
	"context"
	"fmt"

	"mvdan.cc/sh/v3/interp"
)

const goshShoptQuietCommand = "__gosh_shopt_quiet"

// These tables mirror mvdan.cc/sh's internal shopt option order, because the
// corresponding option states are only exposed as an unexported bool slice.
var goshShoptPosixOptionNames = []string{
	"allexport",
	"errexit",
	"noexec",
	"noglob",
	"nounset",
	"xtrace",
	"pipefail",
}

var goshShoptBashOptionNames = []string{
	"dotglob",
	"expand_aliases",
	"extglob",
	"globstar",
	"nocaseglob",
	"nullglob",
	"assoc_expand_once",
	"autocd",
	"cdable_vars",
	"cdspell",
	"checkhash",
	"checkjobs",
	"checkwinsize",
	"cmdhist",
	"compat31",
	"compat32",
	"compat40",
	"compat41",
	"compat42",
	"compat44",
	"compat43",
	"compat44",
	"complete_fullquote",
	"direxpand",
	"dirspell",
	"execfail",
	"extdebug",
	"extquote",
	"failglob",
	"force_fignore",
	"globasciiranges",
	"gnu_errfmt",
	"histappend",
	"histreedit",
	"histverify",
	"hostcomplete",
	"huponexit",
	"inherit_errexit",
	"interactive_comments",
	"lastpipe",
	"lithist",
	"localvar_inherit",
	"localvar_unset",
	"login_shell",
	"mailwarn",
	"no_empty_cmd_completion",
	"nocasematch",
	"progcomp",
	"progcomp_alias",
	"promptvars",
	"restricted_shell",
	"shift_verbose",
	"sourcepath",
	"xpg_echo",
}

type goshShoptArgs struct {
	quiet       bool
	posix       bool
	mode        string
	invalid     string
	unsupported string
	args        []string
	forward     []string
}

type goshShoptFlagParser struct {
	current           string
	remaining         []string
	stoppedByDashDash bool
}

func (p *goshShoptFlagParser) more() bool {
	if p.current != "" {
		return true
	}
	if len(p.remaining) == 0 {
		p.remaining = nil
		return false
	}
	arg := p.remaining[0]
	if arg == "--" {
		p.remaining = p.remaining[1:]
		p.stoppedByDashDash = true
		return false
	}
	if len(arg) == 0 || (arg[0] != '-' && arg[0] != '+') {
		return false
	}
	return true
}

func (p *goshShoptFlagParser) flag() string {
	arg := p.current
	if arg == "" {
		arg = p.remaining[0]
		p.remaining = p.remaining[1:]
	} else {
		p.current = ""
	}
	if len(arg) > 2 {
		p.current = arg[:1] + arg[2:]
		arg = arg[:2]
	}
	return arg
}

func (p *goshShoptFlagParser) args() []string {
	return p.remaining
}

func goshExecHandler(runner func() *interp.Runner) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) > 0 && args[0] == goshShoptQuietCommand {
				var r *interp.Runner
				if runner != nil {
					r = runner()
				}
				return goshRunShoptQuiet(ctx, r, args[1:])
			}
			return next(ctx, args)
		}
	}
}

func goshShoptArgsHaveQuiet(args []string) bool {
	return goshParseShoptArgs(args).quiet
}

func goshCommandShoptQuietArgs(args []string) ([]string, bool) {
	fp := goshShoptFlagParser{remaining: args}
	show := false
	for fp.more() {
		switch flag := fp.flag(); flag {
		case "-v":
			show = true
		default:
			return nil, false
		}
	}
	if show {
		return nil, false
	}
	rest := fp.args()
	if len(rest) == 0 || rest[0] != "shopt" || !goshShoptArgsHaveQuiet(rest[1:]) {
		return nil, false
	}
	return rest[1:], true
}

func goshParseShoptArgs(args []string) goshShoptArgs {
	fp := goshShoptFlagParser{remaining: args}
	var parsed goshShoptArgs
	for fp.more() {
		switch flag := fp.flag(); flag {
		case "-q":
			parsed.quiet = true
		case "-s", "-u":
			parsed.mode = flag
			parsed.forward = append(parsed.forward, flag)
		case "-o":
			parsed.posix = true
			parsed.forward = append(parsed.forward, flag)
		case "-p":
			parsed.unsupported = flag
			return parsed
		default:
			parsed.invalid = flag
			return parsed
		}
	}
	if fp.stoppedByDashDash {
		parsed.forward = append(parsed.forward, "--")
	}
	parsed.args = fp.args()
	parsed.forward = append(parsed.forward, parsed.args...)
	return parsed
}

func goshRunShoptQuiet(ctx context.Context, runner *interp.Runner, args []string) error {
	hc := interp.HandlerCtx(ctx)
	parsed := goshParseShoptArgs(args)
	if parsed.unsupported != "" {
		fmt.Fprintf(hc.Stderr, "shopt: unsupported option %q\n", parsed.unsupported)
		return interp.ExitStatus(2)
	}
	if parsed.invalid != "" {
		fmt.Fprintf(hc.Stderr, "shopt: invalid option %q\n", parsed.invalid)
		return interp.ExitStatus(2)
	}
	if !parsed.quiet {
		next := append([]string{"shopt"}, args...)
		return hc.Builtin(ctx, next)
	}
	if parsed.mode != "" {
		if len(parsed.args) == 0 {
			return nil
		}
		next := append([]string{"shopt"}, parsed.forward...)
		return hc.Builtin(ctx, next)
	}
	for _, arg := range parsed.args {
		enabled, ok := goshShoptOptionEnabled(runner, parsed.posix, arg)
		if !ok {
			fmt.Fprintf(hc.Stderr, "shopt: invalid option name %q\n", arg)
			return interp.ExitStatus(1)
		}
		if !enabled {
			return interp.ExitStatus(1)
		}
	}
	return nil
}

func goshShoptOptionEnabled(runner *interp.Runner, posix bool, name string) (bool, bool) {
	opts := goshRunnerOpts(runner)
	if posix {
		for i, opt := range goshShoptPosixOptionNames {
			if opt == name {
				if i >= len(opts) {
					return false, false
				}
				return opts[i], true
			}
		}
		return false, false
	}
	for i, opt := range goshShoptBashOptionNames {
		if opt == name {
			index := len(goshShoptPosixOptionNames) + i
			if index >= len(opts) {
				return false, false
			}
			return opts[index], true
		}
	}
	return false, false
}
