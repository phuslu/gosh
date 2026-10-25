package gosh

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// shellPrintf formats args with format like Bash's printf builtin. The
// format is reused while arguments are left, missing arguments count as
// empty strings or zeros, and an argument which is not a valid number is
// reported in errs while its leading digits are used, as in Bash. A fatal
// error, such as an invalid conversion, ends the output early. start is
// when the shell started, for %(fmt)T with -2.
func shellPrintf(format string, args []string, start time.Time) (out string, errs []string, fatal error) {
	p := &printfState{args: args, start: start}
	for {
		consumed := len(p.args)
		if stop, err := p.formatOnce(format); err != nil || stop {
			return p.out.String(), p.errs, err
		}
		consumed -= len(p.args)
		if consumed == 0 || len(p.args) == 0 {
			return p.out.String(), p.errs, nil
		}
	}
}

type printfState struct {
	out   strings.Builder
	args  []string
	errs  []string
	start time.Time
}

func (p *printfState) next() (string, bool) {
	if len(p.args) == 0 {
		return "", false
	}
	arg := p.args[0]
	p.args = p.args[1:]
	return arg, true
}

// formatOnce writes format once and reports whether a \c in a %b argument
// stopped all output.
func (p *printfState) formatOnce(format string) (bool, error) {
	for i := 0; i < len(format); i++ {
		c := format[i]
		switch c {
		case '\\':
			n, stop := writeEscape(&p.out, format[i+1:], false)
			if stop {
				return true, nil
			}
			i += n
		case '%':
			n, stop, err := p.conversion(format[i:])
			if err != nil || stop {
				return stop, err
			}
			i += n - 1
		default:
			p.out.WriteByte(c)
		}
	}
	return false, nil
}

