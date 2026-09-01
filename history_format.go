package gosh

import "strings"

// This file implements the cmdhist/lithist formatting Bash applies when a
// multi-line command is stored as one history entry. With cmdhist enabled the
// physical lines are concatenated, and with lithist disabled the newlines are
// replaced by the delimiter that keeps the entry valid: usually "; ", but a
// space or a preserved newline where a semicolon would change the meaning.
// The logic mirrors history_delimiting_chars in Bash's parse.y for the
// constructs mvdan.cc/sh parses.

type historyTokenKind uint8

const (
	htNone historyTokenKind = iota
	htNewline
	htWord
	htLBrace // {
	htRBrace // }
	htLParen // (
	htRParen // )
	htSemi
	htAmp
	htPipe
	htPipeAmp // |&
	htDblSemi
	htSemiAmp
	htSemiSemiAmp
	htAndAnd
	htOrOr
	htIf
	htThen
	htElif
	htElse
	htFi
	htFor
	htIn
	htWhile
	htUntil
	htDo
	htDone
	htCase
	htEsac
	htFunction
	htSelect
)

type historyDelimKind uint8

const (
	histDelimSingle   historyDelimKind = iota // '...' with no backslash escapes
	histDelimDouble                           // "..." with backslash escapes
	histDelimBacktick                         // `...` with backslash escapes
	histDelimParen                            // $(...), $((...)), <(...), >(...), (...)
	histDelimCond                             // [[...]]
)

type historyDelim struct {
	kind  historyDelimKind
	close byte
}

// historyScanState is the per-entry lexer state needed to decide the
// separator between two physical lines. It only tracks what affects the
// newline-to-delimiter conversion, not the full shell grammar.
type historyScanState struct {
	delims       []historyDelim
	heredocTerm  string
	heredocTab   bool
	inHeredoc    bool
	heredocFirst bool
	compAssign   int
	caseDepth    int
	lastToken    historyTokenKind
	twoAgo       historyTokenKind
	cmdStart     bool
	inWord       bool
	hasComment   bool
}

func (s *historyScanState) depth() int { return len(s.delims) }

// formatHistoryEntries turns the physical lines of one parsed command batch
// into history entries. With cmdhist disabled every physical line is its own
// entry. With cmdhist enabled the lines are joined into one entry, and
// lithist controls whether embedded newlines survive.
func formatHistoryEntries(lines []string, cmdhist, lithist bool) []string {
	if len(lines) == 0 {
		return nil
	}
	if !cmdhist {
		out := make([]string, len(lines))
		for i, line := range lines {
			out[i] = strings.TrimRight(line, "\r")
		}
		return out
	}
	if len(lines) == 1 {
		return []string{strings.TrimRight(lines[0], "\r")}
	}

	var b strings.Builder
	var sc historyScanState
	sc.cmdStart = true
	b.WriteString(strings.TrimRight(lines[0], "\r"))
	sc.scanLine(b.String())
	commentMarked := sc.hasComment

	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")

		// An escaped newline joins the next line directly, dropping the
		// backslash exactly like the parser does.
		if !sc.inHeredoc && sc.depth() == 0 && endsWithSingleBackslash(b.String()) {
			s := b.String()
			b.Reset()
			b.WriteString(s[:len(s)-1])
			b.WriteString(line)
			sc.scanLine(line)
			commentMarked = false
			continue
		}

		// A line whose first non-blank character starts a comment is dropped
		// from the entry unless lithist is on. The following line keeps its
		// newline because a semicolon could comment out the remainder.
		if !sc.inHeredoc && sc.depth() == 0 && strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			if !lithist {
				commentMarked = true
				sc.twoAgo = sc.lastToken
				sc.lastToken = htNewline
				sc.cmdStart = true
				continue
			}
			b.WriteByte('\n')
			b.WriteString(line)
			commentMarked = true
			sc.twoAgo = sc.lastToken
			sc.lastToken = htNewline
			sc.cmdStart = true
			continue
		}

		if strings.TrimSpace(line) == "" {
			switch {
			case sc.inHeredoc:
				sc.appendHeredocLine(&b, line)
				commentMarked = false
			case sc.depth() > 0:
				b.WriteByte('\n')
				b.WriteString(line)
				sc.scanLine(line)
				commentMarked = false
			case historyNoSemiSuccessor(sc.lastToken):
				// A blank continuation after a token that cannot precede a
				// semicolon is dropped entirely.
				sc.twoAgo = sc.lastToken
				sc.lastToken = htNewline
				sc.cmdStart = true
			default:
				if lithist {
					b.WriteByte('\n')
				} else {
					b.WriteString("; ")
				}
				sc.twoAgo = sc.lastToken
				sc.lastToken = htNewline
				sc.cmdStart = true
				commentMarked = false
			}
			continue
		}

		if sc.inHeredoc {
			sc.appendHeredocLine(&b, line)
			commentMarked = false
			continue
		}

		b.WriteString(historyLineSeparator(&sc, b.String(), line, commentMarked, lithist))
		b.WriteString(line)
		sc.scanLine(line)
		commentMarked = sc.hasComment
	}
	return []string{b.String()}
}

