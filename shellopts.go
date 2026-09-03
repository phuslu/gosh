package gosh

import (
	"strconv"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// installShellOptionVariable wraps the interpreter environment with the
// gosh-specific BASH_VERSION value and a small overlay that lets gosh-owned
// runtime state (currently COLUMNS) be written without reaching into
// unexported interp.Runner fields.
func installShellOptionVariable(runner *interp.Runner, version string) {
	if runner == nil {
		return
	}
	base := runner.Env
	if base == nil {
		base = expand.ListEnviron()
	}
	runner.Env = &shellEnviron{base: base, version: version, overlay: make(map[string]expand.Variable)}
}

type shellEnviron struct {
	mu      sync.RWMutex
	base    expand.Environ
	overlay map[string]expand.Variable
	version string
}

func (e *shellEnviron) Get(name string) expand.Variable {
	if name == "BASH_VERSION" {
		return expand.Variable{Set: true, Kind: expand.String, Str: e.version + "(1)-gosh"}
	}
	e.mu.RLock()
	if vr, ok := e.overlay[name]; ok {
		e.mu.RUnlock()
		return vr
	}
	e.mu.RUnlock()
	if e.base == nil {
		return expand.Variable{}
	}
	return e.base.Get(name)
}

func (e *shellEnviron) Each(f func(name string, vr expand.Variable) bool) {
	if e.base != nil {
		e.base.Each(f)
	}
	e.mu.RLock()
	for name, vr := range e.overlay {
		if !f(name, vr) {
			break
		}
	}
	e.mu.RUnlock()
}

func (e *shellEnviron) Set(name string, vr expand.Variable) error {
	if name == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !vr.IsSet() {
		delete(e.overlay, name)
		return nil
	}
	e.overlay[name] = vr
	return nil
}

// shellOptionDesc describes one shell option: which namespace it lives in,
// how it starts out, and who implements it. It is the single source of truth
// for the option metadata that shopt, "set -o" and the prompt/completion code
// used to rediscover through separate switch statements.
type shellOptionDesc struct {
	name string
	// posix marks a "set -o" option instead of a shopt option.
	posix bool
	// defaultOn marks an option that starts enabled.
	defaultOn bool
	// interactiveOn marks an option that also starts enabled in an
	// interactive shell.
	interactiveOn bool
	// managed marks an option upstream declares unsupported but gosh
	// implements itself.
	managed bool
	// upstream marks an option mvdan.cc/sh implements via interp.BashOpts.
	upstream bool
}

// shellOptionTable lists every option gosh mirrors, in the order shopt prints
// it. The order matches mvdan.cc/sh's internal shopt option order, because the
// corresponding option states are only exposed as an unexported bool slice.
// managed and upstream are mutually exclusive by construction.
var shellOptionTable = []shellOptionDesc{
	{name: "allexport", posix: true},
	{name: "errexit", posix: true},
	{name: "noexec", posix: true},
	{name: "noglob", posix: true},
	{name: "nounset", posix: true},
	{name: "xtrace", posix: true},
	{name: "pipefail", posix: true},

	{name: "dotglob", upstream: true},
	{name: "expand_aliases", upstream: true, interactiveOn: true},
	{name: "extglob", upstream: true},
	{name: "globstar", upstream: true},
	{name: "nocaseglob", upstream: true},
	{name: "nullglob", upstream: true},
	{name: "assoc_expand_once"},
	{name: "autocd"},
	{name: "cdable_vars"},
	{name: "cdspell"},
	{name: "checkhash"},
	{name: "checkjobs"},
	{name: "checkwinsize", defaultOn: true, managed: true},
	{name: "cmdhist", defaultOn: true, managed: true},
	{name: "compat31"},
	{name: "compat32"},
	{name: "compat40"},
	{name: "compat41"},
	{name: "compat42"},
	{name: "compat43"},
	{name: "compat44"},
	{name: "complete_fullquote", defaultOn: true},
	{name: "direxpand"},
	{name: "dirspell"},
	{name: "execfail"},
	{name: "extdebug"},
	{name: "extquote", defaultOn: true},
	{name: "failglob", managed: true},
	{name: "force_fignore", defaultOn: true},
	{name: "globasciiranges"},
	{name: "gnu_errfmt"},
	{name: "histappend", managed: true},
	{name: "histreedit"},
	{name: "histverify"},
	{name: "hostcomplete", defaultOn: true, managed: true},
	{name: "huponexit"},
	{name: "inherit_errexit", defaultOn: true},
	{name: "interactive_comments", defaultOn: true},
	{name: "lastpipe"},
	{name: "lithist", managed: true},
	{name: "localvar_inherit"},
	{name: "localvar_unset"},
	{name: "login_shell"},
	{name: "mailwarn"},
	{name: "no_empty_cmd_completion"},
	{name: "nocasematch"},
	{name: "progcomp", defaultOn: true, managed: true},
	{name: "progcomp_alias"},
	{name: "promptvars", defaultOn: true, managed: true},
	{name: "restricted_shell"},
	{name: "shift_verbose"},
	{name: "sourcepath", defaultOn: true},
	{name: "xpg_echo"},
}

// shellOptionIndex indexes shellOptionTable by name. Option names are unique
// across both namespaces, so one map covers them all.
var shellOptionIndex = func() map[string]shellOptionDesc {
	index := make(map[string]shellOptionDesc, len(shellOptionTable))
	for _, desc := range shellOptionTable {
		index[desc.name] = desc
	}
	return index
}()

// shellOptionNames returns the table's option names for one namespace, in
// listing order.
func shellOptionNames(posix bool) []string {
	names := make([]string, 0, len(shellOptionTable))
	for _, desc := range shellOptionTable {
		if desc.posix == posix {
			names = append(names, desc.name)
		}
	}
	return names
}

var (
	shoptPosixOptionNames = shellOptionNames(true)
	shoptBashOptionNames  = shellOptionNames(false)
)

// shellOptions mirrors the shell option state needed by gosh's shopt and
// prompt/completion code. Mutation goes through the public mvdan.cc/sh API
// (interp.BashOpts and the "set"/"shopt" builtins); this map is gosh's
// getter, removing the previous reflection/unsafe dependency on the
// interpreter's private option slice.
type shellOptions struct {
	mu    sync.RWMutex
	posix map[string]*bool
	bash  map[string]*bool
}

func newShellOptions(interactive bool) *shellOptions {
	opts := &shellOptions{
		posix: make(map[string]*bool, len(shoptPosixOptionNames)),
		bash:  make(map[string]*bool, len(shoptBashOptionNames)),
	}
	for _, desc := range shellOptionTable {
		value := desc.defaultOn || (interactive && desc.interactiveOn)
		if desc.posix {
			opts.posix[desc.name] = &value
			continue
		}
		opts.bash[desc.name] = &value
	}
	return opts
}

func (o *shellOptions) option(posix bool, name string) (*bool, bool, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if posix {
		opt, ok := o.posix[name]
		if !ok {
			return nil, false, false
		}
		return opt, true, false
	}
	opt, ok := o.bash[name]
	if !ok {
		return nil, false, false
	}
	desc := shellOptionIndex[name]
	return opt, desc.managed || desc.upstream, desc.managed
}

// enabled reports whether the named option is on and whether it exists at
// all. A nil receiver has no options, so nothing is enabled.
func (o *shellOptions) enabled(posix bool, name string) (bool, bool) {
	if o == nil {
		return false, false
	}
	opt, _, _ := o.option(posix, name)
	if opt == nil {
		return false, false
	}
	return *opt, true
}

func (o *shellOptions) set(posix bool, name string, value bool) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	table := o.bash
	if posix {
		table = o.posix
	}
	opt, ok := table[name]
	if !ok {
		return false
	}
	*opt = value
	return true
}

