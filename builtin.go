package gosh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// builtinHistory implements the history builtin: listing all or the last
// n entries, -c, -d offset or start-end, -a, -n, -r and -w with an optional
// file, -p and -s. It is a separate function so it can be tested without an
// interpreter runner.
func builtinHistory(h *history, args []string, stdout io.Writer) error {
	var (
		clear              bool
		deleteSpec         string
		fileOp             byte
		store, print       bool
		listCount, hasList = 0, false
	)
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		for j := 1; j < len(arg); j++ {
			switch opt := arg[j]; opt {
			case 'c':
				clear = true
			case 'd':
				if j+1 < len(arg) {
					deleteSpec = arg[j+1:]
				} else if i+1 < len(args) {
					i++
					deleteSpec = args[i]
				} else {
					return fmt.Errorf("history: -d: option requires an argument")
				}
				j = len(arg)
			case 'a', 'n', 'r', 'w':
				if fileOp != 0 && fileOp != opt {
					return fmt.Errorf("history: cannot use more than one of -anrw")
				}
				fileOp = opt
			case 's':
				store = true
			case 'p':
				print = true
			default:
				return fmt.Errorf("history: -%c: invalid option\n%s", opt, historyUsage)
			}
		}
	}
	args = args[i:]

	if print {
		// Like Bash, -p prints what it can and fails if anything did not
		// expand.
		var failed error
		for _, arg := range args {
			out, _, _, err := h.Expand(arg)
			if err != nil {
				failed = fmt.Errorf("history: %s: history expansion failed", arg)
				continue
			}
			fmt.Fprintln(stdout, out)
		}
		return failed
	}
	if store {
		if len(args) > 0 {
			h.ReplaceLast(strings.Join(args, " "))
		}
		return nil
	}
	if clear {
		h.Clear()
	}
	if deleteSpec != "" {
		if err := deleteHistory(h, deleteSpec); err != nil {
			return err
		}
	}
	if fileOp != 0 {
		file := ""
		if len(args) > 0 {
			file = args[0]
		}
		switch fileOp {
		case 'a':
			if file != "" && file != h.cfg.file {
				return writeHistoryFileEntries(h, file)
			}
			return h.AppendNew()
		case 'n':
			return h.ReadNew()
		case 'r':
			return h.ReadFile(file)
		case 'w':
			return h.WriteFile(file)
		}
	}
	if clear || deleteSpec != "" {
		return nil
	}
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			return fmt.Errorf("history: %s: numeric argument required", args[0])
		}
		listCount, hasList = n, true
	}
	first, entries, timeFormat, timestamps := h.Timed()
	if hasList && listCount < len(entries) {
		first += len(entries) - listCount
		entries = entries[len(entries)-listCount:]
	}
	for idx, entry := range entries {
		stamp := ""
		if timestamps {
			stamp = strftime(timeFormat, time.Unix(entry.time, 0))
		}
		fmt.Fprintf(stdout, "%5d  %s%s\n", first+idx, stamp, entry.line)
	}
	return nil
}

const historyUsage = "history: usage: history [-c] [-d offset] [n] or history -anrw [filename] or history -ps arg [arg...]"

// deleteHistory deletes the entry at offset, or the entries from start to
// end inclusive for a start-end range, as "history -d" does.
func deleteHistory(h *history, spec string) error {
	start, end, isRange := spec, spec, false
	if i := strings.IndexByte(spec[1:], '-'); i >= 0 {
		start, end, isRange = spec[:i+1], spec[i+2:], true
	}
	first, err := strconv.Atoi(start)
	if err != nil {
		return fmt.Errorf("history: %s: history position out of range", spec)
	}
	last := first
	if isRange {
		if last, err = strconv.Atoi(end); err != nil {
			return fmt.Errorf("history: %s: history position out of range", spec)
		}
	}
	base, entries := h.Numbered()
	toIndex := func(pos int) int {
		if pos < 0 {
			return len(entries) + pos + 1
		}
		return pos - base + 1
	}
	from, to := toIndex(first), toIndex(last)
	if from < 1 || to > len(entries) || from > to {
		return fmt.Errorf("history: %s: history position out of range", spec)
	}
	// Delete from the end so the earlier numbers stay put.
	for pos := to; pos >= from; pos-- {
		h.Delete(pos + base - 1)
	}
	return nil
}

// writeHistoryFileEntries appends the entries not yet in the history file
// to another file, for "history -a file".
func writeHistoryFileEntries(h *history, name string) error {
	h.mu.Lock()
	n := min(h.unsaved, len(h.entries))
	entries := slices.Clone(h.entries[len(h.entries)-n:])
	timestamps := h.cfg.timestamps
	h.mu.Unlock()
	_, err := appendHistoryLines(name, timestamps, entries...)
	return err
}

