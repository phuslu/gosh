package gosh

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

// completionSentinel is a valid shell word that is inserted at the cursor when
// the cursor sits at a fresh word position, so that the parser produces a word
// node for the position being completed. It is located by position (a word
// starting exactly at the cursor offset cannot otherwise exist in line[:pos]),
// so its text never leaks into the derived context.
const completionSentinel = "gosh-completion-sentinel"

// maxCompletionRepairs bounds the parser-driven repair loop. Complex or
// pathological input gives up well before this limit.
const maxCompletionRepairs = 8

// parseCompletionContext derives a completionContext from the text before the
// cursor using mvdan.cc/sh's parser and AST. line must already be truncated to
// the cursor position.
func parseCompletionContext(line []rune) completionContext {
	src := trimCompletionTrailingBackslash(string(line))
	cursor := len(src)

	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	repaired := src
	sentinelInserted := false
	for i := 0; i < maxCompletionRepairs; i++ {
		prog, err := parser.Parse(strings.NewReader(repaired), "")
		if err == nil {
			// A fresh word position needs a sentinel word before the AST is
			// walked, otherwise the cursor lands inside a wrapper word such as
			// `$(...)` and the inner command position is never seen.
			if !sentinelInserted && completionFreshPosition(src) {
				repaired = insertCompletionSentinel(repaired, cursor)
				sentinelInserted = true
				continue
			}
			if ctx, ok := completionContextFromFile(prog, repaired, cursor, sentinelInserted); ok {
				return ctx
			}
			if sentinelInserted {
				return completionContext{}
			}
			return completionContext{}
		}
		pe, ok := err.(syntax.ParseError)
		if !ok {
			return completionContext{}
		}
		var fixed bool
		repaired, sentinelInserted, fixed = repairCompletion(pe, repaired, src, cursor, sentinelInserted)
		if !fixed {
			return completionContext{}
		}
	}
	return completionContext{}
}

// trimCompletionTrailingBackslash drops one trailing backslash when the input
// ends with a dangling escape outside quotes. The parser keeps such a
// backslash as literal text, which would poison the completion prefix.
func trimCompletionTrailingBackslash(src string) string {
	if !strings.HasSuffix(src, "\\") {
		return src
	}
	inSingle, inDouble := completionEndQuoteState(src)
	if inSingle || inDouble {
		return src
	}
	backslashes := 0
	for i := len(src) - 1; i >= 0 && src[i] == '\\'; i-- {
		backslashes++
	}
	if backslashes%2 == 0 {
		return src
	}
	return src[:len(src)-1]
}

// completionFreshPosition reports whether the cursor sits at a fresh word
// position: the text before it ends with whitespace or an operator that
// starts a new word rather than inside an existing word.
func completionFreshPosition(src string) bool {
	// The cursor is at a fresh word position when the innermost open
	// construct at the cursor is the command list itself (or a command
	// substitution) and the last character starts a new word. Inside an open
	// quote the position belongs to the quoted word instead.
	top := completionInnermostContext(src)
	if top == '\'' || top == '"' || top == '`' {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(src)
	if r == utf8.RuneError && src == "" {
		return true
	}
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '|', '&', ';', '<', '>', '(', '{':
		return true
	}
	return false
}

