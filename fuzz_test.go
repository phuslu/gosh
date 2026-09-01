package gosh

import (
	"strings"
	"testing"
)

func FuzzParseKeySequence(f *testing.F) {
	for _, seed := range []string{
		`"\e[A"`,
		`"\C-a"`,
		`"\x1b"`,
		`"\M-x"`,
		"plain",
		`"unterminated`,
		`"\xZZ"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = parseKeySequence(input)
	})
}

func FuzzCompletionContext(f *testing.F) {
	for _, seed := range []string{
		"echo hello ",
		`git commit -m "`,
		"if foo; then bar ",
		"echo $(foo bar ",
		`foo="a b"; "$foo`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_ = parseCompletionContext([]rune(input))
	})
}

func FuzzParsePromptTemplate(f *testing.F) {
	for _, seed := range []string{
		`\u@\h:\w `,
		`\[\e[1;32m\]\$\[\e[0m\]`,
		`$(git branch --show-current)`,
		`$((31+3*!$?))`,
		`${USER} \D{%F %T} \# \!`,
		`\$(`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		template := parsePromptTemplate(input)
		_ = template.render(&promptState{})
	})
}

func FuzzParseShoptArgs(f *testing.F) {
	for _, seed := range []string{
		"-q extglob",
		"-s -o pipefail",
		"-qu nullglob globstar",
		"-p",
		"--",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_ = parseShoptArgs(splitFuzzArgs(input))
	})
}

func FuzzDecodeHistoryLine(f *testing.F) {
	for _, seed := range []string{
		"echo plain",
		"# gosh-history-v1 ZWNobyBvbmUK",
		"# gosh-history-v1 invalid!!",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_ = decodeHistoryLine(input)
	})
}

// splitFuzzArgs performs the same whitespace split as a shell's flag parser
// tests; it is intentionally simple and only needs to avoid panics.
func splitFuzzArgs(input string) []string {
	if input == "" {
		return nil
	}
	return strings.Fields(input)
}