// conversion handles the conversion specification at the start of spec and
// returns its length.
func (p *printfState) conversion(spec string) (int, bool, error) {
	i := 1
	if i < len(spec) && spec[i] == '%' {
		p.out.WriteByte('%')
		return 2, false, nil
	}
	var flags strings.Builder
	for i < len(spec) && strings.IndexByte("-+ #0'", spec[i]) >= 0 {
		if spec[i] != '\'' {
			flags.WriteByte(spec[i])
		}
		i++
	}
	width, hasWidth := 0, false
	if i < len(spec) && spec[i] == '*' {
		arg, _ := p.next()
		width, hasWidth = int(p.integer(arg)), true
		if width < 0 {
			flags.WriteByte('-')
			width = -width
		}
		i++
	} else {
		j := i
		for i < len(spec) && spec[i] >= '0' && spec[i] <= '9' {
			i++
		}
		if i > j {
			width, _ = strconv.Atoi(spec[j:i])
			hasWidth = true
		}
	}
	precision, hasPrecision := 0, false
	if i < len(spec) && spec[i] == '.' {
		i++
		hasPrecision = true
		if i < len(spec) && spec[i] == '*' {
			arg, _ := p.next()
			precision = int(p.integer(arg))
			if precision < 0 {
				hasPrecision = false
			}
			i++
		} else {
			j := i
			for i < len(spec) && spec[i] >= '0' && spec[i] <= '9' {
				i++
			}
			precision, _ = strconv.Atoi(spec[j:i])
		}
	}
	// Length modifiers mean nothing to the shell.
	for i < len(spec) && strings.IndexByte("hlLjzt", spec[i]) >= 0 {
		i++
	}
	timeFormat := ""
	if i < len(spec) && spec[i] == '(' {
		end := strings.IndexByte(spec[i:], ')')
		if end < 0 {
			return 0, false, fmt.Errorf("`%s': missing format character", spec)
		}
		timeFormat = spec[i+1 : i+end]
		i += end + 1
		if i >= len(spec) || spec[i] != 'T' {
			return 0, false, fmt.Errorf("`%s': missing format character", spec[:i])
		}
	}
	if i >= len(spec) {
		return 0, false, fmt.Errorf("`%s': missing format character", spec)
	}
	verb := spec[i]
	i++
	// pad formats s, a string, with the flags, width and precision.
	pad := func(s string, usePrecision bool) {
		if usePrecision && hasPrecision && precision < len(s) {
			s = s[:precision]
		}
		if hasWidth && len(s) < width {
			fill := strings.Repeat(" ", width-len(s))
			if strings.Contains(flags.String(), "-") {
				s += fill
			} else {
				s = fill + s
			}
		}
		p.out.WriteString(s)
	}
	goFormat := func(verb byte) string {
		var f strings.Builder
		f.WriteByte('%')
		f.WriteString(flags.String())
		if hasWidth {
			f.WriteString(strconv.Itoa(width))
		}
		if hasPrecision {
			f.WriteByte('.')
			f.WriteString(strconv.Itoa(precision))
		}
		f.WriteByte(verb)
		return f.String()
	}
	switch verb {
	case 's':
		arg, _ := p.next()
		pad(arg, true)
	case 'b':
		arg, _ := p.next()
		var b strings.Builder
		stop := false
		for k := 0; k < len(arg); k++ {
			if arg[k] != '\\' {
				b.WriteByte(arg[k])
				continue
			}
			n, s := writeEscape(&b, arg[k+1:], true)
			if s {
				stop = true
				break
			}
			k += n
		}
		pad(b.String(), true)
		return i, stop, nil
	case 'q', 'Q':
		arg, _ := p.next()
		if verb == 'Q' && hasPrecision && precision < len(arg) {
			arg = arg[:precision]
		}
		pad(bashQuote(arg), verb == 'q')
	case 'c':
		arg, _ := p.next()
		if arg == "" {
			// Like Bash, an empty argument is a NUL byte.
			arg = "\x00"
		}
		pad(arg[:1], false)
	case 'd', 'i':
		arg, _ := p.next()
		fmt.Fprintf(&p.out, goFormat('d'), p.integer(arg))
	case 'u', 'o', 'x', 'X':
		arg, _ := p.next()
		goVerb := verb
		if verb == 'u' {
			goVerb = 'd'
		}
		fmt.Fprintf(&p.out, goFormat(goVerb), uint64(p.integer(arg)))
	case 'f', 'F', 'e', 'E', 'g', 'G', 'a', 'A':
		arg, _ := p.next()
		p.float(p.number(arg), verb, flags.String(), width, hasWidth, precision, hasPrecision)
	case 'T':
		arg, ok := p.next()
		t := time.Now()
		if ok && arg != "" {
			switch n := p.integer(arg); n {
			case -1:
			case -2:
				t = p.start
			default:
				t = time.Unix(n, 0)
			}
		}
		if timeFormat == "" {
			timeFormat = "%X"
		}
		pad(strftime(timeFormat, t), true)
	default:
		return 0, false, fmt.Errorf("`%c': invalid format character", verb)
	}
	return i, false, nil
}