// builtinFc implements "fc -l [-nr] [first [last]]". first and last are
// history numbers, or offsets from the end when negative. The default range
// is the last 16 entries before the fc command itself.
func builtinFc(h *history, args []string, stdout io.Writer) error {
	if h == nil {
		return fmt.Errorf("fc: no history")
	}
	var list, noNumbers, reverse bool
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' && !isNumber(args[0][1:]) {
		for _, opt := range args[0][1:] {
			switch opt {
			case 'l':
				list = true
			case 'n':
				noNumbers = true
			case 'r':
				reverse = true
			default:
				return fmt.Errorf("fc: -%c: invalid option", opt)
			}
		}
		args = args[1:]
	}
	if !list {
		return fmt.Errorf("fc: only `fc -l` is supported")
	}
	if len(args) > 2 {
		return fmt.Errorf("fc: too many arguments")
	}

	base, entries := h.Numbered()
	base--
	// The fc command itself is the newest entry.
	n := len(entries) - 1
	if n <= 0 {
		return nil
	}
	first, last := max(n-15, 1), n
	var err error
	if len(args) >= 1 && args[0] != "" {
		if first, err = fcOffset(args[0], base, n); err != nil {
			return err
		}
		last = first
		if len(args) == 1 {
			last = n
		}
	}
	if len(args) >= 2 {
		if last, err = fcOffset(args[1], base, n); err != nil {
			return err
		}
	}
	if first > last {
		first, last = last, first
		reverse = !reverse
	}
	if first < 1 || last > len(entries) {
		return fmt.Errorf("fc: history specification out of range")
	}
	for k := first; k <= last; k++ {
		i := k
		if reverse {
			i = first + last - k
		}
		if noNumbers {
			fmt.Fprintf(stdout, "\t %s\n", entries[i-1])
		} else {
			fmt.Fprintf(stdout, "%d\t %s\n", base+i, entries[i-1])
		}
	}
	return nil
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// fcOffset turns a history number, or a negative offset from the end, into
// a 1-based index into the n entries after the first base were dropped.
func fcOffset(spec string, base, n int) (int, error) {
	off, err := strconv.Atoi(spec)
	if err != nil {
		return 0, fmt.Errorf("fc: invalid history specification %q", spec)
	}
	if off >= 0 {
		off -= base
	} else {
		off += n + 1
	}
	return off, nil
}

// dropPrintfDashDash strips the leading "--" that Bash's printf accepts as an
// end-of-options marker but mvdan.cc/sh treats as a format string.
func dropPrintfDashDash(args []string) ([]string, bool) {
	if len(args) < 2 || args[1] != "--" {
		return nil, false
	}
	next := make([]string, 1, len(args)-1)
	next[0] = args[0]
	next = append(next, args[2:]...)
	return next, true
}

// builtinWget downloads one URL into dir, the shell's working directory,
// and returns the name of the file it saved.
func builtinWget(ctx context.Context, args []string, dir string, out io.Writer) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("wget: builtin only supports a single URL argument")
	}
	rawURL := args[0]
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("wget: invalid url %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("wget: unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("wget: missing host in %q", rawURL)
	}
	name := path.Base(parsed.Path)
	if name == "." || name == "/" || name == "" {
		name = "index.html"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("wget: failed to build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("wget: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("wget: bad status: %s", resp.Status)
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("wget: cannot open %s: %w", name, err)
	}
	defer file.Close()
	progress := &wgetProgress{out: out, size: resp.ContentLength}
	defer progress.Done()
	reader := io.TeeReader(resp.Body, progress)
	if _, err := io.Copy(file, reader); err != nil {
		return "", fmt.Errorf("wget: failed to save %s: %w", name, err)
	}
	if err := file.Chmod(0o755); err != nil {
		return "", fmt.Errorf("wget: chmod failed for %s: %w", name, err)
	}
	return name, nil
}

type wgetProgress struct {
	out   io.Writer
	size  int64
	total int64
	last  time.Time
	done  bool
}

func (p *wgetProgress) Write(b []byte) (int, error) {
	if p == nil || p.out == nil {
		return len(b), nil
	}
	p.total += int64(len(b))
	p.print(false)
	return len(b), nil
}

func (p *wgetProgress) print(force bool) {
	if p == nil || p.out == nil {
		return
	}
	if !force && time.Since(p.last) < 200*time.Millisecond {
		return
	}
	p.last = time.Now()
	if p.size > 0 {
		percent := p.total * 100 / p.size
		fmt.Fprintf(p.out, "\r%3d%% %s/%s", percent, p.formatSize(p.total), p.formatSize(p.size))
	} else {
		fmt.Fprintf(p.out, "\r%s", p.formatSize(p.total))
	}
}

func (p *wgetProgress) Done() {
	if p == nil || p.out == nil || p.done {
		return
	}
	p.print(true)
	fmt.Fprint(p.out, "\n")
	p.done = true
}

func (p *wgetProgress) formatSize(v int64) string {
	if v < 1024 {
		return fmt.Sprintf("%dB", v)
	}
	type unit struct {
		name  string
		value float64
	}
	units := []unit{
		{"K", 1024},
		{"M", 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
	}
	val := float64(v)
	for i := len(units) - 1; i >= 0; i-- {
		if val >= units[i].value {
			return fmt.Sprintf("%.1f%s", val/units[i].value, units[i].name)
		}
	}
	return fmt.Sprintf("%.1fK", val/1024)
}
