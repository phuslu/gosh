package gosh

import (
	"reflect"
	"strconv"
	"strings"
	"unsafe"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func installShellOptionVariable(runner *interp.Runner, interactive, readFromStdin bool, version string) {
	if runner == nil {
		return
	}
	base := runner.Env
	if base == nil {
		base = expand.ListEnviron()
	}
	provider := &shellOptionProvider{
		runner:        runner,
		interactive:   interactive,
		readFromStdin: readFromStdin,
	}
	runner.Env = &shellEnviron{base: base, flags: provider.Flags, version: version}
}

type shellEnviron struct {
	base    expand.Environ
	flags   func() string
	version string
}

func (e *shellEnviron) Get(name string) expand.Variable {
	switch name {
	case "-":
		return expand.Variable{Set: true, Kind: expand.String, Str: e.flags()}
	case "BASH_VERSION":
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

type shellOptionProvider struct {
	runner        *interp.Runner
	interactive   bool
	readFromStdin bool
}

func (p *shellOptionProvider) Flags() string {
	if p == nil || p.runner == nil {
		return ""
	}
	opts := runnerOpts(p.runner)
	var b strings.Builder
	b.WriteByte('h')
	if p.interactive {
		b.WriteByte('i')
		b.WriteByte('m')
		b.WriteByte('B')
		b.WriteByte('H')
		b.WriteByte('s')
	} else {
		b.WriteByte('B')
		if p.readFromStdin {
			b.WriteByte('s')
		}
	}
	// shell option mapping based on interp.shellOptsTable order
	// (allexport, errexit, noexec, noglob, nounset, xtrace).
	for _, opt := range []struct {
		index int
		flag  byte
	}{
		{0, 'a'},
		{1, 'e'},
		{2, 'n'},
		{3, 'f'},
		{4, 'u'},
		{5, 'x'},
	} {
		if opt.index < len(opts) && opts[opt.index] {
			b.WriteByte(opt.flag)
		}
	}
	return b.String()
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