// float writes f for a floating-point conversion the way C's printf does.
func (p *printfState) float(f float64, verb byte, flags string, width int, hasWidth bool, precision int, hasPrecision bool) {
	lower := verb >= 'a'
	var s string
	switch {
	case math.IsInf(f, 0) || math.IsNaN(f):
		s = "inf"
		if math.IsNaN(f) {
			s = "nan"
		}
		if !lower {
			s = strings.ToUpper(s)
		}
		switch {
		case f < 0 || math.Signbit(f) && math.IsNaN(f):
			s = "-" + s
		case strings.Contains(flags, "+"):
			s = "+" + s
		case strings.Contains(flags, " "):
			s = " " + s
		}
		flags = strings.ReplaceAll(flags, "0", "")
	case verb == 'a' || verb == 'A':
		prec := -1
		if hasPrecision {
			prec = precision
		}
		s = strconv.FormatFloat(f, 'x', prec, 64)
		// C writes the exponent without leading zeros.
		if k := strings.LastIndexAny(s, "p"); k >= 0 && k+2 < len(s) {
			exp := strings.TrimLeft(s[k+2:], "0")
			if exp == "" {
				exp = "0"
			}
			s = s[:k+2] + exp
		}
		if !lower {
			s = strings.ToUpper(s)
		}
		switch {
		case f >= 0 && strings.Contains(flags, "+"):
			s = "+" + s
		case f >= 0 && strings.Contains(flags, " "):
			s = " " + s
		}
	default:
		if !hasPrecision {
			precision = 6
		}
		goVerb := verb
		if verb == 'F' {
			goVerb = 'f'
		}
		s = fmt.Sprintf("%"+flags+"."+strconv.Itoa(precision)+string(goVerb), f)
		if verb == 'g' || verb == 'G' {
			// Go's %g switches to exponents at a different point than C's.
			s = cFormatG(f, precision, flags, verb)
		}
	}
	if hasWidth && len(s) < width {
		switch {
		case strings.Contains(flags, "-"):
			s += strings.Repeat(" ", width-len(s))
		case strings.Contains(flags, "0"):
			sign := ""
			if s != "" && strings.IndexByte("+- ", s[0]) >= 0 {
				sign, s = s[:1], s[1:]
			}
			s = sign + strings.Repeat("0", width-len(s)-len(sign)) + s
		default:
			s = strings.Repeat(" ", width-len(s)) + s
		}
	}
	p.out.WriteString(s)
}

// cFormatG formats f like C's %g: %e when the exponent is below -4 or not
// below the precision, %f otherwise, without trailing zeros unless the #
// flag is given.
func cFormatG(f float64, precision int, flags string, verb byte) string {
	if precision == 0 {
		precision = 1
	}
	exp := 0
	if f != 0 {
		// The exponent after rounding to precision digits.
		e := strconv.FormatFloat(f, 'e', precision-1, 64)
		exp, _ = strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	}
	var s string
	if exp < -4 || exp >= precision {
		e := byte('e')
		if verb == 'G' {
			e = 'E'
		}
		s = fmt.Sprintf("%"+flags+"."+strconv.Itoa(precision-1)+string(e), f)
	} else {
		s = fmt.Sprintf("%"+flags+"."+strconv.Itoa(precision-1-exp)+"f", f)
	}
	if !strings.Contains(flags, "#") {
		mant, expPart := s, ""
		if k := strings.IndexAny(s, "eE"); k >= 0 {
			mant, expPart = s[:k], s[k:]
		}
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		s = mant + expPart
	}
	return s
}

// integer parses a numeric printf argument like Bash: decimal, octal with
// a leading 0, hexadecimal with 0x, or the character code after a leading
// quote. An invalid number is reported and its valid prefix used.
func (p *printfState) integer(arg string) int64 {
	if n, ok := quotedCharCode(arg); ok {
		return n
	}
	s := strings.TrimLeft(arg, " \t\n")
	neg := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	base := 10
	switch {
	case len(s) > 1 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X'):
		base, s = 16, s[2:]
	case len(s) > 1 && s[0] == '0':
		base = 8
	}
	end := 0
	for end < len(s) && digitValue(s[end]) < base {
		end++
	}
	if end == 0 || end < len(s) {
		p.errs = append(p.errs, arg+": invalid number")
	}
	n, err := strconv.ParseUint(s[:end], base, 64)
	if err != nil && end > 0 {
		p.errs = append(p.errs, arg+": Numerical result out of range")
		n = math.MaxInt64
	}
	if neg {
		return -int64(n)
	}
	return int64(n)
}

