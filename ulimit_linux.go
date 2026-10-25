package gosh

import "golang.org/x/sys/unix"

// ulimitResources lists the limits of "ulimit -a" on Linux, in Bash's
// order and units.
var ulimitResources = []ulimitResource{
	{'R', "real-time non-blocking time", "microseconds", unix.RLIMIT_RTTIME, 1},
	{'c', "core file size", "blocks", unix.RLIMIT_CORE, 1024},
	{'d', "data seg size", "kbytes", unix.RLIMIT_DATA, 1024},
	{'e', "scheduling priority", "", unix.RLIMIT_NICE, 1},
	{'f', "file size", "blocks", unix.RLIMIT_FSIZE, 1024},
	{'i', "pending signals", "", unix.RLIMIT_SIGPENDING, 1},
	{'l', "max locked memory", "kbytes", unix.RLIMIT_MEMLOCK, 1024},
	{'m', "max memory size", "kbytes", unix.RLIMIT_RSS, 1024},
	{'n', "open files", "", unix.RLIMIT_NOFILE, 1},
	{'p', "pipe size", "512 bytes", -1, 1},
	{'q', "POSIX message queues", "bytes", unix.RLIMIT_MSGQUEUE, 1},
	{'r', "real-time priority", "", unix.RLIMIT_RTPRIO, 1},
	{'s', "stack size", "kbytes", unix.RLIMIT_STACK, 1024},
	{'t', "cpu time", "seconds", unix.RLIMIT_CPU, 1},
	{'u', "max user processes", "", unix.RLIMIT_NPROC, 1},
	{'v', "virtual memory", "kbytes", unix.RLIMIT_AS, 1024},
	{'x', "file locks", "", unix.RLIMIT_LOCKS, 1},
}
