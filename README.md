# gosh

`gosh` is a compact Bash-style shell written in Go.

It uses [`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3) for shell
parsing and interpretation, then adds the pieces that make it usable as an
interactive shell: readline editing, history, prompt rendering, tab completion,
`bind`, and Bash-flavored compatibility helpers.

Use it as a small local shell, a script runner, or an embeddable shell runtime
inside a Go program.

## Quick tour

Build the CLI from a checkout:

```sh
git clone https://github.com/phuslu/gosh
cd gosh/cmd/gosh
go build -mod=readonly -o gosh .
./gosh
```

Run a one-shot command:

```sh
./gosh -c 'name=gosh; for n in 1 2 3; do printf "%s:%s\n" "$name" "$n"; done'
```

Run a script from standard input:

```sh
printf 'echo one\necho two\n' | ./gosh
```

Start an interactive session:

```sh
./gosh
sh-0.0$ echo "$BASH_VERSION"
0.0.0(1)-gosh
sh-0.0$ shopt -s extglob
sh-0.0$ shopt -q extglob && echo enabled
enabled
```

## Features

- Bash-style syntax through `mvdan.cc/sh/v3`: variables, functions, conditionals,
  loops, pipelines, redirects, command substitution, arithmetic, globbing, and
  shell builtins.
- Interactive line editing with readline.
- Command and path completion.
- Prefix history search for the up/down arrow bindings.
- Bash prompt escape rendering for common `PS1` and `PS2` sequences.
- `bind` support for common cursor and history-search actions.
- `history` builtin backed by `HISTFILE`, `HISTSIZE`, and `HISTCONTROL`.
- `shopt -q` compatibility, including `builtin shopt -q` and
  `command shopt -q`.
- `BASH_VERSION` and `$-` compatibility variables for shell startup files.
- Programmatic use through `gosh.Run`.

## Configuration

Interactive `gosh` loads `GOSH_ENV` when it is set. If `GOSH_ENV` is unset, it
loads `$HOME/.bashrc` for interactive sessions. Non-interactive scripts from
standard input and commands run with `-c` do not load a startup file or render
prompts.

Example `~/.bashrc` or `GOSH_ENV` file:

```sh
if [ -n "${BASH_VERSION-}" ] && case "$-" in *i*) true;; *) false;; esac; then
    bind '"\e[1~": beginning-of-line'
    bind '"\e[4~": end-of-line'
    bind '"\e[5~": previous-screen'
    bind '"\e[6~": next-screen'
    bind '"\e[F": end-of-line'
    bind '"\e[H": beginning-of-line'
    bind '"\e[B": history-search-forward'
    bind '"\e[A": history-search-backward'
fi

export LC_ALL=en_US.UTF-8
export TERM=xterm-256color
export SHELL=/bin/bash
export PATH="$HOME/.local/bin:$PATH"

export HISTFILE="$HOME/.gosh_history"
export HISTSIZE=1000
export HISTCONTROL=ignoreboth

export PS1='\[\e]0;\h:\w\a\]\n\[\e[1;32m\]\u@\H\[\e[0;33m\] \w \[\e[0m[\D{%T}]\n\[\e[1;$((31+3*!$?))m\]\$\[\e[0m\] '

alias ls='ls -p --color'
alias ll='ls -lF --color'
alias grep='grep --color'
```

## Embed in Go

`gosh.Run` accepts explicit input, output, environment, arguments, and working
directory, so it can be used without a process-level shell.

```go
package main

import (
	"bytes"
	"fmt"

	"github.com/phuslu/gosh"
)

func main() {
	var stdout, stderr bytes.Buffer

	err := gosh.Run(gosh.Config{
		Args:   []string{"gosh", "-c", `printf "hello %s\n" "$1"`, "gosh", "world"},
		Stdout: &stdout,
		Stderr: &stderr,
		Env: []string{
			"PATH=/usr/bin:/bin",
			"HOME=/tmp",
			"HISTFILE=/dev/null",
		},
	})
	if err != nil {
		fmt.Println(stderr.String())
		panic(err)
	}
	fmt.Print(stdout.String())
}
```

## Compatibility notes

`gosh` is Bash-flavored, not a byte-for-byte Bash replacement. Shell syntax and
many builtins come from `mvdan.cc/sh/v3`, while external commands are executed
from the host `PATH`.

Some Bash features that depend on a full POSIX job-control shell, a PTY-managed
process tree, or Bash internals may behave differently. Treat `gosh` as a small,
embeddable shell with strong Bash compatibility for common scripts and
interactive workflows, not as a login-shell replacement.

## Development

Run the library tests from the repository root:

```sh
go test ./...
```

Build the CLI from its module:

```sh
cd cmd/gosh
go build -mod=readonly -o gosh .
```