// number parses a floating-point printf argument.
func (p *printfState) number(arg string) float64 {
	if n, ok := quotedCharCode(arg); ok {
		return float64(n)
	}
	s := strings.TrimLeft(arg, " \t\n")
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	// The longest valid prefix, as strtod would take.
	for end := len(s) - 1; end > 0; end-- {
		if f, err := strconv.ParseFloat(s[:end], 64); err == nil {
			p.errs = append(p.errs, arg+": invalid number")
			return f
		}
	}
	p.errs = append(p.errs, arg+": invalid number")
	return 0
}

// quotedCharCode returns the character code of the character after a
// leading single or double quote.
func quotedCharCode(arg string) (int64, bool) {
	if len(arg) < 2 || (arg[0] != '\'' && arg[0] != '"') {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(arg[1:])
	return int64(r), true
}

func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 99
}

// writeEscape writes the backslash escape at the start of s, which follows
// the backslash, and returns how many bytes of s it used. In %b arguments
// \0nnn is octal as well as \nnn, and \c stops all output, which it
// reports.
func writeEscape(b *strings.Builder, s string, inArg bool) (int, bool) {
	if s == "" {
		b.WriteByte('\\')
		return 0, false
	}
	digits := func(from, max, base int) (int, int) {
		n, k := 0, from
		for k < len(s) && k-from < max && digitValue(s[k]) < base {
			n = n*base + digitValue(s[k])
			k++
		}
		return n, k
	}
	switch c := s[0]; c {
	case 'a':
		b.WriteByte('\a')
	case 'b':
		b.WriteByte('\b')
	case 'e', 'E':
		b.WriteByte('\x1b')
	case 'f':
		b.WriteByte('\f')
	case 'n':
		b.WriteByte('\n')
	case 'r':
		b.WriteByte('\r')
	case 't':
		b.WriteByte('\t')
	case 'v':
		b.WriteByte('\v')
	case '\\':
		b.WriteByte('\\')
	case '\'', '"', '?':
		if inArg {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	case 'c':
		if inArg {
			return 1, true
		}
		b.WriteString(`\c`)
	case 'x':
		n, k := digits(1, 2, 16)
		if k == 1 {
			b.WriteString(`\x`)
			return 1, false
		}
		b.WriteByte(byte(n))
		return k, false
	case 'u', 'U':
		max := 4
		if c == 'U' {
			max = 8
		}
		n, k := digits(1, max, 16)
		if k == 1 {
			b.WriteByte('\\')
			b.WriteByte(c)
			return 1, false
		}
		b.WriteRune(rune(n))
		return k, false
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// In %b arguments, \0nnn takes up to three digits after the 0.
		from := 0
		if inArg && c == '0' {
			from = 1
		}
		n, k := digits(from, 3, 8)
		b.WriteByte(byte(n))
		return k, false
	default:
		b.WriteByte('\\')
		b.WriteByte(c)
	}
	return 1, false
}

// bashQuote quotes s for reuse as shell input like Bash's printf %q: with
// backslashes, as ” when empty, or as $'...' when it holds characters
// which are not printable.
func bashQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if r == utf8.RuneError || r < ' ' || r == 0x7f {
			return ansiCQuote(s)
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\'', '"', '\\', '|', '&', ';', '(', ')', '<', '>',
			'!', '{', '}', '*', '[', '?', ']', '^', '$', '`', ',':
			b.WriteByte('\\')
		case '~':
			if i == 0 || s[i-1] == '=' || s[i-1] == ':' {
				b.WriteByte('\\')
			}
		case '#':
			if i == 0 {
				b.WriteByte('\\')
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ansiCQuote quotes s as $'...', like Bash's ansic_quote.
func ansiCQuote(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\a':
			b.WriteString(`\a`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == 0x1b:
			b.WriteString(`\E`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\v':
			b.WriteString(`\v`)
		case r == '\\' || r == '\'':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == utf8.RuneError && size == 1, r < ' ', r == 0x7f:
			fmt.Fprintf(&b, `\%03o`, s[i])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('\'')
	return b.String()
}
