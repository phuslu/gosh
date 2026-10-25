package gosh

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

const umaskUsage = "umask: usage: umask [-p] [-S] [mode]"

// builtinUmask implements Bash's umask: it prints the file creation mask in
// octal, symbolically with -S or reusable as input with -p, and sets it from
// an octal number or a symbolic mode such as u=rwx,g=rx,o= or g-w. The mask
// belongs to the gosh process, so a subshell setting it changes it for the
// whole shell.
func builtinUmask(args []string, stdout io.Writer) error {
	symbolic, reusable := false, false
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		for _, opt := range args[0][1:] {
			switch opt {
			case 'S':
				symbolic = true
			case 'p':
				reusable = true
			default:
				return builtinUsagef("umask: -%c: invalid option\n%s", opt, umaskUsage)
			}
		}
		args = args[1:]
	}
	mask, err := getUmask()
	if err != nil {
		return fmt.Errorf("umask: %v", err)
	}
	if len(args) == 0 {
		prefix := ""
		if reusable {
			prefix = "umask "
			if symbolic {
				prefix += "-S "
			}
		}
		if symbolic {
			fmt.Fprintf(stdout, "%s%s\n", prefix, umaskSymbolic(mask))
		} else {
			fmt.Fprintf(stdout, "%s%04o\n", prefix, mask)
		}
		return nil
	}
	mode := args[0]
	if mode[0] >= '0' && mode[0] <= '9' {
		n, err := strconv.ParseUint(mode, 8, 32)
		if err != nil || n > 0o777 {
			return fmt.Errorf("umask: %s: octal number out of range", mode)
		}
		mask = int(n)
	} else if mask, err = parseSymbolicUmask(mode, mask); err != nil {
		return err
	}
	if err := setUmask(mask); err != nil {
		return fmt.Errorf("umask: %v", err)
	}
	if symbolic {
		fmt.Fprintln(stdout, umaskSymbolic(mask))
	}
	return nil
}

// umaskSymbolic shows the permissions mask leaves, as "u=rwx,g=rx,o=rx".
func umaskSymbolic(mask int) string {
	perm := ^mask & 0o777
	var parts []string
	for i, who := range []string{"u", "g", "o"} {
		bits := perm >> (6 - 3*i) & 7
		var b strings.Builder
		b.WriteString(who + "=")
		for k, c := range "rwx" {
			if bits&(4>>k) != 0 {
				b.WriteRune(c)
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ",")
}

// parseSymbolicUmask applies a symbolic mode, as chmod reads it, to the
// permissions mask leaves and returns the resulting mask.
func parseSymbolicUmask(mode string, mask int) (int, error) {
	perm := ^mask & 0o777
	for _, clause := range strings.Split(mode, ",") {
		who := 0
		i := 0
		for ; i < len(clause) && strings.IndexByte("ugoa", clause[i]) >= 0; i++ {
			switch clause[i] {
			case 'u':
				who |= 0o700
			case 'g':
				who |= 0o070
			case 'o':
				who |= 0o007
			case 'a':
				who |= 0o777
			}
		}
		if who == 0 {
			who = 0o777
		}
		if i >= len(clause) || strings.IndexByte("=+-", clause[i]) < 0 {
			if i < len(clause) {
				return 0, fmt.Errorf("umask: `%c': invalid symbolic mode operator", clause[i])
			}
			return 0, fmt.Errorf("umask: %s: invalid symbolic mode", mode)
		}
		op := clause[i]
		bits := 0
		for _, c := range clause[i+1:] {
			switch c {
			case 'r':
				bits |= 0o444
			case 'w':
				bits |= 0o222
			case 'x':
				bits |= 0o111
			default:
				return 0, fmt.Errorf("umask: `%c': invalid symbolic mode character", c)
			}
		}
		bits &= who
		switch op {
		case '=':
			perm = perm&^who | bits
		case '+':
			perm |= bits
		case '-':
			perm &^= bits
		}
	}
	return ^perm & 0o777, nil
}
