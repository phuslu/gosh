module gosh

go 1.26

require (
	github.com/phuslu/gosh v1.0.0
	github.com/phuslu/pty v0.0.0-20260515102020-389761547580
)

require (
	github.com/chzyer/readline v1.5.1 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/term v0.41.0 // indirect
	mvdan.cc/sh/v3 v3.13.1 // indirect
)

replace (
	github.com/phuslu/gosh v1.0.0 => ../..
)