// appendHeredocLine stores a heredoc body line exactly as Bash does: the
// first body line is preceded by a newline, and every body line keeps its
// trailing newline (including the terminator).
func (s *historyScanState) appendHeredocLine(b *strings.Builder, line string) {
	if s.heredocFirst {
		b.WriteByte('\n')
		s.heredocFirst = false
	}
	b.WriteString(line)
	b.WriteByte('\n')
	s.finishHeredocLine(line)
}

// finishHeredocLine appends state transitions after a heredoc body line and
// detects the terminator, which returns the token stream to a newline state.
func (s *historyScanState) finishHeredocLine(line string) {
	if !s.inHeredoc {
		return
	}
	cmp := strings.TrimRight(line, "\r")
	if s.heredocTab {
		cmp = strings.TrimLeft(cmp, "\t")
	}
	if cmp != s.heredocTerm {
		return
	}
	s.inHeredoc = false
	s.heredocTerm = ""
	s.twoAgo = s.lastToken
	s.lastToken = htNewline
	s.cmdStart = true
}

func historyLineSeparator(sc *historyScanState, entry, line string, afterComment, lithist bool) string {
	if lithist || afterComment {
		return "\n"
	}
	if sc.inHeredoc {
		return "\n"
	}
	if d := sc.depth(); d > 0 {
		top := sc.delims[d-1]
		if top.kind == histDelimParen && strings.HasSuffix(entry, "\n") {
			return ""
		}
		return "\n"
	}
	if sc.compAssign > 0 {
		return " "
	}
	if sc.lastToken == htRParen {
		switch {
		case sc.twoAgo == htLParen:
			// f() { body...
			return " "
		case sc.caseDepth > 0:
			// case pattern list terminator
			return " "
		case historyNextLineNoSemiPredecessor(line):
			return "\n"
		default:
			return "; "
		}
	}
	if sc.lastToken == htWord && sc.twoAgo == htFunction {
		// function name without ()
		return " "
	}
	if sc.lastToken == htNewline && strings.Contains(line, "<<") {
		// A continuation line that opens a heredoc keeps its newline so the
		// body delimiters do not end up semicolon-separated.
		return "\n"
	}
	if sc.lastToken == htWord && sc.twoAgo == htFor {
		if historyNextLineStartsWithIn(line) {
			return " "
		}
		return ";"
	}
	if sc.twoAgo == htCase && sc.lastToken == htWord && sc.caseDepth > 0 {
		return " "
	}
	if historyNoSemiSuccessor(sc.lastToken) {
		return " "
	}
	return "; "
}

func historyNoSemiSuccessor(tok historyTokenKind) bool {
	switch tok {
	case htNewline, htLBrace, htLParen, htRParen, htSemi, htAmp, htPipe,
		htDblSemi, htSemiAmp, htSemiSemiAmp, htAndAnd, htOrOr,
		htCase, htDo, htElse, htIf, htThen, htUntil, htWhile, htIn:
		return true
	}
	return false
}

