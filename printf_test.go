package gosh

import (
	"testing"
	"time"
)

// TestDifferentialPrintf compares shellPrintf with Bash's printf builtin,
// output and status, in a UTF-8 locale and the local time zone.
func TestDifferentialPrintf(t *testing.T) {
	requireBash(t)
	for _, args := range [][]string{
		{`%s|%5s|%-5s|%.2s|%5.1s|\n`, "abc", "ab", "cd", "efg", "xyz"},
		{`%d %i %+d % d %05d %-5d| %.3d %x %X %#x %o %#o %u\n`, "42", "-7", "5", "5", "-42", "3", "7", "255", "255", "255", "8", "8", "-1"},
		{`%d %d %d %d %d\n`, "0x1f", "017", "'A", `"é`, "  12"},
		{`%d|%d|%d\n`, "12abc", "", "x"},
		{`%f %.2f %10.3f %-10.1f| %+f %e %E %.0e\n`, "3.14159", "2.675", "-1.5", "2.25", "1", "12345.678", "0.000123", "5e10"},
		{`%g %g %g %g %G %.3g %#g %g\n`, "0.0001", "0.00001", "123456", "1234567", "1e-10", "3.14159", "1.5", "100"},
		{`%f %e %g %F %5.1f\n`, "inf", "-inf", "nan", "inf", "inf"},
		{`%a %A %.2a\n`, "1.5", "-2", "1"},
		{`%*d|%-*d|%.*f|%*s|\n`, "5", "1", "4", "2", "2", "3.14159", "-4", "x"},
		{`%c|%c|%5c|%c\n`, "hello", "", "x", "é"},
		{`%b|%b|%b|%b\n`, `a\tb\n`, `\0101\101`, `\x41\u00e9`, `q\'\"`},
		{`%b|%s\n`, `x\cy`, "never"},
		{`a\tb\x41\101\0101\u00e9\e[0m\c\n`},
		{`%q\n`, "a b", "", "a'b", "a\nb", "~x", "x~", "a=~", "#x", "x#", "a=b", "a,b", "{a}", "*?[]", "é", "\x01", "-x", "!", "%", "a\x1bb", "\xff"},
		{`%5q|%-8q|\n`, "a b", "c"},
		{`%s %s\n`, "a", "b", "c"},
		{`no conversions\n`, "extra"},
		{`%%|%5%|\n`},
		{`%(%Y-%m-%d %H:%M:%S)T|%(%s)T|%20(%j)T|\n`, "0", "86400", "1000000000"},
		{`%z`},
		{`abc%`},
		{`%k\n`, "1"},
		{`%s %d\n`, "a", "b", "c"},
		{`%ld %hhd %lld %Lf\n`, "1", "2", "3", "4.5"},
		{`%'d\n`, "1234567"},
	} {
		t.Run(args[0], func(t *testing.T) {
			script := `printf "$@"; echo "|rc=$?"`
			want := runDifferentialShell(t, "bash", append([]string{"--norc", "-c", script, "sh"}, args...), nil, []string{"LC_ALL=C.UTF-8"}, "")
			out, errs, fatal := shellPrintf(args[0], args[1:], time.Now())
			got := out
			code := 0
			if len(errs) > 0 || fatal != nil {
				code = 1
			}
			got += "|rc=" + itoa(code) + "\n"
			if got != want.stdout {
				t.Fatalf("printf %q:\nbash: %q (stderr %q)\ngosh: %q (errs %q, fatal %v)", args, want.stdout, want.stderr, got, errs, fatal)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}
