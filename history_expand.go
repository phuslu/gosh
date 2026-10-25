package gosh

import (
	"fmt"
	"strconv"
	"strings"
)

// historyExpander implements Bash's history expansion of interactive input
// lines: event designators such as !!, !n, !-n, !string and !?string?, word
// designators such as :0, :n, :^, :$, :*, :x-y and :%, the modifiers :h, :t,
// :r, :e, :p, :q, :x, :s/old/new/, :gs/old/new/ and :&, and the ^old^new^
// quick substitution.
type historyExpander struct {
	// subOld and subNew are the last substitution, which :& and an empty
	// old string reuse; search is the last !?string? searched for.
	subOld, subNew string
	search         string
}

// historyExpandError is the message Bash prints for a failed expansion,
// without the shell name.
type historyExpandError string

func (e historyExpandError) Error() string { return string(e) }

// expand expands line against entries, the history before it. It reports
// whether anything was expanded and whether a :p modifier asked to print
// the result instead of running it.
func (x *historyExpander) expand(line string, entries []string) (out string, changed, printOnly bool, err error) {
	if strings.HasPrefix(line, "^") {
		// ^old^new^ is !!:s^old^new^.
		line = "!!:s" + line
	}
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(line):
			b.WriteByte(c)
			i++
			b.WriteByte(line[i])
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		}
		if c != '!' || inSingle || x.inhibited(line, i, inDouble) {
			b.WriteByte(c)
			continue
		}
		text, end, print, err := x.expandOne(line, i, entries)
		if err != nil {
			return "", false, false, err
		}
		b.WriteString(text)
		changed = true
		printOnly = printOnly || print
		i = end - 1
	}
	return b.String(), changed, printOnly, nil
}

// inhibited reports whether the "!" at line[i] starts no history expansion:
// at the end of the line, before a blank, "=" or "(", or right before a
// closing double quote, and in $!, ${!name} and [!...] bracket expressions.
func (x *historyExpander) inhibited(line string, i int, inDouble bool) bool {
	if i+1 >= len(line) {
		return true
	}
	switch line[i+1] {
	case ' ', '\t', '\n', '\r', '=', '(':
		return true
	case '"':
		return inDouble
	}
	if i > 0 && (line[i-1] == '$' || line[i-1] == '[') {
		return true
	}
	return i > 1 && line[i-2:i] == "${"
}

// expandOne expands the history reference starting at the "!" at
// line[start] and returns its text and where it ends.
func (x *historyExpander) expandOne(line string, start int, entries []string) (string, int, bool, error) {
	i := start + 1
	event, i, err := x.event(line, i, entries)
	if err != nil {
		return "", 0, false, err
	}
	words := historyWords(event)
	text := event
	// A word designator follows a colon, or directly one of ^$*-%.
	if i < len(line) && (line[i] == ':' && i+1 < len(line) && isWordDesignatorStart(line[i+1]) || strings.IndexByte("^$*-%", line[i]) >= 0) {
		if line[i] == ':' {
			i++
		}
		var spec string
		spec, i = scanWordDesignator(line, i)
		if text, err = x.selectWords(words, spec); err != nil {
			return "", 0, false, err
		}
	}
	printOnly := false
	for i+1 < len(line) && line[i] == ':' {
		var err error
		var print bool
		text, i, print, err = x.modify(text, line, i+1)
		if err != nil {
			return "", 0, false, err
		}
		printOnly = printOnly || print
	}
	return text, i, printOnly, nil
}

