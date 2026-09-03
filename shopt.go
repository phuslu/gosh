package gosh

import (
	"context"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

type shoptArgs struct {
	quiet       bool
	posix       bool
	mode        string
	invalid     string
	unsupported string
	args        []string
	forward     []string
}

type shoptFlagParser struct {
	current           string
	remaining         []string
	stoppedByDashDash bool
}

func (p *shoptFlagParser) more() bool {
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

func (p *shoptFlagParser) flag() string {
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

func (p *shoptFlagParser) args() []string {
	return p.remaining
}

func shouldHandleShopt(args []string) bool {
	parsed := parseShoptArgs(args)
	if parsed.quiet {
		return true
	}
	if parsed.unsupported != "" || parsed.invalid != "" {
		return false
	}
	if len(parsed.args) == 0 {
		return true
	}
	// Route option mutations (including upstream-supported ones) through
	// gosh so its mirrored option state stays in sync with the interpreter.
	if parsed.mode != "" {
		return true
	}
	return shoptArgsHaveManagedOption(parsed.args)
}

func parseShoptArgs(args []string) shoptArgs {
	fp := shoptFlagParser{remaining: args}
	var parsed shoptArgs
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

func runShopt(ctx context.Context, runner *interp.Runner, opts *shellOptions, args []string) error {
	hc := interp.HandlerCtx(ctx)
	parsed := parseShoptArgs(args)
	if parsed.unsupported != "" {
		fmt.Fprintf(hc.Stderr, "shopt: unsupported option %q\n", parsed.unsupported)
		return interp.ExitStatus(2)
	}
	if parsed.invalid != "" {
		fmt.Fprintf(hc.Stderr, "shopt: invalid option %q\n", parsed.invalid)
		return interp.ExitStatus(2)
	}
	if parsed.mode != "" && (parsed.posix || !shoptArgsHaveManagedOption(parsed.args)) {
		next := append([]string{"shopt"}, parsed.forward...)
		err := hc.Builtin(ctx, next)
		if err == nil && opts != nil {
			for _, arg := range parsed.args {
				opts.set(parsed.posix, arg, parsed.mode == "-s")
			}
		}
		return err
	}
	if parsed.mode != "" {
		return setShoptOptions(ctx, runner, opts, parsed)
	}
	if len(parsed.args) == 0 {
		if parsed.quiet {
			return nil
		}
		printShoptOptions(hc.Stdout, opts, parsed.posix)
		return nil
	}
	for _, arg := range parsed.args {
		enabled, ok := opts.enabled(parsed.posix, arg)
		if !ok {
			fmt.Fprintf(hc.Stderr, "shopt: invalid option name %q\n", arg)
			return interp.ExitStatus(1)
		}
		if parsed.quiet {
			if !enabled {
				return interp.ExitStatus(1)
			}
			continue
		}
		printShoptOption(hc.Stdout, opts, parsed.posix, arg)
	}
	return nil
}

func setShoptOptions(ctx context.Context, runner *interp.Runner, opts *shellOptions, parsed shoptArgs) error {
	// runShopt only calls this for non-POSIX lists containing at least one
	// gosh-managed option. Supported Bash options use interp.BashOpts, and
	// gosh-managed options are stored in the mirrored option state above.
	hc := interp.HandlerCtx(ctx)
	for _, arg := range parsed.args {
		if shoptManagedOption(arg) {
			// Upstream declares these options unsupported, so BashOpts cannot
			// set them. gosh manages them in its own option state.
			opt, supported, managed := shoptOption(opts, false, arg)
			if opt == nil || !supported || !managed {
				fmt.Fprintf(hc.Stderr, "shopt: unsupported option %q\n", arg)
				return interp.ExitStatus(1)
			}
			*opt = parsed.mode == "-s"
			continue
		}

		// Use the public mutation API for the supported Bash options.
		if err := interp.BashOpts(parsed.mode, arg)(runner); err != nil {
			switch {
			case strings.Contains(err.Error(), "invalid option name"):
				fmt.Fprintf(hc.Stderr, "shopt: invalid option name %q\n", arg)
			case strings.Contains(err.Error(), "unsupported option"):
				fmt.Fprintf(hc.Stderr, "shopt: unsupported option %q\n", arg)
			default:
				fmt.Fprintf(hc.Stderr, "shopt: %v\n", err)
			}
			return interp.ExitStatus(1)
		}
		opts.set(false, arg, parsed.mode == "-s")
	}
	return nil
}

// shoptEnabled reports whether the named Bash option is on, treating an
// unknown option and a nil option set as "off".
func shoptEnabled(opts *shellOptions, name string) bool {
	enabled, ok := opts.enabled(false, name)
	return ok && enabled
}

func shoptOption(opts *shellOptions, posix bool, name string) (*bool, bool, bool) {
	if opts == nil {
		return nil, false, false
	}
	return opts.option(posix, name)
}

func shoptArgsHaveManagedOption(args []string) bool {
	for _, arg := range args {
		if shoptManagedOption(arg) {
			return true
		}
	}
	return false
}

// shoptManagedOption reports whether gosh implements the named option itself
// rather than delegating it to interp.BashOpts.
func shoptManagedOption(name string) bool {
	return shellOptionIndex[name].managed
}

func printShoptOptions(w interface {
	Write([]byte) (int, error)
}, opts *shellOptions, posix bool) {
	names := shoptBashOptionNames
	if posix {
		names = shoptPosixOptionNames
	}
	for _, name := range names {
		printShoptOption(w, opts, posix, name)
	}
}

func printShoptOption(w interface {
	Write([]byte) (int, error)
}, opts *shellOptions, posix bool, name string) {
	opt, supported, _ := shoptOption(opts, posix, name)
	if opt == nil {
		return
	}
	state := "off"
	if *opt {
		state = "on"
	}
	if supported {
		fmt.Fprintf(w, "%s\t%s\n", name, state)
		return
	}
	unsupported := "on"
	if *opt {
		unsupported = "off"
	}
	fmt.Fprintf(w, "%s\t%s\t(%q not supported)\n", name, state, unsupported)
}
