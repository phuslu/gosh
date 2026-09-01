package gosh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func callHandler(runner func() *interp.Runner, history *history, bindings *keyBindingManager) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		if len(args) == 0 {
			return args, nil
		}
		switch args[0] {
		case "printf":
			var r *interp.Runner
			if runner != nil {
				r = runner()
			}
			if r != nil && r.Funcs[args[0]] != nil {
				return args, nil
			}
			if next, ok := dropBuiltinPrintfDashDash(args); ok {
				return next, nil
			}
		case "shopt":
			var r *interp.Runner
			if runner != nil {
				r = runner()
			}
			if r != nil && r.Funcs[args[0]] != nil {
				return args, nil
			}
			if shouldHandleShopt(args[1:]) {
				next := make([]string, 1, len(args))
				next[0] = shoptCommand
				next = append(next, args[1:]...)
				return next, nil
			}
		case "set":
			if next, ok := handleSetVerboseOption(args); ok {
				return next, nil
			}
		case "builtin":
			var r *interp.Runner
			if runner != nil {
				r = runner()
			}
			if r != nil && r.Funcs[args[0]] != nil {
				return args, nil
			}
			if len(args) >= 2 && args[1] == "shopt" && shouldHandleShopt(args[2:]) {
				next := make([]string, 1, len(args)-1)
				next[0] = shoptCommand
				next = append(next, args[2:]...)
				return next, nil
			}
			if len(args) >= 2 && args[1] == "set" {
				if next, ok := handleSetVerboseOption(args[1:]); ok {
					if len(next) == 1 && next[0] == ":" {
						return next, nil
					}
					return append([]string{args[0]}, next...), nil
				}
			}
			if len(args) >= 2 && args[1] == "printf" {
				if next, ok := dropBuiltinPrintfDashDash(args[1:]); ok {
					return append([]string{args[0]}, next...), nil
				}
			}
		case "command":
			var r *interp.Runner
			if runner != nil {
				r = runner()
			}
			if r != nil && r.Funcs[args[0]] != nil {
				return args, nil
			}
			if shoptArgs, ok := commandShoptArgs(args[1:]); ok {
				next := make([]string, 1, len(shoptArgs)+1)
				next[0] = shoptCommand
				next = append(next, shoptArgs...)
				return next, nil
			}
			if len(args) >= 2 && args[1] == "set" {
				if next, ok := handleSetVerboseOption(args[1:]); ok {
					if len(next) == 1 && next[0] == ":" {
						return next, nil
					}
					return append([]string{args[0]}, next...), nil
				}
			}
			if next, ok := dropCommandPrintfDashDash(args); ok {
				return next, nil
			}
		case "wget":
			if _, err := exec.LookPath(args[0]); err == nil {
				return args, nil
			}
			hc := interp.HandlerCtx(ctx)
			file, err := builtinWget(ctx, args[1:], hc.Stdout)
			if err != nil {
				fmt.Fprintln(hc.Stderr, err)
				return []string{"false"}, nil
			}
			fmt.Fprintf(hc.Stdout, "Saved %s\n", file)
			return []string{":"}, nil
		case "history":
			if history == nil {
				return args, nil
			}
			hc := interp.HandlerCtx(ctx)
			if err := builtinHistory(history, args[1:], hc.Stdout); err != nil {
				fmt.Fprintln(hc.Stderr, err)
				return []string{"false"}, nil
			}
			return []string{":"}, nil
		case "bind":
			if bindings == nil {
				return args, nil
			}
			if err := bindings.handleBind(args[1:]); err != nil {
				hc := interp.HandlerCtx(ctx)
				fmt.Fprintln(hc.Stderr, err)
				return []string{"false"}, nil
			}
			return []string{":"}, nil
		case "kill", "newgrp":
			var r *interp.Runner
			if runner != nil {
				r = runner()
			}
			if r != nil && r.Funcs[args[0]] != nil {
				return args, nil
			}
			hc := interp.HandlerCtx(ctx)
			path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
			if err != nil {
				return args, nil
			}
			next := slices.Clone(args)
			next[0] = path
			return next, nil
		default:
			return args, nil
		}
		return args, nil
	}
}

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

func dropBuiltinPrintfDashDash(args []string) ([]string, bool) {
	if len(args) < 2 || args[0] != "printf" || args[1] != "--" {
		return nil, false
	}
	next := make([]string, 1, len(args)-1)
	next[0] = args[0]
	next = append(next, args[2:]...)
	return next, true
}

func dropCommandPrintfDashDash(args []string) ([]string, bool) {
	if len(args) < 3 || args[0] != "command" || args[1] != "printf" || args[2] != "--" {
		return nil, false
	}
	next := make([]string, 2, len(args)-1)
	next[0] = args[0]
	next[1] = args[1]
	next = append(next, args[3:]...)
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
