package gosh_test

import (
	"bytes"
	"fmt"

	"github.com/phuslu/gosh"
)

func ExampleRun() {
	var stdout, stderr bytes.Buffer
	err := gosh.Run(gosh.Config{
		Args:   []string{"gosh", "-c", `printf "hello %s\n" "$1"`, "gosh", "world"},
		Stdout: &stdout,
		Stderr: &stderr,
		Env: []string{
			"HOME=/tmp",
			"PATH=/usr/bin:/bin",
		},
	})
	if err != nil {
		fmt.Println(stderr.String())
		panic(err)
	}
	fmt.Print(stdout.String())
	// Output: hello world
}
