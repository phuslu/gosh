//go:build !linux && !windows

package gosh

import "golang.org/x/sys/unix"

// ulimitResources lists the limits of "ulimit -a" on the other Unix
// systems, those every one of them has, in Bash's order and units.
var ulimitResources = []ulimitResource{
	{'c', "core file size", "blocks", unix.RLIMIT_CORE, 1024},
	{'d', "data seg size", "kbytes", unix.RLIMIT_DATA, 1024},
	{'f', "file size", "blocks", unix.RLIMIT_FSIZE, 1024},
	{'n', "open files", "", unix.RLIMIT_NOFILE, 1},
	{'p', "pipe size", "512 bytes", -1, 1},
	{'s', "stack size", "kbytes", unix.RLIMIT_STACK, 1024},
	{'t', "cpu time", "seconds", unix.RLIMIT_CPU, 1},
}
