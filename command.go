package gosh

import (
	"fmt"
	"strings"
)

type commandSpec struct {
	isCommand   bool
	script      string
	scriptFile  string
	argv0       string
	params      []string
	setArgs     []string
	interactive bool
	readStdin   bool
	noRC        bool
	rcFile      string
	showVersion bool
	showHelp    bool
}

const goshUsage = `Usage: gosh [options] [-c command [arg0 [args...]] | script [args...]]

Options:
  -c              read commands from the first argument and exit
  -s              read commands from standard input
  -i              force an interactive shell
  -aefnux, +aefnux
                  set or unset shell options, as for the set builtin
  -o name, +o name
                  set or unset a shell option by name, as for set -o
  --norc          do not read the interactive startup file
  --rcfile file   read file instead of the default startup file
  --noprofile     accepted for Bash compatibility (no-op)
  --version       print version information
  --help          print this help text
`

// usageError reports an invalid gosh command line. Like Bash, gosh exits
// with status 2 for it.
type usageError struct {
	msg string
}

func (e *usageError) Error() string { return e.msg }

func usageErrorf(msg string, args ...any) error {
	return &usageError{msg: "gosh: " + fmt.Sprintf(msg, args...)}
}

// parseCommand parses a Bash-style command line. Options may be grouped
// ("-ec") and end at "--", "-" or the first non-option argument, which is the
// command string with -c, the first positional parameter with -s, and
// otherwise the script file to run.
func parseCommand(args []string) (*commandSpec, error) {
	spec := &commandSpec{}
	i := 1
options:
	for ; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--", "-":
			i++
			break options
		case "--norc":
			spec.noRC = true
			continue
		case "--noprofile":
			// Accepted for Bash command-line compatibility; gosh has no
			// separate login-profile mechanism.
			continue
		case "--rcfile":
			if i+1 >= len(args) {
				return nil, usageErrorf("--rcfile requires a file name")
			}
			i++
			spec.rcFile = strings.Clone(args[i])
			continue
		case "--version":
			spec.showVersion = true
			return spec, nil
		case "--help":
			spec.showHelp = true
			return spec, nil
		}
		if len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
			break options
		}
		if strings.HasPrefix(arg, "--") {
			return nil, usageErrorf("invalid option %q", arg)
		}
		enable := arg[0] == '-'
		sign := arg[:1]
		for _, flag := range arg[1:] {
			switch {
			case flag == 'c' && enable:
				spec.isCommand = true
			case flag == 's' && enable:
				spec.readStdin = true
			case flag == 'i' && enable:
				spec.interactive = true
			case flag == 'v':
				// Accepted like "set -v", which gosh treats as a no-op.
			case flag == 'o':
				if i+1 >= len(args) {
					return nil, usageErrorf("%so requires an option name", sign)
				}
				i++
				name := args[i]
				if name == "verbose" {
					continue
				}
				if !isPosixOptionName(name) {
					return nil, usageErrorf("%s: invalid option name", name)
				}
				spec.setArgs = append(spec.setArgs, sign+"o", strings.Clone(name))
			default:
				if _, ok := posixOptionNameByFlag(byte(flag)); !ok {
					return nil, usageErrorf("invalid option %q", sign+string(flag))
				}
				spec.setArgs = append(spec.setArgs, sign+string(flag))
			}
		}
	}

	rest := make([]string, len(args)-i)
	for j, val := range args[i:] {
		rest[j] = strings.Clone(val)
	}
	spec.argv0 = strings.Clone(args[0])
	switch {
	case spec.isCommand:
		if len(rest) == 0 {
			return nil, usageErrorf("-c requires a command string")
		}
		spec.script = rest[0]
		if len(rest) > 1 {
			spec.argv0 = rest[1]
			spec.params = rest[2:]
		}
	case spec.readStdin:
		spec.params = rest
	case len(rest) > 0:
		spec.scriptFile = rest[0]
		spec.argv0 = rest[0]
		spec.params = rest[1:]
	}
	return spec, nil
}

func isPosixOptionName(name string) bool {
	for _, desc := range shellOptionTable {
		if desc.posix && desc.name == name {
			return true
		}
	}
	return false
}