// completionInnermostContext returns the kind of the innermost unclosed
// construct at the end of src: 0 for the top-level command list, '\” or '"'
// for single or double quotes, '`' for backquote command substitution, and
// '(' for a `$(` command substitution. It tracks the nesting needed to
// distinguish `"foo ` (inside a quote) from `"$(foo ` (inside the
// substitution, which itself sits in a quote).
func completionInnermostContext(src string) byte {
	var stack []byte
	i := 0
	for i < len(src) {
		top := byte(0)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		c := src[i]
		switch top {
		case '\'':
			if c == '\'' {
				stack = stack[:len(stack)-1]
			}
		case '"':
			switch c {
			case '\\':
				i++
			case '"':
				stack = stack[:len(stack)-1]
			case '$':
				if i+1 < len(src) && src[i+1] == '(' {
					stack = append(stack, '(')
					i++
				}
			}
		case '`':
			switch c {
			case '\\':
				i++
			case '`':
				stack = stack[:len(stack)-1]
			}
		case '(':
			switch c {
			case '\\':
				i++
			case '\'':
				stack = append(stack, '\'')
			case '"':
				stack = append(stack, '"')
			case '`':
				stack = append(stack, '`')
			case '$':
				if i+1 < len(src) && src[i+1] == '(' {
					stack = append(stack, '(')
					i++
				}
			case ')':
				stack = stack[:len(stack)-1]
			}
		default:
			switch c {
			case '\\':
				i++
			case '\'':
				stack = append(stack, '\'')
			case '"':
				stack = append(stack, '"')
			case '`':
				stack = append(stack, '`')
			case '$':
				if i+1 < len(src) && src[i+1] == '(' {
					stack = append(stack, '(')
					i++
				}
			}
		}
		i++
	}
	if len(stack) == 0 {
		return 0
	}
	return stack[len(stack)-1]
}

// completionEndQuoteState reports whether src ends inside a single-quoted or
// double-quoted segment, honoring backslash escapes.
func completionEndQuoteState(src string) (inSingle, inDouble bool) {
	escaped := false
	for i := 0; i < len(src); i++ {
		r := src[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if r == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			switch r {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			}
			continue
		}
		switch r {
		case '\\':
			escaped = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		}
	}
	return inSingle, inDouble
}

func insertCompletionSentinel(src string, cursor int) string {
	return src[:cursor] + completionSentinel + src[cursor:]
}

// repairCompletion reacts to a ParseError from the repaired input. It returns
// the next repaired string, whether the sentinel is now present, and whether
// the repair succeeded. All appends happen after the cursor, so the cursor
// offset stays fixed for the whole repair loop.
func repairCompletion(pe syntax.ParseError, repaired, src string, cursor int, sentinelInserted bool) (string, bool, bool) {
	text := pe.Text
	if !pe.Incomplete {
		// mvdan reports a bare `${` at EOF as a non-incomplete error. The
		// sentinel turns it into a valid parameter name.
		if strings.Contains(text, "invalid parameter name") && strings.HasSuffix(strings.TrimSpace(repaired), "${") {
			if sentinelInserted {
				return repaired, sentinelInserted, false
			}
			return insertCompletionSentinel(repaired, cursor), true, true
		}
		return repaired, sentinelInserted, false
	}

	if strings.Contains(text, "here-document") {
		// There is no safe way to complete inside or after an unfinished
		// here-document: give up rather than guess.
		return repaired, sentinelInserted, false
	}
	if strings.Contains(text, "closing quote") {
		q := completionQuoteFromError(text)
		if q == "" {
			return repaired, sentinelInserted, false
		}
		return repaired + q, sentinelInserted, true
	}

	needSentinel := func() (string, bool, bool) {
		if sentinelInserted {
			return repaired, sentinelInserted, false
		}
		return insertCompletionSentinel(repaired, cursor), true, true
	}
	headerRepair := func(suffix string) (string, bool, bool) {
		if !sentinelInserted && completionFreshPosition(src) {
			repaired = insertCompletionSentinel(repaired, cursor)
			sentinelInserted = true
		}
		return repaired + suffix, sentinelInserted, true
	}

	switch {
	case strings.Contains(text, "`if <cond>` must be followed by `then`"):
		return headerRepair("; then :; fi")
	case strings.Contains(text, "`while` must be followed by a statement list"),
		strings.Contains(text, "`until` must be followed by a statement list"):
		return headerRepair("; do :; done")
	case strings.Contains(text, "must be followed by `do`"):
		return headerRepair("; do :; done")
	case strings.Contains(text, "`foo()` must be followed by a statement"):
		// `foo()` is a declaration awaiting a body; the sentinel is the body.
		// `function foo` is a declaration awaiting a name and a body.
		if strings.HasSuffix(strings.TrimRight(repaired, " \t"), ")") {
			return needSentinel()
		}
		return repaired + " { :; }", sentinelInserted, true
	case strings.Contains(text, "must be followed by a statement list"),
		strings.Contains(text, "must be followed by a statement"),
		strings.Contains(text, "must be followed by a word"),
		strings.Contains(text, "must be followed by an expression"),
		strings.Contains(text, "requires a command"),
		strings.Contains(text, "must be followed by a name"):
		return needSentinel()
	case strings.Contains(text, "statement must end with `fi`"):
		return repaired + "; fi", sentinelInserted, true
	case strings.Contains(text, "statement must end with `done`"):
		return repaired + "; done", sentinelInserted, true
	case strings.Contains(text, "case patterns must be separated"):
		return repaired + ") ;; esac", sentinelInserted, true
	case strings.Contains(text, "statement must end with `esac`"):
		if !strings.Contains(caseTailAfterIn(repaired), ")") {
			// No pattern has been opened yet; the sentinel becomes it.
			if !sentinelInserted && completionFreshPosition(src) {
				repaired = insertCompletionSentinel(repaired, cursor)
				sentinelInserted = true
			}
			return repaired + ") ;; esac", sentinelInserted, true
		}
		return repaired + ";; esac", sentinelInserted, true
	case strings.Contains(text, "with `))`"):
		return repaired + "))", sentinelInserted, true
	case strings.Contains(text, "with `]]`"):
		return repaired + " ]]", sentinelInserted, true
	case strings.Contains(text, "with `]`"):
		return repaired + " ]", sentinelInserted, true
	case strings.Contains(text, "matching `${` with `}`"):
		return repaired + "}", sentinelInserted, true
	case strings.Contains(text, "matching `{` with `}`"):
		if strings.HasSuffix(strings.TrimRight(repaired, " \t"), ";") {
			return repaired + " }", sentinelInserted, true
		}
		return repaired + "; }", sentinelInserted, true
	case strings.Contains(text, "with `)`"):
		return repaired + ")", sentinelInserted, true
	default:
		return needSentinel()
	}
}

