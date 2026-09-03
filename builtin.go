package gosh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// builtinHistory implements the history builtin's -c/-d/-w/-r flags and the
// plain listing. It is a separate function so it can be tested without an
// interpreter runner.
func builtinHistory(h *history, args []string, stdout io.Writer) error {
	var (
		clear, delete, write, read bool
		deletePos                  int
		file                       string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			clear = true
		case "-d":
			delete = true
			if i+1 >= len(args) {
				return fmt.Errorf("history: -d requires an offset")
			}
			i++
			pos, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("history: invalid offset %q", args[i])
			}
			deletePos = pos
		case "-w":
			write = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				file = args[i]
			}
		case "-r":
			read = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				file = args[i]
			}
		default:
			return fmt.Errorf("history: unsupported argument %q", args[i])
		}
	}

	if clear {
		h.Clear()
	}
	if delete {
		if !h.Delete(deletePos) {
			return fmt.Errorf("history: position out of range")
		}
	}
	if read {
		if err := h.ReadFile(file); err != nil {
			return err
		}
	}
	if write {
		if err := h.WriteFile(file); err != nil {
			return err
		}
	}
	if !clear && !delete && !read && !write {
		for idx, entry := range h.Entries() {
			fmt.Fprintf(stdout, "%5d  %s\n", idx+1, entry)
		}
	}
	return nil
}

// builtinFc implements the `fc -l [first [last]]` subset of the fc builtin.
// Offsets are 1-based history positions; negative offsets count from the
// end. The default range is the last 16 entries, printed chronologically.
func builtinFc(h *history, args []string, stdout io.Writer) error {
	if h == nil {
		return fmt.Errorf("fc: no history")
	}
	if len(args) == 0 || args[0] != "-l" {
		return fmt.Errorf("fc: only `fc -l` is supported")
	}
	args = args[1:]
	if len(args) > 2 {
		return fmt.Errorf("fc: too many arguments")
	}

	entries := h.Entries()
	n := len(entries)
	if n == 0 {
		return nil
	}
	first, last := n-15, n
	if first < 1 {
		first = 1
	}
	var err error
	if len(args) >= 1 && args[0] != "" {
		if first, err = fcOffset(args[0], n); err != nil {
			return err
		}
	}
	if len(args) >= 2 {
		if last, err = fcOffset(args[1], n); err != nil {
			return err
		}
	}
	if first < 1 || last < 1 || first > last || last > n {
		return fmt.Errorf("fc: history specification out of range")
	}
	for i := first; i <= last; i++ {
		fmt.Fprintf(stdout, "%d\t%s\n", i, entries[i-1])
	}
	return nil
}

func fcOffset(spec string, n int) (int, error) {
	off, err := strconv.Atoi(spec)
	if err != nil {
		return 0, fmt.Errorf("fc: invalid history specification %q", spec)
	}
	if off < 0 {
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

func builtinWget(ctx context.Context, args []string, out io.Writer) (string, error) {
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
	file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
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
