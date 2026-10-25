package gosh

import (
	"strconv"
	"strings"
	"time"
)

// strftime formats t like C's strftime in the C locale, as Bash does for
// HISTTIMEFORMAT, \D{format} in prompts and printf's %(format)T. It accepts
// the GNU flags "-" (no padding), "_" (pad with spaces) and "0" (pad with
// zeros) after the percent sign. Unknown conversions are copied as is.
func strftime(format string, t time.Time) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			b.WriteByte(c)
			continue
		}
		start := i
		i++
		pad := byte(0) // 0 means the conversion's own padding
		for i < len(format) && (format[i] == '-' || format[i] == '_' || format[i] == '0') {
			pad = format[i]
			i++
		}
		if i >= len(format) {
			b.WriteString(format[start:])
			break
		}
		num := func(n, width int, fill byte) {
			switch pad {
			case '-':
				width = 0
			case '_':
				fill = ' '
			case '0':
				fill = '0'
			}
			s := strconv.Itoa(n)
			for k := len(s); k < width; k++ {
				b.WriteByte(fill)
			}
			b.WriteString(s)
		}
		hour12 := t.Hour() % 12
		if hour12 == 0 {
			hour12 = 12
		}
		switch format[i] {
		case '%':
			b.WriteByte('%')
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'c':
			b.WriteString(strftime("%a %b %e %H:%M:%S %Y", t))
		case 'C':
			num(t.Year()/100, 2, '0')
		case 'd':
			num(t.Day(), 2, '0')
		case 'D', 'x':
			b.WriteString(strftime("%m/%d/%y", t))
		case 'e':
			num(t.Day(), 2, ' ')
		case 'F':
			b.WriteString(strftime("%Y-%m-%d", t))
		case 'g':
			year, _ := t.ISOWeek()
			num(year%100, 2, '0')
		case 'G':
			year, _ := t.ISOWeek()
			num(year, 0, '0')
		case 'H':
			num(t.Hour(), 2, '0')
		case 'I':
			num(hour12, 2, '0')
		case 'j':
			num(t.YearDay(), 3, '0')
		case 'k':
			num(t.Hour(), 2, ' ')
		case 'l':
			num(hour12, 2, ' ')
		case 'm':
			num(int(t.Month()), 2, '0')
		case 'M':
			num(t.Minute(), 2, '0')
		case 'n':
			b.WriteByte('\n')
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'P':
			b.WriteString(t.Format("pm"))
		case 'r':
			b.WriteString(strftime("%I:%M:%S %p", t))
		case 'R':
			b.WriteString(strftime("%H:%M", t))
		case 's':
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'S':
			num(t.Second(), 2, '0')
		case 't':
			b.WriteByte('\t')
		case 'T', 'X':
			b.WriteString(strftime("%H:%M:%S", t))
		case 'u':
			num((int(t.Weekday())+6)%7+1, 0, '0')
		case 'U':
			num((t.YearDay()+6-int(t.Weekday()))/7, 2, '0')
		case 'V':
			_, week := t.ISOWeek()
			num(week, 2, '0')
		case 'w':
			num(int(t.Weekday()), 0, '0')
		case 'W':
			num((t.YearDay()+6-(int(t.Weekday())+6)%7)/7, 2, '0')
		case 'y':
			num(t.Year()%100, 2, '0')
		case 'Y':
			num(t.Year(), 0, '0')
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		default:
			b.WriteString(format[start : i+1])
		}
	}
	return b.String()
}