func completionQuoteFromError(text string) string {
	if idx := strings.Index(text, "quote `"); idx >= 0 {
		if rest := text[idx+len("quote `"):]; rest != "" {
			return rest[:1]
		}
	}
	return ""
}

func caseTailAfterIn(src string) string {
	if idx := strings.LastIndex(src, " in "); idx >= 0 {
		return src[idx+len(" in "):]
	}
	return src
}

// completionContextFromFile derives the completion context from a parsed
// (and repaired) file. It returns false when the cursor position does not map
// to any word in the tree.
func completionContextFromFile(f *syntax.File, src string, cursor int, sentinelInserted bool) (completionContext, bool) {
	curWord, ok := completionWordAtCursor(f, cursor, sentinelInserted)
	if !ok {
		return completionContext{}, false
	}

	if arith := completionArithmExpAt(f, cursor); arith != nil {
		start := int(arith.Pos().Offset())
		skip := 2
		if !arith.Bracket {
			skip = 3 // $((  ->  expression starts three bytes in
		}
		start += skip
		if start > cursor {
			start = cursor
		}
		return completionContext{prefix: src[start:cursor], inWord: true}, true
	}
	if param := completionParamExpAt(f, cursor); param != nil {
		start := int(param.Pos().Offset())
		var prefix string
		if param.Short || start+2 > cursor {
			prefix = src[start:cursor]
		} else {
			prefix = src[start+2 : cursor]
		}
		return completionContext{prefix: prefix, quote: completionQuoteAtWord(curWord, cursor), inWord: true}, true
	}
	if name, ok := completionFunctionNameAt(f, cursor); ok {
		return completionContext{
			prefix:    src[int(name.Pos().Offset()):cursor],
			isCommand: true,
			inWord:    true,
		}, true
	}
	if call, contains := completionCallAtCursor(f, cursor); call != nil {
		return completionContextFromCall(call, curWord, src, cursor, contains)
	}

	prefix, quote := completionWordPrefix(curWord, src, cursor)
	return completionContext{
		prefix: prefix,
		quote:  quote,
		inWord: int(curWord.Pos().Offset()) != cursor,
	}, true
}