func historyNextLineNoSemiPredecessor(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '&', '|', ';':
		return true
	}
	return false
}

// historyNextLineStartsWithIn mirrors Bash's lookahead, which only compares
// the first two characters after leading whitespace.
func historyNextLineStartsWithIn(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return len(trimmed) >= 2 && trimmed[0] == 'i' && trimmed[1] == 'n'
}

func endsWithSingleBackslash(s string) bool {
	n := len(s)
	if n == 0 || s[n-1] != '\\' {
		return false
	}
	return n < 2 || s[n-2] != '\\'
}

func (s *historyScanState) scanLine(line string) {
	if s.inHeredoc {
		return
	}
	s.hasComment = false
	s.cmdStart = true
	i, n := 0, len(line)
	for i < n {
		if s.inWord {
			i = s.consumeWord(line, i, -1)
			continue
		}
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '#':
			s.hasComment = true
			return
		case c == '[' && i+1 < n && line[i+1] == '[' && s.cmdStart:
			// [[ conditional command: newlines are preserved inside.
			s.delims = append(s.delims, historyDelim{kind: histDelimCond})
			s.inWord = true
			i += 2
		default:
			if tok, size := historyOperator(line[i:]); size > 0 {
				switch tok {
				case htLParen:
					if s.compAssign > 0 || i > 0 && line[i-1] == '=' {
						s.compAssign++
					}
				case htRParen:
					if s.compAssign > 0 {
						s.compAssign--
					}
				}
				s.emit(tok)
				i += size
				continue
			}
			start := i
			s.inWord = true
			i = s.consumeWord(line, i, start)
		}
	}
}

// consumeWord advances through one shell word, tracking quoted and
// substitution delimiters so the word can span physical lines. It emits the
// word when it terminates at a blank, an operator, or end of line.
func (s *historyScanState) consumeWord(line string, i, start int) int {
	n := len(line)
	for i < n {
		if len(s.delims) > 0 {
			top := &s.delims[len(s.delims)-1]
			switch top.kind {
			case histDelimSingle:
				if line[i] == '\'' {
					s.delims = s.delims[:len(s.delims)-1]
				}
				i++
			case histDelimDouble, histDelimBacktick:
				c := line[i]
				switch {
				case c == '\\':
					i++
					if i < n {
						i++
					}
				case c == top.close:
					s.delims = s.delims[:len(s.delims)-1]
					i++
				case c == '`' && top.kind == histDelimDouble:
					s.delims = append(s.delims, historyDelim{kind: histDelimBacktick, close: '`'})
					i++
				default:
					if size := s.pushNestedDelim(line, i); size > 0 {
						i += size
					} else {
						i++
					}
				}
			case histDelimParen:
				c := line[i]
				switch {
				case c == '\\':
					i++
					if i < n {
						i++
					}
				case c == '\'':
					s.delims = append(s.delims, historyDelim{kind: histDelimSingle, close: '\''})
					i++
				case c == '"':
					s.delims = append(s.delims, historyDelim{kind: histDelimDouble, close: '"'})
					i++
				case c == '`':
					s.delims = append(s.delims, historyDelim{kind: histDelimBacktick, close: '`'})
					i++
				case c == '(':
					s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
					i++
				case c == ')':
					s.delims = s.delims[:len(s.delims)-1]
					i++
				default:
					if size := s.pushNestedDelim(line, i); size > 0 {
						i += size
					} else {
						i++
					}
				}
			case histDelimCond:
				c := line[i]
				switch {
				case c == '\\':
					i++
					if i < n {
						i++
					}
				case c == '\'':
					s.delims = append(s.delims, historyDelim{kind: histDelimSingle, close: '\''})
					i++
				case c == '"':
					s.delims = append(s.delims, historyDelim{kind: histDelimDouble, close: '"'})
					i++
				case c == '`':
					s.delims = append(s.delims, historyDelim{kind: histDelimBacktick, close: '`'})
					i++
				case c == ']' && i+1 < n && line[i+1] == ']':
					s.delims = s.delims[:len(s.delims)-1]
					i += 2
				default:
					if size := s.pushNestedDelim(line, i); size > 0 {
						i += size
					} else {
						i++
					}
				}
			}
			continue
		}

		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			s.finishWordAt(line, start, i)
			return i
		case isHistoryOperatorByte(c):
			s.finishWordAt(line, start, i)
			return i
		case c == '\\':
			i++
			if i < n {
				i++
			}
		case c == '\'':
			s.delims = append(s.delims, historyDelim{kind: histDelimSingle, close: '\''})
			i++
		case c == '"':
			s.delims = append(s.delims, historyDelim{kind: histDelimDouble, close: '"'})
			i++
		case c == '`':
			s.delims = append(s.delims, historyDelim{kind: histDelimBacktick, close: '`'})
			i++
		case c == '#':
			s.hasComment = true
			i++
		case c == '$' && i+1 < n:
			switch line[i+1] {
			case '(':
				s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
				i += 2
				if i < n && line[i] == '(' {
					s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
					i++
				}
			case '\'':
				s.delims = append(s.delims, historyDelim{kind: histDelimSingle, close: '\''})
				i += 2
			case '"':
				s.delims = append(s.delims, historyDelim{kind: histDelimDouble, close: '"'})
				i += 2
			case '{':
				i = skipHistoryBalanced(line, i, '{', '}')
			case '[':
				i = skipHistoryBalanced(line, i, '[', ']')
			default:
				i++
			}
		case (c == '<' || c == '>') && i+1 < n && line[i+1] == '(':
			s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
			i += 2
		case c == '<' && i+1 < n && line[i+1] == '<':
			i = s.scanHeredocOp(line, i)
		default:
			i++
		}
	}
	if s.depth() == 0 {
		s.finishWordAt(line, start, i)
	}
	return i
}

