module gosh

go 1.26.0

require (
	github.com/phuslu/gosh v1.0.0
	github.com/phuslu/pty v0.0.0-20260904123709-7bfbeb22b99f
)

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	mvdan.cc/sh/v3 v3.14.0 // indirect
)

replace github.com/phuslu/gosh v1.0.0 => ../..