func (o *shellOptions) reset(interactive bool) {
	fresh := newShellOptions(interactive)
	o.mu.Lock()
	o.posix = fresh.posix
	o.bash = fresh.bash
	o.mu.Unlock()
}

// recordSet updates the mirrored POSIX options after the interpreter has
// successfully executed a "set" builtin. It understands the same grouped
// flag syntax as bash: -ex, -o name, +o name, "--", and "-".
func (o *shellOptions) recordSet(args []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" {
			return
		}
		if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
			return
		}
		enable := arg[0] == '-'
		for _, flag := range arg[1:] {
			if flag == 'o' {
				if i+1 >= len(args) {
					return
				}
				i++
				o.set(true, args[i], enable)
				break
			}
			if name, ok := posixOptionNameByFlag(byte(flag)); ok {
				o.set(true, name, enable)
			}
		}
	}
}

func posixOptionNameByFlag(flag byte) (string, bool) {
	switch flag {
	case 'a':
		return "allexport", true
	case 'e':
		return "errexit", true
	case 'n':
		return "noexec", true
	case 'f':
		return "noglob", true
	case 'u':
		return "nounset", true
	case 'x':
		return "xtrace", true
	default:
		return "", false
	}
}

// handleSetVerboseOption keeps "set -v"/"set +v" working as accepted no-ops.
// mvdan.cc/sh/v3 does not implement the Bash verbose option, and v3.14 now
// computes "$-" itself, so we strip the flag before forwarding to its "set".
func handleSetVerboseOption(args []string) ([]string, bool) {
	if len(args) == 0 || args[0] != "set" {
		return nil, false
	}
	changed := false
	forward := []string{args[0]}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			forward = append(forward, args[i:]...)
			return forward, changed
		}
		if arg == "-" {
			forward = append(forward, args[i:]...)
			return forward, changed
		}
		if (arg == "-o" || arg == "+o") && i+1 < len(args) && args[i+1] == "verbose" {
			changed = true
			i++
			continue
		}
		if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
			forward = append(forward, args[i:]...)
			return forward, changed
		}
		var rest []byte
		for idx := 1; idx < len(arg); idx++ {
			if arg[idx] == 'v' {
				changed = true
				continue
			}
			rest = append(rest, arg[idx])
		}
		if len(rest) > 0 {
			forward = append(forward, string(append([]byte{arg[0]}, rest...)))
		}
	}
	if changed && len(forward) == 1 {
		return []string{":"}, true
	}
	return forward, changed
}

func updateCheckwinsizeColumns(opts *shellOptions, runner *interp.Runner, width func() int) {
	enabled, ok := opts.enabled(false, "checkwinsize")
	if !ok || !enabled || width == nil {
		return
	}
	cols := width()
	if cols <= 0 {
		return
	}
	setRunnerStringVar(runner, "COLUMNS", strconv.Itoa(cols))
}

func setRunnerStringVar(runner *interp.Runner, name, value string) {
	if runner == nil {
		return
	}
	vr := expand.Variable{Set: true, Kind: expand.String, Str: value}
	if wenv, ok := runner.Env.(expand.WriteEnviron); ok {
		_ = wenv.Set(name, vr)
	}
	if runner.Vars == nil {
		runner.Vars = make(map[string]expand.Variable)
	}
	runner.Vars[name] = vr
}