func completionWordAtCursor(f *syntax.File, cursor int, sentinelInserted bool) (*syntax.Word, bool) {
	var best *syntax.Word
	bestSpan := -1
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		w, ok := n.(*syntax.Word)
		if !ok {
			return true
		}
		start := int(w.Pos().Offset())
		end := int(w.End().Offset())
		if sentinelInserted {
			if start == cursor && cursor < end {
				if best == nil || end-start < bestSpan {
					best, bestSpan = w, end-start
				}
			}
			return true
		}
		if start <= cursor && cursor <= end {
			if best == nil || end-start < bestSpan {
				best, bestSpan = w, end-start
			}
		}
		return true
	})
	return best, best != nil
}

func completionArithmExpAt(f *syntax.File, cursor int) *syntax.ArithmExp {
	var best *syntax.ArithmExp
	bestSpan := -1
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		a, ok := n.(*syntax.ArithmExp)
		if !ok {
			return true
		}
		start := int(a.Pos().Offset())
		end := int(a.End().Offset())
		if start < cursor && cursor < end {
			if best == nil || end-start < bestSpan {
				best, bestSpan = a, end-start
			}
		}
		return true
	})
	return best
}

func completionParamExpAt(f *syntax.File, cursor int) *syntax.ParamExp {
	var best *syntax.ParamExp
	bestSpan := -1
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		p, ok := n.(*syntax.ParamExp)
		if !ok {
			return true
		}
		start := int(p.Pos().Offset())
		end := int(p.End().Offset())
		if start <= cursor && cursor < end {
			if best == nil || end-start < bestSpan {
				best, bestSpan = p, end-start
			}
		}
		return true
	})
	return best
}

func completionFunctionNameAt(f *syntax.File, cursor int) (*syntax.Lit, bool) {
	var name *syntax.Lit
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		fd, ok := n.(*syntax.FuncDecl)
		if !ok || fd.Name == nil {
			return true
		}
		if int(fd.Name.Pos().Offset()) <= cursor && cursor <= int(fd.Name.End().Offset()) {
			name = fd.Name
		}
		return true
	})
	return name, name != nil
}

func completionCallAtCursor(f *syntax.File, cursor int) (*syntax.CallExpr, bool) {
	var best *syntax.CallExpr
	contains := false
	bestSpan := -1
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		c, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		start := int(c.Pos().Offset())
		end := int(c.End().Offset())
		if start <= cursor && cursor <= end {
			if best == nil || end-start < bestSpan {
				best, bestSpan = c, end-start
				contains = true
			}
		}
		return true
	})
	if best != nil {
		return best, contains
	}
	// The cursor can sit past a call's End when it targets a redirect word,
	// e.g. `echo a <`. Fall back to the closest preceding command.
	best = nil
	bestSpan = -1
	bestEnd := -1
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			return false
		}
		c, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		end := int(c.End().Offset())
		if end > cursor {
			return true
		}
		span := end - int(c.Pos().Offset())
		if end > bestEnd || (end == bestEnd && (best == nil || span < bestSpan)) {
			best, bestEnd, bestSpan = c, end, span
		}
		return true
	})
	return best, false
}

func completionContextFromCall(call *syntax.CallExpr, curWord *syntax.Word, src string, cursor int, containsCursor bool) (completionContext, bool) {
	idx := -1
	for i, arg := range call.Args {
		if arg == curWord {
			idx = i
			break
		}
	}
	if !containsCursor {
		// A redirect word follows the command; it is a fresh argument slot.
		words := completionCallWords(call, src, len(call.Args))
		ctx := completionContext{words: words, cword: len(words)}
		if len(call.Args) == 0 {
			ctx.isCommand = true
		} else {
			ctx.command = completionWordLiteral(call.Args[0], src)
		}
		return ctx, true
	}
	if idx < 0 {
		prefix, quote := completionWordPrefix(curWord, src, cursor)
		return completionContext{
			prefix: prefix,
			quote:  quote,
			inWord: int(curWord.Pos().Offset()) != cursor,
		}, true
	}

	words := completionCallWords(call, src, idx)
	prefix, quote := completionWordPrefix(curWord, src, cursor)
	ctx := completionContext{
		prefix: prefix,
		quote:  quote,
		words:  words,
		cword:  len(words),
		inWord: int(curWord.Pos().Offset()) != cursor,
	}
	if idx == 0 {
		ctx.isCommand = true
	} else {
		ctx.command = completionWordLiteral(call.Args[0], src)
	}
	return ctx, true
}