func (s *historyScanState) finishWordAt(line string, start, end int) {
	word := ""
	cross := start < 0
	if !cross {
		word = line[start:end]
	}
	s.finishWord(word, cross)
}

func (s *historyScanState) finishWord(word string, crossLine bool) {
	s.inWord = false
	if crossLine || word == "" {
		s.emit(htWord)
		return
	}
	if word == "in" && ((s.lastToken == htWord &&
		(s.twoAgo == htFor || s.twoAgo == htCase || s.twoAgo == htSelect)) || s.caseDepth > 0) {
		s.emit(htIn)
		return
	}
	if s.cmdStart {
		s.emit(historyKeyword(word))
		return
	}
	s.emit(htWord)
}

func (s *historyScanState) emit(tok historyTokenKind) {
	switch tok {
	case htCase:
		s.caseDepth++
	case htEsac:
		if s.caseDepth > 0 {
			s.caseDepth--
		}
	}
	s.twoAgo = s.lastToken
	s.lastToken = tok
	s.cmdStart = historyTokenStartsCommand(tok)
}

func historyTokenStartsCommand(tok historyTokenKind) bool {
	switch tok {
	case htWord, htIf, htElif, htFor, htIn, htWhile, htUntil, htCase, htFunction, htSelect:
		return false
	}
	return true
}

func historyKeyword(word string) historyTokenKind {
	switch word {
	case "if":
		return htIf
	case "then":
		return htThen
	case "elif":
		return htElif
	case "else":
		return htElse
	case "fi":
		return htFi
	case "for":
		return htFor
	case "while":
		return htWhile
	case "until":
		return htUntil
	case "do":
		return htDo
	case "done":
		return htDone
	case "case":
		return htCase
	case "esac":
		return htEsac
	case "function":
		return htFunction
	case "select":
		return htSelect
	}
	return htWord
}

