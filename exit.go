package gosh

import (
	"context"
	"errors"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var status interp.ExitStatus
	if errors.As(err, &status) {
		return int(status)
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return 2
	}
	var parse syntax.ParseError
	if errors.As(err, &parse) {
		return 2
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 127
}

func IsExitStatus(err error) bool {
	var status interp.ExitStatus
	return errors.As(err, &status)
}