func completionCallWords(call *syntax.CallExpr, src string, argLimit int) []string {
	var words []string
	if n := len(call.Assigns) + argLimit; n > 0 {
		words = make([]string, 0, n)
	}
	for _, assign := range call.Assigns {
		words = append(words, src[int(assign.Pos().Offset()):int(assign.End().Offset())])
	}
	for _, arg := range call.Args[:argLimit] {
		words = append(words, completionWordLiteral(arg, src))
	}
	return words
}

// completionWordLiteral returns the literal text of a whole word, unquoting
// literals and quoted segments while keeping expansions verbatim.
func completionWordLiteral(w *syntax.Word, src string) string {
	var b strings.Builder
	for _, part := range w.Parts {
		b.WriteString(completionPartLiteral(part, src))
	}
	return b.String()
}

// completionWordPrefix returns the literal text of the word up to upTo along
// with the quote rune active at that offset (0 when unquoted).
func completionWordPrefix(w *syntax.Word, src string, upTo int) (string, rune) {
	var b strings.Builder
	for _, part := range w.Parts {
		start := int(part.Pos().Offset())
		end := int(part.End().Offset())
		if end <= upTo {
			b.WriteString(completionPartLiteral(part, src))
			continue
		}
		if start >= upTo {
			break
		}
		text, quote := completionPartLiteralUpTo(part, src, upTo)
		b.WriteString(text)
		return b.String(), quote
	}
	return b.String(), 0
}

func completionPartLiteral(part syntax.WordPart, src string) string {
	switch x := part.(type) {
	case *syntax.Lit:
		return unescapeCompletionLiteral(x.Value)
	case *syntax.SglQuoted:
		return x.Value
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, inner := range x.Parts {
			b.WriteString(completionPartLiteral(inner, src))
		}
		return b.String()
	default:
		return src[int(x.Pos().Offset()):int(x.End().Offset())]
	}
}

func completionPartLiteralUpTo(part syntax.WordPart, src string, upTo int) (string, rune) {
	switch x := part.(type) {
	case *syntax.Lit:
		return unescapeCompletionLiteral(src[int(x.Pos().Offset()):upTo]), 0
	case *syntax.SglQuoted:
		return src[int(x.Left.Offset())+1 : upTo], '\''
	case *syntax.DblQuoted:
		var b strings.Builder
		for _, inner := range x.Parts {
			if int(inner.Pos().Offset()) >= upTo {
				break
			}
			if int(inner.End().Offset()) <= upTo {
				b.WriteString(completionPartLiteral(inner, src))
				continue
			}
			text, _ := completionPartLiteralUpTo(inner, src, upTo)
			b.WriteString(text)
			break
		}
		return b.String(), '"'
	default:
		return src[int(x.Pos().Offset()):upTo], 0
	}
}

func completionQuoteAtWord(w *syntax.Word, cursor int) rune {
	for _, part := range w.Parts {
		switch x := part.(type) {
		case *syntax.SglQuoted:
			if int(x.Left.Offset()) < cursor && cursor < int(x.End().Offset()) {
				return '\''
			}
		case *syntax.DblQuoted:
			if int(x.Left.Offset()) < cursor && cursor < int(x.End().Offset()) {
				return '"'
			}
		}
	}
	return 0
}

// unescapeCompletionLiteral removes backslash escapes from a literal segment.
// Completion input is a single readline line, so backslash-newline
// continuations never occur and a plain escape is the only case.
func unescapeCompletionLiteral(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	escaped := false
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