// event resolves the event designator at line[i:].
func (x *historyExpander) event(line string, i int, entries []string) (string, int, error) {
	find := func(match func(string) bool, spec string) (string, error) {
		for k := len(entries) - 1; k >= 0; k-- {
			if match(entries[k]) {
				return entries[k], nil
			}
		}
		return "", historyExpandError(spec + ": event not found")
	}
	switch c := line[i]; {
	case c == '!':
		return x.byOffset(entries, -1, "!!", i+1)
	case strings.IndexByte("^$*%:", c) >= 0:
		// A word designator alone refers to the previous command.
		return x.byOffset(entries, -1, "!"+string(c), i)
	case c == '-' || c >= '0' && c <= '9':
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(line[i:j])
		if err != nil {
			return "", 0, historyExpandError("!" + line[i:j] + ": event not found")
		}
		return x.byOffset(entries, n, "!"+line[i:j], j)
	case c == '?':
		j := strings.IndexByte(line[i+1:], '?')
		end := len(line)
		if j >= 0 {
			end = i + 1 + j
		}
		search := line[i+1 : end]
		if search == "" {
			search = x.search
		}
		x.search = search
		if j >= 0 {
			end++
		}
		event, err := find(func(e string) bool { return search != "" && strings.Contains(e, search) }, "!?"+line[i+1:min(end, len(line))])
		return event, end, err
	case c == '#':
		return line[:i-1], i + 1, nil
	default:
		j := i
		for j < len(line) && !strings.ContainsRune(" \t\n:;&|<>()\"'`", rune(line[j])) {
			j++
		}
		prefix := line[i:j]
		event, err := find(func(e string) bool { return strings.HasPrefix(e, prefix) }, "!"+prefix)
		return event, j, err
	}
}

// byOffset returns entry n of entries, 1-based from the start or, when n
// is negative, counted back from the end.
func (x *historyExpander) byOffset(entries []string, n int, spec string, end int) (string, int, error) {
	k := n - 1
	if n < 0 {
		k = len(entries) + n
	}
	if k < 0 || k >= len(entries) {
		return "", 0, historyExpandError(spec + ": event not found")
	}
	return entries[k], end, nil
}

func isWordDesignatorStart(c byte) bool {
	return c >= '0' && c <= '9' || strings.IndexByte("^$*-%", c) >= 0
}

// scanWordDesignator returns the word designator at line[i:] and where it
// ends: a number, ^, $, % or *, or a range x-y, x*, x- or -y.
func scanWordDesignator(line string, i int) (string, int) {
	start := i
	word := func() {
		if i < len(line) && (line[i] == '^' || line[i] == '$' || line[i] == '%') {
			i++
			return
		}
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
	}
	if i < len(line) && line[i] == '*' {
		return "*", i + 1
	}
	word()
	if i < len(line) && line[i] == '*' {
		return line[start : i+1], i + 1
	}
	if i < len(line) && line[i] == '-' {
		i++
		word()
	}
	return line[start:i], i
}

