package gosh

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func defaultShell() string {
	if exe, err := exec.LookPath("bash"); err == nil {
		return exe
	}
	switch runtime.GOOS {
	case "windows":
		return "cmd.exe"
	default:
		return "/bin/sh"
	}
}

func environWithDefaultShell(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	env = slices.Clone(env)
	if _, ok := lookupEnv(env, "SHELL"); !ok {
		env = append(env, "SHELL="+defaultShell())
	}
	return env
}

func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		name, value, ok := strings.Cut(env[i], "=")
		if !ok {
			continue
		}
		if name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func expandEnv(env []string, s string) string {
	return os.Expand(s, func(key string) string {
		value, _ := lookupEnv(env, key)
		return value
	})
}

func resolveInitFile(env []string, interactive bool) string {
	file, ok := lookupEnv(env, "GOSH_ENV")
	if !ok && interactive {
		file = "$HOME/.bashrc"
	}
	if file == "" {
		return ""
	}
	return expandEnv(env, file)
}

// runnerStringVar reads one shell variable straight out of the interpreter
// state, giving what `${name-}` would expand to: name references are
// followed, an indexed array yields its first element, and an associative
// array yields the empty string. The second result reports whether the
// variable is set at all, so callers can tell "unset" from "set to empty".
//
// It is not a substitute for an expansion: the parameters the interpreter
// computes per lookup (LINENO, RANDOM, SRANDOM, PPID, DIRSTACK, $?, $$, the
// positional parameters) exist only inside one, and are not read this way.
func runnerStringVar(runner *interp.Runner, name string) (string, bool) {
	vr, ok := lookupRunnerVar(runner, name)
	if !ok {
		return "", false
	}
	return vr.String(), true
}

// maxRunnerNameRefDepth bounds name reference chains, like the interpreter
// does, so that `declare -n a=b; declare -n b=a` cannot hang a lookup.
const maxRunnerNameRefDepth = 100

func lookupRunnerVar(runner *interp.Runner, name string) (expand.Variable, bool) {
	if runner == nil {
		return expand.Variable{}, false
	}
	for range maxRunnerNameRefDepth {
		// Runner.Vars is refreshed from the interpreter's variable scopes
		// after every Run and keeps entries for variables a script unset, so
		// a name found there shadows Runner.Env even when it is not set.
		vr, ok := runner.Vars[name]
		if !ok {
			if runner.Env == nil {
				return expand.Variable{}, false
			}
			vr = runner.Env.Get(name)
		}
		if !vr.IsSet() {
			return expand.Variable{}, false
		}
		if vr.Kind != expand.NameRef {
			return vr, true
		}
		name = vr.Str
	}
	return expand.Variable{}, false
}

func SetEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		name, _, ok := strings.Cut(env[i], "=")
		if !ok {
			continue
		}
		if name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
