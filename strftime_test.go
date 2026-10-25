package gosh

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStrftimeMatchesDate compares strftime with date(1), which uses the C
// library's strftime, for every conversion over a spread of dates.
func TestStrftimeMatchesDate(t *testing.T) {
	if _, err := exec.LookPath("date"); err != nil {
		t.Skip("date is not available")
	}
	const format = "%a|%A|%b|%h|%B|%c|%C|%d|%D|%e|%F|%g|%G|%H|%I|%j|%k|%l|%m|%M|%p|%P|%r|%R|%s|%S|%T|%u|%U|%V|%w|%W|%x|%X|%y|%Y|%z|%Z|%%|%-d|%_m|%0e|%-I"
	for _, unix := range []int64{0, 951782400, 1104537600, 1230681599, 1609459200, 1735603200, 1792950235} {
		cmd := exec.Command("date", "-u", "-d", "@"+strconv.FormatInt(unix, 10), "+"+format)
		cmd.Env = []string{"LC_ALL=C", "TZ=UTC"}
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("date does not support -d @seconds: %v", err)
		}
		want := strings.TrimSuffix(string(out), "\n")
		if got := strftime(format, time.Unix(unix, 0).UTC()); got != want {
			t.Errorf("strftime at %d:\ngot  %q\nwant %q", unix, got, want)
		}
	}
}