// selectWords applies a word designator to the words of an event.
func (x *historyExpander) selectWords(words []string, spec string) (string, error) {
	bad := historyExpandError(":" + spec + ": bad word specifier")
	last := len(words) - 1
	index := func(s string) (int, bool) {
		switch s {
		case "^":
			return 1, true
		case "$":
			return last, true
		case "%":
			for k, w := range words {
				if x.search != "" && strings.Contains(w, x.search) {
					return k, true
				}
			}
			return 0, false
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	var from, to int
	switch {
	case spec == "*":
		if last < 1 {
			return "", nil
		}
		from, to = 1, last
	case strings.HasSuffix(spec, "*"):
		n, ok := index(strings.TrimSuffix(spec, "*"))
		if !ok {
			return "", bad
		}
		if n > last {
			return "", nil
		}
		from, to = n, last
	case strings.Contains(spec[min(1, len(spec)):], "-") || strings.HasPrefix(spec, "-"):
		k := strings.IndexByte(spec[1:], '-') + 1
		if strings.HasPrefix(spec, "-") {
			k = 0
		}
		left, right := spec[:k], spec[k+1:]
		var ok bool
		if left == "" {
			from = 0
		} else if from, ok = index(left); !ok {
			return "", bad
		}
		if right == "" {
			// x- leaves out the last word.
			to = last - 1
		} else if to, ok = index(right); !ok {
			return "", bad
		}
	default:
		n, ok := index(spec)
		if !ok {
			return "", bad
		}
		from, to = n, n
	}
	if from < 0 || to > last || from > to {
		return "", bad
	}
	return strings.Join(words[from:to+1], " "), nil
}

// modify applies the modifier at line[i:] to text.
func (x *historyExpander) modify(text, line string, i int) (string, int, bool, error) {
	global := false
	if line[i] == 'g' || line[i] == 'G' {
		global = true
		i++
		if i >= len(line) {
			return "", 0, false, historyExpandError(":g: unrecognized history modifier")
		}
	}
	switch m := line[i]; m {
	case 'h':
		if k := strings.LastIndexByte(text, '/'); k >= 0 {
			text = text[:k]
			if text == "" {
				text = "/"
			}
		}
	case 't':
		if k := strings.LastIndexByte(text, '/'); k >= 0 {
			text = text[k+1:]
		}
	case 'r':
		// Like Bash, the suffix starts at the last dot, even before a slash.
		if k := strings.LastIndexByte(text, '.'); k >= 0 {
			text = text[:k]
		}
	case 'e':
		if k := strings.LastIndexByte(text, '.'); k >= 0 {
			text = text[k:]
		}
	case 'p':
		return text, i + 1, true, nil
	case 'q':
		text = shellSingleQuote(text)
	case 'x':
		words := historyWords(text)
		for k, w := range words {
			words[k] = shellSingleQuote(w)
		}
		text = strings.Join(words, " ")
	case 's', '&':
		end := i + 1
		if m == 's' {
			if end >= len(line) {
				return "", 0, false, historyExpandError(":s: substitution failed")
			}
			delim := line[end]
			old, j := scanSubstPart(line, end+1, delim)
			repl, k := scanSubstPart(line, j, delim)
			end = k
			if old == "" {
				old = x.search
				if x.subOld != "" {
					old = x.subOld
				}
			}
			x.subOld, x.subNew = old, strings.ReplaceAll(repl, "&", old)
		}
		if x.subOld == "" || !strings.Contains(text, x.subOld) {
			return "", 0, false, historyExpandError(":" + line[i:end] + ": substitution failed")
		}
		if global {
			text = strings.ReplaceAll(text, x.subOld, x.subNew)
		} else {
			text = strings.Replace(text, x.subOld, x.subNew, 1)
		}
		return text, end, false, nil
	default:
		return "", 0, false, historyExpandError(fmt.Sprintf(":%c: unrecognized history modifier", m))
	}
	return text, i + 1, false, nil
}

// scanSubstPart reads one part of a :s modifier up to delim or the end of
// the line; a backslash quotes the delimiter.
func scanSubstPart(line string, i int, delim byte) (string, int) {
	var b strings.Builder
	for ; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == delim:
			i++
			b.WriteByte(delim)
		case line[i] == delim:
			return b.String(), i + 1
		default:
			b.WriteByte(line[i])
		}
	}
	return b.String(), i
}

// historyWords splits a history entry into words the way Bash's history
// tokenizer does: at blanks outside quotes, with the shell's operator
// characters as words of their own.
func historyWords(line string) []string {
	var words []string
	i := 0
	for i < len(line) {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\n') {
			i++
		}
		if i >= len(line) {
			break
		}
		start := i
		if strings.IndexByte(";&|<>()", line[i]) >= 0 {
			for i < len(line) && strings.IndexByte(";&|<>", line[i]) >= 0 && i-start < 2 && (i == start || line[i] == line[start] || line[i] == '>' || line[i] == '&') {
				i++
			}
			if i == start {
				i++
			}
			words = append(words, line[start:i])
			continue
		}
		var quote byte
		for i < len(line) {
			c := line[i]
			if quote != 0 {
				if c == '\\' && quote == '"' && i+1 < len(line) {
					i++
				} else if c == quote {
					quote = 0
				}
				i++
				continue
			}
			if c == '\\' && i+1 < len(line) {
				i += 2
				continue
			}
			if c == '\'' || c == '"' || c == '`' {
				quote = c
				i++
				continue
			}
			if c == ' ' || c == '\t' || c == '\n' || strings.IndexByte(";&|<>()", c) >= 0 {
				break
			}
			i++
		}
		words = append(words, line[start:i])
	}
	return words
}
