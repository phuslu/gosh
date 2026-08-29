package gosh

import (
	"reflect"
	"strconv"
	"unsafe"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func installShellOptionVariable(runner *interp.Runner, version string) {
	if runner == nil {
		return
	}
	base := runner.Env
	if base == nil {
		base = expand.ListEnviron()
	}
	runner.Env = &shellEnviron{base: base, version: version}
}

type shellEnviron struct {
	base    expand.Environ
	version string
}

func (e *shellEnviron) Get(name string) expand.Variable {
	if name == "BASH_VERSION" {
		return expand.Variable{Set: true, Kind: expand.String, Str: e.version + "(1)-gosh"}
	}
	if e.base == nil {
		return expand.Variable{}
	}
	return e.base.Get(name)
}

func (e *shellEnviron) Each(f func(name string, vr expand.Variable) bool) {
	if e.base == nil {
		return
	}
	e.base.Each(f)
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

func runnerOpts(r *interp.Runner) []bool {
	if r == nil {
		return nil
	}
	val := reflect.ValueOf(r).Elem().FieldByName("opts")
	if !val.IsValid() || !val.CanAddr() || val.Len() == 0 {
		return nil
	}
	ptr := unsafe.Pointer(val.UnsafeAddr())
	return unsafe.Slice((*bool)(ptr), val.Len())
}

func updateCheckwinsizeColumns(runner *interp.Runner, width func() int) {
	enabled, ok := shoptOptionEnabled(runner, false, "checkwinsize")
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
	if wenv := runnerWriteEnv(runner); wenv != nil {
		_ = wenv.Set(name, vr)
	}
	if runner.Vars == nil {
		runner.Vars = make(map[string]expand.Variable)
	}
	runner.Vars[name] = vr
}

func runnerWriteEnv(r *interp.Runner) expand.WriteEnviron {
	if r == nil {
		return nil
	}
	val := reflect.ValueOf(r).Elem().FieldByName("writeEnv")
	if !val.IsValid() || !val.CanAddr() {
		return nil
	}
	ptr := unsafe.Pointer(val.UnsafeAddr())
	return *(*expand.WriteEnviron)(ptr)
}