func historyOperator(s string) (historyTokenKind, int) {
	switch s[0] {
	case ';':
		if len(s) > 1 && s[1] == ';' {
			if len(s) > 2 && s[2] == '&' {
				return htSemiSemiAmp, 3
			}
			return htDblSemi, 2
		}
		if len(s) > 1 && s[1] == '&' {
			return htSemiAmp, 2
		}
		return htSemi, 1
	case '&':
		if len(s) > 1 && s[1] == '&' {
			return htAndAnd, 2
		}
		return htAmp, 1
	case '|':
		if len(s) > 1 && s[1] == '|' {
			return htOrOr, 2
		}
		if len(s) > 1 && s[1] == '&' {
			return htPipeAmp, 2
		}
		return htPipe, 1
	case '(':
		return htLParen, 1
	case ')':
		return htRParen, 1
	case '{':
		return htLBrace, 1
	case '}':
		return htRBrace, 1
	}
	return htNone, 0
}

func isHistoryOperatorByte(c byte) bool {
	switch c {
	case ';', '&', '|', '(', ')', '{', '}':
		return true
	}
	return false
}

// pushNestedDelim handles the expansions that can open a delimiter while
// another delimiter is already open: $(, $((, $', $", <(, and >(.
func (s *historyScanState) pushNestedDelim(line string, i int) int {
	if i+1 >= len(line) {
		return 0
	}
	if line[i] == '$' {
		switch line[i+1] {
		case '(':
			s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
			size := 2
			if i+2 < len(line) && line[i+2] == '(' {
				s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
				size = 3
			}
			return size
		case '\'':
			s.delims = append(s.delims, historyDelim{kind: histDelimSingle, close: '\''})
			return 2
		case '"':
			s.delims = append(s.delims, historyDelim{kind: histDelimDouble, close: '"'})
			return 2
		}
		return 0
	}
	if (line[i] == '<' || line[i] == '>') && line[i+1] == '(' {
		s.delims = append(s.delims, historyDelim{kind: histDelimParen, close: ')'})
		return 2
	}
	return 0
}

// scanHeredocOp consumes a << or <<- redirection and records the delimiter
// word that terminates the heredoc body.
func (s *historyScanState) scanHeredocOp(line string, i int) int {
	n := len(line)
	s.heredocTab = false
	if i+2 < n && line[i+2] == '<' {
		// <<< here-string: not a heredoc.
		return i + 3
	}
	j := i + 2
	if j < n && line[j] == '-' {
		s.heredocTab = true
		j++
	}
	for j < n && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j >= n {
		return j
	}
	if line[j] == '\'' || line[j] == '"' {
		close := line[j]
		k := j + 1
		for k < n && line[k] != close {
			if close == '"' && line[k] == '\\' && k+1 < n {
				k += 2
				continue
			}
			k++
		}
		s.heredocTerm = line[j+1 : k]
		s.inHeredoc = true
		s.heredocFirst = true
		if k < n {
			k++
		}
		return k
	}
	k := j
	for k < n && !isHistoryWordEndByte(line[k]) {
		k++
	}
	term := line[j:k]
	var unquoted strings.Builder
	skip := false
	for _, r := range term {
		if skip {
			unquoted.WriteRune(r)
			skip = false
			continue
		}
		if r == '\\' {
			skip = true
			continue
		}
		unquoted.WriteRune(r)
	}
	s.heredocTerm = unquoted.String()
	s.inHeredoc = true
	s.heredocFirst = true
	return k
}

func isHistoryWordEndByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || isHistoryOperatorByte(c) || c == '#' ||
		c == '<' || c == '>' || c == '\n'
}

// skipHistoryBalanced consumes ${...} or $[...] starting at the '$' at i.
// Neither construct opens a Bash history delimiter, so an unclosed one ends
// the word at the line boundary.
func skipHistoryBalanced(line string, i int, open, close byte) int {
	depth := 1
	j := i + 2
	n := len(line)
	quote := byte(0)
	for j < n {
		c := line[j]
		if quote != 0 {
			if c == '\\' && quote == '"' && j+1 < n {
				j += 2
				continue
			}
			if c == quote {
				quote = 0
			}
			j++
			continue
		}
		switch c {
		case '\\':
			j += 2
			continue
		case '\'', '"':
			quote = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j + 1
			}
		}
		j++
	}
	return n
}
