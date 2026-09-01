package gosh

import (
	"fmt"
	"strings"
)

type commandSpec struct {
	isCommand   bool
	script      string
	argv0       string
	params      []string
	interactive bool
	readStdin   bool
	noRC        bool
	rcFile      string
	showVersion bool
	showHelp    bool
}

const goshUsage = `Usage: gosh [options] [-c command [arg0 [args...]]]

Options:
  -c command      execute command and exit
  -s              read commands from standard input
  -i              force an interactive shell
  --norc          do not read the interactive startup file
  --rcfile file   read file instead of the default startup file
  --noprofile     accepted for Bash compatibility (no-op)
  --version       print version information
  --help          print this help text
`

func parseCommand(args []string) (*commandSpec, error) {
	spec := &commandSpec{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--":
			return spec, nil
		case "-c":
			if spec.isCommand {
				return nil, fmt.Errorf("gosh: multiple -c options are not supported")
			}
			if i+1 >= len(args) {
				return nil, fmt.Errorf("gosh: -c requires a command string")
			}
			spec.isCommand = true
			spec.script = strings.Clone(args[i+1])
			spec.argv0 = strings.Clone(args[0])
			i++
			if i+1 < len(args) {
				spec.argv0 = strings.Clone(args[i+1])
				i++
				if i+1 < len(args) {
					rest := args[i+1:]
					spec.params = make([]string, len(rest))
					for j, val := range rest {
						spec.params[j] = strings.Clone(val)
					}
				}
			}
			return spec, nil
		case "-s":
			spec.readStdin = true
			if i+1 < len(args) {
				rest := args[i+1:]
				spec.params = make([]string, len(rest))
				for j, val := range rest {
					spec.params[j] = strings.Clone(val)
				}
			}
			return spec, nil
		case "-i":
			spec.interactive = true
		case "--norc":
			spec.noRC = true
		case "--noprofile":
			// Accepted for Bash command-line compatibility; gosh has no
			// separate login-profile mechanism.
		case "--rcfile":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("gosh: --rcfile requires a file name")
			}
			i++
			spec.rcFile = strings.Clone(args[i])
		case "--version":
			spec.showVersion = true
			return spec, nil
		case "--help":
			spec.showHelp = true
			return spec, nil
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("gosh: invalid option %q", arg)
			}
			// Embedders conventionally include the shell name in Args
			// ("gosh", "-c", ...). Ignore positional arguments before a
			// recognized option, matching gosh's previous CLI behavior.
		}
	}
	return spec, nil
}
