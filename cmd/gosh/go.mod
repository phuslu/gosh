module gosh

go 1.26.0

require (
	github.com/phuslu/gosh v1.0.0
	github.com/phuslu/pty v0.0.0-20260518141308-9cb014534fff
)

require (
	github.com/chzyer/readline v1.5.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	mvdan.cc/sh/v3 v3.14.0 // indirect
)

replace github.com/phuslu/gosh v1.0.0 => ../..
